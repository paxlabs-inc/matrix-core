#![allow(clippy::missing_errors_doc)]

use std::fs::{self, File, OpenOptions};
use std::io::Write;
use std::path::{Path, PathBuf};
use std::sync::{Mutex, MutexGuard};

use fs2::FileExt;
use keith_agent_types::{EntityId, UtcTimestamp};
use keith_platform_contracts::RedactedText;
use serde::{Deserialize, Serialize};
use thiserror::Error;

use crate::{
    CandidateDisposition, CandidatePopulation, EvaluationMeasurements, EvaluationSlice,
    HarnessCandidate, HarnessCandidateId, HarnessExperimentId,
};

const STATE_FILE: &str = "promotion-state.json";
const LOCK_FILE: &str = "promotion-state.lock";
const STATE_VERSION: u16 = 1;
const MAX_STATE_BYTES: u64 = 8 * 1_024 * 1_024;
const MAX_OPERATIONS: usize = 2_048;

#[derive(Clone, Copy, Debug, Eq, Hash, Ord, PartialEq, PartialOrd, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum HarnessOperationMode {
    Advisory,
    Shadow,
    Autonomous,
}

#[allow(clippy::struct_excessive_bools)]
#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct HarnessModeAvailability {
    pub advisory: bool,
    pub shadow: bool,
    pub autonomous: bool,
    pub shadow_unavailable_reason: Option<RedactedText>,
    pub autonomous_unavailable_reason: Option<RedactedText>,
}

impl HarnessModeAvailability {
    #[must_use]
    pub const fn fully_available() -> Self {
        Self {
            advisory: true,
            shadow: true,
            autonomous: true,
            shadow_unavailable_reason: None,
            autonomous_unavailable_reason: None,
        }
    }

    /// Creates an availability projection that always leaves read-only advice available.
    ///
    /// # Errors
    ///
    /// Returns an error if a supplied reason is unsafe or if autonomous execution is offered
    /// without the shadow execution it depends on.
    pub fn new(
        shadow_unavailable_reason: Option<String>,
        autonomous_unavailable_reason: Option<String>,
    ) -> Result<Self, HarnessPromotionError> {
        let shadow_reason = shadow_unavailable_reason
            .map(RedactedText::parse)
            .transpose()
            .map_err(|_| HarnessPromotionError::UnsafeText)?;
        let autonomous_reason = autonomous_unavailable_reason
            .map(RedactedText::parse)
            .transpose()
            .map_err(|_| HarnessPromotionError::UnsafeText)?;
        let shadow = shadow_reason.is_none();
        let autonomous = shadow && autonomous_reason.is_none();
        Ok(Self {
            advisory: true,
            shadow,
            autonomous,
            shadow_unavailable_reason: shadow_reason,
            autonomous_unavailable_reason: autonomous_reason,
        })
    }

    #[must_use]
    pub const fn supports(&self, mode: HarnessOperationMode) -> bool {
        match mode {
            HarnessOperationMode::Advisory => self.advisory,
            HarnessOperationMode::Shadow => self.shadow,
            HarnessOperationMode::Autonomous => self.autonomous,
        }
    }
}

#[derive(Clone, Copy, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum HarnessRepairPhase {
    Admitted,
    AwaitingApproval,
    Approved,
    Building,
    BuildFailed,
    Built,
    CanaryRunning,
    CanaryRejected,
    CanaryPassed,
    Promoting,
    Observing,
    Promoted,
    ReversalRequired,
    Reversing,
    Reverted,
    Failed,
}

impl HarnessRepairPhase {
    const fn terminal(self) -> bool {
        matches!(
            self,
            Self::BuildFailed | Self::CanaryRejected | Self::Reverted | Self::Failed
        )
    }
}

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum HarnessRecoveryAction {
    None,
    ResumeBuild,
    ResumeCanary,
    RecoverPromotion,
    ResumeObservation,
    RecoverReversal,
}

