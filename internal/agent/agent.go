// Package agent ties everything together: it keeps one Device per managed
// machine, checks and upgrades them one operation at a time, and publishes
// their state to Home Assistant over MQTT.
package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"maps"
	"os"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/robfig/cron/v3"

	"github.com/lvlie/linux-ha-agent/internal/config"
	"github.com/lvlie/linux-ha-agent/internal/containers"
	"github.com/lvlie/linux-ha-agent/internal/guest"
	"github.com/lvlie/linux-ha-agent/internal/ha"
	"github.com/lvlie/linux-ha-agent/internal/mqtt"
	"github.com/lvlie/linux-ha-agent/internal/runner"
)

// ProjectURL is shown in Home Assistant as the origin of the entities.
const ProjectURL = "https://github.com/lvlie/linux-ha-agent"

// Options configure an Agent.
type Options struct {
	Config config.Config
	MQTT   mqtt.Client
	// Providers manage the guests of virtualisation platforms on this host
	// (Incus, Proxmox VE). Empty on machines without guests.
	Providers []guest.Provider
	Local     runner.Runner
	Registry  containers.DigestResolver
	Hostname  string
	Version   string
	Logger    *slog.Logger
	Now       func() time.Time
	// RebootHost reboots the machine; defaults to `systemctl reboot`.
	RebootHost func(ctx context.Context) error
	// OperationMarker is a file that exists while an operation that changes
	// the system (upgrade, restart, restore, reboot) runs. The package's
	// postinstall script waits for it before restarting an agent that
	// upgraded itself. Empty: no marker.
	OperationMarker string
}

// Agent manages the host and its Incus guests.
type Agent struct {
	cfg       config.Config
	mqtt      mqtt.Client
	providers []guest.Provider
	local     runner.Runner
	registry  containers.DigestResolver
	version   string
	log       *slog.Logger
	now       func() time.Time
	reboot    func(ctx context.Context) error
	topics    ha.Topics
	schedule  cron.Schedule

	// marker is Options.OperationMarker. stopping is closed when shutdown
	// starts, workerDone when the worker has finished its last operation.
	marker     string
	stopping   chan struct{}
	stopOnce   sync.Once
	workerDone chan struct{}

	// composeProjects caches incuscompose.Find; see composeDir.
	composeMu       sync.Mutex
	composeProjects map[string]string
	composeTime     time.Time
	// composeImages caches incuscompose.ServiceImages by directory for as
	// long as composeProjects.
	composeImages map[string]map[string]string

	mu      sync.Mutex
	devices map[string]*Device
	host    *Device
	state   persisted
	pending map[string]bool
	ops     chan op
	idle    *sync.Cond
	busy    int
}

type op struct {
	kind    string
	device  string
	trigger string
	// arg is the container key of container commands.
	arg string
}

func (o op) key() string { return o.kind + ":" + o.device + ":" + o.arg }

// New creates an agent. Nothing happens until Run.
func New(opts Options) (*Agent, error) {
	if opts.MQTT == nil || opts.Local == nil {
		return nil, errors.New("agent needs an MQTT client and a local runner")
	}
	a := &Agent{
		cfg:       opts.Config,
		mqtt:      opts.MQTT,
		providers: opts.Providers,
		local:     opts.Local,
		registry:  opts.Registry,
		version:   opts.Version,
		log:       opts.Logger,
		now:       opts.Now,
		reboot:    opts.RebootHost,
		devices:   map[string]*Device{},
		pending:   map[string]bool{},
		ops:       make(chan op, 256),
	}
	a.marker = opts.OperationMarker
	a.stopping = make(chan struct{})
	a.workerDone = make(chan struct{})
	a.idle = sync.NewCond(&a.mu)
	if a.log == nil {
		a.log = slog.Default()
	}
	if a.now == nil {
		a.now = time.Now
	}
	if a.version == "" {
		a.version = "dev"
	}
	if a.reboot == nil {
		a.reboot = func(ctx context.Context) error {
			_, err := runner.Check(ctx, a.local, runner.Command("systemctl", "reboot"))
			return err
		}
	}
	name := opts.Config.Host.Name
	if name == "" {
		name = opts.Hostname
	}
	if name == "" {
		return nil, errors.New("no host name: set host.name")
	}
	hostID := HostID(name)
	a.topics = ha.Topics{DiscoveryPrefix: a.cfg.HomeAssistant.DiscoveryPrefix, Base: a.cfg.HomeAssistant.BaseTopic, HostID: hostID}
	if a.cfg.Upgrade.Schedule != "" {
		s, err := config.ParseSchedule(a.cfg.Upgrade.Schedule)
		if err != nil {
			return nil, err
		}
		a.schedule = s
	}
	st, err := loadState(a.cfg.StateFile)
	if err != nil {
		a.log.Warn("ignoring unreadable state file", "err", err)
	}
	a.state = st
	a.host = &Device{ID: hostID, Name: name, Host: true, Runner: a.local, LastRun: st.LastRuns[hostID],
		components: st.Components[hostID], lastChecked: lastChecked(st, hostID)}
	a.devices[hostID] = a.host
	return a, nil
}

