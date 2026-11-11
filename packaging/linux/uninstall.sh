#!/bin/sh
# ถอน PhoneDrop (ไฟล์ที่รับมาและการตั้งค่าใน ~/.config/PhoneDrop ไม่ถูกลบ)
pkill -x phonedrop 2>/dev/null || true
rm -f "$HOME/.local/bin/phonedrop" \
      "$HOME/.local/share/applications/phonedrop.desktop" \
      "$HOME/.local/share/icons/hicolor/512x512/apps/phonedrop.png" \
      "$HOME/.config/autostart/phonedrop.desktop"
update-desktop-database "$HOME/.local/share/applications" 2>/dev/null || true
echo "ถอน PhoneDrop แล้ว"
