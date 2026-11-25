#!/usr/bin/env bash
# Gives a hosted runner a Docker engine with the fix of moby/moby#50054, which Docker 28.3.0 shipped.
# The image ubuntu-24.04 pins Docker 28.0.4, which can give one ephemeral host port both to a mapping
# on 127.0.0.1, as the trial load maps its control listener (Edge ADR 0004), and to one on 0.0.0.0, as
# testcontainers maps a scenario's Edge. The scenarios run in parallel, so containers failed to start
# with "address already in use" (run 45532153358). The image removes Docker's apt repository after it
# installs the engine, so this adds it again and upgrades the same daemon in place.
set -euo pipefail

FIXED=28.3.0   # the first engine with the fix
VERSION=29.2.1 # the engine that a runner with an older one gets

current=$(docker version --format '{{.Server.Version}}')
if [ "$(printf '%s\n' "$FIXED" "$current" | sort -V | head -n 1)" = "$FIXED" ]; then
  echo "Docker engine $current holds the fix"
  exit 0
fi

key=/usr/share/keyrings/docker.gpg
list=/etc/apt/sources.list.d/docker.list
codename=$(lsb_release -cs)
curl -fsSL https://download.docker.com/linux/ubuntu/gpg | sudo gpg --dearmor --yes --output "$key"
echo "deb [arch=$(dpkg --print-architecture) signed-by=$key] https://download.docker.com/linux/ubuntu $codename stable" |
  sudo tee "$list" >/dev/null
# Docker's list alone, so that the update does not read every repository of the image.
sudo apt-get update -o Dir::Etc::sourcelist="$list" -o Dir::Etc::sourceparts=- -o APT::Get::List-Cleanup=0
package="5:$VERSION-1~ubuntu.$(lsb_release -rs)~$codename"
sudo DEBIAN_FRONTEND=noninteractive apt-get install --yes --no-install-recommends \
  -o Dpkg::Options::=--force-confold "docker-ce=$package" "docker-ce-cli=$package"
sudo systemctl restart docker.service

server=
for _ in $(seq 30); do
  server=$(docker version --format '{{.Server.Version}}' 2>/dev/null) && break
  sleep 1
done
if [ "$server" != "$VERSION" ]; then
  echo "::error::the Docker engine is ${server:-not running}, not $VERSION"
  exit 1
fi
echo "Docker engine $current replaced by $server"
