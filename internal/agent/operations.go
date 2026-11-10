package agent

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/lvlie/linux-ha-agent/internal/containers"
	"github.com/lvlie/linux-ha-agent/internal/ha"
	"github.com/lvlie/linux-ha-agent/internal/osinfo"
	"github.com/lvlie/linux-ha-agent/internal/packages"
	"github.com/lvlie/linux-ha-agent/internal/sysinfo"
)

// SnapshotPrefix marks snapshots taken by the agent before upgrades.
const SnapshotPrefix = "ha-agent-"

func (a *Agent) withTimeout(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, a.cfg.Upgrade.CommandTimeout)
}

func (a *Agent) managesPackages(d *Device) bool {
	return !d.Host || a.cfg.Host.Manage
}

func (a *Agent) packageManager(ctx context.Context, d *Device) (packages.Manager, osinfo.Info, error) {
	info, err := osinfo.Detect(ctx, d.Runner)
	if err != nil {
		return nil, info, err
	}
	mgr, err := packages.For(info, d.Runner, packages.Options{NixFlake: a.cfg.NixOS.Flake, NixGCOlderThan: a.cfg.NixOS.GCOlderThan})
	return mgr, info, err
}

// check inspects a device and publishes the result.
func (a *Agent) check(ctx context.Context, d *Device) {
	if d.app() != nil {
		a.appCheck(ctx, d)
		return
	}
	if !d.running() || !a.managesPackages(d) {
		a.publish(d, false)
		return
	}
	if !d.manageable() {
		a.mu.Lock()
		d.Check = &Check{Time: a.now(), Error: d.Guest.Unmanageable}
		a.mu.Unlock()
		a.publish(d, false)
		return
	}
	a.setOperation(d, "checking")
	ctx, cancel := a.withTimeout(ctx)
	defer cancel()

	c := &Check{Time: a.now()}
	var errs []string
	mgr, info, err := a.packageManager(ctx, d)
	c.OS = info.PrettyName
	var unsupported packages.ErrUnsupported
	switch {
	case errors.As(err, &unsupported):
		// Not an error: containers are still managed.
	case err != nil:
		errs = append(errs, err.Error())
	default:
		st, err := mgr.Check(ctx)
		if err != nil {
			errs = append(errs, "packages: "+err.Error())
		} else {
			c.Packages = &st
			c.RebootRequired = st.RebootRequired
		}
	}

	if r, ok := a.registry.(interface{ Reset() }); ok {
		r.Reset()
	}
	local := map[string][]string{}
	engines, err := containers.Detect(ctx, d.Runner, a.podman(d))
	if err != nil {
		errs = append(errs, err.Error())
	}
	for _, e := range engines {
		c.Engines = append(c.Engines, e.Name)
		cs, err := e.Containers(ctx)
		if err != nil {
			errs = append(errs, e.Name+": "+err.Error())
			continue
		}
		for i := range cs {
			cs[i].Engine = e.Name
		}
		c.Containers = append(c.Containers, cs...)
		c.Projects = append(c.Projects, containers.Projects(cs)...)
		if !a.cfg.Checks.ContainerImages || a.registry == nil || len(cs) == 0 {
			continue
		}
		ids := map[string]bool{}
		for _, ctr := range cs {
			ids[ctr.ImageID] = true
		}
		list := make([]string, 0, len(ids))
		for id := range ids {
			list = append(list, id)
		}
		digests, err := e.RepoDigests(ctx, list)
		if err != nil {
			errs = append(errs, e.Name+" images: "+err.Error())
			continue
		}
		for k, v := range digests {
			local[k] = v
		}
	}
	c.LocalDigests = local
	if a.cfg.Checks.ContainerImages && a.registry != nil && len(c.Containers) > 0 {
		c.Images = containers.CheckImages(ctx, c.Containers, local, a.registry)
		c.ImagesChecked = true
	}
	stats, err := sysinfo.Collect(ctx, d.Runner, d.sharedKernel())
	if err != nil {
		errs = append(errs, "system info: "+err.Error())
	}
	c.Error = strings.Join(errs, "; ")
	if c.Error != "" {
		a.log.Warn("check finished with errors", "device", d.ID, "err", c.Error)
	}

	a.mu.Lock()
	d.Check = c
	if err == nil {
		d.Stats, d.StatsTime = &stats, a.now()
	}
	d.Operation = ""
	a.mu.Unlock()
	a.publish(d, false)
}

