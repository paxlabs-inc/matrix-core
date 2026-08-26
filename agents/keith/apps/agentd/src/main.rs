#![forbid(unsafe_code)]

use std::collections::{BTreeMap, BTreeSet};
use std::ffi::OsString;
use std::fs::{self, File};
use std::io::Write;
use std::path::PathBuf;
use std::process::{Command, ExitStatus};
use std::sync::atomic::{AtomicBool, Ordering};
use std::sync::{Arc, Mutex};
use std::thread;
use std::time::Duration;

use base64::Engine as _;
use keith_agent_types::{
    CURRENT_PROTOCOL_VERSION, ClientId, EntityId, ErrorCode, Generation, ProfileId, RootTreeId,
    Sequence, StableKey, UtcTimestamp,
};
use keith_computer::{
    AuditActor, ComputerActor, ComputerAuditContext, ComputerAuditKind, ComputerAuditService,
    ComputerBoundaryPolicy, ComputerButtonState, ComputerFrameEncoding, ComputerHost,
    ComputerHostConfig, ComputerInputCommand, ComputerInputPayload, ComputerPointerButton,
    ComputerRepository, ComputerResourcePolicy, ComputerState, ComputerStreamController,
    ComputerStreamLimits, ComputerStreamOpenRequest, ComputerStreamOrigin, ComputerStreamSession,
    ComputerStreamSubject, ComputerTaskLease, ComputerTaskRequest, ControlState,
    DurableComputerRepository, OwningTaskResumeOrFail, PolicyDecision,
    RefreshedComputerObservation, SecretInjectionRequest, SecretInjectionTarget,
    SecretInjectionTargetKind, TakeoverAcquireRequest, TakeoverBoundaryError, TakeoverClaim,
    TakeoverDueRecovery, TakeoverHandbackRequest, TakeoverLeaseService, TakeoverPauseBoundary,
    TakeoverRenewRequest, TakeoverResolution, TakeoverResolutionBoundary, TakeoverTaskBoundary,
    TakeoverUserActedNote, TaskAdmission, TaskConflictPolicy, TypedComputerAuditEvent,
    UserActedKind,
};
use keith_credentials::{CredentialOwner, CredentialRef};
use keith_daemon_core::{
    ComputerCommandRuntime, ComputerCommandRuntimeError, DaemonCore, DaemonOptions,
};
use keith_local_runtime::{
    ComputerSecretInjectionService, ComputerSecretInjectionServiceError,
    ComputerTakeoverTaskBoundaryService, ComputerTakeoverTaskNoteKind, LocalRuntimeLaunchConfig,
    RuntimeCredentialKeySource,
};
use keith_platform::PlatformPaths;
use keith_profile::AgentLifecycleService;
use keith_protocol::{
    COMPUTER_PROTOCOL_PRODUCER, COMPUTER_PROTOCOL_VERSION, ComputerButtonStateProjection,
    ComputerCredentialOwnerProjection, ComputerFrameEncodingProjection, ComputerFrameProjection,
    ComputerHandBackDispositionProjection, ComputerHandBackReceiptProjection,
    ComputerInputPayloadProjection, ComputerPointerButtonProjection, ComputerProjection,
    ComputerProtocolCommand, ComputerProtocolEvent, ComputerProtocolEventEnvelope,
    ComputerProtocolResponse, ComputerSecretInjectionReceiptProjection,
    ComputerSecretInjectionTargetKindProjection, ComputerSecretInjectionTargetProjection,
    ComputerSnapshot, ComputerStateProjection, ComputerStreamControllerProjection,
    ComputerStreamCursorProjection, ComputerStreamDescriptorProjection,
    ComputerStreamInputReceiptProjection, ComputerStreamLimitsProjection,
    ComputerStreamOriginProjection, ComputerStreamSubjectProjection,
    ComputerTakeoverClaimProjection, ComputerTakeoverReceiptProjection,
    ConversationProtocolEnvelope, ConversationProtocolEvent, ConversationResumeGap, DaemonEvent,
    EventEnvelope, ResumeConversationEventsCommand, TEAMMATES_PROTOCOL_PRODUCER,
    TEAMMATES_PROTOCOL_VERSION, TeammatesDelta, TeammatesSnapshot,
};
use keith_self_evolution::DaemonStaging;
use keith_state_store::{EmbeddedStore, FileBackupHook};
use sha2::{Digest, Sha256};
use signal_hook::consts::{SIGINT, SIGTERM};

struct Arguments {
    data_root: PathBuf,
    socket: PathBuf,
    bootstrap_worker_executable: PathBuf,
    browser_runner_executable: PathBuf,
    chromium_executable: PathBuf,
    idle_seconds: u64,
    credential_root: PathBuf,
    credential_key_source: CredentialKeySource,
    workspace_root: PathBuf,
    openai_base_url: String,
    anthropic_base_url: String,
    provider_base_urls: BTreeMap<String, String>,
}

enum CredentialKeySource {
    Environment(String),
    Native(String),
    Restricted(PathBuf),
}

const CHILD_ENV: &str = "KEITH_DAEMON_CHILD";
const READY_PATH_ENV: &str = "KEITH_DAEMON_READY_PATH";
const READY_IMAGE_ENV: &str = "KEITH_DAEMON_READY_IMAGE";
const LAUNCHER_PID_ENV: &str = "KEITH_DAEMON_LAUNCHER_PID";
const STAGING_ROOT_ENV: &str = "KEITH_DAEMON_STAGING_ROOT";
// The child publishes and fsyncs its real worker bootstrap image before declaring readiness.
// Parallel debug process tests can concurrently write several GiB of unstripped binaries.
const READY_TIMEOUT: Duration = Duration::from_secs(120);
const READY_POLL: Duration = Duration::from_millis(20);

type DaemonComputerRepository = DurableComputerRepository<EmbeddedStore>;

struct ComputerHostRuntime {
    host: Arc<ComputerHost<DaemonComputerRepository>>,
    takeovers: TakeoverLeaseService<DaemonComputerRepository>,
    secrets: ComputerSecretInjectionService,
    task_boundaries: Arc<ComputerTakeoverTaskBoundaryService>,
    audit: ComputerAuditService<DaemonComputerRepository>,
    recovery_shutdown: Arc<AtomicBool>,
    recovery_thread: Option<thread::JoinHandle<()>>,
    lifecycle: AgentLifecycleService<EmbeddedStore>,
    owners: Arc<Mutex<BTreeSet<ProfileId>>>,
    sessions: BTreeMap<ClientId, ClientComputerStream>,
    catalog_subscriptions: BTreeMap<ClientId, ComputerCatalogSubscription>,
}

struct ClientComputerStream {
    session: ComputerStreamSession,
    root_tree_id: RootTreeId,
}

struct ComputerCatalogSubscription {
    profile_id: ProfileId,
    subscription_id: EntityId,
    authority_key: StableKey,
    origin_server_instance_id: EntityId,
    generation: u64,
    sequence: u64,
    root_tree_id: RootTreeId,
    last_projection: Option<ComputerProjection>,
    replacement_snapshot_required: bool,
}

impl ComputerHostRuntime {
    fn open(
        data_root: &std::path::Path,
        browser_runner_binary: PathBuf,
        chromium_binary: PathBuf,
        secrets: ComputerSecretInjectionService,
        task_boundaries: ComputerTakeoverTaskBoundaryService,
    ) -> Result<Self, String> {
        let state_path = data_root.join("state.sqlite");
        let lifecycle = AgentLifecycleService::new(
            EmbeddedStore::open(&state_path, Some(&FileBackupHook))
                .map_err(|error| error.to_string())?,
        );
        let computer_repository = DurableComputerRepository::new(
            EmbeddedStore::open(&state_path, Some(&FileBackupHook))
                .map_err(|error| error.to_string())?,
        );
        let host = Arc::new(ComputerHost::new(
            DurableComputerRepository::new(
                EmbeddedStore::open(&state_path, Some(&FileBackupHook))
                    .map_err(|error| error.to_string())?,
            ),
            ComputerHostConfig {
                browser_runner_binary,
                chromium_binary,
                xvfb_binary: PathBuf::from("/usr/bin/Xvfb"),
                systemd_run_binary: PathBuf::from("/usr/bin/systemd-run"),
                display_base: 100,
                control_port_base: 31_000,
                screen_width: 1_440,
                screen_height: 900,
                screen_depth: 24,
            },
        ));
        let now = UtcTimestamp::now().map_err(|error| error.to_string())?;
        let mut owners = BTreeSet::new();
        for roster_entry in lifecycle.roster().map_err(|error| error.to_string())? {
            let Some(agent) = lifecycle
                .get(&roster_entry.profile_id)
                .map_err(|error| error.to_string())?
            else {
                continue;
            };
            let computer_policy = &agent.presentation.computer_policy;
            let Some(computer) = computer_repository
                .computer(&roster_entry.profile_id)
                .map_err(|error| error.to_string())?
            else {
                continue;
            };
            if !roster_entry.enabled || !computer_policy.enabled {
                host.shutdown(&roster_entry.profile_id)
                    .map_err(|error| error.to_string())?;
                continue;
            }
            if computer.state == ComputerState::Ready {
                owners.insert(roster_entry.profile_id);
            }
        }
        let task_boundaries = Arc::new(task_boundaries);
        let recovery_shutdown = Arc::new(AtomicBool::new(false));
        let recovery_takeovers = TakeoverLeaseService::new(DurableComputerRepository::new(
            EmbeddedStore::open(&state_path, Some(&FileBackupHook))
                .map_err(|error| error.to_string())?,
        ));
        let recovery_audit = ComputerAuditService::new(DurableComputerRepository::new(
            EmbeddedStore::open(&state_path, Some(&FileBackupHook))
                .map_err(|error| error.to_string())?,
        ));
        for owner in &owners {
            let boundary = DurableTakeoverBoundary {
                host: &host,
                tasks: &task_boundaries,
            };
            let outcome = recovery_takeovers
                .recover_due(owner, &boundary, now)
                .map_err(|error| error.to_string())?;
            if let TakeoverDueRecovery::Recovered(resolution) = &outcome {
                append_recovery_audit(&recovery_audit, &host, resolution, now)
                    .map_err(|error| error.to_string())?;
            }
        }
        let recovery_host = Arc::clone(&host);
        let recovery_tasks = Arc::clone(&task_boundaries);
        let recovery_stop = Arc::clone(&recovery_shutdown);
        let owners = Arc::new(Mutex::new(owners));
        let recovery_owners = Arc::clone(&owners);
        let recovery_thread = thread::Builder::new()
            .name("keith-computer-takeover-recovery".into())
            .spawn(move || {
                while !recovery_stop.load(Ordering::Acquire) {
                    let now = UtcTimestamp::now().unwrap_or(UtcTimestamp::UNIX_EPOCH);
                    let boundary = DurableTakeoverBoundary {
                        host: &recovery_host,
                        tasks: &recovery_tasks,
                    };
                    let current_owners = recovery_owners
                        .lock()
                        .map(|owners| owners.iter().cloned().collect::<Vec<_>>())
                        .unwrap_or_default();
                    for owner in &current_owners {
                        if let Ok(TakeoverDueRecovery::Recovered(resolution)) =
                            recovery_takeovers.recover_due(owner, &boundary, now)
                        {
                            let _ = append_recovery_audit(
                                &recovery_audit,
                                &recovery_host,
                                &resolution,
                                now,
                            );
                        }
                    }
                    thread::park_timeout(Duration::from_secs(1));
                }
            })
            .map_err(|error| error.to_string())?;
        Ok(Self {
            host,
            takeovers: TakeoverLeaseService::new(computer_repository),
            secrets,
            task_boundaries,
            audit: ComputerAuditService::new(DurableComputerRepository::new(
                EmbeddedStore::open(&state_path, Some(&FileBackupHook))
                    .map_err(|error| error.to_string())?,
            )),
            recovery_shutdown,
            recovery_thread: Some(recovery_thread),
            lifecycle,
            owners,
            sessions: BTreeMap::new(),
            catalog_subscriptions: BTreeMap::new(),
        })
    }

