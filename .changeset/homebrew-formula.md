---
"wail-tauri": minor
---

Add Homebrew from-source installation support. Users on macOS can now install WAIL and its DAW plugins directly from source via `brew tap quasor/wail && brew install quasor/wail/wail`. A new `cargo xtask bundle-plugin` command assembles CLAP/VST3 plugin bundles without requiring `cargo-nih-plug` to be installed globally. A `wail-install-plugins` helper script copies the installed plugin bundles to `~/Library/Audio/Plug-Ins/`.
