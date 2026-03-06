use std::path::{Path, PathBuf};
use tracing::{info, warn};

struct PluginDirs {
    clap: PathBuf,
    vst3: PathBuf,
}

fn system_plugin_dirs() -> Option<PluginDirs> {
    let home = dirs::home_dir()?;
    #[cfg(target_os = "macos")]
    {
        let base = home.join("Library/Audio/Plug-Ins");
        Some(PluginDirs { clap: base.join("CLAP"), vst3: base.join("VST3") })
    }
    #[cfg(target_os = "linux")]
    {
        Some(PluginDirs { clap: home.join(".clap"), vst3: home.join(".vst3") })
    }
    #[cfg(target_os = "windows")]
    {
        let base = PathBuf::from(std::env::var("COMMONPROGRAMFILES").ok()?);
        Some(PluginDirs { clap: base.join("CLAP"), vst3: base.join("VST3") })
    }
}

/// Install plugins from bundled resources if they are not already present in
/// the system plugin directories.
pub fn install_if_missing(resource_dir: &Path) {
    let Some(dirs) = system_plugin_dirs() else {
        warn!("plugin_install: could not determine system plugin directories");
        return;
    };

    let plugins: &[(&str, &Path)] = &[
        ("wail-plugin-send.clap", &dirs.clap),
        ("wail-plugin-recv.clap", &dirs.clap),
        ("wail-plugin-send.vst3", &dirs.vst3),
        ("wail-plugin-recv.vst3", &dirs.vst3),
    ];

    for (name, dest_dir) in plugins {
        let src = resource_dir.join("plugins").join(name);
        let dest = dest_dir.join(name);

        if dest.exists() {
            continue;
        }
        if !src.exists() {
            warn!("plugin_install: bundled plugin not found: {}", src.display());
            continue;
        }
        if let Err(e) = std::fs::create_dir_all(dest_dir) {
            warn!("plugin_install: failed to create {}: {e}", dest_dir.display());
            continue;
        }
        match copy_path(&src, &dest) {
            Ok(()) => info!("plugin_install: installed {name} → {}", dest_dir.display()),
            Err(e) => warn!("plugin_install: failed to install {name}: {e}"),
        }
    }
}

/// Copy a file or directory recursively.
fn copy_path(src: &Path, dest: &Path) -> std::io::Result<()> {
    if src.is_dir() {
        std::fs::create_dir_all(dest)?;
        for entry in std::fs::read_dir(src)? {
            let entry = entry?;
            copy_path(&entry.path(), &dest.join(entry.file_name()))?;
        }
    } else {
        std::fs::copy(src, dest)?;
    }
    Ok(())
}