    fn catalog_projection(
        &self,
        profile_id: &ProfileId,
        now: UtcTimestamp,
    ) -> Result<Option<ComputerProjection>, ComputerCommandRuntimeError> {
        let lifecycle = self
            .lifecycle
            .get(profile_id)
            .map_err(host_error)?
            .ok_or_else(|| {
                runtime_error(
                    ErrorCode::NotFound,
                    "computer profile does not exist",
                    false,
                )
            })?;
        if !lifecycle.profile.enabled
            || lifecycle.presentation.lifecycle != keith_profile::ProfileLifecycleState::Enabled
            || !lifecycle.presentation.computer_policy.enabled
        {
            return Err(runtime_error(
                ErrorCode::Unauthorized,
                "computer catalog profile is not enabled",
                false,
            ));
        }
        self.ensure_host_running(profile_id, now)?;
        let Some(record) = self
            .host
            .repository()
            .computer(profile_id)
            .map_err(host_error)?
        else {
            return Ok(None);
        };
        if record.owner_profile_id != *profile_id {
            return Err(runtime_error(
                ErrorCode::Unauthorized,
                "computer catalog ownership is invalid",
                false,
            ));
        }
        let active_lease = self
            .host
            .repository()
            .lease(profile_id)
            .map_err(host_error)?
            .filter(|lease| {
                lease.state == keith_computer::TakeoverState::Active && lease.expires_at > now
            });
        let state = match record.state {
            ComputerState::Provisioning => ComputerStateProjection::Starting,
            ComputerState::Ready => match record.control_state {
                ControlState::Idle => ComputerStateProjection::Ready,
                ControlState::Agent => ComputerStateProjection::Busy,
                ControlState::UserTakeover | ControlState::Paused => {
                    ComputerStateProjection::Takeover
                }
            },
            ComputerState::Quarantined => ComputerStateProjection::Quarantined,
            ComputerState::Disabled | ComputerState::Tombstoned => ComputerStateProjection::Stopped,
        };
        Ok(Some(ComputerProjection {
            computer_id: record.computer_id.as_entity_id().clone(),
            profile_id: profile_id.clone(),
            state,
            active_task_id: record.current_task_key.as_ref().map(stable_key_entity_id),
            takeover_lease_id: active_lease
                .as_ref()
                .map(|lease| lease.takeover_lease_id.as_entity_id().clone()),
            revision: record.revision,
        }))
    }

    fn catalog_delta(
        &mut self,
        authenticated_client_id: &ClientId,
    ) -> Result<Option<EventEnvelope>, ComputerCommandRuntimeError> {
        let Some(current) = self.catalog_subscriptions.get(authenticated_client_id) else {
            return Ok(None);
        };
        if current.replacement_snapshot_required {
            return Ok(None);
        }
        let profile_id = current.profile_id.clone();
        let now = runtime_now()?;
        let projection = self.catalog_projection(&profile_id, now)?;
        if projection == current.last_projection {
            return Ok(None);
        }
        let subscription = self
            .catalog_subscriptions
            .get_mut(authenticated_client_id)
            .ok_or_else(|| {
                runtime_error(
                    ErrorCode::Conflict,
                    "computer catalog subscription changed",
                    true,
                )
            })?;
        let event = if let Some(changed) = projection.clone() {
            subscription.sequence = subscription.sequence.checked_add(1).ok_or_else(|| {
                runtime_error(
                    ErrorCode::Conflict,
                    "computer catalog sequence exhausted",
                    false,
                )
            })?;
            ConversationProtocolEvent::Delta(TeammatesDelta::ComputerChanged(changed))
        } else {
            subscription.generation = subscription.generation.checked_add(1).ok_or_else(|| {
                runtime_error(
                    ErrorCode::Conflict,
                    "computer catalog generation exhausted",
                    false,
                )
            })?;
            subscription.sequence = 0;
            subscription.subscription_id = EntityId::new();
            subscription.authority_key = StableKey::parse(format!(
                "computer/catalog/{}/{}",
                profile_id,
                EntityId::new(),
            ))
            .map_err(|_| {
                runtime_error(
                    ErrorCode::Internal,
                    "computer catalog authority could not be replaced",
                    false,
                )
            })?;
            ConversationProtocolEvent::Snapshot(TeammatesSnapshot::Computers(ComputerSnapshot {
                computers: Vec::new(),
            }))
        };
        subscription.last_projection = projection;
        let envelope = ConversationProtocolEnvelope {
            version: TEAMMATES_PROTOCOL_VERSION,
            subscription_id: subscription.subscription_id.clone(),
            subject_profile_id: Some(subscription.profile_id.clone()),
            origin_server_instance_id: subscription.origin_server_instance_id.clone(),
            authority_key: subscription.authority_key.clone(),
            producer: TEAMMATES_PROTOCOL_PRODUCER.into(),
            generation: subscription.generation,
            sequence: subscription.sequence,
            event,
        };
        Ok(Some(EventEnvelope {
            protocol: CURRENT_PROTOCOL_VERSION,
            root_tree_id: subscription.root_tree_id.clone(),
            generation: Generation::new(subscription.generation),
            first_sequence: Sequence::new(0),
            sequence: Sequence::new(subscription.sequence),
            occurred_at: now,
            event: DaemonEvent::Teammates(Box::new(envelope)),
        }))
    }

    fn ensure_host_running(
        &self,
        profile_id: &ProfileId,
        now: UtcTimestamp,
    ) -> Result<(), ComputerCommandRuntimeError> {
        let lifecycle = self
            .lifecycle
            .get(profile_id)
            .map_err(host_error)?
            .ok_or_else(|| {
                runtime_error(
                    ErrorCode::NotFound,
                    "computer profile does not exist",
                    false,
                )
            })?;
        let computer = self
            .host
            .repository()
            .computer(profile_id)
            .map_err(host_error)?
            .ok_or_else(|| runtime_error(ErrorCode::NotFound, "computer does not exist", false))?;
        if computer.state != ComputerState::Ready {
            return Ok(());
        }
        let policy = computer_boundary_policy(&lifecycle);
        self.host
            .reconcile(profile_id, policy, now)
            .map_err(host_error)?;
        self.owners
            .lock()
            .map_err(|_| {
                runtime_error(
                    ErrorCode::Internal,
                    "computer owner registry is unavailable",
                    true,
                )
            })?
            .insert(profile_id.clone());
        Ok(())
    }
}

impl Drop for ComputerHostRuntime {
    fn drop(&mut self) {
        self.recovery_shutdown.store(true, Ordering::Release);
        if let Some(recovery) = self.recovery_thread.take() {
            recovery.thread().unpark();
            let _ = recovery.join();
        }
        self.sessions.clear();
        let owners = self
            .owners
            .lock()
            .map(|owners| owners.iter().cloned().collect::<Vec<_>>())
            .unwrap_or_default();
        for owner in &owners {
            let _ = self.host.shutdown(owner);
        }
    }
}

