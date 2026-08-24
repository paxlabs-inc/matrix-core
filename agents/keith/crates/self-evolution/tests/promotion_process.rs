#![cfg(unix)]

use std::collections::BTreeSet;
use std::fs;
use std::path::{Path, PathBuf};
use std::process::Command;
use std::sync::Arc;
use std::time::Duration;

use keith_agent_types::{
    ActionId, ChildId, DeliveryId, EntityId, JobId, SessionId, ToolCallId, canonical_json_bytes,
};
use keith_release::BuildReport;
use keith_sandbox::{IsolationLevel, SandboxBackend, SandboxStatus};
use keith_self_evolution::{
    CanaryEvaluation, CanaryVerdict, ChangedPath, EvolutionEvent, EvolutionLedger,
    EvolutionProposal, GateKind, GateResult, ObservationRequest, PromotionError, PromotionRecord,
    PromotionRequest, PromotionTransaction, ProposalPreimage, ReversalError, ReversalRequest,
    ReversalScope, ReversalTransaction, RevertWatchdog, SelfEvolutionEnablement,
    ThresholdDirection, ToolchainIdentity, WatchdogDecision, WatchdogNotificationPolicy,
    WatchdogThresholds, WorkerImage, WorkerImageManifest,
};
use keith_state_store::EmbeddedStore;
use keith_supervisor::{SupervisorOptions, WorkerHealth, WorkerSupervisor};
use keith_telemetry::{CandidateObservation, CandidateSignal, MetricName};
use ring::signature::{Ed25519KeyPair, KeyPair};
use sha2::{Digest, Sha256};

const SOURCE: &str = "crates/feature/src/lib.rs";
const OLD_SOURCE: &[u8] = b"pub fn answer() -> u8 { 41 }\n";
const NEW_SOURCE: &[u8] = b"pub fn answer() -> u8 { 42 }\n";

fn options() -> SupervisorOptions {
    SupervisorOptions {
        startup_timeout: Duration::from_secs(3),
        drain_timeout: Duration::from_secs(1),
        stale_heartbeat: Duration::from_secs(1),
        heartbeat_interval: Duration::from_millis(20),
        lease_duration: Duration::from_millis(500),
    }
}

fn sha256(bytes: &[u8]) -> String {
    format!("{:x}", Sha256::digest(bytes))
}

fn evolution_ledger(root: &Path) -> Arc<EvolutionLedger<EmbeddedStore>> {
    Arc::new(
        EvolutionLedger::from_seed(
            Arc::new(EmbeddedStore::open(&root.join("evolution.sqlite"), None).unwrap()),
            &[44; 32],
        )
        .unwrap(),
    )
}

fn signed_image(executable: Vec<u8>, build_id: &str) -> (WorkerImage, [u8; 32], String) {
    let output = "real process gate exited successfully";
    let gates = [
        GateKind::Formatting,
        GateKind::StrictClippy,
        GateKind::WorkspaceTests,
        GateKind::DependencyPolicy,
        GateKind::Security,
        GateKind::Platform,
    ]
    .into_iter()
    .map(|gate| GateResult {
        gate,
        exit_code: 0,
        elapsed_millis: 1,
        output: output.into(),
        output_sha256: sha256(output.as_bytes()),
        sandbox: SandboxStatus {
            backend: SandboxBackend::LinuxBubblewrap,
            level: IsolationLevel::Strong,
            launcher: Some(PathBuf::from("/usr/bin/bwrap")),
            filesystem_containment: true,
            process_tree_control: true,
            network_isolation: true,
            cpu_limit: true,
            memory_limit: true,
            reduced_reasons: Vec::new(),
        },
    })
    .collect();
    let manifest = WorkerImageManifest {
        format: "keith-worker-image-v1".into(),
        build_id: build_id.into(),
        base_revision: "a".repeat(40),
        source_manifest_sha256: "b".repeat(64),
        executable_sha256: sha256(&executable),
        executable_bytes: executable.len() as u64,
        toolchain: ToolchainIdentity {
            rustc: "rustc 1.93.0".into(),
            cargo: "cargo 1.93.0".into(),
            target: "x86_64-linux".into(),
        },
        worker_report: BuildReport {
            component: "worker".into(),
            package_version: "0.1.0".into(),
            build_id: build_id.into(),
            protocol_version: "1.0".into(),
            storage_schema: "1.0".into(),
            enabled_features: BTreeSet::from(["runtime".into()]),
        },
        gates,
        artifact_source_paths: vec![PathBuf::from(SOURCE)],
        change_class: "b".into(),
    };
    let key = Ed25519KeyPair::from_seed_unchecked(&[23; 32]).unwrap();
    let public_key: [u8; 32] = key.public_key().as_ref().try_into().unwrap();
    let manifest_bytes = canonical_json_bytes(&manifest).unwrap();
    let signature = key.sign(&manifest_bytes).as_ref().to_vec();
    let image_id = sha256(&manifest_bytes);
    let image =
        WorkerImage::from_signed_parts(manifest, executable, signature, public_key, &public_key)
            .unwrap();
    (image, public_key, image_id)
}

