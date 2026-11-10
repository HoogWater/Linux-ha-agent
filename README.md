# linux-ha-agent

[![CI](https://github.com/lvlie/linux-ha-agent/actions/workflows/ci.yml/badge.svg)](https://github.com/lvlie/linux-ha-agent/actions/workflows/ci.yml)
[![End-to-end](https://github.com/lvlie/linux-ha-agent/actions/workflows/e2e.yml/badge.svg)](https://github.com/lvlie/linux-ha-agent/actions/workflows/e2e.yml)

A small agent that brings your Linux machines into Home Assistant over MQTT:
pending updates, reboot status, Docker and Podman containers, and controls to
upgrade, restart and roll back. On an [Incus](https://linuxcontainers.org/incus/)
or [Proxmox VE](https://www.proxmox.com/en/proxmox-virtual-environment) host it
also manages **every guest through the platform itself** (the Incus API, or
`pct`/`qm` on Proxmox), so containers and VMs need no agent, no SSH and no
credentials of their own.

Inspired by [LNXlink](https://github.com/bkbilly/lnxlink), written in Go as a
single static binary.

```text
Incus host:    linux-ha-agent ── Incus API (exec, snapshots, events) ──► containers, VMs
Proxmox node:  linux-ha-agent ── pct exec / QEMU guest agent, pvesh ──► LXC containers, VMs
                   │
                   └── MQTT (TLS, one user per host, ACL) ──► Mosquitto ──► Home Assistant ──► HA MCP / Claude
Other machines: linux-ha-agent (manages only itself)
```

## Quick start

1. **Install the package** on every Linux machine (on Incus and Proxmox VE
   hosts only on the host, not in the guests):

   ```sh
   curl -fsSLO https://github.com/lvlie/linux-ha-agent/releases/latest/download/install.sh
   sudo sh install.sh
   ```

   On Debian, Ubuntu, Proxmox VE, Fedora, RHEL, Rocky and Alma this adds the
   signed [package repository](https://lvlie.github.io/Linux-ha-agent/), so
   new versions arrive with the normal system updates (also through the
   agent's own *Install* action). See [docs/installation.md](docs/installation.md)
   for setting up the repository by hand, the packages on the
   [releases page](https://github.com/lvlie/linux-ha-agent/releases), Go and NixOS.

2. **Prepare Home Assistant**: install the *Mosquitto broker* add-on, accept
   the MQTT integration it offers, and create a user for the machine named
   after its host ID (its host name, lowercase, `-` and `.` become `_`).

3. **Configure the agent** in `/etc/linux-ha-agent/config.yaml`:

   ```yaml
   mqtt:
     url: tcp://homeassistant.local:1883
     username: pve_01      # the host ID
     password_file: /etc/linux-ha-agent/mqtt-password
   ```

4. **Start it**:

   ```sh
   echo 'the-password' | sudo tee /etc/linux-ha-agent/mqtt-password >/dev/null
   sudo chmod 600 /etc/linux-ha-agent/mqtt-password
   sudo systemctl enable --now linux-ha-agent
   ```

5. The machine, its guests and containers appear under **Settings → Devices
   & services → MQTT**, and pending updates under **Settings → Updates**.

Then lock down the broker with the per-host access list, add a dashboard and
automations: [docs/home-assistant.md](docs/home-assistant.md).

## What you get in Home Assistant

Every machine and every guest becomes a device; guests are linked to their
host. The full list is in [docs/entities.md](docs/entities.md).

- **Updates**: pending and security updates with an *Install* action (update
  entities, shown under Settings → Updates), an *Updates available* sensor,
  and totals over the host and all guests.
- **Reboots and kernels**: *Reboot required*, running and newest installed
  kernel, *Kernel update pending*, last boot.
- **System**: CPU, memory, disk, load and temperature, like LNXlink.
- **Guests** (Incus, Proxmox VE): running state, platform, and per guest all
  of the above; on the host the number of guests running, with updates and
  needing a reboot.
- **Containers** (Docker, Podman, on hosts and in guests): per container a
  running sensor, an update entity that pulls the newer image and recreates
  just that container, and a restart button; per machine the number of
  running containers and of newer images. Image versions show the image as
  written in the compose file with its tag and digest, e.g.
  `ghcr.io/linuxserver/sonarr:latest (3d5ef2653c7b)`, also for the OCI
  application containers below.
- **Incus OCI application containers** (`incus launch docker:nginx`, and
  incus-compose): each one is a device with an image update entity that
  rebuilds it from the newer image, or updates it through incus-compose.
- **Proxmox VE OCI application containers** (Proxmox VE 9.1+): each one is a
  device whose image (from an `image:` line in its notes) is checked against
  its registry; updates are reported, Proxmox has no way to update them in
  place. See [Proxmox VE notes](#proxmox-ve-notes).
- **Actions**: check, upgrade (packages, containers, single containers, or
  the host and all guests), reboot or restart, restore the pre-upgrade
  snapshot, and an automatic upgrade schedule with an on/off switch.
- **Commands**: your own shell commands as sensors or binary sensors, and
  optionally a box to run ad-hoc commands, like LNXlink, but always as the
  user `nobody` without privileges, so they can only read.
- **Cleanup**: guests and containers that are removed disappear from Home
  Assistant with their entities.

### Compared with LNXlink

| | LNXlink | linux-ha-agent |
| --- | --- | --- |
| Language, install | Python, pip | Go, one static binary; .deb, .rpm, `go install`, NixOS |
| System sensors (CPU, memory, disk, load, temperature, uptime) | yes | yes |
| Package updates | count | count, security, list, install, reboot and kernel status |
| Docker | containers, start/stop | Docker and Podman, image updates, update and restart per container |
| Incus / Proxmox guests | no | yes, through the platform API; nothing installed in guests |
| Snapshots before upgrades, restore | no | yes |
| Desktop features (media, notifications, screenshots, ...) | yes | no, it is meant for servers |
| Commands as sensors, ad-hoc commands from Home Assistant | yes, as the agent's user | yes, always as `nobody` without privileges (read-only) |
| Reboot permissions | – | separately for the host and guests, with exclusions (e.g. all VMs) |

## Supported systems

| | Packages | Notes |
| --- | --- | --- |
| Ubuntu, Debian and derivatives | apt | security updates detected from the `-security` pockets |
| Fedora, RHEL, Rocky, Alma, CentOS Stream | dnf (dnf4 and dnf5) | security updates from advisories |
| NixOS | `nixos-rebuild` | channels (`--upgrade`) or a flake (`--refresh`); garbage collection as cleanup |
| Others (e.g. Alpine) | – | listed with OS and containers; packages are skipped, not failed |

Virtualisation platforms (both can be used on one machine; detected automatically):

| | Guests | How commands run inside | Snapshots | Changes noticed by |
| --- | --- | --- | --- | --- |
| Incus | containers and VMs, all projects | Incus API exec (like `incus exec`) | Incus API, with native expiry | Incus event stream |
| Proxmox VE | LXC containers (also OCI application containers) and QEMU VMs on the local node (templates skipped) | `pct exec`; `qm guest exec` through the QEMU guest agent; nothing in OCI application containers | `pct`/`qm snapshot`; the agent removes its own expired snapshots | polling `pvesh` and the container configs in `/etc/pve` (every minute) |

Containers, on hosts and inside guests:

- **Docker** and **Podman** (a `docker` command that is really podman is detected).
  On the host, `containers.podman.path` picks one of several installed podman
  versions and `containers.podman.user` manages a user's rootless containers.
- Compose projects are found from the labels compose puts on containers, so
  they are found wherever the files live. Upgrading runs `compose pull` +
  `compose up -d --remove-orphans` and only reports a change when an image
  actually changed. `podman compose` and `podman-compose` both work.
- Podman containers labelled `io.containers.autoupdate` are updated with
  `podman auto-update`.
- **Incus OCI application containers** (Incus 6.3+, with skopeo and umoci on
  the host) are devices of their own. The agent asks the registry for the
  current image and computes its Incus fingerprint (the SHA-256 of the layer
  digests), so an update is found without pulling it. Upgrading rebuilds the
  instance from the newer image, like `incus rebuild`: configuration, devices
  and volumes stay, the root file system is replaced. Incus cannot rebuild an
  instance that has snapshots, so the agent takes none (there is no restore
  button) and reports instances with snapshots instead of updating them. Nothing runs
  inside these containers (many have no shell), so they get no package,
  system or command sensors, and they are checked and updated also while
  stopped (a stopped container stays stopped). A container whose registry
  has the installed image is not rebuilt; when the registry cannot be asked,
  only an upgrade of that container itself rebuilds it, not `upgrade_all`.
- Containers created by **incus-compose** are updated through incus-compose
  (`incus-compose up --detach --pull always --no-deps <service>` in the
  project directory), because it recreates instances whose image differs
  from the one it pulled. It does not record where a project lives, so the
  agent looks for compose files in `incus.compose_dirs` (default `/root`, and
  its subdirectories) and matches them by project name. incus-compose
  recreates instances, so no snapshot is taken and there is no restore button.
  A stopped service is not updated, since `incus-compose up` would start it.
  incus-compose launches each service from the digest it resolved, so Incus
  does not know the tag: the image is checked as written in the compose file
  (`postgres:16-alpine`), and the digest Incus has is the running one. An
  image with variables (`${TAG}`), or a project not found in
  `incus.compose_dirs`, can only be checked against that digest, which never
  shows an update. incus-compose itself needs Incus listening on the network
  (`core.https_address`) for its image cache, and an Incus OCI remote named
  after each registry it pulls from (for example `docker.io`). The agent
  passes its own `INCUS_*` environment variables on to incus-compose, so its
  settings can be set on the service, for example `INCUS_COMPOSE_NO_HEALTHD=true`
  in a systemd drop-in when a project has a `restart:` policy but Incus has no
  https listener for the healthd sidecar.
- New images are detected by comparing local image digests with the registry
  (HEAD requests, which do not count against Docker Hub limits). Containers
  pinned by digest are left alone; standalone containers are reported but not
  recreated.

## How upgrades work

For a guest, in order:

1. **Snapshot** through the platform (`ha-agent-<time>`, removed after a week).
   If the snapshot fails, nothing is upgraded.
2. **Packages**: refresh and upgrade non-interactively (keeping existing config
   files), then remove unused packages and caches.
3. **Containers**: pull and recreate compose projects, podman auto-update, prune
   dangling images.
4. **Restart** the guest if the upgrade requires it (configurable).

*Upgrade host and guests* (button or schedule) upgrades the host first, then
every running guest one by one, then reboots the host only if it is needed
**and** `upgrade.reboot_host` is enabled. Only one operation runs at a time;
stopped guests are skipped. Lifecycle events (Incus) or polling (Proxmox) keep
the guest list current and trigger a check when a guest starts.

### Proxmox VE notes

- Install the agent on **every node**; each manages the guests running on it.
  Guests that migrate move to the other node's agent automatically.
- The node itself is Debian: it is upgraded with `apt-get dist-upgrade`, as
  Proxmox requires. Without a subscription, switch to the
  `pve-no-subscription` repository, or the enterprise repository makes every
  check fail with 401.
- Debian does not create `/var/run/reboot-required`; a pending reboot is
  detected by comparing the newest installed kernel with the running one.
- **OCI application containers** (created from an OCI image, Proxmox VE 9.1+)
  are recognised by their entrypoint and get the entities of
  [application containers](docs/entities.md#oci-application-containers)
  instead of running commands inside. Proxmox records neither the image nor
  its digest, so add the image to the container's **Notes**:

  ```text
  image: ghcr.io/linuxserver/sonarr:latest
  ```

  The agent assumes that the image was the newest when it first checks the
  container, and reports when the registry has a newer one. To state the
  running version exactly, add its digest:
  `image: ghcr.io/linuxserver/sonarr:latest@sha256:...`; update that line when
  you recreate the container from a newer image. Updates are only reported:
  Proxmox cannot rebuild a container from a newer image, so there is no
  *Install* action and *Upgrade host and guests* leaves them alone.
- **VMs** need the QEMU guest agent: enable it in the VM options and install
  `qemu-guest-agent` inside. VMs without it (and non-Linux VMs) are listed
  with their state, but are not checked or upgraded.
- **Snapshots** need storage that supports them (ZFS, LVM-thin, Ceph, qcow2).
  On other storage the snapshot fails and the upgrade does not run; set
  `guests.snapshot_before_upgrade: false` for those guests' hosts if you
  accept upgrading without one.

## Configuration

See [`config.example.yaml`](config.example.yaml) and
[docs/installation.md](docs/installation.md#configuration).

## Security

The agent runs as root, so the design keeps what can reach it small:

- **No listening ports.** The agent only makes outgoing connections to the
  broker, the container registries and the local Incus socket.
- **No privileged remote commands.** Home Assistant can only send a fixed set
  of words (`check`, `upgrade`, `upgrade_packages`, `upgrade_containers`,
  `upgrade_all`, `reboot`, `restore_snapshot`, ...); anything else is rejected
  and logged. Nothing received over MQTT becomes part of a command that runs
  as root.
- **Commands run as `nobody`.** Configured command sensors and, when
  `commands.allow_any` is enabled, typed commands run through `setpriv` as
  `nobody`: no capabilities, no supplementary groups, `no_new_privs` (so
  `sudo` and other setuid programs cannot gain privileges), a time limit and
  an output limit. Each command runs in its own session; on timeout, and when
  it exits, everything it started is killed. util-linux and BusyBox (Alpine)
  both work; if `nobody`, `setpriv` or `setsid` is missing the command does
  not run at all.
- **Permissions** decide which actions Home Assistant may use; reboots (host
  and guests separately, with exclusions), restores and typed commands are
  off by default. Entities for disallowed actions are not even published.
- **One MQTT user per host, limited by ACL.** With
  [`packaging/mosquitto/acl.conf`](packaging/mosquitto/acl.conf) each agent
  logs in with its host ID and can only publish its own devices and read its
  own commands: a compromised machine cannot command or impersonate another.
  This exact ACL runs in the end-to-end tests.
- **TLS** (`ssl://`), a private CA (`ca_file`) and client certificates are
  supported. Passwords are read from a file, never from the configuration.
- **Guests hold nothing**: no agent, no credentials, no network access needed
  (Proxmox VMs only need the standard QEMU guest agent).
  Every guest upgrade starts with a snapshot you can restore from Home Assistant.
- **Releases** are built in GitHub Actions with checksums and signed build
  provenance (`gh attestation verify`). Actions are pinned to commits;
  Dependabot, govulncheck and zizmor keep dependencies and workflows in check.

Mosquitto setup in short (Home Assistant's Mosquitto add-on works too):

```text
# /etc/mosquitto/conf.d/agents.conf
listener 8883
certfile /etc/mosquitto/certs/server.pem
keyfile  /etc/mosquitto/certs/server.key
allow_anonymous false
password_file /etc/mosquitto/passwd
acl_file /etc/mosquitto/acl.conf
```

```sh
sudo mosquitto_passwd /etc/mosquitto/passwd myhost   # one per agent, named after its host ID
```

The host ID is the host name in lowercase with everything except `a-z`, `0-9`
and `_` replaced by `_` (shown in the logs at startup).

## MQTT topics

| Topic | |
| --- | --- |
| `homeassistant/device/<host>/<device>/config` | Device discovery (retained) |
| `linux-ha-agent/<host>/<device>/state` | Device state as JSON (retained) |
| `linux-ha-agent/<host>/<device>/cmd` | Commands from Home Assistant |
| `linux-ha-agent/<host>/auto_upgrade/set` | `ON` / `OFF` for the schedule |
| `linux-ha-agent/<host>/status` | `online` / `offline` (last will) |

Removed guests are removed from Home Assistant automatically, also when they
were deleted while the agent was down.

## Development

```sh
make test     # unit and integration tests (race detector)
make lint     # pre-commit: golangci-lint, yamllint, shellcheck, actionlint, zizmor, ...
make build    # static binary in dist/
make packages # .deb and .rpm in dist/ (GOARCH=amd64|arm64)
make nix      # nix flake check
```

Tests:

- **Unit tests** for every package, with recorded output of apt, dnf, nix,
  docker and podman.
- **Agent tests** with simulated machines (Ubuntu + docker, Fedora, Alpine,
  stopped and excluded guests) covering checks, upgrades, permissions,
  snapshots, restores, restarts, ordering, lifecycle and persistence, plus a
  simulated Proxmox VE node (LXC through `pct exec`, a VM through the guest
  agent, a VM without guest agent, templates).
- **MQTT integration tests** with an embedded broker: authentication, last
  will, reconnects, and the ACL (another agent cannot command or spoof).
- **Registry tests** against an in-memory OCI registry.
- **Packages**: the `.deb` is installed, reinstalled, removed and purged on
  Ubuntu 24.04 and Debian 13, the `.rpm` on Fedora 43 and Rocky Linux 10
  ([`install-test.sh`](tests/packages/install-test.sh)); `go install` of the
  pushed commit is tested too.
- **End-to-end** ([`e2e.yml`](.github/workflows/e2e.yml)): a GitHub runner
  becomes an Incus host (stable and 6.0 LTS) with Ubuntu + docker, Fedora +
  podman, Debian in another project, Alpine and a stopped guest, and a real
  Mosquitto with the recommended ACL. The test drives the agent over MQTT like
  Home Assistant: discovery, checks, upgrades, snapshot and restore, restarts,
  guest lifecycle and an intruder that must be refused. A NixOS guest is
  tested as well (experimental). Proxmox VE cannot run on GitHub's runners, so
  it is covered by the simulated node, whose `pvesh`, `pct` and `qm` output
  follows the documented formats; it has not yet run against a real node.

When `go.mod` changes, update `vendorHash` in `nix/package.nix` (set it to
`lib.fakeHash`, run `nix build`, copy the hash from the error).

## License

GPL-3.0-or-later