// HostID is the device ID of a host with the given name.
func HostID(name string) string { return ha.Slug(name) }

// Reconnected republishes availability, discovery and state after the MQTT
// connection was (re)established.
func (a *Agent) Reconnected() {
	if err := a.mqtt.Publish(a.topics.Availability(), true, []byte("online")); err != nil {
		a.log.Error("publishing availability failed", "err", err)
	}
	a.PublishAll(true)
}

// Topics exposes the MQTT topics (for tests and logs).
func (a *Agent) Topics() ha.Topics { return a.topics }

func (a *Agent) autoUpgrade() bool {
	if a.state.AutoUpgrade != nil {
		return *a.state.AutoUpgrade
	}
	return a.cfg.Upgrade.AutoUpgrade
}

// features are the actions Home Assistant may trigger on a device.
func (a *Agent) features(d *Device) ha.Features {
	p := a.cfg.Permissions
	reboot := p.RebootHost
	if !d.Host {
		reboot = p.RebootGuests && !a.rebootExcluded(d)
	}
	return ha.Features{
		Upgrade: p.Upgrade,
		Reboot:  reboot,
		// Application containers are rebuilt or recreated on upgrades, which
		// is impossible with or removes snapshots, so they have none to restore.
		Restore:  p.Restore && a.cfg.Guests.Snapshot && len(a.providers) > 0 && d.app() == nil,
		Schedule: a.schedule != nil,

		RestartContainers: p.RestartContainers,
	}
}

// rebootExcluded reports whether a guest must never be rebooted.
func (a *Agent) rebootExcluded(d *Device) bool {
	return d.Guest != nil && guestRef(*d.Guest).Matches(a.cfg.Permissions.RebootExclude)
}

func guestRef(g guest.Guest) config.GuestRef {
	return config.GuestRef{Project: g.Project, Name: g.Name, ID: g.ID, Type: g.Type}
}

// Start subscribes, publishes the initial state and starts the worker. It
// returns once everything is running; Run blocks instead.
func (a *Agent) Start(ctx context.Context) error {
	if err := a.mqtt.Subscribe(a.topics.CommandFilter(), a.handleCommand); err != nil {
		return err
	}
	if err := a.mqtt.Subscribe(a.topics.AutoUpgrade(), a.handleAutoUpgrade); err != nil {
		return err
	}
	if err := a.mqtt.Subscribe(a.topics.HAStatus(), func(_ string, payload []byte) {
		if string(payload) == "online" {
			a.log.Info("home assistant restarted, republishing discovery")
			a.PublishAll(true)
		}
	}); err != nil {
		return err
	}
	if err := a.mqtt.Publish(a.topics.Availability(), true, []byte("online")); err != nil {
		return err
	}
	a.publish(a.host, false)

	go func() {
		<-ctx.Done()
		a.stopOnce.Do(func() { close(a.stopping) })
	}()
	go a.worker(ctx)
	if len(a.providers) > 0 {
		a.enqueue(op{kind: "sync"})
	}
	a.enqueue(op{kind: "check_all", trigger: "startup"})
	return nil
}

