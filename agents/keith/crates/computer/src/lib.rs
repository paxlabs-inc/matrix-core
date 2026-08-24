#![forbid(unsafe_code)]

mod audit;
mod host;
mod model;
pub mod stream;

pub use audit::*;
pub use stream::*;
mod policy;
mod recovery;
mod secrets;
mod takeover;

use keith_agent_types::{
    AuditId, CURRENT_SCHEMA_VERSION, ComputerId, ProfileId, Revision, StableKey, UtcTimestamp,
};
use keith_state_store_core::{RecordMutation, StateRecordRepository};
use std::fmt::Display;

pub use host::{
    ComputerHost, ComputerHostConfig, ComputerHostError, ComputerHostSecretWriter,
    ComputerTaskLease, ComputerTaskRequest, HeadedBrowserSupervisor, TaskAdmission,
    TaskConflictPolicy,
};
pub use model::{
    AuditActor, COMPUTER_SCHEMA_VERSION, ComputerAudit, ComputerAuditKind, ComputerError,
    ComputerProvisionRollback, ComputerRecord, ComputerRepository, ComputerRepositoryBatch,
    ComputerState, ControlState, ControlTransition, DurableComputerRepository,
    InMemoryComputerRepository, PolicyDecision, TakeoverLease, TakeoverState,
    TakeoverTransferMetadata,
};
pub use policy::{
    BoundaryDecision, ComputerAction, ComputerActor, ComputerBoundaryPolicy, ComputerResourcePolicy,
};
pub use recovery::{
    ComputerCrashTracker, ComputerQuarantine, RecoveryDecision, reconcile_computer,
};
pub use secrets::{
    FocusedSecretWriter, SecretInjectionAuthority, SecretInjectionAuthorityResolver,
    SecretInjectionError, SecretInjectionReceipt, SecretInjectionRequest, SecretInjectionTarget,
    SecretInjectionTargetKind, SecureSecretInjection,
};
pub use takeover::*;

#[derive(Clone, Debug, Eq, PartialEq)]
pub struct ComputerProvisionRequest {
    pub owner_profile_id: ProfileId,
    pub browser_profile_root: String,
    pub screen_key: StableKey,
    pub enabled: bool,
    pub operation_key: StableKey,
    pub now: UtcTimestamp,
}

#[derive(Clone, Debug, Eq, PartialEq)]
pub struct ComputerDeleteReport {
    pub record: ComputerRecord,
    pub lease_revoked: bool,
    pub retained_browser_profile_root: String,
}

#[derive(Clone, Debug)]
pub struct ComputerProvisionPlan {
    pub record: ComputerRecord,
    pub mutations: Vec<RecordMutation>,
    pub already_committed: bool,
}

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum InactiveComputerReason {
    Disable,
    Archive,
    Delete,
}

#[derive(Clone, Debug, Eq, PartialEq)]
pub struct InactiveComputerRequest {
    pub owner_profile_id: ProfileId,
    pub expected_revision: Revision,
    pub operation_key: StableKey,
    pub reason: InactiveComputerReason,
    pub now: UtcTimestamp,
}

#[derive(Clone, Debug)]
pub struct InactiveComputerPlan {
    pub record: ComputerRecord,
    pub mutations: Vec<RecordMutation>,
    pub already_committed: bool,
    pub lease_revoked: bool,
    pub retained_browser_profile_root: Option<String>,
}

pub struct ComputerLifecycleService<R> {
    repository: R,
}

struct ComputerStateTransition {
    state: ComputerState,
    operation_key: StableKey,
    audit_kind: ComputerAuditKind,
    summary: &'static str,
    now: UtcTimestamp,
    revoke_lease: bool,
}

