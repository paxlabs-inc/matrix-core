use std::collections::{BTreeMap, BTreeSet};
use std::fs::{self, File, OpenOptions};
use std::io::Write;
use std::path::{Component, Path, PathBuf};
use std::process::{Child, Command};
use std::sync::{Mutex, MutexGuard};

use fs2::FileExt;
use keith_agent_types::{ActionId, EntityId, UtcTimestamp, canonical_json_bytes};
use keith_resource_governor::{ExhaustionBehavior, ResourceCeiling, ResourceKind, ResourceScope};
use keith_sandbox::{configure_owned_process, terminate_owned_process_tree};
#[cfg(unix)]
use nix::errno::Errno;
#[cfg(unix)]
use nix::sys::signal::{Signal, killpg};
#[cfg(unix)]
use nix::unistd::Pid;
use serde::{Deserialize, Serialize};
use thiserror::Error;

const STATE_VERSION: u32 = 1;
const STATE_FILE: &str = "budget.json";
const LOCK_FILE: &str = "budget.lock";
const MAX_STATE_BYTES: u64 = 8 * 1024 * 1024;
const MAX_ANCESTORS: usize = 64;
const MAX_ARTIFACTS: usize = 64;
const MAX_REASON_BYTES: usize = 2 * 1024;

#[derive(Clone, Copy, Debug, Eq, Hash, Ord, PartialEq, PartialOrd, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum EvolutionWorkKind {
    Hypothesis,
    ShadowTree,
    SandboxedBuild,
    CanaryWorker,
    Promotion,
}

#[derive(Clone, Copy, Debug, Eq, Hash, Ord, PartialEq, PartialOrd, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum EvolutionUsageKind {
    Tokens,
    ModelCostMicros,
    WallTimeMs,
    CpuTimeMs,
    MemoryBytes,
    DiskBytes,
}

#[derive(Clone, Copy, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct EvolutionUsage {
    pub tokens: u64,
    pub model_cost_micros: u64,
    pub wall_time_ms: u64,
    pub cpu_time_ms: u64,
    pub memory_bytes: u64,
    pub disk_bytes: u64,
}

impl EvolutionUsage {
    #[must_use]
    pub const fn none() -> Self {
        Self {
            tokens: 0,
            model_cost_micros: 0,
            wall_time_ms: 0,
            cpu_time_ms: 0,
            memory_bytes: 0,
            disk_bytes: 0,
        }
    }

    fn values(self) -> [(EvolutionUsageKind, u64); 6] {
        [
            (EvolutionUsageKind::Tokens, self.tokens),
            (EvolutionUsageKind::ModelCostMicros, self.model_cost_micros),
            (EvolutionUsageKind::WallTimeMs, self.wall_time_ms),
            (EvolutionUsageKind::CpuTimeMs, self.cpu_time_ms),
            (EvolutionUsageKind::MemoryBytes, self.memory_bytes),
            (EvolutionUsageKind::DiskBytes, self.disk_bytes),
        ]
    }
}

impl Default for EvolutionUsage {
    fn default() -> Self {
        Self::none()
    }
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct EvolutionCeilings {
    pub concurrent_hypotheses: u32,
    pub concurrent_shadow_trees: u32,
    pub concurrent_sandboxed_builds: u32,
    pub concurrent_canary_workers: u32,
    pub promotions_per_interval: u32,
    pub promotion_interval_ms: u64,
    pub retained_images: u32,
    pub usage_interval_ms: u64,
    pub tokens: u64,
    pub model_cost_micros: u64,
    pub wall_time_ms: u64,
    pub cpu_time_ms: u64,
    pub memory_bytes: u64,
    pub disk_bytes: u64,
}

impl EvolutionCeilings {
    /// Validates an immutable installation-owned ceiling set.
    pub fn validate(&self) -> Result<(), BudgetError> {
        let concurrency = [
            self.concurrent_hypotheses,
            self.concurrent_shadow_trees,
            self.concurrent_sandboxed_builds,
            self.concurrent_canary_workers,
            self.promotions_per_interval,
            self.retained_images,
        ];
        let usage = [
            self.promotion_interval_ms,
            self.usage_interval_ms,
            self.tokens,
            self.model_cost_micros,
            self.wall_time_ms,
            self.cpu_time_ms,
            self.memory_bytes,
            self.disk_bytes,
        ];
        if concurrency.contains(&0) || usage.contains(&0) {
            return Err(BudgetError::Invalid(
                "every evolution ceiling and interval must be non-zero".into(),
            ));
        }
        Ok(())
    }

    /// Returns the exact installation-scoped entries to merge into the ordinary resource policy.
    #[must_use]
    pub fn governor_ceilings(&self) -> BTreeMap<(ResourceScope, ResourceKind), ResourceCeiling> {
        let fail = |maximum| ResourceCeiling {
            maximum,
            exhaustion: ExhaustionBehavior::Fail,
        };
        BTreeMap::from([
            (
                (
                    ResourceScope::Installation,
                    ResourceKind::EvolutionHypotheses,
                ),
                fail(u64::from(self.concurrent_hypotheses)),
            ),
            (
                (
                    ResourceScope::Installation,
                    ResourceKind::EvolutionShadowTrees,
                ),
                fail(u64::from(self.concurrent_shadow_trees)),
            ),
            (
                (ResourceScope::Installation, ResourceKind::EvolutionBuilds),
                fail(u64::from(self.concurrent_sandboxed_builds)),
            ),
            (
                (ResourceScope::Installation, ResourceKind::EvolutionCanaries),
                fail(u64::from(self.concurrent_canary_workers)),
            ),
            (
                (ResourceScope::Installation, ResourceKind::Tokens),
                fail(self.tokens),
            ),
            (
                (ResourceScope::Installation, ResourceKind::ModelCostMicros),
                fail(self.model_cost_micros),
            ),
            (
                (ResourceScope::Installation, ResourceKind::WallTimeMs),
                fail(self.wall_time_ms),
            ),
            (
                (ResourceScope::Installation, ResourceKind::CpuTimeMs),
                fail(self.cpu_time_ms),
            ),
            (
                (ResourceScope::Installation, ResourceKind::MemoryBytes),
                fail(self.memory_bytes),
            ),
            (
                (ResourceScope::Installation, ResourceKind::StorageBytes),
                fail(self.disk_bytes),
            ),
        ])
    }

    const fn concurrent_limit(&self, kind: EvolutionWorkKind) -> Option<u64> {
        match kind {
            EvolutionWorkKind::Hypothesis => Some(self.concurrent_hypotheses as u64),
            EvolutionWorkKind::ShadowTree => Some(self.concurrent_shadow_trees as u64),
            EvolutionWorkKind::SandboxedBuild => Some(self.concurrent_sandboxed_builds as u64),
            EvolutionWorkKind::CanaryWorker => Some(self.concurrent_canary_workers as u64),
            EvolutionWorkKind::Promotion => None,
        }
    }

