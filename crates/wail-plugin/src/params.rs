use nih_plug::prelude::*;

/// Empty params struct — WAIL plugin has no user-controllable parameters.
#[derive(Params)]
pub struct WailParams {}

impl Default for WailParams {
    fn default() -> Self {
        Self {}
    }
}
