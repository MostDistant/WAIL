use std::path::PathBuf;

/// On macOS, a valid plugin bundle is a directory (e.g. foo.clap/Contents/MacOS/…).
/// A 0-byte file or missing path is not valid.
/// On Linux/Windows, a valid bundle is a non-empty file.
fn bundle_is_valid(path: &PathBuf) -> bool {
    #[cfg(target_os = "macos")]
    return path.is_dir();
    #[cfg(not(target_os = "macos"))]
    return path.is_file() && path.metadata().map(|m| m.len() > 0).unwrap_or(false);
}

fn main() {
    let manifest_dir = std::env::var("CARGO_MANIFEST_DIR").unwrap();
    let workspace_root = PathBuf::from(&manifest_dir)
        .parent()
        .unwrap() // crates/wail-plugin-test -> crates/
        .parent()
        .unwrap() // crates/ -> workspace root
        .to_path_buf();

    let recv_bundle = workspace_root.join("target/bundled/wail-plugin-recv.clap");
    let send_bundle = workspace_root.join("target/bundled/wail-plugin-send.clap");

    if !bundle_is_valid(&recv_bundle) || !bundle_is_valid(&send_bundle) {
        // NOTE: We cannot spawn `cargo xtask bundle-plugin` here because the
        // outer cargo process holds the workspace lock, causing the inner cargo
        // to block forever (deadlock). Instead, fail fast with a clear message.
        panic!(
            "Plugin bundles missing. Build them first:\n\
             \n  cargo xtask bundle-plugin --debug\n\
             \nOr use `cargo xtask test` which handles this automatically."
        );
    }

    // Rebuild if the plugin bundles are replaced
    println!("cargo:rerun-if-changed={}", recv_bundle.display());
    println!("cargo:rerun-if-changed={}", send_bundle.display());
    // Rebuild if plugin source changes
    println!(
        "cargo:rerun-if-changed={}",
        workspace_root.join("crates/wail-plugin-recv/src").display()
    );
    println!(
        "cargo:rerun-if-changed={}",
        workspace_root.join("crates/wail-plugin-send/src").display()
    );
}