fn canary(image_id: String) -> CanaryEvaluation {
    CanaryEvaluation {
        transaction_id: EntityId::new(),
        image_id,
        generation: None,
        metric: MetricName::ModelLatency,
        baseline: 100.0,
        measured: Some(90.0),
        target_threshold: 95.0,
        latency_ms: 90,
        token_cost: 10,
        operations: 1,
        measurements: Vec::new(),
        verdict: CanaryVerdict::Passed,
    }
}

fn source_fixture(root: &Path) -> (PathBuf, EvolutionProposal) {
    let live = root.join("live");
    let shadow = root.join("shadow");
    let live_source = live.join(SOURCE);
    let shadow_source = shadow.join(SOURCE);
    fs::create_dir_all(live_source.parent().unwrap()).unwrap();
    fs::create_dir_all(shadow_source.parent().unwrap()).unwrap();
    fs::write(&live_source, OLD_SOURCE).unwrap();
    fs::write(&shadow_source, NEW_SOURCE).unwrap();
    (
        shadow,
        EvolutionProposal {
            summary: "advance the real worker result".into(),
            changes: vec![ChangedPath::Write(PathBuf::from(SOURCE))],
            preimages: vec![ProposalPreimage {
                path: PathBuf::from(SOURCE),
                prior_bytes: Some(OLD_SOURCE.to_vec()),
            }],
            new_dependencies: Vec::new(),
        },
    )
}

#[derive(serde::Serialize)]
struct DurableWork {
    session: SessionId,
    action: ActionId,
    goal: EntityId,
    child: ChildId,
    wait: EntityId,
    commitment: EntityId,
    schedule: JobId,
    delivery: DeliveryId,
    tool: ToolCallId,
}

#[test]
fn live_roll_preserves_durable_work_and_advances_every_exact_generation() {
    let directory = tempfile::tempdir().unwrap();
    let worker = Path::new(env!("CARGO_BIN_EXE_keith-promotion-worker-process"));
    let state = directory.path().join("daemon-runtime");
    let live = directory.path().join("live");
    let (shadow, proposal) = source_fixture(directory.path());
    let durable = canonical_json_bytes(&DurableWork {
        session: SessionId::new(),
        action: ActionId::new(),
        goal: EntityId::new(),
        child: ChildId::new(),
        wait: EntityId::new(),
        commitment: EntityId::new(),
        schedule: JobId::new(),
        delivery: DeliveryId::new(),
        tool: ToolCallId::new(),
    })
    .unwrap();
    fs::create_dir_all(state.join("durable")).unwrap();
    fs::write(state.join("durable/work.json"), &durable).unwrap();
    let mut supervisor = WorkerSupervisor::open(&state, worker, options()).unwrap();
    let roots = [
        keith_agent_types::RootTreeId::new(),
        keith_agent_types::RootTreeId::new(),
    ];
    let before = roots
        .iter()
        .map(|root| supervisor.start(root.clone()).unwrap())
        .collect::<Vec<_>>();
    let owner_pid = std::process::id();
    let (image, key, image_id) = signed_image(fs::read(worker).unwrap(), "live-roll");
    let ledger = evolution_ledger(directory.path());
    let transaction = PromotionTransaction::open(directory.path().join("tx"), &live).unwrap();
    let outcome = transaction
        .promote(
            &mut supervisor,
            &ledger,
            PromotionRequest {
                hypothesis_id: EntityId::new(),
                occurred_at: keith_agent_types::UtcTimestamp::from_unix_millis(10),
                image: &image,
                trusted_public_key: &key,
                canary: &canary(image_id.clone()),
                proposal: &proposal,
                shadow_root: &shadow,
                failure_threshold: 1,
            },
        )
        .unwrap();
    assert_eq!(
        owner_pid,
        std::process::id(),
        "the daemon owner did not restart"
    );
    assert_eq!(fs::read(state.join("durable/work.json")).unwrap(), durable);
    assert_eq!(fs::read(live.join(SOURCE)).unwrap(), NEW_SOURCE);
    assert_eq!(outcome.rolls.len(), roots.len());
    for (prior, root) in before.iter().zip(roots.iter()) {
        let after = supervisor.status(root).unwrap();
        assert_eq!(after.image_id, image_id);
        assert_eq!(after.health, WorkerHealth::Healthy);
        assert!(after.generation > prior.generation);
        assert_ne!(after.pid, prior.pid);
    }
    supervisor.drain_all().unwrap();
}

