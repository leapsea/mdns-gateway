#!/bin/sh
set -eu
cd "$(dirname "$0")/.."
tag="${MDNS_GATEWAY_IMAGE_TAG:-mdns-gateway:local}"
docker build --file Dockerfile --tag "$tag" .
printf 'Local test image built: %s\n' "$tag"
