# Synology DSM

linux-ha-agent runs on Synology NAS models with DSM 7 as a package that is
installed in Package Center and set up in its wizard.

What it reports and does on DSM:

- **Package Center updates** (`synopkg checkupdateall`) in the *System updates*
  entity. *Install* upgrades them (`synopkg upgradeall`) when upgrades are
  allowed.
- **DSM updates** are reported in the same entity, but never installed: they
  restart the NAS, so install them in DSM.
- **Docker containers** of Container Manager: running sensors, image updates,
  compose projects and restarts, as on any other host.
- CPU, memory, disk, load, uptime and temperature.

Not available on DSM: rebooting the NAS from Home Assistant, and the command
sensors of `commands:` (DSM has no `setpriv`).

## Install

1. Create an MQTT user for the NAS on your broker (for example in the
   Mosquitto app of Home Assistant).
2. Download `linux-ha-agent_<version>_dsm7_amd64.spk` (Intel and AMD models)
   or `…_arm64.spk` (64-bit ARM models) from the
   [releases](https://github.com/lvlie/linux-ha-agent/releases).
3. In **Package Center → Settings → General**, set **Trust level** to **Any
   publisher**: the package is not signed by Synology.
4. **Package Center → Manual Install**, choose the `.spk` and fill in the
   wizard: the broker URL (`tcp://host:1883`, or `ssl://host:8883` for TLS),
   the MQTT user and password, an optional device name and what Home
   Assistant may do.
5. Create the root task (below), run it once, then **Run** the package.

### The root task

DSM 7 runs third-party packages as their own unprivileged user, and their
install scripts may not create anything that runs as root. The agent needs
root for Docker (`/var/run/docker.sock` belongs to root) and for
`synopkg upgradeall`. A Task Scheduler task therefore runs it as root; the
package's **Run** and **Stop** still start and stop it.

In **Control Panel → Task Scheduler → Create → Triggered Task →
User-defined script**:

| Tab | Field | Value |
| --- | --- | --- |
| General | User | `root` |
| General | Event | Boot-up |
| Task Settings | User-defined script | see below |

```sh
T=$(readlink -f /var/packages/LinuxHAAgent/target) && chown -R root:root "$T" && /bin/sh "$T/bin/root-task.sh"
```

Save it, select it and click **Run** once. The task keeps running and follows
the package: it starts the agent when the package runs, stops it when it is
stopped (a running operation, such as an upgrade, is finished first), restarts
it after an upgrade and exits when the package is
uninstalled. It only needs to be created once.

The command first gives the package's program files to root: DSM installs them
owned by the package user, and root must not run files that another user can
change. The task copies its script to `/run/LinuxHAAgent` before it runs it,
so package upgrades can replace the files.

## Change the settings

Install the package again (**Manual Install** of the same or a newer `.spk`):
DSM runs the upgrade wizard, filled in with the current settings. Leave the
password empty to keep it. The agent restarts with the new settings.

The wizard writes `/var/packages/LinuxHAAgent/var/config.yaml` (see
[config.example.yaml](../config.example.yaml)); edits there are replaced by
the next wizard run.

## Logs and files

| | |
| --- | --- |
| Agent log | `/var/packages/LinuxHAAgent/var/root/agent.log`, also under **Package Center → Linux HA Agent → Log** |
| Settings | `/var/packages/LinuxHAAgent/var/settings`, `config.yaml`, `mqtt-password` (readable only by the package user and root) |
| State | `/var/packages/LinuxHAAgent/var/root/` (written by root only) |

If the package does not start, Package Center shows why: no settings yet, the
root task is not running, or an invalid configuration (details in the log).

## Uninstall

Uninstall the package in Package Center; the root task notices and exits
within two minutes. Then delete the task in Task Scheduler.

## Security notes

The agent runs as root, as it does on other systems. Its configuration is
written by the package user, which DSM creates for this package only and
which runs nothing but the package's scripts. That user can change what the
agent connects to and what Home Assistant may do, so do not give other
software its rights. The state file is always kept in a directory that only
root can write, whatever the configuration says.