impl ComputerCommandRuntime for ComputerHostRuntime {
    fn execute(
        &mut self,
        authenticated_client_id: &ClientId,
        daemon_instance_id: &EntityId,
        command: ComputerProtocolCommand,
    ) -> Result<ComputerProtocolResponse, ComputerCommandRuntimeError> {
        match command {
            ComputerProtocolCommand::Open(command) => {
                if let Some(current) = self.sessions.get(authenticated_client_id) {
                    let descriptor = current.session.descriptor();
                    let requested =
                        stream_subject(command.profile_id.clone(), command.computer_id.clone());
                    if descriptor.subject != requested {
                        return Err(runtime_error(
                            ErrorCode::Unauthorized,
                            "computer stream subject changed",
                            false,
                        ));
                    }
                    if let Some(resume) = command.resume {
                        if stream_origin_from_projection(resume.origin) != descriptor.origin
                            || resume.cursor.generation != descriptor.cursor.generation
                            || resume.cursor.sequence != descriptor.cursor.sequence
                        {
                            return Err(runtime_error(
                                ErrorCode::Conflict,
                                "computer stream resume cursor is not current",
                                true,
                            ));
                        }
                    }
                    return Ok(ComputerProtocolResponse::Opened(descriptor_projection(
                        descriptor,
                    )));
                }
                if command.resume.is_some() {
                    return Err(runtime_error(
                        ErrorCode::Conflict,
                        "computer stream resume authority is no longer live",
                        true,
                    ));
                }
                let now = runtime_now()?;
                self.ensure_host_running(&command.profile_id, now)?;
                let mut record = self
                    .host
                    .repository()
                    .computer(&command.profile_id)
                    .map_err(host_error)?
                    .ok_or_else(|| {
                        runtime_error(ErrorCode::NotFound, "computer does not exist", false)
                    })?;
                if record.computer_id != command.computer_id
                    || record.owner_profile_id != command.profile_id
                {
                    return Err(runtime_error(
                        ErrorCode::Unauthorized,
                        "computer stream subject is not owned by the profile",
                        false,
                    ));
                }
                if record.control_state == ControlState::Idle && record.current_task_key.is_none() {
                    let task_key =
                        StableKey::parse(format!("computer/observe/{}", authenticated_client_id,))
                            .map_err(|_| {
                                runtime_error(
                                    ErrorCode::Internal,
                                    "computer observation authority could not be issued",
                                    false,
                                )
                            })?;
                    match self
                        .host
                        .acquire_task(
                            ComputerTaskRequest {
                                owner_profile_id: command.profile_id.clone(),
                                task_key,
                                actor: ComputerActor::Agent {
                                    profile_id: command.profile_id.clone(),
                                },
                                conflict: TaskConflictPolicy::Deny,
                            },
                            now,
                        )
                        .map_err(host_error)?
                    {
                        TaskAdmission::Acquired(_) => {
                            record = self
                                .host
                                .repository()
                                .computer(&command.profile_id)
                                .map_err(host_error)?
                                .ok_or_else(|| {
                                    runtime_error(
                                        ErrorCode::NotFound,
                                        "computer disappeared during observation",
                                        true,
                                    )
                                })?;
                        }
                        TaskAdmission::Queued { .. } | TaskAdmission::Denied => {
                            return Err(runtime_error(
                                ErrorCode::Conflict,
                                "computer observation is currently busy",
                                true,
                            ));
                        }
                    }
                }
                if record.control_state != ControlState::Agent {
                    return Err(runtime_error(
                        ErrorCode::Conflict,
                        "computer is not under agent control",
                        true,
                    ));
                }
                let task_key = record.current_task_key.clone().ok_or_else(|| {
                    runtime_error(
                        ErrorCode::Conflict,
                        "computer has no active task authority",
                        true,
                    )
                })?;
                let subject = stream_subject(command.profile_id, command.computer_id);
                let origin = ComputerStreamOrigin {
                    server_instance_id: daemon_instance_id.clone(),
                    stream_instance_id: EntityId::new(),
                    authority_key: StableKey::parse(format!(
                        "computer/stream/{}",
                        EntityId::new().as_str()
                    ))
                    .map_err(|_| {
                        runtime_error(
                            ErrorCode::Internal,
                            "failed to issue stream authority",
                            false,
                        )
                    })?,
                    generation: 1,
                };
                let controller = ComputerStreamController::Agent {
                    profile_id: subject.profile_id.clone(),
                    task_key,
                    fencing_token: record.revision.get(),
                };
                let expires = timestamp_after(now, 30_000)?;
                let authorization = self
                    .host
                    .authorize_stream(subject.clone(), origin.clone(), controller, now, expires)
                    .map_err(host_error)?;
                let session = ComputerStreamSession::open(
                    EntityId::new(),
                    ComputerStreamOpenRequest {
                        subject,
                        resume: None,
                    },
                    authorization,
                    ComputerStreamLimits::STRICT,
                    now,
                    timestamp_after(now, 10_000)?,
                )
                .map_err(stream_error)?;
                let descriptor = session.descriptor();
                self.sessions.insert(
                    authenticated_client_id.clone(),
                    ClientComputerStream {
                        session,
                        root_tree_id: RootTreeId::new(),
                    },
                );
                Ok(ComputerProtocolResponse::Opened(descriptor_projection(
                    descriptor,
                )))
            }
            ComputerProtocolCommand::Input(command) => {
                let request_id = command.request_id;
                let subject = stream_subject_from_projection(command.subject);
                let controller = controller_from_projection(command.controller);
                let input_summary = input_audit_summary(&command.payload);
                let audit_context = ComputerAuditContext {
                    operation_key: StableKey::parse(format!(
                        "computer/input/{}",
                        request_id.as_str(),
                    ))
                    .map_err(|_| {
                        runtime_error(
                            ErrorCode::Internal,
                            "computer input audit key is invalid",
                            false,
                        )
                    })?,
                    owner_profile_id: subject.profile_id.clone(),
                    computer_id: subject.computer_id.clone(),
                    actor: audit_actor(&controller),
                    task_key: Some(controller.task_key().clone()),
                    occurred_at: runtime_now()?,
                    expected_computer_revision: command.expected_computer_revision,
                };
                let input = ComputerInputCommand {
                    session_id: command.session_id,
                    subject,
                    origin: stream_origin_from_projection(command.origin),
                    sequence: command.sequence,
                    expected_computer_revision: command.expected_computer_revision,
                    controller,
                    payload: input_from_projection(command.payload),
                };
                let now = runtime_now()?;
                let result = {
                    let stream =
                        self.sessions
                            .get_mut(authenticated_client_id)
                            .ok_or_else(|| {
                                runtime_error(
                                    ErrorCode::Unauthorized,
                                    "computer stream is not open",
                                    true,
                                )
                            })?;
                    self.host.dispatch_stream_input(
                        &mut stream.session,
                        input,
                        now,
                        timestamp_after(now, 10_000)?,
                    )
                };
                let receipt = match result {
                    Ok(receipt) => receipt,
                    Err(error) => {
                        let _ = self.audit.append_typed(
                            audit_context.clone(),
                            TypedComputerAuditEvent::Policy {
                                decision: PolicyDecision::Denied,
                                safe_summary: format!("computer {input_summary} input was denied"),
                            },
                        );
                        let _ = self.audit.append_typed(
                            audit_context,
                            TypedComputerAuditEvent::Failure {
                                safe_failure: "computer stream input failed at the authoritative host boundary".into(),
                                safe_summary: "computer input failure was contained".into(),
                            },
                        );
                        return Err(host_error(error));
                    }
                };
                self.audit
                    .append_typed(
                        audit_context.clone(),
                        TypedComputerAuditEvent::Policy {
                            decision: PolicyDecision::Allowed,
                            safe_summary: format!("computer {input_summary} input was authorized"),
                        },
                    )
                    .map_err(audit_error)?;
                self.audit
                    .append_typed(
                        audit_context,
                        TypedComputerAuditEvent::Input {
                            transition: None,
                            safe_summary: format!("computer {input_summary} input was applied"),
                        },
                    )
                    .map_err(audit_error)?;
                Ok(ComputerProtocolResponse::InputApplied(
                    ComputerStreamInputReceiptProjection {
                        request_id,
                        sequence: receipt.sequence,
                        computer_revision: receipt.computer_revision,
                        takeover_lease_id: receipt.takeover_lease_id,
                    },
                ))
            }
            ComputerProtocolCommand::AcquireTakeover(command) => {
                let stream = self
                    .sessions
                    .get_mut(authenticated_client_id)
                    .ok_or_else(|| {
                        runtime_error(ErrorCode::Unauthorized, "computer stream is not open", true)
                    })?;
                validate_takeover_scope(
                    &stream.session,
                    &command.session_id,
                    &command.subject,
                    &command.origin,
                    command.expected_computer_revision,
                    &command.controller,
                )?;
                if command.task_key != *command.controller.task_key() {
                    return Err(runtime_error(
                        ErrorCode::Unauthorized,
                        "takeover task authority changed",
                        false,
                    ));
                }
                let now = runtime_now()?;
                let replacement_token = issue_takeover_token()?;
                let boundary = DurableTakeoverBoundary {
                    host: &self.host,
                    tasks: &self.task_boundaries,
                };
                let claim = self
                    .takeovers
                    .acquire(
                        TakeoverAcquireRequest {
                            owner_profile_id: command.subject.profile_id,
                            expected_computer_revision: command.expected_computer_revision,
                            task_key: command.task_key,
                            token_digest_hex: token_digest(&replacement_token),
                            lease_millis: command.lease_millis,
                            operation_key: command.operation_key,
                            now,
                        },
                        &boundary,
                    )
                    .map_err(takeover_error)?;
                let descriptor = reopen_takeover_session(
                    &self.host,
                    &mut stream.session,
                    daemon_instance_id,
                    &claim,
                    now,
                )?;
                Ok(ComputerProtocolResponse::TakeoverAcquired(
                    ComputerTakeoverReceiptProjection {
                        request_id: command.request_id,
                        claim: claim_projection(claim),
                        replacement_token,
                        computer_revision: descriptor.computer_revision,
                        descriptor,
                    },
                ))
            }
            ComputerProtocolCommand::RenewTakeover(command) => {
                let stream = self
                    .sessions
                    .get_mut(authenticated_client_id)
                    .ok_or_else(|| {
                        runtime_error(ErrorCode::Unauthorized, "computer stream is not open", true)
                    })?;
                validate_takeover_scope(
                    &stream.session,
                    &command.session_id,
                    &command.subject,
                    &command.origin,
                    command.expected_computer_revision,
                    &command.controller,
                )?;
                let now = runtime_now()?;
                let replacement_token = issue_takeover_token()?;
                let claim = self
                    .takeovers
                    .renew(TakeoverRenewRequest {
                        claim: claim_from_projection(command.claim),
                        presented_token_digest_hex: token_digest(&command.presented_token),
                        replacement_token_digest_hex: token_digest(&replacement_token),
                        lease_millis: command.lease_millis,
                        operation_key: command.operation_key,
                        now,
                    })
                    .map_err(takeover_error)?;
                let descriptor = reopen_takeover_session(
                    &self.host,
                    &mut stream.session,
                    daemon_instance_id,
                    &claim,
                    now,
                )?;
                Ok(ComputerProtocolResponse::TakeoverRenewed(
                    ComputerTakeoverReceiptProjection {
                        request_id: command.request_id,
                        claim: claim_projection(claim),
                        replacement_token,
                        computer_revision: descriptor.computer_revision,
                        descriptor,
                    },
                ))
            }
            ComputerProtocolCommand::HandBack(command) => {
                let stream = self
                    .sessions
                    .get_mut(authenticated_client_id)
                    .ok_or_else(|| {
                        runtime_error(ErrorCode::Unauthorized, "computer stream is not open", true)
                    })?;
                validate_takeover_scope(
                    &stream.session,
                    &command.session_id,
                    &command.subject,
                    &command.origin,
                    command.expected_computer_revision,
                    &command.controller,
                )?;
                let now = runtime_now()?;
                let boundary = DurableTakeoverBoundary {
                    host: &self.host,
                    tasks: &self.task_boundaries,
                };
                let user_acted = TakeoverUserActedNote::new(
                    command.operation_key.clone(),
                    UserActedKind::Other,
                    "owner acted during authenticated takeover and handed control back",
                    now,
                )
                .map_err(takeover_error)?;
                let receipt = self
                    .takeovers
                    .handback_user_acted(
                        TakeoverHandbackRequest {
                            claim: claim_from_projection(command.claim),
                            presented_token_digest_hex: token_digest(&command.presented_token),
                            operation_key: command.operation_key,
                            now,
                        },
                        user_acted,
                        &boundary,
                    )
                    .map_err(takeover_error)?;
                let (disposition, observation, reason) = match receipt.owning_task {
                    OwningTaskResumeOrFail::Resumed { observation } => (
                        ComputerHandBackDispositionProjection::Resumed,
                        Some(observation.observation_key),
                        None,
                    ),
                    OwningTaskResumeOrFail::Failed { safe_reason } => (
                        ComputerHandBackDispositionProjection::OwningTaskFailed,
                        None,
                        Some(safe_reason),
                    ),
                };
                let claim = receipt.claim;
                let descriptor = reopen_agent_session(
                    &self.host,
                    &mut stream.session,
                    daemon_instance_id,
                    &claim.owner_profile_id,
                    now,
                )?;
                Ok(ComputerProtocolResponse::HandedBack(
                    ComputerHandBackReceiptProjection {
                        request_id: command.request_id,
                        claim: claim_projection(claim),
                        disposition,
                        refreshed_observation_key: observation,
                        safe_reason: reason,
                        computer_revision: descriptor.computer_revision,
                        descriptor,
                    },
                ))
            }
            ComputerProtocolCommand::InjectSecret(command) => {
                let subject = stream_subject_from_projection(command.subject.clone());
                let controller = controller_from_projection(command.controller.clone());
                {
                    let stream = self.sessions.get(authenticated_client_id).ok_or_else(|| {
                        runtime_error(ErrorCode::Unauthorized, "computer stream is not open", true)
                    })?;
                    validate_takeover_scope(
                        &stream.session,
                        &command.session_id,
                        &command.subject,
                        &command.origin,
                        command.expected_computer_revision,
                        &command.controller,
                    )?;
                }
                let ComputerStreamController::UserTakeover {
                    profile_id,
                    task_key,
                    fencing_token,
                    ..
                } = &controller
                else {
                    return Err(runtime_error(
                        ErrorCode::Unauthorized,
                        "secret injection requires current owner takeover authority",
                        false,
                    ));
                };
                if profile_id != &subject.profile_id
                    || task_key != &command.task_key
                    || *fencing_token != command.task_fencing_token
                {
                    return Err(runtime_error(
                        ErrorCode::Unauthorized,
                        "secret injection task authority is stale or forged",
                        false,
                    ));
                }
                let credential_owner = match command.credential_ref.owner {
                    ComputerCredentialOwnerProjection::Provider(value) => {
                        CredentialOwner::Provider(value)
                    }
                    ComputerCredentialOwnerProjection::Channel(value) => {
                        CredentialOwner::Channel(value)
                    }
                    ComputerCredentialOwnerProjection::Mcp(value) => CredentialOwner::Mcp(value),
                    ComputerCredentialOwnerProjection::Tool(value) => CredentialOwner::Tool(value),
                };
                let credential_ref =
                    CredentialRef::new(command.credential_ref.name, credential_owner).map_err(
                        |_| {
                            runtime_error(
                                ErrorCode::InvalidInput,
                                "computer credential reference is invalid",
                                false,
                            )
                        },
                    )?;
                let target = match command.target {
                    ComputerSecretInjectionTargetProjection::FocusedField {
                        exact_origin,
                        frame_origin,
                        field_id,
                        expected_focus_revision,
                    } => SecretInjectionTarget::FocusedField {
                        exact_origin,
                        frame_origin,
                        field_id,
                        focus_revision: expected_focus_revision,
                    },
                    ComputerSecretInjectionTargetProjection::CredentialBroker {
                        exact_origin,
                        broker_id,
                    } => SecretInjectionTarget::CredentialBroker {
                        exact_origin,
                        broker_id,
                    },
                };
                let now = runtime_now()?;
                let request_id = command.request_id;
                let request = SecretInjectionRequest {
                    operation_key: command.operation_key,
                    claimed_profile_id: subject.profile_id.clone(),
                    computer_id: subject.computer_id.clone(),
                    task_key: command.task_key,
                    task_fencing_token: command.task_fencing_token,
                    computer_revision: command.expected_computer_revision,
                    policy_revision: command.expected_policy_revision,
                    credential_ref,
                    target,
                    owner_approved: true,
                };
                let authenticated_profile_id = request.claimed_profile_id.clone();
                let writer = self.host.secret_writer(subject, controller);
                let receipt = self
                    .secrets
                    .inject(&authenticated_profile_id, request, writer, now)
                    .map_err(secret_error)?;
                Ok(ComputerProtocolResponse::SecretInjected(
                    ComputerSecretInjectionReceiptProjection {
                        request_id,
                        operation_key: receipt.operation_key,
                        profile_id: receipt.profile_id,
                        computer_id: receipt.computer_id,
                        task_key: receipt.task_key,
                        task_fencing_token: receipt.task_fencing_token,
                        computer_revision: receipt.computer_revision,
                        policy_revision: receipt.policy_revision,
                        target_kind: match receipt.target_kind {
                            SecretInjectionTargetKind::FocusedField => {
                                ComputerSecretInjectionTargetKindProjection::FocusedField
                            }
                            SecretInjectionTargetKind::CredentialBroker => {
                                ComputerSecretInjectionTargetKindProjection::CredentialBroker
                            }
                        },
                        injected_at: receipt.injected_at,
                    },
                ))
            }
        }
    }