#[test]
fn partial_roll_failure_restores_every_root_and_never_writes_source() {
    let directory = tempfile::tempdir().unwrap();
    let worker = Path::new(env!("CARGO_BIN_EXE_keith-promotion-worker-process"));
    let state = directory.path().join("daemon-runtime");
    let live = directory.path().join("live");
    let (shadow, proposal) = source_fixture(directory.path());
    let mut supervisor = WorkerSupervisor::open(&state, worker, options()).unwrap();
    let roots = [
        keith_agent_types::RootTreeId::new(),
        keith_agent_types::RootTreeId::new(),
    ];
    for root in &roots {
        supervisor.start(root.clone()).unwrap();
    }
    let prior_image = supervisor.image_registry().current().image_id.clone();
    let script = format!(
        "#!/bin/sh\ncase \" $* \" in *\" --root-tree {} \"*) exit 41;; esac\nexec \"{}\" \"$@\"\n",
        roots[1],
        worker.display()
    );
    let (image, key, image_id) = signed_image(script.into_bytes(), "partial-failure");
    let ledger = evolution_ledger(directory.path());
    let transaction = PromotionTransaction::open(directory.path().join("tx"), &live).unwrap();
    let error = transaction
        .promote(
            &mut supervisor,
            &ledger,
            PromotionRequest {
                hypothesis_id: EntityId::new(),
                occurred_at: keith_agent_types::UtcTimestamp::from_unix_millis(10),
                image: &image,
                trusted_public_key: &key,
                canary: &canary(image_id),
                proposal: &proposal,
                shadow_root: &shadow,
                failure_threshold: 1,
            },
        )
        .unwrap_err();
    assert!(matches!(error, PromotionError::RollAborted));
    assert_eq!(supervisor.image_registry().current().image_id, prior_image);
    assert_eq!(fs::read(live.join(SOURCE)).unwrap(), OLD_SOURCE);
    for root in &roots {
        let status = supervisor.status(root).unwrap();
        assert_eq!(status.image_id, prior_image);
        assert_eq!(status.health, WorkerHealth::Healthy);
    }
    supervisor.drain_all().unwrap();
}

