// Package config loads and validates the agent configuration.
package config

import (
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/robfig/cron/v3"
	"go.yaml.in/yaml/v3"
)

// Config is the agent configuration file.
type Config struct {
	MQTT          MQTT          `yaml:"mqtt"`
	HomeAssistant HomeAssistant `yaml:"homeassistant"`
	Host          Host          `yaml:"host"`
	Guests        Guests        `yaml:"guests"`
	Incus         Incus         `yaml:"incus"`
	Proxmox       Proxmox       `yaml:"proxmox"`
	Containers    Containers    `yaml:"containers"`
	Checks        Checks        `yaml:"checks"`
	Upgrade       Upgrade       `yaml:"upgrade"`
	NixOS         NixOS         `yaml:"nixos"`
	Permissions   Permissions   `yaml:"permissions"`
	Commands      Commands      `yaml:"commands"`
	StateFile     string        `yaml:"state_file"`
	LogLevel      string        `yaml:"log_level"`
	// Warnings are about deprecated settings; they do not stop the agent.
	Warnings []string `yaml:"-"`
}

// MQTT connection settings.
type MQTT struct {
	// URL of the broker: tcp://host:1883, ssl://host:8883 (or mqtts://).
	URL                string `yaml:"url"`
	Username           string `yaml:"username"`
	PasswordFile       string `yaml:"password_file"`
	CAFile             string `yaml:"ca_file"`
	ClientCertFile     string `yaml:"client_cert_file"`
	ClientKeyFile      string `yaml:"client_key_file"`
	InsecureSkipVerify bool   `yaml:"insecure_skip_verify"`
	ClientID           string `yaml:"client_id"`
}

// HomeAssistant discovery settings.
type HomeAssistant struct {
	DiscoveryPrefix string `yaml:"discovery_prefix"`
	BaseTopic       string `yaml:"base_topic"`
}

// Host settings.
type Host struct {
	// Name of the host device in Home Assistant; defaults to the hostname.
	Name string `yaml:"name"`
	// Manage the host's own packages and containers.
	Manage bool `yaml:"manage"`
}

// Guests are the settings shared by all virtualisation platforms.
type Guests struct {
	Types        []string      `yaml:"types"`
	Exclude      []string      `yaml:"exclude"`
	Snapshot     bool          `yaml:"snapshot_before_upgrade"`
	SnapshotKeep time.Duration `yaml:"snapshot_expiry"`
	StopTimeout  time.Duration `yaml:"restart_timeout"`
}

// Incus settings.
type Incus struct {
	// Enabled is "auto" (use Incus when the socket exists), "true" or "false".
	Enabled  string   `yaml:"enabled"`
	Socket   string   `yaml:"socket"`
	Projects []string `yaml:"projects"`
	// ComposeDirs are searched (with their direct subdirectories) for the
	// compose files of incus-compose projects.
	ComposeDirs []string `yaml:"compose_dirs"`
}

// Proxmox VE settings.
type Proxmox struct {
	// Enabled is "auto" (use Proxmox VE when /etc/pve exists), "true" or "false".
	Enabled string `yaml:"enabled"`
	// Node name; defaults to the short host name.
	Node string `yaml:"node"`
	// How often to look for guests that were created, removed, started or stopped.
	PollInterval time.Duration `yaml:"poll_interval"`
}

// Containers select the container engines of the host. They do not apply to
// guests, where the engines are always found in PATH.
type Containers struct {
	Podman Podman `yaml:"podman"`
}

// Podman selects the podman instance managed on the host.
type Podman struct {
	// Path of the podman executable; empty finds podman in PATH. Use it
	// when several versions are installed, e.g. /usr/local/bin/podman.
	Path string `yaml:"path"`
	// User whose rootless podman containers are managed; empty manages the
	// containers of root. Needs runuser (util-linux) and lingering
	// (loginctl enable-linger <user>) so the user's runtime directory exists.
	User string `yaml:"user"`
}

