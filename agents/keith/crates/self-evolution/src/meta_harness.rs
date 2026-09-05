#![allow(clippy::missing_errors_doc)]

use std::collections::{BTreeMap, BTreeSet};
use std::fs;
use std::path::{Component, Path};

use keith_agent_types::{EntityId, UtcTimestamp};
use keith_meta_harness::{
    CandidateDiffOperation, CandidatePopulation, HarnessCandidate, HarnessCandidateId,
    HarnessModeAvailability, HarnessOperationMode, HarnessPromotionError, HarnessPromotionRegistry,
    HarnessRecoveryAction, HarnessRepairOperation, HarnessRepairPhase, HarnessRepairProjection,
};
use keith_provider_core::CancellationToken;
use keith_state_store_core::EvolutionLedgerRepository;
use keith_supervisor::{ImageInstallRequest, ImageRegistryError, InstalledImage};
use keith_telemetry::{CandidateObservation, MetricName};
use sha2::{Digest, Sha256};
use thiserror::Error;

use crate::{
    BuildError, BuildVerdict, CanaryError, CanaryEvaluation, CanaryRunner, CanaryVerdict,
    EvolutionLedger, EvolutionProposal, EvolutionWorkRoot, GateFailure, GateResult, ImageError,
    ObservationRequest, ObservationSignal, PromotionError, PromotionRequest, PromotionRuntime,
    PromotionTransaction, ProposalEdit, ProposalError, ProposalLimits, ReversalAuthority,
    ReversalError, ReversalRequest, ReversalScope, ReversalTransaction, RevertWatchdog,
    ShadowError, ShadowTree, VerificationGate, WatchdogDecision, WatchdogError,
    WatchdogNotificationPolicy, WatchdogThresholds, WorkerImage,
};