#[derive(Clone, Copy, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum HarnessRetryAuthority {
    Denied,
    ExplicitApproval,
    PromotedCandidate,
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct HarnessApproval {
    pub acting_identity: RedactedText,
    pub approved_at: UtcTimestamp,
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct HarnessRepairMetrics {
    pub cases: u32,
    pub task_success_basis_points: u16,
    pub truthful_completion_basis_points: u16,
    pub safety_basis_points: u16,
    pub correction_adherence_basis_points: u16,
    pub tokens: u64,
    pub external_cost_micros: u64,
    pub latency_ms: u64,
    pub retries: u32,
    pub cpu_ms: u64,
    pub peak_memory_bytes: u64,
    pub disk_bytes: u64,
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct HarnessRepairOperation {
    pub id: EntityId,
    pub diagnosis_id: EntityId,
    pub population_id: HarnessExperimentId,
    pub candidate_id: HarnessCandidateId,
    pub candidate_digest: String,
    pub hypothesis: RedactedText,
    pub mode: HarnessOperationMode,
    pub phase: HarnessRepairPhase,
    pub metrics: HarnessRepairMetrics,
    pub approval: Option<HarnessApproval>,
    pub build_image_id: Option<String>,
    pub canary_passed: bool,
    pub promotion_id: Option<EntityId>,
    pub prior_known_good_image_id: Option<String>,
    pub safe_status: RedactedText,
    pub created_at: UtcTimestamp,
    pub updated_at: UtcTimestamp,
    pub revision: u64,
}

impl HarnessRepairOperation {
    #[must_use]
    pub const fn recovery_action(&self) -> HarnessRecoveryAction {
        match self.phase {
            HarnessRepairPhase::Building => HarnessRecoveryAction::ResumeBuild,
            HarnessRepairPhase::CanaryRunning => HarnessRecoveryAction::ResumeCanary,
            HarnessRepairPhase::Promoting => HarnessRecoveryAction::RecoverPromotion,
            HarnessRepairPhase::Observing | HarnessRepairPhase::ReversalRequired => {
                HarnessRecoveryAction::ResumeObservation
            }
            HarnessRepairPhase::Reversing => HarnessRecoveryAction::RecoverReversal,
            _ => HarnessRecoveryAction::None,
        }
    }

    #[must_use]
    pub const fn retry_authority(&self) -> HarnessRetryAuthority {
        match (self.phase, self.approval.is_some()) {
            (HarnessRepairPhase::Promoted, _) => HarnessRetryAuthority::PromotedCandidate,
            (_, true) => HarnessRetryAuthority::ExplicitApproval,
            _ => HarnessRetryAuthority::Denied,
        }
    }

    #[must_use]
    pub const fn promotion_allowed(&self) -> bool {
        self.canary_passed
            && (matches!(self.mode, HarnessOperationMode::Autonomous) || self.approval.is_some())
    }
}

#[allow(clippy::struct_excessive_bools)]
#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct HarnessRepairProjection {
    pub id: EntityId,
    pub candidate_id: HarnessCandidateId,
    pub mode: HarnessOperationMode,
    pub phase: HarnessRepairPhase,
    pub headline: String,
    pub summary: String,
    pub metrics: HarnessRepairMetrics,
    pub needs_approval: bool,
    pub can_retry_current_task: bool,
    pub can_promote: bool,
    pub can_reverse: bool,
    pub created_at: UtcTimestamp,
    pub updated_at: UtcTimestamp,
}

impl From<&HarnessRepairOperation> for HarnessRepairProjection {
    fn from(operation: &HarnessRepairOperation) -> Self {
        Self {
            id: operation.id.clone(),
            candidate_id: operation.candidate_id.clone(),
            mode: operation.mode,
            phase: operation.phase,
            headline: projection_headline(operation.phase).into(),
            summary: operation.safe_status.as_str().into(),
            metrics: operation.metrics.clone(),
            needs_approval: operation.approval.is_none()
                && matches!(
                    operation.phase,
                    HarnessRepairPhase::AwaitingApproval | HarnessRepairPhase::CanaryPassed
                )
                && operation.mode != HarnessOperationMode::Autonomous,
            can_retry_current_task: operation.retry_authority() != HarnessRetryAuthority::Denied,
            can_promote: operation.promotion_allowed()
                && operation.phase == HarnessRepairPhase::CanaryPassed,
            can_reverse: matches!(
                operation.phase,
                HarnessRepairPhase::Promoted | HarnessRepairPhase::ReversalRequired
            ),
            created_at: operation.created_at,
            updated_at: operation.updated_at,
        }
    }
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
struct PromotionState {
    version: u16,
    availability: HarnessModeAvailability,
    operations: Vec<HarnessRepairOperation>,
}

#[derive(Debug, Error)]
pub enum HarnessPromotionError {
    #[error("meta-harness promotion storage is invalid")]
    InvalidStorage,
    #[error("meta-harness promotion state is corrupt: {0}")]
    State(#[from] serde_json::Error),
    #[error("meta-harness promotion filesystem failed: {0}")]
    Io(#[from] std::io::Error),
    #[error("another meta-harness promotion coordinator owns this installation")]
    Locked,
    #[error("the requested meta-harness mode is unavailable")]
    ModeUnavailable,
    #[error("the candidate is not an admitted Pareto-frontier repair")]
    CandidateNotAdmitted,
    #[error("the repair operation was not found")]
    Missing,
    #[error("the repair transition is invalid")]
    InvalidTransition,
    #[error("operator-visible text is unsafe")]
    UnsafeText,
    #[error("meta-harness promotion history reached its durable bound")]
    HistoryLimit,
}

/// Durable, content-safe authority for candidate admission and promotion lifecycle state.
pub struct HarnessPromotionRegistry {
    root: PathBuf,
    state_path: PathBuf,
    state: Mutex<PromotionState>,
    _lock: File,
}

impl HarnessPromotionRegistry {
    /// Opens the durable registry without changing an interrupted phase.
    ///
    /// # Errors
    ///
    /// Returns an error for unsafe storage, corrupt state, or a competing coordinator.
    pub fn open(
        root: impl AsRef<Path>,
        availability: HarnessModeAvailability,
    ) -> Result<Self, HarnessPromotionError> {
        let root = initialize_root(root.as_ref())?;
        let lock = OpenOptions::new()
            .create(true)
            .read(true)
            .write(true)
            .truncate(false)
            .open(root.join(LOCK_FILE))?;
        lock.try_lock_exclusive()
            .map_err(|_| HarnessPromotionError::Locked)?;
        let state_path = root.join(STATE_FILE);
        let state = if state_path.exists() {
            let state = read_state(&state_path)?;
            validate_state(&state)?;
            state
        } else {
            let state = PromotionState {
                version: STATE_VERSION,
                availability,
                operations: Vec::new(),
            };
            persist_state(&root, &state_path, &state)?;
            state
        };
        Ok(Self {
            root,
            state_path,
            state: Mutex::new(state),
            _lock: lock,
        })
    }

    /// Returns the persisted availability rather than reinterpreting local capabilities.
    ///
    /// # Errors
    ///
    /// Returns an error if the in-process state lock is poisoned.
    pub fn availability(&self) -> Result<HarnessModeAvailability, HarnessPromotionError> {
        Ok(self.lock_state()?.availability.clone())
    }

    /// Admits only an evaluated, safe Pareto-frontier candidate.
    ///
    /// # Errors
    ///
    /// Returns an error for an unavailable mode, incomplete evaluation, or exhausted history.
    pub fn admit(
        &self,
        population: &CandidatePopulation,
        candidate_id: &HarnessCandidateId,
        mode: HarnessOperationMode,
        now: UtcTimestamp,
    ) -> Result<HarnessRepairOperation, HarnessPromotionError> {
        let mut state = self.lock_state()?;
        if !state.availability.supports(mode) {
            return Err(HarnessPromotionError::ModeUnavailable);
        }
        if state.operations.len() >= MAX_OPERATIONS {
            return Err(HarnessPromotionError::HistoryLimit);
        }
        let candidate = admitted_candidate(population, candidate_id)?;
        if let Some(existing) = state.operations.iter().find(|operation| {
            operation.population_id == population.id && operation.candidate_id == *candidate_id
        }) {
            return Ok(existing.clone());
        }
        let phase = if mode == HarnessOperationMode::Advisory {
            HarnessRepairPhase::AwaitingApproval
        } else {
            HarnessRepairPhase::Admitted
        };
        let operation = HarnessRepairOperation {
            id: EntityId::new(),
            diagnosis_id: population.diagnosis_id.clone(),
            population_id: population.id.clone(),
            candidate_id: candidate.id.clone(),
            candidate_digest: candidate.candidate_digest.clone(),
            hypothesis: candidate.hypothesis.clone(),
            mode,
            phase,
            metrics: repair_metrics(candidate)?,
            approval: None,
            build_image_id: None,
            canary_passed: false,
            promotion_id: None,
            prior_known_good_image_id: None,
            safe_status: safe_text(match mode {
                HarnessOperationMode::Advisory => {
                    "A repair passed isolated evaluation and is waiting for approval."
                }
                HarnessOperationMode::Shadow => {
                    "A repair passed isolated evaluation and can enter shadow verification."
                }
                HarnessOperationMode::Autonomous => {
                    "A repair passed isolated evaluation and can enter guarded promotion."
                }
            })?,
            created_at: now,
            updated_at: now,
            revision: 1,
        };
        state.operations.push(operation.clone());
        self.persist_locked(&state)?;
        Ok(operation)
    }

    /// Records explicit operator approval and unlocks retry authority for this exact candidate.
    ///
    /// # Errors
    ///
    /// Returns an error for unsafe identity text or a terminal operation.
    pub fn approve(
        &self,
        id: &EntityId,
        acting_identity: String,
        now: UtcTimestamp,
    ) -> Result<HarnessRepairOperation, HarnessPromotionError> {
        let identity =
            RedactedText::parse(acting_identity).map_err(|_| HarnessPromotionError::UnsafeText)?;
        self.update(id, now, |operation| {
            if operation.phase.terminal() || operation.promotion_id.is_some() {
                return Err(HarnessPromotionError::InvalidTransition);
            }
            operation.approval = Some(HarnessApproval {
                acting_identity: identity,
                approved_at: now,
            });
            if matches!(
                operation.phase,
                HarnessRepairPhase::Admitted | HarnessRepairPhase::AwaitingApproval
            ) {
                operation.phase = HarnessRepairPhase::Approved;
            }
            operation.safe_status = safe_text(
                "The operator approved this exact repair for guarded execution and task retry.",
            )?;
            Ok(())
        })
    }

    pub fn begin_build(
        &self,
        id: &EntityId,
        now: UtcTimestamp,
    ) -> Result<HarnessRepairOperation, HarnessPromotionError> {
        self.update(id, now, |operation| {
            if !matches!(
                operation.phase,
                HarnessRepairPhase::Admitted | HarnessRepairPhase::Approved
            ) || (operation.mode == HarnessOperationMode::Advisory
                && operation.approval.is_none())
            {
                return Err(HarnessPromotionError::InvalidTransition);
            }
            operation.phase = HarnessRepairPhase::Building;
            operation.safe_status =
                safe_text("The repair is running the full isolated build gate.")?;
            Ok(())
        })
    }

    pub fn record_build(
        &self,
        id: &EntityId,
        image_id: Option<String>,
        safe_failure: Option<String>,
        now: UtcTimestamp,
    ) -> Result<HarnessRepairOperation, HarnessPromotionError> {
        let failure = safe_failure
            .map(RedactedText::parse)
            .transpose()
            .map_err(|_| HarnessPromotionError::UnsafeText)?;
        self.update(id, now, |operation| {
            if operation.phase != HarnessRepairPhase::Building {
                return Err(HarnessPromotionError::InvalidTransition);
            }
            if let Some(image_id) = image_id {
                validate_image_id(&image_id)?;
                operation.build_image_id = Some(image_id);
                operation.phase = HarnessRepairPhase::Built;
                operation.safe_status = safe_text(
                    "The real build and security gates passed and produced a signed worker image.",
                )?;
            } else {
                operation.phase = HarnessRepairPhase::BuildFailed;
                operation.safe_status = failure.unwrap_or(safe_text(
                    "The real build gate rejected the repair. Nothing was promoted.",
                )?);
            }
            Ok(())
        })
    }

    pub fn begin_canary(
        &self,
        id: &EntityId,
        now: UtcTimestamp,
    ) -> Result<HarnessRepairOperation, HarnessPromotionError> {
        self.transition(
            id,
            HarnessRepairPhase::Built,
            HarnessRepairPhase::CanaryRunning,
            "The signed repair is running in an isolated canary worker.",
            now,
        )
    }

    pub fn record_canary(
        &self,
        id: &EntityId,
        passed: bool,
        safe_failure: Option<String>,
        now: UtcTimestamp,
    ) -> Result<HarnessRepairOperation, HarnessPromotionError> {
        let failure = safe_failure
            .map(RedactedText::parse)
            .transpose()
            .map_err(|_| HarnessPromotionError::UnsafeText)?;
        self.update(id, now, |operation| {
            if operation.phase != HarnessRepairPhase::CanaryRunning {
                return Err(HarnessPromotionError::InvalidTransition);
            }
            operation.canary_passed = passed;
            if passed {
                operation.phase = HarnessRepairPhase::CanaryPassed;
                operation.safe_status = if operation.promotion_allowed() {
                    safe_text("The canary passed and the repair is ready for guarded promotion.")?
                } else {
                    safe_text("The canary passed. Live promotion is waiting for approval.")?
                };
            } else {
                operation.phase = HarnessRepairPhase::CanaryRejected;
                operation.safe_status = failure.unwrap_or(safe_text(
                    "The canary rejected the repair. Existing workers were unchanged.",
                )?);
            }
            Ok(())
        })
    }

    pub fn begin_promotion(
        &self,
        id: &EntityId,
        now: UtcTimestamp,
    ) -> Result<HarnessRepairOperation, HarnessPromotionError> {
        self.update(id, now, |operation| {
            if operation.phase != HarnessRepairPhase::CanaryPassed || !operation.promotion_allowed()
            {
                return Err(HarnessPromotionError::InvalidTransition);
            }
            operation.phase = HarnessRepairPhase::Promoting;
            operation.safe_status =
                safe_text("The repair is rolling through the durable promotion transaction.")?;
            Ok(())
        })
    }

    pub fn record_observing(
        &self,
        id: &EntityId,
        promotion_id: EntityId,
        prior_known_good_image_id: String,
        now: UtcTimestamp,
    ) -> Result<HarnessRepairOperation, HarnessPromotionError> {
        validate_installed_image_id(&prior_known_good_image_id)?;
        self.update(id, now, |operation| {
            if operation.phase != HarnessRepairPhase::Promoting {
                return Err(HarnessPromotionError::InvalidTransition);
            }
            operation.phase = HarnessRepairPhase::Observing;
            operation.promotion_id = Some(promotion_id);
            operation.prior_known_good_image_id = Some(prior_known_good_image_id);
            operation.safe_status = safe_text(
                "The repair is live under observation. The previous known-good image is retained.",
            )?;
            Ok(())
        })
    }

    pub fn record_known_good(
        &self,
        id: &EntityId,
        now: UtcTimestamp,
    ) -> Result<HarnessRepairOperation, HarnessPromotionError> {
        self.transition(
            id,
            HarnessRepairPhase::Observing,
            HarnessRepairPhase::Promoted,
            "The observation window passed and this repair is now the known-good version.",
            now,
        )
    }

    pub fn require_reversal(
        &self,
        id: &EntityId,
        safe_reason: String,
        now: UtcTimestamp,
    ) -> Result<HarnessRepairOperation, HarnessPromotionError> {
        let reason =
            RedactedText::parse(safe_reason).map_err(|_| HarnessPromotionError::UnsafeText)?;
        self.update(id, now, |operation| {
            if !matches!(
                operation.phase,
                HarnessRepairPhase::Observing | HarnessRepairPhase::Promoted
            ) {
                return Err(HarnessPromotionError::InvalidTransition);
            }
            operation.phase = HarnessRepairPhase::ReversalRequired;
            operation.safe_status = reason;
            Ok(())
        })
    }

    pub fn begin_reversal(
        &self,
        id: &EntityId,
        now: UtcTimestamp,
    ) -> Result<HarnessRepairOperation, HarnessPromotionError> {
        self.update(id, now, |operation| {
            if !matches!(
                operation.phase,
                HarnessRepairPhase::Promoted | HarnessRepairPhase::ReversalRequired
            ) {
                return Err(HarnessPromotionError::InvalidTransition);
            }
            operation.phase = HarnessRepairPhase::Reversing;
            operation.safe_status = safe_text(
                "Keith is restoring the retained known-good image and exact source preimages.",
            )?;
            Ok(())
        })
    }

    pub fn record_reverted(
        &self,
        id: &EntityId,
        now: UtcTimestamp,
    ) -> Result<HarnessRepairOperation, HarnessPromotionError> {
        self.transition(
            id,
            HarnessRepairPhase::Reversing,
            HarnessRepairPhase::Reverted,
            "The prior known-good image and source were restored.",
            now,
        )
    }

    pub fn record_failed(
        &self,
        id: &EntityId,
        safe_reason: String,
        now: UtcTimestamp,
    ) -> Result<HarnessRepairOperation, HarnessPromotionError> {
        let reason =
            RedactedText::parse(safe_reason).map_err(|_| HarnessPromotionError::UnsafeText)?;
        self.update(id, now, |operation| {
            if operation.phase.terminal()
                || matches!(
                    operation.phase,
                    HarnessRepairPhase::Observing | HarnessRepairPhase::Promoted
                )
            {
                return Err(HarnessPromotionError::InvalidTransition);
            }
            operation.phase = HarnessRepairPhase::Failed;
            operation.safe_status = reason;
            Ok(())
        })
    }

    /// Returns the exact candidate retry boundary. Mere admission, build, or canary success does
    /// not authorize rerunning the user's current task.
    ///
    /// # Errors
    ///
    /// Returns an error when the operation does not exist.
    pub fn retry_authority(
        &self,
        id: &EntityId,
    ) -> Result<HarnessRetryAuthority, HarnessPromotionError> {
        self.operation(id)
            .map(|operation| operation.retry_authority())
    }

    pub fn operation(
        &self,
        id: &EntityId,
    ) -> Result<HarnessRepairOperation, HarnessPromotionError> {
        self.lock_state()?
            .operations
            .iter()
            .find(|operation| &operation.id == id)
            .cloned()
            .ok_or(HarnessPromotionError::Missing)
    }

    pub fn operations(&self) -> Result<Vec<HarnessRepairOperation>, HarnessPromotionError> {
        Ok(self.lock_state()?.operations.clone())
    }

    pub fn projections(&self) -> Result<Vec<HarnessRepairProjection>, HarnessPromotionError> {
        Ok(self
            .lock_state()?
            .operations
            .iter()
            .rev()
            .map(HarnessRepairProjection::from)
            .collect())
    }

    fn transition(
        &self,
        id: &EntityId,
        from: HarnessRepairPhase,
        to: HarnessRepairPhase,
        status: &'static str,
        now: UtcTimestamp,
    ) -> Result<HarnessRepairOperation, HarnessPromotionError> {
        self.update(id, now, |operation| {
            if operation.phase != from {
                return Err(HarnessPromotionError::InvalidTransition);
            }
            operation.phase = to;
            operation.safe_status = safe_text(status)?;
            Ok(())
        })
    }

    fn update(
        &self,
        id: &EntityId,
        now: UtcTimestamp,
        mutate: impl FnOnce(&mut HarnessRepairOperation) -> Result<(), HarnessPromotionError>,
    ) -> Result<HarnessRepairOperation, HarnessPromotionError> {
        let mut state = self.lock_state()?;
        let before = state.clone();
        let operation = state
            .operations
            .iter_mut()
            .find(|operation| &operation.id == id)
            .ok_or(HarnessPromotionError::Missing)?;
        mutate(operation)?;
        operation.updated_at = now;
        operation.revision = operation
            .revision
            .checked_add(1)
            .ok_or(HarnessPromotionError::InvalidTransition)?;
        let result = operation.clone();
        if let Err(error) = self.persist_locked(&state) {
            *state = before;
            return Err(error);
        }
        Ok(result)
    }

    fn lock_state(&self) -> Result<MutexGuard<'_, PromotionState>, HarnessPromotionError> {
        self.state
            .lock()
            .map_err(|_| HarnessPromotionError::InvalidStorage)
    }

    fn persist_locked(&self, state: &PromotionState) -> Result<(), HarnessPromotionError> {
        validate_state(state)?;
        persist_state(&self.root, &self.state_path, state)
    }
}

fn admitted_candidate<'a>(
    population: &'a CandidatePopulation,
    candidate_id: &HarnessCandidateId,
) -> Result<&'a HarnessCandidate, HarnessPromotionError> {
    let candidate = population
        .candidates
        .iter()
        .find(|candidate| &candidate.id == candidate_id)
        .ok_or(HarnessPromotionError::CandidateNotAdmitted)?;
    let evaluation = candidate
        .evaluation
        .as_ref()
        .ok_or(HarnessPromotionError::CandidateNotAdmitted)?;
    if candidate.disposition != CandidateDisposition::Eligible
        || !population.frontier.candidate_ids.contains(candidate_id)
        || !evaluation.reproducible
        || !evaluation.safe
        || !evaluation.correction_adherent
        || !evaluation.within_budget
        || !evaluation.regression_free
        || !evaluation.statistically_meaningful
        || evaluation.leakage_detected
        || evaluation.reward_hacking_detected
        || !evaluation.evaluator_integrity
        || ![
            EvaluationSlice::Search,
            EvaluationSlice::Validation,
            EvaluationSlice::HeldOut,
        ]
        .iter()
        .all(|slice| evaluation.measurements.contains_key(slice))
    {
        return Err(HarnessPromotionError::CandidateNotAdmitted);
    }
    Ok(candidate)
}

fn repair_metrics(
    candidate: &HarnessCandidate,
) -> Result<HarnessRepairMetrics, HarnessPromotionError> {
    let evaluation = candidate
        .evaluation
        .as_ref()
        .ok_or(HarnessPromotionError::CandidateNotAdmitted)?;
    let measurements = evaluation.measurements.values().collect::<Vec<_>>();
    let cases = measurements.iter().fold(0_u32, |total, metric| {
        total.saturating_add(metric.case_count)
    });
    if cases == 0 {
        return Err(HarnessPromotionError::CandidateNotAdmitted);
    }
    Ok(HarnessRepairMetrics {
        cases,
        task_success_basis_points: weighted_basis_points(&measurements, |value| {
            value.task_success_basis_points
        }),
        truthful_completion_basis_points: weighted_basis_points(&measurements, |value| {
            value.truthful_completion_basis_points
        }),
        safety_basis_points: weighted_basis_points(&measurements, |value| {
            value.safety_basis_points
        }),
        correction_adherence_basis_points: weighted_basis_points(&measurements, |value| {
            value.correction_adherence_basis_points
        }),
        tokens: evaluation.actual_resources.tokens,
        external_cost_micros: evaluation.actual_resources.external_cost_micros,
        latency_ms: measurements
            .iter()
            .fold(0_u64, |total, value| total.saturating_add(value.latency_ms)),
        retries: u32::try_from(evaluation.actual_resources.retries).unwrap_or(u32::MAX),
        cpu_ms: evaluation.actual_resources.cpu_ms,
        peak_memory_bytes: evaluation.actual_resources.peak_memory_bytes,
        disk_bytes: evaluation.actual_resources.disk_bytes,
    })
}

fn weighted_basis_points(
    measurements: &[&EvaluationMeasurements],
    field: impl Fn(&EvaluationMeasurements) -> u16,
) -> u16 {
    let total_cases = measurements
        .iter()
        .map(|value| u64::from(value.case_count))
        .sum::<u64>();
    let weighted = measurements
        .iter()
        .map(|value| u64::from(value.case_count) * u64::from(field(value)))
        .sum::<u64>();
    u16::try_from(weighted / total_cases).unwrap_or(10_000)
}

fn projection_headline(phase: HarnessRepairPhase) -> &'static str {
    match phase {
        HarnessRepairPhase::Admitted => "Repair admitted",
        HarnessRepairPhase::AwaitingApproval => "Waiting for approval",
        HarnessRepairPhase::Approved => "Repair approved",
        HarnessRepairPhase::Building => "Checking the repair",
        HarnessRepairPhase::BuildFailed => "Build checks failed",
        HarnessRepairPhase::Built => "Build checks passed",
        HarnessRepairPhase::CanaryRunning => "Testing with an isolated worker",
        HarnessRepairPhase::CanaryRejected => "Canary rejected the repair",
        HarnessRepairPhase::CanaryPassed => "Canary passed",
        HarnessRepairPhase::Promoting => "Promoting the repair",
        HarnessRepairPhase::Observing => "Watching the live repair",
        HarnessRepairPhase::Promoted => "Repair is known-good",
        HarnessRepairPhase::ReversalRequired => "Reversal required",
        HarnessRepairPhase::Reversing => "Restoring the prior version",
        HarnessRepairPhase::Reverted => "Prior version restored",
        HarnessRepairPhase::Failed => "Repair stopped safely",
    }
}

