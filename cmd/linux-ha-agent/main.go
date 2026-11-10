// Command linux-ha-agent publishes update information for a Linux host and
// its Incus guests to Home Assistant over MQTT, and runs upgrades on request
// or on a schedule.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"runtime/debug"
	"strings"
	"syscall"
	"time"

	"github.com/lvlie/linux-ha-agent/internal/agent"
	"github.com/lvlie/linux-ha-agent/internal/config"
	"github.com/lvlie/linux-ha-agent/internal/containers"
	"github.com/lvlie/linux-ha-agent/internal/guest"
	"github.com/lvlie/linux-ha-agent/internal/incus"
	"github.com/lvlie/linux-ha-agent/internal/mqtt"
	"github.com/lvlie/linux-ha-agent/internal/proxmox"
	"github.com/lvlie/linux-ha-agent/internal/runner"
)

// version is set at build time with -ldflags "-X main.version=..."; builds
// with `go install module@version` report the module version instead.
var version = "dev"

func init() {
	if version != "dev" {
		return
	}
	if info, ok := debug.ReadBuildInfo(); ok && info.Main.Version != "" && info.Main.Version != "(devel)" {
		version = info.Main.Version
	}
}

const defaultIncusSocket = "/var/lib/incus/unix.socket"

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "linux-ha-agent:", err)
		os.Exit(1)
	}
}

func run() error {
	configPath := flag.String("config", "/etc/linux-ha-agent/config.yaml", "configuration file")
	showVersion := flag.Bool("version", false, "print the version and exit")
	checkConfig := flag.Bool("check-config", false, "validate the configuration and exit")
	once := flag.Bool("once", false, "check all devices once, print their state as JSON and exit (no MQTT)")
	flag.Parse()

	if *showVersion {
		fmt.Println(version)
		return nil
	}
	cfg, err := config.Load(*configPath)
	if err != nil {
		return err
	}
	if *checkConfig {
		for _, w := range cfg.Warnings {
			fmt.Println("warning:", w)
		}
		fmt.Println("configuration is valid")
		return nil
	}

	var level slog.Level
	if err := level.UnmarshalText([]byte(cfg.LogLevel)); err != nil {
		return err
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))
	slog.SetDefault(log)
	for _, w := range cfg.Warnings {
		log.Warn(w)
	}

	hostname, err := os.Hostname()
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	providers, err := guestProviders(ctx, cfg, log)
	if err != nil {
		return err
	}

	opts := agent.Options{
		Config:    cfg,
		Providers: providers,
		Local:     runner.Local{},
		Registry:  &containers.Registry{},
		Hostname:  hostname,
		Version:   version,
		Logger:    log,
	}
	// systemd sets RUNTIME_DIRECTORY from the unit's RuntimeDirectory=.
	if dir := os.Getenv("RUNTIME_DIRECTORY"); dir != "" && !*once {
		opts.OperationMarker = filepath.Join(dir, "operation")
	}
	if *once {
		return runOnce(ctx, opts)
	}

	var a *agent.Agent
	ready := make(chan struct{})
	client, err := mqtt.Connect(ctx, cfg.MQTT, mqtt.Options{
		ClientID:    clientID(cfg, hostname),
		WillTopic:   cfg.HomeAssistant.BaseTopic + "/" + hostID(cfg, hostname) + "/status",
		WillPayload: "offline",
		Logger:      log,
		OnConnect: func() {
			<-ready
			a.Reconnected()
		},
	})
	if err != nil {
		return err
	}
	defer client.Disconnect()
	opts.MQTT = client
	a, err = agent.New(opts)
	if err != nil {
		return err
	}
	close(ready)
	platforms := make([]string, 0, len(providers))
	for _, p := range providers {
		platforms = append(platforms, p.Platform())
	}
	log.Info("linux-ha-agent starting", "version", version, "host", hostname, "host_id", hostID(cfg, hostname), "guests", platforms)
	return a.Run(ctx)
}

func hostID(cfg config.Config, hostname string) string {
	name := cfg.Host.Name
	if name == "" {
		name = hostname
	}
	return agent.HostID(name)
}

func clientID(cfg config.Config, hostname string) string {
	if cfg.MQTT.ClientID != "" {
		return cfg.MQTT.ClientID
	}
	return "linux-ha-agent-" + hostID(cfg, hostname)
}

// guestProviders connects to the virtualisation platforms on this machine.
func guestProviders(ctx context.Context, cfg config.Config, log *slog.Logger) ([]guest.Provider, error) {
	var providers []guest.Provider
	inc, err := connectIncus(cfg.Incus, log)
	if err != nil {
		return nil, err
	}
	if inc != nil {
		providers = append(providers, inc)
	}
	pve, err := connectProxmox(ctx, cfg.Proxmox, log)
	if err != nil {
		return nil, err
	}
	if pve != nil {
		providers = append(providers, pve)
	}
	return providers, nil
}

func connectProxmox(ctx context.Context, cfg config.Proxmox, log *slog.Logger) (*proxmox.Provider, error) {
	local := runner.Local{}
	switch cfg.Enabled {
	case "false":
		return nil, nil
	case "auto":
		if !proxmox.Detect(ctx, local) {
			return nil, nil
		}
	}
	p, err := proxmox.New(ctx, local, cfg.Node, cfg.PollInterval)
	if err != nil {
		return nil, err
	}
	log.Info("managing proxmox ve guests", "node", p.Node())
	return p, nil
}

func connectIncus(cfg config.Incus, log *slog.Logger) (*incus.API, error) {
	if cfg.Enabled == "false" {
		return nil, nil
	}
	socket := cfg.Socket
	if socket == "" {
		socket = os.Getenv("INCUS_SOCKET")
	}
	if socket == "" {
		socket = defaultIncusSocket
	}
	if cfg.Enabled == "auto" {
		if _, err := os.Stat(socket); errors.Is(err, fs.ErrNotExist) { //nolint:gosec // the socket path comes from the configuration
			log.Debug("no incus socket found", "socket", socket)
			return nil, nil
		}
	}
	client, err := incus.Connect(socket)
	if err != nil {
		return nil, err
	}
	return client, nil
}

func runOnce(ctx context.Context, opts agent.Options) error {
	memory := mqtt.NewMemory()
	opts.MQTT = memory
	// Keep the real state file untouched.
	dir, err := os.MkdirTemp("", "linux-ha-agent-once")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	opts.Config.StateFile = filepath.Join(dir, "state.json")
	a, err := agent.New(opts)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, 2*opts.Config.Upgrade.CommandTimeout+time.Minute)
	defer cancel()
	if err := a.Start(ctx); err != nil {
		return err
	}
	a.WaitIdle()
	out := map[string]json.RawMessage{}
	for _, topic := range memory.RetainedTopics(a.Topics().Base + "/" + a.Topics().HostID + "/+/state") {
		payload, _ := memory.Retained(topic)
		parts := strings.Split(topic, "/")
		out[parts[len(parts)-2]] = payload
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(out)
}