// Run starts the agent and blocks until ctx is cancelled.
func (a *Agent) Run(ctx context.Context) error {
	if err := a.Start(ctx); err != nil {
		return err
	}
	ticker := time.NewTicker(a.cfg.Checks.Interval)
	defer ticker.Stop()
	var stats <-chan time.Time
	if a.cfg.Checks.StatsInterval > 0 {
		t := time.NewTicker(a.cfg.Checks.StatsInterval)
		defer t.Stop()
		stats = t.C
	}

	var scheduler *cron.Cron
	if a.schedule != nil {
		scheduler = cron.New()
		scheduler.Schedule(a.schedule, cron.FuncJob(func() {
			if a.autoUpgrade() {
				a.log.Info("scheduled upgrade starting")
				a.enqueue(op{kind: ha.CmdUpgradeAll, trigger: "schedule"})
			} else {
				a.log.Info("scheduled upgrade skipped: automatic upgrades are off")
			}
			a.publish(a.host, false) // next run time changed
		}))
		scheduler.Start()
		defer scheduler.Stop()
	}
	for _, p := range a.providers {
		go a.watchGuests(ctx, p)
	}
	if len(a.cfg.Commands.Expose) > 0 {
		go a.commandLoop(ctx)
	}
	for {
		select {
		case <-ctx.Done():
			a.waitWorker()
			_ = a.mqtt.Publish(a.topics.Availability(), true, []byte("offline"))
			return nil
		case <-ticker.C:
			a.enqueue(op{kind: "check_all", trigger: "interval"})
		case <-stats:
			for _, d := range a.deviceList() {
				a.enqueue(op{kind: "stats", device: d.ID})
			}
		}
	}
}

func (a *Agent) watchGuests(ctx context.Context, p guest.Provider) {
	for ctx.Err() == nil {
		err := p.WatchLifecycle(ctx, func() { a.enqueue(op{kind: "sync"}) })
		if ctx.Err() != nil {
			return
		}
		a.log.Warn("watching guests failed, retrying", "platform", p.Platform(), "err", err)
		select {
		case <-ctx.Done():
			return
		case <-time.After(10 * time.Second):
		}
		a.enqueue(op{kind: "sync"})
	}
}

// enqueue adds an operation unless an identical one is already waiting.
func (a *Agent) enqueue(o op) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.pending[o.key()] {
		return
	}
	select {
	case a.ops <- o:
		a.pending[o.key()] = true
		a.busy++
	default:
		a.log.Warn("operation queue full, dropping", "op", o.kind, "device", o.device)
	}
}

// WaitIdle blocks until the queue is empty and no operation runs (tests).
func (a *Agent) WaitIdle() {
	a.mu.Lock()
	defer a.mu.Unlock()
	for a.busy > 0 {
		a.idle.Wait()
	}
}

// worker runs the queued operations one at a time. Once ctx is cancelled it
// starts no new operation, but lets the running one finish (see execute).
func (a *Agent) worker(ctx context.Context) {
	defer close(a.workerDone)
	for ctx.Err() == nil {
		select {
		case <-ctx.Done():
			return
		case o := <-a.ops:
			a.mu.Lock()
			delete(a.pending, o.key())
			a.mu.Unlock()
			if ctx.Err() == nil {
				a.execute(ctx, o)
			}
			a.mu.Lock()
			a.busy--
			if a.busy == 0 {
				a.idle.Broadcast()
			}
			a.mu.Unlock()
		}
	}
}

// waitWorker waits for the operation running at shutdown.
func (a *Agent) waitWorker() {
	select {
	case <-a.workerDone:
		return
	default:
	}
	a.log.Info("waiting for the running operation to finish before stopping")
	<-a.workerDone
}

// isStopping reports whether shutdown has started.
func (a *Agent) isStopping() bool {
	select {
	case <-a.stopping:
		return true
	default:
		return false
	}
}

