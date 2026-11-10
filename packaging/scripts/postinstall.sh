#!/bin/sh
# Runs after install and upgrade, for both .deb (configure) and .rpm (1 or 2).
set -e

UNIT=linux-ha-agent.service

# agent_child prints the process through which the running agent started this
# script, typically the apt-get or dnf it runs to upgrade itself, and fails
# when the agent is not an ancestor.
agent_child() {
  main=$(systemctl show -p MainPID --value "$UNIT" 2>/dev/null) || return 1
  [ -n "$main" ] && [ "$main" != 0 ] || return 1
  pid=$$
  while [ "$pid" -gt 1 ]; do
    parent=
    while read -r key value _; do
      if [ "$key" = PPid: ]; then
        parent=$value
        break
      fi
    done <"/proc/$pid/status" || return 1
    [ -n "$parent" ] || return 1
    if [ "$parent" = "$main" ]; then
      echo "$pid"
      return 0
    fi
    pid=$parent
  done
  return 1
}

if [ -d /run/systemd/system ]; then
  systemctl daemon-reload || true
  # Upgrades restart a running agent; a fresh install is left stopped until
  # it is configured.
  if child=$(agent_child); then
    # The agent is upgrading itself. Restarting it now would stop it in the
    # middle of the package manager it runs and leave dpkg or rpm unfinished,
    # so the restart waits, in a unit of its own, until that package manager
    # has exited and the agent finished the operation (it keeps a marker in
    # its runtime directory while one runs; agents before 0.5.2 do not). A
    # restart that waited 2 hours goes ahead anyway.
    # shellcheck disable=SC2016 # expanded by the waiting shell
    systemd-run --no-block --collect --quiet \
      --unit="linux-ha-agent-restart-$$" \
      --description="Restart linux-ha-agent after it upgraded itself" \
      /bin/sh -c '
        end=$(($(date +%s) + 7200))
        while { kill -0 "$1" 2>/dev/null || [ -e /run/linux-ha-agent/operation ]; } &&
          [ "$(date +%s)" -lt "$end" ]; do
          sleep 2
        done
        exec systemctl try-restart linux-ha-agent.service
      ' sh "$child" ||
      echo "linux-ha-agent: restart it once the upgrade is done: systemctl restart $UNIT" >&2
  else
    systemctl try-restart "$UNIT" || true
  fi
fi

if ! systemctl is-enabled --quiet "$UNIT" 2>/dev/null; then
  cat <<'MSG'
linux-ha-agent is installed. To start it:
  1. edit /etc/linux-ha-agent/config.yaml (at least mqtt.url and mqtt.username)
  2. put the MQTT password in /etc/linux-ha-agent/mqtt-password (chmod 600)
  3. systemctl enable --now linux-ha-agent
MSG
fi