#[test]
fn one_action_reversal_uses_pinned_bytes_while_disabled_and_advances_generation() {
    let directory = tempfile::tempdir().unwrap();
    let worker = Path::new(env!("CARGO_BIN_EXE_keith-promotion-worker-process"));
    let state = directory.path().join("daemon-runtime");
    let live = directory.path().join("live");
    let (shadow, proposal) = source_fixture(directory.path());
    let mut supervisor = WorkerSupervisor::open(&state, worker, options()).unwrap();
    let root = keith_agent_types::RootTreeId::new();
    let original = supervisor.start(root.clone()).unwrap();
    let (image, key, image_id) = signed_image(fs::read(worker).unwrap(), "reversal");
    let transaction_root = directory.path().join("tx");
    let hypothesis_id = EntityId::new();
    let ledger = evolution_ledger(directory.path());
    let promotion = PromotionTransaction::open(&transaction_root, &live).unwrap();
    let promoted = promotion
        .promote(
            &mut supervisor,
            &ledger,
            PromotionRequest {
                hypothesis_id,
                occurred_at: keith_agent_types::UtcTimestamp::from_unix_millis(10),
                image: &image,
                trusted_public_key: &key,
                canary: &canary(image_id),
                proposal: &proposal,
                shadow_root: &shadow,
                failure_threshold: 1,
            },
        )
        .unwrap();
    drop(promotion);
    let promoted_status = supervisor.status(&root).unwrap();
    assert!(promoted_status.generation > original.generation);

    let enablement =
        SelfEvolutionEnablement::new(live.clone(), [71; 32], "owner".into(), ledger.clone());
    assert!(!enablement.enabled());
    let installation = enablement.authenticate_installation(&[71; 32]).unwrap();
    let authority = enablement.authorize_reversal(&installation).unwrap();
    let reversal = ReversalTransaction::open(&transaction_root, &live).unwrap();
    let outcome = reversal
        .reverse(
            &mut supervisor,
            &ledger,
            ReversalRequest {
                scope: ReversalScope::Promotion(promoted.transaction_id),
                trusted_public_key: &key,
                authority: &authority,
                reason: "owner selected undo",
                occurred_at: keith_agent_types::UtcTimestamp::from_unix_millis(100),
            },
        )
        .unwrap();
    let restored = supervisor.status(&root).unwrap();
    assert!(restored.generation > promoted_status.generation);
    assert_eq!(restored.image_id, outcome.restored_image_id);
    assert_eq!(fs::read(live.join(SOURCE)).unwrap(), OLD_SOURCE);
    assert!(ledger.records().unwrap().iter().any(|record| {
        matches!(
            &record.event,
            EvolutionEvent::Revert { promotion_ids, .. }
                if promotion_ids == &outcome.promotion_ids
        )
    }));
    supervisor.drain_all().unwrap();
}

#[test]
fn real_candidate_crash_after_watchdog_restart_reverts_exact_promotion_automatically() {
    let directory = tempfile::tempdir().unwrap();
    let worker = Path::new(env!("CARGO_BIN_EXE_keith-promotion-worker-process"));
    let state = directory.path().join("daemon-runtime");
    let live = directory.path().join("live");
    let (shadow, proposal) = source_fixture(directory.path());
    let mut supervisor = WorkerSupervisor::open(&state, worker, options()).unwrap();
    let root = keith_agent_types::RootTreeId::new();
    supervisor.start(root.clone()).unwrap();
    let prior_image = supervisor.image_registry().current().image_id.clone();
    let (image, key, image_id) = signed_image(fs::read(worker).unwrap(), "watchdog-crash");
    let transaction_root = directory.path().join("tx");
    let hypothesis_id = EntityId::new();
    let ledger = evolution_ledger(directory.path());
    let promotion = PromotionTransaction::open(&transaction_root, &live).unwrap();
    let promoted = promotion
        .promote(
            &mut supervisor,
            &ledger,
            PromotionRequest {
                hypothesis_id: hypothesis_id.clone(),
                occurred_at: keith_agent_types::UtcTimestamp::from_unix_millis(10),
                image: &image,
                trusted_public_key: &key,
                canary: &canary(image_id.clone()),
                proposal: &proposal,
                shadow_root: &shadow,
                failure_threshold: 1,
            },
        )
        .unwrap();
    drop(promotion);
    let candidate = supervisor.status(&root).unwrap();

    {
        let mut watchdog = RevertWatchdog::open(&transaction_root).unwrap();
        watchdog
            .start(ObservationRequest {
                promotion_id: promoted.transaction_id.clone(),
                hypothesis_id: hypothesis_id.clone(),
                profile_id: EntityId::new(),
                candidate_image_id: image_id.clone(),
                prior_known_good_image_id: prior_image.clone(),
                candidate_workers: BTreeSet::from([(root.clone(), candidate.generation)]),
                hypothesis_metric: MetricName::ModelLatency,
                started_at: keith_agent_types::UtcTimestamp::from_unix_millis(20),
                deadline: keith_agent_types::UtcTimestamp::from_unix_millis(1_020),
                previous_image_retain_until: keith_agent_types::UtcTimestamp::from_unix_millis(
                    2_020,
                ),
                thresholds: WatchdogThresholds {
                    hypothesis_direction: ThresholdDirection::AtMost,
                    hypothesis_revert_threshold: 100.0,
                    maximum_crashes: 1,
                    maximum_turn_failure_rate: 0.5,
                    maximum_mean_latency_ms: 1_000,
                    maximum_total_token_cost: 10_000,
                    maximum_resident_bytes: u64::MAX,
                    maximum_virtual_bytes: u64::MAX,
                    minimum_hypothesis_samples: 1,
                    minimum_turn_samples: 1,
                    minimum_resource_samples: 1,
                },
                notification_policy: WatchdogNotificationPolicy::NotifyOnRevert,
            })
            .unwrap();
    }

    let mut watchdog = RevertWatchdog::open(&transaction_root).unwrap();
    assert_eq!(watchdog.active().unwrap().candidate_image_id, image_id);
    let killed = Command::new("kill")
        .args(["-KILL", &candidate.pid.to_string()])
        .status()
        .unwrap();
    assert!(killed.success());
    for _ in 0..100 {
        if supervisor
            .status(&root)
            .is_some_and(|status| status.health == WorkerHealth::Exited)
        {
            break;
        }
        std::thread::sleep(Duration::from_millis(10));
    }
    assert_eq!(
        supervisor.status(&root).unwrap().health,
        WorkerHealth::Exited
    );
    let decision = watchdog
        .observe_candidate(
            CandidateObservation::new(
                image_id,
                root.clone(),
                candidate.generation,
                keith_agent_types::UtcTimestamp::from_unix_millis(30),
                CandidateSignal::WorkerCrash,
            )
            .unwrap(),
        )
        .unwrap();
    assert!(matches!(decision, WatchdogDecision::RevertRequired(_)));

    let enablement =
        SelfEvolutionEnablement::new(live.clone(), [71; 32], "owner".into(), ledger.clone());
    let installation = enablement.authenticate_installation(&[71; 32]).unwrap();
    let authority = enablement.authorize_reversal(&installation).unwrap();
    let reversal = ReversalTransaction::open(&transaction_root, &live).unwrap();
    assert!(matches!(
        watchdog
            .apply_revert(
                &reversal,
                &mut supervisor,
                &ledger,
                &key,
                &authority,
                keith_agent_types::UtcTimestamp::from_unix_millis(40),
            )
            .unwrap(),
        WatchdogDecision::Reverted(_)
    ));
    assert_eq!(supervisor.image_registry().current().image_id, prior_image);
    assert_eq!(fs::read(live.join(SOURCE)).unwrap(), OLD_SOURCE);
    assert_eq!(watchdog.pending_notifications().len(), 1);
    supervisor.drain_all().unwrap();
}

