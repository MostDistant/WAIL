#!/usr/bin/env bash
# package-macos.sh — build WAIL.app and a distributable .dmg for macOS.
#
# The app binary is self-contained except for libopus (cgo, dynamically
# linked from Homebrew), so the bundle carries its own copy in
# Contents/Frameworks and the binary's reference is rewritten to
# @executable_path. The bundle is ad-hoc signed: first launch shows
# Gatekeeper's "developer cannot be verified" — right-click → Open once.
# (Developer ID signing + notarization is a separate, later step.)
#
# Usage:
#   scripts/package-macos.sh --version X.Y.Z [--binary PATH] [--plugins DIR] [--out DIR]
#
#   --version   Version string baked into the binary and Info.plist (required).
#   --binary    Prebuilt wail binary (default: build wail-app from source).
#   --plugins   Directory containing wail-send.clap and wail-recv.clap; staged
#               into Contents/lib where the app's first-launch installer finds
#               them (FindPluginDir → {exe}/../lib). Warns loudly if omitted.
#   --out       Output directory for the .dmg (default: dist/macos).
set -euo pipefail

VERSION=""
BINARY=""
PLUGINS_DIR=""
OUT_DIR="dist/macos"

while [ $# -gt 0 ]; do
	case "$1" in
		--version) VERSION="$2"; shift 2 ;;
		--binary)  BINARY="$2"; shift 2 ;;
		--plugins) PLUGINS_DIR="$2"; shift 2 ;;
		--out)     OUT_DIR="$2"; shift 2 ;;
		*) echo "unknown argument: $1" >&2; exit 2 ;;
	esac
done

if [ -z "$VERSION" ]; then
	echo "error: --version is required" >&2
	exit 2
fi
if [ "$(uname -s)" != "Darwin" ]; then
	echo "error: package-macos.sh must run on macOS" >&2
	exit 1
fi

REPO_ROOT="$(cd "$(dirname "$0")/.." && pwd)"
WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT

# --- 1. Build (or accept) the app binary -----------------------------------
if [ -z "$BINARY" ]; then
	echo ">> building wail-app (version $VERSION)"
	# nolibopusfile: the opus binding's stream.go (libopusfile) is unused by
	# WAIL — suppressing it leaves libopus as the only non-system dylib.
	(cd "$REPO_ROOT/wail-app" && go build -tags nolibopusfile \
		-ldflags "-X main.appVersion=${VERSION}" -o "$WORK/wail" .)
	BINARY="$WORK/wail"
fi

# --- 2. Stage the .app bundle ----------------------------------------------
APP="$WORK/WAIL.app"
mkdir -p "$APP/Contents/MacOS" "$APP/Contents/Frameworks" "$APP/Contents/Resources"
cp "$BINARY" "$APP/Contents/MacOS/wail"

cat > "$APP/Contents/Info.plist" <<EOF
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>CFBundlePackageType</key><string>APPL</string>
	<key>CFBundleInfoDictionaryVersion</key><string>6.0</string>
	<key>CFBundleName</key><string>WAIL</string>
	<key>CFBundleDisplayName</key><string>WAIL</string>
	<key>CFBundleIdentifier</key><string>com.mostdistant.wail</string>
	<key>CFBundleExecutable</key><string>wail</string>
	<key>CFBundleVersion</key><string>${VERSION}</string>
	<key>CFBundleShortVersionString</key><string>${VERSION}</string>
	<key>CFBundleIconFile</key><string>AppIcon</string>
	<key>LSMinimumSystemVersion</key><string>11.0</string>
	<key>NSHighResolutionCapable</key><true/>
</dict>
</plist>
EOF

# Icon: the frontend logo, rendered into an .icns at the sizes the source
# resolution honestly supports (128px source → up to 128x128).
ICON_SRC="$REPO_ROOT/wail-app/frontend/logo.png"
if [ -f "$ICON_SRC" ]; then
	ICONSET="$WORK/AppIcon.iconset"
	mkdir -p "$ICONSET"
	for spec in "16:16" "16@2x:32" "32:32" "32@2x:64" "128:128"; do
		name="${spec%%:*}"; px="${spec##*:}"
		sips -z "$px" "$px" "$ICON_SRC" --out "$ICONSET/icon_${name}x${name}.png" >/dev/null
	done
	iconutil -c icns "$ICONSET" -o "$APP/Contents/Resources/AppIcon.icns"