impl<R> ComputerLifecycleService<R>
where
    R: ComputerRepository,
{
    pub const fn new(repository: R) -> Self {
        Self { repository }
    }

    pub const fn repository(&self) -> &R {
        &self.repository
    }

    /// Creates exactly one isolated computer for a profile, or reconciles an interrupted
    /// provisioning record without replacing its durable identity.
    ///
    /// # Errors
    ///
    /// Returns an error for invalid or conflicting resources, a tombstoned identity, corrupt
    /// repository state, or an atomic persistence failure.
    pub fn provision(
        &self,
        request: ComputerProvisionRequest,
    ) -> Result<ComputerRecord, ComputerError> {
        if let Some(current) = self.repository.computer(&request.owner_profile_id)? {
            if current.browser_profile_root != request.browser_profile_root
                || current.screen_key != request.screen_key
            {
                return Err(ComputerError::Malformed(
                    "computer reconciliation changed durable resources",
                ));
            }
            if current.state == ComputerState::Tombstoned {
                return Err(ComputerError::Malformed(
                    "deleted computer identity cannot be reprovisioned",
                ));
            }
            let desired = if request.enabled {
                ComputerState::Ready
            } else {
                ComputerState::Disabled
            };
            if current.state == desired {
                return Ok(current);
            }
            return self.replace_state(
                &current,
                ComputerStateTransition {
                    state: desired,
                    operation_key: request.operation_key,
                    audit_kind: ComputerAuditKind::Reconciled,
                    summary: "reconciled profile computer resources",
                    now: request.now,
                    revoke_lease: false,
                },
            );
        }

        let record = ComputerRecord {
            version: CURRENT_SCHEMA_VERSION,
            computer_id: ComputerId::new(),
            owner_profile_id: request.owner_profile_id,
            browser_profile_root: request.browser_profile_root,
            screen_key: request.screen_key,
            state: if request.enabled {
                ComputerState::Ready
            } else {
                ComputerState::Disabled
            },
            control_state: ControlState::Idle,
            current_task_key: None,
            created_at: request.now,
            updated_at: request.now,
            revision: Revision::ZERO,
        };
        let audit = self.audit(
            &record,
            request.operation_key,
            ComputerAuditKind::Provisioned,
            "provisioned isolated profile computer",
            request.now,
        )?;
        self.repository.transact(&[
            ComputerRepositoryBatch::InsertComputer(record.clone()),
            ComputerRepositoryBatch::AppendAudit(audit),
        ])?;
        Ok(record)
    }

    /// Creates a fresh target identity and resources. No browser state, task, lease, or audit
    /// history from the source profile is copied.
    ///
    /// # Errors
    ///
    /// Returns an error if the source is missing or deleted, the target conflicts with durable
    /// state, target resources are invalid, or the atomic write fails.
    pub fn duplicate_isolated(
        &self,
        source_profile_id: &ProfileId,
        target: ComputerProvisionRequest,
    ) -> Result<ComputerRecord, ComputerError> {
        if source_profile_id == &target.owner_profile_id {
            return Err(ComputerError::Malformed(
                "computer duplication requires a distinct profile",
            ));
        }
        let source = self
            .repository
            .computer(source_profile_id)?
            .ok_or_else(|| ComputerError::MissingComputer(source_profile_id.clone()))?;
        if source.state == ComputerState::Tombstoned {
            return Err(ComputerError::Malformed(
                "deleted computer cannot be duplicated",
            ));
        }
        if self
            .repository
            .computer(&target.owner_profile_id)?
            .is_some()
        {
            return self.provision(target);
        }
        let record = ComputerRecord {
            version: CURRENT_SCHEMA_VERSION,
            computer_id: ComputerId::new(),
            owner_profile_id: target.owner_profile_id,
            browser_profile_root: target.browser_profile_root,
            screen_key: target.screen_key,
            state: if target.enabled {
                ComputerState::Ready
            } else {
                ComputerState::Disabled
            },
            control_state: ControlState::Idle,
            current_task_key: None,
            created_at: target.now,
            updated_at: target.now,
            revision: Revision::ZERO,
        };
        let audit = self.audit(
            &record,
            target.operation_key,
            ComputerAuditKind::Duplicated,
            "created isolated computer for duplicated profile",
            target.now,
        )?;
        self.repository.transact(&[
            ComputerRepositoryBatch::InsertComputer(record.clone()),
            ComputerRepositoryBatch::AppendAudit(audit),
        ])?;
        Ok(record)
    }

    /// Disables a computer at the expected revision and atomically revokes active control.
    ///
    /// # Errors
    ///
    /// Returns an error for a missing, stale, deleted, or corrupt computer, or if persistence
    /// fails.
    pub fn disable(
        &self,
        owner: &ProfileId,
        expected_revision: Revision,
        operation_key: StableKey,
        now: UtcTimestamp,
    ) -> Result<ComputerRecord, ComputerError> {
        let current = self.required(owner)?;
        if self.operation_committed(owner, &operation_key)? {
            return Ok(current);
        }
        check_expected(expected_revision, current.revision)?;
        if current.state == ComputerState::Disabled {
            return Ok(current);
        }
        if current.state == ComputerState::Tombstoned {
            return Err(ComputerError::Malformed(
                "deleted computer cannot be disabled",
            ));
        }
        self.replace_state(
            &current,
            ComputerStateTransition {
                state: ComputerState::Disabled,
                operation_key,
                audit_kind: ComputerAuditKind::Disabled,
                summary: "disabled profile computer and revoked active control",
                now,
                revoke_lease: true,
            },
        )
    }

    /// Tombstones a computer at the expected revision and reports retained browser data.
    ///
    /// # Errors
    ///
    /// Returns an error for a missing or stale computer, corrupt state, invalid audit data, or
    /// an atomic persistence failure.
    pub fn delete(
        &self,
        owner: &ProfileId,
        expected_revision: Revision,
        operation_key: StableKey,
        now: UtcTimestamp,
    ) -> Result<ComputerDeleteReport, ComputerError> {
        let current = self.required(owner)?;
        if self.operation_committed(owner, &operation_key)? {
            return Ok(ComputerDeleteReport {
                retained_browser_profile_root: current.browser_profile_root.clone(),
                lease_revoked: false,
                record: current,
            });
        }
        check_expected(expected_revision, current.revision)?;
        if current.state == ComputerState::Tombstoned {
            return Ok(ComputerDeleteReport {
                retained_browser_profile_root: current.browser_profile_root.clone(),
                record: current,
                lease_revoked: false,
            });
        }
        let had_lease = self.repository.lease(owner)?.is_some();
        let retained = current.browser_profile_root.clone();
        let record = self.replace_state(
            &current,
            ComputerStateTransition {
                state: ComputerState::Tombstoned,
                operation_key,
                audit_kind: ComputerAuditKind::Deleted,
                summary: "tombstoned profile computer; browser data retained for explicit erasure",
                now,
                revoke_lease: true,
            },
        )?;
        Ok(ComputerDeleteReport {
            record,
            lease_revoked: had_lease,
            retained_browser_profile_root: retained,
        })
    }

    /// Verifies the fail-closed startup invariant for a disabled, archived, or deleted profile.
    ///
    /// # Errors
    ///
    /// Returns an error if the record is missing, stale, not in the expected inactive state,
    /// retains an active task/control mode, has any live lease, or cannot be decoded.
    pub fn verify_inactive(
        &self,
        owner: &ProfileId,
        expected_revision: Revision,
        expected_state: ComputerState,
    ) -> Result<ComputerRecord, ComputerError> {
        if !matches!(
            expected_state,
            ComputerState::Disabled | ComputerState::Tombstoned
        ) {
            return Err(ComputerError::Malformed(
                "inactive verification requires disabled or tombstoned state",
            ));
        }
        let record = self.required(owner)?;
        check_expected(expected_revision, record.revision)?;
        if record.state != expected_state
            || record.control_state != ControlState::Idle
            || record.current_task_key.is_some()
            || self.repository.lease(owner)?.is_some()
        {
            return Err(ComputerError::Malformed(
                "inactive computer is not fully fenced",
            ));
        }
        Ok(record)
    }

    /// Compensates a failed profile-creation transaction without turning the provisional
    /// computer into a user-visible deletion tombstone.
    ///
    /// # Errors
    ///
    /// Returns an error unless the exact revision-zero provision operation is untouched and
    /// lease-free, or if rollback persistence or replay validation fails.
    pub fn rollback_new_provision(
        &self,
        owner: &ProfileId,
        expected_revision: Revision,
        provision_operation_key: StableKey,
        rollback_operation_key: StableKey,
        now: UtcTimestamp,
    ) -> Result<ComputerProvisionRollback, ComputerError> {
        if let Some(existing) = self.repository.provision_rollback(owner)? {
            if existing.provision_operation_key == provision_operation_key
                && existing.rollback_operation_key == rollback_operation_key
            {
                return Ok(existing);
            }
            return Err(ComputerError::UnsafeProvisionRollback(
                "rollback sentinel belongs to another operation",
            ));
        }
        let current = self.required(owner)?;
        check_expected(expected_revision, current.revision)?;
        if expected_revision != Revision::ZERO {
            return Err(ComputerError::UnsafeProvisionRollback(
                "only an untouched revision-zero provision may be rolled back",
            ));
        }
        let rollback = ComputerProvisionRollback {
            version: CURRENT_SCHEMA_VERSION,
            rollback_id: AuditId::new(),
            owner_profile_id: owner.clone(),
            removed_computer_id: current.computer_id.clone(),
            removed_browser_profile_root: current.browser_profile_root.clone(),
            removed_screen_key: current.screen_key.clone(),
            provision_operation_key,
            rollback_operation_key,
            rolled_back_at: now,
        };
        self.repository
            .transact(&[ComputerRepositoryBatch::RollbackNewProvision(
                rollback.clone(),
            )])?;
        Ok(rollback)
    }

    fn required(&self, owner: &ProfileId) -> Result<ComputerRecord, ComputerError> {
        self.repository
            .computer(owner)?
            .ok_or_else(|| ComputerError::MissingComputer(owner.clone()))
    }

    fn operation_committed(
        &self,
        owner: &ProfileId,
        operation_key: &StableKey,
    ) -> Result<bool, ComputerError> {
        Ok(self
            .repository
            .audit(owner)?
            .iter()
            .any(|event| &event.stable_key == operation_key))
    }

    fn replace_state(
        &self,
        current: &ComputerRecord,
        transition: ComputerStateTransition,
    ) -> Result<ComputerRecord, ComputerError> {
        let mut replacement = current.clone();
        replacement.state = transition.state;
        replacement.control_state = ControlState::Idle;
        replacement.current_task_key = None;
        replacement.updated_at = transition.now;
        replacement.revision = current
            .revision
            .checked_next()
            .ok_or(ComputerError::RevisionOverflow)?;
        let mut changes = vec![ComputerRepositoryBatch::ReplaceComputer {
            expected_revision: current.revision,
            record: replacement.clone(),
        }];
        if transition.revoke_lease
            && let Some(lease) = self.repository.lease(&current.owner_profile_id)?
        {
            changes.push(ComputerRepositoryBatch::RemoveLease {
                owner_profile_id: current.owner_profile_id.clone(),
                expected_revision: lease.revision,
            });
        }
        changes.push(ComputerRepositoryBatch::AppendAudit(self.audit(
            &replacement,
            transition.operation_key,
            transition.audit_kind,
            transition.summary,
            transition.now,
        )?));
        self.repository.transact(&changes)?;
        Ok(replacement)
    }

    fn audit(
        &self,
        record: &ComputerRecord,
        operation_key: StableKey,
        kind: ComputerAuditKind,
        summary: &str,
        now: UtcTimestamp,
    ) -> Result<ComputerAudit, ComputerError> {
        let sequence = u64::try_from(self.repository.audit(&record.owner_profile_id)?.len())
            .map_err(|_| ComputerError::AuditConflict)?
            .checked_add(1)
            .ok_or(ComputerError::AuditConflict)?;
        Ok(ComputerAudit {
            version: CURRENT_SCHEMA_VERSION,
            audit_id: AuditId::new(),
            computer_id: record.computer_id.clone(),
            owner_profile_id: record.owner_profile_id.clone(),
            sequence,
            stable_key: operation_key,
            actor: AuditActor::Owner,
            kind,
            task_key: None,
            navigation_origin: None,
            control_transition: None,
            policy_decision: None,
            side_effect_summary: None,
            transfer: None,
            safe_failure: None,
            recovery_correlation: None,
            safe_summary: summary.into(),
            occurred_at: now,
            computer_revision: record.revision,
        })
    }
}

