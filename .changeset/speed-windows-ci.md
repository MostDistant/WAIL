---
default: patch
---

Speed up Windows CI builds by caching the vcpkg opus installation, stripping PDB debug symbol files before saving the Cargo cache (reducing cache size by ~500MB–2GB), and switching to the rust-lld linker for faster linking on Windows.
