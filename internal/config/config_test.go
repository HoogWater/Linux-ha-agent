package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestDefaultsAndOverrides(t *testing.T) {
	cfg, err := Parse([]byte(`
mqtt:
  url: ssl://mqtt.lan:8883
  username: incus1
guests:
  exclude: [build-*, lab/scratch, "105"]
  snapshot_expiry: 72h
proxmox:
  node: pve1
checks:
  interval: 30m
upgrade:
  schedule: "CRON_TZ=Europe/Amsterdam 30 3 * * 0"
permissions:
  reboot: true
containers:
  podman:
    path: /usr/local/bin/podman
    user: mark
`))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Containers.Podman.Path != "/usr/local/bin/podman" || cfg.Containers.Podman.User != "mark" {
		t.Errorf("podman settings not parsed: %+v", cfg.Containers.Podman)
	}
	if cfg.HomeAssistant.DiscoveryPrefix != "homeassistant" || cfg.HomeAssistant.BaseTopic != "linux-ha-agent" {
		t.Errorf("discovery defaults not applied: %+v", cfg.HomeAssistant)
	}
	if cfg.Guests.SnapshotKeep != 72*time.Hour || cfg.Checks.Interval != 30*time.Minute {
		t.Errorf("durations not parsed: %v %v", cfg.Guests.SnapshotKeep, cfg.Checks.Interval)
	}
	if !cfg.Permissions.Upgrade || !cfg.Permissions.RebootHost || !cfg.Permissions.RebootGuests || len(cfg.Warnings) != 1 || cfg.Permissions.Restore {
		t.Errorf("permissions wrong: %+v", cfg.Permissions)
	}
	if !cfg.Host.Manage || cfg.Incus.Enabled != "auto" || !cfg.Guests.Snapshot || cfg.Proxmox.Enabled != "auto" ||
		cfg.Proxmox.Node != "pve1" || cfg.Proxmox.PollInterval != time.Minute {
		t.Errorf("defaults lost: %+v %+v %+v %+v", cfg.Host, cfg.Incus, cfg.Guests, cfg.Proxmox)
	}
}

func TestValidationErrors(t *testing.T) {
	_, err := Parse([]byte(`
mqtt:
  url: http://broker
  password_file: /x
homeassistant:
  base_topic: "bad topic/#"
incus:
  enabled: maybe
proxmox:
  enabled: sometimes
  poll_interval: 1s
guests:
  types: [container, pod]
checks:
  interval: 1s
upgrade:
  schedule: "every sunday"
nixos:
  gc_older_than: forever
containers:
  podman:
    path: bin/podman
    user: "mark; reboot"
log_level: loud
`))
	if err == nil {
		t.Fatal("expected errors")
	}
	for _, want := range []string{
		"mqtt.url", "password_file requires", "base_topic", "incus.enabled", "proxmox.enabled", "proxmox.poll_interval", `unknown type "pod"`,
		"checks.interval", "upgrade.schedule", "gc_older_than", "log_level",
		"containers.podman.path", "containers.podman.user",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("missing error about %q in:\n%v", want, err)
		}
	}
}

func TestUnknownFieldsRejected(t *testing.T) {
	_, err := Parse([]byte("mqtt:\n  url: tcp://b:1883\n  pasword_file: /typo\n"))
	if err == nil || !strings.Contains(err.Error(), "pasword_file") {
		t.Fatalf("expected unknown field error, got %v", err)
	}
}

func TestEmptyConfigNeedsBroker(t *testing.T) {
	_, err := Parse(nil)
	if err == nil || !strings.Contains(err.Error(), "mqtt.url is required") {
		t.Fatalf("got %v", err)
	}
}