fn initialize_root(root: &Path) -> Result<PathBuf, HarnessPromotionError> {
    match fs::symlink_metadata(root) {
        Ok(metadata) if metadata.file_type().is_symlink() || !metadata.is_dir() => {
            return Err(HarnessPromotionError::InvalidStorage);
        }
        Ok(_) => {}
        Err(error) if error.kind() == std::io::ErrorKind::NotFound => fs::create_dir_all(root)?,
        Err(error) => return Err(error.into()),
    }
    fs::canonicalize(root).map_err(Into::into)
}

fn read_state(path: &Path) -> Result<PromotionState, HarnessPromotionError> {
    let metadata = fs::symlink_metadata(path)?;
    if metadata.file_type().is_symlink() || !metadata.is_file() || metadata.len() > MAX_STATE_BYTES
    {
        return Err(HarnessPromotionError::InvalidStorage);
    }
    serde_json::from_slice(&fs::read(path)?).map_err(Into::into)
}

fn persist_state(
    root: &Path,
    path: &Path,
    state: &PromotionState,
) -> Result<(), HarnessPromotionError> {
    let bytes = serde_json::to_vec_pretty(state)?;
    if u64::try_from(bytes.len()).unwrap_or(u64::MAX) > MAX_STATE_BYTES {
        return Err(HarnessPromotionError::HistoryLimit);
    }
    let temporary = root.join(format!(".{STATE_FILE}.{}.tmp", EntityId::new()));
    let result = (|| {
        let mut file = OpenOptions::new()
            .create_new(true)
            .write(true)
            .open(&temporary)?;
        file.write_all(&bytes)?;
        file.sync_all()?;
        fs::rename(&temporary, path)?;
        File::open(root)?.sync_all()
    })();
    if result.is_err() {
        let _ = fs::remove_file(&temporary);
    }
    result.map_err(Into::into)
}