// Checks control how often state is refreshed.
type Checks struct {
	Interval        time.Duration `yaml:"interval"`
	ContainerImages bool          `yaml:"container_images"`
	// StatsInterval is how often CPU, memory, disk, load and uptime are
	// refreshed; 0 only refreshes them with the update checks.
	StatsInterval time.Duration `yaml:"stats_interval"`
	// ContainerEntities adds entities for every docker/podman container.
	ContainerEntities bool `yaml:"container_entities"`
}

// Upgrade controls upgrades.
type Upgrade struct {
	// Schedule is a cron expression (optionally prefixed with CRON_TZ=...)
	// for automatic upgrades of everything. Empty disables the schedule.
	Schedule       string        `yaml:"schedule"`
	AutoUpgrade    bool          `yaml:"auto_upgrade"`
	RebootHost     bool          `yaml:"reboot_host"`
	RestartGuests  bool          `yaml:"restart_guests"`
	Cleanup        bool          `yaml:"cleanup"`
	Containers     bool          `yaml:"containers"`
	CommandTimeout time.Duration `yaml:"command_timeout"`
}

// NixOS settings.
type NixOS struct {
	// Flake to rebuild from, e.g. "github:me/nix-config". Empty uses channels.
	// The hostname is appended as #<hostname> unless the flake has a fragment.
	Flake       string `yaml:"flake"`
	GCOlderThan string `yaml:"gc_older_than"`
}

// Permissions decide which actions Home Assistant may trigger.
type Permissions struct {
	Upgrade bool `yaml:"upgrade"`
	// Reboot is deprecated: it enables both RebootHost and RebootGuests.
	Reboot       bool `yaml:"reboot"`
	RebootHost   bool `yaml:"reboot_host"`
	RebootGuests bool `yaml:"reboot_guests"`
	// RebootExclude are guests that are never rebooted, even with
	// RebootGuests or upgrade.restart_guests; see MatchGuest for the patterns.
	RebootExclude     []string `yaml:"reboot_exclude"`
	Restore           bool     `yaml:"restore"`
	RestartContainers bool     `yaml:"restart_containers"`
}

// Commands are shell commands whose output is published, like LNXlink's
// bash module. They always run as the user nobody, without privileges.
type Commands struct {
	// Timeout of one command.
	Timeout time.Duration `yaml:"timeout"`
	// AllowAny adds a text entity to every machine that runs whatever is
	// typed into it (as nobody) and shows the output in a sensor.
	AllowAny bool `yaml:"allow_any"`
	// Machines that get the "Run command" entity: see Command.Machines.
	AllowAnyMachines []string  `yaml:"allow_any_machines"`
	Expose           []Command `yaml:"expose"`
}

// Command is a command published as a sensor.
type Command struct {
	Name    string `yaml:"name"`
	Command string `yaml:"command"`
	// Type is sensor (the output is the state) or binary_sensor (on when the
	// command exits with 0).
	Type        string        `yaml:"type"`
	Unit        string        `yaml:"unit"`
	Icon        string        `yaml:"icon"`
	DeviceClass string        `yaml:"device_class"`
	Interval    time.Duration `yaml:"interval"`
	// Machines to run it on: "host", "guests" (all guests) or guest patterns
	// (see MatchGuest). Defaults to the host.
	Machines []string `yaml:"machines"`
}

// Key identifies the command in entity IDs and the state.
func (c Command) Key() string {
	return "cmd_" + slug(c.Name)
}

var nonSlug = regexp.MustCompile(`[^a-z0-9_]+`)

func slug(s string) string {
	return strings.Trim(nonSlug.ReplaceAllString(strings.ToLower(s), "_"), "_")
}