#[test]
fn tampered_promotion_archive_is_rejected_before_image_or_source_mutation() {
    let directory = tempfile::tempdir().unwrap();
    let worker = Path::new(env!("CARGO_BIN_EXE_keith-promotion-worker-process"));
    let state = directory.path().join("daemon-runtime");
    let live = directory.path().join("live");
    let (shadow, proposal) = source_fixture(directory.path());
    let mut supervisor = WorkerSupervisor::open(&state, worker, options()).unwrap();
    let (image, key, image_id) = signed_image(fs::read(worker).unwrap(), "tamper-proof");
    let ledger = evolution_ledger(directory.path());
    let transaction_root = directory.path().join("tx");
    let promotion = PromotionTransaction::open(&transaction_root, &live).unwrap();
    let promoted = promotion
        .promote(
            &mut supervisor,
            &ledger,
            PromotionRequest {
                hypothesis_id: EntityId::new(),
                occurred_at: keith_agent_types::UtcTimestamp::from_unix_millis(10),
                image: &image,
                trusted_public_key: &key,
                canary: &canary(image_id.clone()),
                proposal: &proposal,
                shadow_root: &shadow,
                failure_threshold: 1,
            },
        )
        .unwrap();
    drop(promotion);
    let archive = fs::read_dir(transaction_root.join("promotion-history"))
        .unwrap()
        .next()
        .unwrap()
        .unwrap()
        .path();
    let mut record: PromotionRecord = serde_json::from_slice(&fs::read(&archive).unwrap()).unwrap();
    record.source_changes[0].prior_bytes = Some(b"forged prior bytes".to_vec());
    fs::write(&archive, canonical_json_bytes(&record).unwrap()).unwrap();

    let enablement =
        SelfEvolutionEnablement::new(live.clone(), [71; 32], "owner".into(), ledger.clone());
    let installation = enablement.authenticate_installation(&[71; 32]).unwrap();
    let authority = enablement.authorize_reversal(&installation).unwrap();
    let reversal = ReversalTransaction::open(&transaction_root, &live).unwrap();
    let error = reversal
        .reverse(
            &mut supervisor,
            &ledger,
            ReversalRequest {
                scope: ReversalScope::Promotion(promoted.transaction_id),
                trusted_public_key: &key,
                authority: &authority,
                reason: "must fail closed",
                occurred_at: keith_agent_types::UtcTimestamp::from_unix_millis(100),
            },
        )
        .unwrap_err();
    assert!(matches!(error, ReversalError::Invalid(_)));
    assert_eq!(supervisor.image_registry().current().image_id, image_id);
    assert_eq!(fs::read(live.join(SOURCE)).unwrap(), NEW_SOURCE);
}