func TestPassword(t *testing.T) {
	file := filepath.Join(t.TempDir(), "pw")
	if err := os.WriteFile(file, []byte("s3cret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	pw, err := MQTT{PasswordFile: file}.Password()
	if err != nil || pw != "s3cret" {
		t.Fatalf("got %q %v", pw, err)
	}
}

func TestGuestSelection(t *testing.T) {
	g := Guests{Types: []string{"container"}, Exclude: []string{"build-*", "lab/scratch", "105"}}
	i := Incus{Projects: []string{"default", "lab"}}
	cases := []struct {
		project, name, id, typ string
		want                   bool
	}{
		{"default", "web", "", "container", true},
		{"default", "build-42", "", "container", false},
		{"lab", "scratch", "", "container", false},
		{"default", "scratch", "", "container", true},
		{"default", "vm1", "", "virtual-machine", false},
		{"other", "web", "", "container", false},
		{"", "media", "104", "container", true},
		{"", "media", "105", "container", false},
	}
	for _, c := range cases {
		got := g.Includes(c.typ) && !g.Excluded(GuestRef{Project: c.project, Name: c.name, ID: c.id}) && (c.project == "" || i.InProject(c.project))
		if got != c.want {
			t.Errorf("%s/%s/%s (%s): got %v want %v", c.project, c.name, c.id, c.typ, got, c.want)
		}
	}
}

func TestRebootPermissions(t *testing.T) {
	cfg, err := Parse([]byte(`
mqtt: {url: tcp://b:1883}
permissions:
  reboot_guests: true
  reboot_exclude: [type:vm, "db-*", lab/critical]
`))
	if err != nil {
		t.Fatal(err)
	}
	p := cfg.Permissions
	if p.RebootHost || !p.RebootGuests || len(cfg.Warnings) != 0 {
		t.Fatalf("%+v %v", p, cfg.Warnings)
	}
	for ref, want := range map[GuestRef]bool{
		{Name: "web", Type: "container"}:                        false,
		{Name: "win", Type: "virtual-machine"}:                  true,
		{Name: "db-1", Type: "container"}:                       true,
		{Project: "lab", Name: "critical", Type: "container"}:   true,
		{Project: "other", Name: "critical", Type: "container"}: false,
	} {
		if got := ref.Matches(p.RebootExclude); got != want {
			t.Errorf("%+v: excluded %v, want %v", ref, got, want)
		}
	}
	_, err = Parse([]byte("mqtt: {url: tcp://b:1883}\npermissions: {reboot_exclude: [\"type:pod\", \"[\"]}\n"))
	if err == nil || !strings.Contains(err.Error(), "unknown guest type") || !strings.Contains(err.Error(), "bad pattern") {
		t.Fatalf("got %v", err)
	}
}

func TestCommands(t *testing.T) {
	cfg, err := Parse([]byte(`
mqtt: {url: tcp://b:1883}
commands:
  allow_any: true
  expose:
    - name: Failed units
      command: systemctl --failed --no-legend | wc -l
    - name: Backup ok
      command: test -f /backup/ok
      type: binary_sensor
      interval: 1h
      machines: [guests]
`))
	if err != nil {
		t.Fatal(err)
	}
	c := cfg.Commands
	if c.Timeout != 30*time.Second || !c.AllowAny || len(c.AllowAnyMachines) != 1 || c.AllowAnyMachines[0] != "host" {
		t.Errorf("defaults: %+v", c)
	}
	first, second := c.Expose[0], c.Expose[1]
	if first.Key() != "cmd_failed_units" || first.Type != "sensor" || first.Interval != 5*time.Minute || len(first.Machines) != 1 {
		t.Errorf("first: %+v", first)
	}
	vm := GuestRef{Name: "x", Type: "virtual-machine"}
	if !RunsOn(first.Machines, nil) || RunsOn(first.Machines, &vm) || RunsOn(second.Machines, nil) || !RunsOn(second.Machines, &vm) {
		t.Error("machines")
	}

	_, err = Parse([]byte(`
mqtt: {url: tcp://b:1883}
commands:
  timeout: 1h
  allow_any_machines: ["type:pod"]
  expose:
    - name: A
      command: x
    - name: a
      command: ""
      type: switch
      interval: 1s
`))
	if err == nil {
		t.Fatal("expected errors")
	}
	for _, want := range []string{"commands.timeout", "allow_any_machines", "used twice", "command is required", "type must be", "interval must be"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("missing %q in %v", want, err)
		}
	}
}
