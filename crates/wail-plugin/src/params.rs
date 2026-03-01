use nih_plug::prelude::*;

/// Plugin parameters exposed to the DAW.
#[derive(Params)]
pub struct WailParams {
    /// Bars per interval (NINJAM-style)
    #[id = "bars"]
    pub bars: IntParam,

    /// Time signature numerator (quantum / beats per bar)
    #[id = "timesig_num"]
    pub time_sig_numerator: IntParam,

    /// Send audio to remote peers
    #[id = "send"]
    pub send_enabled: BoolParam,

    /// Receive audio from remote peers
    #[id = "receive"]
    pub receive_enabled: BoolParam,

    /// Output volume for received audio (0.0 to 1.0)
    #[id = "volume"]
    pub volume: FloatParam,

    /// Opus bitrate in kbps
    #[id = "bitrate"]
    pub bitrate_kbps: IntParam,
}

impl Default for WailParams {
    fn default() -> Self {
        Self {
            bars: IntParam::new("Bars", 4, IntRange::Linear { min: 1, max: 16 }),

            time_sig_numerator: IntParam::new(
                "Time Sig",
                4,
                IntRange::Linear { min: 1, max: 12 },
            ),

            send_enabled: BoolParam::new("Send", true),

            receive_enabled: BoolParam::new("Receive", true),

            volume: FloatParam::new(
                "Volume",
                0.8,
                FloatRange::Linear { min: 0.0, max: 1.0 },
            )
            .with_value_to_string(formatters::v2s_f32_rounded(2)),

            bitrate_kbps: IntParam::new(
                "Bitrate",
                128,
                IntRange::Linear { min: 32, max: 320 },
            )
            .with_unit(" kbps"),
        }
    }
}

impl WailParams {
    /// Get quantum (beats per bar) from the time signature numerator.
    pub fn quantum(&self) -> f64 {
        self.time_sig_numerator.value() as f64
    }
}
