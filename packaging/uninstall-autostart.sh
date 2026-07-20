#!/usr/bin/env bash
# 移除桌面自启与应用菜单入口。
set -euo pipefail
AUTOSTART_DIR="${XDG_CONFIG_HOME:-$HOME/.config}/autostart"
APPS_DIR="${XDG_DATA_HOME:-$HOME/.local/share}/applications"
ICONS_DIR="${XDG_DATA_HOME:-$HOME/.local/share}/icons/hicolor/48x48/apps"
DESKTOP_NAME="codex-api-gateway.desktop"

rm -f "$AUTOSTART_DIR/$DESKTOP_NAME" "$APPS_DIR/$DESKTOP_NAME" "$ICONS_DIR/codex-api-gateway.png"
echo "已移除 $DESKTOP_NAME（autostart + applications）"