    fn drain_events(
        &mut self,
        authenticated_client_id: &ClientId,
        max_events: usize,
    ) -> Result<Vec<EventEnvelope>, ComputerCommandRuntimeError> {
        if max_events == 0 {
            return Ok(Vec::new());
        }
        let mut events = Vec::new();
        if let Some(event) = self.catalog_delta(authenticated_client_id)? {
            events.push(event);
            if events.len() == max_events {
                return Ok(events);
            }
        }
        let Some(stream) = self.sessions.get_mut(authenticated_client_id) else {
            return Ok(events);
        };
        let now = runtime_now()?;
        let descriptor_before = stream.session.descriptor();
        let failure_context = ComputerAuditContext {
            operation_key: StableKey::parse(format!(
                "computer/frame/{}/{}/{}",
                descriptor_before.session_id,
                descriptor_before.cursor.sequence.saturating_add(1),
                now.unix_millis(),
            ))
            .map_err(|_| {
                runtime_error(
                    ErrorCode::Internal,
                    "computer frame audit key is invalid",
                    false,
                )
            })?,
            owner_profile_id: descriptor_before.subject.profile_id.clone(),
            computer_id: descriptor_before.subject.computer_id.clone(),
            actor: audit_actor(&descriptor_before.controller),
            task_key: Some(descriptor_before.controller.task_key().clone()),
            occurred_at: now,
            expected_computer_revision: descriptor_before.computer_revision,
        };
        let capture =
            self.host
                .capture_stream_frame(&mut stream.session, now, timestamp_after(now, 10_000)?);
        let frame = match capture {
            Ok(frame) => frame,
            Err(error) => {
                let _ = self.audit.append_typed(
                    failure_context,
                    TypedComputerAuditEvent::Failure {
                        safe_failure:
                            "computer frame capture failed at the authoritative host boundary"
                                .into(),
                        safe_summary: "computer stream capture failure was contained".into(),
                    },
                );
                return Err(host_error(error));
            }
        };
        let descriptor = stream.session.descriptor();
        let projected_frame = frame_projection(frame.clone());
        let sequence = Sequence::new(frame.sequence);
        events.push(EventEnvelope {
            protocol: CURRENT_PROTOCOL_VERSION,
            root_tree_id: stream.root_tree_id.clone(),
            generation: Generation::new(frame.origin.generation),
            first_sequence: Sequence::new(1),
            sequence,
            occurred_at: frame.captured_at,
            event: DaemonEvent::Computer(Box::new(ComputerProtocolEventEnvelope {
                version: COMPUTER_PROTOCOL_VERSION,
                producer: COMPUTER_PROTOCOL_PRODUCER.to_owned(),
                session_id: frame.session_id,
                subject: subject_projection(frame.subject),
                origin: origin_projection(frame.origin),
                computer_revision: descriptor.computer_revision,
                controller: controller_projection(descriptor.controller),
                sequence: frame.sequence,
                event: ComputerProtocolEvent::KeyFrame(projected_frame),
            })),
        });
        Ok(events)
    }

    fn resume_catalog(
        &mut self,
        authenticated_client_id: &ClientId,
        daemon_instance_id: &EntityId,
        request: ResumeConversationEventsCommand,
    ) -> Result<ConversationProtocolEnvelope, ComputerCommandRuntimeError> {
        if request.conversation_id.is_some() {
            return Err(runtime_error(
                ErrorCode::InvalidInput,
                "computer catalog subscriptions cannot name a conversation",
                false,
            ));
        }
        let profile_id = request.profile_id.ok_or_else(|| {
            runtime_error(
                ErrorCode::Unauthorized,
                "computer catalog subscriptions require an exact profile",
                false,
            )
        })?;
        let now = runtime_now()?;
        let projection = self.catalog_projection(&profile_id, now)?;
        let replacement_snapshot_required =
            request.cursor.generation != 0 || request.cursor.sequence != 0;
        let generation = if replacement_snapshot_required {
            request.cursor.generation.checked_add(1).ok_or_else(|| {
                runtime_error(
                    ErrorCode::Conflict,
                    "computer catalog generation exhausted",
                    false,
                )
            })?
        } else {
            1
        };
        let subscription_id = EntityId::new();
        let authority_key = StableKey::parse(format!(
            "computer/catalog/{}/{}",
            profile_id,
            EntityId::new(),
        ))
        .map_err(|_| {
            runtime_error(
                ErrorCode::Internal,
                "computer catalog authority could not be issued",
                false,
            )
        })?;
        let event = if replacement_snapshot_required {
            ConversationProtocolEvent::ResumeGap(ConversationResumeGap {
                requested: request.cursor,
                current_generation: generation,
                oldest_available_sequence: 0,
                replacement_snapshot_required: true,
            })
        } else {
            ConversationProtocolEvent::Snapshot(TeammatesSnapshot::Computers(ComputerSnapshot {
                computers: projection.clone().into_iter().collect(),
            }))
        };
        let envelope = ConversationProtocolEnvelope {
            version: TEAMMATES_PROTOCOL_VERSION,
            subscription_id: subscription_id.clone(),
            subject_profile_id: Some(profile_id.clone()),
            origin_server_instance_id: daemon_instance_id.clone(),
            authority_key: authority_key.clone(),
            producer: TEAMMATES_PROTOCOL_PRODUCER.into(),
            generation,
            sequence: 0,
            event,
        };
        self.catalog_subscriptions.insert(
            authenticated_client_id.clone(),
            ComputerCatalogSubscription {
                profile_id,
                subscription_id,
                authority_key,
                origin_server_instance_id: daemon_instance_id.clone(),
                generation,
                sequence: 0,
                root_tree_id: RootTreeId::new(),
                last_projection: projection,
                replacement_snapshot_required,
            },
        );
        Ok(envelope)
    }

    fn disconnect(&mut self, authenticated_client_id: &ClientId) {
        if let Some(stream) = self.sessions.remove(authenticated_client_id) {
            let descriptor = stream.session.descriptor();
            let task_key = descriptor.controller.task_key();
            if task_key.as_str().starts_with("computer/observe/")
                && let Ok(Some(record)) = self
                    .host
                    .repository()
                    .computer(&descriptor.subject.profile_id)
                && record.control_state == ControlState::Agent
                && record.current_task_key.as_ref() == Some(task_key)
            {
                let _ = self.host.release_task(
                    &ComputerTaskLease {
                        owner_profile_id: descriptor.subject.profile_id,
                        task_key: task_key.clone(),
                        fencing_token: record.revision.get(),
                        computer_revision: record.revision,
                    },
                    UtcTimestamp::now().unwrap_or(UtcTimestamp::UNIX_EPOCH),
                );
            }
        }
        self.catalog_subscriptions.remove(authenticated_client_id);
    }
}

fn runtime_now() -> Result<UtcTimestamp, ComputerCommandRuntimeError> {
    UtcTimestamp::now()
        .map_err(|_| runtime_error(ErrorCode::Internal, "host clock is unavailable", true))
}