// refreshStats updates CPU, memory, disk, load and kernel information.
func (a *Agent) refreshStats(ctx context.Context, d *Device) {
	if !d.running() || !d.manageable() || !a.managesPackages(d) || d.app() != nil {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	stats, err := sysinfo.Collect(ctx, d.Runner, d.sharedKernel())
	if err != nil {
		a.log.Debug("collecting system info failed", "device", d.ID, "err", err)
		return
	}
	a.mu.Lock()
	d.Stats, d.StatsTime = &stats, a.now()
	a.mu.Unlock()
}

// upgradeContainer updates one container: its compose service, or podman
// auto-update for containers labelled for it.
func (a *Agent) upgradeContainer(ctx context.Context, d *Device, key, trigger string) {
	report := &RunReport{Trigger: trigger, Action: ha.CmdUpgradeContainer + " " + key, Started: a.now()}
	a.mu.Lock()
	c, ok := d.container(key)
	a.mu.Unlock()
	a.setOperation(d, ha.CmdUpgradeContainer)
	ctx, cancel := a.withTimeout(ctx)
	defer cancel()
	e, err := a.engine(ctx, d, c.Engine)
	switch {
	case !ok:
		report.step("container", false, fmt.Errorf("container %s is gone", key), "")
	case err != nil:
		report.step("container", false, err, "")
	case c.Project != "":
		p := containers.Project{Name: c.Project, WorkingDir: c.WorkingDir, ConfigFiles: c.ConfigFiles, PodmanCompose: c.PodmanCompose}
		var changed bool
		if c.Service == "" {
			changed, err = e.UpgradeProject(ctx, p) // older compose without service labels
		} else {
			changed, err = e.UpgradeService(ctx, p, c.Service)
		}
		report.step(fmt.Sprintf("%s compose %s/%s", c.Engine, c.Project, c.Service), changed, err, c.Image)
	case c.AutoUpdate:
		changed, err := e.AutoUpdate(ctx)
		report.step("podman auto-update", changed, err, c.Image)
	default:
		report.step("container", false, fmt.Errorf("container %s is not managed by compose or podman auto-update", c.Name), "")
	}
	a.finish(ctx, d, report)
}

func (a *Agent) restartContainer(ctx context.Context, d *Device, key, trigger string) {
	report := &RunReport{Trigger: trigger, Action: ha.CmdRestartContainer + " " + key, Started: a.now()}
	a.mu.Lock()
	c, ok := d.container(key)
	a.mu.Unlock()
	a.setOperation(d, ha.CmdRestartContainer)
	ctx, cancel := a.withTimeout(ctx)
	defer cancel()
	if !ok {
		report.step("restart", false, fmt.Errorf("container %s is gone", key), "")
	} else if e, err := a.engine(ctx, d, c.Engine); err != nil {
		report.step("restart "+c.Name, false, err, "")
	} else {
		report.step("restart "+c.Name, true, e.Restart(ctx, c.ID), "")
	}
	a.finish(ctx, d, report)
}

// upgrade upgrades one device and reports whether it needs a reboot that was
// not done (only the host; guests are restarted here when allowed). all is
// set when the device is upgraded as one of all devices.
func (a *Agent) upgrade(ctx context.Context, d *Device, action, trigger string, all bool) (needsReboot bool) {
	report := &RunReport{Trigger: trigger, Action: action, Started: a.now()}
	log := a.log.With("device", d.ID, "action", action, "trigger", trigger)
	log.Info("upgrade starting")
	a.setOperation(d, action)
	defer func() {
		report.Finished = a.now()
		report.Success = true
		for _, s := range report.Steps {
			report.Success = report.Success && s.OK
		}
		a.mu.Lock()
		d.LastRun = report
		d.Operation = ""
		a.mu.Unlock()
		a.saveState()
		log.Info("upgrade finished", "success", report.Success, "changed", report.Changed)
		a.check(ctx, d)
	}()

	opCtx, cancel := a.withTimeout(ctx)
	defer cancel()

	// Nothing runs inside application containers, so stopped ones are
	// updated too (upgradeApp refuses what it cannot do while stopped).
	if d.app() != nil {
		a.upgradeApp(opCtx, d, report, all)
		return false
	}
	if !d.running() {
		report.step("start", false, errors.New("guest is not running"), "")
		return false
	}
	if !a.managesPackages(d) {
		report.step("start", false, errors.New("the host is not managed (host.manage is false)"), "")
		return false
	}
	if !d.manageable() {
		report.step("start", false, errors.New(d.Guest.Unmanageable), "")
		return false
	}
	if d.Guest != nil && a.cfg.Guests.Snapshot && !a.snapshot(opCtx, d, report) {
		return false // never upgrade without the safety net
	}

	var mgr packages.Manager
	if action == ha.CmdUpgrade || action == ha.CmdUpgradePackages {
		var err error
		var unsupported packages.ErrUnsupported
		mgr, _, err = a.packageManager(opCtx, d)
		switch {
		case errors.As(err, &unsupported):
			report.step("packages", false, nil, "skipped: "+err.Error())
		case err != nil:
			report.step("packages", false, err, "")
			return false
		default:
			res, err := mgr.Upgrade(opCtx)
			if !report.step("packages", res.Changed, err, res.Summary) {
				return false
			}
			if a.cfg.Upgrade.Cleanup {
				report.step("package cleanup", false, mgr.Cleanup(opCtx), "")
			}
		}
	}

	if (action == ha.CmdUpgrade || action == ha.CmdUpgradeContainers) && a.cfg.Upgrade.Containers {
		a.upgradeContainers(opCtx, d, report)
	}

	if mgr == nil {
		return false
	}
	reboot, err := mgr.RebootRequired(opCtx)
	if err != nil || !reboot {
		return false
	}
	if d.Host {
		report.step("reboot", false, nil, "reboot required")
		return true
	}
	if !a.cfg.Upgrade.RestartGuests {
		report.step("restart", false, nil, "restart required; automatic restarts are off")
		return false
	}
	if a.rebootExcluded(d) {
		report.step("restart", false, nil, "restart required; the guest is excluded from reboots")
		return false
	}
	report.step("restart", true, d.Provider.Restart(opCtx, *d.Guest, a.cfg.Guests.StopTimeout), "restarted because a reboot was required")
	return false
}

// snapshot takes the pre-upgrade snapshot of a guest.
func (a *Agent) snapshot(ctx context.Context, d *Device, report *RunReport) bool {
	name := SnapshotPrefix + a.now().UTC().Format("20060102-150405")
	err := d.Provider.CreateSnapshot(ctx, *d.Guest, name, a.now().Add(a.cfg.Guests.SnapshotKeep))
	return report.step("snapshot", false, err, name)
}

// podman returns the podman instance managed on a device: the configured one
// on the host, the one in PATH on guests.
func (a *Agent) podman(d *Device) containers.Podman {
	if !d.Host {
		return containers.Podman{}
	}
	return containers.Podman{Path: a.cfg.Containers.Podman.Path, User: a.cfg.Containers.Podman.User}
}

// engine returns a device's container engine by name.
func (a *Agent) engine(ctx context.Context, d *Device, name string) (*containers.Engine, error) {
	if name == "podman" {
		return a.podman(d).Engine(ctx, d.Runner)
	}
	return containers.NewEngine(name, d.Runner), nil
}

func (a *Agent) upgradeContainers(ctx context.Context, d *Device, report *RunReport) {
	engines, err := containers.Detect(ctx, d.Runner, a.podman(d))
	if err != nil {
		report.step("podman", false, err, "")
	}
	for _, e := range engines {
		cs, err := e.Containers(ctx)
		if !report.step(e.Name+" containers", false, err, "") {
			continue
		}
		for _, p := range containers.Projects(cs) {
			changed, err := e.UpgradeProject(ctx, p)
			detail := "up to date"
			if changed {
				detail = "recreated with newer images"
			}
			report.step(fmt.Sprintf("%s compose %s", e.Name, p.Name), changed, err, detail)
		}
		autoUpdate := false
		for _, c := range cs {
			autoUpdate = autoUpdate || c.AutoUpdate
		}
		if autoUpdate {
			changed, err := e.AutoUpdate(ctx)
			report.step("podman auto-update", changed, err, "")
		}
		if a.cfg.Upgrade.Cleanup {
			changed, err := e.PruneImages(ctx)
			report.step(e.Name+" image prune", changed, err, "")
		}
	}
}

// upgradeAll upgrades the host first, then every running guest, then
// reboots the host if needed and allowed.
func (a *Agent) upgradeAll(ctx context.Context, trigger string) {
	a.log.Info("upgrade of host and guests starting", "trigger", trigger)
	hostReboot := false
	if a.cfg.Host.Manage {
		hostReboot = a.upgrade(ctx, a.host, ha.CmdUpgrade, trigger, true)
	}
	for _, d := range a.deviceList() {
		if d.Host || !a.upgradable(d) {
			continue
		}
		if ctx.Err() != nil {
			return
		}
		a.upgrade(ctx, d, ha.CmdUpgrade, trigger, true)
	}
	if hostReboot && a.cfg.Upgrade.RebootHost {
		a.rebootHost(ctx, trigger)
	}
}

// upgradable reports whether upgrade_all upgrades a guest: running guests,
// and application containers that can be rebuilt, also when stopped. Those
// that cannot (Proxmox VE only reports their updates) are left out.
func (a *Agent) upgradable(d *Device) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	if app := d.app(); app != nil && app.ComposeProject == "" {
		return a.appInstallable(d)
	}
	return d.running()
}

