package agent

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/lvlie/linux-ha-agent/internal/config"
	"github.com/lvlie/linux-ha-agent/internal/ha"
	"github.com/lvlie/linux-ha-agent/internal/mqtt"
	"github.com/lvlie/linux-ha-agent/internal/runner"
)

// blockingRunner holds commands containing pattern until release is closed,
// and fails them with the context's error once their context is cancelled,
// like exec.CommandContext kills a command.
type blockingRunner struct {
	*runner.Fake
	pattern string
	started chan struct{}
	release chan struct{}

	mu        sync.Mutex
	cancelled bool
	blocked   bool
	// after are the commands run after the blocking one started.
	after []string
}

func (b *blockingRunner) Run(ctx context.Context, cmd runner.Cmd) (runner.Result, error) {
	if !strings.Contains(cmd.String(), b.pattern) {
		b.mu.Lock()
		if b.blocked {
			b.after = append(b.after, cmd.String())
		}
		b.mu.Unlock()
		return b.Fake.Run(ctx, cmd)
	}
	b.mu.Lock()
	b.blocked = true
	b.mu.Unlock()
	close(b.started)
	select {
	case <-b.release:
		return b.Fake.Run(ctx, cmd)
	case <-ctx.Done():
		b.mu.Lock()
		b.cancelled = true
		b.mu.Unlock()
		return runner.Result{}, ctx.Err()
	}
}

func (b *blockingRunner) wasCancelled() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.cancelled
}

// ranAfter reports whether a command containing pattern ran after the
// blocking command started.
func (b *blockingRunner) ranAfter(pattern string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, c := range b.after {
		if strings.Contains(c, pattern) {
			return true
		}
	}
	return false
}

type shutdownEnv struct {
	agent  *Agent
	mqtt   *mqtt.Memory
	runner *blockingRunner
	marker string
	cancel context.CancelFunc
	// stopped is closed when Run returned, with its error in runErr.
	stopped chan struct{}
	runErr  error
}

// newShutdownEnv runs an agent for a host whose package upgrade blocks.
func newShutdownEnv(t *testing.T) *shutdownEnv {
	t.Helper()
	cfg := config.Default()
	cfg.MQTT.URL = "tcp://broker:1883"
	cfg.StateFile = filepath.Join(t.TempDir(), "state.json")
	cfg.Permissions = config.Permissions{Upgrade: true}
	f := &runner.Fake{}
	stats(f, "6.8.0-45-generic", "6.8.0-45-generic")
	(&machine{}).ubuntu(f)
	e := &shutdownEnv{
		mqtt: mqtt.NewMemory(),
		runner: &blockingRunner{Fake: f, pattern: "--force-confdef",
			started: make(chan struct{}), release: make(chan struct{})},
		marker:  filepath.Join(t.TempDir(), "operation"),
		stopped: make(chan struct{}),
	}
	a, err := New(Options{
		Config: cfg, MQTT: e.mqtt, Local: e.runner, Hostname: "Lab1", Version: "1.2.3",
		Logger: quiet, OperationMarker: e.marker,
	})
	if err != nil {
		t.Fatal(err)
	}
	e.agent = a
	ctx, cancel := context.WithCancel(context.Background())
	e.cancel = cancel
	t.Cleanup(func() {
		cancel()
		select {
		case <-e.runner.release:
		default:
			close(e.runner.release)
		}
		<-e.stopped
	})
	go func() {
		e.runErr = a.Run(ctx)
		close(e.stopped)
	}()
	waitFor(t, func() bool { _, ok := e.mqtt.Retained(a.Topics().State("lab1")); return ok })
	a.WaitIdle()
	return e
}

// startUpgrade sends an upgrade and waits until its package manager runs.
func (e *shutdownEnv) startUpgrade(t *testing.T) {
	t.Helper()
	_ = e.mqtt.Publish(e.agent.Topics().Command("lab1"), false, []byte(ha.CmdUpgradePackages))
	select {
	case <-e.runner.started:
	case <-time.After(5 * time.Second):
		t.Fatal("the package upgrade did not start")
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition not met in time")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestStoppingLetsARunningUpgradeFinish(t *testing.T) {
	e := newShutdownEnv(t)
	e.startUpgrade(t)
	if _, err := os.Stat(e.marker); err != nil {
		t.Fatalf("no operation marker while upgrading: %v", err)
	}

	// systemd stops the agent, for example from the postinstall script of
	// the agent's own package, while the package manager runs.
	e.cancel()
	select {
	case <-e.stopped:
		t.Fatal("Run returned while the upgrade was still running")
	case <-time.After(200 * time.Millisecond):
	}
	if e.runner.wasCancelled() {
		t.Fatal("stopping the agent cancelled the package manager")
	}

	close(e.runner.release)
	select {
	case <-e.stopped:
		if e.runErr != nil {
			t.Fatal(e.runErr)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after the upgrade finished")
	}
	e.agent.mu.Lock()
	run := e.agent.host.LastRun
	e.agent.mu.Unlock()
	if run == nil || !run.Success || !run.Changed {
		t.Fatalf("upgrade did not finish successfully: %+v", run)
	}
	if _, err := os.Stat(e.marker); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("operation marker left behind: %v", err)
	}
	if status, _ := e.mqtt.Retained(e.agent.Topics().Availability()); string(status) != "offline" {
		t.Errorf("availability after stopping: %q", status)
	}
	// No check after the upgrade once stopping: the next agent does it.
	if e.runner.ranAfter("-s dist-upgrade") {
		t.Error("checked for updates again while stopping")
	}
}

func TestStoppingStartsNoQueuedOperation(t *testing.T) {
	e := newShutdownEnv(t)
	e.startUpgrade(t)
	// Queued behind the running upgrade.
	_ = e.mqtt.Publish(e.agent.Topics().Command("lab1"), false, []byte(ha.CmdCheck))
	e.cancel()
	close(e.runner.release)
	select {
	case <-e.stopped:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return")
	}
	if e.runner.ranAfter("-s dist-upgrade") {
		t.Error("a queued check ran after stopping")
	}
}

func TestChecksStillStopWithTheAgent(t *testing.T) {
	for _, kind := range []string{ha.CmdCheck, "stats", "sync", "check_all"} {
		if changesSystem(kind) {
			t.Errorf("%s would delay stopping the agent", kind)
		}
	}
	for _, kind := range []string{ha.CmdUpgrade, ha.CmdUpgradePackages, ha.CmdUpgradeContainers,
		ha.CmdUpgradeAll, ha.CmdUpgradeContainer, ha.CmdRestartContainer, ha.CmdReboot, ha.CmdRestore} {
		if !changesSystem(kind) {
			t.Errorf("stopping the agent would interrupt %s", kind)
		}
	}
}