else
	echo ">> warning: no icon source at $ICON_SRC — bundle gets the default icon"
fi

# --- 3. Bundle non-system dylibs (libopus) ---------------------------------
# Anything linked from a package-manager prefix gets copied into
# Contents/Frameworks and rewritten to @executable_path. Loops to catch
# transitive deps (e.g. opusfile → ogg) if the link set ever grows.
rewrite_refs() { # $1 = mach-o file to rewrite
	local file="$1" dep base
	while read -r dep; do
		base="$(basename "$dep")"
		install_name_tool -change "$dep" "@executable_path/../Frameworks/$base" "$file"
	done < <(otool -L "$file" | awk '/^[[:space:]]+\/(opt\/homebrew|usr\/local)/ {print $1}')
}

for pass in 1 2 3; do
	targets=("$APP/Contents/MacOS/wail")
	for d in "$APP"/Contents/Frameworks/*.dylib; do
		[ -e "$d" ] && targets+=("$d")
	done
	pending="$(otool -L "${targets[@]}" | awk '/^[[:space:]]+\/(opt\/homebrew|usr\/local)/ {print $1}' | sort -u)"
	[ -z "$pending" ] && break
	while read -r dep; do
		[ -z "$dep" ] && continue
		base="$(basename "$dep")"
		if [ ! -f "$APP/Contents/Frameworks/$base" ]; then
			echo ">> bundling $base"
			cp "$dep" "$APP/Contents/Frameworks/$base"
			chmod +w "$APP/Contents/Frameworks/$base"
			install_name_tool -id "@executable_path/../Frameworks/$base" "$APP/Contents/Frameworks/$base"
		fi
	done <<< "$pending"
done
rewrite_refs "$APP/Contents/MacOS/wail"
for dylib in "$APP"/Contents/Frameworks/*.dylib; do
	[ -e "$dylib" ] || continue
	rewrite_refs "$dylib"
done

if otool -L "$APP/Contents/MacOS/wail" | grep -qE '/(opt/homebrew|usr/local)/'; then
	echo "error: binary still references package-manager dylibs:" >&2
	otool -L "$APP/Contents/MacOS/wail" | grep -E '/(opt/homebrew|usr/local)/' >&2
	exit 1
fi

# --- 4. Stage the CLAP plugins ---------------------------------------------
if [ -n "$PLUGINS_DIR" ]; then
	mkdir -p "$APP/Contents/lib"
	for b in wail-send wail-recv; do
		if [ ! -e "$PLUGINS_DIR/$b.clap" ]; then
			echo "error: missing plugin bundle $PLUGINS_DIR/$b.clap" >&2
			exit 1
		fi
		cp -R "$PLUGINS_DIR/$b.clap" "$APP/Contents/lib/"
	done
else
	echo ">> WARNING: no --plugins dir — the .app will not carry the CLAP plugins" >&2
fi

# --- 5. Ad-hoc sign ---------------------------------------------------------
# arm64 requires a signature; install_name_tool invalidated the linker's.
# Sign inside-out: dylibs, plugin bundles, then the app itself.
find "$APP/Contents/Frameworks" -name '*.dylib' -exec codesign --force --sign - {} \;
if [ -d "$APP/Contents/lib" ]; then
	find "$APP/Contents/lib" -name '*.clap' -exec codesign --force --sign - {} \;
fi
codesign --force --sign - "$APP"
codesign --verify --deep --strict "$APP"

# --- 6. Build the .dmg ------------------------------------------------------
mkdir -p "$OUT_DIR"
DMG_STAGE="$WORK/dmg"
mkdir -p "$DMG_STAGE"
cp -R "$APP" "$DMG_STAGE/"
ln -s /Applications "$DMG_STAGE/Applications"
DMG="$OUT_DIR/wail-macos-arm64-${VERSION}.dmg"
hdiutil create -volname "WAIL" -srcfolder "$DMG_STAGE" -ov -format UDZO "$DMG" >/dev/null

echo ">> built $DMG"
echo ">> note: unsigned build — first launch needs right-click → Open (Gatekeeper)"