// RunsOn reports whether a command with these machines runs on the host
// (guest nil) or on a guest.
func RunsOn(machines []string, g *GuestRef) bool {
	if g == nil {
		return slices.Contains(machines, "host")
	}
	if slices.Contains(machines, "guests") {
		return true
	}
	return g.Matches(machines)
}

// GuestRef describes a guest for pattern matching.
type GuestRef struct {
	Project, Name, ID string
	// Type is "container" or "virtual-machine".
	Type string
}

// Matches reports whether the guest matches one of the patterns. A pattern
// is "type:container" (alias "type:lxc"), "type:virtual-machine" (alias
// "type:vm"), or a shell-style wildcard matched against the name,
// "project/name" (Incus) and the VMID (Proxmox).
func (g GuestRef) Matches(patterns []string) bool {
	candidates := []string{g.Name}
	if g.Project != "" {
		candidates = append(candidates, g.Project+"/"+g.Name)
	}
	if g.ID != "" {
		candidates = append(candidates, g.ID)
	}
	for _, pattern := range patterns {
		if t, ok := strings.CutPrefix(pattern, "type:"); ok {
			if guestTypes[t] == g.Type {
				return true
			}
			continue
		}
		for _, candidate := range candidates {
			if ok, _ := path.Match(pattern, candidate); ok {
				return true
			}
		}
	}
	return false
}

var guestTypes = map[string]string{
	"container": "container", "lxc": "container",
	"virtual-machine": "virtual-machine", "vm": "virtual-machine",
}

func validatePatterns(field string, patterns []string, keywords ...string) []error {
	var errs []error
	for _, pattern := range patterns {
		if slices.Contains(keywords, pattern) {
			continue
		}
		if t, ok := strings.CutPrefix(pattern, "type:"); ok {
			if guestTypes[t] == "" {
				errs = append(errs, fmt.Errorf("%s: unknown guest type in %q (container, lxc, virtual-machine or vm)", field, pattern))
			}
			continue
		}
		if _, err := path.Match(pattern, ""); err != nil {
			errs = append(errs, fmt.Errorf("%s: bad pattern %q: %w", field, pattern, err))
		}
	}
	return errs
}

// Default returns the configuration with all defaults applied.
func Default() Config {
	return Config{
		HomeAssistant: HomeAssistant{DiscoveryPrefix: "homeassistant", BaseTopic: "linux-ha-agent"},
		Host:          Host{Manage: true},
		Guests: Guests{
			Types:        []string{"container", "virtual-machine"},
			Snapshot:     true,
			SnapshotKeep: 7 * 24 * time.Hour,
			StopTimeout:  2 * time.Minute,
		},
		Incus:   Incus{Enabled: "auto", ComposeDirs: []string{"/root"}},
		Proxmox: Proxmox{Enabled: "auto", PollInterval: time.Minute},
		Checks:  Checks{Interval: 6 * time.Hour, ContainerImages: true, StatsInterval: 5 * time.Minute, ContainerEntities: true},
		Upgrade: Upgrade{
			AutoUpgrade:    true,
			RestartGuests:  true,
			Cleanup:        true,
			Containers:     true,
			CommandTimeout: time.Hour,
		},
		NixOS:       NixOS{GCOlderThan: "14d"},
		Permissions: Permissions{Upgrade: true},
		Commands:    Commands{Timeout: 30 * time.Second, AllowAnyMachines: []string{"host"}},
		StateFile:   "/var/lib/linux-ha-agent/state.json",
		LogLevel:    "info",
	}
}

// Load reads and validates a configuration file.
func Load(file string) (Config, error) {
	data, err := os.ReadFile(file) //nolint:gosec // the path comes from the command line
	if err != nil {
		return Config{}, fmt.Errorf("reading config: %w", err)
	}
	return Parse(data)
}