fn computer_boundary_policy(agent: &keith_profile::AgentLifecycleRecord) -> ComputerBoundaryPolicy {
    let workspace_root = agent.profile.resources.workspace_root.clone();
    let computer_policy = &agent.presentation.computer_policy;
    ComputerBoundaryPolicy {
        allowed_origins: Vec::new(),
        writable_roots: vec![workspace_root.clone()],
        download_root: workspace_root.join("downloads"),
        allow_credentials: false,
        resources: ComputerResourcePolicy {
            cpu_quota_percent: 100,
            max_memory_bytes: 2 * 1_024 * 1_024 * 1_024,
            max_processes: 512,
            max_disk_bytes: 10 * 1_024 * 1_024 * 1_024,
            max_download_bytes: 256 * 1_024 * 1_024,
            max_network_requests_per_minute: 120,
            idle_timeout_seconds: u64::from(computer_policy.max_idle_seconds),
            crash_limit: 3,
            crash_window_seconds: 300,
        },
    }
}

fn timestamp_after(
    now: UtcTimestamp,
    millis: i64,
) -> Result<UtcTimestamp, ComputerCommandRuntimeError> {
    now.unix_millis()
        .checked_add(millis)
        .map(UtcTimestamp::from_unix_millis)
        .ok_or_else(|| {
            runtime_error(
                ErrorCode::Internal,
                "computer stream deadline overflowed",
                false,
            )
        })
}

fn runtime_error(
    code: ErrorCode,
    message: impl Into<String>,
    retryable: bool,
) -> ComputerCommandRuntimeError {
    ComputerCommandRuntimeError::new(code, message, retryable)
}

fn host_error(error: impl std::fmt::Display) -> ComputerCommandRuntimeError {
    runtime_error(ErrorCode::Unavailable, error.to_string(), true)
}

fn stream_error(error: impl std::fmt::Display) -> ComputerCommandRuntimeError {
    runtime_error(ErrorCode::InvalidInput, error.to_string(), false)
}

fn secret_error(error: ComputerSecretInjectionServiceError) -> ComputerCommandRuntimeError {
    let (code, retryable) = match &error {
        ComputerSecretInjectionServiceError::Replay => (ErrorCode::Conflict, false),
        ComputerSecretInjectionServiceError::State => (ErrorCode::Internal, true),
        ComputerSecretInjectionServiceError::Authority
        | ComputerSecretInjectionServiceError::Injection(_) => (ErrorCode::Forbidden, false),
    };
    runtime_error(code, error.to_string(), retryable)
}

fn audit_error(error: impl std::fmt::Display) -> ComputerCommandRuntimeError {
    runtime_error(ErrorCode::Internal, error.to_string(), true)
}

fn audit_actor(controller: &ComputerStreamController) -> AuditActor {
    match controller {
        ComputerStreamController::UserTakeover { .. } => AuditActor::Owner,
        ComputerStreamController::Agent { profile_id, .. }
        | ComputerStreamController::Routine { profile_id, .. }
        | ComputerStreamController::Child { profile_id, .. } => {
            AuditActor::Profile(profile_id.clone())
        }
    }
}

fn input_audit_summary(input: &ComputerInputPayloadProjection) -> &'static str {
    match input {
        ComputerInputPayloadProjection::PointerMove { .. } => "pointer-move",
        ComputerInputPayloadProjection::PointerButton { .. } => "pointer-button",
        ComputerInputPayloadProjection::Scroll { .. } => "scroll",
        ComputerInputPayloadProjection::Key { .. } => "key",
        ComputerInputPayloadProjection::Text { .. } => "text",
        ComputerInputPayloadProjection::CredentialReference { .. } => "credential-reference",
        ComputerInputPayloadProjection::Focus => "focus",
        ComputerInputPayloadProjection::ReleaseAll => "release-all",
    }
}

fn append_recovery_audit(
    audit: &ComputerAuditService<DaemonComputerRepository>,
    host: &ComputerHost<DaemonComputerRepository>,
    resolution: &TakeoverResolution,
    now: UtcTimestamp,
) -> Result<(), keith_computer::ComputerAuditServiceError> {
    let (claim, summary) = match resolution {
        TakeoverResolution::Resumed { claim, .. } => {
            (claim, "expired takeover recovered and owning task resumed")
        }
        TakeoverResolution::OwningTaskFailed { claim, .. } => (
            claim,
            "expired takeover recovered and owning task failed safely",
        ),
    };
    let Some(record) = host.repository().computer(&claim.owner_profile_id)? else {
        return Err(keith_computer::ComputerAuditServiceError::StaleSubject);
    };
    let operation_key = StableKey::parse(format!(
        "computer/recovery/{}/{}",
        claim.takeover_lease_id, claim.fencing_token,
    ))
    .map_err(|_| keith_computer::ComputerAuditServiceError::InvalidEventKey)?;
    let correlation_key = StableKey::parse(format!(
        "takeover/{}/{}",
        claim.takeover_lease_id,
        claim.lease_revision.get(),
    ))
    .map_err(|_| keith_computer::ComputerAuditServiceError::InvalidEventKey)?;
    let stable_key = StableKey::parse(format!("computer-audit/{operation_key}/recovery",))
        .map_err(|_| keith_computer::ComputerAuditServiceError::InvalidEventKey)?;
    if let Some(existing) = audit
        .history(&claim.owner_profile_id)?
        .into_iter()
        .find(|event| event.stable_key == stable_key)
    {
        if existing.computer_id == claim.computer_id
            && existing.owner_profile_id == claim.owner_profile_id
            && existing.actor == AuditActor::System
            && existing.kind == ComputerAuditKind::Recovery
            && existing.task_key.as_ref() == Some(&claim.task_key)
            && existing.recovery_correlation.as_ref() == Some(&correlation_key)
        {
            return Ok(());
        }
        return Err(keith_computer::ComputerAuditServiceError::StableKeyConflict);
    }
    audit.append_typed(
        ComputerAuditContext {
            operation_key,
            owner_profile_id: claim.owner_profile_id.clone(),
            computer_id: claim.computer_id.clone(),
            actor: AuditActor::System,
            task_key: Some(claim.task_key.clone()),
            occurred_at: now,
            expected_computer_revision: record.revision,
        },
        TypedComputerAuditEvent::Recovery {
            correlation_key,
            safe_summary: summary.into(),
        },
    )?;
    Ok(())
}

fn stream_subject(
    profile_id: ProfileId,
    computer_id: keith_agent_types::ComputerId,
) -> ComputerStreamSubject {
    ComputerStreamSubject {
        profile_id,
        computer_id,
    }
}

fn stream_subject_from_projection(value: ComputerStreamSubjectProjection) -> ComputerStreamSubject {
    stream_subject(value.profile_id, value.computer_id)
}

fn subject_projection(value: ComputerStreamSubject) -> ComputerStreamSubjectProjection {
    ComputerStreamSubjectProjection {
        profile_id: value.profile_id,
        computer_id: value.computer_id,
    }
}

fn stream_origin_from_projection(value: ComputerStreamOriginProjection) -> ComputerStreamOrigin {
    ComputerStreamOrigin {
        server_instance_id: value.server_instance_id,
        stream_instance_id: value.stream_instance_id,
        authority_key: value.authority_key,
        generation: value.generation,
    }
}

fn origin_projection(value: ComputerStreamOrigin) -> ComputerStreamOriginProjection {
    ComputerStreamOriginProjection {
        server_instance_id: value.server_instance_id,
        stream_instance_id: value.stream_instance_id,
        authority_key: value.authority_key,
        generation: value.generation,
    }
}

fn controller_from_projection(
    value: ComputerStreamControllerProjection,
) -> ComputerStreamController {
    match value {
        ComputerStreamControllerProjection::Agent {
            profile_id,
            task_key,
            fencing_token,
        } => ComputerStreamController::Agent {
            profile_id,
            task_key,
            fencing_token,
        },
        ComputerStreamControllerProjection::Routine {
            profile_id,
            routine_id,
            task_key,
            fencing_token,
        } => ComputerStreamController::Routine {
            profile_id,
            routine_id,
            task_key,
            fencing_token,
        },
        ComputerStreamControllerProjection::Child {
            profile_id,
            child_id,
            task_key,
            fencing_token,
        } => ComputerStreamController::Child {
            profile_id,
            child_id,
            task_key,
            fencing_token,
        },
        ComputerStreamControllerProjection::UserTakeover {
            profile_id,
            lease_id,
            task_key,
            fencing_token,
            lease_revision,
        } => ComputerStreamController::UserTakeover {
            profile_id,
            lease_id,
            task_key,
            fencing_token,
            lease_revision,
        },
    }
}

fn controller_projection(value: ComputerStreamController) -> ComputerStreamControllerProjection {
    match value {
        ComputerStreamController::Agent {
            profile_id,
            task_key,
            fencing_token,
        } => ComputerStreamControllerProjection::Agent {
            profile_id,
            task_key,
            fencing_token,
        },
        ComputerStreamController::Routine {
            profile_id,
            routine_id,
            task_key,
            fencing_token,
        } => ComputerStreamControllerProjection::Routine {
            profile_id,
            routine_id,
            task_key,
            fencing_token,
        },
        ComputerStreamController::Child {
            profile_id,
            child_id,
            task_key,
            fencing_token,
        } => ComputerStreamControllerProjection::Child {
            profile_id,
            child_id,
            task_key,
            fencing_token,
        },
        ComputerStreamController::UserTakeover {
            profile_id,
            lease_id,
            task_key,
            fencing_token,
            lease_revision,
        } => ComputerStreamControllerProjection::UserTakeover {
            profile_id,
            lease_id,
            task_key,
            fencing_token,
            lease_revision,
        },
    }
}

fn input_from_projection(value: ComputerInputPayloadProjection) -> ComputerInputPayload {
    match value {
        ComputerInputPayloadProjection::PointerMove { x, y } => {
            ComputerInputPayload::PointerMove { x, y }
        }
        ComputerInputPayloadProjection::PointerButton {
            x,
            y,
            button,
            state,
        } => ComputerInputPayload::PointerButton {
            x,
            y,
            button: match button {
                ComputerPointerButtonProjection::Primary => ComputerPointerButton::Primary,
                ComputerPointerButtonProjection::Middle => ComputerPointerButton::Middle,
                ComputerPointerButtonProjection::Secondary => ComputerPointerButton::Secondary,
            },
            state: button_state(state),
        },
        ComputerInputPayloadProjection::Scroll { delta_x, delta_y } => {
            ComputerInputPayload::Scroll { delta_x, delta_y }
        }
        ComputerInputPayloadProjection::Key {
            code,
            state,
            alt,
            control,
            meta,
            shift,
        } => ComputerInputPayload::Key {
            code,
            state: button_state(state),
            alt,
            control,
            meta,
            shift,
        },
        ComputerInputPayloadProjection::Text { text } => ComputerInputPayload::Text { text },
        ComputerInputPayloadProjection::CredentialReference { grant_id } => {
            ComputerInputPayload::CredentialReference { grant_id }
        }
        ComputerInputPayloadProjection::Focus => ComputerInputPayload::Focus,
        ComputerInputPayloadProjection::ReleaseAll => ComputerInputPayload::ReleaseAll,
    }
}