// changesSystem reports whether an operation changes the machines. Stopping
// the agent does not interrupt those: a package manager killed halfway
// leaves dpkg or rpm unfinished, also when the agent is stopped by the very
// upgrade of its own package.
func changesSystem(kind string) bool {
	switch kind {
	case ha.CmdUpgrade, ha.CmdUpgradePackages, ha.CmdUpgradeContainers, ha.CmdUpgradeAll,
		ha.CmdUpgradeContainer, ha.CmdRestartContainer, ha.CmdReboot, ha.CmdRestore:
		return true
	}
	return false
}

// markOperation creates or removes the operation marker.
func (a *Agent) markOperation(running bool) {
	if a.marker == "" {
		return
	}
	var err error
	if running {
		err = os.WriteFile(a.marker, nil, 0o600)
	} else if err = os.Remove(a.marker); errors.Is(err, fs.ErrNotExist) {
		err = nil
	}
	if err != nil {
		a.log.Warn("operation marker", "path", a.marker, "err", err)
	}
}

func (a *Agent) execute(ctx context.Context, o op) {
	defer func() {
		if r := recover(); r != nil {
			a.log.Error("operation panicked", "op", o.kind, "device", o.device, "panic", r)
		}
	}()
	if changesSystem(o.kind) {
		// Its commands keep their own timeouts.
		ctx = context.WithoutCancel(ctx)
		a.markOperation(true)
		defer a.markOperation(false)
	}
	if o.kind == "sync" {
		a.syncGuests(ctx)
		return
	}
	if o.kind == "check_all" {
		for _, d := range a.deviceList() {
			a.enqueue(op{kind: ha.CmdCheck, device: d.ID, trigger: o.trigger})
		}
		return
	}
	if o.kind == ha.CmdUpgradeAll {
		a.upgradeAll(ctx, o.trigger)
		return
	}
	d := a.device(o.device)
	if d == nil {
		a.log.Warn("operation for unknown device", "op", o.kind, "device", o.device)
		return
	}
	switch o.kind {
	case "stats":
		a.refreshStats(ctx, d)
		a.publish(d, false)
	case ha.CmdUpgradeContainer:
		a.upgradeContainer(ctx, d, o.arg, o.trigger)
	case ha.CmdRestartContainer:
		a.restartContainer(ctx, d, o.arg, o.trigger)
	case ha.CmdCheck:
		a.check(ctx, d)
	case ha.CmdUpgrade, ha.CmdUpgradePackages, ha.CmdUpgradeContainers:
		needsReboot := a.upgrade(ctx, d, o.kind, o.trigger, false)
		if needsReboot && d.Host && a.cfg.Upgrade.RebootHost && !a.isStopping() {
			a.rebootHost(ctx, o.trigger)
		}
	case ha.CmdReboot:
		if d.Host {
			a.rebootHost(ctx, o.trigger)
		} else {
			a.restartGuest(ctx, d, o.trigger)
		}
	case ha.CmdRestore:
		a.restore(ctx, d, o.trigger)
	}
}

func (a *Agent) device(id string) *Device {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.devices[id]
}

// deviceList returns the host first, then guests by name.
func (a *Agent) deviceList() []*Device {
	a.mu.Lock()
	defer a.mu.Unlock()
	list := make([]*Device, 0, len(a.devices))
	for _, d := range a.devices {
		if !d.Host {
			list = append(list, d)
		}
	}
	slices.SortFunc(list, func(x, y *Device) int { return strings.Compare(x.ID, y.ID) })
	return append([]*Device{a.host}, list...)
}

// handleCommand validates and queues a command from Home Assistant.
func (a *Agent) handleCommand(topic string, payload []byte) {
	parts := strings.Split(topic, "/")
	if len(parts) < 2 {
		return
	}
	deviceID := parts[len(parts)-2]
	cmd, arg, _ := strings.Cut(strings.TrimSpace(string(payload)), " ")
	arg = strings.TrimSpace(arg)
	d := a.device(deviceID)
	log := a.log.With("device", deviceID, "command", cmd, "arg", arg)
	if d == nil {
		log.Warn("command rejected: unknown device")
		return
	}
	if err := a.allowed(d, cmd, arg); err != nil {
		log.Warn("command rejected", "reason", err)
		return
	}
	if cmd == ha.CmdRunCommand {
		// Read-only and unprivileged: runs next to operations, not after them.
		a.runAnyCommand(d, arg, log)
		return
	}
	log.Info("command accepted")
	a.enqueue(op{kind: cmd, device: d.ID, arg: arg, trigger: "home assistant"})
}

