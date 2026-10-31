#!/bin/bash
# Entrypoint for the Fly runner Machines (fly.runner.toml copies it in with [[files]]).
#
# Fly's root filesystem is an overlay, and Docker can't stack its own overlayfs layers on
# one ("not supported as upperdir"); the vfs driver works but copies every layer in full.
# So /var/lib/docker gets an ext4 filesystem in a sparse file on a loop device, sized to
# the root filesystem it lives on, before the image's entrypoint starts dockerd
# (START_DOCKER_SERVICE). Fly rebuilds the root filesystem on every start, so each job
# gets an empty one.
set -euo pipefail

img=/var/lib/docker.img
truncate -s "$(df --output=size -B1 / | tail -n 1)" "$img"
mkfs.ext4 -q -F "$img"
mkdir -p /var/lib/docker
mount -o loop "$img" /var/lib/docker

exec /entrypoint.sh "$@"
