#!/bin/sh
# 只读取本机 Docker/网络状态；不会修改容器或访问云端。
set -u
cd "$(dirname "$0")/.."
domain="${1:-media.local}"
target_url="${2:-http://192.168.50.21:2030/}"

printf '== Docker host ==\n'
docker info --format 'OS={{.OperatingSystem}} Server={{.ServerVersion}}' 2>&1 || true
docker info --format 'SecurityOptions={{json .SecurityOptions}}' 2>&1 || true

printf '\n== Host network interfaces ==\n'
if command -v ip >/dev/null 2>&1; then
  ip -4 -br addr
  ip route
else
  ifconfig | grep -E '^[a-zA-Z0-9]|inet '
fi

printf '\n== Compose service ==\n'
docker compose -f deploy/compose.yml ps -a 2>&1 || true
docker compose -f deploy/compose.yml logs --tail=30 2>&1 || true
container_id="$(docker compose -f deploy/compose.yml ps -q mdns-gateway 2>/dev/null)"
if [ -z "$container_id" ]; then
  printf 'No running mdns-gateway container was found.\n'
  exit 0
fi

printf '\n== Container network mode and published ports ==\n'
docker inspect --format 'NetworkMode={{.HostConfig.NetworkMode}} Ports={{json .NetworkSettings.Ports}}' "$container_id" 2>&1 || true
listen_addr="$(docker exec "$container_id" printenv GATEWAY_LISTEN 2>/dev/null)"
port="${listen_addr##*:}"
port="${port:-8082}"
port_hex="$(printf '%04X' "$port")"
printf 'GATEWAY_LISTEN=%s\n' "$listen_addr"
docker exec "$container_id" printenv GATEWAY_IPV4 2>&1 || true
docker exec "$container_id" ip -4 addr 2>&1 || true

printf '\n== Inside container: health and TCP listeners ==\n'
docker exec "$container_id" wget -q -O - "http://127.0.0.1:$port/health" 2>&1 || true
docker exec "$container_id" sh -c 'cat /proc/net/tcp /proc/net/tcp6' | grep -i ":$port_hex" || true

printf '\n== Host: localhost health ==\n'
curl --noproxy '*' --connect-timeout 3 --max-time 5 -v "http://127.0.0.1:$port/health" 2>&1 || true

printf '\n== Host listeners ==\n'
if command -v ss >/dev/null 2>&1; then
  ss -lntup | grep -E ":(80|5353|$port)([[:space:]]|$)" || true
else
  lsof -nP -iTCP:"$port" -sTCP:LISTEN 2>&1 || true
fi

printf '\n== Proxy with domain resolution bypassed: %s ==\n' "$domain"
curl --noproxy '*' --connect-timeout 3 --max-time 5 -sS -D - -o /dev/null -H "Host: $domain" http://127.0.0.1/ 2>&1 || true

printf '\n== Container upstream: %s ==\n' "$target_url"
docker exec "$container_id" wget -S --spider -T 5 "$target_url" 2>&1 || true

printf '\n== Host domain resolution and HTTP: %s ==\n' "$domain"
curl --noproxy '*' --connect-timeout 3 --max-time 5 -sS -D - -o /dev/null "http://$domain/" 2>&1 || true

if command -v ufw >/dev/null 2>&1; then
  printf '\n== UFW status (requires permission) ==\n'
  ufw status 2>&1 || true
fi
