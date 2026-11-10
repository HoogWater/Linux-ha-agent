// Package runner runs commands on a target machine: the local host or an
// Incus guest. Everything that inspects or upgrades a machine goes through a
// Runner, so the same code works on hosts and inside guests.
package runner

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
)

// DefaultPath is used for every command, so tools are found on all supported
// distributions, including NixOS.
const DefaultPath = "/run/wrappers/bin:/run/current-system/sw/bin:/nix/var/nix/profiles/default/bin:" +
	"/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"

// Cmd is a command with its arguments. Commands are never passed through a
// shell unless the caller explicitly runs one.
type Cmd struct {
	Args []string
	Env  map[string]string
}

// Command builds a Cmd from arguments.
func Command(args ...string) Cmd {
	return Cmd{Args: args}
}

// WithEnv returns a copy of the command with extra environment variables.
func (c Cmd) WithEnv(kv ...string) Cmd {
	env := make(map[string]string, len(c.Env)+len(kv)/2)
	for k, v := range c.Env {
		env[k] = v
	}
	for i := 0; i+1 < len(kv); i += 2 {
		env[kv[i]] = kv[i+1]
	}
	c.Env = env
	return c
}

// String renders the command for logs.
func (c Cmd) String() string {
	return strings.Join(c.Args, " ")
}

// Result of a finished command.
type Result struct {
	Stdout   string
	Stderr   string
	ExitCode int
}

// ExitError is returned by Check when a command exits non-zero.
type ExitError struct {
	Cmd    Cmd
	Result Result
}

func (e *ExitError) Error() string {
	msg := strings.TrimSpace(e.Result.Stderr)
	if msg == "" {
		msg = strings.TrimSpace(e.Result.Stdout)
	}
	if len(msg) > 500 {
		msg = "..." + msg[len(msg)-500:]
	}
	cmd := e.Cmd.String()
	if i := strings.IndexByte(cmd, '\n'); i >= 0 {
		cmd = cmd[:i] + " ..." // scripts: the first line is enough
	}
	if len(cmd) > 120 {
		cmd = cmd[:117] + "..."
	}
	return fmt.Sprintf("%q exited with %d: %s", cmd, e.Result.ExitCode, msg)
}

// Runner runs commands on one machine.
type Runner interface {
	// Run runs the command. The error is only set when the command could not
	// be run at all; a non-zero exit code is reported in Result.
	Run(ctx context.Context, cmd Cmd) (Result, error)
}

// Check runs the command and turns a non-zero exit code into an *ExitError.
func Check(ctx context.Context, r Runner, cmd Cmd) (Result, error) {
	res, err := r.Run(ctx, cmd)
	if err != nil {
		return res, err
	}
	if res.ExitCode != 0 {
		return res, &ExitError{Cmd: cmd, Result: res}
	}
	return res, nil
}

// Has reports whether an executable is available on the target.
func Has(ctx context.Context, r Runner, name string) bool {
	res, err := r.Run(ctx, Command("sh", "-c", `command -v "$1" >/dev/null 2>&1`, "sh", name))
	return err == nil && res.ExitCode == 0
}

// Environment merges the defaults with the command's environment.
func Environment(cmd Cmd) map[string]string {
	env := map[string]string{"PATH": DefaultPath, "LC_ALL": "C", "HOME": "/root"}
	for k, v := range cmd.Env {
		env[k] = v
	}
	return env
}

// lookPath finds an executable in a PATH value.
func lookPath(name, path string) (string, bool) {
	for _, dir := range filepath.SplitList(path) {
		if dir == "" {
			continue // the current directory: never wanted for root
		}
		p := filepath.Join(dir, name)
		if fi, err := os.Stat(p); err == nil && fi.Mode().IsRegular() && fi.Mode()&0o111 != 0 {
			return p, true
		}
	}
	return "", false
}

// Local runs commands on the machine the agent runs on.
type Local struct{}

// Run implements Runner.
func (Local) Run(ctx context.Context, cmd Cmd) (Result, error) {
	if len(cmd.Args) == 0 {
		return Result{}, errors.New("empty command")
	}
	env := Environment(cmd)
	// Find the program in the PATH it gets, not in the agent's own: that
	// may lack directories such as /usr/local/bin (sudo's secure_path,
	// Synology DSM, where Container Manager installs docker there).
	name := cmd.Args[0]
	if !strings.Contains(name, "/") {
		if path, ok := lookPath(name, env["PATH"]); ok {
			name = path
		}
	}
	c := exec.CommandContext(ctx, name, cmd.Args[1:]...)
	keys := make([]string, 0, len(env))
	for k := range env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		c.Env = append(c.Env, k+"="+env[k])
	}
	var stdout, stderr bytes.Buffer
	c.Stdout = &stdout
	c.Stderr = &stderr
	err := c.Run()
	res := Result{Stdout: stdout.String(), Stderr: stderr.String()}
	var exitErr *exec.ExitError
	switch {
	case err == nil:
	case errors.As(err, &exitErr):
		res.ExitCode = exitErr.ExitCode()
		err = nil
	case errors.Is(err, exec.ErrNotFound):
		res.ExitCode = 127
		res.Stderr = err.Error()
		err = nil
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return res, ctxErr
	}
	return res, err
}
