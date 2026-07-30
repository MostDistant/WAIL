#!/usr/bin/env bash
# Install the WAIL CLAP plugins into the user's plugin directory.
#
# Homebrew can't write into ~/Library/Audio/Plug-Ins itself, so this copies the
# bundles the formula installed under <prefix>/lib into your CLAP folder.
#
# Usage:
#   wail-install-plugins [PREFIX]
#
# PREFIX defaults to the Homebrew prefix (brew --prefix). Override it to point at a
# directory that contains the plugin bundles under lib/, e.g. a local build dir:
#
#   wail-install-plugins /opt/homebrew                # Homebrew install
#   wail-install-plugins "$(pwd)/build/plugins"       # local cmake build (bundles at top level)

set -euo pipefail

if [ $# -ge 1 ]; then
    PREFIX="$1"
else
    PREFIX="$(brew --prefix 2>/dev/null)" || {
        echo "error: could not determine Homebrew prefix; pass the plugin directory as an argument." >&2
        exit 1
    }
fi

# Accept both a Homebrew-style prefix (bundles under lib/) and a raw build dir
# (bundles at the top level). Product bundles only — dev tools
# (transport-probe, linkbridge-spike) are never installed.
if [ -e "${PREFIX}/lib/wail-send.clap" ]; then
    SRC_DIR="${PREFIX}/lib"
else
    SRC_DIR="${PREFIX}"
fi

case "$(uname -s)" in
    Darwin) CLAP_DEST="${HOME}/Library/Audio/Plug-Ins/CLAP" ;;
    *)      CLAP_DEST="${HOME}/.clap" ;;
esac

mkdir -p "$CLAP_DEST"

install_bundle() {
    local src="$1"
    local name
    name="$(basename "$src")"
    if [ ! -e "$src" ]; then
        echo "warning: $src not found, skipping." >&2
        return
    fi
    local dest="${CLAP_DEST}/${name}"
    rm -rf "$dest"
    # -L: dereference symlinks. Under Homebrew, ${PREFIX}/lib/<name>.clap is a
    # *relative* symlink into the Cellar — a plain `cp -R` copies the link
    # verbatim, leaving a broken symlink in the CLAP folder.
    cp -RL "$src" "$dest"
    echo "Installed: $dest"
}

install_bundle "${SRC_DIR}/wail-send.clap"
install_bundle "${SRC_DIR}/wail-recv.clap"

echo ""
echo "Done. Rescan plugins in your DAW to pick up the changes."
