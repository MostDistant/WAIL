use std::time::Duration;

use anyhow::{bail, Result};

#[derive(Debug, Clone, PartialEq)]
pub enum ChaosAction {
    /// Wait for N complete intervals of stable audio exchange.
    Stable(u32),
    /// Disconnect from signaling + WebRTC entirely, wait duration.
    Leave(Duration),
    /// Reconnect to the same room.
    Rejoin,
    /// Stop Link transport (like DAW hitting stop), wait duration.
    TransportStop(Duration),
    /// Resume Link transport (like DAW hitting play).
    Resume,
}

/// Parse a chaos script string into a list of actions.
///
/// Format: comma-separated entries:
/// - `stable:N` — wait for N intervals
/// - `leave:Ns` — disconnect for N seconds
/// - `rejoin` — reconnect (must follow a `leave`)
/// - `transport-stop:Ns` — stop transport for N seconds
/// - `resume` — restart transport (must follow a `transport-stop`)
pub fn parse_chaos_script(script: &str) -> Result<Vec<ChaosAction>> {
    let trimmed = script.trim();
    if trimmed.is_empty() {
        bail!("chaos script is empty");
    }

    let mut actions = Vec::new();
    for entry in trimmed.split(',') {
        let entry = entry.trim();
        if entry.is_empty() {
            continue;
        }

        let action = if let Some(rest) = entry.strip_prefix("stable:") {
            let n: u32 = rest
                .trim()
                .parse()
                .map_err(|_| anyhow::anyhow!("invalid interval count in {entry:?}"))?;
            ChaosAction::Stable(n)
        } else if let Some(rest) = entry.strip_prefix("leave:") {
            let dur = parse_duration_secs(rest.trim(), entry)?;
            ChaosAction::Leave(dur)
        } else if entry == "rejoin" {
            ChaosAction::Rejoin
        } else if let Some(rest) = entry.strip_prefix("transport-stop:") {
            let dur = parse_duration_secs(rest.trim(), entry)?;
            ChaosAction::TransportStop(dur)
        } else if entry == "resume" {
            ChaosAction::Resume
        } else {
            bail!("unknown chaos action: {entry:?}");
        };

        actions.push(action);
    }

    if actions.is_empty() {
        bail!("chaos script has no actions");
    }

    // Validate sequencing constraints.
    for (i, action) in actions.iter().enumerate() {
        match action {
            ChaosAction::Rejoin => {
                let has_prior_leave = actions[..i].iter().rev().any(|a| matches!(a, ChaosAction::Leave(_)));
                if !has_prior_leave {
                    bail!("`rejoin` at position {i} must be preceded by a `leave`");
                }
            }
            ChaosAction::Resume => {
                let has_prior_stop = actions[..i].iter().rev().any(|a| matches!(a, ChaosAction::TransportStop(_)));
                if !has_prior_stop {
                    bail!("`resume` at position {i} must be preceded by a `transport-stop`");
                }
            }
            _ => {}
        }
    }

    Ok(actions)
}

fn parse_duration_secs(s: &str, entry: &str) -> Result<Duration> {
    let s = s.trim();
    if let Some(num_str) = s.strip_suffix('s') {
        let secs: u64 = num_str
            .trim()
            .parse()
            .map_err(|_| anyhow::anyhow!("invalid duration in {entry:?}"))?;
        Ok(Duration::from_secs(secs))
    } else {
        bail!("duration must end with 's' (seconds) in {entry:?}");
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn test_parse_simple_stable() {
        let actions = parse_chaos_script("stable:4").unwrap();
        assert_eq!(actions, vec![ChaosAction::Stable(4)]);
    }

    #[test]
    fn test_parse_full_script() {
        let actions = parse_chaos_script(
            "stable:4,leave:5s,rejoin,stable:4,transport-stop:3s,resume,stable:2",
        )
        .unwrap();
        assert_eq!(actions.len(), 7);
        assert_eq!(actions[0], ChaosAction::Stable(4));
        assert_eq!(actions[1], ChaosAction::Leave(Duration::from_secs(5)));
        assert_eq!(actions[2], ChaosAction::Rejoin);
        assert_eq!(actions[3], ChaosAction::Stable(4));
        assert_eq!(
            actions[4],
            ChaosAction::TransportStop(Duration::from_secs(3))
        );
        assert_eq!(actions[5], ChaosAction::Resume);
        assert_eq!(actions[6], ChaosAction::Stable(2));
    }

    #[test]
    fn test_parse_leave_duration() {
        let actions = parse_chaos_script("leave:10s").unwrap();
        assert_eq!(actions, vec![ChaosAction::Leave(Duration::from_secs(10))]);
    }

    #[test]
    fn test_parse_error_empty() {
        assert!(parse_chaos_script("").is_err());
    }

    #[test]
    fn test_parse_error_rejoin_without_leave() {
        assert!(parse_chaos_script("rejoin").is_err());
    }

    #[test]
    fn test_parse_error_resume_without_stop() {
        assert!(parse_chaos_script("resume").is_err());
    }

    #[test]
    fn test_parse_error_bad_duration() {
        assert!(parse_chaos_script("leave:abc").is_err());
    }
}
