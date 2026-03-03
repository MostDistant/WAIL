#!/usr/bin/env bash
# Install WAIL DAW plugins to the user's Audio/Plug-Ins directories.
#
# Usage:
#   wail-install-plugins [PREFIX]
#
# PREFIX defaults to the Homebrew prefix (brew --prefix). Override it to point
# at a directory that contains the plugin bundles under lib/:
#
#   wail-install-plugins /opt/homebrew          # default when installed via Homebrew
#   wail-install-plugins /path/to/target/bundled  # when building from source locally

set -euo pipefail

# Determine plugin bundle source directory.
if [ $# -ge 1 ]; then
    PREFIX="$1"
else
    PREFIX="$(brew --prefix 2>/dev/null)" || {
        echo "error: could not determine Homebrew prefix; pass the plugin directory as an argument." >&2
        exit 1
    }
fi

SRC_DIR="${PREFIX}/lib"
CLAP_DEST="${HOME}/Library/Audio/Plug-Ins/CLAP"
VST3_DEST="${HOME}/Library/Audio/Plug-Ins/VST3"

mkdir -p "$CLAP_DEST" "$VST3_DEST"

install_bundle() {
    local src="$1"
    local dest_dir="$2"
    local name
    name="$(basename "$src")"

    if [ ! -e "$src" ]; then
        echo "warning: $src not found, skipping." >&2
        return
    fi

    local dest="${dest_dir}/${name}"
    if [ -e "$dest" ]; then
        rm -rf "$dest"
    fi
    cp -r "$src" "$dest"
    echo "Installed: $dest"
}

install_bundle "${SRC_DIR}/wail-plugin-send.clap" "$CLAP_DEST"
install_bundle "${SRC_DIR}/wail-plugin-recv.clap" "$CLAP_DEST"
install_bundle "${SRC_DIR}/wail-plugin-send.vst3" "$VST3_DEST"
install_bundle "${SRC_DIR}/wail-plugin-recv.vst3" "$VST3_DEST"

echo ""
echo "Done. Rescan plugins in your DAW to pick up the changes."
