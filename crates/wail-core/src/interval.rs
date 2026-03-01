/// NINJAM-style interval tracking on top of the Ableton Link beat grid.
///
/// An interval is N bars of a given quantum (time signature numerator).
/// For example, 4 bars of quantum 4 (4/4) = 16 beats per interval.
pub struct IntervalTracker {
    bars: u32,
    quantum: f64,
    last_interval_index: Option<i64>,
}

impl IntervalTracker {
    pub fn new(bars: u32, quantum: f64) -> Self {
        Self {
            bars: bars.max(1),
            quantum: quantum.max(f64::EPSILON),
            last_interval_index: None,
        }
    }

    /// Beats per interval (bars * quantum).
    pub fn beats_per_interval(&self) -> f64 {
        self.bars as f64 * self.quantum
    }

    /// Current interval index for a given beat position.
    pub fn interval_index(&self, beat: f64) -> i64 {
        (beat / self.beats_per_interval()).floor() as i64
    }

    /// Call this with the current beat position. Returns `Some(interval_index)`
    /// if we just crossed an interval boundary.
    ///
    /// Indices are monotonically increasing: if the locally-computed index is
    /// *behind* the last known value (e.g. after `sync_to` adopted a remote
    /// index that is ahead of the local beat clock), the call is silently
    /// suppressed until the beat clock catches up.
    pub fn update(&mut self, beat: f64) -> Option<i64> {
        let idx = self.interval_index(beat);
        match self.last_interval_index {
            Some(last) if idx > last => {
                self.last_interval_index = Some(idx);
                Some(idx)
            }
            None => {
                self.last_interval_index = Some(idx);
                // First update — report the initial interval
                Some(idx)
            }
            _ => None,
        }
    }

    pub fn bars(&self) -> u32 {
        self.bars
    }

    pub fn quantum(&self) -> f64 {
        self.quantum
    }

    pub fn set_config(&mut self, bars: u32, quantum: f64) {
        self.bars = bars.max(1);
        self.quantum = quantum.max(f64::EPSILON);
        self.last_interval_index = None; // reset
    }

    /// Adopt a remote interval index (NINJAM-style ground-truth sync).
    /// After calling this, `update()` will not fire a boundary for beat
    /// positions that belong to `index`.
    pub fn sync_to(&mut self, index: i64) {
        self.last_interval_index = Some(index);
    }

    /// The most recently seen interval index, or `None` before the first
    /// call to `update()` or `sync_to()`.
    pub fn current_index(&self) -> Option<i64> {
        self.last_interval_index
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn current_index_none_before_first_update() {
        let tracker = IntervalTracker::new(4, 4.0);
        assert_eq!(tracker.current_index(), None);
    }

    #[test]
    fn sync_to_adopts_remote_index() {
        let mut tracker = IntervalTracker::new(4, 4.0);
        tracker.sync_to(99);
        assert_eq!(tracker.current_index(), Some(99));
    }

    #[test]
    fn sync_to_does_not_fire_boundary() {
        // beats_per_interval = 4 bars * 4.0 quantum = 16 beats
        // interval 5 spans beats [80, 96)
        let mut tracker = IntervalTracker::new(4, 4.0);
        tracker.sync_to(5);
        // A beat squarely in interval 5 should not fire a boundary
        let result = tracker.update(82.0);
        assert_eq!(result, None, "expected no boundary after sync_to same interval");
    }

    #[test]
    fn update_behind_synced_index_does_not_flap() {
        // Reproduces the flapping bug: local beat gives index 0 but remote
        // synced us to index 2. update() with a beat in interval 0 must NOT
        // fire — it's behind, not ahead.
        // beats_per_interval = 4 * 4 = 16; interval 0 = beats [0, 16)
        let mut tracker = IntervalTracker::new(4, 4.0);
        tracker.sync_to(2);
        // Beats 12, 13, 14, 15 all compute to interval 0, which is < 2
        for beat in [12.0, 12.5, 13.0, 14.0, 15.0] {
            assert_eq!(
                tracker.update(beat),
                None,
                "beat {beat} is behind synced index 2 — must not fire"
            );
            // Must not undo the sync
            assert_eq!(tracker.current_index(), Some(2));
        }
    }

    #[test]
    fn update_fires_when_beat_clock_catches_up() {
        // After sync_to(2) with a behind beat clock, a boundary fires once
        // the beat clock advances into interval 3.
        let mut tracker = IntervalTracker::new(4, 4.0);
        tracker.sync_to(2);
        // Still in interval 0 territory — suppressed
        assert_eq!(tracker.update(12.0), None);
        // Beat advances into interval 3 (beat 48+)
        assert_eq!(tracker.update(50.0), Some(3));
    }
}
