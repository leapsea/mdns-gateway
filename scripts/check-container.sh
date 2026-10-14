#!/bin/sh
set -eu
cd "$(dirname "$0")/.."
# CI 检查 Buildx 已生成的镜像，本地默认仍执行构建。
if [ "${MDNS_GATEWAY_SKIP_BUILD:-0}" != 1 ]; then
  sh build/build-local-image.sh
fi
tag="${MDNS_GATEWAY_IMAGE_TAG:-mdns-gateway:local}"
name="mdns-gateway-smoke-$$"
data_dir="$(mktemp -d)"
cleanup() { docker rm -f "$name" >/dev/null 2>&1 || true; rm -rf "$data_dir"; }
trap cleanup EXIT INT TERM
set --
if [ -n "${MDNS_GATEWAY_PLATFORM:-}" ]; then
  set -- --platform "$MDNS_GATEWAY_PLATFORM"
fi
docker run "$@" -d --name "$name" --network host -v "$data_dir:/data" "$tag" >/dev/null
attempt=0
while [ "$attempt" -lt 20 ]; do
  if docker exec "$name" wget -q -O - http://127.0.0.1:8080/health; then break; fi
  attempt=$((attempt + 1))
  sleep 1
done
if [ "$attempt" -eq 20 ]; then docker logs "$name"; exit 1; fi
docker exec "$name" wget -q -O - http://127.0.0.1:8080/api/v1/auth/setup-status
docker exec "$name" openresty -t -c /data/openresty/current/nginx.conf
echo 'Container startup, health and active OpenResty configuration passed.'
