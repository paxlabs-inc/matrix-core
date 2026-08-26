use std::collections::BTreeSet;
use std::fs::{self, File, OpenOptions};
use std::io::Write;
use std::path::{Path, PathBuf};

use fs2::FileExt;
use keith_agent_types::{EntityId, Generation, RootTreeId, UtcTimestamp};
use keith_state_store_core::EvolutionLedgerRepository;
use keith_telemetry::{CandidateObservation, CandidateSignal, MetricName};
use serde::{Deserialize, Serialize};
use sha2::{Digest, Sha256};
use thiserror::Error;

use crate::{
    EvolutionEvent, EvolutionLedger, LedgerError, ReversalAuthority, ReversalError,
    ReversalOutcome, ReversalRequest, ReversalScope, ReversalTransaction,
};

const STATE_VERSION: u32 = 1;
const STATE_FILE: &str = "watchdog.json";
const LOCK_FILE: &str = "watchdog.lock";
const MAX_STATE_BYTES: u64 = 4 * 1024 * 1024;

#[derive(Clone, Copy, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum ThresholdDirection {
    AtMost,
    AtLeast,
}

#[derive(Clone, Debug, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct WatchdogThresholds {
    pub hypothesis_direction: ThresholdDirection,
    pub hypothesis_revert_threshold: f64,
    pub maximum_crashes: u32,
    pub maximum_turn_failure_rate: f64,
    pub maximum_mean_latency_ms: u64,
    pub maximum_total_token_cost: u64,
    pub maximum_resident_bytes: u64,
    pub maximum_virtual_bytes: u64,
    pub minimum_hypothesis_samples: u32,
    pub minimum_turn_samples: u32,
    pub minimum_resource_samples: u32,
}

impl WatchdogThresholds {
    fn validate(&self) -> bool {
        self.hypothesis_revert_threshold.is_finite()
            && self.maximum_crashes > 0
            && self.maximum_turn_failure_rate.is_finite()
            && (0.0..=1.0).contains(&self.maximum_turn_failure_rate)
            && self.minimum_hypothesis_samples > 0
            && self.minimum_turn_samples > 0
            && self.minimum_resource_samples > 0
    }
}

#[derive(Clone, Copy, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum WatchdogNotificationPolicy {
    Silent,
    NotifyOnRevert,
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct WatchdogNotification {
    pub profile_id: EntityId,
    pub hypothesis_id: EntityId,
    pub promotion_id: EntityId,
    pub reason: String,
    pub occurred_at: UtcTimestamp,
}

#[derive(Clone, Debug, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case", tag = "kind")]
pub enum ObservationSignal {
    HypothesisMetric {
        image_id: String,
        generation: Generation,
        value: f64,
    },
    WorkerExit {
        image_id: String,
        generation: Generation,
        root_tree_id: RootTreeId,
        success: Option<bool>,
    },
    Turn {
        image_id: String,
        generation: Generation,
        succeeded: bool,
        latency_ms: u64,
        token_cost: u64,
    },
    Resource {
        image_id: String,
        generation: Generation,
        resident_bytes: u64,
        virtual_bytes: u64,
    },
}

impl ObservationSignal {
    fn attribution(&self) -> (&str, Generation) {
        match self {
            Self::HypothesisMetric {
                image_id,
                generation,
                ..
            }
            | Self::WorkerExit {
                image_id,
                generation,
                ..
            }
            | Self::Turn {
                image_id,
                generation,
                ..
            }
            | Self::Resource {
                image_id,
                generation,
                ..
            } => (image_id, *generation),
        }
    }
}

