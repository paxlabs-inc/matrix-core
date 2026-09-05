#![cfg(unix)]

use std::fs;
use std::path::{Path, PathBuf};
use std::process::Command;
use std::sync::Arc;
use std::time::{Duration, Instant};

use keith_agent_types::{EntityId, ProfileId, RootTreeId, SessionId, UtcTimestamp};
use keith_meta_harness::{
    CandidateEdit, CandidateExecutionRequest, CandidateExecutor, CandidateLimits, CandidatePolicy,
    CandidateProposal, CandidateRegistry, CandidateRun, CandidateSourceKind, CausalEvidence,
    CausalRole, CompletionEvidence, ContextSelectionEvidence, CostEvidence, DiagnosisRequest,
    DiagnosticLimits, EvaluationCase, EvaluationCostCeiling, EvaluationDataset,
    HarnessFailureClass, HarnessFaultCategory, HarnessModeAvailability, HarnessOperationMode,
    HarnessRepairPhase, HarnessRetryAuthority, HarnessSourceEvidence, HarnessTraceBundle,
    IndependentEvaluator, LatencyEvidence, MetaHarness, MetricDirection, RedactedText,
    RegressionBounds, RetryEvidence, TargetMetric, TaskOutcome, TaskOutcomeState,
    ToolSchemaEvidence,
};
use keith_platform_contracts::{
    AuditCorrelationId, ExecutionTraceBundle, PLATFORM_CONTRACT_VERSION, TraceEvent, TraceEventKind,
};
use keith_provider_core::CancellationToken;
use keith_self_evolution::{
    BuildSandbox, CanaryRunner, EvolutionLedger, EvolutionWorkRoot, HarnessBuildOutcome,
    HarnessCanaryOutcome, HarnessObservationPlan, MetaHarnessCoordinator, ObservationSignal,
    PromotionTransaction, ReversalTransaction, RevertWatchdog, SelfEvolutionEnablement,
    ThresholdDirection, VerificationGate, WatchdogDecision, WatchdogNotificationPolicy,
    WatchdogThresholds, WorkerImageSigner,
};
use keith_state_store::EmbeddedStore;
use keith_supervisor::{SupervisorOptions, WorkerSupervisor};
use keith_telemetry::MetricName;
use sha2::{Digest, Sha256};

const ROUTING_PATH: &str = "apps/agent-worker/src/routing.rs";
const DEFECTIVE_ROUTING: &str = r#"pub fn route(value: &str) -> String {
    format!("misrouted:{value}")
}

pub const fn claims_success(_value: &str) -> bool {
    false
}

#[allow(dead_code)]
fn main() {
    let value = std::env::args().nth(1).expect("input");
    println!("{}", route(&value));
    if !claims_success(&value) {
        std::process::exit(1);
    }
}
"#;
const REPAIRED_ROUTING: &str = r#"pub fn route(value: &str) -> String {
    format!("routed:{value}")
}

pub const fn claims_success(_value: &str) -> bool {
    true
}

#[allow(dead_code)]
fn main() {
    let value = std::env::args().nth(1).expect("input");
    println!("{}", route(&value));
    if !claims_success(&value) {
        std::process::exit(1);
    }
}
"#;
const REGRESSED_ROUTING: &str = r#"pub fn route(value: &str) -> String {
    if matches!(value, "alpha" | "beta") {
        format!("routed:{value}")
    } else {
        format!("misrouted:{value}")
    }
}

pub fn claims_success(value: &str) -> bool {
    matches!(value, "alpha" | "beta")
}

#[allow(dead_code)]
fn main() {
    let value = std::env::args().nth(1).expect("input");
    println!("{}", route(&value));
    if !claims_success(&value) {
        std::process::exit(1);
    }
}
"#;

struct RoutingExecutor;