func (a *Agent) rebootHost(ctx context.Context, trigger string) {
	a.log.Warn("rebooting host", "trigger", trigger)
	a.setOperation(a.host, "rebooting")
	a.saveState()
	if err := a.reboot(ctx); err != nil {
		a.log.Error("host reboot failed", "err", err)
		a.setOperation(a.host, "")
	}
}

func (a *Agent) restartGuest(ctx context.Context, d *Device, trigger string) {
	report := &RunReport{Trigger: trigger, Action: ha.CmdReboot, Started: a.now()}
	a.setOperation(d, "restarting")
	ctx, cancel := a.withTimeout(ctx)
	defer cancel()
	report.step("restart", true, d.Provider.Restart(ctx, *d.Guest, a.cfg.Guests.StopTimeout), "")
	a.finish(ctx, d, report)
}

func (a *Agent) restore(ctx context.Context, d *Device, trigger string) {
	report := &RunReport{Trigger: trigger, Action: ha.CmdRestore, Started: a.now()}
	a.setOperation(d, "restoring")
	ctx, cancel := a.withTimeout(ctx)
	defer cancel()
	snaps, err := d.Provider.Snapshots(ctx, *d.Guest)
	name := ""
	for _, s := range snaps {
		if strings.HasPrefix(s.Name, SnapshotPrefix) {
			name = s.Name
			break
		}
	}
	switch {
	case err != nil:
		report.step("restore", false, err, "")
	case name == "":
		report.step("restore", false, errors.New("no pre-upgrade snapshot found"), "")
	default:
		report.step("restore", true, d.Provider.RestoreSnapshot(ctx, *d.Guest, name), name)
	}
	a.finish(ctx, d, report)
}

func (a *Agent) finish(ctx context.Context, d *Device, report *RunReport) {
	report.Finished = a.now()
	report.Success = true
	for _, s := range report.Steps {
		report.Success = report.Success && s.OK
	}
	a.mu.Lock()
	d.LastRun = report
	d.Operation = ""
	a.mu.Unlock()
	a.saveState()
	a.log.Info(report.Action+" finished", "device", d.ID, "success", report.Success)
	// The guest's status changed; platform events trigger a sync, and the
	// check below refreshes everything else.
	a.check(ctx, d)
}