#[test]
fn reversal_crash_boundary_matrix_resumes_on_the_pinned_image() {
    if std::env::var_os("KEITH_REVERSAL_CHILD_ROOT").is_some() {
        return;
    }
    for boundary in [
        "prepared",
        "image_pinned",
        "worker_rolled",
        "workers_rolled",
        "source_restored",
        "source_complete",
        "ledger_recorded",
        "complete",
    ] {
        let directory = tempfile::tempdir().unwrap();
        let status = Command::new(std::env::current_exe().unwrap())
            .args(["--exact", "reversal_crash_child", "--nocapture"])
            .env("KEITH_REVERSAL_CHILD_ROOT", directory.path())
            .env("KEITH_REVERSAL_CRASH_AT", boundary)
            .status()
            .unwrap();
        assert_eq!(status.code(), Some(87), "boundary {boundary}");

        let worker = Path::new(env!("CARGO_BIN_EXE_keith-promotion-worker-process"));
        let state = directory.path().join("daemon-runtime");
        let live = directory.path().join("live");
        let mut supervisor = WorkerSupervisor::open(&state, worker, options()).unwrap();
        supervisor.adopt_existing().unwrap();
        let ledger = EvolutionLedger::from_seed(
            Arc::new(
                EmbeddedStore::open(&directory.path().join("evolution.sqlite"), None).unwrap(),
            ),
            &[44; 32],
        )
        .unwrap();
        let reversal = ReversalTransaction::open(directory.path().join("tx"), &live).unwrap();
        reversal
            .recover(
                &mut supervisor,
                &ledger,
                keith_agent_types::UtcTimestamp::from_unix_millis(100),
            )
            .unwrap()
            .unwrap();
        let bootstrap = format!("bootstrap-{}", sha256(&fs::read(worker).unwrap()));
        assert_eq!(supervisor.image_registry().current().image_id, bootstrap);
        assert_eq!(fs::read(live.join(SOURCE)).unwrap(), OLD_SOURCE);
        assert!(supervisor.statuses().iter().all(|status| {
            status.image_id == bootstrap && status.health == WorkerHealth::Healthy
        }));
        assert_eq!(
            ledger
                .records()
                .unwrap()
                .iter()
                .filter(|record| matches!(record.event, EvolutionEvent::Revert { .. }))
                .count(),
            1
        );
        supervisor.drain_all().unwrap();
    }
}

#[test]
fn reversal_crash_child() {
    let Some(root) = std::env::var_os("KEITH_REVERSAL_CHILD_ROOT") else {
        return;
    };
    let root = PathBuf::from(root);
    let worker = Path::new(env!("CARGO_BIN_EXE_keith-promotion-worker-process"));
    let state = root.join("daemon-runtime");
    let live = root.join("live");
    let (shadow, proposal) = source_fixture(&root);
    let mut supervisor = WorkerSupervisor::open(&state, worker, options()).unwrap();
    supervisor
        .start(keith_agent_types::RootTreeId::new())
        .unwrap();
    let (image, key, image_id) = signed_image(fs::read(worker).unwrap(), "reversal-crash");
    let transaction_root = root.join("tx");
    let ledger = evolution_ledger(&root);
    let promotion = PromotionTransaction::open(&transaction_root, &live).unwrap();
    let promoted = promotion
        .promote(
            &mut supervisor,
            &ledger,
            PromotionRequest {
                hypothesis_id: EntityId::new(),
                occurred_at: keith_agent_types::UtcTimestamp::from_unix_millis(10),
                image: &image,
                trusted_public_key: &key,
                canary: &canary(image_id),
                proposal: &proposal,
                shadow_root: &shadow,
                failure_threshold: 1,
            },
        )
        .unwrap();
    drop(promotion);
    let enablement =
        SelfEvolutionEnablement::new(live.clone(), [71; 32], "owner".into(), ledger.clone());
    let installation = enablement.authenticate_installation(&[71; 32]).unwrap();
    let authority = enablement.authorize_reversal(&installation).unwrap();
    let reversal = ReversalTransaction::open(&transaction_root, &live).unwrap();
    let _ = reversal.reverse(
        &mut supervisor,
        &ledger,
        ReversalRequest {
            scope: ReversalScope::Promotion(promoted.transaction_id),
            trusted_public_key: &key,
            authority: &authority,
            reason: "crash recovery proof",
            occurred_at: keith_agent_types::UtcTimestamp::from_unix_millis(100),
        },
    );
    panic!("reversal crash boundary was not reached");
}