impl<S> ComputerLifecycleService<DurableComputerRepository<S>>
where
    S: StateRecordRepository,
    S::Error: Display,
{
    /// Produces the exact validated state-store mutations for a new provision so callers can
    /// atomically commit them with the profile and permanent-DM mutations.
    ///
    /// # Errors
    ///
    /// Returns an error for conflicting or reused identity/resources, corrupt durable state,
    /// invalid audit data, or failure to construct the exact state-store mutations.
    pub fn provision_mutations(
        &self,
        request: ComputerProvisionRequest,
    ) -> Result<ComputerProvisionPlan, ComputerError> {
        if let Some(current) = self.repository.computer(&request.owner_profile_id)? {
            if current.browser_profile_root == request.browser_profile_root
                && current.screen_key == request.screen_key
                && self.operation_committed(&request.owner_profile_id, &request.operation_key)?
            {
                return Ok(ComputerProvisionPlan {
                    record: current,
                    mutations: Vec::new(),
                    already_committed: true,
                });
            }
            return Err(ComputerError::DuplicateComputer(request.owner_profile_id));
        }
        let record = ComputerRecord {
            version: CURRENT_SCHEMA_VERSION,
            computer_id: ComputerId::new(),
            owner_profile_id: request.owner_profile_id,
            browser_profile_root: request.browser_profile_root,
            screen_key: request.screen_key,
            state: if request.enabled {
                ComputerState::Ready
            } else {
                ComputerState::Disabled
            },
            control_state: ControlState::Idle,
            current_task_key: None,
            created_at: request.now,
            updated_at: request.now,
            revision: Revision::ZERO,
        };
        let audit = self.audit(
            &record,
            request.operation_key,
            ComputerAuditKind::Provisioned,
            "provisioned isolated profile computer",
            request.now,
        )?;
        let mutations = self.repository.plan_mutations(&[
            ComputerRepositoryBatch::InsertComputer(record.clone()),
            ComputerRepositoryBatch::AppendAudit(audit),
        ])?;
        Ok(ComputerProvisionPlan {
            record,
            mutations,
            already_committed: false,
        })
    }

    /// Produces validated computer and lease-fencing mutations for composition with the matching
    /// profile disable, archive, or delete transaction.
    ///
    /// # Errors
    ///
    /// Returns an error for a missing or stale computer, mismatched replay, corrupt audit or lease
    /// state, revision overflow, or failure to construct the exact durable mutations.
    pub fn inactive_mutations(
        &self,
        request: InactiveComputerRequest,
    ) -> Result<InactiveComputerPlan, ComputerError> {
        let current = self.required(&request.owner_profile_id)?;
        let desired_state = match request.reason {
            InactiveComputerReason::Disable | InactiveComputerReason::Archive => {
                ComputerState::Disabled
            }
            InactiveComputerReason::Delete => ComputerState::Tombstoned,
        };
        if self.operation_committed(&request.owner_profile_id, &request.operation_key)? {
            if current.state != desired_state
                || self.repository.lease(&request.owner_profile_id)?.is_some()
            {
                return Err(ComputerError::Malformed(
                    "inactive operation replay does not match fenced state",
                ));
            }
            return Ok(InactiveComputerPlan {
                retained_browser_profile_root: (desired_state == ComputerState::Tombstoned)
                    .then(|| current.browser_profile_root.clone()),
                record: current,
                mutations: Vec::new(),
                already_committed: true,
                lease_revoked: false,
            });
        }
        check_expected(request.expected_revision, current.revision)?;
        if current.state == ComputerState::Tombstoned {
            return Err(ComputerError::Malformed(
                "tombstoned computer rejects new inactive transitions",
            ));
        }
        let mut replacement = current.clone();
        replacement.state = desired_state;
        replacement.control_state = ControlState::Idle;
        replacement.current_task_key = None;
        replacement.updated_at = request.now;
        replacement.revision = current
            .revision
            .checked_next()
            .ok_or(ComputerError::RevisionOverflow)?;
        let lease = self.repository.lease(&request.owner_profile_id)?;
        let mut batch = vec![ComputerRepositoryBatch::ReplaceComputer {
            expected_revision: current.revision,
            record: replacement.clone(),
        }];
        if let Some(lease) = &lease {
            batch.push(ComputerRepositoryBatch::RemoveLease {
                owner_profile_id: request.owner_profile_id.clone(),
                expected_revision: lease.revision,
            });
        }
        let (kind, summary) = match request.reason {
            InactiveComputerReason::Disable => (
                ComputerAuditKind::Disabled,
                "disabled profile computer and revoked active control",
            ),
            InactiveComputerReason::Archive => (
                ComputerAuditKind::Archived,
                "archived profile computer and revoked active control",
            ),
            InactiveComputerReason::Delete => (
                ComputerAuditKind::Deleted,
                "tombstoned profile computer; browser data retained for explicit erasure",
            ),
        };
        batch.push(ComputerRepositoryBatch::AppendAudit(self.audit(
            &replacement,
            request.operation_key,
            kind,
            summary,
            request.now,
        )?));
        let mutations = self.repository.plan_mutations(&batch)?;
        Ok(InactiveComputerPlan {
            retained_browser_profile_root: (desired_state == ComputerState::Tombstoned)
                .then(|| replacement.browser_profile_root.clone()),
            record: replacement,
            mutations,
            already_committed: false,
            lease_revoked: lease.is_some(),
        })
    }
}

