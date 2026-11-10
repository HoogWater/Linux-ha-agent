# Installation

Every [release](https://github.com/lvlie/linux-ha-agent/releases) has `.deb`
and `.rpm` packages and plain binaries for amd64 and arm64, with SHA-256
checksums and signed build provenance, and is published in a signed apt and
dnf [package repository](#package-repository). The binary is static: it runs
on any Linux distribution without dependencies.

## Package repository

A signed apt and dnf repository on GitHub Pages,
<https://lvlie.github.io/Linux-ha-agent/>, has the packages of the last five
releases. With it, new versions come with the normal system updates, and the
agent can update itself from Home Assistant. The [install script](#install-script)
sets it up; by hand:

```sh
# Debian, Ubuntu, Proxmox VE
sudo install -d -m 0755 /etc/apt/keyrings
sudo curl -fsSL -o /etc/apt/keyrings/linux-ha-agent.asc https://lvlie.github.io/Linux-ha-agent/linux-ha-agent.asc
sudo curl -fsSL -o /etc/apt/sources.list.d/linux-ha-agent.sources https://lvlie.github.io/Linux-ha-agent/linux-ha-agent.sources
sudo apt update && sudo apt install linux-ha-agent

# Fedora, RHEL, Rocky, Alma
sudo curl -fsSL -o /etc/yum.repos.d/linux-ha-agent.repo https://lvlie.github.io/Linux-ha-agent/linux-ha-agent.repo
sudo dnf install linux-ha-agent
```

The repository and the `.rpm` packages are signed with the key
`31E1 AABF FB9E EE9D 4230  430E 319E 121B 9087 AD84`
([linux-ha-agent.asc](../packaging/repo/linux-ha-agent.asc)). The `.deb`
packages are verified through the signed repository index.

## Packages from a release

Without the repository, install a package from the
[releases page](https://github.com/lvlie/linux-ha-agent/releases) directly:

```sh
# Debian, Ubuntu, Proxmox VE
curl -fsSLO https://github.com/lvlie/linux-ha-agent/releases/download/v0.4.4/linux-ha-agent_0.4.4-1_amd64.deb
sudo apt install ./linux-ha-agent_0.4.4-1_amd64.deb
# Fedora, RHEL, Rocky, Alma
sudo dnf install https://github.com/lvlie/linux-ha-agent/releases/download/v0.4.4/linux-ha-agent-0.4.4-1.x86_64.rpm
```

The packages install:

| | |
| --- | --- |
| `/usr/bin/linux-ha-agent` | the agent |
| `/etc/linux-ha-agent/config.yaml` | configuration (kept on upgrades and removal) |
| `/usr/lib/systemd/system/linux-ha-agent.service` | systemd unit, not started until you enable it |
| `/usr/share/doc/linux-ha-agent/` | README, example configuration, Mosquitto ACL |

Upgrading the package restarts a running agent. When the agent upgraded
itself, from Home Assistant or on its schedule, the restart waits until the
package manager and the rest of that upgrade (guests included) are done.
Stopping the agent never interrupts a running upgrade, restart or restore:
it finishes that machine first (up to an hour, `TimeoutStopSec`), then exits.
Removing the package stops the service; `apt purge` also removes the
configuration.

Before 0.5.2, an agent that upgraded its own package was restarted in the
middle of it and left dpkg unfinished: later upgrades then fail with
`dpkg was interrupted, you must manually run 'dpkg --configure -a'`. Run
`sudo dpkg --configure -a` once. The 0.5.2 package keeps such an agent's own
package upgrade from being interrupted, but an older agent does not tell it
when the rest of an operation is done: upgrade it to 0.5.2 with the host's
*Upgrade* button (or `apt`), not *Upgrade host and guests*.

## Install script

```sh
curl -fsSLO https://github.com/lvlie/linux-ha-agent/releases/latest/download/install.sh
less install.sh        # read it first
sudo sh install.sh     # or: sudo sh install.sh v0.4.4
```

With apt or dnf it adds the [package repository](#package-repository) and
installs the latest or the given version from it (a given version must be one
of the last five). Running it again on a machine with a package installed
from a release switches that machine to the repository.

Elsewhere it installs the plain binary and the systemd unit from a release.
It always checks the binary's SHA-256 checksum, and the signed build
provenance as well when the GitHub CLI 2.49 or newer is installed and logged
in (`gh auth login`, or `GH_TOKEN` set); otherwise the script says why it
skipped that.

## With Go

With Go installed (1.26.7 or newer; older Go versions download it
automatically):

```sh
go install github.com/lvlie/linux-ha-agent/cmd/linux-ha-agent@latest
sudo install -m 0755 "$(go env GOPATH)/bin/linux-ha-agent" /usr/local/bin/
```

Then add the [systemd unit](../packaging/systemd/linux-ha-agent.service) (change
`/usr/bin` to `/usr/local/bin`) and a configuration.

## After installing

```sh
sudoedit /etc/linux-ha-agent/config.yaml
echo 'the-mqtt-password' | sudo tee /etc/linux-ha-agent/mqtt-password >/dev/null
sudo chmod 600 /etc/linux-ha-agent/mqtt-password
sudo systemctl enable --now linux-ha-agent
journalctl -u linux-ha-agent -f
```

Try it without MQTT: `sudo linux-ha-agent -config /etc/linux-ha-agent/config.yaml -once`
checks everything once and prints the state as JSON.

## NixOS

```nix
{
  inputs.linux-ha-agent.url = "github:lvlie/linux-ha-agent";

  outputs = { nixpkgs, linux-ha-agent, ... }: {
    nixosConfigurations.myhost = nixpkgs.lib.nixosSystem {
      modules = [
        linux-ha-agent.nixosModules.default
        {
          services.linux-ha-agent = {
            enable = true;
            settings = {
              mqtt = {
                url = "ssl://mqtt.lan:8883";
                username = "myhost";
                password_file = "/run/secrets/linux-ha-agent-mqtt"; # sops-nix, agenix, ...
              };
              upgrade.schedule = "30 3 * * 0";
              nixos.flake = "github:me/nix-config"; # or leave empty for channels
            };
          };
        }
      ];
    };
  };
}
```

The configuration is validated when the system is built.

## Configuration

See [`config.example.yaml`](../config.example.yaml); only `mqtt.url` is required.
Unknown keys are rejected so typos do not go unnoticed. Validate with
`linux-ha-agent -check-config`.

The most important settings:

| Setting | Default | |
| --- | --- | --- |
| `upgrade.schedule` | off | Cron schedule for *Upgrade host and guests*, e.g. `30 3 * * 0`; prefix `CRON_TZ=Europe/Amsterdam` for a time zone |
| `upgrade.reboot_host` | `false` | Reboot the host when needed after an upgrade |
| `upgrade.restart_guests` | `true` | Restart guests when needed after an upgrade |
| `guests.snapshot_before_upgrade` | `true` | Snapshot guests before upgrading |
| `guests.exclude` | none | Guests to leave alone: `name`, `project/name` (Incus), VMID (Proxmox), wildcards |
| `incus.enabled` / `proxmox.enabled` | `auto` | Manage Incus or Proxmox VE guests when found |
| `permissions.upgrade` / `restore` / `restart_containers` | `true` / `false` / `false` | What Home Assistant may trigger |
| `permissions.reboot_host` / `reboot_guests` | `false` / `false` | Reboot buttons for the host and for guests |
| `permissions.reboot_exclude` | none | Guests never restarted (also not after upgrades): names, `project/name`, VMIDs, wildcards, `type:vm`, `type:lxc` |
| `commands.expose` / `commands.allow_any` | none / `false` | Command sensors and ad-hoc commands, run as `nobody` |
| `checks.interval` | `6h` | How often to look for updates |

Next: [connect it to Home Assistant](home-assistant.md).