#[test]
fn promotion_crash_boundary_matrix_recovers_to_old_or_fully_committed_state() {
    if std::env::var_os("KEITH_PROMOTION_CHILD_ROOT").is_some() {
        return;
    }
    for boundary in [
        "prepared",
        "installed",
        "image_selected",
        "rolling",
        "rolled_root",
        "workers_rolled",
        "source_writing",
        "source_0",
        "committed",
    ] {
        let directory = tempfile::tempdir().unwrap();
        let status = Command::new(std::env::current_exe().unwrap())
            .args(["--exact", "promotion_crash_child", "--nocapture"])
            .env("KEITH_PROMOTION_CHILD_ROOT", directory.path())
            .env("KEITH_PROMOTION_CRASH_AT", boundary)
            .status()
            .unwrap();
        assert_eq!(status.code(), Some(86), "boundary {boundary}");

        let worker = Path::new(env!("CARGO_BIN_EXE_keith-promotion-worker-process"));
        let state = directory.path().join("daemon-runtime");
        let live = directory.path().join("live");
        let mut supervisor = WorkerSupervisor::open(&state, worker, options()).unwrap();
        supervisor.adopt_existing().unwrap();
        let transaction = PromotionTransaction::open(directory.path().join("tx"), &live).unwrap();
        let ledger = evolution_ledger(directory.path());
        let recovered = transaction.recover(&mut supervisor, &ledger).unwrap();
        if boundary == "committed" {
            assert!(!recovered);
            assert_eq!(fs::read(live.join(SOURCE)).unwrap(), NEW_SOURCE);
        } else {
            assert!(recovered, "boundary {boundary}");
            assert_eq!(fs::read(live.join(SOURCE)).unwrap(), OLD_SOURCE);
            let prior = format!("bootstrap-{}", sha256(&fs::read(worker).unwrap()));
            assert_eq!(supervisor.image_registry().current().image_id, prior);
            assert!(supervisor.statuses().iter().all(|status| {
                status.image_id == prior && status.health == WorkerHealth::Healthy
            }));
        }
        supervisor.drain_all().unwrap();
    }
}

#[test]
fn promotion_crash_child() {
    let Some(root) = std::env::var_os("KEITH_PROMOTION_CHILD_ROOT") else {
        return;
    };
    let root = PathBuf::from(root);
    let worker = Path::new(env!("CARGO_BIN_EXE_keith-promotion-worker-process"));
    let state = root.join("daemon-runtime");
    let live = root.join("live");
    let (shadow, proposal) = source_fixture(&root);
    let mut supervisor = WorkerSupervisor::open(&state, worker, options()).unwrap();
    supervisor
        .start(keith_agent_types::RootTreeId::new())
        .unwrap();
    let (image, key, image_id) = signed_image(fs::read(worker).unwrap(), "crash-matrix");
    let ledger = evolution_ledger(&root);
    let transaction = PromotionTransaction::open(root.join("tx"), &live).unwrap();
    let _ = transaction.promote(
        &mut supervisor,
        &ledger,
        PromotionRequest {
            hypothesis_id: EntityId::new(),
            occurred_at: keith_agent_types::UtcTimestamp::from_unix_millis(10),
            image: &image,
            trusted_public_key: &key,
            canary: &canary(image_id),
            proposal: &proposal,
            shadow_root: &shadow,
            failure_threshold: 1,
        },
    );
    panic!("crash boundary was not reached");
}