fn check_expected(expected: Revision, actual: Revision) -> Result<(), ComputerError> {
    if expected == actual {
        Ok(())
    } else {
        Err(ComputerError::RevisionConflict { expected, actual })
    }
}

#[cfg(test)]
mod agent_lifecycle_tests {
    use super::*;
    use keith_agent_types::{EntityId, TakeoverLeaseId};
    use keith_state_store::EmbeddedStore;
    use keith_state_store_core::{
        AtomicStateRepository, Collection, VersionedRecord, WritePrecondition,
    };

    fn profile(value: u128) -> ProfileId {
        ProfileId::from(EntityId::from_u128(value))
    }

    fn key(value: &str) -> StableKey {
        StableKey::parse(value).unwrap()
    }

    fn request(owner: ProfileId, suffix: &str) -> ComputerProvisionRequest {
        ComputerProvisionRequest {
            owner_profile_id: owner,
            browser_profile_root: format!("/profiles/{suffix}/browser"),
            screen_key: key(&format!("screen-{suffix}")),
            enabled: true,
            operation_key: key(&format!("provision-{suffix}")),
            now: UtcTimestamp(10),
        }
    }

    fn active_lease(record: &ComputerRecord) -> TakeoverLease {
        TakeoverLease {
            version: CURRENT_SCHEMA_VERSION,
            takeover_lease_id: TakeoverLeaseId::new(),
            computer_id: record.computer_id.clone(),
            owner_profile_id: record.owner_profile_id.clone(),
            task_key: key("active-task"),
            token_digest_hex: "a".repeat(64),
            fencing_token: 1,
            acquired_at: UtcTimestamp(11),
            renewed_at: UtcTimestamp(11),
            expires_at: UtcTimestamp(20),
            state: TakeoverState::Active,
            revision: Revision::ZERO,
        }
    }