fn button_state(value: ComputerButtonStateProjection) -> ComputerButtonState {
    match value {
        ComputerButtonStateProjection::Pressed => ComputerButtonState::Pressed,
        ComputerButtonStateProjection::Released => ComputerButtonState::Released,
    }
}

fn descriptor_projection(
    value: keith_computer::ComputerStreamDescriptor,
) -> ComputerStreamDescriptorProjection {
    ComputerStreamDescriptorProjection {
        session_id: value.session_id,
        subject: subject_projection(value.subject),
        computer_revision: value.computer_revision,
        origin: origin_projection(value.origin),
        cursor: ComputerStreamCursorProjection {
            generation: value.cursor.generation,
            sequence: value.cursor.sequence,
        },
        controller: controller_projection(value.controller),
        takeover_lease_id: value.takeover_lease_id,
        limits: ComputerStreamLimitsProjection {
            max_frame_bytes: value.limits.max_frame_bytes,
            max_input_bytes: value.limits.max_input_bytes,
            max_width: value.limits.max_width,
            max_height: value.limits.max_height,
        },
        connected_at: value.connected_at,
        liveness_deadline: value.liveness_deadline,
    }
}

fn frame_projection(value: keith_computer::ComputerFrame) -> ComputerFrameProjection {
    ComputerFrameProjection {
        captured_at: value.captured_at,
        width: value.width,
        height: value.height,
        encoding: match value.encoding {
            ComputerFrameEncoding::Png => ComputerFrameEncodingProjection::Png,
            ComputerFrameEncoding::Jpeg => ComputerFrameEncodingProjection::Jpeg,
            ComputerFrameEncoding::WebP => ComputerFrameEncodingProjection::WebP,
        },
        key_frame: value.key_frame,
        bytes_base64: base64::engine::general_purpose::STANDARD.encode(value.bytes),
    }
}

struct DurableTakeoverBoundary<'a> {
    host: &'a ComputerHost<DaemonComputerRepository>,
    tasks: &'a ComputerTakeoverTaskBoundaryService,
}

impl TakeoverTaskBoundary for DurableTakeoverBoundary<'_> {
    fn pause_agent_input(
        &self,
        boundary: &TakeoverPauseBoundary,
    ) -> Result<(), TakeoverBoundaryError> {
        let record = self
            .host
            .repository()
            .computer(&boundary.owner_profile_id)
            .map_err(|_| takeover_boundary_error("computer state could not be read"))?
            .ok_or_else(|| takeover_boundary_error("computer state is missing"))?;
        if record.computer_id != boundary.computer_id
            || record.current_task_key.as_ref() != Some(&boundary.task_key)
            || record.revision != boundary.expected_computer_revision
            || record.control_state != ControlState::Agent
        {
            return Err(takeover_boundary_error(
                "owning task authority changed before pause",
            ));
        }
        self.tasks
            .pause(
                boundary,
                runtime_now().map_err(|_| takeover_boundary_error("host clock is unavailable"))?,
            )
            .map_err(|_| takeover_boundary_error("owning task pause was not durable"))
    }

    fn release_uncommitted_pause(
        &self,
        boundary: &TakeoverPauseBoundary,
    ) -> Result<(), TakeoverBoundaryError> {
        let record = self
            .host
            .repository()
            .computer(&boundary.owner_profile_id)
            .map_err(|_| takeover_boundary_error("computer state could not be read"))?
            .ok_or_else(|| takeover_boundary_error("computer state is missing"))?;
        if record.computer_id != boundary.computer_id
            || record.current_task_key.as_ref() != Some(&boundary.task_key)
            || record.control_state != ControlState::Agent
        {
            return Err(takeover_boundary_error(
                "uncommitted pause could not be released",
            ));
        }
        self.tasks
            .release_uncommitted_pause(
                boundary,
                runtime_now().map_err(|_| takeover_boundary_error("host clock is unavailable"))?,
            )
            .map_err(|_| takeover_boundary_error("owning task pause rollback failed"))
    }

    fn refresh_observation(
        &self,
        boundary: &TakeoverResolutionBoundary,
    ) -> Result<RefreshedComputerObservation, TakeoverBoundaryError> {
        self.host
            .refresh_takeover_observation(
                boundary,
                runtime_now().map_err(|_| takeover_boundary_error("host clock is unavailable"))?,
            )
            .map_err(|_| takeover_boundary_error("computer observation refresh failed"))
    }

    fn resume_owning_task(
        &self,
        boundary: &TakeoverResolutionBoundary,
        observation: &RefreshedComputerObservation,
    ) -> Result<(), TakeoverBoundaryError> {
        let record = self
            .host
            .repository()
            .computer(&boundary.owner_profile_id)
            .map_err(|_| takeover_boundary_error("computer state could not be read"))?
            .ok_or_else(|| takeover_boundary_error("computer state is missing"))?;
        if record.computer_id != boundary.computer_id
            || record.current_task_key.as_ref() != Some(&boundary.task_key)
            || record.control_state != ControlState::Paused
            || observation.observed_at < record.updated_at
        {
            return Err(takeover_boundary_error(
                "owning task could not resume from refreshed state",
            ));
        }
        let lease = self
            .host
            .repository()
            .lease(&boundary.owner_profile_id)
            .map_err(|_| takeover_boundary_error("takeover state could not be read"))?
            .ok_or_else(|| takeover_boundary_error("takeover state is missing"))?;
        let kind = if lease.state == keith_computer::TakeoverState::Expired {
            ComputerTakeoverTaskNoteKind::TakeoverExpired
        } else {
            ComputerTakeoverTaskNoteKind::UserActed
        };
        self.tasks
            .resume(
                boundary,
                observation,
                kind,
                runtime_now().map_err(|_| takeover_boundary_error("host clock is unavailable"))?,
            )
            .map_err(|_| takeover_boundary_error("owning task resume was not durable"))
    }

    fn fail_owning_task(
        &self,
        boundary: &TakeoverResolutionBoundary,
        safe_reason: &str,
    ) -> Result<(), TakeoverBoundaryError> {
        if safe_reason.is_empty() {
            return Err(takeover_boundary_error(
                "owning task failure reason is empty",
            ));
        }
        let record = self
            .host
            .repository()
            .computer(&boundary.owner_profile_id)
            .map_err(|_| takeover_boundary_error("computer state could not be read"))?
            .ok_or_else(|| takeover_boundary_error("computer state is missing"))?;
        if record.computer_id != boundary.computer_id
            || record.current_task_key.as_ref() != Some(&boundary.task_key)
            || record.control_state != ControlState::Paused
        {
            return Err(takeover_boundary_error(
                "owning task failure boundary changed",
            ));
        }
        let lease = self
            .host
            .repository()
            .lease(&boundary.owner_profile_id)
            .map_err(|_| takeover_boundary_error("takeover state could not be read"))?
            .ok_or_else(|| takeover_boundary_error("takeover state is missing"))?;
        let kind = if lease.state == keith_computer::TakeoverState::Expired {
            ComputerTakeoverTaskNoteKind::TakeoverExpired
        } else {
            ComputerTakeoverTaskNoteKind::ResumeFailed
        };
        self.tasks
            .fail(
                boundary,
                kind,
                safe_reason,
                runtime_now().map_err(|_| takeover_boundary_error("host clock is unavailable"))?,
            )
            .map_err(|_| takeover_boundary_error("owning task failure was not durable"))
    }
}

fn takeover_boundary_error(reason: &'static str) -> TakeoverBoundaryError {
    TakeoverBoundaryError::new(reason)
        .unwrap_or_else(|_| unreachable!("static takeover boundary reason is valid"))
}

fn takeover_error(error: impl std::fmt::Display) -> ComputerCommandRuntimeError {
    runtime_error(ErrorCode::Conflict, error.to_string(), false)
}

fn issue_takeover_token() -> Result<StableKey, ComputerCommandRuntimeError> {
    StableKey::parse(format!("takeover/token/{}", EntityId::new().as_str()))
        .map_err(|_| runtime_error(ErrorCode::Internal, "failed to issue takeover token", false))
}

fn token_digest(token: &StableKey) -> String {
    let digest = Sha256::digest(token.as_str().as_bytes());
    let mut encoded = String::with_capacity(64);
    for byte in digest {
        use std::fmt::Write as _;
        let _ = write!(&mut encoded, "{byte:02x}");
    }
    encoded
}

fn stable_key_entity_id(key: &StableKey) -> EntityId {
    let digest = Sha256::digest(key.as_str().as_bytes());
    let mut bytes = [0_u8; 16];
    bytes.copy_from_slice(&digest[..16]);
    EntityId::from_u128(u128::from_be_bytes(bytes))
}

fn claim_from_projection(value: ComputerTakeoverClaimProjection) -> TakeoverClaim {
    TakeoverClaim {
        takeover_lease_id: value.takeover_lease_id,
        computer_id: value.computer_id,
        owner_profile_id: value.owner_profile_id,
        task_key: value.task_key,
        fencing_token: value.fencing_token,
        lease_revision: value.lease_revision,
        expires_at: value.expires_at,
    }
}

fn claim_projection(value: TakeoverClaim) -> ComputerTakeoverClaimProjection {
    ComputerTakeoverClaimProjection {
        takeover_lease_id: value.takeover_lease_id,
        computer_id: value.computer_id,
        owner_profile_id: value.owner_profile_id,
        task_key: value.task_key,
        fencing_token: value.fencing_token,
        lease_revision: value.lease_revision,
        expires_at: value.expires_at,
    }
}

fn validate_takeover_scope(
    session: &ComputerStreamSession,
    session_id: &EntityId,
    subject: &ComputerStreamSubjectProjection,
    origin: &ComputerStreamOriginProjection,
    revision: keith_agent_types::Revision,
    controller: &ComputerStreamControllerProjection,
) -> Result<(), ComputerCommandRuntimeError> {
    let current = session.descriptor();
    if &current.session_id != session_id
        || current.subject != stream_subject_from_projection(subject.clone())
        || current.origin != stream_origin_from_projection(origin.clone())
        || current.computer_revision != revision
        || current.controller != controller_from_projection(controller.clone())
    {
        return Err(runtime_error(
            ErrorCode::Unauthorized,
            "takeover stream authority is stale or forged",
            false,
        ));
    }
    Ok(())
}

fn next_origin(
    current: &ComputerStreamSession,
    daemon_instance_id: &EntityId,
) -> Result<ComputerStreamOrigin, ComputerCommandRuntimeError> {
    let generation = current
        .descriptor()
        .origin
        .generation
        .checked_add(1)
        .ok_or_else(|| {
            runtime_error(
                ErrorCode::Internal,
                "computer stream generation exhausted",
                false,
            )
        })?;
    Ok(ComputerStreamOrigin {
        server_instance_id: daemon_instance_id.clone(),
        stream_instance_id: EntityId::new(),
        authority_key: StableKey::parse(format!("computer/stream/{}", EntityId::new().as_str()))
            .map_err(|_| {
                runtime_error(
                    ErrorCode::Internal,
                    "failed to issue stream authority",
                    false,
                )
            })?,
        generation,
    })
}

