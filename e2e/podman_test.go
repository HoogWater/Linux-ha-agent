//go:build e2e

package e2e

import (
	"encoding/json"
	"os"
	"os/exec"
	"os/user"
	"slices"
	"strings"
	"testing"
	"time"
)

// The podman end-to-end test runs on a host prepared by
// tests/e2e/setup-podman.sh with the agent configured by
// tests/e2e/agent-podman.yaml; see .github/workflows/e2e-podman.yml.
var (
	podmanUser = os.Getenv("E2E_PODMAN_USER")
	podmanPath = env("E2E_PODMAN_PATH", "/usr/bin/podman")
)

// podman runs the real podman as user ("" for root) and returns its output.
func podman(t *testing.T, as string, args ...string) string {
	t.Helper()
	cmd := exec.Command(podmanPath, args...)
	if as != "" {
		u, err := user.Lookup(as)
		if err != nil {
			t.Fatal(err)
		}
		cmd = exec.Command("runuser", append([]string{"-u", as, "--", podmanPath}, args...)...)
		cmd.Env = append(os.Environ(), "XDG_RUNTIME_DIR=/run/user/"+u.Uid)
		cmd.Dir = u.HomeDir
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("podman %s (as %q): %v\n%s", strings.Join(args, " "), as, err, out)
	}
	return strings.TrimSpace(string(out))
}

// sleeper is the ID of the compose container, which is replaced on upgrades.
func sleeper(t *testing.T) string {
	t.Helper()
	return podman(t, podmanUser, "ps", "-q", "--no-trunc", "--filter", "label=com.docker.compose.service=sleeper")
}

func TestPodmanHost(t *testing.T) {
	if podmanUser == "" {
		t.Skip("E2E_PODMAN_USER is not set: no podman host")
	}
	b := homeAssistant(t)
	// The keys of the containers: podman-compose names its containers
	// app_sleeper_1 or app-sleeper-1 depending on the version.
	const composeKey, quadletKey = "podman_app_sleeper_1", "podman_autoupd"
	rootStarted := podman(t, "", "inspect", "--format", "{{.State.StartedAt}}", "root-only")

	ctrs := func() map[string]any {
		c, _ := b.state(hostID)["ctr"].(map[string]any)
		return c
	}
	ctr := func(key string) map[string]any {
		c, _ := ctrs()[key].(map[string]any)
		return c
	}
	upToDate := func(key string) bool {
		u, _ := ctr(key)["update"].(map[string]any)
		return u != nil && u["installed_version"] == u["latest_version"]
	}

	t.Run("discovery", func(t *testing.T) {
		eventually(t, 2*time.Minute, "discovery of the host", func() bool {
			_, ok := b.get("homeassistant/device/" + hostID + "/" + hostID + "/config")
			return ok
		})
		if p, _ := b.get(base + "/status"); string(p) != "online" {
			t.Fatalf("availability %q", p)
		}
	})

	t.Run("the configured podman instance is checked", func(t *testing.T) {
		eventually(t, 20*time.Minute, "check of the host", func() bool {
			st := b.state(hostID)
			return st != nil && st["last_check"] != nil && st["in_progress"] == false
		})
		st := b.state(hostID)
		if st["check_error"] != "" {
			// The wrong podman in PATH or a failing runuser shows up here.
			t.Fatalf("check error: %v", st["check_error"])
		}
		engines, _ := json.Marshal(st["engines"])
		if !strings.Contains(string(engines), `"podman"`) {
			t.Fatalf("podman not found: engines %s", engines)
		}
		var podmanCtrs []string
		for key := range ctrs() {
			if strings.HasPrefix(key, "podman_") {
				podmanCtrs = append(podmanCtrs, key)
			}
		}
		slices.Sort(podmanCtrs)
		// Only the user's containers: not root's root-only.
		if !slices.Equal(podmanCtrs, []string{composeKey, quadletKey}) {
			t.Fatalf("podman containers %v, want the user's %s and %s", podmanCtrs, composeKey, quadletKey)
		}
		for _, key := range podmanCtrs {
			c := ctr(key)
			if c["running"] != true || upToDate(key) {
				t.Errorf("%s should run and have a newer image: %v", key, c)
			}
		}
		cmps := b.discovery(hostID)
		for _, key := range podmanCtrs {
			if cmps["ctr_"+key+"_update"]["payload_install"] != "upgrade_container "+key || cmps["ctr_"+key+"_restart"] == nil {
				t.Errorf("%s: entities missing", key)
			}
		}
	})

	t.Run("upgrade a compose container", func(t *testing.T) {
		before := podman(t, podmanUser, "inspect", "--format", "{{.Image}}", sleeper(t))
		since := time.Now()
		b.command(t, hostID, "upgrade_container "+composeKey)
		run := waitForRun(t, b, hostID, since)
		steps, _ := json.Marshal(run["steps"])
		t.Logf("steps: %s", steps)
		if after := podman(t, podmanUser, "inspect", "--format", "{{.Image}}", sleeper(t)); after == before {
			t.Error("container was not recreated on the newer image")
		}
		eventually(t, 5*time.Minute, "compose container up to date", func() bool { return upToDate(composeKey) })
	})

	t.Run("restart a container", func(t *testing.T) {
		started := func() string {
			return podman(t, podmanUser, "inspect", "--format", "{{.State.StartedAt}}", sleeper(t))
		}
		before := started()
		since := time.Now()
		b.command(t, hostID, "restart_container "+composeKey)
		waitForRun(t, b, hostID, since)
		if started() == before {
			t.Error("container was not restarted")
		}
	})

	t.Run("upgrade all containers with auto-update and cleanup", func(t *testing.T) {
		before := podman(t, podmanUser, "inspect", "--format", "{{.Image}}", "autoupd")
		since := time.Now()
		b.command(t, hostID, "upgrade_containers")
		run := waitForRun(t, b, hostID, since)
		steps, _ := json.Marshal(run["steps"])
		t.Logf("steps: %s", steps)
		for _, step := range []string{"podman auto-update", "podman image prune"} {
			if !strings.Contains(string(steps), step) {
				t.Errorf("step %q missing", step)
			}
		}
		// auto-update restarts the Quadlet's systemd user unit over D-Bus.
		if after := podman(t, podmanUser, "inspect", "--format", "{{.Image}}", "autoupd"); after == before {
			t.Error("auto-update did not update the Quadlet container")
		}
		if out := podman(t, podmanUser, "images", "-q", "--filter", "dangling=true"); out != "" {
			t.Errorf("dangling images left: %s", out)
		}
		eventually(t, 5*time.Minute, "all containers up to date", func() bool {
			return upToDate(composeKey) && upToDate(quadletKey) && ctr(quadletKey)["running"] == true
		})
	})

	t.Run("root's containers are left alone", func(t *testing.T) {
		if got := podman(t, "", "inspect", "--format", "{{.State.StartedAt}}", "root-only"); got != rootStarted {
			t.Errorf("root's container was restarted: %s, was %s", got, rootStarted)
		}
		if _, ok := ctrs()["podman_root_only"]; ok {
			t.Error("root's container is reported")
		}
	})
}