    #[test]
    fn agent_lifecycle_provision_is_idempotent_and_reconciles_without_identity_drift() {
        let service = ComputerLifecycleService::new(InMemoryComputerRepository::default());
        let owner = profile(1);
        let created = service.provision(request(owner.clone(), "one")).unwrap();
        let replay = service.provision(request(owner.clone(), "one")).unwrap();
        assert_eq!(created, replay);
        assert_eq!(service.repository().audit(&owner).unwrap().len(), 1);

        let disabled = service
            .disable(&owner, Revision::ZERO, key("disable-one"), UtcTimestamp(20))
            .unwrap();
        assert_eq!(disabled.state, ComputerState::Disabled);
        let mut reconcile = request(owner.clone(), "one");
        reconcile.operation_key = key("reconcile-one");
        reconcile.now = UtcTimestamp(30);
        let ready = service.provision(reconcile).unwrap();
        assert_eq!(ready.computer_id, created.computer_id);
        assert_eq!(ready.state, ComputerState::Ready);
        assert_eq!(ready.revision, Revision::new(2));
    }

    #[test]
    fn agent_lifecycle_duplicate_uses_fresh_private_resources_and_no_control_state() {
        let service = ComputerLifecycleService::new(InMemoryComputerRepository::default());
        let source_owner = profile(2);
        let target_owner = profile(3);
        let source = service
            .provision(request(source_owner.clone(), "source"))
            .unwrap();
        service
            .repository()
            .transact(&[ComputerRepositoryBatch::PutLease {
                expected_revision: None,
                lease: active_lease(&source),
            }])
            .unwrap();
        let target = service
            .duplicate_isolated(&source_owner, request(target_owner.clone(), "target"))
            .unwrap();
        assert_ne!(target.computer_id, source.computer_id);
        assert_ne!(target.browser_profile_root, source.browser_profile_root);
        assert_eq!(target.control_state, ControlState::Idle);
        assert!(target.current_task_key.is_none());
        assert!(service.repository().lease(&target_owner).unwrap().is_none());
        assert_eq!(service.repository().audit(&target_owner).unwrap().len(), 1);
    }

