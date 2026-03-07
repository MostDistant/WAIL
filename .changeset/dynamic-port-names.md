---
default: minor
---

Dynamic DAW aux output port names via nih_plug fork. CLAP hosts now show peer display names (e.g. "Ringo") instead of static "Slot 1" labels when peers join a session. Adds PeerName IPC message to forward display names from the Tauri session to the recv plugin. VST3 hosts still show static names (no equivalent API).
