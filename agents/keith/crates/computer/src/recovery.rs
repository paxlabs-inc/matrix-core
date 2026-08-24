use std::collections::{BTreeMap, VecDeque};
use std::sync::Mutex;

use keith_agent_types::{ProfileId, UtcTimestamp};
use serde::{Deserialize, Serialize};

use crate::model::{ComputerRecord, ComputerState};

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum RecoveryDecision {
    Noop,
    Relaunch,
    Quarantine,
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct ComputerQuarantine {
    pub owner_profile_id: ProfileId,
    pub crash_count: u32,
    pub window_seconds: u64,
    pub quarantined_at: UtcTimestamp,
    pub safe_reason: String,
}

#[derive(Debug, Default)]
pub struct ComputerCrashTracker {
    crashes: Mutex<BTreeMap<ProfileId, VecDeque<i64>>>,
}

impl ComputerCrashTracker {
    pub fn record_and_decide(
        &self,
        owner: &ProfileId,
        now: UtcTimestamp,
        crash_limit: u32,
        window_seconds: u64,
    ) -> RecoveryDecision {
        let Ok(mut crashes) = self.crashes.lock() else {
            return RecoveryDecision::Quarantine;
        };
        let history = crashes.entry(owner.clone()).or_default();
        let cutoff = now.unix_millis().saturating_sub(
            i64::try_from(window_seconds.saturating_mul(1_000)).unwrap_or(i64::MAX),
        );
        while history.front().is_some_and(|time| *time < cutoff) {
            history.pop_front();
        }
        history.push_back(now.unix_millis());
        if u32::try_from(history.len()).unwrap_or(u32::MAX) >= crash_limit {
            RecoveryDecision::Quarantine
        } else {
            RecoveryDecision::Relaunch
        }
    }

    pub fn clear(&self, owner: &ProfileId) {
        if let Ok(mut crashes) = self.crashes.lock() {
            crashes.remove(owner);
        }
    }
}

pub fn reconcile_computer(record: &ComputerRecord, process_running: bool) -> RecoveryDecision {
    match (record.state, process_running) {
        (ComputerState::Ready, false) => RecoveryDecision::Relaunch,
        (ComputerState::Ready, true)
        | (
            ComputerState::Disabled | ComputerState::Tombstoned | ComputerState::Quarantined,
            false,
        ) => RecoveryDecision::Noop,
        (
            ComputerState::Disabled | ComputerState::Tombstoned | ComputerState::Quarantined,
            true,
        ) => RecoveryDecision::Quarantine,
        (ComputerState::Provisioning, _) => RecoveryDecision::Quarantine,
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn recovery_process_crash_window_quarantines_without_affecting_other_profiles() {
        let tracker = ComputerCrashTracker::default();
        let owner = ProfileId::new();
        let other = ProfileId::new();
        assert_eq!(
            tracker.record_and_decide(&owner, UtcTimestamp::from_unix_millis(0), 2, 60),
            RecoveryDecision::Relaunch
        );
        assert_eq!(
            tracker.record_and_decide(&owner, UtcTimestamp::from_unix_millis(1), 2, 60),
            RecoveryDecision::Quarantine
        );
        assert_eq!(
            tracker.record_and_decide(&other, UtcTimestamp::from_unix_millis(1), 2, 60),
            RecoveryDecision::Relaunch
        );
    }
}
