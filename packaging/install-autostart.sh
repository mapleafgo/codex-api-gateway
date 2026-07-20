#!/usr/bin/env bash
# 安装 codex-api-gateway 到当前用户的桌面自启（~/.config/autostart）。
# 用法：
#   ./packaging/install-autostart.sh              # 用仓库根目录的二进制
#   ./packaging/install-autostart.sh /path/to/bin # 指定二进制
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
BIN="${1:-$ROOT/codex-api-gateway}"
CONFIG="${CONFIG:-$ROOT/config.yaml}"
ICON_SRC="$ROOT/assets/logo.png"
AUTOSTART_DIR="${XDG_CONFIG_HOME:-$HOME/.config}/autostart"
APPS_DIR="${XDG_DATA_HOME:-$HOME/.local/share}/applications"
ICONS_DIR="${XDG_DATA_HOME:-$HOME/.local/share}/icons/hicolor/48x48/apps"
DESKTOP_NAME="codex-api-gateway.desktop"

if [[ ! -x "$BIN" ]]; then
  echo "错误: 找不到可执行文件: $BIN" >&2
  echo "请先 task build，或传入绝对路径。" >&2
  exit 1
fi
BIN="$(cd "$(dirname "$BIN")" && pwd)/$(basename "$BIN")"
WORKDIR="$(cd "$(dirname "$BIN")" && pwd)"

mkdir -p "$AUTOSTART_DIR" "$APPS_DIR" "$ICONS_DIR"

ICON_NAME="codex-api-gateway"
if [[ -f "$ICON_SRC" ]]; then
  cp -f "$ICON_SRC" "$ICONS_DIR/${ICON_NAME}.png"
  ICON_VALUE="$ICON_NAME"
else
  ICON_VALUE=""
fi

# Exec: 绝对路径 + 显式 config，工作目录与二进制同目录（首次运行可生成 config.yaml）
EXEC_LINE="\"$BIN\" -config \"$CONFIG\""

tmp="$(mktemp)"
cat >"$tmp" <<DESKTOP
[Desktop Entry]
Type=Application
Version=1.0
Name=Codex API Gateway
Comment=OpenAI Responses → Anthropic 兼容后端本地网关
Exec=$EXEC_LINE
Path=$WORKDIR
Icon=$ICON_VALUE
Terminal=false
Categories=Network;Utility;
StartupNotify=false
X-GNOME-Autostart-enabled=true
X-GNOME-Autostart-Delay=3
X-KDE-autostart-after=panel
DESKTOP

install -m 644 "$tmp" "$APPS_DIR/$DESKTOP_NAME"
install -m 644 "$tmp" "$AUTOSTART_DIR/$DESKTOP_NAME"
rm -f "$tmp"

# 刷新桌面数据库（可选）
if command -v update-desktop-database >/dev/null 2>&1; then
  update-desktop-database "$APPS_DIR" 2>/dev/null || true
fi

echo "已安装桌面入口:"
echo "  应用菜单: $APPS_DIR/$DESKTOP_NAME"
echo "  开机自启: $AUTOSTART_DIR/$DESKTOP_NAME"
echo "  二进制:   $BIN"
echo "  配置:     $CONFIG"
echo
echo "提示: 日常开关优先用托盘菜单「开机自启」（读写同一 desktop 文件）。"
echo "若仍启用 systemd --user 的 codex-api-gateway.service，请先禁用，避免双实例:"
echo "  systemctl --user disable --now codex-api-gateway.service"