fn reopen_takeover_session(
    host: &ComputerHost<DaemonComputerRepository>,
    session: &mut ComputerStreamSession,
    daemon_instance_id: &EntityId,
    claim: &TakeoverClaim,
    now: UtcTimestamp,
) -> Result<ComputerStreamDescriptorProjection, ComputerCommandRuntimeError> {
    let old = session.descriptor();
    let origin = next_origin(session, daemon_instance_id)?;
    let subject = ComputerStreamSubject {
        profile_id: claim.owner_profile_id.clone(),
        computer_id: claim.computer_id.clone(),
    };
    let controller = ComputerStreamController::UserTakeover {
        profile_id: claim.owner_profile_id.clone(),
        lease_id: claim.takeover_lease_id.clone(),
        task_key: claim.task_key.clone(),
        fencing_token: claim.fencing_token,
        lease_revision: claim.lease_revision,
    };
    replace_session(
        host,
        session,
        old.session_id,
        subject,
        controller,
        origin,
        old.limits,
        now,
    )
}

fn reopen_agent_session(
    host: &ComputerHost<DaemonComputerRepository>,
    session: &mut ComputerStreamSession,
    daemon_instance_id: &EntityId,
    owner: &ProfileId,
    now: UtcTimestamp,
) -> Result<ComputerStreamDescriptorProjection, ComputerCommandRuntimeError> {
    let old = session.descriptor();
    let origin = next_origin(session, daemon_instance_id)?;
    let record = host
        .repository()
        .computer(owner)
        .map_err(host_error)?
        .ok_or_else(|| runtime_error(ErrorCode::NotFound, "computer does not exist", false))?;
    let task_key = record.current_task_key.clone().ok_or_else(|| {
        runtime_error(ErrorCode::Conflict, "owning task was safely failed", false)
    })?;
    let subject = ComputerStreamSubject {
        profile_id: owner.clone(),
        computer_id: record.computer_id,
    };
    let controller = ComputerStreamController::Agent {
        profile_id: owner.clone(),
        task_key,
        fencing_token: record.revision.get(),
    };
    replace_session(
        host,
        session,
        old.session_id,
        subject,
        controller,
        origin,
        old.limits,
        now,
    )
}

fn replace_session(
    host: &ComputerHost<DaemonComputerRepository>,
    session: &mut ComputerStreamSession,
    session_id: EntityId,
    subject: ComputerStreamSubject,
    controller: ComputerStreamController,
    origin: ComputerStreamOrigin,
    limits: ComputerStreamLimits,
    now: UtcTimestamp,
) -> Result<ComputerStreamDescriptorProjection, ComputerCommandRuntimeError> {
    let authorization = host
        .authorize_stream(
            subject.clone(),
            origin,
            controller,
            now,
            timestamp_after(now, 30_000)?,
        )
        .map_err(host_error)?;
    let replacement = ComputerStreamSession::open(
        session_id,
        ComputerStreamOpenRequest {
            subject,
            resume: None,
        },
        authorization,
        limits,
        now,
        timestamp_after(now, 10_000)?,
    )
    .map_err(stream_error)?;
    let descriptor = replacement.descriptor();
    *session = replacement;
    Ok(descriptor_projection(descriptor))
}

impl Arguments {
    #[allow(clippy::too_many_lines)]
    fn parse<I, S>(arguments: I) -> Result<Option<Self>, String>
    where
        I: IntoIterator<Item = S>,
        S: Into<OsString>,
    {
        let mut arguments = arguments.into_iter().map(Into::into);
        let program = arguments.next().unwrap_or_else(|| OsString::from("agentd"));
        let mut data_root = None;
        let mut socket = None;
        let mut worker_executable = None;
        let mut browser_runner_executable = None;
        let mut chromium_executable = None;
        let mut idle_seconds = 15 * 60;
        let mut credential_root = None;
        let mut credential_key_source = None;
        let mut workspace_root = None;
        let mut openai_base_url = "https://api.openai.com".to_owned();
        let mut anthropic_base_url = "https://api.anthropic.com".to_owned();
        let mut provider_base_urls = BTreeMap::new();
        while let Some(argument) = arguments.next() {
            let argument = argument
                .into_string()
                .map_err(|_| "arguments must be UTF-8".to_owned())?;
            if matches!(argument.as_str(), "--version" | "-V") {
                println!("{} {}", env!("CARGO_BIN_NAME"), env!("CARGO_PKG_VERSION"));
                return Ok(None);
            }
            if argument == "--build-info" {
                let report = keith_build_info::daemon_report();
                println!(
                    "{}",
                    serde_json::to_string_pretty(&report).map_err(|error| error.to_string())?
                );
                return Ok(None);
            }
            let value = arguments
                .next()
                .ok_or_else(|| format!("missing value for {argument}"))?;
            match argument.as_str() {
                "--data-root" => data_root = Some(PathBuf::from(value)),
                "--socket" => socket = Some(PathBuf::from(value)),
                "--worker-executable" => worker_executable = Some(PathBuf::from(value)),
                "--browser-runner-executable" => {
                    browser_runner_executable = Some(PathBuf::from(value));
                }
                "--chromium-executable" => chromium_executable = Some(PathBuf::from(value)),
                "--credential-root" => credential_root = Some(PathBuf::from(value)),
                "--credential-key-env" => {
                    credential_key_source = Some(CredentialKeySource::Environment(
                        value
                            .into_string()
                            .map_err(|_| "credential key environment must be UTF-8".to_owned())?,
                    ));
                }
                "--credential-key-native-account" => {
                    credential_key_source =
                        Some(CredentialKeySource::Native(value.into_string().map_err(
                            |_| "native key account must be UTF-8".to_owned(),
                        )?));
                }
                "--workspace-root" => workspace_root = Some(PathBuf::from(value)),
                "--openai-base-url" => {
                    openai_base_url = value
                        .into_string()
                        .map_err(|_| "OpenAI base URL must be UTF-8".to_owned())?;
                }
                "--anthropic-base-url" => {
                    anthropic_base_url = value
                        .into_string()
                        .map_err(|_| "Anthropic base URL must be UTF-8".to_owned())?;
                }
                "--provider-base-url" => {
                    let value = value
                        .into_string()
                        .map_err(|_| "provider base URL must be UTF-8".to_owned())?;
                    let (provider, base_url) = value.split_once('=').ok_or_else(|| {
                        "provider base URL must use the form PROVIDER=https://endpoint".to_owned()
                    })?;
                    if provider.trim().is_empty() || base_url.trim().is_empty() {
                        return Err(
                            "provider base URL must use the form PROVIDER=https://endpoint".into(),
                        );
                    }
                    if provider_base_urls
                        .insert(provider.to_owned(), base_url.to_owned())
                        .is_some()
                    {
                        return Err(format!("provider base URL for {provider} was repeated"));
                    }
                }
                "--idle-seconds" => {
                    idle_seconds = value
                        .into_string()
                        .map_err(|_| "idle seconds must be UTF-8".to_owned())?
                        .parse()
                        .map_err(|_| "idle seconds must be an integer".to_owned())?;
                }
                _ => return Err(format!("unknown argument {argument}")),
            }
        }
        let platform_paths = if data_root.is_none() {
            Some(PlatformPaths::discover().map_err(|error| error.to_string())?)
        } else {
            None
        };
        let data_root = data_root
            .or_else(|| platform_paths.as_ref().map(|paths| paths.data_root.clone()))
            .ok_or_else(|| "native data root is unavailable".to_owned())?;
        let socket = socket.unwrap_or_else(|| {
            platform_paths.as_ref().map_or_else(
                || data_root.join("agentd.sock"),
                |paths| paths.daemon_endpoint.clone(),
            )
        });
        let worker_executable = worker_executable.unwrap_or_else(|| {
            let mut sibling = PathBuf::from(program.clone());
            sibling.set_file_name("agent-worker");
            sibling
        });
        let browser_runner_executable = browser_runner_executable.unwrap_or_else(|| {
            let mut sibling = PathBuf::from(program);
            sibling.set_file_name("browser-runner");
            sibling
        });
        let chromium_executable =
            chromium_executable.unwrap_or_else(|| PathBuf::from("/usr/bin/chromium"));
        let credential_root = credential_root.unwrap_or_else(|| data_root.join("credentials"));
        let credential_key_source = credential_key_source
            .unwrap_or_else(|| CredentialKeySource::Restricted(credential_root.clone()));
        let workspace_root = workspace_root
            .map_or_else(std::env::current_dir, Ok)
            .map_err(|error| error.to_string())?;
        Ok(Some(Self {
            data_root,
            socket,
            bootstrap_worker_executable: worker_executable,
            browser_runner_executable,
            chromium_executable,
            idle_seconds,
            credential_root,
            credential_key_source,
            workspace_root,
            openai_base_url,
            anthropic_base_url,
            provider_base_urls,
        }))
    }
}

fn run_child() -> Result<(), String> {
    arm_launcher_parent_death()?;
    let Some(arguments) = Arguments::parse(std::env::args_os())? else {
        return Ok(());
    };
    let shutdown = Arc::new(AtomicBool::new(false));
    signal_hook::flag::register(SIGTERM, Arc::clone(&shutdown))
        .map_err(|error| format!("failed to register SIGTERM: {error}"))?;
    signal_hook::flag::register(SIGINT, Arc::clone(&shutdown))
        .map_err(|error| format!("failed to register SIGINT: {error}"))?;
    let mut options = DaemonOptions {
        idle_evict_after: Duration::from_secs(arguments.idle_seconds),
        evolution_source_root: Some(arguments.workspace_root.clone()),
        ..DaemonOptions::default()
    };
    options.supervisor.startup_timeout = Duration::from_secs(30);
    let runtime = LocalRuntimeLaunchConfig {
        data_root: arguments.data_root.clone(),
        credential_root: arguments.credential_root,
        credential_key_source: match arguments.credential_key_source {
            CredentialKeySource::Environment(environment) => {
                RuntimeCredentialKeySource::Environment(environment)
            }
            CredentialKeySource::Native(account) => RuntimeCredentialKeySource::Native(account),
            CredentialKeySource::Restricted(root) => RuntimeCredentialKeySource::Restricted(root),
        },
        workspace_root: arguments.workspace_root,
        openai_base_url: arguments.openai_base_url,
        anthropic_base_url: arguments.anthropic_base_url,
        provider_base_urls: arguments.provider_base_urls,
    };
    let computer_secrets = runtime
        .open_computer_secret_injection_service()
        .map_err(|error| error.to_string())?;
    let computer_task_boundaries = runtime
        .open_computer_takeover_task_boundary_service()
        .map_err(|error| error.to_string())?;
    let runtime_config = write_runtime_config(&arguments.data_root, &runtime)?;
    let mut daemon = DaemonCore::open_with_worker_runtime(
        &arguments.data_root,
        arguments.bootstrap_worker_executable,
        options,
        runtime_config,
    )
    .map_err(|error| error.to_string())?;
    let computer_host = ComputerHostRuntime::open(
        &arguments.data_root,
        arguments.browser_runner_executable,
        arguments.chromium_executable,
        computer_secrets,
        computer_task_boundaries,
    )?;
    daemon.install_computer_runtime(Box::new(computer_host));
    let ready_path = std::env::var_os(READY_PATH_ENV).map(PathBuf::from);
    let ready_image = std::env::var(READY_IMAGE_ENV).ok();
    let serve_result = daemon
        .serve_local_with_ready(&arguments.socket, &shutdown, || {
            if let (Some(path), Some(image_id)) = (&ready_path, &ready_image) {
                write_ready(path, image_id)?;
            }
            Ok(())
        })
        .map_err(|error| error.to_string());
    serve_result
}

