#!/bin/sh
# ติดตั้ง PhoneDrop สำหรับผู้ใช้ปัจจุบัน (ไม่ต้องใช้ sudo)
#   ./install.sh
set -e
DIR="$(cd "$(dirname "$0")" && pwd)"
BIN="$HOME/.local/bin"
APPS="$HOME/.local/share/applications"
ICONS="$HOME/.local/share/icons/hicolor/512x512/apps"

mkdir -p "$BIN" "$APPS" "$ICONS"

# ปิดตัวที่เปิดอยู่ก่อน จะได้เขียนทับได้
pkill -x phonedrop 2>/dev/null || true
sleep 1

install -m 755 "$DIR/phonedrop" "$BIN/phonedrop"
install -m 644 "$DIR/phonedrop.png" "$ICONS/phonedrop.png"

cat > "$APPS/phonedrop.desktop" <<EOF
[Desktop Entry]
Type=Application
Name=PhoneDrop
Comment=Receive photos and files from your phone over Wi-Fi
Exec=$BIN/phonedrop
Icon=phonedrop
Terminal=false
Categories=Utility;Network;
EOF

update-desktop-database "$APPS" 2>/dev/null || true
gtk-update-icon-cache -q "$HOME/.local/share/icons/hicolor" 2>/dev/null || true

echo "ติดตั้งแล้วที่ $BIN/phonedrop"
echo "เปิดจากเมนูแอป (PhoneDrop) ได้เลย — กำลังเปิดให้ครั้งแรก..."
echo "ถ้ามือถือเปิดหน้าเว็บไม่ได้ ให้เปิดพอร์ตที่ไฟร์วอลล์ เช่น: sudo ufw allow 5050/tcp"

nohup "$BIN/phonedrop" >/dev/null 2>&1 &