fn validate_state(state: &PromotionState) -> Result<(), HarnessPromotionError> {
    if state.version != STATE_VERSION
        || !state.availability.advisory
        || (state.availability.autonomous && !state.availability.shadow)
        || state.operations.len() > MAX_OPERATIONS
    {
        return Err(HarnessPromotionError::InvalidStorage);
    }
    for operation in &state.operations {
        if operation.revision == 0
            || operation.updated_at < operation.created_at
            || operation.metrics.cases == 0
            || operation.metrics.task_success_basis_points > 10_000
            || operation.metrics.truthful_completion_basis_points > 10_000
            || operation.metrics.safety_basis_points > 10_000
            || operation.metrics.correction_adherence_basis_points > 10_000
            || operation.canary_passed
                && operation
                    .build_image_id
                    .as_deref()
                    .is_none_or(|id| validate_image_id(id).is_err())
            || operation.promotion_id.is_some() && !operation.canary_passed
            || operation.phase == HarnessRepairPhase::Promoted && operation.promotion_id.is_none()
            || operation
                .prior_known_good_image_id
                .as_deref()
                .is_some_and(|id| validate_installed_image_id(id).is_err())
        {
            return Err(HarnessPromotionError::InvalidStorage);
        }
        safe_text(operation.safe_status.as_str())?;
    }
    Ok(())
}

