// Package osinfo detects the distribution of a target from /etc/os-release,
// or from /etc.defaults/VERSION on Synology DSM, which has no os-release.
package osinfo

import (
	"context"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/lvlie/linux-ha-agent/internal/runner"
)

// Family groups distributions that are managed the same way.
type Family string

// Supported families.
const (
	FamilyDebian  Family = "debian"
	FamilyRHEL    Family = "rhel"
	FamilyNixOS   Family = "nixos"
	FamilyDSM     Family = "dsm"
	FamilyUnknown Family = "unknown"
)

// Info describes the operating system of a target.
type Info struct {
	ID         string
	IDLike     []string
	Name       string
	Version    string
	PrettyName string
	Family     Family
}

// dsmVersionFile describes the installed DSM version on Synology systems.
const dsmVersionFile = "/etc.defaults/VERSION"

// Detect reads /etc/os-release on the target, or the DSM version file on
// Synology systems.
func Detect(ctx context.Context, r runner.Runner) (Info, error) {
	res, err := runner.Check(ctx, r, runner.Command("cat", "/etc/os-release"))
	if err != nil {
		if dsm, dsmErr := runner.Check(ctx, r, runner.Command("cat", dsmVersionFile)); dsmErr == nil {
			return ParseDSM(dsm.Stdout), nil
		}
		return Info{Family: FamilyUnknown}, fmt.Errorf("reading /etc/os-release: %w", err)
	}
	return Parse(res.Stdout), nil
}

// ParseDSM parses Synology's /etc.defaults/VERSION.
func ParseDSM(content string) Info {
	values := parseValues(content)
	version := values["productversion"]
	if version == "" {
		version = strings.Trim(values["majorversion"]+"."+values["minorversion"], ".")
	}
	pretty := strings.TrimSpace("DSM " + version)
	if build := values["buildnumber"]; build != "" {
		pretty += "-" + build
	}
	if fix := values["smallfixnumber"]; fix != "" && fix != "0" {
		pretty += " Update " + fix
	}
	return Info{ID: "dsm", Name: "DSM", Version: version, PrettyName: pretty, Family: FamilyDSM}
}

// parseValues reads KEY=value lines with optionally quoted values.
func parseValues(content string) map[string]string {
	values := map[string]string{}
	for _, line := range strings.Split(content, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		if unquoted, err := strconv.Unquote(value); err == nil {
			value = unquoted
		} else {
			value = strings.Trim(value, `"'`)
		}
		values[key] = value
	}
	return values
}

// Parse parses the contents of an os-release file.
func Parse(content string) Info {
	values := parseValues(content)
	info := Info{
		ID:         strings.ToLower(values["ID"]),
		IDLike:     strings.Fields(strings.ToLower(values["ID_LIKE"])),
		Name:       values["NAME"],
		Version:    values["VERSION_ID"],
		PrettyName: values["PRETTY_NAME"],
	}
	if info.PrettyName == "" {
		info.PrettyName = strings.TrimSpace(info.Name + " " + info.Version)
	}
	info.Family = family(info)
	return info
}

func family(info Info) Family {
	ids := append([]string{info.ID}, info.IDLike...)
	switch {
	case slices.Contains(ids, "nixos"):
		return FamilyNixOS
	case slices.Contains(ids, "debian") || slices.Contains(ids, "ubuntu"):
		return FamilyDebian
	case slices.ContainsFunc(ids, func(id string) bool {
		return id == "fedora" || id == "rhel" || id == "centos"
	}):
		return FamilyRHEL
	default:
		return FamilyUnknown
	}
}