#[derive(Debug, Error)]
pub enum MetaHarnessCoordinatorError {
    #[error("meta-harness promotion state failed: {0}")]
    State(#[from] HarnessPromotionError),
    #[error("candidate shadow failed: {0}")]
    Shadow(#[from] ShadowError),
    #[error("candidate proposal failed: {0}")]
    Proposal(#[from] ProposalError),
    #[error("candidate build failed: {0}")]
    Build(#[from] BuildError),
    #[error("candidate image registry failed: {0}")]
    Registry(#[from] ImageRegistryError),
    #[error("candidate image authentication failed: {0}")]
    Image(#[from] ImageError),
    #[error("candidate canary failed: {0}")]
    Canary(#[from] CanaryError),
    #[error("candidate promotion failed: {0}")]
    Promotion(#[from] PromotionError),
    #[error("candidate observation failed: {0}")]
    Watchdog(#[source] Box<WatchdogError>),
    #[error("candidate reversal failed: {0}")]
    Reversal(#[source] Box<ReversalError>),
    #[error("the admitted candidate does not match the staged source")]
    CandidateMismatch,
    #[error("the observation plan cannot attribute a live candidate worker")]
    MissingCandidateWorker,
    #[error("the durable recovery record does not match the promotion history")]
    RecoveryMismatch,
    #[error("candidate source inspection failed: {0}")]
    Io(#[from] std::io::Error),
}

impl From<WatchdogError> for MetaHarnessCoordinatorError {
    fn from(error: WatchdogError) -> Self {
        Self::Watchdog(Box::new(error))
    }
}

impl From<ReversalError> for MetaHarnessCoordinatorError {
    fn from(error: ReversalError) -> Self {
        Self::Reversal(Box::new(error))
    }
}

pub struct PreparedHarnessCandidate {
    pub operation: HarnessRepairOperation,
    pub shadow: ShadowTree,
    pub proposal: EvolutionProposal,
}

pub struct BuiltHarnessCandidate {
    pub operation: HarnessRepairOperation,
    pub shadow: ShadowTree,
    pub proposal: EvolutionProposal,
    pub image: WorkerImage,
    pub gate_results: Vec<GateResult>,
}

pub struct RejectedHarnessBuild {
    pub operation: HarnessRepairOperation,
    pub gate_results: Vec<GateResult>,
    pub failure: GateFailure,
}

pub enum HarnessBuildOutcome {
    Passed(Box<BuiltHarnessCandidate>),
    Rejected(Box<RejectedHarnessBuild>),
}

pub struct CanaryHarnessCandidate {
    pub operation: HarnessRepairOperation,
    pub shadow: ShadowTree,
    pub proposal: EvolutionProposal,
    pub image: WorkerImage,
    pub installed_image: InstalledImage,
    pub evaluation: CanaryEvaluation,
}

pub struct RejectedHarnessCanary {
    pub operation: HarnessRepairOperation,
    pub evaluation: CanaryEvaluation,
}

pub enum HarnessCanaryOutcome {
    Passed(Box<CanaryHarnessCandidate>),
    Rejected(Box<RejectedHarnessCanary>),
}

#[derive(Clone, Debug)]
pub struct HarnessObservationPlan {
    pub profile_id: EntityId,
    pub metric: MetricName,
    pub started_at: UtcTimestamp,
    pub deadline: UtcTimestamp,
    pub previous_image_retain_until: UtcTimestamp,
    pub thresholds: WatchdogThresholds,
    pub notification_policy: WatchdogNotificationPolicy,
    pub promotion_failure_threshold: usize,
}

/// Connects admitted Meta-Harness candidates to the existing durable self-evolution pipeline.
pub struct MetaHarnessCoordinator {
    registry: HarnessPromotionRegistry,
}

impl MetaHarnessCoordinator {
    /// Opens the durable promotion registry.
    ///
    /// # Errors
    ///
    /// Returns an error for unsafe or competing registry storage.
    pub fn open(
        root: impl AsRef<Path>,
        availability: HarnessModeAvailability,
    ) -> Result<Self, MetaHarnessCoordinatorError> {
        Ok(Self {
            registry: HarnessPromotionRegistry::open(root, availability)?,
        })
    }

    pub fn admit(
        &self,
        population: &CandidatePopulation,
        candidate_id: &HarnessCandidateId,
        mode: HarnessOperationMode,
        now: UtcTimestamp,
    ) -> Result<HarnessRepairOperation, MetaHarnessCoordinatorError> {
        self.registry
            .admit(population, candidate_id, mode, now)
            .map_err(Into::into)
    }

    pub fn approve(
        &self,
        operation_id: &EntityId,
        acting_identity: String,
        now: UtcTimestamp,
    ) -> Result<HarnessRepairOperation, MetaHarnessCoordinatorError> {
        self.registry
            .approve(operation_id, acting_identity, now)
            .map_err(Into::into)
    }

    pub fn projections(&self) -> Result<Vec<HarnessRepairProjection>, MetaHarnessCoordinatorError> {
        self.registry.projections().map_err(Into::into)
    }

    pub fn operation(
        &self,
        operation_id: &EntityId,
    ) -> Result<HarnessRepairOperation, MetaHarnessCoordinatorError> {
        self.registry.operation(operation_id).map_err(Into::into)
    }

    /// Stages the exact admitted diff into a commit-exact self-evolution shadow.
    ///
    /// The full pre-edit and post-edit tree digests must match the evaluated candidate. A caller
    /// cannot substitute another revision or patch after admission.
    pub fn prepare_candidate(
        &self,
        operation_id: &EntityId,
        population: &CandidatePopulation,
        source_repository: &Path,
        requested_revision: &str,
        work_root: &EvolutionWorkRoot,
        now: UtcTimestamp,
    ) -> Result<PreparedHarnessCandidate, MetaHarnessCoordinatorError> {
        let operation = self.registry.operation(operation_id)?;
        let candidate = candidate_for_operation(&operation, population)?;
        let operation = self.registry.begin_build(operation_id, now)?;
        let shadow = ShadowTree::stage(source_repository, requested_revision, work_root)?;
        if candidate_tree_digest(shadow.root())? != candidate.base_source_digest {
            let _ = self.registry.record_build(
                operation_id,
                None,
                Some("The admitted repair was based on a different source revision.".into()),
                now,
            );
            shadow.reclaim()?;
            return Err(MetaHarnessCoordinatorError::CandidateMismatch);
        }
        let edits = proposal_edits(candidate)?;
        let proposal = match shadow.apply_proposal(
            bounded_summary(operation.hypothesis.as_str()),
            &edits,
            ProposalLimits::default(),
            None,
        ) {
            Ok(proposal) => proposal,
            Err(error) => {
                let _ = self.registry.record_build(
                    operation_id,
                    None,
                    Some("The admitted repair could not be applied to its isolated source.".into()),
                    now,
                );
                let _ = shadow.reclaim();
                return Err(error.into());
            }
        };
        if candidate_tree_digest(shadow.root())? != operation.candidate_digest {
            let _ = self.registry.record_build(
                operation_id,
                None,
                Some("The staged repair did not match its evaluated content.".into()),
                now,
            );
            shadow.reclaim()?;
            return Err(MetaHarnessCoordinatorError::CandidateMismatch);
        }
        Ok(PreparedHarnessCandidate {
            operation,
            shadow,
            proposal,
        })
    }

    /// Runs the complete real `VerificationGate` and accepts only its signed image.
    pub fn build_candidate(
        &self,
        prepared: PreparedHarnessCandidate,
        gate: &VerificationGate,
        cancellation: &CancellationToken,
        now: UtcTimestamp,
    ) -> Result<HarnessBuildOutcome, MetaHarnessCoordinatorError> {
        match gate.verify(&prepared.shadow, &prepared.proposal, cancellation) {
            Ok(BuildVerdict::Passed { results, image }) => {
                let image_id = image_id(&image)?;
                let operation = self.registry.record_build(
                    &prepared.operation.id,
                    Some(image_id),
                    None,
                    now,
                )?;
                Ok(HarnessBuildOutcome::Passed(Box::new(
                    BuiltHarnessCandidate {
                        operation,
                        shadow: prepared.shadow,
                        proposal: prepared.proposal,
                        image,
                        gate_results: results,
                    },
                )))
            }
            Ok(BuildVerdict::Failed { results, failure }) => {
                let operation = self.registry.record_build(
                    &prepared.operation.id,
                    None,
                    Some("The real build gate rejected the repair. Nothing was promoted.".into()),
                    now,
                )?;
                prepared.shadow.reclaim()?;
                Ok(HarnessBuildOutcome::Rejected(Box::new(
                    RejectedHarnessBuild {
                        operation,
                        gate_results: results,
                        failure,
                    },
                )))
            }
            Err(error) => {
                let _ = self.registry.record_build(
                    &prepared.operation.id,
                    None,
                    Some("The isolated build could not complete. Nothing was promoted.".into()),
                    now,
                );
                Err(error.into())
            }
        }
    }

    /// Installs the signed bytes without selecting them and runs the exact candidate process in
    /// the existing `CanaryRunner`.
    #[allow(clippy::too_many_arguments)]
    pub fn run_canary<R: PromotionRuntime>(
        &self,
        built: BuiltHarnessCandidate,
        runner: &CanaryRunner,
        runtime: &mut R,
        trusted_public_key: &[u8; 32],
        metric: MetricName,
        baseline: f64,
        target_threshold: f64,
        now: UtcTimestamp,
    ) -> Result<HarnessCanaryOutcome, MetaHarnessCoordinatorError> {
        self.registry.begin_canary(&built.operation.id, now)?;
        built.image.verify(trusted_public_key)?;
        let manifest = built.image.manifest_bytes()?;
        let installed = runtime
            .registry_mut()
            .install_verified(&ImageInstallRequest {
                manifest: &manifest,
                signature: built.image.signature(),
                executable: built.image.executable(),
                trusted_public_key,
            })?;
        if Some(installed.image_id.as_str()) != built.operation.build_image_id.as_deref() {
            self.registry.record_canary(
                &built.operation.id,
                false,
                Some("The installed canary image did not match the signed build.".into()),
                now,
            )?;
            built.shadow.reclaim()?;
            return Err(MetaHarnessCoordinatorError::CandidateMismatch);
        }
        let evaluation = match runner.evaluate(&installed, metric, baseline, target_threshold) {
            Ok(evaluation) => evaluation,
            Err(error) => {
                let _ = self.registry.record_canary(
                    &built.operation.id,
                    false,
                    Some(
                        "The isolated canary could not complete. Existing workers were unchanged."
                            .into(),
                    ),
                    now,
                );
                return Err(error.into());
            }
        };
        let passed = matches!(evaluation.verdict, CanaryVerdict::Passed);
        let operation = self.registry.record_canary(
            &built.operation.id,
            passed,
            (!passed)
                .then(|| "The canary rejected the repair. Existing workers were unchanged.".into()),
            now,
        )?;
        if passed {
            Ok(HarnessCanaryOutcome::Passed(Box::new(
                CanaryHarnessCandidate {
                    operation,
                    shadow: built.shadow,
                    proposal: built.proposal,
                    image: built.image,
                    installed_image: installed,
                    evaluation,
                },
            )))
        } else {
            built.shadow.reclaim()?;
            Ok(HarnessCanaryOutcome::Rejected(Box::new(
                RejectedHarnessCanary {
                    operation,
                    evaluation,
                },
            )))
        }
    }

    /// Promotes through the existing transaction, records the durable relationship, and opens the
    /// existing watchdog window before task retry can rely on autonomous promotion.
    #[allow(clippy::too_many_arguments)]
    pub fn promote_and_observe<R, L>(
        &self,
        candidate: CanaryHarnessCandidate,
        transaction: &PromotionTransaction,
        watchdog: &mut RevertWatchdog,
        runtime: &mut R,
        ledger: &EvolutionLedger<L>,
        trusted_public_key: &[u8; 32],
        plan: &HarnessObservationPlan,
        now: UtcTimestamp,
    ) -> Result<HarnessRepairOperation, MetaHarnessCoordinatorError>
    where
        R: PromotionRuntime,
        L: EvolutionLedgerRepository,
    {
        let workers_before = runtime.active_workers();
        if workers_before.is_empty() {
            return Err(MetaHarnessCoordinatorError::MissingCandidateWorker);
        }
        let prior_known_good = runtime.registry().known_good().image_id.clone();
        self.registry
            .begin_promotion(&candidate.operation.id, now)?;
        let outcome = transaction.promote(
            runtime,
            ledger,
            PromotionRequest {
                hypothesis_id: candidate.operation.diagnosis_id.clone(),
                occurred_at: now,
                image: &candidate.image,
                trusted_public_key,
                canary: &candidate.evaluation,
                proposal: &candidate.proposal,
                shadow_root: candidate.shadow.root(),
                failure_threshold: plan.promotion_failure_threshold,
            },
        )?;
        let workers = candidate_workers(&outcome.rolls)?;
        let operation = self.registry.record_observing(
            &candidate.operation.id,
            outcome.transaction_id.clone(),
            prior_known_good.clone(),
            now,
        )?;
        watchdog.start(observation_request(
            &operation,
            outcome.transaction_id,
            prior_known_good,
            workers,
            plan,
        )?)?;
        candidate.shadow.reclaim()?;
        Ok(operation)
    }

    /// Reconciles a crash-interrupted promotion using the authoritative transaction journal and
    /// committed history, then reconstructs the watchdog attribution from current live workers.
    #[allow(clippy::too_many_arguments)]
    pub fn recover_promotion<R, L>(
        &self,
        operation_id: &EntityId,
        transaction: &PromotionTransaction,
        watchdog: &mut RevertWatchdog,
        runtime: &mut R,
        ledger: &EvolutionLedger<L>,
        plan: &HarnessObservationPlan,
        now: UtcTimestamp,
    ) -> Result<HarnessRepairOperation, MetaHarnessCoordinatorError>
    where
        R: PromotionRuntime,
        L: EvolutionLedgerRepository,
    {
        let operation = self.registry.operation(operation_id)?;
        if operation.recovery_action() != HarnessRecoveryAction::RecoverPromotion {
            return Err(HarnessPromotionError::InvalidTransition.into());
        }
        if transaction.recover(runtime, ledger)? {
            return self
                .registry
                .record_failed(
                    operation_id,
                    "An interrupted promotion was safely restored to the prior version.".into(),
                    now,
                )
                .map_err(Into::into);
        }
        let image_id = operation
            .build_image_id
            .as_deref()
            .ok_or(MetaHarnessCoordinatorError::RecoveryMismatch)?;
        let record = transaction
            .history()?
            .into_iter()
            .rev()
            .find(|record| {
                record.hypothesis_id == operation.diagnosis_id
                    && record.candidate_image_id == image_id
            })
            .ok_or(MetaHarnessCoordinatorError::RecoveryMismatch)?;
        let workers = live_candidate_workers(runtime, image_id)?;
        let operation = self.registry.record_observing(
            operation_id,
            record.transaction_id.clone(),
            record.prior_image_id.clone(),
            now,
        )?;
        watchdog.start(observation_request(
            &operation,
            record.transaction_id,
            record.prior_image_id,
            workers,
            plan,
        )?)?;
        Ok(operation)
    }

    /// Reopens a missing watchdog window after a crash that happened after the promotion record
    /// became durable but before the observation window did.
    pub fn resume_observation<R: PromotionRuntime>(
        &self,
        operation_id: &EntityId,
        watchdog: &mut RevertWatchdog,
        runtime: &R,
        plan: &HarnessObservationPlan,
    ) -> Result<HarnessRepairOperation, MetaHarnessCoordinatorError> {
        let operation = self.registry.operation(operation_id)?;
        if operation.phase != HarnessRepairPhase::Observing {
            return Err(HarnessPromotionError::InvalidTransition.into());
        }
        if let Some(active) = watchdog.active() {
            if operation.promotion_id.as_ref() == Some(&active.promotion_id)
                && operation.build_image_id.as_deref() == Some(&active.candidate_image_id)
            {
                return Ok(operation);
            }
            return Err(MetaHarnessCoordinatorError::RecoveryMismatch);
        }
        let image_id = operation
            .build_image_id
            .as_deref()
            .ok_or(MetaHarnessCoordinatorError::RecoveryMismatch)?;
        let promotion_id = operation
            .promotion_id
            .clone()
            .ok_or(MetaHarnessCoordinatorError::RecoveryMismatch)?;
        let prior = operation
            .prior_known_good_image_id
            .clone()
            .ok_or(MetaHarnessCoordinatorError::RecoveryMismatch)?;
        watchdog.start(observation_request(
            &operation,
            promotion_id,
            prior,
            live_candidate_workers(runtime, image_id)?,
            plan,
        )?)?;
        Ok(operation)
    }

    #[allow(clippy::too_many_arguments)]
    pub fn observe_candidate<R, L>(
        &self,
        operation_id: &EntityId,
        watchdog: &mut RevertWatchdog,
        runtime: &mut R,
        ledger: &EvolutionLedger<L>,
        trusted_public_key: &[u8; 32],
        observation: CandidateObservation,
        now: UtcTimestamp,
    ) -> Result<WatchdogDecision, MetaHarnessCoordinatorError>
    where
        R: PromotionRuntime,
        L: EvolutionLedgerRepository,
    {
        let decision = watchdog.observe_candidate(observation)?;
        self.apply_observation_decision(
            operation_id,
            watchdog,
            runtime,
            ledger,
            trusted_public_key,
            decision,
            now,
        )
    }

    #[allow(clippy::too_many_arguments)]
    pub fn observe_signal<R, L>(
        &self,
        operation_id: &EntityId,
        watchdog: &mut RevertWatchdog,
        runtime: &mut R,
        ledger: &EvolutionLedger<L>,
        trusted_public_key: &[u8; 32],
        signal: ObservationSignal,
        observed_at: UtcTimestamp,
    ) -> Result<WatchdogDecision, MetaHarnessCoordinatorError>
    where
        R: PromotionRuntime,
        L: EvolutionLedgerRepository,
    {
        let decision = watchdog.observe(signal, observed_at)?;
        self.apply_observation_decision(
            operation_id,
            watchdog,
            runtime,
            ledger,
            trusted_public_key,
            decision,
            observed_at,
        )
    }

    pub fn tick_observation<R, L>(
        &self,
        operation_id: &EntityId,
        watchdog: &mut RevertWatchdog,
        runtime: &mut R,
        ledger: &EvolutionLedger<L>,
        trusted_public_key: &[u8; 32],
        now: UtcTimestamp,
    ) -> Result<WatchdogDecision, MetaHarnessCoordinatorError>
    where
        R: PromotionRuntime,
        L: EvolutionLedgerRepository,
    {
        let decision = watchdog.tick(now)?;
        self.apply_observation_decision(
            operation_id,
            watchdog,
            runtime,
            ledger,
            trusted_public_key,
            decision,
            now,
        )
    }

    #[allow(clippy::too_many_arguments)]
    pub fn apply_watchdog_reversal<R, L>(
        &self,
        operation_id: &EntityId,
        watchdog: &mut RevertWatchdog,
        reversal: &ReversalTransaction,
        runtime: &mut R,
        ledger: &EvolutionLedger<L>,
        trusted_public_key: &[u8; 32],
        authority: &ReversalAuthority,
        now: UtcTimestamp,
    ) -> Result<WatchdogDecision, MetaHarnessCoordinatorError>
    where
        R: PromotionRuntime,
        L: EvolutionLedgerRepository,
    {
        if self.registry.operation(operation_id)?.phase != HarnessRepairPhase::ReversalRequired {
            return Err(HarnessPromotionError::InvalidTransition.into());
        }
        self.registry.begin_reversal(operation_id, now)?;
        let decision = watchdog.apply_revert(
            reversal,
            runtime,
            ledger,
            trusted_public_key,
            authority,
            now,
        )?;
        if matches!(decision, WatchdogDecision::Reverted(_)) {
            self.registry.record_reverted(operation_id, now)?;
        }
        Ok(decision)
    }

    #[allow(clippy::too_many_arguments)]
    pub fn reverse<R, L>(
        &self,
        operation_id: &EntityId,
        reversal: &ReversalTransaction,
        runtime: &mut R,
        ledger: &EvolutionLedger<L>,
        trusted_public_key: &[u8; 32],
        authority: &ReversalAuthority,
        safe_reason: &str,
        now: UtcTimestamp,
    ) -> Result<HarnessRepairOperation, MetaHarnessCoordinatorError>
    where
        R: PromotionRuntime,
        L: EvolutionLedgerRepository,
    {
        if self.registry.operation(operation_id)?.phase != HarnessRepairPhase::Promoted {
            return Err(HarnessPromotionError::InvalidTransition.into());
        }
        let operation = self.registry.begin_reversal(operation_id, now)?;
        let promotion_id = operation
            .promotion_id
            .clone()
            .ok_or(MetaHarnessCoordinatorError::RecoveryMismatch)?;
        reversal.reverse(
            runtime,
            ledger,
            ReversalRequest {
                scope: ReversalScope::Promotion(promotion_id),
                trusted_public_key,
                authority,
                reason: safe_reason,
                occurred_at: now,
            },
        )?;
        self.registry
            .record_reverted(operation_id, now)
            .map_err(Into::into)
    }

    pub fn recover_reversal<R, L>(
        &self,
        operation_id: &EntityId,
        reversal: &ReversalTransaction,
        runtime: &mut R,
        ledger: &EvolutionLedger<L>,
        now: UtcTimestamp,
    ) -> Result<HarnessRepairOperation, MetaHarnessCoordinatorError>
    where
        R: PromotionRuntime,
        L: EvolutionLedgerRepository,
    {
        let operation = self.registry.operation(operation_id)?;
        if operation.recovery_action() != HarnessRecoveryAction::RecoverReversal {
            return Err(HarnessPromotionError::InvalidTransition.into());
        }
        reversal
            .recover(runtime, ledger, now)?
            .ok_or(MetaHarnessCoordinatorError::RecoveryMismatch)?;
        self.registry
            .record_reverted(operation_id, now)
            .map_err(Into::into)
    }

    #[allow(clippy::too_many_arguments)]
    fn apply_observation_decision<R, L>(
        &self,
        operation_id: &EntityId,
        watchdog: &mut RevertWatchdog,
        runtime: &mut R,
        ledger: &EvolutionLedger<L>,
        trusted_public_key: &[u8; 32],
        decision: WatchdogDecision,
        now: UtcTimestamp,
    ) -> Result<WatchdogDecision, MetaHarnessCoordinatorError>
    where
        R: PromotionRuntime,
        L: EvolutionLedgerRepository,
    {
        match decision {
            WatchdogDecision::RevertRequired(reason) => {
                self.registry
                    .require_reversal(operation_id, reason.clone(), now)?;
                Ok(WatchdogDecision::RevertRequired(reason))
            }
            WatchdogDecision::AdvanceKnownGood => {
                let advanced =
                    watchdog.advance_known_good(runtime, ledger, trusted_public_key, now)?;
                if advanced == WatchdogDecision::KnownGood {
                    self.registry.record_known_good(operation_id, now)?;
                }
                Ok(advanced)
            }
            other => Ok(other),
        }
    }
}

fn candidate_for_operation<'a>(
    operation: &HarnessRepairOperation,
    population: &'a CandidatePopulation,
) -> Result<&'a HarnessCandidate, MetaHarnessCoordinatorError> {
    if population.id != operation.population_id || population.diagnosis_id != operation.diagnosis_id
    {
        return Err(MetaHarnessCoordinatorError::CandidateMismatch);
    }
    let candidate = population
        .candidates
        .iter()
        .find(|candidate| candidate.id == operation.candidate_id)
        .ok_or(MetaHarnessCoordinatorError::CandidateMismatch)?;
    if candidate.candidate_digest != operation.candidate_digest
        || !population.frontier.candidate_ids.contains(&candidate.id)
    {
        return Err(MetaHarnessCoordinatorError::CandidateMismatch);
    }
    Ok(candidate)
}

fn proposal_edits(
    candidate: &HarnessCandidate,
) -> Result<Vec<ProposalEdit>, MetaHarnessCoordinatorError> {
    let mut snapshots = candidate
        .source_snapshot
        .iter()
        .map(|snapshot| (snapshot.relative_path.clone(), snapshot))
        .collect::<BTreeMap<_, _>>();
    if snapshots.len() != candidate.source_snapshot.len() {
        return Err(MetaHarnessCoordinatorError::CandidateMismatch);
    }
    let mut edits = Vec::with_capacity(candidate.diff.len());
    for entry in &candidate.diff {
        validate_relative(&entry.relative_path)?;
        match entry.operation {
            CandidateDiffOperation::Write => {
                let snapshot = snapshots
                    .remove(&entry.relative_path)
                    .ok_or(MetaHarnessCoordinatorError::CandidateMismatch)?;
                let bytes = snapshot.source.as_bytes().to_vec();
                if entry.resulting_digest.as_deref() != Some(snapshot.source_digest.as_str())
                    || entry.resulting_bytes != u64::try_from(bytes.len()).unwrap_or(u64::MAX)
                    || digest_bytes(&bytes) != snapshot.source_digest
                {
                    return Err(MetaHarnessCoordinatorError::CandidateMismatch);
                }
                edits.push(ProposalEdit::Write {
                    path: entry.relative_path.clone(),
                    bytes,
                });
            }
            CandidateDiffOperation::Delete => {
                if entry.resulting_digest.is_some()
                    || entry.resulting_bytes != 0
                    || snapshots.contains_key(&entry.relative_path)
                {
                    return Err(MetaHarnessCoordinatorError::CandidateMismatch);
                }
                edits.push(ProposalEdit::Delete {
                    path: entry.relative_path.clone(),
                });
            }
        }
    }
    if !snapshots.is_empty() || edits.is_empty() {
        return Err(MetaHarnessCoordinatorError::CandidateMismatch);
    }
    Ok(edits)
}

fn candidate_tree_digest(root: &Path) -> Result<String, MetaHarnessCoordinatorError> {
    let mut hasher = Sha256::new();
    digest_tree_inner(root, root, &mut hasher)?;
    Ok(format!("sha256:{:x}", hasher.finalize()))
}

fn digest_tree_inner(
    root: &Path,
    directory: &Path,
    hasher: &mut Sha256,
) -> Result<(), MetaHarnessCoordinatorError> {
    let mut entries = fs::read_dir(directory)?.collect::<Result<Vec<_>, _>>()?;
    entries.sort_by_key(std::fs::DirEntry::file_name);
    for entry in entries {
        let path = entry.path();
        let metadata = fs::symlink_metadata(&path)?;
        if metadata.file_type().is_symlink() {
            return Err(MetaHarnessCoordinatorError::CandidateMismatch);
        }
        let relative = path
            .strip_prefix(root)
            .map_err(|_| MetaHarnessCoordinatorError::CandidateMismatch)?;
        hasher.update(relative.to_string_lossy().as_bytes());
        if metadata.is_dir() {
            hasher.update(b"directory\0");
            digest_tree_inner(root, &path, hasher)?;
        } else if metadata.is_file() {
            hasher.update(b"file\0");
            hasher.update(fs::read(path)?);
        } else {
            return Err(MetaHarnessCoordinatorError::CandidateMismatch);
        }
    }
    Ok(())
}

fn digest_bytes(bytes: &[u8]) -> String {
    format!("sha256:{:x}", Sha256::digest(bytes))
}

fn bounded_summary(value: &str) -> String {
    let mut bytes = 0_usize;
    value
        .chars()
        .take_while(|character| {
            let next = bytes.saturating_add(character.len_utf8());
            if next > 512 {
                false
            } else {
                bytes = next;
                true
            }
        })
        .collect()
}

fn validate_relative(path: &Path) -> Result<(), MetaHarnessCoordinatorError> {
    if path.as_os_str().is_empty()
        || path.is_absolute()
        || path
            .components()
            .any(|component| !matches!(component, Component::Normal(_)))
    {
        return Err(MetaHarnessCoordinatorError::CandidateMismatch);
    }
    Ok(())
}

fn image_id(image: &WorkerImage) -> Result<String, MetaHarnessCoordinatorError> {
    Ok(format!("{:x}", Sha256::digest(image.manifest_bytes()?)))
}

fn candidate_workers(
    rolls: &[crate::WorkerRoll],
) -> Result<
    BTreeSet<(keith_agent_types::RootTreeId, keith_agent_types::Generation)>,
    MetaHarnessCoordinatorError,
> {
    let workers = rolls
        .iter()
        .map(|roll| {
            roll.candidate_generation
                .map(|generation| (roll.root_tree_id.clone(), generation))
                .ok_or(MetaHarnessCoordinatorError::MissingCandidateWorker)
        })
        .collect::<Result<BTreeSet<_>, _>>()?;
    if workers.is_empty() {
        return Err(MetaHarnessCoordinatorError::MissingCandidateWorker);
    }
    Ok(workers)
}

fn live_candidate_workers<R: PromotionRuntime>(
    runtime: &R,
    image_id: &str,
) -> Result<
    BTreeSet<(keith_agent_types::RootTreeId, keith_agent_types::Generation)>,
    MetaHarnessCoordinatorError,
> {
    let workers = runtime
        .active_workers()
        .into_iter()
        .filter(|status| status.image_id == image_id)
        .map(|status| (status.root_tree_id, status.generation))
        .collect::<BTreeSet<_>>();
    if workers.is_empty() {
        return Err(MetaHarnessCoordinatorError::MissingCandidateWorker);
    }
    Ok(workers)
}

fn observation_request(
    operation: &HarnessRepairOperation,
    promotion_id: EntityId,
    prior_known_good_image_id: String,
    candidate_workers: BTreeSet<(keith_agent_types::RootTreeId, keith_agent_types::Generation)>,
    plan: &HarnessObservationPlan,
) -> Result<ObservationRequest, MetaHarnessCoordinatorError> {
    let candidate_image_id = operation
        .build_image_id
        .clone()
        .ok_or(MetaHarnessCoordinatorError::RecoveryMismatch)?;
    Ok(ObservationRequest {
        promotion_id,
        hypothesis_id: operation.diagnosis_id.clone(),
        profile_id: plan.profile_id.clone(),
        candidate_image_id,
        prior_known_good_image_id,
        candidate_workers,
        hypothesis_metric: plan.metric,
        started_at: plan.started_at,
        deadline: plan.deadline,
        previous_image_retain_until: plan.previous_image_retain_until,
        thresholds: plan.thresholds.clone(),
        notification_policy: plan.notification_policy,
    })
}

#[cfg(test)]
mod tests {
    use std::fs;
    use std::path::{Path, PathBuf};
    use std::process::Command;

    use keith_meta_harness::RedactedText;
    use keith_meta_harness::{
        CandidateEdit, CandidateExecutionRequest, CandidateExecutor, CandidateLimits,
        CandidatePolicy, CandidateProposal, CandidateRegistry, CandidateRun, CandidateSourceKind,
        EvaluationCase, EvaluationCostCeiling, EvaluationDataset, FailureAttribution,
        HarnessDiagnosis, HarnessFailureClass, HarnessFaultCategory, IndependentEvaluator,
        MetricDirection, RegressionBounds, TargetMetric,
    };

    use super::*;

    struct ProcessExecutor;

    impl CandidateExecutor for ProcessExecutor {
        fn execute(
            &mut self,
            request: CandidateExecutionRequest<'_>,
        ) -> Result<CandidateRun, RedactedText> {
            let input = std::str::from_utf8(request.input)
                .map_err(|_| text("the reproduction input was invalid"))?;
            let output = compile_and_run(&request.shadow_root.join("harness/router.rs"), input)
                .map_err(|_| text("the reproduction process could not start"))?;
            Ok(CandidateRun {
                output: output.stdout,
                claimed_success: output.status.success(),
                unsafe_action_count: 0,
                correction_followed: true,
                tokens: 1,
                external_cost_micros: 0,
                latency_ms: 1,
                retries: 0,
                cpu_ms: 1,
                peak_memory_bytes: 1_024,
                disk_bytes: 1_024,
            })
        }
    }

    #[test]
    #[allow(clippy::too_many_lines)]
    fn deliberate_harness_defect_fails_then_repaired_shadow_passes_without_changing_original() {
        let directory = tempfile::tempdir().expect("temporary journey root");
        let source = directory.path().join("source-export");
        fs::create_dir_all(source.join("harness")).expect("source directory");
        let defective = "fn main() { let value = std::env::args().nth(1).unwrap(); println!(\"misrouted:{value}\"); std::process::exit(1); }\n";
        let repaired = "fn main() { let value = std::env::args().nth(1).unwrap(); println!(\"routed:{value}\"); }\n";
        fs::write(source.join("harness/router.rs"), defective).expect("defective harness");

        let repository = directory.path().join("repository");
        fs::create_dir(&repository).expect("repository directory");
        copy_tree(&source, &repository);
        git(&repository, &["init", "-q"]);
        git(&repository, &["add", "."]);
        git(
            &repository,
            &[
                "-c",
                "user.name=meta-harness-test",
                "-c",
                "user.email=meta-harness@example.invalid",
                "commit",
                "-qm",
                "deliberate defect",
            ],
        );
        let revision = command_text(&repository, &["rev-parse", "HEAD"]);

        let failed = compile_and_run(&source.join("harness/router.rs"), "request")
            .expect("run deliberate defect");
        assert!(!failed.status.success());
        assert_eq!(failed.stdout, b"misrouted:request\n");

        let candidate_registry = CandidateRegistry::open(
            directory.path().join("candidate-state"),
            CandidateLimits::default(),
            CandidatePolicy::new([PathBuf::from("harness")]).expect("candidate policy"),
        )
        .expect("candidate registry");
        let diagnosis = diagnosis();
        let population = candidate_registry
            .create_population(
                &source,
                &diagnosis,
                vec![CandidateProposal {
                    parent_id: None,
                    source: CandidateSourceKind::Proposed,
                    hypothesis: text("route the complete request context"),
                    edits: vec![CandidateEdit::Write {
                        relative_path: PathBuf::from("harness/router.rs"),
                        expected_digest: None,
                        contents: repaired.into(),
                    }],
                    trace_references: vec![text("deliberate-defect-process")],
                    safe_trace_excerpts: vec![text("the harness routed the request incorrectly")],
                    proposal_tokens: 12,
                    estimated_latency_ms: 1,
                    estimated_external_cost_micros: 0,
                }],
                UtcTimestamp::from_unix_millis(2),
            )
            .expect("candidate population");
        let evaluator_root = directory.path().join("operator-evaluator");
        fs::create_dir(&evaluator_root).expect("evaluator root");
        fs::write(evaluator_root.join("version"), "operator-owned-v1").expect("evaluator version");
        let evaluator = evaluator(&evaluator_root);
        let evaluated = candidate_registry
            .evaluate_population(
                &population.id,
                &evaluator,
                &mut ProcessExecutor,
                UtcTimestamp::from_unix_millis(3),
            )
            .expect("real process evaluation");
        let admitted_id = evaluated
            .frontier
            .candidate_ids
            .iter()
            .find(|candidate_id| {
                evaluated.candidates.iter().any(|candidate| {
                    candidate.id == **candidate_id
                        && candidate.source != CandidateSourceKind::Baseline
                })
            })
            .cloned()
            .expect("repaired frontier candidate");

        let coordinator = MetaHarnessCoordinator::open(
            directory.path().join("promotion-state"),
            HarnessModeAvailability::fully_available(),
        )
        .expect("coordinator");
        let operation = coordinator
            .admit(
                &evaluated,
                &admitted_id,
                HarnessOperationMode::Shadow,
                UtcTimestamp::from_unix_millis(4),
            )
            .expect("admitted repair");
        let work_root_path = directory.path().join("evolution-work");
        fs::create_dir(&work_root_path).expect("work root");
        let work_root = EvolutionWorkRoot::for_test(work_root_path);
        let prepared = coordinator
            .prepare_candidate(
                &operation.id,
                &evaluated,
                &repository,
                &revision,
                &work_root,
                UtcTimestamp::from_unix_millis(5),
            )
            .expect("exact repaired shadow");
        let reproduced =
            compile_and_run(&prepared.shadow.root().join("harness/router.rs"), "request")
                .expect("run repaired reproduction");
        assert!(reproduced.status.success());
        assert_eq!(reproduced.stdout, b"routed:request\n");
        assert_eq!(
            fs::read_to_string(source.join("harness/router.rs")).expect("original source"),
            defective
        );
        prepared.shadow.reclaim().expect("reclaim repaired shadow");
        candidate_registry
            .cleanup_population(&evaluated.id, UtcTimestamp::from_unix_millis(6))
            .expect("reclaim evaluator shadows");
    }

    fn diagnosis() -> HarnessDiagnosis {
        HarnessDiagnosis {
            id: EntityId::new(),
            trace_fingerprint: text(&format!("sha256:{}", "a".repeat(64))),
            attribution: FailureAttribution {
                failure_class: HarnessFailureClass::HarnessCaused,
                confidence_basis_points: 10_000,
                evidence_sequences: vec![1],
                competing_classes: Vec::new(),
            },
            fault_category: HarnessFaultCategory::Routing,
            causal_component: text("request context router"),
            reproduction: text("run the request through the harness router"),
            expected_behavior_change: text("route the complete request context"),
            target_metric: TargetMetric {
                name: text("successful requests"),
                direction: MetricDirection::Increase,
                baseline: 0,
                threshold: 1,
                revert_threshold: 0,
            },
            cost_ceiling: EvaluationCostCeiling {
                max_external_cost_micros: 10,
                max_latency_ms: 1_000,
                max_tokens: 1_000,
                max_retries: 2,
            },
            created_at: UtcTimestamp::from_unix_millis(1),
        }
    }

    fn evaluator(root: &Path) -> IndependentEvaluator {
        let dataset = |version: &str, id: &str, input: &str| {
            EvaluationDataset::new(
                text(version),
                vec![
                    EvaluationCase::new(
                        text(id),
                        input.as_bytes().to_vec(),
                        format!("routed:{input}\n").into_bytes(),
                        format!("private-{version}-canary").into_bytes(),
                        true,
                    )
                    .expect("evaluation case"),
                ],
            )
            .expect("evaluation dataset")
        };
        IndependentEvaluator::new(
            dataset("search-v1", "case-alpha", "alpha"),
            dataset("validation-v1", "case-beta", "beta"),
            dataset("held-out-v1", "case-gamma", "gamma"),
            RegressionBounds::default(),
            root,
        )
        .expect("independent evaluator")
    }

    fn text(value: &str) -> RedactedText {
        RedactedText::parse(value).expect("safe test text")
    }

    fn copy_tree(source: &Path, destination: &Path) {
        for entry in fs::read_dir(source).expect("read source") {
            let entry = entry.expect("source entry");
            let target = destination.join(entry.file_name());
            if entry.file_type().expect("entry type").is_dir() {
                fs::create_dir(&target).expect("destination directory");
                copy_tree(&entry.path(), &target);
            } else {
                fs::copy(entry.path(), target).expect("copy source file");
            }
        }
    }

    fn compile_and_run(source: &Path, input: &str) -> std::io::Result<std::process::Output> {
        let build = tempfile::tempdir()?;
        let executable = build.path().join("router-process");
        let compiled = Command::new("rustc")
            .arg("--edition=2024")
            .arg(source)
            .arg("-o")
            .arg(&executable)
            .output()?;
        if !compiled.status.success() {
            return Err(std::io::Error::other(
                String::from_utf8_lossy(&compiled.stderr).into_owned(),
            ));
        }
        Command::new(executable).arg(input).output()
    }

    fn git(repository: &Path, arguments: &[&str]) {
        assert!(
            Command::new("git")
                .arg("-C")
                .arg(repository)
                .args(arguments)
                .status()
                .expect("git command")
                .success()
        );
    }

    fn command_text(repository: &Path, arguments: &[&str]) -> String {
        let output = Command::new("git")
            .arg("-C")
            .arg(repository)
            .args(arguments)
            .output()
            .expect("git output");
        assert!(output.status.success());
        String::from_utf8(output.stdout)
            .expect("utf8 git output")
            .trim()
            .into()
    }
}
