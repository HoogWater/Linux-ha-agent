#!/bin/sh
# The agent upgrades its own package: it installs OLD_DEB, starts the agent,
# offers NEW_DEB in a local apt repository and asks the agent over MQTT to
# upgrade the host's packages. The upgrade must finish cleanly (dpkg not left
# interrupted) and the new agent must be running afterwards.
#
# Run as root on a disposable Debian or Ubuntu machine booted with systemd,
# with mosquitto (listening on 127.0.0.1 without authentication),
# mosquitto-clients and dpkg-dev installed.
set -eu
: "${OLD_DEB:?}" "${NEW_DEB:?}"
HOST=selfup
TOPIC="linux-ha-agent/$HOST"
export DEBIAN_FRONTEND=noninteractive

new_version=$(dpkg-deb -f "$NEW_DEB" Version)

echo "== install $(dpkg-deb -f "$OLD_DEB" Version)"
apt-get install -y -q "$OLD_DEB" >/dev/null
cat >/etc/linux-ha-agent/config.yaml <<EOF
mqtt:
  url: tcp://127.0.0.1:1883
host:
  name: $HOST
checks:
  interval: 1h
permissions:
  upgrade: true
log_level: debug
EOF

echo "== offer $new_version, and nothing else, in a local repository"
mkdir -p /srv/repo
cp "$NEW_DEB" /srv/repo/
(cd /srv/repo && dpkg-scanpackages --multiversion . >Packages)
mkdir -p /etc/apt/disabled
mv /etc/apt/sources.list.d/* /etc/apt/disabled/ 2>/dev/null || true
: >/etc/apt/sources.list
echo "deb [trusted=yes] file:/srv/repo ./" >/etc/apt/sources.list.d/selfup.list
apt-get update -q >/dev/null

echo "== start the agent"
systemctl enable --now linux-ha-agent.service
timeout 60 mosquitto_sub -t "$TOPIC/status" -C 1 | grep -qx online
# Let the startup check finish; it runs apt-get update too.
for _ in $(seq 60); do pgrep -x apt-get >/dev/null || break; sleep 1; done

echo "== the agent upgrades itself"
old_pid=$(systemctl show -p MainPID --value linux-ha-agent.service)
mosquitto_pub -t "$TOPIC/$HOST/cmd" -m upgrade_packages

installed() {
  dpkg-query -W -f '${Status} ${Version}' linux-ha-agent 2>/dev/null
}
ok=
for _ in $(seq 180); do
  sleep 2
  pid=$(systemctl show -p MainPID --value linux-ha-agent.service)
  if [ "$(installed)" = "install ok installed $new_version" ] &&
    [ "$pid" != 0 ] && [ "$pid" != "$old_pid" ] &&
    ! pgrep -x apt-get >/dev/null && ! pgrep -x dpkg >/dev/null; then
    ok=1
    break
  fi
done

echo "-- package: $(installed)"
echo "-- dpkg --audit:"
dpkg --audit || true
echo "-- agent log:"
journalctl -u linux-ha-agent.service --no-pager | tail -40
if [ -z "$ok" ]; then
  echo "self-upgrade did not finish: dpkg state, a running package manager or the agent's restart is wrong" >&2
  exit 1
fi
test -z "$(dpkg --audit)"
systemctl is-active --quiet linux-ha-agent.service
# The running agent is the new binary, not the deleted old one.
exe=$(readlink "/proc/$(systemctl show -p MainPID --value linux-ha-agent.service)/exe")
test "$exe" = /usr/bin/linux-ha-agent
timeout 60 mosquitto_sub -t "$TOPIC/status" -C 1 | grep -qx online
echo "self-upgrade test passed"