    #[test]
    fn agent_lifecycle_disable_and_delete_atomically_revoke_and_retain_fences() {
        let service = ComputerLifecycleService::new(InMemoryComputerRepository::default());
        let owner = profile(4);
        let created = service.provision(request(owner.clone(), "delete")).unwrap();
        service
            .repository()
            .transact(&[ComputerRepositoryBatch::PutLease {
                expected_revision: None,
                lease: active_lease(&created),
            }])
            .unwrap();
        let disabled = service
            .disable(
                &owner,
                Revision::ZERO,
                key("disable-delete"),
                UtcTimestamp(20),
            )
            .unwrap();
        let disable_replay = service
            .disable(
                &owner,
                Revision::ZERO,
                key("disable-delete"),
                UtcTimestamp(21),
            )
            .unwrap();
        assert_eq!(disable_replay, disabled);
        assert!(service.repository().lease(&owner).unwrap().is_none());
        assert_eq!(disabled.state, ComputerState::Disabled);

        let deleted = service
            .delete(
                &owner,
                Revision::new(1),
                key("delete-delete"),
                UtcTimestamp(30),
            )
            .unwrap();
        let delete_replay = service
            .delete(
                &owner,
                Revision::new(1),
                key("delete-delete"),
                UtcTimestamp(31),
            )
            .unwrap();
        assert_eq!(delete_replay.record, deleted.record);
        assert_eq!(deleted.record.state, ComputerState::Tombstoned);
        assert_eq!(
            deleted.retained_browser_profile_root,
            "/profiles/delete/browser"
        );
        assert!(service.provision(request(owner.clone(), "delete")).is_err());

        let mut reacquire = active_lease(&created);
        reacquire.takeover_lease_id = TakeoverLeaseId::new();
        reacquire.revision = Revision::new(2);
        reacquire.fencing_token = 2;
        reacquire.token_digest_hex = "b".repeat(64);
        assert!(
            service
                .repository()
                .transact(&[ComputerRepositoryBatch::PutLease {
                    expected_revision: None,
                    lease: reacquire,
                }])
                .is_err()
        );
    }

    #[test]
    fn agent_lifecycle_stale_revision_rolls_back_state_lease_and_audit() {
        let service = ComputerLifecycleService::new(InMemoryComputerRepository::default());
        let owner = profile(5);
        let record = service.provision(request(owner.clone(), "stale")).unwrap();
        service
            .repository()
            .transact(&[ComputerRepositoryBatch::PutLease {
                expected_revision: None,
                lease: active_lease(&record),
            }])
            .unwrap();
        assert!(matches!(
            service.disable(
                &owner,
                Revision::new(9),
                key("stale-disable"),
                UtcTimestamp(20)
            ),
            Err(ComputerError::RevisionConflict { .. })
        ));
        assert_eq!(service.repository().computer(&owner).unwrap(), Some(record));
        assert!(service.repository().lease(&owner).unwrap().is_some());
        assert_eq!(service.repository().audit(&owner).unwrap().len(), 1);
    }

    #[test]
    fn agent_lifecycle_provision_rollback_is_replay_safe_and_retry_is_isolated() {
        let service = ComputerLifecycleService::new(InMemoryComputerRepository::default());
        let owner = profile(6);
        let original = service.provision(request(owner.clone(), "failed")).unwrap();
        let rollback = service
            .rollback_new_provision(
                &owner,
                Revision::ZERO,
                key("provision-failed"),
                key("rollback-failed"),
                UtcTimestamp(20),
            )
            .unwrap();
        assert!(service.repository().computer(&owner).unwrap().is_none());
        assert!(service.repository().audit(&owner).unwrap().is_empty());
        assert_eq!(
            service
                .rollback_new_provision(
                    &owner,
                    Revision::ZERO,
                    key("provision-failed"),
                    key("rollback-failed"),
                    UtcTimestamp(21),
                )
                .unwrap(),
            rollback
        );
        assert!(service.provision(request(owner.clone(), "failed")).is_err());
        let retry = service.provision(request(owner, "retry")).unwrap();
        assert_ne!(retry.computer_id, original.computer_id);
        assert_ne!(retry.browser_profile_root, original.browser_profile_root);
        assert_ne!(retry.screen_key, original.screen_key);
    }

