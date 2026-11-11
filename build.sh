#!/usr/bin/env bash
# build PhoneDrop — รันจาก Git Bash (Windows) หรือ shell ปกติ (Linux/macOS)
#   ./build.sh            build สำหรับเครื่องนี้
#   ./build.sh windows    ได้ dist/PhoneDrop-Setup.exe
#   ./build.sh linux      ได้ dist/PhoneDrop-linux-{amd64,arm64}.tar.gz (cross-compile จาก Windows ได้)
#   ./build.sh darwin     ได้ dist/PhoneDrop-macOS.zip — ต้องรันบน Mac เพราะไอคอนแถบเมนูใช้ cgo
set -euo pipefail
cd "$(dirname "$0")"

# ใช้ Go ที่อยู่ใน .tools ถ้ามี ไม่งั้นใช้ตัวที่ติดตั้งในระบบ
[ -d .tools/go/bin ] && export PATH="$PWD/.tools/go/bin:$PATH"

mkdir -p dist
go run ./tools/mkico

target="${1:-$(go env GOOS)}"
case "$target" in
  windows)
    # ฝังไอคอนลงใน .exe
    go run github.com/akavel/rsrc@v0.10.2 -ico assets/icon.ico -arch amd64 -o rsrc_windows_amd64.syso
    # -H=windowsgui: ไม่มีหน้าต่าง console, -s -w: ตัดข้อมูล debug ให้ไฟล์เล็ก
    GOOS=windows GOARCH=amd64 go build -trimpath -ldflags "-H=windowsgui -s -w" -o dist/PhoneDrop.exe .

    makensis="$(command -v makensis || true)"
    if [ -z "$makensis" ]; then
      makensis="$(find "${LOCALAPPDATA:-/nonexistent}/electron-builder/Cache" -name makensis.exe -path '*Bin*' 2>/dev/null | head -1 || true)"
    fi
    if [ -z "$makensis" ] && [ -x "/c/Program Files (x86)/NSIS/makensis.exe" ]; then
      makensis="/c/Program Files (x86)/NSIS/makensis.exe"
    fi
    if [ -n "$makensis" ]; then
      # INPUTCHARSET: สคริปต์เป็น UTF-8 (มีข้อความภาษาไทย)
      MSYS2_ARG_CONV_EXCL="*" "$makensis" -V2 -INPUTCHARSET UTF8 installer.nsi
    else
      echo "ไม่พบ makensis — ได้เฉพาะ dist/PhoneDrop.exe (ติดตั้ง NSIS เพื่อสร้างตัวติดตั้ง)"
    fi
    ;;

  linux)
    for arch in amd64 arm64; do
      stage="dist/stage/PhoneDrop-linux-$arch"
      rm -rf "$stage"; mkdir -p "$stage"
      GOOS=linux GOARCH=$arch CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o "$stage/phonedrop" .
      cp packaging/linux/install.sh packaging/linux/uninstall.sh "$stage/"
      cp assets/icon.png "$stage/phonedrop.png"
      # --mode: ตั้งสิทธิ์ execute ให้ถูกแม้ build บน Windows
      tar -C dist/stage --owner=0 --group=0 --mode='u=rwx,go=rx' -czf "dist/PhoneDrop-linux-$arch.tar.gz" "PhoneDrop-linux-$arch"
    done
    rm -rf dist/stage
    ;;

  darwin)
    if [ "$(uname -s)" != "Darwin" ]; then
      echo "ตัว macOS ต้อง build บนเครื่อง Mac"; exit 1
    fi
    app="dist/PhoneDrop.app"
    rm -rf "$app"; mkdir -p "$app/Contents/MacOS" "$app/Contents/Resources"

    # universal binary: ใช้ได้ทั้ง Apple Silicon และ Intel
    CGO_ENABLED=1 GOOS=darwin GOARCH=arm64 go build -trimpath -ldflags "-s -w" -o dist/pd-arm64 .
    CGO_ENABLED=1 GOOS=darwin GOARCH=amd64 go build -trimpath -ldflags "-s -w" -o dist/pd-amd64 .
    lipo -create -output "$app/Contents/MacOS/PhoneDrop" dist/pd-arm64 dist/pd-amd64
    rm dist/pd-arm64 dist/pd-amd64

    # ไอคอน .icns จาก assets/icon.png
    iconset="dist/icon.iconset"
    rm -rf "$iconset"; mkdir -p "$iconset"
    for s in 16 32 128 256; do
      sips -z $s $s assets/icon.png --out "$iconset/icon_${s}x${s}.png" >/dev/null
      sips -z $((s*2)) $((s*2)) assets/icon.png --out "$iconset/icon_${s}x${s}@2x.png" >/dev/null
    done
    iconutil -c icns "$iconset" -o "$app/Contents/Resources/icon.icns"
    rm -rf "$iconset"

    cp packaging/macos/Info.plist "$app/Contents/Info.plist"
    codesign --force --deep -s - "$app"   # เซ็นแบบ ad-hoc (ไม่มีใบรับรองนักพัฒนา)
    rm -f dist/PhoneDrop-macOS.zip
    ditto -c -k --keepParent "$app" dist/PhoneDrop-macOS.zip
    ;;

  *) echo "ไม่รู้จัก target: $target"; exit 1 ;;
esac
ls -la dist