#[derive(Clone, Debug, Default, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
struct ObservationTotals {
    hypothesis_samples: u32,
    hypothesis_sum: f64,
    crashes: u32,
    turns: u32,
    failed_turns: u32,
    latency_total_ms: u64,
    token_cost: u64,
    resource_samples: u32,
    maximum_resident_bytes: u64,
    maximum_virtual_bytes: u64,
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case", tag = "state", content = "reason")]
pub enum ObservationState {
    Observing,
    RevertRequired(String),
    AdvancingKnownGood,
    Reverted,
    KnownGood,
}

#[derive(Clone, Debug, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct ObservationWindow {
    pub id: EntityId,
    pub promotion_id: EntityId,
    pub hypothesis_id: EntityId,
    pub profile_id: EntityId,
    pub candidate_image_id: String,
    pub prior_known_good_image_id: String,
    pub candidate_workers: BTreeSet<(RootTreeId, Generation)>,
    pub hypothesis_metric: MetricName,
    pub started_at: UtcTimestamp,
    pub deadline: UtcTimestamp,
    pub previous_image_retain_until: UtcTimestamp,
    pub last_observed_at: UtcTimestamp,
    pub thresholds: WatchdogThresholds,
    pub notification_policy: WatchdogNotificationPolicy,
    pub state: ObservationState,
    pub revision: u64,
    totals: ObservationTotals,
}

pub struct ObservationRequest {
    pub promotion_id: EntityId,
    pub hypothesis_id: EntityId,
    pub profile_id: EntityId,
    pub candidate_image_id: String,
    pub prior_known_good_image_id: String,
    pub candidate_workers: BTreeSet<(RootTreeId, Generation)>,
    pub hypothesis_metric: MetricName,
    pub started_at: UtcTimestamp,
    pub deadline: UtcTimestamp,
    pub previous_image_retain_until: UtcTimestamp,
    pub thresholds: WatchdogThresholds,
    pub notification_policy: WatchdogNotificationPolicy,
}

#[derive(Clone, Debug, PartialEq)]
pub enum WatchdogDecision {
    IgnoredForeignGeneration,
    Observing,
    RevertRequired(String),
    AdvanceKnownGood,
    Reverted(ReversalOutcome),
    KnownGood,
}

#[derive(Clone, Debug, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
struct WatchdogState {
    version: u32,
    revision: u64,
    active: Option<ObservationWindow>,
    last_completed: Option<ObservationWindow>,
    notifications: Vec<WatchdogNotification>,
}

impl Default for WatchdogState {
    fn default() -> Self {
        Self {
            version: STATE_VERSION,
            revision: 0,
            active: None,
            last_completed: None,
            notifications: Vec::new(),
        }
    }
}