func (a *Agent) allowed(d *Device, cmd, arg string) error {
	p := a.cfg.Permissions
	if arg != "" && cmd != ha.CmdUpgradeContainer && cmd != ha.CmdRestartContainer && cmd != ha.CmdRunCommand {
		return fmt.Errorf("%s takes no argument", cmd)
	}
	switch cmd {
	case ha.CmdRunCommand:
		if !a.anyCommand(d) {
			return errors.New("running commands is not permitted")
		}
		if arg == "" || len(arg) > 255 {
			return errors.New("the command must be 1 to 255 characters")
		}
		return nil
	case ha.CmdUpgradeContainer, ha.CmdRestartContainer:
		if cmd == ha.CmdUpgradeContainer && !p.Upgrade {
			return errors.New("upgrades are not permitted")
		}
		if cmd == ha.CmdRestartContainer && !p.RestartContainers {
			return errors.New("restarting containers is not permitted")
		}
		a.mu.Lock()
		c, ok := d.container(arg)
		a.mu.Unlock()
		if !ok {
			return fmt.Errorf("unknown container %q", arg)
		}
		if cmd == ha.CmdUpgradeContainer && c.Project == "" && !c.AutoUpdate {
			return fmt.Errorf("container %s is not managed by compose or podman auto-update", c.Name)
		}
		return nil
	case ha.CmdCheck:
		return nil
	case ha.CmdUpgrade, ha.CmdUpgradePackages, ha.CmdUpgradeContainers:
		if !p.Upgrade {
			return errors.New("upgrades are not permitted")
		}
		a.mu.Lock()
		isApp := d.app() != nil
		installable := !isApp || a.appInstallable(d)
		a.mu.Unlock()
		if isApp && cmd != ha.CmdUpgrade {
			return fmt.Errorf("application containers have no packages or containers of their own; use %s to update the image", ha.CmdUpgrade)
		}
		if !installable {
			return errors.New("the agent cannot update this application container (incus-compose project not found, or the service is stopped?)")
		}
	case ha.CmdUpgradeAll:
		if !p.Upgrade {
			return errors.New("upgrades are not permitted")
		}
		if !d.Host {
			return errors.New("upgrade_all is only available on the host")
		}
	case ha.CmdReboot:
		switch {
		case d.Host && !p.RebootHost:
			return errors.New("rebooting the host is not permitted")
		case !d.Host && !p.RebootGuests:
			return errors.New("rebooting guests is not permitted")
		case a.rebootExcluded(d):
			return errors.New("this guest is excluded from reboots")
		}
	case ha.CmdRestore:
		if !a.features(d).Restore {
			return errors.New("snapshot restores are not permitted")
		}
		if d.Guest == nil {
			return errors.New("only guests can be restored")
		}
	default:
		return fmt.Errorf("unknown command %q", cmd)
	}
	return nil
}

func (a *Agent) handleAutoUpgrade(_ string, payload []byte) {
	if a.schedule == nil {
		return
	}
	on := strings.EqualFold(strings.TrimSpace(string(payload)), "ON")
	a.mu.Lock()
	a.state.AutoUpgrade = &on
	a.mu.Unlock()
	a.log.Info("automatic upgrades switched", "enabled", on)
	a.saveState()
	a.publish(a.host, false)
}

func (a *Agent) saveState() {
	a.mu.Lock()
	st := persisted{AutoUpgrade: a.state.AutoUpgrade, LastRuns: map[string]*RunReport{},
		Components: map[string]map[string]string{}, Discovery: map[string]ha.Device{}, ImageDigests: map[string]map[string]string{}}
	for id, d := range a.devices {
		st.Devices = append(st.Devices, id)
		if d.LastRun != nil {
			st.LastRuns[id] = d.LastRun
		}
		if len(d.components) > 0 {
			st.Components[id] = d.components
		}
		if d.lastChecked != nil {
			st.Discovery[id] = *d.lastChecked
		}
		if len(d.imageDigests) > 0 {
			st.ImageDigests[id] = d.imageDigests
		}
	}
	a.mu.Unlock()
	slices.Sort(st.Devices)
	if err := saveState(a.cfg.StateFile, st); err != nil {
		a.log.Error("saving state failed", "err", err)
	}
}

