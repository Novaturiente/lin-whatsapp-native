#!/bin/sh
# Installs or updates lin-whatsapp-native from the latest GitHub release.
# Run it again to update. Uninstall: sh install.sh --uninstall
# Your chats and login in ~/.local/share/lin-whatsapp-native are never touched.
set -eu

REPO=Novaturiente/lin-whatsapp-native
APP=lin-whatsapp-native
# Launcher entry and icon are named after the GTK app ID so docks and switchers match the window.
ID=io.github.nova.LinWhatsappNative
BIN_DIR="$HOME/.local/bin"
APPS_DIR="${XDG_DATA_HOME:-$HOME/.local/share}/applications"
ICON_DIR="${XDG_DATA_HOME:-$HOME/.local/share}/icons/hicolor/scalable/apps"

if [ "${1:-}" = "--uninstall" ]; then
	rm -f "$BIN_DIR/$APP" "$APPS_DIR/$ID.desktop" "$ICON_DIR/$ID.svg" "$APPS_DIR/$APP.desktop" "$ICON_DIR/$APP.svg"
	echo "Removed $APP. Chats stay in ~/.local/share/$APP (delete it to unlink this computer)."
	exit 0
fi

[ "$(uname -s)" = Linux ] && [ "$(uname -m)" = x86_64 ] || {
	echo "Only Linux x86_64 builds are released. Build from source: go build -o $APP ." >&2
	exit 1
}
command -v curl >/dev/null || { echo "curl is required." >&2; exit 1; }

URL="https://github.com/$REPO/releases/latest/download"
TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT

echo "Downloading the latest release…"
for f in "$APP-linux-amd64" "$APP.desktop" icon.svg SHA256SUMS; do
	curl -fsSL -o "$TMP/$f" "$URL/$f"
done
(cd "$TMP" && sha256sum -c --quiet SHA256SUMS)

mkdir -p "$BIN_DIR" "$APPS_DIR" "$ICON_DIR"
install -m 755 "$TMP/$APP-linux-amd64" "$BIN_DIR/$APP"
install -m 644 "$TMP/icon.svg" "$ICON_DIR/$ID.svg"
# Full path: desktop sessions often lack ~/.local/bin in PATH.
sed "s|^Exec=.*|Exec=$BIN_DIR/$APP|" "$TMP/$APP.desktop" >"$APPS_DIR/$ID.desktop"
# Files from v0.1.0/v0.1.1 used the short name; drop them to avoid a duplicate launcher entry.
rm -f "$APPS_DIR/$APP.desktop" "$ICON_DIR/$APP.svg"
command -v update-desktop-database >/dev/null && update-desktop-database -q "$APPS_DIR" || true

echo "Installed $APP to $BIN_DIR/$APP."
missing=""
for lib in libgtk-4.so.1 libadwaita-1.so.0; do
	ldconfig -p 2>/dev/null | grep -q "$lib" || missing="$missing $lib"
done
[ -z "$missing" ] || echo "Missing libraries:$missing. Install gtk4 and libadwaita (1.5 or newer)."
for tool in ffmpeg notify-send gst-launch-1.0; do
	command -v "$tool" >/dev/null || echo "Optional: $tool not found (see README, Requirements)."
done
echo "Start it from your app launcher (WhatsApp (native)) or run: $BIN_DIR/$APP"
