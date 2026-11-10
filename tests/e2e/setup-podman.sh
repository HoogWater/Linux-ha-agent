#!/usr/bin/env bash
# Prepare the host for the podman end-to-end test (e2e/podman_test.go). Run as
# root on a disposable Ubuntu host with podman, podman-compose and systemd.
#
# The scenario is a host with two podman installations and rootless
# containers, which the agent only finds through containers.podman.path and
# containers.podman.user (tests/e2e/agent-podman.yaml):
#   - E2E_PODMAN_USER runs a compose project and a Quadlet with
#     AutoUpdate=registry, both on images older than their registry tag, and
#     has a dangling image for the cleanup;
#   - root runs a container the agent must not see;
#   - /usr/local/bin/podman, first in PATH, is a broken "other" podman.
set -euo pipefail

# Google's cache of Docker Hub images, more forgiving than pulling directly.
REGISTRY="mirror.gcr.io/library"
PODMAN="${E2E_PODMAN_PATH:-/usr/bin/podman}"
USER_NAME="${E2E_PODMAN_USER:-e2epod}"

# GitHub's runner sets XDG_CONFIG_HOME for its own user in /etc/environment,
# which PAM hands to every session, including the test user's systemd
# manager: its Quadlet generator and podman would look in the runner's home.
# Hosts normally have no such entries.
sed -i -E '/^(export )?XDG_(CONFIG|DATA|CACHE|STATE)_HOME=/d' /etc/environment

useradd --create-home --shell /bin/bash "$USER_NAME"
# Rootless podman needs subordinate IDs; useradd adds them on most systems.
grep -q "^${USER_NAME}:" /etc/subuid || usermod --add-subuids 200000-265535 "$USER_NAME"
grep -q "^${USER_NAME}:" /etc/subgid || usermod --add-subgids 200000-265535 "$USER_NAME"
loginctl enable-linger "$USER_NAME"
uid=$(id -u "$USER_NAME")
echo "waiting for the user manager of $USER_NAME"
for _ in $(seq 60); do
  [ -S "/run/user/${uid}/bus" ] && break
  sleep 1
done
[ -S "/run/user/${uid}/bus" ]

# as_user runs a command as the test user, from its home directory. runuser
# resets HOME but keeps the rest of the environment, and podman prefers the
# XDG base directories over HOME: GitHub's runner sets XDG_CONFIG_HOME to the
# runner user's home in /etc/environment, which sudo loads.
as_user() {
  (cd "/home/${USER_NAME}" && runuser -u "$USER_NAME" -- env \
    -u XDG_CONFIG_HOME -u XDG_DATA_HOME -u XDG_CACHE_HOME -u XDG_STATE_HOME \
    XDG_RUNTIME_DIR="/run/user/${uid}" \
    DBUS_SESSION_BUS_ADDRESS="unix:path=/run/user/${uid}/bus" "$@")
}

# retry <command...>: registries hiccup now and then.
retry() {
  for i in 1 2 3 4 5; do
    "$@" && return 0
    echo "$* failed (attempt $i), retrying"
    sleep $((i * 5))
  done
  return 1
}

# Images whose "latest" tag is really an older release, so the agent sees a
# newer image in the registry, plus a dangling image for the cleanup.
retry as_user "$PODMAN" pull -q "${REGISTRY}/busybox:1.36"
retry as_user "$PODMAN" pull -q "${REGISTRY}/alpine:3.20"
retry as_user "$PODMAN" pull -q "${REGISTRY}/alpine:3.19"
as_user "$PODMAN" tag "${REGISTRY}/busybox:1.36" "${REGISTRY}/busybox:latest"
as_user "$PODMAN" tag "${REGISTRY}/alpine:3.20" "${REGISTRY}/alpine:latest"
as_user "$PODMAN" tag "${REGISTRY}/alpine:3.19" dangling:latest
as_user "$PODMAN" rmi "${REGISTRY}/alpine:3.19"
as_user "$PODMAN" tag "${REGISTRY}/alpine:3.20" dangling:latest

# A compose project.
as_user mkdir -p "/home/${USER_NAME}/app"
as_user tee "/home/${USER_NAME}/app/compose.yaml" >/dev/null <<YAML
services:
  sleeper:
    image: ${REGISTRY}/busybox:latest
    command: [sleep, infinity]
    restart: unless-stopped
YAML
# shellcheck disable=SC2016 # expanded by the inner shell
as_user sh -c 'cd app && exec podman-compose --podman-path "$1" up -d' sh "$PODMAN"

# A Quadlet that podman auto-update may update and restart.
as_user mkdir -p "/home/${USER_NAME}/.config/containers/systemd"
as_user tee "/home/${USER_NAME}/.config/containers/systemd/autoupd.container" >/dev/null <<UNIT
[Container]
ContainerName=autoupd
Image=${REGISTRY}/alpine:latest
Exec=sleep infinity
AutoUpdate=registry
UNIT
as_user systemctl --user daemon-reload
as_user systemctl --user start autoupd.service

# A root container that belongs to another podman instance.
retry "$PODMAN" pull -q "${REGISTRY}/busybox:1.36"
"$PODMAN" run -d --name root-only "${REGISTRY}/busybox:1.36" sleep infinity

# The other podman, first in PATH: anything that uses it instead of the
# configured one fails loudly.
cat >/usr/local/bin/podman <<'SH'
#!/bin/sh
echo "wrong podman: /usr/local/bin/podman was used instead of containers.podman.path" >&2
exit 1
SH
chmod 0755 /usr/local/bin/podman

as_user "$PODMAN" ps -a
"$PODMAN" ps -a