// PublishAll republishes every device; force resends discovery too.
func (a *Agent) PublishAll(force bool) {
	for _, d := range a.deviceList() {
		a.publish(d, force)
	}
}

// publish sends a device's discovery (when changed) and state.
func (a *Agent) publish(d *Device, forceDiscovery bool) {
	a.mu.Lock()
	origin := ha.Origin{Name: "linux-ha-agent", Version: a.version, URL: ProjectURL}
	dev := d.haDevice(a.topics.HostID, a.cfg.Checks.ContainerEntities)
	dev.Commands, dev.AnyCommand = a.commandEntities(d), a.anyCommand(d)
	dev.AppInstall = a.appInstallable(d)
	payload, components, err := ha.Payload(a.topics, origin, dev, a.features(d), d.components)
	sendDiscovery := err == nil && (forceDiscovery || string(payload) != d.discovery)
	componentsChanged := false
	if sendDiscovery {
		d.discovery = string(payload)
		componentsChanged = !maps.Equal(components, d.components)
		d.components = components
		if d.Check != nil {
			checked := d.haDevice(a.topics.HostID, a.cfg.Checks.ContainerEntities)
			d.lastChecked = &checked
		}
	}
	var extra map[string]any
	if d.Host {
		extra = a.hostTotals()
	}
	state, stateErr := json.Marshal(d.state(extra))
	a.mu.Unlock()
	if componentsChanged {
		a.saveState()
	}

	if err != nil || stateErr != nil {
		a.log.Error("building mqtt payload failed", "device", d.ID, "err", errors.Join(err, stateErr))
		return
	}
	if sendDiscovery {
		if err := a.mqtt.Publish(a.topics.Config(d.ID), true, payload); err != nil {
			a.log.Error("publishing discovery failed", "device", d.ID, "err", err)
		}
	}
	if err := a.mqtt.Publish(a.topics.State(d.ID), true, state); err != nil {
		a.log.Error("publishing state failed", "device", d.ID, "err", err)
	}
	if !d.Host {
		// The host shows totals over all guests.
		a.publish(a.host, false)
	}
}

// hostTotals are the host-only state fields. Callers hold a.mu.
func (a *Agent) hostTotals() map[string]any {
	totals := map[string]any{
		"agent_version": a.version,
		"auto_upgrade":  a.autoUpgrade(),
		"next_upgrade":  nil,
	}
	if a.schedule != nil && a.autoUpgrade() {
		totals["next_upgrade"] = a.schedule.Next(a.now()).Format(time.RFC3339)
	}
	guests, running := 0, 0
	withUpdates, needReboot := []string{}, []string{}
	updates, images := 0, 0
	for _, d := range a.devices {
		updates += d.pendingUpdates()
		images += d.pendingImages()
		if d.Host {
			continue
		}
		guests++
		if d.running() {
			running++
		}
		if d.pendingUpdates()+d.pendingImages() > 0 {
			withUpdates = append(withUpdates, d.Name)
		}
		if d.rebootRequired() {
			needReboot = append(needReboot, d.Name)
		}
	}
	slices.Sort(withUpdates)
	slices.Sort(needReboot)
	totals["guests"] = guests
	totals["guests_running"] = running
	totals["guests_with_updates"] = len(withUpdates)
	totals["guests_with_updates_list"] = withUpdates
	totals["guests_reboot_required"] = len(needReboot)
	totals["guests_reboot_required_list"] = needReboot
	totals["updates_total"] = updates
	totals["image_updates_total"] = images
	return totals
}