#[derive(Debug, Error)]
pub enum WatchdogError {
    #[error("watchdog configuration is invalid: {0}")]
    Invalid(String),
    #[error("an observation window is already active")]
    WindowOpen,
    #[error("no observation window is active")]
    NoWindow,
    #[error("watchdog is owned by another process")]
    Locked,
    #[error("watchdog durable state changed concurrently")]
    Conflict,
    #[error("watchdog filesystem failed: {0}")]
    Io(#[from] std::io::Error),
    #[error("watchdog durable state is corrupt: {0}")]
    State(#[from] serde_json::Error),
    #[error("automatic reversal failed: {0}")]
    Reversal(#[from] ReversalError),
    #[error("watchdog ledger append failed: {0}")]
    Ledger(#[from] LedgerError),
    #[error("known-good registry advancement failed: {0}")]
    Registry(#[from] keith_supervisor::ImageRegistryError),
}

/// Durable single-window authority for post-promotion regression containment.
pub struct RevertWatchdog {
    root: PathBuf,
    state_path: PathBuf,
    state: WatchdogState,
    _lock: File,
}

impl Drop for RevertWatchdog {
    fn drop(&mut self) {
        let _ = FileExt::unlock(&self._lock);
    }
}

impl RevertWatchdog {
    pub fn open(root: impl Into<PathBuf>) -> Result<Self, WatchdogError> {
        let root = root.into();
        fs::create_dir_all(&root)?;
        let root = fs::canonicalize(root)?;
        let lock = OpenOptions::new()
            .create(true)
            .read(true)
            .write(true)
            .truncate(false)
            .open(root.join(LOCK_FILE))?;
        lock.try_lock_exclusive()
            .map_err(|_| WatchdogError::Locked)?;
        let state_path = root.join(STATE_FILE);
        let state = load_state(&state_path)?;
        Ok(Self {
            root,
            state_path,
            state,
            _lock: lock,
        })
    }

    /// Used by promotion admission before any candidate bytes or worker generations change.
    pub fn assert_promotion_allowed(root: impl AsRef<Path>) -> Result<(), WatchdogError> {
        if load_state(&root.as_ref().join(STATE_FILE))?
            .active
            .is_some()
        {
            return Err(WatchdogError::WindowOpen);
        }
        Ok(())
    }

    pub fn active(&self) -> Option<&ObservationWindow> {
        self.state.active.as_ref()
    }

    pub fn pending_notifications(&self) -> &[WatchdogNotification] {
        &self.state.notifications
    }

    pub fn acknowledge_notifications(&mut self) -> Result<(), WatchdogError> {
        let expected = self.state.revision;
        self.state.notifications.clear();
        self.persist(expected)
    }

    pub fn start(
        &mut self,
        request: ObservationRequest,
    ) -> Result<&ObservationWindow, WatchdogError> {
        if self.state.active.is_some() {
            return Err(WatchdogError::WindowOpen);
        }
        if request.candidate_image_id.is_empty()
            || request.prior_known_good_image_id.is_empty()
            || request.candidate_image_id == request.prior_known_good_image_id
            || request.candidate_workers.is_empty()
            || request.deadline <= request.started_at
            || request.previous_image_retain_until < request.deadline
            || !request.thresholds.validate()
        {
            return Err(WatchdogError::Invalid(
                "window bounds, attribution, or thresholds are invalid".into(),
            ));
        }
        let expected = self.state.revision;
        self.state.active = Some(ObservationWindow {
            id: EntityId::new(),
            promotion_id: request.promotion_id,
            hypothesis_id: request.hypothesis_id,
            profile_id: request.profile_id,
            candidate_image_id: request.candidate_image_id,
            prior_known_good_image_id: request.prior_known_good_image_id,
            candidate_workers: request.candidate_workers,
            hypothesis_metric: request.hypothesis_metric,
            started_at: request.started_at,
            deadline: request.deadline,
            previous_image_retain_until: request.previous_image_retain_until,
            last_observed_at: request.started_at,
            thresholds: request.thresholds,
            notification_policy: request.notification_policy,
            state: ObservationState::Observing,
            revision: 1,
            totals: ObservationTotals::default(),
        });
        self.persist(expected)?;
        Ok(self.state.active.as_ref().expect("window persisted"))
    }

    pub fn observe(
        &mut self,
        signal: ObservationSignal,
        observed_at: UtcTimestamp,
    ) -> Result<WatchdogDecision, WatchdogError> {
        let expected = self.state.revision;
        let window = self.state.active.as_mut().ok_or(WatchdogError::NoWindow)?;
        if observed_at < window.last_observed_at {
            window.state = ObservationState::RevertRequired("wall clock moved backwards".into());
        } else if signal.attribution().0 != window.candidate_image_id
            || !window
                .candidate_workers
                .iter()
                .any(|(_, generation)| *generation == signal.attribution().1)
        {
            return Ok(WatchdogDecision::IgnoredForeignGeneration);
        } else if matches!(window.state, ObservationState::Observing) {
            apply_signal(window, signal)?;
            window.last_observed_at = observed_at;
            window.revision = window
                .revision
                .checked_add(1)
                .ok_or(WatchdogError::Conflict)?;
            evaluate(window, observed_at);
        }
        let decision = decision(window);
        self.persist(expected)?;
        Ok(decision)
    }

    /// Consumes one closed daemon observation after validating the exact image, root, and
    /// generation tuple captured before the worker event could be reused.
    pub fn observe_candidate(
        &mut self,
        observation: CandidateObservation,
    ) -> Result<WatchdogDecision, WatchdogError> {
        let expected = self.state.revision;
        let window = self.state.active.as_mut().ok_or(WatchdogError::NoWindow)?;
        if observation.observed_at < window.last_observed_at {
            window.state = ObservationState::RevertRequired("wall clock moved backwards".into());
        } else if observation.image_id != window.candidate_image_id
            || !window
                .candidate_workers
                .contains(&(observation.root_tree_id.clone(), observation.generation))
        {
            return Ok(WatchdogDecision::IgnoredForeignGeneration);
        } else if matches!(window.state, ObservationState::Observing) {
            apply_candidate_signal(window, observation.signal);
            window.last_observed_at = observation.observed_at;
            window.revision = window
                .revision
                .checked_add(1)
                .ok_or(WatchdogError::Conflict)?;
            evaluate(window, observation.observed_at);
        }
        let decision = decision(window);
        self.persist(expected)?;
        Ok(decision)
    }

    pub fn tick(&mut self, now: UtcTimestamp) -> Result<WatchdogDecision, WatchdogError> {
        let expected = self.state.revision;
        let window = self.state.active.as_mut().ok_or(WatchdogError::NoWindow)?;
        if now < window.last_observed_at {
            window.state = ObservationState::RevertRequired("wall clock moved backwards".into());
        } else {
            evaluate(window, now);
            window.last_observed_at = now;
        }
        let decision = decision(window);
        self.persist(expected)?;
        Ok(decision)
    }

    /// Executes a pending automatic reversal through the exact one-click reversal transaction.
    pub fn apply_revert<R, L>(
        &mut self,
        reversal: &ReversalTransaction,
        runtime: &mut R,
        ledger: &EvolutionLedger<L>,
        trusted_public_key: &[u8; 32],
        authority: &ReversalAuthority,
        occurred_at: UtcTimestamp,
    ) -> Result<WatchdogDecision, WatchdogError>
    where
        R: crate::PromotionRuntime,
        L: EvolutionLedgerRepository,
    {
        let window = self.state.active.as_ref().ok_or(WatchdogError::NoWindow)?;
        let ObservationState::RevertRequired(reason) = &window.state else {
            return Ok(decision(window));
        };
        let reason = reason.clone();
        let recovered = reversal.recover(runtime, ledger, occurred_at)?;
        let outcome = if let Some(recovered) = recovered
            && recovered.promotion_ids.contains(&window.promotion_id)
        {
            recovered
        } else {
            match reversal.reverse(
                runtime,
                ledger,
                ReversalRequest {
                    scope: ReversalScope::Promotion(window.promotion_id.clone()),
                    trusted_public_key,
                    authority,
                    reason: &reason,
                    occurred_at,
                },
            ) {
                Ok(outcome) => outcome,
                Err(ReversalError::AlreadyReversed) => ReversalOutcome {
                    transaction_id: window.id.clone(),
                    promotion_ids: vec![window.promotion_id.clone()],
                    restored_image_id: window.prior_known_good_image_id.clone(),
                    restored_paths: Vec::new(),
                    workers: Vec::new(),
                },
                Err(error) => return Err(error.into()),
            }
        };
        let measured = hypothesis_mean(&window.totals);
        ledger.append(
            stable_observation_event_id(&window.id),
            occurred_at,
            EvolutionEvent::Observation {
                hypothesis_id: window.hypothesis_id.clone(),
                before: window.thresholds.hypothesis_revert_threshold,
                after: if measured.is_finite() {
                    measured
                } else {
                    window.thresholds.hypothesis_revert_threshold
                },
                healthy: false,
            },
        )?;
        let expected = self.state.revision;
        let mut completed = self.state.active.take().ok_or(WatchdogError::NoWindow)?;
        completed.state = ObservationState::Reverted;
        completed.revision = completed.revision.saturating_add(1);
        if completed.notification_policy == WatchdogNotificationPolicy::NotifyOnRevert {
            self.state.notifications.push(WatchdogNotification {
                profile_id: completed.profile_id.clone(),
                hypothesis_id: completed.hypothesis_id.clone(),
                promotion_id: completed.promotion_id.clone(),
                reason,
                occurred_at,
            });
        }
        self.state.last_completed = Some(completed);
        self.persist(expected)?;
        Ok(WatchdogDecision::Reverted(outcome))
    }

    /// Records a clean window as the new known-good baseline. The registry's pinned prior image
    /// remains protected while `previous_image_retain_until` is current.
    pub fn advance_known_good<R, L>(
        &mut self,
        runtime: &mut R,
        ledger: &EvolutionLedger<L>,
        trusted_public_key: &[u8; 32],
        occurred_at: UtcTimestamp,
    ) -> Result<WatchdogDecision, WatchdogError>
    where
        R: crate::PromotionRuntime,
        L: EvolutionLedgerRepository,
    {
        let window = self.state.active.as_ref().ok_or(WatchdogError::NoWindow)?;
        if window.state != ObservationState::AdvancingKnownGood {
            return Ok(decision(window));
        }
        let after = hypothesis_mean(&window.totals);
        runtime.registry_mut().advance_known_good(
            &window.candidate_image_id,
            trusted_public_key,
            window.previous_image_retain_until,
        )?;
        ledger.append(
            stable_observation_event_id(&window.id),
            occurred_at,
            EvolutionEvent::Observation {
                hypothesis_id: window.hypothesis_id.clone(),
                before: window.thresholds.hypothesis_revert_threshold,
                after,
                healthy: true,
            },
        )?;
        let expected = self.state.revision;
        let mut completed = self.state.active.take().ok_or(WatchdogError::NoWindow)?;
        completed.state = ObservationState::KnownGood;
        completed.revision = completed.revision.saturating_add(1);
        self.state.last_completed = Some(completed);
        self.persist(expected)?;
        Ok(WatchdogDecision::KnownGood)
    }

    fn persist(&mut self, expected_revision: u64) -> Result<(), WatchdogError> {
        let disk = load_state(&self.state_path)?;
        if disk.revision != expected_revision {
            return Err(WatchdogError::Conflict);
        }
        self.state.revision = expected_revision
            .checked_add(1)
            .ok_or(WatchdogError::Conflict)?;
        persist_state(&self.root, &self.state_path, &self.state)
    }
}

fn apply_candidate_signal(window: &mut ObservationWindow, signal: CandidateSignal) {
    match signal {
        CandidateSignal::HypothesisMetric { metric, value } => {
            if metric == window.hypothesis_metric {
                window.totals.hypothesis_samples =
                    window.totals.hypothesis_samples.saturating_add(1);
                window.totals.hypothesis_sum += value as f64;
            }
        }
        CandidateSignal::WorkerCrash => {
            window.totals.crashes = window.totals.crashes.saturating_add(1);
        }
        CandidateSignal::TurnCompleted {
            succeeded,
            latency_ms,
            token_cost_microunits,
        } => {
            window.totals.turns = window.totals.turns.saturating_add(1);
            window.totals.failed_turns = window
                .totals
                .failed_turns
                .saturating_add(u32::from(!succeeded));
            window.totals.latency_total_ms =
                window.totals.latency_total_ms.saturating_add(latency_ms);
            window.totals.token_cost = window
                .totals
                .token_cost
                .saturating_add(token_cost_microunits);
        }
        CandidateSignal::ResourceUse {
            resident_bytes,
            virtual_bytes,
        } => {
            if let (Some(resident_bytes), Some(virtual_bytes)) = (resident_bytes, virtual_bytes) {
                window.totals.resource_samples = window.totals.resource_samples.saturating_add(1);
                window.totals.maximum_resident_bytes =
                    window.totals.maximum_resident_bytes.max(resident_bytes);
                window.totals.maximum_virtual_bytes =
                    window.totals.maximum_virtual_bytes.max(virtual_bytes);
            }
        }
    }
}

fn apply_signal(
    window: &mut ObservationWindow,
    signal: ObservationSignal,
) -> Result<(), WatchdogError> {
    match signal {
        ObservationSignal::HypothesisMetric { value, .. } => {
            if !value.is_finite() {
                window.state =
                    ObservationState::RevertRequired("hypothesis metric is inconclusive".into());
            } else {
                window.totals.hypothesis_samples =
                    window.totals.hypothesis_samples.saturating_add(1);
                window.totals.hypothesis_sum += value;
            }
        }
        ObservationSignal::WorkerExit { success, .. } => {
            if success != Some(true) {
                window.totals.crashes = window.totals.crashes.saturating_add(1);
            }
        }
        ObservationSignal::Turn {
            succeeded,
            latency_ms,
            token_cost,
            ..
        } => {
            window.totals.turns = window.totals.turns.saturating_add(1);
            window.totals.failed_turns = window
                .totals
                .failed_turns
                .saturating_add(u32::from(!succeeded));
            window.totals.latency_total_ms =
                window.totals.latency_total_ms.saturating_add(latency_ms);
            window.totals.token_cost = window.totals.token_cost.saturating_add(token_cost);
        }
        ObservationSignal::Resource {
            resident_bytes,
            virtual_bytes,
            ..
        } => {
            window.totals.resource_samples = window.totals.resource_samples.saturating_add(1);
            window.totals.maximum_resident_bytes =
                window.totals.maximum_resident_bytes.max(resident_bytes);
            window.totals.maximum_virtual_bytes =
                window.totals.maximum_virtual_bytes.max(virtual_bytes);
        }
    }
    Ok(())
}

fn evaluate(window: &mut ObservationWindow, now: UtcTimestamp) {
    if !matches!(window.state, ObservationState::Observing) {
        return;
    }
    let threshold = &window.thresholds;
    let totals = &window.totals;
    let hypothesis = hypothesis_mean(totals);
    let hypothesis_breached = totals.hypothesis_samples > 0
        && match threshold.hypothesis_direction {
            ThresholdDirection::AtMost => hypothesis > threshold.hypothesis_revert_threshold,
            ThresholdDirection::AtLeast => hypothesis < threshold.hypothesis_revert_threshold,
        };
    let failure_rate = if totals.turns == 0 {
        0.0
    } else {
        f64::from(totals.failed_turns) / f64::from(totals.turns)
    };
    let mean_latency = if totals.turns == 0 {
        0
    } else {
        totals.latency_total_ms / u64::from(totals.turns)
    };
    let reason = if hypothesis_breached {
        Some("hypothesis revert threshold breached")
    } else if totals.crashes >= threshold.maximum_crashes {
        Some("candidate worker crash loop")
    } else if failure_rate > threshold.maximum_turn_failure_rate {
        Some("turn failure rate threshold breached")
    } else if mean_latency > threshold.maximum_mean_latency_ms {
        Some("latency threshold breached")
    } else if totals.token_cost > threshold.maximum_total_token_cost {
        Some("token cost threshold breached")
    } else if totals.maximum_resident_bytes > threshold.maximum_resident_bytes
        || totals.maximum_virtual_bytes > threshold.maximum_virtual_bytes
    {
        Some("worker resource threshold breached")
    } else {
        None
    };
    if let Some(reason) = reason {
        window.state = ObservationState::RevertRequired(reason.into());
    } else if now >= window.deadline {
        if totals.hypothesis_samples < threshold.minimum_hypothesis_samples
            || totals.turns < threshold.minimum_turn_samples
            || totals.resource_samples < threshold.minimum_resource_samples
        {
            window.state =
                ObservationState::RevertRequired("observation window was inconclusive".into());
        } else {
            window.state = ObservationState::AdvancingKnownGood;
        }
    }
}

fn hypothesis_mean(totals: &ObservationTotals) -> f64 {
    if totals.hypothesis_samples == 0 {
        f64::NAN
    } else {
        totals.hypothesis_sum / f64::from(totals.hypothesis_samples)
    }
}

fn decision(window: &ObservationWindow) -> WatchdogDecision {
    match &window.state {
        ObservationState::Observing => WatchdogDecision::Observing,
        ObservationState::RevertRequired(reason) => {
            WatchdogDecision::RevertRequired(reason.clone())
        }
        ObservationState::AdvancingKnownGood => WatchdogDecision::AdvanceKnownGood,
        ObservationState::Reverted => WatchdogDecision::Reverted(ReversalOutcome {
            transaction_id: EntityId::from_u128(0),
            promotion_ids: vec![window.promotion_id.clone()],
            restored_image_id: window.prior_known_good_image_id.clone(),
            restored_paths: Vec::new(),
            workers: Vec::new(),
        }),
        ObservationState::KnownGood => WatchdogDecision::KnownGood,
    }
}

fn load_state(path: &Path) -> Result<WatchdogState, WatchdogError> {
    if !path.exists() {
        return Ok(WatchdogState::default());
    }
    let metadata = fs::symlink_metadata(path)?;
    if !metadata.is_file() || metadata.file_type().is_symlink() || metadata.len() > MAX_STATE_BYTES
    {
        return Err(WatchdogError::Invalid(
            "watchdog state is unsafe or oversized".into(),
        ));
    }
    let state: WatchdogState = serde_json::from_slice(&fs::read(path)?)?;
    if state.version != STATE_VERSION
        || state
            .active
            .as_ref()
            .is_some_and(|window| window.revision == 0)
    {
        return Err(WatchdogError::Invalid(
            "watchdog state version or revision is invalid".into(),
        ));
    }
    Ok(state)
}

fn persist_state(root: &Path, path: &Path, state: &WatchdogState) -> Result<(), WatchdogError> {
    let bytes = serde_json::to_vec(state)?;
    if u64::try_from(bytes.len()).unwrap_or(u64::MAX) > MAX_STATE_BYTES {
        return Err(WatchdogError::Invalid(
            "watchdog state exceeds its storage bound".into(),
        ));
    }
    let temporary = root.join(format!(".{STATE_FILE}.{}.tmp", std::process::id()));
    match fs::remove_file(&temporary) {
        Ok(()) => {}
        Err(error) if error.kind() == std::io::ErrorKind::NotFound => {}
        Err(error) => return Err(error.into()),
    }
    let mut file = OpenOptions::new()
        .create_new(true)
        .write(true)
        .open(&temporary)?;
    file.write_all(&bytes)?;
    file.sync_all()?;
    keith_platform::replace_file(&temporary, path)?;
    File::open(root)?.sync_all()?;
    Ok(())
}

fn stable_observation_event_id(window_id: &EntityId) -> EntityId {
    let digest = Sha256::digest(window_id.as_str().as_bytes());
    let mut value = [0_u8; 16];
    value.copy_from_slice(&digest[..16]);
    EntityId::from_u128(u128::from_be_bytes(value))
}

#[cfg(test)]
mod tests {
    use super::*;

    fn request() -> ObservationRequest {
        ObservationRequest {
            promotion_id: EntityId::from_u128(1),
            hypothesis_id: EntityId::from_u128(2),
            profile_id: EntityId::from_u128(3),
            candidate_image_id: "candidate".into(),
            prior_known_good_image_id: "known-good".into(),
            candidate_workers: BTreeSet::from([(
                RootTreeId::from(EntityId::from_u128(7)),
                Generation::new(7),
            )]),
            hypothesis_metric: MetricName::RefinementOutcomes,
            started_at: UtcTimestamp::from_unix_millis(100),
            deadline: UtcTimestamp::from_unix_millis(200),
            previous_image_retain_until: UtcTimestamp::from_unix_millis(300),
            thresholds: WatchdogThresholds {
                hypothesis_direction: ThresholdDirection::AtMost,
                hypothesis_revert_threshold: 5.0,
                maximum_crashes: 2,
                maximum_turn_failure_rate: 0.5,
                maximum_mean_latency_ms: 100,
                maximum_total_token_cost: 1000,
                maximum_resident_bytes: 1024,
                maximum_virtual_bytes: 2048,
                minimum_hypothesis_samples: 1,
                minimum_turn_samples: 1,
                minimum_resource_samples: 1,
            },
            notification_policy: WatchdogNotificationPolicy::NotifyOnRevert,
        }
    }

    #[test]
    fn single_window_is_durable_and_blocks_promotion_across_restart() {
        let directory = tempfile::tempdir().unwrap();
        let mut watchdog = RevertWatchdog::open(directory.path()).unwrap();
        watchdog.start(request()).unwrap();
        assert!(matches!(
            RevertWatchdog::assert_promotion_allowed(directory.path()),
            Err(WatchdogError::WindowOpen)
        ));
        drop(watchdog);
        let resumed = RevertWatchdog::open(directory.path()).unwrap();
        assert_eq!(
            resumed.active().unwrap().deadline,
            UtcTimestamp::from_unix_millis(200)
        );
    }

    #[test]
    fn attributed_breach_crash_loop_and_foreign_generation_are_distinguished() {
        let directory = tempfile::tempdir().unwrap();
        let mut watchdog = RevertWatchdog::open(directory.path()).unwrap();
        watchdog.start(request()).unwrap();
        let ignored = watchdog
            .observe(
                ObservationSignal::WorkerExit {
                    image_id: "known-good".into(),
                    generation: Generation::new(6),
                    root_tree_id: RootTreeId::new(),
                    success: Some(false),
                },
                UtcTimestamp::from_unix_millis(110),
            )
            .unwrap();
        assert_eq!(ignored, WatchdogDecision::IgnoredForeignGeneration);
        assert_eq!(
            watchdog
                .observe(
                    ObservationSignal::WorkerExit {
                        image_id: "candidate".into(),
                        generation: Generation::new(7),
                        root_tree_id: RootTreeId::new(),
                        success: Some(false),
                    },
                    UtcTimestamp::from_unix_millis(120)
                )
                .unwrap(),
            WatchdogDecision::Observing
        );
        assert!(matches!(
            watchdog
                .observe(
                    ObservationSignal::WorkerExit {
                        image_id: "candidate".into(),
                        generation: Generation::new(7),
                        root_tree_id: RootTreeId::new(),
                        success: None,
                    },
                    UtcTimestamp::from_unix_millis(130)
                )
                .unwrap(),
            WatchdogDecision::RevertRequired(_)
        ));
    }

    #[test]
    fn restart_resume_inconclusive_clock_rollback_and_clean_advancement_are_fail_closed() {
        let directory = tempfile::tempdir().unwrap();
        {
            let mut watchdog = RevertWatchdog::open(directory.path()).unwrap();
            watchdog.start(request()).unwrap();
            watchdog
                .observe(
                    ObservationSignal::HypothesisMetric {
                        image_id: "candidate".into(),
                        generation: Generation::new(7),
                        value: 4.0,
                    },
                    UtcTimestamp::from_unix_millis(110),
                )
                .unwrap();
        }
        let mut resumed = RevertWatchdog::open(directory.path()).unwrap();
        assert!(
            matches!(resumed.tick(UtcTimestamp::from_unix_millis(200)).unwrap(), WatchdogDecision::RevertRequired(reason) if reason.contains("inconclusive"))
        );

        let second = tempfile::tempdir().unwrap();
        let mut watchdog = RevertWatchdog::open(second.path()).unwrap();
        watchdog.start(request()).unwrap();
        watchdog
            .observe(
                ObservationSignal::HypothesisMetric {
                    image_id: "candidate".into(),
                    generation: Generation::new(7),
                    value: 4.0,
                },
                UtcTimestamp::from_unix_millis(120),
            )
            .unwrap();
        assert!(
            matches!(watchdog.tick(UtcTimestamp::from_unix_millis(119)).unwrap(), WatchdogDecision::RevertRequired(reason) if reason.contains("backwards"))
        );

        let third = tempfile::tempdir().unwrap();
        let mut watchdog = RevertWatchdog::open(third.path()).unwrap();
        watchdog.start(request()).unwrap();
        watchdog
            .observe(
                ObservationSignal::HypothesisMetric {
                    image_id: "candidate".into(),
                    generation: Generation::new(7),
                    value: 4.0,
                },
                UtcTimestamp::from_unix_millis(110),
            )
            .unwrap();
        watchdog
            .observe(
                ObservationSignal::Turn {
                    image_id: "candidate".into(),
                    generation: Generation::new(7),
                    succeeded: true,
                    latency_ms: 50,
                    token_cost: 10,
                },
                UtcTimestamp::from_unix_millis(120),
            )
            .unwrap();
        watchdog
            .observe(
                ObservationSignal::Resource {
                    image_id: "candidate".into(),
                    generation: Generation::new(7),
                    resident_bytes: 512,
                    virtual_bytes: 1024,
                },
                UtcTimestamp::from_unix_millis(130),
            )
            .unwrap();
        assert_eq!(
            watchdog.tick(UtcTimestamp::from_unix_millis(200)).unwrap(),
            WatchdogDecision::AdvanceKnownGood
        );
    }
}
