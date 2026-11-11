#!/usr/bin/env bash
# ทดสอบตัว Linux แบบไม่มีหน้าจอ: เปิดโปรแกรม ส่งไฟล์เข้าไป แล้วเทียบ checksum
#   bash packaging/linux/smoke-test.sh dist/PhoneDrop-linux-amd64.tar.gz
set -euo pipefail
tarball="$(realpath "$1")"
work="$(mktemp -d)"
trap 'kill "${pid:-}" 2>/dev/null || true; rm -rf "$work"' EXIT

tar -C "$work" -xzf "$tarball"
bin="$(echo "$work"/PhoneDrop-linux-*/phonedrop)"
test -x "$bin"
test -x "$(dirname "$bin")/install.sh"

mkdir -p "$work/cfg" "$work/recv"
printf '{"token":"smoketest","dir":"%s","notify":false}' "$work/recv" > "$work/cfg/config.json"

# ไอคอนถาดระบบต้องใช้ D-Bus session — ใช้ dbus-run-session ถ้ามี
runner=()
command -v dbus-run-session >/dev/null && runner=(dbus-run-session --)
"${runner[@]}" "$bin" --hidden --no-autostart --config "$work/cfg" &
pid=$!

port="$(for i in $(seq 1 20); do
  p="$(sed -n 's/.*"lastPort": *\([0-9]*\).*/\1/p' "$work/cfg/config.json" 2>/dev/null || true)"
  if [ -n "$p" ] && curl -fs "http://127.0.0.1:$p/api/ping" >/dev/null 2>&1; then echo "$p"; break; fi
  sleep 0.5
done)"
[ -n "$port" ] || { echo "โปรแกรมไม่ตอบสนอง"; exit 1; }

head -c 5000000 /dev/urandom > "$work/sample.bin"
curl -fs -X PUT --data-binary @"$work/sample.bin" "http://127.0.0.1:$port/api/upload/smoketest?name=sample.bin"
echo
cmp "$work/sample.bin" "$work/recv/sample.bin"
[ "$(curl -s -o /dev/null -w '%{http_code}' -X PUT --data-binary @"$work/sample.bin" "http://127.0.0.1:$port/api/upload/wrong?name=x.bin")" = "404" ]
curl -fs "http://127.0.0.1:$port/s/smoketest" | grep -q "<title>PhoneDrop</title>"
echo "smoke test ผ่าน (port $port)"
