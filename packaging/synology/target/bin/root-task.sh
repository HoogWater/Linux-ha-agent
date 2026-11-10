#!/bin/sh
# Runs linux-ha-agent as root on Synology DSM.
#
# DSM 7 runs third-party packages as their own unprivileged user, but the
# agent needs root for docker and synopkg. A Task Scheduler boot-up task
# (user root) therefore starts this script; it keeps running and starts or
# stops the agent when the package is started or stopped in Package Center.
#
# The task runs:
#   T=$(readlink -f /var/packages/LinuxHAAgent/target) && chown -R root:root "$T" && /bin/sh "$T/bin/root-task.sh"
# so that everything root runs belongs to root before it is run.
PKG=LinuxHAAgent
PKGDIR=/var/packages/$PKG
VAR=$PKGDIR/var
# Files written by root live in a directory the package user cannot change.
ROOTDIR=$VAR/root
RUN=/run/$PKG

if [ "$(id -u)" != 0 ]; then
	echo "root-task.sh must run as root (Task Scheduler: user root)" >&2
	exit 1
fi

# Run from a copy, so upgrades of the package can replace this file.
if [ "$0" != "$RUN/root-task.sh" ]; then
	install -d -m 0700 "$RUN" && install -m 0600 "$0" "$RUN/root-task.sh" || exit 1
	exec /bin/sh "$RUN/root-task.sh"
fi

# One instance only; a new copy of this script takes over the lock. The
# agent and the commands it runs never get the lock (9>&- below), so a
# command that outlives the agent cannot keep a new copy from starting.
exec 9>"$RUN/lock"
if ! flock -w 30 9; then
	echo "$(date '+%Y-%m-%d %H:%M:%S') root-task: another root-task.sh holds $RUN/lock; exiting" >>"$ROOTDIR/agent.log" 2>/dev/null
	exit 0
fi

log() { echo "$(date '+%Y-%m-%d %H:%M:%S') root-task: $*" >>"$ROOTDIR/agent.log"; }

target() { readlink -f "$PKGDIR/target"; }

# secure keeps the files root runs out of reach of the package user.
secure() {
	t=$(target) || return 1
	if [ -n "$(find "$t" ! -user root -print -quit)" ]; then
		chown -R root:root "$t"
	fi
	chmod -R go-w "$t"
}

# ensure_rootdir makes the root-owned directory inside the package's data
# directory. It never creates the data directory itself: DSM replaces the
# links in /var/packages/LinuxHAAgent during upgrades.
ensure_rootdir() {
	[ -d "$VAR/" ] || return 1
	if [ -L "$ROOTDIR" ] || [ ! -d "$ROOTDIR" ] || [ "$(stat -c %u "$ROOTDIR")" != 0 ]; then
		rm -rf "$ROOTDIR"
		mkdir -m 0755 "$ROOTDIR" || return 1
	fi
}

agent_pid() {
	pid=$(cat "$ROOTDIR/agent.pid" 2>/dev/null) || return 1
	case $(readlink "/proc/$pid/exe" 2>/dev/null) in
	*/bin/linux-ha-agent | */bin/linux-ha-agent" (deleted)") echo "$pid" ;;
	*) return 1 ;;
	esac
}

# config writes the configuration the agent gets: the package's config.yaml
# with the state file kept in the root-owned directory.
config() {
	grep -v '^state_file:' "$VAR/config.yaml" >"$RUN/config.yaml" || return 1
	echo "state_file: $ROOTDIR/state.json" >>"$RUN/config.yaml"
	chmod 0600 "$RUN/config.yaml"
}

start_agent() {
	secure || return 1
	config || return 1
	t=$(target)
	# An invalid configuration is only tried again once it changes.
	sum=$(md5sum <"$RUN/config.yaml")
	[ "$sum" = "$(cat "$ROOTDIR/invalid.sum" 2>/dev/null)" ] && return 1
	if ! "$t/bin/linux-ha-agent" -config "$RUN/config.yaml" -check-config >>"$ROOTDIR/agent.log" 2>&1 9>&-; then
		log "the configuration is invalid; the agent starts once it is fixed"
		echo "$sum" >"$ROOTDIR/invalid.sum"
		return 1
	fi
	rm -f "$ROOTDIR/invalid.sum"
	log "starting the agent"
	"$t/bin/linux-ha-agent" -config "$RUN/config.yaml" >>"$ROOTDIR/agent.log" 2>&1 9>&- &
	echo $! >"$ROOTDIR/agent.pid"
	md5sum <"$t/bin/linux-ha-agent" >"$ROOTDIR/agent.sum"
}

# stop_agent stops the agent like the systemd unit does (TimeoutStopSec=1h,
# KillMode=process): it finishes a running operation, such as an upgrade,
# first and is only killed after an hour; commands it started are left alone.
stop_agent() {
	pid=$(agent_pid) || return 0
	log "stopping the agent"
	# Package Center counts the package as running while the agent stops.
	touch "$ROOTDIR/stopping"
	kill -TERM "$pid" 2>/dev/null
	waited=0
	while [ -d "/proc/$pid" ] && [ "$waited" -lt 3600 ]; do
		touch "$ROOTDIR/alive"
		[ "$waited" = 30 ] && log "waiting for the agent to finish its running operation"
		sleep 1
		waited=$((waited + 1))
	done
	if [ -d "/proc/$pid" ]; then
		log "the agent did not stop within an hour; killing it"
		kill -KILL "$pid" 2>/dev/null
	fi
	rm -f "$ROOTDIR/agent.pid" "$ROOTDIR/stopping"
}

[ -d "$ROOTDIR" ] && log "running (pid $$)"
missing=0
while :; do
	# Uninstalled (not just being upgraded): stop and leave.
	if [ ! -e "$PKGDIR/INFO" ]; then
		missing=$((missing + 1))
		if [ "$missing" -ge 24 ]; then
			agent_pid >/dev/null && kill -TERM "$(agent_pid)"
			rm -rf "$RUN"
			exit 0
		fi
		sleep 5
		continue
	fi
	missing=0
	if ! ensure_rootdir; then
		sleep 5 # being upgraded
		continue
	fi
	rm -f "$ROOTDIR/stopping" # left over when the task was killed while stopping
	touch "$ROOTDIR/alive"

	# A new version of this script takes over; the agent keeps running.
	t=$(target)
	if [ -f "$t/bin/root-task.sh" ] && ! cmp -s "$t/bin/root-task.sh" "$RUN/root-task.sh"; then
		secure && install -m 0600 "$t/bin/root-task.sh" "$RUN/root-task.sh" && exec /bin/sh "$RUN/root-task.sh"
	fi

	if [ -e "$VAR/run-requested" ] && [ -e "$VAR/config.yaml" ]; then
		if agent_pid >/dev/null; then
			# Restart for new settings or a new agent binary.
			if [ -e "$VAR/restart-requested" ] || ! md5sum <"$t/bin/linux-ha-agent" | cmp -s - "$ROOTDIR/agent.sum"; then
				stop_agent
			fi
		fi
		rm -f "$VAR/restart-requested"
		agent_pid >/dev/null || start_agent
	else
		stop_agent
	fi

	# Keep the log small.
	if [ "$(stat -c %s "$ROOTDIR/agent.log" 2>/dev/null || echo 0)" -gt 5000000 ]; then
		cp "$ROOTDIR/agent.log" "$ROOTDIR/agent.log.1" && : >"$ROOTDIR/agent.log"
	fi
	sleep 5
done