// Parse parses and validates configuration data.
func Parse(data []byte) (Config, error) {
	cfg := Default()
	dec := yaml.NewDecoder(strings.NewReader(string(data)))
	dec.KnownFields(true)
	// An empty file is fine: it leaves the defaults.
	if err := dec.Decode(&cfg); err != nil && !errors.Is(err, io.EOF) {
		return Config{}, fmt.Errorf("parsing config: %w", err)
	}
	if cfg.Permissions.Reboot {
		cfg.Permissions.RebootHost = true
		cfg.Permissions.RebootGuests = true
		cfg.Warnings = append(cfg.Warnings, "permissions.reboot is deprecated: use permissions.reboot_host and permissions.reboot_guests")
	}
	for i := range cfg.Commands.Expose {
		c := &cfg.Commands.Expose[i]
		if c.Type == "" {
			c.Type = "sensor"
		}
		if c.Interval == 0 {
			c.Interval = 5 * time.Minute
		}
		if c.Machines == nil {
			c.Machines = []string{"host"}
		}
	}
	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func (c Commands) validate() []error {
	var errs []error
	if c.Timeout < time.Second || c.Timeout > 10*time.Minute {
		errs = append(errs, errors.New("commands.timeout must be between 1s and 10m"))
	}
	errs = append(errs, validatePatterns("commands.allow_any_machines", c.AllowAnyMachines, "host", "guests")...)
	keys := map[string]bool{}
	for i, cmd := range c.Expose {
		field := fmt.Sprintf("commands.expose[%d]", i)
		switch {
		case slug(cmd.Name) == "":
			errs = append(errs, fmt.Errorf("%s: name is required", field))
		case keys[cmd.Key()]:
			errs = append(errs, fmt.Errorf("%s: name %q is used twice", field, cmd.Name))
		}
		keys[cmd.Key()] = true
		if strings.TrimSpace(cmd.Command) == "" {
			errs = append(errs, fmt.Errorf("%s: command is required", field))
		}
		if cmd.Type != "sensor" && cmd.Type != "binary_sensor" {
			errs = append(errs, fmt.Errorf("%s: type must be sensor or binary_sensor, not %q", field, cmd.Type))
		}
		if cmd.Interval < 10*time.Second {
			errs = append(errs, fmt.Errorf("%s: interval must be at least 10s", field))
		}
		errs = append(errs, validatePatterns(field+".machines", cmd.Machines, "host", "guests")...)
	}
	return errs
}

var (
	topicSafe = regexp.MustCompile(`^[A-Za-z0-9_-]+(/[A-Za-z0-9_-]+)*$`)
	gcAge     = regexp.MustCompile(`^[0-9]+[dhm]$`)
	userName  = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.-]*$`)
)

// Validate checks the configuration for mistakes.
func (c *Config) Validate() error {
	var errs []error
	if c.MQTT.URL == "" {
		errs = append(errs, errors.New("mqtt.url is required"))
	} else if u, err := url.Parse(c.MQTT.URL); err != nil || !slices.Contains([]string{"tcp", "mqtt", "ssl", "tls", "mqtts", "ws", "wss"}, u.Scheme) || u.Host == "" {
		errs = append(errs, fmt.Errorf("mqtt.url %q must look like tcp://host:1883 or ssl://host:8883", c.MQTT.URL))
	}
	if c.MQTT.PasswordFile != "" && c.MQTT.Username == "" {
		errs = append(errs, errors.New("mqtt.password_file requires mqtt.username"))
	}
	if (c.MQTT.ClientCertFile == "") != (c.MQTT.ClientKeyFile == "") {
		errs = append(errs, errors.New("mqtt.client_cert_file and mqtt.client_key_file must be set together"))
	}
	if !topicSafe.MatchString(c.HomeAssistant.DiscoveryPrefix) {
		errs = append(errs, fmt.Errorf("homeassistant.discovery_prefix %q is not a valid topic", c.HomeAssistant.DiscoveryPrefix))
	}
	if !topicSafe.MatchString(c.HomeAssistant.BaseTopic) {
		errs = append(errs, fmt.Errorf("homeassistant.base_topic %q is not a valid topic", c.HomeAssistant.BaseTopic))
	}
	if !slices.Contains([]string{"auto", "true", "false"}, c.Incus.Enabled) {
		errs = append(errs, fmt.Errorf("incus.enabled must be auto, true or false, not %q", c.Incus.Enabled))
	}
	if !slices.Contains([]string{"auto", "true", "false"}, c.Proxmox.Enabled) {
		errs = append(errs, fmt.Errorf("proxmox.enabled must be auto, true or false, not %q", c.Proxmox.Enabled))
	}
	if c.Proxmox.PollInterval < 10*time.Second {
		errs = append(errs, errors.New("proxmox.poll_interval must be at least 10s"))
	}
	if p := c.Containers.Podman.Path; p != "" && !path.IsAbs(p) {
		errs = append(errs, fmt.Errorf("containers.podman.path %q must be an absolute path", p))
	}
	if u := c.Containers.Podman.User; u != "" && !userName.MatchString(u) {
		errs = append(errs, fmt.Errorf("containers.podman.user %q is not a valid user name", u))
	}
	for _, t := range c.Guests.Types {
		if t != "container" && t != "virtual-machine" {
			errs = append(errs, fmt.Errorf("guests.types: unknown type %q", t))
		}
	}
	errs = append(errs, validatePatterns("guests.exclude", c.Guests.Exclude)...)
	errs = append(errs, validatePatterns("permissions.reboot_exclude", c.Permissions.RebootExclude)...)
	errs = append(errs, c.Commands.validate()...)
	if c.Checks.Interval < time.Minute {
		errs = append(errs, errors.New("checks.interval must be at least 1m"))
	}
	if c.Checks.StatsInterval != 0 && c.Checks.StatsInterval < 30*time.Second {
		errs = append(errs, errors.New("checks.stats_interval must be 0 or at least 30s"))
	}
	if c.Upgrade.CommandTimeout < time.Minute {
		errs = append(errs, errors.New("upgrade.command_timeout must be at least 1m"))
	}
	if c.Upgrade.Schedule != "" {
		if _, err := ParseSchedule(c.Upgrade.Schedule); err != nil {
			errs = append(errs, fmt.Errorf("upgrade.schedule: %w", err))
		}
	}
	if c.NixOS.GCOlderThan != "" && !gcAge.MatchString(c.NixOS.GCOlderThan) {
		errs = append(errs, fmt.Errorf("nixos.gc_older_than %q must look like 14d", c.NixOS.GCOlderThan))
	}
	if !slices.Contains([]string{"debug", "info", "warn", "error"}, c.LogLevel) {
		errs = append(errs, fmt.Errorf("log_level must be debug, info, warn or error, not %q", c.LogLevel))
	}
	return errors.Join(errs...)
}

// ParseSchedule parses a standard 5-field cron expression, optionally
// prefixed with CRON_TZ=<zone>.
func ParseSchedule(spec string) (cron.Schedule, error) {
	return cron.ParseStandard(spec)
}

// Password reads the MQTT password file, if configured.
func (m MQTT) Password() (string, error) {
	if m.PasswordFile == "" {
		return "", nil
	}
	data, err := os.ReadFile(m.PasswordFile)
	if err != nil {
		return "", fmt.Errorf("reading mqtt password: %w", err)
	}
	return strings.TrimRight(string(data), "\r\n"), nil
}

// Excluded reports whether a guest matches one of the exclude patterns.
func (g Guests) Excluded(ref GuestRef) bool {
	return ref.Matches(g.Exclude)
}

// Includes reports whether guests of this type are managed.
func (g Guests) Includes(guestType string) bool {
	return slices.Contains(g.Types, guestType)
}

// InProject reports whether an Incus project is managed.
func (i Incus) InProject(project string) bool {
	return len(i.Projects) == 0 || slices.Contains(i.Projects, project)
}