fn validate_image_id(value: &str) -> Result<(), HarnessPromotionError> {
    if value.len() == 64 && value.bytes().all(|byte| byte.is_ascii_hexdigit()) {
        Ok(())
    } else {
        Err(HarnessPromotionError::InvalidTransition)
    }
}

fn validate_installed_image_id(value: &str) -> Result<(), HarnessPromotionError> {
    if validate_image_id(value).is_ok()
        || value
            .strip_prefix("bootstrap-")
            .is_some_and(|digest| validate_image_id(digest).is_ok())
    {
        Ok(())
    } else {
        Err(HarnessPromotionError::InvalidTransition)
    }
}

fn safe_text(value: &str) -> Result<RedactedText, HarnessPromotionError> {
    RedactedText::parse(value.to_owned()).map_err(|_| HarnessPromotionError::UnsafeText)
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::{
        CandidateDiffEntry, CandidateDiffOperation, CandidateResourceEstimate,
        CandidateSafetyResult, CandidateSourceKind, CandidateSourceSnapshot,
        EvaluationResourceUsage, HarnessEvaluation, MetricDirection, ParetoFrontier,
        PopulationState,
    };
    use std::collections::BTreeMap;

    fn text(value: &str) -> RedactedText {
        RedactedText::parse(value).unwrap()
    }

    fn population() -> CandidatePopulation {
        let candidate_id = HarnessCandidateId::new();
        let measurements = BTreeMap::from([
            (EvaluationSlice::Search, measurement(8_500)),
            (EvaluationSlice::Validation, measurement(8_800)),
            (EvaluationSlice::HeldOut, measurement(9_100)),
        ]);
        let candidate = HarnessCandidate {
            id: candidate_id.clone(),
            population_id: HarnessExperimentId::new(),
            parent_id: None,
            source: CandidateSourceKind::Proposed,
            hypothesis: text("repair a retry routing defect"),
            base_source_digest: format!("sha256:{}", "1".repeat(64)),
            candidate_digest: format!("sha256:{}", "2".repeat(64)),
            diff: vec![CandidateDiffEntry {
                relative_path: PathBuf::from("src/lib.rs"),
                operation: CandidateDiffOperation::Write,
                previous_digest: Some(format!("sha256:{}", "3".repeat(64))),
                resulting_digest: Some(format!("sha256:{}", "4".repeat(64))),
                resulting_bytes: 12,
            }],
            source_snapshot: vec![CandidateSourceSnapshot {
                relative_path: PathBuf::from("src/lib.rs"),
                source_digest: format!("sha256:{}", "4".repeat(64)),
                source: "fn fixed() {}".into(),
            }],
            trace_references: vec![text("trace-fingerprint")],
            safe_trace_excerpts: vec![text("the retry omitted required context")],
            resources: CandidateResourceEstimate {
                proposal_tokens: 20,
                estimated_latency_ms: 40,
                estimated_external_cost_micros: 3,
                shadow_disk_bytes: 200,
            },
            evaluation: Some(HarnessEvaluation {
                candidate_id: candidate_id.clone(),
                search_version: text("search-v1"),
                validation_version: text("validation-v1"),
                held_out_version: text("held-out-v1"),
                measurements,
                actual_resources: EvaluationResourceUsage {
                    tokens: 40,
                    external_cost_micros: 9,
                    wall_ms: 60,
                    retries: 0,
                    cpu_ms: 50,
                    peak_memory_bytes: 300,
                    disk_bytes: 200,
                },
                reproducible: true,
                safe: true,
                correction_adherent: true,
                within_budget: true,
                regression_free: true,
                statistically_meaningful: true,
                leakage_detected: false,
                reward_hacking_detected: false,
                evaluator_integrity: true,
                evaluation_digest: format!("sha256:{}", "5".repeat(64)),
                evaluated_at: UtcTimestamp::from_unix_millis(1),
            }),
            safety_result: CandidateSafetyResult::Passed,
            disposition: CandidateDisposition::Eligible,
            shadow_relative_path: PathBuf::from("shadows/candidate"),
            created_at: UtcTimestamp::from_unix_millis(1),
            updated_at: UtcTimestamp::from_unix_millis(1),
        };
        CandidatePopulation {
            id: candidate.population_id.clone(),
            diagnosis_id: EntityId::new(),
            diagnosis_trace_fingerprint: text("trace-fingerprint"),
            target_direction: MetricDirection::Increase,
            target_baseline: 7_500,
            target_threshold: 8_000,
            cost_ceiling_micros: 1_000,
            latency_ceiling_ms: 1_000,
            token_ceiling: 1_000,
            retry_ceiling: 2,
            state: PopulationState::Evaluated,
            candidates: vec![candidate],
            frontier: ParetoFrontier {
                candidate_ids: vec![candidate_id],
                evaluation_version: 1,
            },
            created_at: UtcTimestamp::from_unix_millis(1),
            updated_at: UtcTimestamp::from_unix_millis(1),
        }
    }

    fn measurement(success: u16) -> EvaluationMeasurements {
        EvaluationMeasurements {
            case_count: 10,
            task_success_basis_points: success,
            truthful_completion_basis_points: 10_000,
            safety_basis_points: 10_000,
            correction_adherence_basis_points: 10_000,
            external_cost_micros: 2,
            latency_ms: 20,
            retries: 0,
            cpu_ms: 15,
            peak_memory_bytes: 300,
            disk_bytes: 200,
        }
    }

    #[test]
    fn meta_harness_shadow_never_authorizes_retry_or_promotion_without_approval() {
        let directory = tempfile::tempdir().unwrap();
        let registry = HarnessPromotionRegistry::open(
            directory.path(),
            HarnessModeAvailability::fully_available(),
        )
        .unwrap();
        let population = population();
        let candidate_id = population.frontier.candidate_ids[0].clone();
        let operation = registry
            .admit(
                &population,
                &candidate_id,
                HarnessOperationMode::Shadow,
                UtcTimestamp::from_unix_millis(2),
            )
            .unwrap();
        assert_eq!(
            registry.retry_authority(&operation.id).unwrap(),
            HarnessRetryAuthority::Denied
        );
        registry
            .begin_build(&operation.id, UtcTimestamp::from_unix_millis(3))
            .unwrap();
        registry
            .record_build(
                &operation.id,
                Some("a".repeat(64)),
                None,
                UtcTimestamp::from_unix_millis(4),
            )
            .unwrap();
        registry
            .begin_canary(&operation.id, UtcTimestamp::from_unix_millis(5))
            .unwrap();
        let operation = registry
            .record_canary(&operation.id, true, None, UtcTimestamp::from_unix_millis(6))
            .unwrap();
        assert!(!operation.promotion_allowed());
        assert!(
            registry
                .begin_promotion(&operation.id, UtcTimestamp::from_unix_millis(7))
                .is_err()
        );
        let approved = registry
            .approve(
                &operation.id,
                "installation owner".into(),
                UtcTimestamp::from_unix_millis(8),
            )
            .unwrap();
        assert!(approved.promotion_allowed());
        assert_eq!(
            registry.retry_authority(&operation.id).unwrap(),
            HarnessRetryAuthority::ExplicitApproval
        );
    }

    #[test]
    fn meta_harness_interrupted_promotion_survives_restart() {
        let directory = tempfile::tempdir().unwrap();
        let population = population();
        let candidate_id = population.frontier.candidate_ids[0].clone();
        let operation_id = {
            let registry = HarnessPromotionRegistry::open(
                directory.path(),
                HarnessModeAvailability::fully_available(),
            )
            .unwrap();
            let operation = registry
                .admit(
                    &population,
                    &candidate_id,
                    HarnessOperationMode::Autonomous,
                    UtcTimestamp::from_unix_millis(2),
                )
                .unwrap();
            registry
                .begin_build(&operation.id, UtcTimestamp::from_unix_millis(3))
                .unwrap();
            registry
                .record_build(
                    &operation.id,
                    Some("a".repeat(64)),
                    None,
                    UtcTimestamp::from_unix_millis(4),
                )
                .unwrap();
            registry
                .begin_canary(&operation.id, UtcTimestamp::from_unix_millis(5))
                .unwrap();
            registry
                .record_canary(&operation.id, true, None, UtcTimestamp::from_unix_millis(6))
                .unwrap();
            registry
                .begin_promotion(&operation.id, UtcTimestamp::from_unix_millis(7))
                .unwrap();
            operation.id
        };
        let reopened = HarnessPromotionRegistry::open(
            directory.path(),
            HarnessModeAvailability::fully_available(),
        )
        .unwrap();
        assert_eq!(
            reopened.operation(&operation_id).unwrap().recovery_action(),
            HarnessRecoveryAction::RecoverPromotion
        );
    }

    #[test]
    fn meta_harness_interrupted_reversal_survives_restart() {
        let directory = tempfile::tempdir().unwrap();
        let population = population();
        let candidate_id = population.frontier.candidate_ids[0].clone();
        let operation_id = {
            let registry = HarnessPromotionRegistry::open(
                directory.path(),
                HarnessModeAvailability::fully_available(),
            )
            .unwrap();
            let operation = registry
                .admit(
                    &population,
                    &candidate_id,
                    HarnessOperationMode::Autonomous,
                    UtcTimestamp::from_unix_millis(2),
                )
                .unwrap();
            registry
                .begin_build(&operation.id, UtcTimestamp::from_unix_millis(3))
                .unwrap();
            registry
                .record_build(
                    &operation.id,
                    Some("a".repeat(64)),
                    None,
                    UtcTimestamp::from_unix_millis(4),
                )
                .unwrap();
            registry
                .begin_canary(&operation.id, UtcTimestamp::from_unix_millis(5))
                .unwrap();
            registry
                .record_canary(&operation.id, true, None, UtcTimestamp::from_unix_millis(6))
                .unwrap();
            registry
                .begin_promotion(&operation.id, UtcTimestamp::from_unix_millis(7))
                .unwrap();
            registry
                .record_observing(
                    &operation.id,
                    EntityId::new(),
                    "b".repeat(64),
                    UtcTimestamp::from_unix_millis(8),
                )
                .unwrap();
            registry
                .require_reversal(
                    &operation.id,
                    "the observed repair regressed".into(),
                    UtcTimestamp::from_unix_millis(9),
                )
                .unwrap();
            registry
                .begin_reversal(&operation.id, UtcTimestamp::from_unix_millis(10))
                .unwrap();
            operation.id
        };
        let reopened = HarnessPromotionRegistry::open(
            directory.path(),
            HarnessModeAvailability::fully_available(),
        )
        .unwrap();
        assert_eq!(
            reopened.operation(&operation_id).unwrap().recovery_action(),
            HarnessRecoveryAction::RecoverReversal
        );
    }
}