#[allow(clippy::too_many_lines)]
fn run_launcher() -> Result<(), String> {
    let original_arguments = std::env::args_os().skip(1).collect::<Vec<_>>();
    if original_arguments
        .iter()
        .any(|argument| argument == "--version" || argument == "-V" || argument == "--build-info")
    {
        return run_child();
    }
    let data_root = launcher_data_root(&original_arguments)?;
    let staging_root = std::env::var_os(STAGING_ROOT_ENV).map_or_else(
        || data_root.join("self-evolution").join("daemon-images"),
        PathBuf::from,
    );
    let bootstrap = std::env::current_exe().map_err(|error| error.to_string())?;
    let mut staging =
        DaemonStaging::open(&staging_root, &bootstrap).map_err(|error| error.to_string())?;
    let shutdown = Arc::new(AtomicBool::new(false));
    signal_hook::flag::register(SIGTERM, Arc::clone(&shutdown))
        .map_err(|error| format!("failed to register launcher SIGTERM: {error}"))?;
    signal_hook::flag::register(SIGINT, Arc::clone(&shutdown))
        .map_err(|error| format!("failed to register launcher SIGINT: {error}"))?;

    loop {
        let selection = staging
            .launch_selection()
            .map_err(|error| error.to_string())?;
        let ready_path = staging_root.join(format!(
            ".ready-{}-{}",
            std::process::id(),
            selection.image.image_id
        ));
        remove_ready(&ready_path)?;
        let mut child_command = Command::new(&selection.image.executable);
        child_command
            .args(&original_arguments)
            .env(CHILD_ENV, "1")
            .env(READY_PATH_ENV, &ready_path)
            .env(READY_IMAGE_ENV, &selection.image.image_id);
        child_command.env(LAUNCHER_PID_ENV, std::process::id().to_string());
        let spawned = child_command.spawn();
        let mut child = match spawned {
            Ok(child) => child,
            Err(error) if selection.candidate => {
                let reason = format!(
                    "failed to launch daemon image {}: {error}",
                    selection.image.image_id
                );
                staging
                    .fail_and_restore(&selection.image.image_id, &reason)
                    .map_err(|restore| format!("{reason}; pinned restore failed: {restore}"))?;
                continue;
            }
            Err(error) => {
                return Err(format!(
                    "failed to launch daemon image {}: {error}",
                    selection.image.image_id
                ));
            }
        };

        match await_ready(
            &mut child,
            &ready_path,
            &selection.image.image_id,
            &shutdown,
        ) {
            Ok(()) => {
                if selection.candidate
                    && let Err(error) = staging.mark_ready(&selection.image.image_id)
                {
                    let _ = child.kill();
                    let _ = child.wait();
                    remove_ready(&ready_path)?;
                    let reason = format!("candidate readiness could not be committed: {error}");
                    staging
                        .fail_and_restore(&selection.image.image_id, &reason)
                        .map_err(|restore| format!("{reason}; pinned restore failed: {restore}"))?;
                    continue;
                }
                remove_ready(&ready_path)?;
                let status = wait_for_exit(&mut child, &shutdown)?;
                if shutdown.load(std::sync::atomic::Ordering::Acquire) {
                    return Ok(());
                }
                if !selection.candidate {
                    return exit_status(status);
                }
                staging
                    .fail_and_restore(
                        &selection.image.image_id,
                        &format!("candidate daemon exited after readiness with {status}"),
                    )
                    .map_err(|error| error.to_string())?;
            }
            Err(reason) if selection.candidate => {
                let _ = child.kill();
                let _ = child.wait();
                remove_ready(&ready_path)?;
                if shutdown.load(std::sync::atomic::Ordering::Acquire) {
                    return Ok(());
                }
                staging
                    .fail_and_restore(&selection.image.image_id, &reason)
                    .map_err(|error| error.to_string())?;
            }
            Err(reason) => {
                let _ = child.kill();
                let _ = child.wait();
                remove_ready(&ready_path)?;
                if shutdown.load(std::sync::atomic::Ordering::Acquire) {
                    return Ok(());
                }
                return Err(reason);
            }
        }
    }
}

#[cfg(target_os = "linux")]
fn arm_launcher_parent_death() -> Result<(), String> {
    let expected = std::env::var(LAUNCHER_PID_ENV)
        .map_err(|_| "daemon child is missing its launcher identity".to_owned())?
        .parse::<i32>()
        .map_err(|_| "daemon child launcher identity is invalid".to_owned())?;
    nix::sys::prctl::set_pdeathsig(nix::sys::signal::Signal::SIGKILL)
        .map_err(|error| format!("failed to arm launcher parent-death containment: {error}"))?;
    if nix::unistd::getppid().as_raw() != expected {
        return Err("daemon launcher exited before child supervision was armed".into());
    }
    Ok(())
}

#[cfg(not(target_os = "linux"))]
fn arm_launcher_parent_death() -> Result<(), String> {
    Ok(())
}

fn launcher_data_root(arguments: &[OsString]) -> Result<PathBuf, String> {
    let mut index = 0;
    while index < arguments.len() {
        if arguments[index] == "--data-root" {
            return arguments
                .get(index + 1)
                .map(PathBuf::from)
                .ok_or_else(|| "missing value for --data-root".to_owned());
        }
        index = index.saturating_add(2);
    }
    PlatformPaths::discover()
        .map(|paths| paths.data_root)
        .map_err(|error| error.to_string())
}

fn await_ready(
    child: &mut std::process::Child,
    ready_path: &std::path::Path,
    image_id: &str,
    shutdown: &AtomicBool,
) -> Result<(), String> {
    let deadline = std::time::Instant::now() + READY_TIMEOUT;
    loop {
        if let Some(status) = child.try_wait().map_err(|error| error.to_string())? {
            return Err(format!(
                "candidate daemon exited before readiness with {status}"
            ));
        }
        match fs::read_to_string(ready_path) {
            Ok(value) if value == image_id => return Ok(()),
            Ok(_) => return Err("daemon readiness identity did not match its image".into()),
            Err(error) if error.kind() == std::io::ErrorKind::NotFound => {}
            Err(error) => return Err(format!("failed to read daemon readiness: {error}")),
        }
        // Readiness is a durable child-to-launcher handoff. If it was published before a
        // concurrent shutdown request, commit it before stopping the child so the registry
        // cannot remain stranded in `Launching` after a successful endpoint launch.
        if shutdown.load(std::sync::atomic::Ordering::Acquire) {
            let _ = child.kill();
            return Err("daemon launcher was asked to shut down".into());
        }
        if std::time::Instant::now() >= deadline {
            return Err("daemon readiness timed out".into());
        }
        thread::sleep(READY_POLL);
    }
}

fn wait_for_exit(
    child: &mut std::process::Child,
    shutdown: &AtomicBool,
) -> Result<ExitStatus, String> {
    loop {
        if let Some(status) = child.try_wait().map_err(|error| error.to_string())? {
            return Ok(status);
        }
        if shutdown.load(std::sync::atomic::Ordering::Acquire) {
            #[cfg(unix)]
            nix::sys::signal::kill(
                nix::unistd::Pid::from_raw(
                    i32::try_from(child.id()).map_err(|_| "daemon child PID is out of range")?,
                ),
                nix::sys::signal::Signal::SIGTERM,
            )
            .map_err(|error| error.to_string())?;
            #[cfg(windows)]
            child.kill().map_err(|error| error.to_string())?;
            let deadline = std::time::Instant::now() + Duration::from_secs(15);
            loop {
                if let Some(status) = child.try_wait().map_err(|error| error.to_string())? {
                    return Ok(status);
                }
                if std::time::Instant::now() >= deadline {
                    child.kill().map_err(|error| error.to_string())?;
                    return child.wait().map_err(|error| error.to_string());
                }
                thread::sleep(READY_POLL);
            }
        }
        thread::sleep(READY_POLL);
    }
}

fn write_ready(path: &std::path::Path, image_id: &str) -> Result<(), std::io::Error> {
    let mut file = File::options().create_new(true).write(true).open(path)?;
    file.write_all(image_id.as_bytes())?;
    file.sync_all()?;
    File::open(path.parent().unwrap_or_else(|| std::path::Path::new(".")))?.sync_all()
}

fn remove_ready(path: &std::path::Path) -> Result<(), String> {
    match fs::remove_file(path) {
        Ok(()) => File::open(path.parent().unwrap_or_else(|| std::path::Path::new(".")))
            .and_then(|directory| directory.sync_all())
            .map_err(|error| error.to_string()),
        Err(error) if error.kind() == std::io::ErrorKind::NotFound => Ok(()),
        Err(error) => Err(error.to_string()),
    }
}

fn exit_status(status: ExitStatus) -> Result<(), String> {
    if status.success() {
        Ok(())
    } else {
        Err(format!("daemon exited with {status}"))
    }
}

fn run() -> Result<(), String> {
    if std::env::var_os(CHILD_ENV).is_some() {
        run_child()
    } else {
        run_launcher()
    }
}

fn write_runtime_config(
    data_root: &std::path::Path,
    runtime: &LocalRuntimeLaunchConfig,
) -> Result<PathBuf, String> {
    let directory = data_root.join("runtime");
    fs::create_dir_all(&directory).map_err(|error| error.to_string())?;
    let path = directory.join("worker-runtime.json");
    let temporary = directory.join(format!(".worker-runtime.{}.tmp", std::process::id()));
    let bytes = serde_json::to_vec(runtime).map_err(|error| error.to_string())?;
    let mut file = File::create(&temporary).map_err(|error| error.to_string())?;
    file.write_all(&bytes).map_err(|error| error.to_string())?;
    file.sync_all().map_err(|error| error.to_string())?;
    keith_platform::replace_file(&temporary, &path).map_err(|error| error.to_string())?;
    File::open(&directory)
        .and_then(|directory| directory.sync_all())
        .map_err(|error| error.to_string())?;
    Ok(path)
}

fn main() {
    if let Err(error) = run() {
        eprintln!("{error}");
        std::process::exit(1);
    }
}