impl CandidateExecutor for RoutingExecutor {
    fn execute(
        &mut self,
        request: CandidateExecutionRequest<'_>,
    ) -> Result<CandidateRun, RedactedText> {
        let input = std::str::from_utf8(request.input).map_err(|_| text("invalid input"))?;
        let output = compile_and_run(&request.shadow_root.join(ROUTING_PATH), input)
            .map_err(|_| text("routing candidate did not compile or run"))?;
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
fn meta_harness_real_build_canary_known_good_retry_and_one_action_reversal() {
    let started = Instant::now();
    let directory = tempfile::tempdir().expect("qualification root");
    let source = create_bounded_workspace(directory.path());
    let original_source_digest = digest_tree(&source);
    let failed =
        compile_and_run(&source.join(ROUTING_PATH), "request").expect("run deliberate defect");
    assert!(!failed.status.success());
    assert_eq!(failed.stdout, b"misrouted:request\n");

    let diagnostic = MetaHarness::open(
        directory.path().join("diagnostic-state"),
        DiagnosticLimits::default(),
    )
    .expect("diagnostic service");
    let diagnosis = diagnostic
        .diagnose(
            failed_trace(),
            diagnosis_request(),
            UtcTimestamp::from_unix_millis(1),
        )
        .expect("diagnose routing failure")
        .record
        .diagnosis
        .expect("harness diagnosis");
    assert_eq!(diagnosis.fault_category, HarnessFaultCategory::Routing);

    let candidates = CandidateRegistry::open(
        directory.path().join("candidate-state"),
        CandidateLimits::default(),
        CandidatePolicy::new([PathBuf::from("apps/agent-worker/src")]).expect("candidate policy"),
    )
    .expect("candidate registry");
    let population = candidates
        .create_population(
            &source,
            &diagnosis,
            vec![
                proposal("route every request", REPAIRED_ROUTING),
                proposal("route only observed examples", REGRESSED_ROUTING),
            ],
            UtcTimestamp::from_unix_millis(2),
        )
        .expect("candidate population");
    let evaluator_root = directory.path().join("operator-evaluator");
    fs::create_dir(&evaluator_root).expect("evaluator root");
    fs::write(evaluator_root.join("version"), "operator-owned-v1").expect("evaluator version");
    let evaluated = candidates
        .evaluate_population(
            &population.id,
            &evaluator(&evaluator_root),
            &mut RoutingExecutor,
            UtcTimestamp::from_unix_millis(3),
        )
        .expect("held-out evaluation");
    let winner = evaluated
        .candidates
        .iter()
        .find(|candidate| candidate.hypothesis.as_str() == "route every request")
        .expect("winning candidate");
    assert_eq!(evaluated.frontier.candidate_ids, vec![winner.id.clone()]);
    assert_eq!(digest_tree(&source), original_source_digest);

    let repository = directory.path().join("repository");
    copy_tree(&source, &repository);
    git(&repository, &["init", "-q"]);
    git(&repository, &["add", "."]);
    git(
        &repository,
        &[
            "-c",
            "user.name=meta-harness-qualification",
            "-c",
            "user.email=meta-harness@example.invalid",
            "commit",
            "-qm",
            "deliberate routing defect",
        ],
    );
    let revision = command_text(&repository, &["rev-parse", "HEAD"]);
    let live = directory.path().join("live");
    copy_tree(&source, &live);

    let availability = HarnessModeAvailability::fully_available();
    assert!(availability.supports(HarnessOperationMode::Advisory));
    assert!(availability.supports(HarnessOperationMode::Shadow));
    assert!(availability.supports(HarnessOperationMode::Autonomous));
    let coordinator =
        MetaHarnessCoordinator::open(directory.path().join("promotion-state"), availability)
            .expect("promotion coordinator");
    let admitted = coordinator
        .admit(
            &evaluated,
            &winner.id,
            HarnessOperationMode::Autonomous,
            UtcTimestamp::from_unix_millis(4),
        )
        .expect("admit Pareto winner");
    assert_eq!(admitted.retry_authority(), HarnessRetryAuthority::Denied);
    let work_root = EvolutionWorkRoot::open(directory.path().join("evolution-work"))
        .expect("evolution work root");
    let prepared = coordinator
        .prepare_candidate(
            &admitted.id,
            &evaluated,
            &repository,
            &revision,
            &work_root,
            UtcTimestamp::from_unix_millis(5),
        )
        .expect("prepare exact candidate");
    assert_eq!(digest_tree(&source), original_source_digest);

    let cargo = rustup_path("cargo");
    let rustc = rustup_path("rustc");
    let cargo_version =
        command_text_inherit(cargo.to_str().expect("utf8 cargo path"), &["--version"]);
    let rustc_version =
        command_text_inherit(rustc.to_str().expect("utf8 rustc path"), &["--version"]);
    assert!(cargo_version.starts_with("cargo 1.93.0"), "{cargo_version}");
    assert!(rustc_version.starts_with("rustc 1.93.0"), "{rustc_version}");
    eprintln!("META_HARNESS_TOOLCHAIN cargo={cargo_version} rustc={rustc_version}");
    let cargo_home = environment_home("CARGO_HOME", ".cargo");
    let rustup_home = PathBuf::from(command_text_inherit("rustup", &["show", "home"]));
    let sandbox = BuildSandbox::production(cargo, rustc, cargo_home, rustup_home)
        .expect("strong build sandbox");
    let signer = WorkerImageSigner::from_seed(&[42; 32]).expect("image signer");
    let public_key = signer.public_key();
    let gate = VerificationGate::new(sandbox, signer, vec![b"never-export-this-secret".to_vec()]);
    let built = match coordinator
        .build_candidate(
            prepared,
            &gate,
            &CancellationToken::default(),
            UtcTimestamp::from_unix_millis(6),
        )
        .expect("real candidate gate")
    {
        HarnessBuildOutcome::Passed(candidate) => candidate,
        HarnessBuildOutcome::Rejected(rejected) => {
            panic!(
                "real build rejected at {:?}: {}",
                rejected.failure.gate,
                rejected
                    .gate_results
                    .last()
                    .map_or("no gate output", |result| result.output.as_str())
            )
        }
    };
    assert_eq!(built.gate_results.len(), 6);
    assert!(built.gate_results.iter().all(|result| result.succeeded()));
    built
        .image
        .verify(&public_key)
        .expect("signed worker image");

    let bootstrap = Path::new(env!("CARGO_BIN_EXE_keith-promotion-worker-process"));
    let bootstrap_bytes = fs::read(bootstrap).expect("bootstrap worker bytes");
    let daemon_root = directory.path().join("daemon-runtime");
    let mut supervisor =
        WorkerSupervisor::open(&daemon_root, bootstrap, supervisor_options()).expect("supervisor");
    let root_tree = RootTreeId::new();
    supervisor
        .start(root_tree.clone())
        .expect("bootstrap worker");
    let prior_image_id = supervisor.image_registry().current().image_id.clone();
    let prior_image_digest = sha256(&bootstrap_bytes);
    let canary_runner = CanaryRunner::open(
        directory.path().join("canary-runtime"),
        supervisor_options(),
    )
    .expect("canary runner");
    let canary = match coordinator
        .run_canary(
            *built,
            &canary_runner,
            &mut supervisor,
            &public_key,
            MetricName::ModelLatency,
            100.0,
            100.0,
            UtcTimestamp::from_unix_millis(7),
        )
        .expect("real candidate canary")
    {
        HarnessCanaryOutcome::Passed(candidate) => candidate,
        HarnessCanaryOutcome::Rejected(rejected) => {
            panic!("real canary rejected: {:?}", rejected.evaluation.verdict)
        }
    };
    assert_eq!(canary.evaluation.measurements.len(), 7);
    assert!(canary.evaluation.measured.is_some());
    assert_eq!(canary.operation.phase, HarnessRepairPhase::CanaryPassed);
    assert!(canary.operation.promotion_allowed());

    let ledger = Arc::new(
        EvolutionLedger::from_seed(
            Arc::new(
                EmbeddedStore::open(&directory.path().join("evolution.sqlite"), None)
                    .expect("evolution store"),
            ),
            &[44; 32],
        )
        .expect("evolution ledger"),
    );
    let transaction_root = directory.path().join("promotion-transaction");
    let promotion =
        PromotionTransaction::open(&transaction_root, &live).expect("promotion transaction");
    let mut watchdog = RevertWatchdog::open(&transaction_root).expect("promotion watchdog");
    let plan = HarnessObservationPlan {
        profile_id: EntityId::new(),
        metric: MetricName::Workers,
        started_at: UtcTimestamp::from_unix_millis(10),
        deadline: UtcTimestamp::from_unix_millis(100),
        previous_image_retain_until: UtcTimestamp::from_unix_millis(1_000),
        thresholds: WatchdogThresholds {
            hypothesis_direction: ThresholdDirection::AtLeast,
            hypothesis_revert_threshold: 1.0,
            maximum_crashes: 1,
            maximum_turn_failure_rate: 0.1,
            maximum_mean_latency_ms: 1_000,
            maximum_total_token_cost: 10_000,
            maximum_resident_bytes: u64::MAX,
            maximum_virtual_bytes: u64::MAX,
            minimum_hypothesis_samples: 1,
            minimum_turn_samples: 1,
            minimum_resource_samples: 1,
        },
        notification_policy: WatchdogNotificationPolicy::NotifyOnRevert,
        promotion_failure_threshold: 1,
    };
    let promotion_result = coordinator.promote_and_observe(
        *canary,
        &promotion,
        &mut watchdog,
        &mut supervisor,
        &ledger,
        &public_key,
        &plan,
        UtcTimestamp::from_unix_millis(10),
    );
    let observing = match promotion_result {
        Ok(operation) => operation,
        Err(error) => panic!(
            "promote candidate: {error:?}; durable phase: {:?}",
            coordinator
                .operation(&admitted.id)
                .expect("operation after promotion failure")
                .phase
        ),
    };
    assert_eq!(observing.phase, HarnessRepairPhase::Observing);
    let candidate_image_id = observing
        .build_image_id
        .clone()
        .expect("candidate image id");
    let generation = supervisor
        .status(&root_tree)
        .expect("candidate status")
        .generation;
    for (signal, at) in [
        (
            ObservationSignal::HypothesisMetric {
                image_id: candidate_image_id.clone(),
                generation,
                value: 2.0,
            },
            20,
        ),
        (
            ObservationSignal::Turn {
                image_id: candidate_image_id.clone(),
                generation,
                succeeded: true,
                latency_ms: 10,
                token_cost: 3,
            },
            30,
        ),
        (
            ObservationSignal::Resource {
                image_id: candidate_image_id.clone(),
                generation,
                resident_bytes: 1,
                virtual_bytes: 1,
            },
            40,
        ),
    ] {
        assert_eq!(
            coordinator
                .observe_signal(
                    &observing.id,
                    &mut watchdog,
                    &mut supervisor,
                    &ledger,
                    &public_key,
                    signal,
                    UtcTimestamp::from_unix_millis(at),
                )
                .expect("healthy observation"),
            WatchdogDecision::Observing
        );
    }
    assert_eq!(
        coordinator
            .tick_observation(
                &observing.id,
                &mut watchdog,
                &mut supervisor,
                &ledger,
                &public_key,
                UtcTimestamp::from_unix_millis(100),
            )
            .expect("advance known good"),
        WatchdogDecision::KnownGood
    );
    let promoted = coordinator
        .operation(&observing.id)
        .expect("promoted operation");
    assert_eq!(promoted.phase, HarnessRepairPhase::Promoted);
    assert_eq!(
        promoted.retry_authority(),
        HarnessRetryAuthority::PromotedCandidate
    );
    assert_eq!(
        supervisor.image_registry().known_good().image_id,
        candidate_image_id
    );

    let repaired_task =
        compile_and_run(&live.join(ROUTING_PATH), "request").expect("rerun original task");
    assert!(repaired_task.status.success());
    assert_eq!(repaired_task.stdout, b"routed:request\n");
    let installed_task = Command::new(&supervisor.image_registry().current().executable)
        .args(["--harness-task", "request"])
        .output()
        .expect("run promoted image task");
    assert!(installed_task.status.success());
    assert_eq!(installed_task.stdout, b"routed:request\n");

    drop(promotion);
    let enablement = SelfEvolutionEnablement::new(
        live.clone(),
        [71; 32],
        "installation-owner".into(),
        ledger.clone(),
    );
    let installation = enablement
        .authenticate_installation(&[71; 32])
        .expect("installation authority");
    let authority = enablement
        .authorize_reversal(&installation)
        .expect("reversal authority");
    let reversal =
        ReversalTransaction::open(&transaction_root, &live).expect("reversal transaction");
    let reverted = coordinator
        .reverse(
            &promoted.id,
            &reversal,
            &mut supervisor,
            &ledger,
            &public_key,
            &authority,
            "owner selected one-action undo",
            UtcTimestamp::from_unix_millis(200),
        )
        .expect("one-action reversal");
    assert_eq!(reverted.phase, HarnessRepairPhase::Reverted);
    assert_eq!(digest_tree(&live), original_source_digest);
    assert_eq!(
        fs::read_to_string(live.join(ROUTING_PATH)).expect("restored route"),
        DEFECTIVE_ROUTING
    );
    assert_eq!(
        supervisor.image_registry().current().image_id,
        prior_image_id
    );
    assert_eq!(
        sha256(
            &fs::read(&supervisor.image_registry().current().executable)
                .expect("restored image bytes")
        ),
        prior_image_digest
    );
    supervisor.drain_all().expect("drain workers");
    candidates
        .cleanup_population(&evaluated.id, UtcTimestamp::from_unix_millis(300))
        .expect("reclaim candidate shadows");
    eprintln!(
        "META_HARNESS_QUALIFICATION build_gates=6 canary_journeys=7 source_restore=byte_exact image_restore=byte_exact elapsed_ms={}",
        started.elapsed().as_millis()
    );
}

fn create_bounded_workspace(root: &Path) -> PathBuf {
    let project = Path::new(env!("CARGO_MANIFEST_DIR"))
        .parent()
        .and_then(Path::parent)
        .expect("workspace root");
    let source = root.join("source-export");
    fs::create_dir(&source).expect("source export");
    let members = [
        "crates/agent-types",
        "crates/build-info",
        "crates/connection",
        "crates/framing",
        "crates/platform",
        "crates/platform-contracts",
        "crates/protocol",
        "crates/runtime-api",
        "crates/state-store",
        "crates/state-store-core",
        "crates/worker-runtime",
        "apps/agent-worker",
        "apps/xtask",
    ];
    for member in &members[..11] {
        copy_tree(&project.join(member), &source.join(member));
    }
    let manifest = fs::read_to_string(project.join("Cargo.toml")).expect("workspace manifest");
    let manifest = manifest.replacen(
        "members = [\"apps/*\", \"crates/*\"]",
        &format!(
            "members = [{}]",
            members
                .iter()
                .map(|member| format!("\"{member}\""))
                .collect::<Vec<_>>()
                .join(", ")
        ),
        1,
    );
    let manifest = manifest.replace(
        "codegen-units = 1\nlto = \"thin\"",
        "codegen-units = 16\nlto = false",
    );
    fs::write(source.join("Cargo.toml"), manifest).expect("bounded workspace manifest");
    fs::copy(project.join("Cargo.lock"), source.join("Cargo.lock")).expect("workspace lock");

    let worker = source.join("apps/agent-worker");
    fs::create_dir_all(worker.join("src")).expect("worker source directory");
    fs::create_dir(worker.join("corpus")).expect("worker corpus directory");
    fs::write(worker.join("Cargo.toml"), WORKER_MANIFEST).expect("worker manifest");
    fs::write(worker.join("src/main.rs"), WORKER_MAIN).expect("worker main");
    fs::write(worker.join("src/routing.rs"), DEFECTIVE_ROUTING).expect("defective routing");
    fs::copy(
        project.join("crates/self-evolution/corpus/v1.json"),
        worker.join("corpus/v1.json"),
    )
    .expect("public canary corpus");

    let xtask = source.join("apps/xtask");
    fs::create_dir_all(xtask.join("src")).expect("xtask source directory");
    fs::write(xtask.join("Cargo.toml"), XTASK_MANIFEST).expect("xtask manifest");
    fs::write(xtask.join("src/main.rs"), XTASK_MAIN).expect("xtask main");
    let locked = Command::new(rustup_path("cargo"))
        .args(["generate-lockfile", "--offline"])
        .current_dir(&source)
        .status()
        .expect("lock bounded workspace");
    assert!(locked.success());
    let formatted = Command::new(rustup_path("cargo"))
        .args(["fmt", "--all"])
        .current_dir(&source)
        .status()
        .expect("format bounded workspace");
    assert!(formatted.success());
    source
}

fn proposal(hypothesis: &str, contents: &str) -> CandidateProposal {
    CandidateProposal {
        parent_id: None,
        source: CandidateSourceKind::Proposed,
        hypothesis: text(hypothesis),
        edits: vec![CandidateEdit::Write {
            relative_path: PathBuf::from(ROUTING_PATH),
            expected_digest: None,
            contents: contents.into(),
        }],
        trace_references: vec![text("deliberate-routing-process")],
        safe_trace_excerpts: vec![text("request was routed to the wrong destination")],
        proposal_tokens: 12,
        estimated_latency_ms: 1,
        estimated_external_cost_micros: 0,
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

fn failed_trace() -> HarnessTraceBundle {
    let kinds = [
        TraceEventKind::DurableTransition,
        TraceEventKind::ModelProgress,
        TraceEventKind::ToolCall,
        TraceEventKind::Observation,
        TraceEventKind::CompletionDecision,
        TraceEventKind::Cost,
        TraceEventKind::Latency,
        TraceEventKind::Failure,
    ];
    HarnessTraceBundle {
        execution: ExecutionTraceBundle {
            contract_version: PLATFORM_CONTRACT_VERSION,
            profile_id: ProfileId::new(),
            session_id: SessionId::new(),
            audit_correlation: AuditCorrelationId::new(),
            events: kinds
                .into_iter()
                .enumerate()
                .map(|(index, kind)| TraceEvent {
                    sequence: u64::try_from(index + 1).expect("sequence"),
                    occurred_at: UtcTimestamp::from_unix_millis(
                        i64::try_from(index + 1).expect("timestamp"),
                    ),
                    kind,
                    label: text("real routing process event"),
                    safe_detail: Some(text("bounded deterministic evidence")),
                    payload_digest: Some(digest_text('a')),
                })
                .collect(),
            redacted: true,
        },
        harness_source: vec![HarnessSourceEvidence {
            relative_path: text(ROUTING_PATH),
            source_digest: digest_text('b'),
            safe_excerpt: text("the route function emits the wrong destination"),
        }],
        task_outcome: TaskOutcome {
            state: TaskOutcomeState::Failed,
            safe_summary: text("the bounded task process exited with failure"),
            task_score_basis_points: Some(0),
        },
        user_corrections: Vec::new(),
        tool_schemas: vec![ToolSchemaEvidence {
            tool_name: text("request_router"),
            schema_digest: digest_text('c'),
            safe_schema: text(r#"{"type":"string"}"#),
        }],
        context_selection: ContextSelectionEvidence {
            selected_items: 1,
            omitted_items: 0,
            token_budget: 1_024,
            selected_digest: digest_text('d'),
            safe_basis: text("the complete request was selected"),
        },
        retries: RetryEvidence {
            attempts: 0,
            exhausted: false,
            safe_basis: text("the deterministic reproduction required no retry"),
        },
        completion: CompletionEvidence {
            event_sequence: 5,
            claimed_success: false,
            safe_basis: text("the process exited nonzero"),
        },
        cost: CostEvidence {
            input_tokens: 1,
            output_tokens: 1,
            external_cost_micros: 0,
        },
        latency: LatencyEvidence {
            wall_ms: 1,
            model_ms: 0,
            tool_ms: 1,
        },
        runtime_evidence: vec![keith_meta_harness::DeterministicRuntimeEvidence {
            component: text("request context router"),
            observed: text("the process emitted misrouted request and exited one"),
            expected: text("the process emits routed request and exits zero"),
            evidence_digest: digest_text('e'),
        }],
        causal_evidence: vec![CausalEvidence {
            event_sequence: 4,
            failure_class: HarnessFailureClass::HarnessCaused,
            role: CausalRole::Direct,
            reliability_basis_points: 10_000,
            harness_category: Some(HarnessFaultCategory::Routing),
            causal_component: text("request context router"),
            observed: text("the deterministic process selected the wrong route"),
            expected: text("the deterministic process selects the requested route"),
            reproduction: text("compile the bounded router and pass request"),
        }],
    }
}

fn diagnosis_request() -> DiagnosisRequest {
    DiagnosisRequest {
        expected_behavior_change: text("route the complete request context"),
        target_metric: TargetMetric {
            name: text("successful routed tasks"),
            direction: MetricDirection::Increase,
            baseline: 0,
            threshold: 1,
            revert_threshold: 0,
        },
        cost_ceiling: EvaluationCostCeiling {
            max_external_cost_micros: 10,
            max_latency_ms: 60_000,
            max_tokens: 1_000,
            max_retries: 2,
        },
    }
}

fn supervisor_options() -> SupervisorOptions {
    SupervisorOptions {
        startup_timeout: Duration::from_secs(5),
        drain_timeout: Duration::from_secs(2),
        stale_heartbeat: Duration::from_secs(2),
        heartbeat_interval: Duration::from_millis(20),
        lease_duration: Duration::from_millis(500),
    }
}

fn compile_and_run(source: &Path, input: &str) -> std::io::Result<std::process::Output> {
    let build = tempfile::tempdir()?;
    let executable = build.path().join("routing-process");
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

fn copy_tree(source: &Path, destination: &Path) {
    fs::create_dir_all(destination).expect("destination directory");
    for entry in fs::read_dir(source).expect("read source directory") {
        let entry = entry.expect("source entry");
        let kind = entry.file_type().expect("source type");
        let target = destination.join(entry.file_name());
        if kind.is_dir() {
            copy_tree(&entry.path(), &target);
        } else {
            assert!(kind.is_file());
            fs::copy(entry.path(), target).expect("copy source file");
        }
    }
}

fn digest_tree(root: &Path) -> String {
    fn visit(root: &Path, directory: &Path, hasher: &mut Sha256) {
        let mut entries = fs::read_dir(directory)
            .expect("digest directory")
            .collect::<Result<Vec<_>, _>>()
            .expect("digest entries");
        entries.sort_by_key(std::fs::DirEntry::file_name);
        for entry in entries {
            let path = entry.path();
            let relative = path.strip_prefix(root).expect("relative digest path");
            hasher.update(relative.to_string_lossy().as_bytes());
            if entry.file_type().expect("digest type").is_dir() {
                hasher.update(b"directory\0");
                visit(root, &path, hasher);
            } else {
                hasher.update(b"file\0");
                hasher.update(fs::read(path).expect("digest file"));
            }
        }
    }
    let mut hasher = Sha256::new();
    visit(root, root, &mut hasher);
    format!("{:x}", hasher.finalize())
}

fn sha256(bytes: &[u8]) -> String {
    format!("{:x}", Sha256::digest(bytes))
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

fn command_text_inherit(program: &str, arguments: &[&str]) -> String {
    let output = Command::new(program)
        .args(arguments)
        .output()
        .expect("tool output");
    assert!(output.status.success());
    String::from_utf8(output.stdout)
        .expect("utf8 tool output")
        .trim()
        .into()
}

fn rustup_path(tool: &str) -> PathBuf {
    PathBuf::from(command_text_inherit(
        "rustup",
        &["which", "--toolchain", "1.93.0", tool],
    ))
}

fn environment_home(variable: &str, fallback: &str) -> PathBuf {
    std::env::var_os(variable).map_or_else(
        || PathBuf::from(std::env::var_os("HOME").expect("home directory")).join(fallback),
        PathBuf::from,
    )
}

fn digest_text(byte: char) -> RedactedText {
    text(&format!("sha256:{}", byte.to_string().repeat(64)))
}

fn text(value: &str) -> RedactedText {
    RedactedText::parse(value).expect("safe text")
}

const WORKER_MANIFEST: &str = r#"[package]
name = "agent-worker"
version.workspace = true
edition.workspace = true
rust-version.workspace = true
license.workspace = true
repository.workspace = true
publish = false

[dependencies]
base64 = "0.22"
keith-agent-types = { path = "../../crates/agent-types" }
keith-build-info = { path = "../../crates/build-info" }
keith-protocol = { path = "../../crates/protocol" }
keith-runtime-api = { path = "../../crates/runtime-api" }
keith-worker-runtime = { path = "../../crates/worker-runtime" }
serde_json.workspace = true
sha2.workspace = true

[lints]
workspace = true
"#;

const XTASK_MANIFEST: &str = r#"[package]
name = "keith-xtask"
version.workspace = true
edition.workspace = true
rust-version.workspace = true
license.workspace = true
repository.workspace = true
publish = false

[lints]
workspace = true
"#;

const XTASK_MAIN: &str = r#"use std::fs;
use std::io;
use std::path::Path;
use std::process::Command;

fn main() -> Result<(), Box<dyn std::error::Error>> {
    let gate = std::env::args().nth(1).unwrap_or_default();
    match gate.as_str() {
        "dependency-policy" => dependency_policy()?,
        "security-gate" => security_gate()?,
        "platform-gate" => platform_gate()?,
        _ => return Err(io::Error::other("unknown gate").into()),
    }
    Ok(())
}

fn dependency_policy() -> io::Result<()> {
    let manifest = fs::read_to_string("Cargo.toml")?;
    if manifest.contains("git =") || !Path::new("Cargo.lock").is_file() {
        return Err(io::Error::other("unlocked or remote dependency"));
    }
    Ok(())
}

fn security_gate() -> io::Result<()> {
    for path in [
        "apps/agent-worker/src/main.rs",
        "apps/agent-worker/src/routing.rs",
    ] {
        let source = fs::read_to_string(path)?;
        if source.contains("BEGIN PRIVATE KEY") || source.contains("sk-live-") {
            return Err(io::Error::other("credential material in worker source"));
        }
    }
    Ok(())
}

fn platform_gate() -> io::Result<()> {
    let cargo = std::env::var_os("CARGO").unwrap_or_else(|| "cargo".into());
    let status = Command::new(&cargo)
        .args([
            "build",
            "--release",
            "--offline",
            "--locked",
            "-p",
            "agent-worker",
        ])
        .status()?;
    if !status.success() {
        return Err(io::Error::other("release worker build failed"));
    }
    let worker = Path::new(&std::env::var_os("CARGO_TARGET_DIR").ok_or_else(|| {
        io::Error::other("cargo target directory is not isolated")
    })?)
    .join("release/agent-worker");
    let report = Command::new(worker).arg("--build-info").output()?;
    if !report.status.success()
        || !String::from_utf8_lossy(&report.stdout).contains("\"component\": \"worker\"")
    {
        return Err(io::Error::other("release worker identity failed"));
    }
    Ok(())
}
"#;

const WORKER_MAIN: &str = r#"mod routing;

use base64::Engine as _;
use keith_agent_types::{ClientId, Generation, RootTreeId, SessionId};
use keith_protocol::{
    ClientCommand, CommandResult, CreateSession, ModelSelection, ProfileSummary, SessionSnapshot,
    SessionState, SubmitPrompt,
};
use keith_runtime_api::{
    CandidateCanaryMeasurement, CandidateCanaryOutcome, CandidateCanaryReport,
    CandidateCanaryRequest, CandidateCanaryVerdict, CommandRuntime, RuntimeSession,
};
use serde_json::Value;
use sha2::{Digest, Sha256};

struct ReplayRuntime;

fn unavailable<T>() -> Result<T, String> {
    Err("canary worker rejects ordinary runtime operations".into())
}

impl CommandRuntime for ReplayRuntime {
    fn profiles(&self) -> Result<Vec<ProfileSummary>, String> { unavailable() }
    fn sessions(&self) -> Result<Vec<RuntimeSession>, String> { unavailable() }
    fn create_default_session(&self, _: Option<String>) -> Result<RuntimeSession, String> { unavailable() }
    fn create_session(&self, _: &CreateSession) -> Result<RuntimeSession, String> { unavailable() }
    fn create_default_session_assigned(&self, _: &SessionId, _: &RootTreeId, _: Option<String>) -> Result<RuntimeSession, String> { unavailable() }
    fn create_session_assigned(&self, _: &SessionId, _: &RootTreeId, _: &CreateSession) -> Result<RuntimeSession, String> { unavailable() }
    fn fork_session_assigned(&self, _: &SessionId, _: &SessionId, _: &RootTreeId, _: Option<String>, _: Generation) -> Result<RuntimeSession, String> { unavailable() }
    fn select_model(&self, _: &ModelSelection) -> Result<(), String> { unavailable() }
    fn run_prompt(&self, _: &SubmitPrompt, _: Generation) -> Result<SessionSnapshot, String> { unavailable() }
    fn cancel_active(&self, _: &SessionId) -> Result<bool, String> { unavailable() }
    fn snapshot(&self, _: &SessionId, _: Generation, _: SessionState) -> Result<SessionSnapshot, String> { unavailable() }
    fn execute_feature(&self, _: &ClientId, _: Option<&SessionId>, _: &ClientCommand, _: Generation) -> Result<CommandResult, String> { unavailable() }
    fn maintain(&self) -> Result<(), String> { unavailable() }

    fn candidate_canary(&self, request: &CandidateCanaryRequest) -> Result<CandidateCanaryReport, String> {
        replay_corpus(request)
    }
}

fn replay_corpus(request: &CandidateCanaryRequest) -> Result<CandidateCanaryReport, String> {
    let corpus: Value = serde_json::from_str(include_str!("../corpus/v1.json"))
        .map_err(|error| error.to_string())?;
    let version = u32::try_from(corpus["version"].as_u64().ok_or("corpus version")?)
        .map_err(|_| "corpus version exceeds u32")?;
    let digest = corpus["content_sha256"].as_str().ok_or("corpus digest")?;
    if request.corpus_version != version || request.corpus_sha256 != digest {
        return Err("candidate corpus identity differs from requested corpus".into());
    }
    let journeys = corpus["journeys"].as_array().ok_or("corpus journeys")?;
    let mut measurements = Vec::with_capacity(journeys.len());
    for journey in journeys {
        measurements.push(replay_journey(journey)?);
    }
    Ok(CandidateCanaryReport {
        corpus_version: version,
        corpus_sha256: digest.into(),
        measurements,
    })
}

fn replay_journey(journey: &Value) -> Result<CandidateCanaryMeasurement, String> {
    let mut output = Vec::new();
    let mut outcome = CandidateCanaryOutcome::Failed;
    let mut tokens = 0_u64;
    let mut operations = 1_u64;
    let mut first_clock = None;
    let mut last_clock = None;
    for step in journey["trace"].as_array().ok_or("journey trace")? {
        match step["source"].as_str().ok_or("trace source")? {
            "provider_event" => match step["event"]["event"].as_str().ok_or("provider event")? {
                "text_delta" => output.extend_from_slice(step["event"]["text"].as_str().ok_or("text delta")?.as_bytes()),
                "usage" => tokens = usage(&step["event"]["usage"]),
                "finished" => {
                    outcome = match step["event"]["reason"].as_str().ok_or("finish reason")? {
                        "end_turn" => CandidateCanaryOutcome::Completed,
                        "tool_use" => CandidateCanaryOutcome::ToolUse,
                        "content_rejected" => CandidateCanaryOutcome::Rejected,
                        _ => CandidateCanaryOutcome::Failed,
                    };
                }
                _ => {}
            },
            "provider_terminal" => {
                if let Some(value) = step["result"]["Ok"].as_object() {
                    tokens = usage(&Value::Object(value.clone()));
                }
            }
            "tool_invocation" => operations = operations.saturating_add(1),
            "tool_outcome" => {
                if let Some(encoded) = step["output"].as_str() {
                    output.extend(base64::engine::general_purpose::STANDARD.decode(encoded).map_err(|error| error.to_string())?);
                }
            }
            "clock" => {
                let clock = step["millis"].as_i64().ok_or("clock")?;
                first_clock.get_or_insert(clock);
                last_clock = Some(clock);
            }
            _ => {}
        }
    }
    let latency_ms = u64::try_from(last_clock.unwrap_or_default() - first_clock.unwrap_or_default())
        .map_err(|_| "clock regressed")?;
    let output_sha256 = format!("{:x}", Sha256::digest(&output));
    let expected_outcome = journey["expected"]["outcome"].as_str().ok_or("expected outcome")?;
    let expected_digest = journey["expected"]["output_digest"].as_str().ok_or("expected digest")?;
    let actual_outcome = match outcome {
        CandidateCanaryOutcome::Completed => "completed",
        CandidateCanaryOutcome::ToolUse => "tool_use",
        CandidateCanaryOutcome::Rejected => "rejected",
        CandidateCanaryOutcome::Failed => "failed",
    };
    let baseline_tokens = journey["baseline_tokens"].as_u64().ok_or("baseline tokens")?;
    let baseline_latency = journey["baseline_latency_ms"].as_u64().ok_or("baseline latency")?;
    let baseline_operations = journey["baseline_operations"].as_u64().ok_or("baseline operations")?;
    let verdict = if actual_outcome != expected_outcome || output_sha256 != expected_digest
        || tokens > baseline_tokens || latency_ms > baseline_latency || operations > baseline_operations
    {
        CandidateCanaryVerdict::Regressed
    } else if tokens < baseline_tokens || latency_ms < baseline_latency || operations < baseline_operations {
        CandidateCanaryVerdict::Improved
    } else {
        CandidateCanaryVerdict::Equivalent
    };
    Ok(CandidateCanaryMeasurement {
        journey_id: journey["id"].as_str().ok_or("journey id")?.into(),
        outcome,
        output_sha256,
        tokens,
        latency_ms,
        operations,
        verdict,
    })
}

fn usage(value: &Value) -> u64 {
    value["input_tokens"].as_u64().unwrap_or_default()
        .saturating_add(value["output_tokens"].as_u64().unwrap_or_default())
}

fn main() {
    if matches!(std::env::args().nth(1).as_deref(), Some("--version" | "-V")) {
        println!("{} {}", env!("CARGO_BIN_NAME"), env!("CARGO_PKG_VERSION"));
        return;
    }
    if matches!(std::env::args().nth(1).as_deref(), Some("--build-info")) {
        let report = keith_build_info::worker_report();
        match serde_json::to_string_pretty(&report) {
            Ok(json) => println!("{json}"),
            Err(error) => {
                eprintln!("{error}");
                std::process::exit(1);
            }
        }
        return;
    }
    if matches!(std::env::args().nth(1).as_deref(), Some("--harness-task")) {
        let value = std::env::args().nth(2).unwrap_or_default();
        println!("{}", routing::route(&value));
        if !routing::claims_success(&value) {
            std::process::exit(1);
        }
        return;
    }
    if let Err(error) = keith_worker_runtime::run_from_environment_with_runtime(|_| {
        Ok(Box::new(ReplayRuntime))
    }) {
        eprintln!("{error}");
        std::process::exit(1);
    }
}
"#;
