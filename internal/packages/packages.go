// Package packages checks for and installs operating system updates with the
// native package manager of a target: apt, dnf, NixOS or Synology DSM.
package packages

import (
	"context"
	"fmt"
	"strings"

	"github.com/lvlie/linux-ha-agent/internal/osinfo"
	"github.com/lvlie/linux-ha-agent/internal/runner"
)

// Update is one pending package update.
type Update struct {
	Name     string `json:"name"`
	Version  string `json:"version,omitempty"`
	Source   string `json:"source,omitempty"`
	Security bool   `json:"security,omitempty"`
}

// Status is the result of a check.
type Status struct {
	Manager string   `json:"manager"`
	Updates []Update `json:"updates"`
	// Security is the number of security updates, or -1 when unknown.
	Security       int  `json:"security"`
	RebootRequired bool `json:"reboot_required"`
}

// Result of an upgrade.
type Result struct {
	Changed bool   `json:"changed"`
	Summary string `json:"summary"`
}

// Manager is a package manager on one target.
type Manager interface {
	Name() string
	// Check refreshes package metadata and reports pending updates.
	Check(ctx context.Context) (Status, error)
	// Upgrade installs all pending updates.
	Upgrade(ctx context.Context) (Result, error)
	// Cleanup removes unused packages and cached downloads.
	Cleanup(ctx context.Context) error
	// RebootRequired reports whether the target needs a reboot.
	RebootRequired(ctx context.Context) (bool, error)
}

// Options tune the managers.
type Options struct {
	// NixFlake rebuilds NixOS from a flake instead of channels.
	NixFlake string
	// NixGCOlderThan is passed to nix-collect-garbage --delete-older-than.
	NixGCOlderThan string
}

// ErrUnsupported is returned for distributions without a manager.
type ErrUnsupported struct{ OS string }

func (e ErrUnsupported) Error() string {
	return fmt.Sprintf("no supported package manager for %s", e.OS)
}

// For picks the manager for the detected OS.
func For(info osinfo.Info, r runner.Runner, opts Options) (Manager, error) {
	switch info.Family {
	case osinfo.FamilyDebian:
		return &Apt{r: r}, nil
	case osinfo.FamilyRHEL:
		return &Dnf{r: r}, nil
	case osinfo.FamilyNixOS:
		return &NixOS{r: r, flake: opts.NixFlake, gcOlderThan: opts.NixGCOlderThan}, nil
	case osinfo.FamilyDSM:
		return &DSM{r: r}, nil
	default:
		name := info.PrettyName
		if name == "" {
			name = info.ID
		}
		return nil, ErrUnsupported{OS: name}
	}
}

// Summary renders a short human readable description of pending updates.
func Summary(s Status, limit int) string {
	if len(s.Updates) == 0 {
		return "Up to date"
	}
	names := make([]string, 0, limit)
	for i, u := range s.Updates {
		if i == limit {
			names = append(names, fmt.Sprintf("and %d more", len(s.Updates)-limit))
			break
		}
		names = append(names, u.Name)
	}
	return strings.Join(names, ", ")
}
