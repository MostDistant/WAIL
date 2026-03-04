---
default: patch
---

### Fixed
- Fix Homebrew formula installing doubly-nested plugin bundles (e.g. `wail-plugin-send.clap/wail-plugin-send.clap/Contents/...`)
- Fix release workflow uploading stale Tauri installer artifacts from cached `target` directory