    #[test]
    fn agent_lifecycle_provision_rollback_rejects_used_or_changed_computers() {
        let service = ComputerLifecycleService::new(InMemoryComputerRepository::default());
        let leased_owner = profile(7);
        let leased = service
            .provision(request(leased_owner.clone(), "leased"))
            .unwrap();
        service
            .repository()
            .transact(&[ComputerRepositoryBatch::PutLease {
                expected_revision: None,
                lease: active_lease(&leased),
            }])
            .unwrap();
        assert!(matches!(
            service.rollback_new_provision(
                &leased_owner,
                Revision::ZERO,
                key("provision-leased"),
                key("rollback-leased"),
                UtcTimestamp(20)
            ),
            Err(ComputerError::UnsafeProvisionRollback(_))
        ));
        assert!(
            service
                .repository()
                .computer(&leased_owner)
                .unwrap()
                .is_some()
        );
        assert!(service.repository().lease(&leased_owner).unwrap().is_some());

        let changed_owner = profile(8);
        service
            .provision(request(changed_owner.clone(), "changed"))
            .unwrap();
        service
            .disable(
                &changed_owner,
                Revision::ZERO,
                key("disable-changed"),
                UtcTimestamp(20),
            )
            .unwrap();
        assert!(matches!(
            service.rollback_new_provision(
                &changed_owner,
                Revision::new(1),
                key("provision-changed"),
                key("rollback-changed"),
                UtcTimestamp(30)
            ),
            Err(ComputerError::UnsafeProvisionRollback(_))
        ));
    }

    #[test]
    fn agent_lifecycle_provision_rollback_survives_restart_without_partial_state() {
        let directory = tempfile::tempdir().unwrap();
        let path = directory.path().join("rollback.sqlite3");
        let owner = profile(9);
        let rollback = {
            let service = ComputerLifecycleService::new(DurableComputerRepository::new(
                EmbeddedStore::open(&path, None).unwrap(),
            ));
            service.provision(request(owner.clone(), "crash")).unwrap();
            service
                .rollback_new_provision(
                    &owner,
                    Revision::ZERO,
                    key("provision-crash"),
                    key("rollback-crash"),
                    UtcTimestamp(20),
                )
                .unwrap()
        };
        let service = ComputerLifecycleService::new(DurableComputerRepository::new(
            EmbeddedStore::open(&path, None).unwrap(),
        ));
        assert!(service.repository().computer(&owner).unwrap().is_none());
        assert!(service.repository().audit(&owner).unwrap().is_empty());
        assert_eq!(
            service.repository().provision_rollback(&owner).unwrap(),
            Some(rollback.clone())
        );
        assert_eq!(
            service
                .rollback_new_provision(
                    &owner,
                    Revision::ZERO,
                    key("provision-crash"),
                    key("rollback-crash"),
                    UtcTimestamp(30),
                )
                .unwrap(),
            rollback
        );
    }

    #[test]
    fn agent_lifecycle_provision_plan_joins_an_outer_atomic_transaction() {
        let directory = tempfile::tempdir().unwrap();
        let path = directory.path().join("plan.sqlite3");
        let owner = profile(10);
        let service = ComputerLifecycleService::new(DurableComputerRepository::new(
            EmbeddedStore::open(&path, None).unwrap(),
        ));
        let plan = service
            .provision_mutations(request(owner.clone(), "planned"))
            .unwrap();
        assert!(!plan.already_committed);
        assert_eq!(plan.mutations.len(), 2);
        assert!(service.repository().computer(&owner).unwrap().is_none());
        service
            .repository()
            .repository()
            .transact(&plan.mutations)
            .unwrap();
        assert_eq!(
            service.repository().computer(&owner).unwrap(),
            Some(plan.record.clone())
        );
        let replay = service
            .provision_mutations(request(owner, "planned"))
            .unwrap();
        assert!(replay.already_committed);
        assert!(replay.mutations.is_empty());
        assert_eq!(replay.record, plan.record);
    }

