---
default: patch
---

Homebrew formula now automatically installs CLAP/VST3 plugins to ~/Library/Audio/Plug-Ins/ during `brew install`, removing the need for a separate `wail-install-plugins` command. The helper script is still available for manual reinstallation.