    const fn usage_limit(&self, kind: EvolutionUsageKind) -> u64 {
        match kind {
            EvolutionUsageKind::Tokens => self.tokens,
            EvolutionUsageKind::ModelCostMicros => self.model_cost_micros,
            EvolutionUsageKind::WallTimeMs => self.wall_time_ms,
            EvolutionUsageKind::CpuTimeMs => self.cpu_time_ms,
            EvolutionUsageKind::MemoryBytes => self.memory_bytes,
            EvolutionUsageKind::DiskBytes => self.disk_bytes,
        }
    }
}

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum EvolutionCapability {
    Observe,
    FormHypothesis,
    StageShadowTree,
    RunSandboxedBuild,
    RunCanary,
    PromoteCandidate,
    RetainImage,
    ReplicateInstallation,
    StartAdditionalDaemon,
    InstallDaemon,
    ModifyInstallation,
    ModifyUpdateChannel,
    EditBudget,
    EditCeilings,
    WidenPermission,
    WidenSandbox,
    AccessCredential,
    WidenNetwork,
    SpawnEvolutionChild,
}

impl EvolutionCapability {
    const fn work_kind(self) -> Option<EvolutionWorkKind> {
        match self {
            Self::FormHypothesis => Some(EvolutionWorkKind::Hypothesis),
            Self::StageShadowTree => Some(EvolutionWorkKind::ShadowTree),
            Self::RunSandboxedBuild => Some(EvolutionWorkKind::SandboxedBuild),
            Self::RunCanary => Some(EvolutionWorkKind::CanaryWorker),
            Self::PromoteCandidate => Some(EvolutionWorkKind::Promotion),
            Self::Observe | Self::RetainImage => None,
            Self::ReplicateInstallation
            | Self::StartAdditionalDaemon
            | Self::InstallDaemon
            | Self::ModifyInstallation
            | Self::ModifyUpdateChannel
            | Self::EditBudget
            | Self::EditCeilings
            | Self::WidenPermission
            | Self::WidenSandbox
            | Self::AccessCredential
            | Self::WidenNetwork
            | Self::SpawnEvolutionChild => None,
        }
    }