    #[test]
    fn agent_lifecycle_computer_and_coordination_audits_coexist_in_dedicated_collections() {
        let directory = tempfile::tempdir().unwrap();
        let path = directory.path().join("audit-coexistence.sqlite3");
        let store = EmbeddedStore::open(&path, None).unwrap();
        store
            .transact(&[RecordMutation::Put {
                collection: Collection::TeammateAudits,
                record: VersionedRecord {
                    version: CURRENT_SCHEMA_VERSION,
                    id: EntityId::from_u128(9_001),
                    revision: Revision::ZERO,
                    updated_at: UtcTimestamp(5),
                    payload: serde_json::json!({
                        "coordination": "ownership_transferred",
                        "actor_profile_id": profile(99)
                    }),
                },
                precondition: WritePrecondition::Missing,
            }])
            .unwrap();
        let service = ComputerLifecycleService::new(DurableComputerRepository::new(store));
        let owner = profile(11);
        service
            .provision(request(owner.clone(), "coexist"))
            .unwrap();
        assert_eq!(service.repository().audit(&owner).unwrap().len(), 1);
        assert_eq!(
            service
                .repository()
                .repository()
                .list_records(Collection::TeammateAudits)
                .unwrap()
                .len(),
            1
        );
        assert_eq!(
            service
                .repository()
                .repository()
                .list_records(Collection::ComputerAudits)
                .unwrap()
                .len(),
            1
        );
        service
            .rollback_new_provision(
                &owner,
                Revision::ZERO,
                key("provision-coexist"),
                key("rollback-coexist"),
                UtcTimestamp(20),
            )
            .unwrap();
        assert_eq!(
            service
                .repository()
                .repository()
                .list_records(Collection::TeammateAudits)
                .unwrap()
                .len(),
            1
        );
        assert!(service.repository().computer(&owner).unwrap().is_none());
        assert!(
            service
                .repository()
                .provision_rollback(&owner)
                .unwrap()
                .is_some()
        );
    }

    #[test]
    fn agent_lifecycle_inactive_plan_fences_lease_and_verifies_after_restart() {
        let directory = tempfile::tempdir().unwrap();
        let path = directory.path().join("inactive-plan.sqlite3");
        let owner = profile(12);
        let expected = {
            let service = ComputerLifecycleService::new(DurableComputerRepository::new(
                EmbeddedStore::open(&path, None).unwrap(),
            ));
            let record = service
                .provision(request(owner.clone(), "inactive"))
                .unwrap();
            service
                .repository()
                .transact(&[ComputerRepositoryBatch::PutLease {
                    expected_revision: None,
                    lease: active_lease(&record),
                }])
                .unwrap();
            assert!(
                service
                    .verify_inactive(&owner, Revision::ZERO, ComputerState::Disabled)
                    .is_err()
            );
            let plan = service
                .inactive_mutations(InactiveComputerRequest {
                    owner_profile_id: owner.clone(),
                    expected_revision: Revision::ZERO,
                    operation_key: key("archive-inactive"),
                    reason: InactiveComputerReason::Archive,
                    now: UtcTimestamp(30),
                })
                .unwrap();
            assert!(plan.lease_revoked);
            assert!(!plan.already_committed);
            service
                .repository()
                .repository()
                .transact(&plan.mutations)
                .unwrap();
            plan.record
        };
        let service = ComputerLifecycleService::new(DurableComputerRepository::new(
            EmbeddedStore::open(&path, None).unwrap(),
        ));
        assert_eq!(
            service
                .verify_inactive(&owner, Revision::new(1), ComputerState::Disabled)
                .unwrap(),
            expected
        );
        let replay = service
            .inactive_mutations(InactiveComputerRequest {
                owner_profile_id: owner,
                expected_revision: Revision::ZERO,
                operation_key: key("archive-inactive"),
                reason: InactiveComputerReason::Archive,
                now: UtcTimestamp(40),
            })
            .unwrap();
        assert!(replay.already_committed);
        assert!(replay.mutations.is_empty());
    }

    #[test]
    fn agent_lifecycle_inactive_delete_plan_reports_remnant_and_rejects_stale_revision() {
        let directory = tempfile::tempdir().unwrap();
        let path = directory.path().join("inactive-delete.sqlite3");
        let owner = profile(13);
        let service = ComputerLifecycleService::new(DurableComputerRepository::new(
            EmbeddedStore::open(&path, None).unwrap(),
        ));
        service
            .provision(request(owner.clone(), "remnant"))
            .unwrap();
        assert!(matches!(
            service.inactive_mutations(InactiveComputerRequest {
                owner_profile_id: owner.clone(),
                expected_revision: Revision::new(9),
                operation_key: key("delete-remnant"),
                reason: InactiveComputerReason::Delete,
                now: UtcTimestamp(20),
            }),
            Err(ComputerError::RevisionConflict { .. })
        ));
        let plan = service
            .inactive_mutations(InactiveComputerRequest {
                owner_profile_id: owner.clone(),
                expected_revision: Revision::ZERO,
                operation_key: key("delete-remnant"),
                reason: InactiveComputerReason::Delete,
                now: UtcTimestamp(20),
            })
            .unwrap();
        assert_eq!(
            plan.retained_browser_profile_root.as_deref(),
            Some("/profiles/remnant/browser")
        );
        service
            .repository()
            .repository()
            .transact(&plan.mutations)
            .unwrap();
        service
            .verify_inactive(&owner, Revision::new(1), ComputerState::Tombstoned)
            .unwrap();
    }
}