// unpublish removes a device from Home Assistant.
func (a *Agent) unpublish(id string) {
	_ = a.mqtt.Publish(a.topics.Config(id), true, nil)
	_ = a.mqtt.Publish(a.topics.State(id), true, nil)
}

func (a *Agent) setOperation(d *Device, operation string) {
	a.mu.Lock()
	d.Operation = operation
	a.mu.Unlock()
	a.publish(d, false)
}

func (a *Agent) managed(g guest.Guest) bool {
	if !a.cfg.Guests.Includes(g.Type) || a.cfg.Guests.Excluded(guestRef(g)) {
		return false
	}
	return g.Platform != "incus" || a.cfg.Incus.InProject(g.Project)
}

// syncGuests reconciles devices with the guests the platforms report.
func (a *Agent) syncGuests(ctx context.Context) {
	type found struct {
		g guest.Guest
		p guest.Provider
	}
	var all []found
	for _, p := range a.providers {
		guests, err := p.Guests(ctx)
		if err != nil {
			// Keep the current devices rather than removing them all.
			a.log.Error("listing guests failed", "platform", p.Platform(), "err", err)
			return
		}
		for _, g := range guests {
			if a.managed(g) {
				all = append(all, found{g, p})
			}
		}
	}
	seen := map[string]bool{a.host.ID: true}
	var added, started, reimaged []*Device
	a.mu.Lock()
	for _, f := range all {
		g := f.g
		id := guestID(a.topics.HostID, g)
		if seen[id] {
			a.log.Warn("two guests map to the same device, ignoring the second", "device", id, "platform", g.Platform)
			continue
		}
		seen[id] = true
		d := a.devices[id]
		if d == nil {
			d = &Device{ID: id, Name: guestName(g), Guest: &g, Provider: f.p, Runner: f.p.Runner(g),
				LastRun: a.state.LastRuns[id], components: a.state.Components[id], lastChecked: lastChecked(a.state, id),
				imageDigests: a.state.ImageDigests[id]}
			a.devices[id] = d
			added = append(added, d)
			continue
		}
		wasRunning := d.running()
		imageChanged := d.Guest.App != nil && g.App != nil && *d.Guest.App != *g.App
		d.Guest = &g
		d.Provider = f.p
		d.Runner = f.p.Runner(g)
		switch {
		case !wasRunning && g.Running():
			started = append(started, d)
		case imageChanged:
			reimaged = append(reimaged, d)
		}
	}
	var removed []string
	for id := range a.devices {
		if !seen[id] {
			delete(a.devices, id)
			removed = append(removed, id)
		}
	}
	// Devices from before a restart that no longer exist.
	for _, id := range a.state.Devices {
		if !seen[id] && !slices.Contains(removed, id) {
			removed = append(removed, id)
		}
	}
	a.state.Devices = nil
	a.mu.Unlock()

	for _, id := range removed {
		a.log.Info("guest gone, removing from home assistant", "device", id)
		a.unpublish(id)
	}
	for _, d := range added {
		a.log.Info("guest found", "device", d.ID, "status", d.Guest.Status)
		// Application containers are checked from outside, also when stopped.
		if d.running() || d.app() != nil {
			a.enqueue(op{kind: ha.CmdCheck, device: d.ID, trigger: "discovered"})
		}
	}
	for _, d := range started {
		a.enqueue(op{kind: ha.CmdCheck, device: d.ID, trigger: "started"})
	}
	for _, d := range reimaged {
		a.enqueue(op{kind: ha.CmdCheck, device: d.ID, trigger: "image changed"})
	}
	a.PublishAll(false)
	if len(added) > 0 || len(removed) > 0 {
		a.saveState()
	}
}

func lastChecked(st persisted, id string) *ha.Device {
	if d, ok := st.Discovery[id]; ok {
		return &d
	}
	return nil
}

func guestID(hostID string, g guest.Guest) string {
	if g.Project == "" || g.Project == "default" {
		return ha.Slug(hostID, g.Name)
	}
	return ha.Slug(hostID, g.Project, g.Name)
}

func guestName(g guest.Guest) string {
	if g.Project == "" || g.Project == "default" {
		return g.Name
	}
	return g.Project + "/" + g.Name
}