    const fn is_refused(self) -> bool {
        matches!(
            self,
            Self::ReplicateInstallation
                | Self::StartAdditionalDaemon
                | Self::InstallDaemon
                | Self::ModifyInstallation
                | Self::ModifyUpdateChannel
                | Self::EditBudget
                | Self::EditCeilings
                | Self::WidenPermission
                | Self::WidenSandbox
                | Self::AccessCredential
                | Self::WidenNetwork
                | Self::SpawnEvolutionChild
        )
    }
}

#[derive(Clone, Debug, Eq, PartialEq)]
pub struct EvolutionActionAncestor {
    pub action_id: ActionId,
    pub is_evolution: bool,
}

#[derive(Clone, Debug, Eq, PartialEq)]
pub struct EvolutionOrigin {
    pub action_id: ActionId,
    pub ancestors: Vec<EvolutionActionAncestor>,
    pub dedicated_child: bool,
}

impl EvolutionOrigin {
    fn validate(&self) -> Result<(), BudgetError> {
        if self.dedicated_child || self.ancestors.iter().any(|ancestor| ancestor.is_evolution) {
            return Err(BudgetError::RecursiveEvolution);
        }
        if self.ancestors.len() > MAX_ANCESTORS {
            return Err(BudgetError::Invalid("action ancestry is too deep".into()));
        }
        let mut identities = BTreeSet::new();
        if !identities.insert(self.action_id.clone())
            || self
                .ancestors
                .iter()
                .any(|ancestor| !identities.insert(ancestor.action_id.clone()))
        {
            return Err(BudgetError::Invalid(
                "action ancestry contains a cycle or duplicate".into(),
            ));
        }
        Ok(())
    }
}

#[derive(Clone, Debug, Eq, PartialEq)]
pub struct EvolutionAdmission {
    pub operation_id: EntityId,
    pub kind: EvolutionWorkKind,
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum BudgetAuditOutcome {
    Admitted,
    Completed,
    Aborted,
    Reconciled,
    Refused,
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct BudgetAuditRecord {
    pub sequence: u64,
    pub operation_id: Option<EntityId>,
    pub occurred_at: UtcTimestamp,
    pub outcome: BudgetAuditOutcome,
    pub reason: String,
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
struct OperationRecord {
    id: EntityId,
    action_id: ActionId,
    kind: EvolutionWorkKind,
    artifacts: Vec<PathBuf>,
    pid: Option<u32>,
    process_start_ticks: Option<u64>,
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
struct UsageSample {
    occurred_at: UtcTimestamp,
    usage: EvolutionUsage,
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
struct DurableBudgetState {
    version: u32,
    ceilings: EvolutionCeilings,
    operations: BTreeMap<EntityId, OperationRecord>,
    active: BTreeMap<EvolutionWorkKind, u64>,
    promotion_times: Vec<UtcTimestamp>,
    retained_images: BTreeSet<String>,
    usage: Vec<UsageSample>,
    last_timestamp: UtcTimestamp,
    audit: Vec<BudgetAuditRecord>,
}

#[derive(Debug, Error)]
pub enum BudgetError {
    #[error("evolution budget configuration is invalid: {0}")]
    Invalid(String),
    #[error("evolution budget ceilings are immutable for this installation")]
    ImmutableCeilings,
    #[error("recursive or dedicated-child evolution is refused")]
    RecursiveEvolution,
    #[error("evolution capability is categorically refused: {0:?}")]
    Refused(EvolutionCapability),
    #[error("evolution budget exhausted for {0}")]
    Exhausted(String),
    #[error("evolution operation does not exist")]
    Missing,
    #[error("evolution operation still owns a running process")]
    ProcessRunning,
    #[error("evolution cleanup remains unresolved: {0}")]
    Cleanup(String),
    #[error("evolution budget filesystem failed: {0}")]
    Io(#[from] std::io::Error),
    #[error("evolution budget state failed: {0}")]
    State(#[from] serde_json::Error),
    #[error("evolution budget lock was poisoned")]
    LockPoisoned,
}

/// Durable, installation-scoped authority for bounded evolution work.
pub struct EvolutionBudget {
    root: PathBuf,
    state_path: PathBuf,
    state: Mutex<DurableBudgetState>,
    children: Mutex<BTreeMap<EntityId, Child>>,
    _lock: File,
}

impl EvolutionBudget {
    /// Opens the budget, rejects ceiling changes, and reconciles interrupted work before returning.
    pub fn open(
        root: impl Into<PathBuf>,
        ceilings: EvolutionCeilings,
        now: UtcTimestamp,
    ) -> Result<Self, BudgetError> {
        ceilings.validate()?;
        let root = root.into();
        fs::create_dir_all(&root)?;
        reject_symlink(&root)?;
        let root = fs::canonicalize(root)?;
        let lock = OpenOptions::new()
            .create(true)
            .read(true)
            .write(true)
            .truncate(false)
            .open(root.join(LOCK_FILE))?;
        lock.try_lock_exclusive()
            .map_err(|_| BudgetError::Invalid("another budget authority owns this root".into()))?;
        let state_path = root.join(STATE_FILE);
        let state = if state_path.exists() {
            let state = load_state(&state_path)?;
            if state.ceilings != ceilings {
                return Err(BudgetError::ImmutableCeilings);
            }
            validate_state(&root, &state)?;
            state
        } else {
            let state = DurableBudgetState {
                version: STATE_VERSION,
                ceilings,
                operations: BTreeMap::new(),
                active: BTreeMap::new(),
                promotion_times: Vec::new(),
                retained_images: BTreeSet::new(),
                usage: Vec::new(),
                last_timestamp: now,
                audit: Vec::new(),
            };
            persist_state(&root, &state_path, &state)?;
            state
        };
        let budget = Self {
            root,
            state_path,
            state: Mutex::new(state),
            children: Mutex::new(BTreeMap::new()),
            _lock: lock,
        };
        budget.reconcile(now)?;
        Ok(budget)
    }

    /// Admits one typed operation after ancestry, capability, and independent ceilings pass.
    pub fn admit(
        &self,
        origin: &EvolutionOrigin,
        capability: EvolutionCapability,
        artifacts: Vec<PathBuf>,
        now: UtcTimestamp,
    ) -> Result<EvolutionAdmission, BudgetError> {
        origin.validate()?;
        if capability.is_refused() {
            self.audit_refusal(now, format!("categorically refused {capability:?}"))?;
            return Err(BudgetError::Refused(capability));
        }
        let kind = capability.work_kind().ok_or_else(|| {
            BudgetError::Invalid("capability does not start budgeted work".into())
        })?;
        validate_artifacts(&self.root, &artifacts)?;
        let mut state = self.state()?;
        let mut next = state.clone();
        advance_time(&mut next, now)?;
        prune_windows(&mut next, now);
        if kind == EvolutionWorkKind::Promotion {
            if next.promotion_times.len()
                >= usize::try_from(next.ceilings.promotions_per_interval).unwrap_or(usize::MAX)
            {
                let reason = "promotions_per_interval".to_owned();
                append_audit(&mut next, None, now, BudgetAuditOutcome::Refused, &reason)?;
                self.persist(&next)?;
                *state = next;
                return Err(BudgetError::Exhausted(reason));
            }
            next.promotion_times.push(now);
        } else if let Some(limit) = next.ceilings.concurrent_limit(kind) {
            let active = next.active.get(&kind).copied().unwrap_or(0);
            if active >= limit {
                let reason = format!("concurrent_{kind:?}");
                append_audit(&mut next, None, now, BudgetAuditOutcome::Refused, &reason)?;
                self.persist(&next)?;
                *state = next;
                return Err(BudgetError::Exhausted(reason));
            }
            next.active.insert(kind, active.saturating_add(1));
        }
        let operation_id = EntityId::new();
        next.operations.insert(
            operation_id.clone(),
            OperationRecord {
                id: operation_id.clone(),
                action_id: origin.action_id.clone(),
                kind,
                artifacts,
                pid: None,
                process_start_ticks: None,
            },
        );
        append_audit(
            &mut next,
            Some(operation_id.clone()),
            now,
            BudgetAuditOutcome::Admitted,
            "budgeted evolution operation admitted",
        )?;
        self.persist(&next)?;
        *state = next;
        Ok(EvolutionAdmission { operation_id, kind })
    }

    /// Spawns a process in its own process group after its cleanup paths are durable.
    pub fn spawn_owned(
        &self,
        operation_id: &EntityId,
        command: &mut Command,
    ) -> Result<u32, BudgetError> {
        {
            let state = self.state()?;
            let operation = state
                .operations
                .get(operation_id)
                .ok_or(BudgetError::Missing)?;
            if operation.pid.is_some() {
                return Err(BudgetError::Invalid(
                    "operation already owns a process".into(),
                ));
            }
        }
        configure_owned_process(command);
        let mut child = command.spawn()?;
        let pid = child.id();
        let start_ticks = process_start_ticks(pid).ok();
        let persist_result = (|| {
            let mut state = self.state()?;
            let operation = state
                .operations
                .get_mut(operation_id)
                .ok_or(BudgetError::Missing)?;
            operation.pid = Some(pid);
            operation.process_start_ticks = start_ticks;
            self.persist(&state)
        })();
        if let Err(error) = persist_result {
            let _ = terminate_owned_process_tree(&mut child);
            return Err(error);
        }
        self.children()?.insert(operation_id.clone(), child);
        Ok(pid)
    }

    /// Accounts measured work inside a fixed installation window and aborts on exhaustion.
    pub fn record_usage(
        &self,
        operation_id: &EntityId,
        usage: EvolutionUsage,
        now: UtcTimestamp,
    ) -> Result<(), BudgetError> {
        if usage.values().iter().all(|(_, value)| *value == 0) {
            return Err(BudgetError::Invalid("usage delta is empty".into()));
        }
        let exhausted = {
            let mut state = self.state()?;
            let mut next = state.clone();
            if !next.operations.contains_key(operation_id) {
                return Err(BudgetError::Missing);
            }
            advance_time(&mut next, now)?;
            prune_windows(&mut next, now);
            let exhausted = usage.values().into_iter().find_map(|(kind, delta)| {
                let consumed = next
                    .usage
                    .iter()
                    .map(|sample| usage_value(sample.usage, kind))
                    .try_fold(0_u64, u64::checked_add)
                    .unwrap_or(u64::MAX);
                consumed
                    .checked_add(delta)
                    .filter(|value| *value > next.ceilings.usage_limit(kind))
                    .map(|_| kind)
            });
            if exhausted.is_none() {
                next.usage.push(UsageSample {
                    occurred_at: now,
                    usage,
                });
                self.persist(&next)?;
                *state = next;
            }
            exhausted
        };
        if let Some(kind) = exhausted {
            let reason = format!("{kind:?} budget exhausted");
            self.abort(operation_id, &reason, now)?;
            return Err(BudgetError::Exhausted(reason));
        }
        Ok(())
    }

    /// Adds one verified image to the durable retention set without exceeding its independent cap.
    pub fn retain_image(&self, image_id: &str, now: UtcTimestamp) -> Result<(), BudgetError> {
        validate_identifier(image_id)?;
        let mut state = self.state()?;
        let mut next = state.clone();
        advance_time(&mut next, now)?;
        if next.retained_images.contains(image_id) {
            return Ok(());
        }
        if next.retained_images.len()
            >= usize::try_from(next.ceilings.retained_images).unwrap_or(usize::MAX)
        {
            return Err(BudgetError::Exhausted("retained_images".into()));
        }
        next.retained_images.insert(image_id.to_owned());
        self.persist(&next)?;
        *state = next;
        Ok(())
    }

    pub fn release_image(&self, image_id: &str) -> Result<bool, BudgetError> {
        let mut state = self.state()?;
        let mut next = state.clone();
        let removed = next.retained_images.remove(image_id);
        if removed {
            self.persist(&next)?;
            *state = next;
        }
        Ok(removed)
    }

    /// Completes successful work only after its owned process has exited.
    pub fn complete(&self, operation_id: &EntityId, now: UtcTimestamp) -> Result<(), BudgetError> {
        if let Some(mut child) = self.children()?.remove(operation_id)
            && child.try_wait()?.is_none()
        {
            self.children()?.insert(operation_id.clone(), child);
            return Err(BudgetError::ProcessRunning);
        }
        let mut state = self.state()?;
        let mut next = state.clone();
        advance_time(&mut next, now)?;
        let operation = next
            .operations
            .remove(operation_id)
            .ok_or(BudgetError::Missing)?;
        release_active(&mut next, operation.kind)?;
        append_audit(
            &mut next,
            Some(operation_id.clone()),
            now,
            BudgetAuditOutcome::Completed,
            "budgeted evolution operation completed",
        )?;
        self.persist(&next)?;
        *state = next;
        Ok(())
    }

    /// Terminates the exact owned process group, removes only registered staging artifacts, and
    /// releases accounting after cleanup is complete.
    pub fn abort(
        &self,
        operation_id: &EntityId,
        reason: &str,
        now: UtcTimestamp,
    ) -> Result<(), BudgetError> {
        let reason = safe_reason(reason)?;
        let operation = self
            .state()?
            .operations
            .get(operation_id)
            .cloned()
            .ok_or(BudgetError::Missing)?;
        self.cleanup_operation(&operation)?;
        let mut state = self.state()?;
        let mut next = state.clone();
        advance_time(&mut next, now)?;
        let removed = next
            .operations
            .remove(operation_id)
            .ok_or(BudgetError::Missing)?;
        release_active(&mut next, removed.kind)?;
        append_audit(
            &mut next,
            Some(operation_id.clone()),
            now,
            BudgetAuditOutcome::Aborted,
            &reason,
        )?;
        self.persist(&next)?;
        *state = next;
        Ok(())
    }

    #[must_use]
    pub fn audit(&self) -> Result<Vec<BudgetAuditRecord>, BudgetError> {
        Ok(self.state()?.audit.clone())
    }

    #[must_use]
    pub fn active(&self, kind: EvolutionWorkKind) -> Result<u64, BudgetError> {
        Ok(self.state()?.active.get(&kind).copied().unwrap_or(0))
    }

    #[must_use]
    pub fn retained_images(&self) -> Result<BTreeSet<String>, BudgetError> {
        Ok(self.state()?.retained_images.clone())
    }

    fn reconcile(&self, now: UtcTimestamp) -> Result<(), BudgetError> {
        let operations = self
            .state()?
            .operations
            .values()
            .cloned()
            .collect::<Vec<_>>();
        for operation in operations {
            self.cleanup_operation(&operation)?;
            let mut state = self.state()?;
            let mut next = state.clone();
            next.operations.remove(&operation.id);
            release_active(&mut next, operation.kind)?;
            append_audit(
                &mut next,
                Some(operation.id),
                now,
                BudgetAuditOutcome::Reconciled,
                "interrupted evolution operation reclaimed",
            )?;
            self.persist(&next)?;
            *state = next;
        }
        Ok(())
    }

    fn cleanup_operation(&self, operation: &OperationRecord) -> Result<(), BudgetError> {
        if let Some(mut child) = self.children()?.remove(&operation.id) {
            terminate_owned_process_tree(&mut child)
                .map_err(|error| BudgetError::Cleanup(error.to_string()))?;
        } else if let Some(pid) = operation.pid {
            terminate_recovered_process(pid, operation.process_start_ticks)?;
        }
        for relative in operation.artifacts.iter().rev() {
            remove_owned(&self.root, relative)?;
        }
        Ok(())
    }

    fn audit_refusal(&self, now: UtcTimestamp, reason: String) -> Result<(), BudgetError> {
        let mut state = self.state()?;
        let mut next = state.clone();
        advance_time(&mut next, now)?;
        append_audit(&mut next, None, now, BudgetAuditOutcome::Refused, &reason)?;
        self.persist(&next)?;
        *state = next;
        Ok(())
    }

    fn state(&self) -> Result<MutexGuard<'_, DurableBudgetState>, BudgetError> {
        self.state.lock().map_err(|_| BudgetError::LockPoisoned)
    }

    fn children(&self) -> Result<MutexGuard<'_, BTreeMap<EntityId, Child>>, BudgetError> {
        self.children.lock().map_err(|_| BudgetError::LockPoisoned)
    }

    fn persist(&self, state: &DurableBudgetState) -> Result<(), BudgetError> {
        persist_state(&self.root, &self.state_path, state)
    }
}

fn release_active(
    state: &mut DurableBudgetState,
    kind: EvolutionWorkKind,
) -> Result<(), BudgetError> {
    let Some(limit) = state.ceilings.concurrent_limit(kind) else {
        return Ok(());
    };
    let active = state.active.get(&kind).copied().unwrap_or(0);
    if active == 0 || active > limit {
        return Err(BudgetError::Invalid(
            "durable active accounting is inconsistent".into(),
        ));
    }
    state.active.insert(kind, active - 1);
    Ok(())
}

fn usage_value(usage: EvolutionUsage, kind: EvolutionUsageKind) -> u64 {
    match kind {
        EvolutionUsageKind::Tokens => usage.tokens,
        EvolutionUsageKind::ModelCostMicros => usage.model_cost_micros,
        EvolutionUsageKind::WallTimeMs => usage.wall_time_ms,
        EvolutionUsageKind::CpuTimeMs => usage.cpu_time_ms,
        EvolutionUsageKind::MemoryBytes => usage.memory_bytes,
        EvolutionUsageKind::DiskBytes => usage.disk_bytes,
    }
}

fn prune_windows(state: &mut DurableBudgetState, now: UtcTimestamp) {
    let promotion_cutoff = now
        .unix_millis()
        .saturating_sub(i64::try_from(state.ceilings.promotion_interval_ms).unwrap_or(i64::MAX));
    state
        .promotion_times
        .retain(|time| time.unix_millis() > promotion_cutoff);
    let usage_cutoff = now
        .unix_millis()
        .saturating_sub(i64::try_from(state.ceilings.usage_interval_ms).unwrap_or(i64::MAX));
    state
        .usage
        .retain(|sample| sample.occurred_at.unix_millis() > usage_cutoff);
}

fn advance_time(state: &mut DurableBudgetState, now: UtcTimestamp) -> Result<(), BudgetError> {
    if now < state.last_timestamp {
        return Err(BudgetError::Invalid(
            "budget clock cannot move backwards".into(),
        ));
    }
    state.last_timestamp = now;
    Ok(())
}

fn append_audit(
    state: &mut DurableBudgetState,
    operation_id: Option<EntityId>,
    occurred_at: UtcTimestamp,
    outcome: BudgetAuditOutcome,
    reason: &str,
) -> Result<(), BudgetError> {
    let reason = safe_reason(reason)?;
    let sequence = u64::try_from(state.audit.len())
        .ok()
        .and_then(|value| value.checked_add(1))
        .ok_or_else(|| BudgetError::Invalid("budget audit sequence overflow".into()))?;
    state.audit.push(BudgetAuditRecord {
        sequence,
        operation_id,
        occurred_at,
        outcome,
        reason,
    });
    Ok(())
}

fn safe_reason(reason: &str) -> Result<String, BudgetError> {
    let reason = reason.trim();
    if reason.is_empty()
        || reason.len() > MAX_REASON_BYTES
        || reason.chars().any(char::is_control)
        || ["api_key", "authorization", "password", "token="]
            .iter()
            .any(|marker| reason.to_ascii_lowercase().contains(marker))
    {
        return Err(BudgetError::Invalid(
            "budget reason is empty, private, or unbounded".into(),
        ));
    }
    Ok(reason.to_owned())
}

fn validate_identifier(value: &str) -> Result<(), BudgetError> {
    if value.is_empty()
        || value.len() > 256
        || !value
            .chars()
            .all(|character| character.is_ascii_alphanumeric() || "-_.:".contains(character))
    {
        return Err(BudgetError::Invalid("image identity is unsafe".into()));
    }
    Ok(())
}

fn validate_artifacts(root: &Path, artifacts: &[PathBuf]) -> Result<(), BudgetError> {
    if artifacts.len() > MAX_ARTIFACTS {
        return Err(BudgetError::Invalid("too many owned artifacts".into()));
    }
    let mut unique = BTreeSet::new();
    for relative in artifacts {
        if relative.as_os_str().is_empty()
            || relative.is_absolute()
            || relative.components().any(|component| {
                matches!(
                    component,
                    Component::ParentDir | Component::RootDir | Component::Prefix(_)
                )
            })
            || relative == Path::new(STATE_FILE)
            || relative == Path::new(LOCK_FILE)
            || !unique.insert(relative.clone())
        {
            return Err(BudgetError::Invalid("owned artifact path is unsafe".into()));
        }
        reject_symlink_components(&root.join(relative))?;
    }
    Ok(())
}

fn remove_owned(root: &Path, relative: &Path) -> Result<(), BudgetError> {
    validate_artifacts(root, &[relative.to_path_buf()])?;
    let path = root.join(relative);
    match fs::symlink_metadata(&path) {
        Ok(metadata) if metadata.file_type().is_symlink() => Err(BudgetError::Cleanup(
            "owned artifact became a symlink".into(),
        )),
        Ok(metadata) if metadata.is_dir() => fs::remove_dir_all(path).map_err(BudgetError::Io),
        Ok(_) => fs::remove_file(path).map_err(BudgetError::Io),
        Err(error) if error.kind() == std::io::ErrorKind::NotFound => Ok(()),
        Err(error) => Err(error.into()),
    }
}

#[cfg(unix)]
fn terminate_recovered_process(pid: u32, expected_start: Option<u64>) -> Result<(), BudgetError> {
    let raw = i32::try_from(pid)
        .map_err(|_| BudgetError::Cleanup("owned process identity is invalid".into()))?;
    let expected = expected_start.ok_or_else(|| {
        BudgetError::Cleanup("owned process has no durable start identity".into())
    })?;
    match process_start_ticks(pid) {
        Ok(actual) if actual != expected => return Ok(()),
        Err(error) if error.kind() == std::io::ErrorKind::NotFound => return Ok(()),
        Err(error) => return Err(BudgetError::Cleanup(error.to_string())),
        Ok(_) => {}
    }
    match killpg(Pid::from_raw(raw), Signal::SIGKILL) {
        Ok(()) | Err(Errno::ESRCH) => Ok(()),
        Err(error) => Err(BudgetError::Cleanup(error.to_string())),
    }
}

#[cfg(windows)]
fn terminate_recovered_process(pid: u32, expected_start: Option<u64>) -> Result<(), BudgetError> {
    let expected = expected_start.ok_or_else(|| {
        BudgetError::Cleanup("owned process has no durable start identity".into())
    })?;
    match process_start_ticks(pid) {
        Ok(actual) if actual != expected => return Ok(()),
        Err(error) if error.kind() == std::io::ErrorKind::NotFound => return Ok(()),
        Err(error) => return Err(BudgetError::Cleanup(error.to_string())),
        Ok(_) => {}
    }
    let status = Command::new("taskkill")
        .args(["/PID", &pid.to_string(), "/T", "/F"])
        .status()
        .map_err(|error| BudgetError::Cleanup(error.to_string()))?;
    if status.success()
        || matches!(
            process_start_ticks(pid),
            Err(error) if error.kind() == std::io::ErrorKind::NotFound
        )
    {
        Ok(())
    } else {
        Err(BudgetError::Cleanup(format!(
            "taskkill failed for the owned process with {status}"
        )))
    }
}

#[cfg(not(any(unix, windows)))]
fn terminate_recovered_process(_pid: u32, _expected_start: Option<u64>) -> Result<(), BudgetError> {
    Err(BudgetError::Cleanup(
        "this platform cannot safely reclaim an owned process".into(),
    ))
}

#[cfg(target_os = "linux")]
fn process_start_ticks(pid: u32) -> Result<u64, std::io::Error> {
    let stat = fs::read_to_string(format!("/proc/{pid}/stat"))?;
    let end = stat
        .rfind(')')
        .ok_or_else(|| std::io::Error::other("process stat has no command terminator"))?;
    stat.get(end + 2..)
        .and_then(|tail| tail.split_whitespace().nth(19))
        .ok_or_else(|| std::io::Error::other("process stat has no start time"))?
        .parse()
        .map_err(|_| std::io::Error::other("process start time is invalid"))
}

#[cfg(all(unix, not(target_os = "linux")))]
fn process_start_ticks(pid: u32) -> Result<u64, std::io::Error> {
    let output = Command::new("ps")
        .args(["-o", "lstart=", "-p", &pid.to_string()])
        .output()?;
    let identity = output.stdout;
    if !output.status.success() || identity.iter().all(u8::is_ascii_whitespace) {
        return Err(std::io::Error::new(
            std::io::ErrorKind::NotFound,
            "owned process is unavailable",
        ));
    }
    Ok(identity
        .into_iter()
        .fold(0xcbf2_9ce4_8422_2325, |hash, byte| {
            (hash ^ u64::from(byte)).wrapping_mul(0x0000_0100_0000_01b3)
        }))
}

#[cfg(windows)]
fn process_start_ticks(pid: u32) -> Result<u64, std::io::Error> {
    let script = format!(
        "$p = Get-Process -Id {pid} -ErrorAction SilentlyContinue; \
         if ($null -eq $p) {{ exit 3 }}; \
         [Console]::Out.Write($p.StartTime.ToUniversalTime().Ticks)"
    );
    let output = Command::new("powershell.exe")
        .args([
            "-NoLogo",
            "-NoProfile",
            "-NonInteractive",
            "-Command",
            &script,
        ])
        .output()?;
    if output.status.code() == Some(3) {
        return Err(std::io::Error::new(
            std::io::ErrorKind::NotFound,
            "owned process is unavailable",
        ));
    }
    if !output.status.success() {
        return Err(std::io::Error::other(
            "owned process start identity query failed",
        ));
    }
    String::from_utf8_lossy(&output.stdout)
        .trim()
        .parse()
        .map_err(|_| std::io::Error::other("owned process start identity is invalid"))
}

#[cfg(not(any(unix, windows)))]
fn process_start_ticks(_pid: u32) -> Result<u64, std::io::Error> {
    Err(std::io::Error::new(
        std::io::ErrorKind::Unsupported,
        "process start identity is unsupported",
    ))
}

fn load_state(path: &Path) -> Result<DurableBudgetState, BudgetError> {
    let metadata = fs::symlink_metadata(path)?;
    if !metadata.is_file() || metadata.file_type().is_symlink() || metadata.len() > MAX_STATE_BYTES
    {
        return Err(BudgetError::Invalid(
            "budget state is unsafe or oversized".into(),
        ));
    }
    Ok(serde_json::from_slice(&fs::read(path)?)?)
}

fn validate_state(root: &Path, state: &DurableBudgetState) -> Result<(), BudgetError> {
    state.ceilings.validate()?;
    if state.version != STATE_VERSION
        || state
            .operations
            .iter()
            .any(|(id, operation)| id != &operation.id)
        || state.audit.iter().enumerate().any(|(index, event)| {
            event.sequence != u64::try_from(index).unwrap_or(u64::MAX).saturating_add(1)
        })
    {
        return Err(BudgetError::Invalid("budget state is inconsistent".into()));
    }
    for operation in state.operations.values() {
        validate_artifacts(root, &operation.artifacts)?;
    }
    for kind in [
        EvolutionWorkKind::Hypothesis,
        EvolutionWorkKind::ShadowTree,
        EvolutionWorkKind::SandboxedBuild,
        EvolutionWorkKind::CanaryWorker,
    ] {
        let expected = u64::try_from(
            state
                .operations
                .values()
                .filter(|operation| operation.kind == kind)
                .count(),
        )
        .unwrap_or(u64::MAX);
        if state.active.get(&kind).copied().unwrap_or(0) != expected
            || expected > state.ceilings.concurrent_limit(kind).unwrap_or(0)
        {
            return Err(BudgetError::Invalid(
                "budget active counters do not match open operations".into(),
            ));
        }
    }
    if state.retained_images.len()
        > usize::try_from(state.ceilings.retained_images).unwrap_or(usize::MAX)
        || state
            .retained_images
            .iter()
            .any(|image| validate_identifier(image).is_err())
    {
        return Err(BudgetError::Invalid(
            "retained image accounting is invalid".into(),
        ));
    }
    Ok(())
}

fn persist_state(
    root: &Path,
    state_path: &Path,
    state: &DurableBudgetState,
) -> Result<(), BudgetError> {
    let bytes = canonical_json_bytes(state)?;
    if u64::try_from(bytes.len()).unwrap_or(u64::MAX) > MAX_STATE_BYTES {
        return Err(BudgetError::Invalid(
            "budget state exceeds its bound".into(),
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
    keith_platform::replace_file(&temporary, state_path)?;
    File::open(root)?.sync_all()?;
    Ok(())
}

fn reject_symlink(path: &Path) -> Result<(), BudgetError> {
    let metadata = fs::symlink_metadata(path)?;
    if metadata.file_type().is_symlink() || !metadata.is_dir() {
        return Err(BudgetError::Invalid("budget root is unsafe".into()));
    }
    Ok(())
}

fn reject_symlink_components(path: &Path) -> Result<(), BudgetError> {
    let mut current = PathBuf::new();
    for component in path.components() {
        current.push(component.as_os_str());
        match fs::symlink_metadata(&current) {
            Ok(metadata) if metadata.file_type().is_symlink() => {
                return Err(BudgetError::Invalid(
                    "owned artifact traverses a symlink".into(),
                ));
            }
            Ok(_) => {}
            Err(error) if error.kind() == std::io::ErrorKind::NotFound => break,
            Err(error) => return Err(error.into()),
        }
    }
    Ok(())
}

#[cfg(test)]
mod tests {
    use std::sync::{Arc, Barrier};
    use std::thread;
    use std::time::Duration;

    use tempfile::tempdir;

    use super::*;

    fn ceilings() -> EvolutionCeilings {
        EvolutionCeilings {
            concurrent_hypotheses: 2,
            concurrent_shadow_trees: 1,
            concurrent_sandboxed_builds: 1,
            concurrent_canary_workers: 1,
            promotions_per_interval: 2,
            promotion_interval_ms: 100,
            retained_images: 2,
            usage_interval_ms: 100,
            tokens: 10,
            model_cost_micros: 10,
            wall_time_ms: 10,
            cpu_time_ms: 10,
            memory_bytes: 10,
            disk_bytes: 10,
        }
    }

    fn origin() -> EvolutionOrigin {
        EvolutionOrigin {
            action_id: ActionId::new(),
            ancestors: Vec::new(),
            dedicated_child: false,
        }
    }

    fn process_is_running(pid: u32) -> bool {
        fs::read_to_string(format!("/proc/{pid}/stat"))
            .ok()
            .is_some_and(|stat| {
                stat.rfind(')').is_some_and(|end| {
                    stat.get(end + 2..)
                        .and_then(|tail| tail.split_whitespace().next())
                        .is_some_and(|state| state != "Z")
                })
            })
    }

    #[test]
    fn independent_ceilings_windows_and_retention_survive_restart() {
        let root = tempdir().unwrap();
        let budget =
            EvolutionBudget::open(root.path(), ceilings(), UtcTimestamp::UNIX_EPOCH).unwrap();
        let first = budget
            .admit(
                &origin(),
                EvolutionCapability::FormHypothesis,
                Vec::new(),
                UtcTimestamp::UNIX_EPOCH,
            )
            .unwrap();
        let second = budget
            .admit(
                &origin(),
                EvolutionCapability::FormHypothesis,
                Vec::new(),
                UtcTimestamp::UNIX_EPOCH,
            )
            .unwrap();
        assert!(matches!(
            budget.admit(
                &origin(),
                EvolutionCapability::FormHypothesis,
                Vec::new(),
                UtcTimestamp::UNIX_EPOCH
            ),
            Err(BudgetError::Exhausted(_))
        ));
        budget
            .complete(&first.operation_id, UtcTimestamp::UNIX_EPOCH)
            .unwrap();
        budget
            .complete(&second.operation_id, UtcTimestamp::UNIX_EPOCH)
            .unwrap();
        for _ in 0..2 {
            let promotion = budget
                .admit(
                    &origin(),
                    EvolutionCapability::PromoteCandidate,
                    Vec::new(),
                    UtcTimestamp::UNIX_EPOCH,
                )
                .unwrap();
            budget
                .complete(&promotion.operation_id, UtcTimestamp::UNIX_EPOCH)
                .unwrap();
        }
        assert!(matches!(
            budget.admit(
                &origin(),
                EvolutionCapability::PromoteCandidate,
                Vec::new(),
                UtcTimestamp::UNIX_EPOCH
            ),
            Err(BudgetError::Exhausted(_))
        ));
        budget
            .retain_image("image-a", UtcTimestamp::UNIX_EPOCH)
            .unwrap();
        budget
            .retain_image("image-b", UtcTimestamp::UNIX_EPOCH)
            .unwrap();
        assert!(matches!(
            budget.retain_image("image-c", UtcTimestamp::UNIX_EPOCH),
            Err(BudgetError::Exhausted(_))
        ));
        drop(budget);
        let recovered =
            EvolutionBudget::open(root.path(), ceilings(), UtcTimestamp::from_unix_millis(101))
                .unwrap();
        let promotion = recovered
            .admit(
                &origin(),
                EvolutionCapability::PromoteCandidate,
                Vec::new(),
                UtcTimestamp::from_unix_millis(101),
            )
            .unwrap();
        recovered
            .complete(&promotion.operation_id, UtcTimestamp::from_unix_millis(101))
            .unwrap();
        assert_eq!(recovered.retained_images().unwrap().len(), 2);
    }

    #[test]
    fn every_concurrency_and_usage_dimension_enforces_its_own_exact_ceiling() {
        let mut one = ceilings();
        one.concurrent_hypotheses = 1;
        for (capability, kind) in [
            (
                EvolutionCapability::FormHypothesis,
                EvolutionWorkKind::Hypothesis,
            ),
            (
                EvolutionCapability::StageShadowTree,
                EvolutionWorkKind::ShadowTree,
            ),
            (
                EvolutionCapability::RunSandboxedBuild,
                EvolutionWorkKind::SandboxedBuild,
            ),
            (
                EvolutionCapability::RunCanary,
                EvolutionWorkKind::CanaryWorker,
            ),
        ] {
            let root = tempdir().unwrap();
            let budget =
                EvolutionBudget::open(root.path(), one.clone(), UtcTimestamp::UNIX_EPOCH).unwrap();
            let admitted = budget
                .admit(&origin(), capability, Vec::new(), UtcTimestamp::UNIX_EPOCH)
                .unwrap();
            assert_eq!(admitted.kind, kind);
            assert!(matches!(
                budget.admit(&origin(), capability, Vec::new(), UtcTimestamp::UNIX_EPOCH),
                Err(BudgetError::Exhausted(_))
            ));
        }

        for usage in [
            EvolutionUsage {
                tokens: 10,
                ..EvolutionUsage::none()
            },
            EvolutionUsage {
                model_cost_micros: 10,
                ..EvolutionUsage::none()
            },
            EvolutionUsage {
                wall_time_ms: 10,
                ..EvolutionUsage::none()
            },
            EvolutionUsage {
                cpu_time_ms: 10,
                ..EvolutionUsage::none()
            },
            EvolutionUsage {
                memory_bytes: 10,
                ..EvolutionUsage::none()
            },
            EvolutionUsage {
                disk_bytes: 10,
                ..EvolutionUsage::none()
            },
        ] {
            let root = tempdir().unwrap();
            let budget =
                EvolutionBudget::open(root.path(), ceilings(), UtcTimestamp::UNIX_EPOCH).unwrap();
            let admitted = budget
                .admit(
                    &origin(),
                    EvolutionCapability::RunSandboxedBuild,
                    Vec::new(),
                    UtcTimestamp::UNIX_EPOCH,
                )
                .unwrap();
            budget
                .record_usage(&admitted.operation_id, usage, UtcTimestamp::UNIX_EPOCH)
                .unwrap();
            let exceeded = EvolutionUsage {
                tokens: u64::from(usage.tokens != 0),
                model_cost_micros: u64::from(usage.model_cost_micros != 0),
                wall_time_ms: u64::from(usage.wall_time_ms != 0),
                cpu_time_ms: u64::from(usage.cpu_time_ms != 0),
                memory_bytes: u64::from(usage.memory_bytes != 0),
                disk_bytes: u64::from(usage.disk_bytes != 0),
            };
            assert!(matches!(
                budget.record_usage(&admitted.operation_id, exceeded, UtcTimestamp::UNIX_EPOCH),
                Err(BudgetError::Exhausted(_))
            ));
        }

        let governor = ceilings().governor_ceilings();
        assert_eq!(
            governor
                .get(&(ResourceScope::Installation, ResourceKind::EvolutionBuilds))
                .unwrap()
                .maximum,
            1
        );
        assert_eq!(
            governor
                .get(&(ResourceScope::Installation, ResourceKind::CpuTimeMs))
                .unwrap()
                .maximum,
            10
        );
    }

    #[test]
    fn recursion_and_every_authority_widening_capability_are_refused_before_mutation() {
        let root = tempdir().unwrap();
        let budget =
            EvolutionBudget::open(root.path(), ceilings(), UtcTimestamp::UNIX_EPOCH).unwrap();
        let recursive = EvolutionOrigin {
            action_id: ActionId::new(),
            ancestors: vec![EvolutionActionAncestor {
                action_id: ActionId::new(),
                is_evolution: true,
            }],
            dedicated_child: false,
        };
        assert!(matches!(
            budget.admit(
                &recursive,
                EvolutionCapability::FormHypothesis,
                Vec::new(),
                UtcTimestamp::UNIX_EPOCH
            ),
            Err(BudgetError::RecursiveEvolution)
        ));
        let refused = [
            EvolutionCapability::ReplicateInstallation,
            EvolutionCapability::StartAdditionalDaemon,
            EvolutionCapability::InstallDaemon,
            EvolutionCapability::ModifyInstallation,
            EvolutionCapability::ModifyUpdateChannel,
            EvolutionCapability::EditBudget,
            EvolutionCapability::EditCeilings,
            EvolutionCapability::WidenPermission,
            EvolutionCapability::WidenSandbox,
            EvolutionCapability::AccessCredential,
            EvolutionCapability::WidenNetwork,
            EvolutionCapability::SpawnEvolutionChild,
        ];
        for capability in refused {
            assert!(matches!(
                budget.admit(
                    &origin(),
                    capability,
                    Vec::new(),
                    UtcTimestamp::UNIX_EPOCH
                ),
                Err(BudgetError::Refused(value)) if value == capability
            ));
        }
        assert!(fs::read_dir(root.path()).unwrap().all(|entry| {
            matches!(
                entry.unwrap().file_name().to_str(),
                Some(STATE_FILE | LOCK_FILE)
            )
        }));
    }

    #[test]
    #[cfg(unix)]
    fn recovered_process_cleanup_requires_a_matching_durable_identity() {
        let mut command = Command::new("sh");
        command.args(["-c", "while :; do sleep 1; done"]);
        configure_owned_process(&mut command);
        let mut child = command.spawn().unwrap();
        let pid = child.id();
        let identity = process_start_ticks(pid).unwrap();

        assert!(matches!(
            terminate_recovered_process(pid, None),
            Err(BudgetError::Cleanup(reason)) if reason.contains("start identity")
        ));
        assert!(child.try_wait().unwrap().is_none());
        terminate_recovered_process(pid, Some(identity.wrapping_add(1))).unwrap();
        assert!(child.try_wait().unwrap().is_none());

        terminate_owned_process_tree(&mut child).unwrap();
    }

    #[test]
    fn usage_exhaustion_kills_real_process_and_removes_only_owned_artifacts() {
        let root = tempdir().unwrap();
        let live_source = root.path().join("live-source");
        let image_pointer = root.path().join("current-image");
        fs::write(&live_source, b"known-good-source").unwrap();
        fs::write(&image_pointer, b"known-good-image").unwrap();
        let budget =
            EvolutionBudget::open(root.path(), ceilings(), UtcTimestamp::UNIX_EPOCH).unwrap();
        let admission = budget
            .admit(
                &origin(),
                EvolutionCapability::RunSandboxedBuild,
                vec![PathBuf::from("staged-build")],
                UtcTimestamp::UNIX_EPOCH,
            )
            .unwrap();
        fs::create_dir(root.path().join("staged-build")).unwrap();
        fs::write(root.path().join("staged-build/output"), b"partial").unwrap();
        let pid = budget
            .spawn_owned(
                &admission.operation_id,
                Command::new("sh").args(["-c", "while :; do sleep 1; done"]),
            )
            .unwrap();
        assert!(process_is_running(pid));
        assert!(matches!(
            budget.record_usage(
                &admission.operation_id,
                EvolutionUsage {
                    cpu_time_ms: 11,
                    ..EvolutionUsage::none()
                },
                UtcTimestamp::from_unix_millis(1)
            ),
            Err(BudgetError::Exhausted(_))
        ));
        for _ in 0..50 {
            if !process_is_running(pid) {
                break;
            }
            thread::sleep(Duration::from_millis(10));
        }
        assert!(!process_is_running(pid));
        assert!(!root.path().join("staged-build").exists());
        assert_eq!(fs::read(live_source).unwrap(), b"known-good-source");
        assert_eq!(fs::read(image_pointer).unwrap(), b"known-good-image");
        assert_eq!(budget.active(EvolutionWorkKind::SandboxedBuild).unwrap(), 0);
        assert!(budget.audit().unwrap().iter().any(|record| {
            record.outcome == BudgetAuditOutcome::Aborted && record.reason.contains("CpuTimeMs")
        }));
    }

    #[test]
    fn restart_reconciles_a_real_orphan_process_and_partial_artifact() {
        let root = tempdir().unwrap();
        let budget =
            EvolutionBudget::open(root.path(), ceilings(), UtcTimestamp::UNIX_EPOCH).unwrap();
        let admitted = budget
            .admit(
                &origin(),
                EvolutionCapability::RunCanary,
                vec![PathBuf::from("interrupted-canary")],
                UtcTimestamp::UNIX_EPOCH,
            )
            .unwrap();
        fs::create_dir(root.path().join("interrupted-canary")).unwrap();
        fs::write(root.path().join("interrupted-canary/manifest"), b"partial").unwrap();
        let pid = budget
            .spawn_owned(
                &admitted.operation_id,
                Command::new("sh").args(["-c", "while :; do sleep 1; done"]),
            )
            .unwrap();
        drop(budget);
        assert!(process_is_running(pid));

        let recovered =
            EvolutionBudget::open(root.path(), ceilings(), UtcTimestamp::from_unix_millis(1))
                .unwrap();
        for _ in 0..50 {
            if !process_is_running(pid) {
                break;
            }
            thread::sleep(Duration::from_millis(10));
        }
        assert!(!process_is_running(pid));
        assert!(!root.path().join("interrupted-canary").exists());
        assert_eq!(
            recovered.active(EvolutionWorkKind::CanaryWorker).unwrap(),
            0
        );
        assert!(
            recovered
                .audit()
                .unwrap()
                .iter()
                .any(|record| record.outcome == BudgetAuditOutcome::Reconciled)
        );
    }

    #[test]
    fn concurrent_admission_never_overshoots_the_durable_limit() {
        let root = tempdir().unwrap();
        let budget = Arc::new(
            EvolutionBudget::open(root.path(), ceilings(), UtcTimestamp::UNIX_EPOCH).unwrap(),
        );
        let barrier = Arc::new(Barrier::new(16));
        let admitted = thread::scope(|scope| {
            let handles = (0..16)
                .map(|_| {
                    let budget = Arc::clone(&budget);
                    let barrier = Arc::clone(&barrier);
                    scope.spawn(move || {
                        barrier.wait();
                        budget
                            .admit(
                                &origin(),
                                EvolutionCapability::StageShadowTree,
                                Vec::new(),
                                UtcTimestamp::UNIX_EPOCH,
                            )
                            .ok()
                    })
                })
                .collect::<Vec<_>>();
            handles
                .into_iter()
                .filter_map(|handle| handle.join().unwrap())
                .collect::<Vec<_>>()
        });
        assert_eq!(admitted.len(), 1);
        assert_eq!(budget.active(EvolutionWorkKind::ShadowTree).unwrap(), 1);
        budget
            .abort(
                &admitted[0].operation_id,
                "load test cleanup",
                UtcTimestamp::UNIX_EPOCH,
            )
            .unwrap();
    }

    #[test]
    fn reopening_refuses_budget_self_edit() {
        let root = tempdir().unwrap();
        drop(EvolutionBudget::open(root.path(), ceilings(), UtcTimestamp::UNIX_EPOCH).unwrap());
        let mut changed = ceilings();
        changed.tokens += 1;
        assert!(matches!(
            EvolutionBudget::open(root.path(), changed, UtcTimestamp::UNIX_EPOCH),
            Err(BudgetError::ImmutableCeilings)
        ));
    }
}
