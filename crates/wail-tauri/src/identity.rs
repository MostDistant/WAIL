use std::path::PathBuf;

use tracing::{info, warn};

/// Tauri managed state wrapper for the persistent peer identity.
pub struct PeerIdentity(pub String);

const IDENTITY_FILENAME: &str = "identity";

/// Get or create a persistent identity for this WAIL installation.
///
/// The identity is a UUID stored in the app's data directory. It survives
/// reconnects and app restarts, enabling peer affinity — returning peers
/// get their original DAW aux slot back.
pub fn get_or_create(data_dir: &PathBuf) -> String {
    let path = data_dir.join(IDENTITY_FILENAME);

    // Try to read existing identity
    if let Ok(existing) = std::fs::read_to_string(&path) {
        let trimmed = existing.trim().to_string();
        if !trimmed.is_empty() {
            info!(identity = %trimmed, "Loaded persistent identity");
            return trimmed;
        }
    }

    // Generate new identity
    let identity = uuid::Uuid::new_v4().to_string();

    // Ensure data dir exists and write
    if let Err(e) = std::fs::create_dir_all(data_dir) {
        warn!(error = %e, "Failed to create data dir for identity — using ephemeral identity");
        return identity;
    }
    if let Err(e) = std::fs::write(&path, &identity) {
        warn!(error = %e, "Failed to persist identity — using ephemeral identity");
    } else {
        info!(identity = %identity, path = %path.display(), "Created new persistent identity");
    }

    identity
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn creates_and_reloads_identity() {
        let dir = std::env::temp_dir().join(format!("wail-identity-test-{}", uuid::Uuid::new_v4()));
        let id1 = get_or_create(&dir);
        assert!(!id1.is_empty());

        let id2 = get_or_create(&dir);
        assert_eq!(id1, id2, "Should return same identity on second call");

        // Cleanup
        let _ = std::fs::remove_dir_all(&dir);
    }
}
