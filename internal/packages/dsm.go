package packages

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/lvlie/linux-ha-agent/internal/runner"
)

// DSM manages Synology DSM: the packages of Package Center are checked and
// upgraded with synopkg. A new DSM version is reported, but never installed:
// DSM upgrades restart the NAS and are left to DSM itself.
type DSM struct {
	r runner.Runner
}

// Name implements Manager.
func (*DSM) Name() string { return "dsm" }

// The Synology tools are not in PATH.
const (
	synopkg    = "/usr/syno/bin/synopkg"
	synowebapi = "/usr/syno/bin/synowebapi"
)

// dsmUpdateName is the name of the pending DSM update in Status.Updates.
const dsmUpdateName = "DSM"

// Check implements Manager.
func (d *DSM) Check(ctx context.Context) (Status, error) {
	updates, err := d.packageUpdates(ctx)
	if err != nil {
		return Status{}, err
	}
	system, err := d.systemUpdate(ctx)
	if err != nil {
		return Status{}, err
	}
	if system != nil {
		updates = append([]Update{*system}, updates...)
	}
	return Status{Manager: d.Name(), Updates: updates, Security: -1}, nil
}

func (d *DSM) packageUpdates(ctx context.Context) ([]Update, error) {
	res, err := runner.Check(ctx, d.r, runner.Command(synopkg, "checkupdateall"))
	if err != nil {
		return nil, err
	}
	return ParseSynopkgUpdates(res.Stdout)
}

// ParseSynopkgUpdates parses the JSON of `synopkg checkupdateall`.
func ParseSynopkgUpdates(out string) ([]Update, error) {
	out = strings.TrimSpace(out)
	if out == "" {
		return []Update{}, nil
	}
	var pkgs []struct {
		ID      string `json:"id"`
		Name    string `json:"name"`
		Version string `json:"version"`
	}
	if err := json.Unmarshal([]byte(out), &pkgs); err != nil {
		return nil, fmt.Errorf("parsing synopkg checkupdateall output: %w", err)
	}
	updates := make([]Update, 0, len(pkgs))
	for _, p := range pkgs {
		name := p.Name
		if name == "" {
			name = p.ID
		}
		updates = append(updates, Update{Name: name, Version: p.Version, Source: "Package Center"})
	}
	return updates, nil
}

// systemUpdate asks DSM's own API (as its update page does) for a new DSM.
func (d *DSM) systemUpdate(ctx context.Context) (*Update, error) {
	res, err := runner.Check(ctx, d.r, runner.Command(synowebapi, "--exec", "api=SYNO.Core.Upgrade.Server", "method=check", "version=1"))
	if err != nil {
		return nil, err
	}
	return ParseDSMUpdateCheck(res.Stdout)
}

// ParseDSMUpdateCheck parses SYNO.Core.Upgrade.Server check (version 1).
func ParseDSMUpdateCheck(out string) (*Update, error) {
	var resp struct {
		Success bool           `json:"success"`
		Data    map[string]any `json:"data"`
		Error   any            `json:"error"`
	}
	if err := json.Unmarshal([]byte(out), &resp); err != nil {
		return nil, fmt.Errorf("parsing the DSM update check: %w", err)
	}
	if !resp.Success {
		return nil, fmt.Errorf("DSM update check failed: %v", resp.Error)
	}
	if available, _ := resp.Data["available"].(bool); !available {
		return nil, nil
	}
	u := &Update{Name: dsmUpdateName, Source: "DSM update (install it in DSM)"}
	for _, key := range []string{"version", "title"} {
		if v, ok := resp.Data[key].(string); ok && v != "" {
			u.Version = v
			break
		}
	}
	return u, nil
}

// Upgrade implements Manager: it upgrades the packages of Package Center.
// A pending DSM update is left alone.
func (d *DSM) Upgrade(ctx context.Context) (Result, error) {
	before, err := d.packageUpdates(ctx)
	if err != nil {
		return Result{}, err
	}
	if len(before) > 0 {
		if _, err := runner.Check(ctx, d.r, runner.Command(synopkg, "upgradeall")); err != nil {
			return Result{}, err
		}
	}
	after, err := d.packageUpdates(ctx)
	if err != nil {
		return Result{}, err
	}
	res := Result{Changed: len(after) < len(before)}
	switch {
	case len(before) == 0:
		res.Summary = "no package updates"
	case len(after) == 0:
		res.Summary = fmt.Sprintf("upgraded %d packages", len(before))
	default:
		names := make([]string, 0, len(after))
		for _, u := range after {
			names = append(names, u.Name)
		}
		res.Summary = fmt.Sprintf("upgraded %d of %d packages; still pending: %s", len(before)-len(after), len(before), strings.Join(names, ", "))
	}
	if system, err := d.systemUpdate(ctx); err == nil && system != nil {
		res.Summary += "; a DSM update is available: install it in DSM"
	}
	return res, nil
}

// Cleanup implements Manager; DSM cleans up after itself.
func (*DSM) Cleanup(context.Context) error { return nil }

// RebootRequired implements Manager. Package upgrades never need a reboot and
// DSM upgrades are not done by the agent.
func (*DSM) RebootRequired(context.Context) (bool, error) { return false, nil }
