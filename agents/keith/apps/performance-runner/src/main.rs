#![forbid(unsafe_code)]

mod native;
mod process;
mod report;
mod ui;

use std::collections::{BTreeMap, BTreeSet};
use std::env;
use std::fs;
use std::io::{Read, Write};
use std::net::{SocketAddr, TcpStream};
use std::path::{Path, PathBuf};
use std::process::{Child, Command, Stdio};
use std::thread;
use std::time::{Duration, Instant};

use keith_agent_types::{SessionId, UtcTimestamp};
use keith_protocol::{
    AttachSession, ChildWorkspaceMode, ClientCommand, CreateChild, CreateGoal, CreateSchedule,
    EventAcknowledgement, ExportFormat, ExportRequest, GoalLimits, MemoryQuery, ResponsePayload,
    ResumeCursor, ScheduleExpression, SessionFilter, SessionSnapshot,
};
use keith_worker_runtime::{read_registration, registration_path};
use serde::{Deserialize, Serialize};

use native::NativeClient;
use process::{
    ManagedProcess, ResourceSample, ResourceSummary, descendant_pids, sample_process,
    summarize_resources, terminate, worker_pids,
};
use report::{LatencySummary, Measurements};

const QUALIFICATION_MINIMUM_SECONDS: u64 = 2 * 60 * 60;
const PROCESS_RESIDENT_LIMIT_BYTES: u64 = 512 * 1_024 * 1_024;
const PROCESS_GROWTH_LIMIT_BYTES: i64 = 64 * 1_024 * 1_024;
const INTERNAL_P95_LIMIT_MICROS: u64 = 500_000;
const RETRIEVAL_REBUILD_P95_LIMIT_MICROS: u64 = 1_000_000;
const LIFECYCLE_P95_LIMIT_MICROS: u64 = 5_000_000;
const UI_P99_LIMIT_MICROS: u64 = 16_667;
const BROWSER_INTERACTION_P95_LIMIT_MICROS: u64 = 5_000_000;
const BROWSER_ROUTE_P99_LIMIT_MICROS: u64 = 100_000;
const MODEL_STREAM_DEADLINE_MICROS: u64 = 180_000_000;
const STEADY_STATE_MEASUREMENTS: &[&str] = &[
    "client_attach_snapshot",
    "client_event_acknowledgement",
    "daemon_list_profiles",
    "daemon_list_sessions",
    "delivery_outbox_claim",
    "event_replay_after_reconnect",
    "session_export",
    "session_resume",
];
const LIFECYCLE_MEASUREMENTS: &[&str] = &[
    "browser_runner_external_navigation",
    "client_attach_cold_snapshot",
    "daemon_readiness",
    "daemon_sigkill_recovery",
    "event_flood_goal_create_baseline",
    "event_flood_goal_create_slow_viewer",
    "kernel_runner_python",
    "recursive_child_create",
    "recursive_grandchild_create",
    "scheduler_create",
    "tool_runner_output_flood",
    "web_readiness",
    "worker_sigkill_recovery",
    "worker_start_and_session_create",
];

#[derive(Clone, Debug)]
struct Arguments {
    agentd: PathBuf,
    agent_worker: PathBuf,
    agent_web: PathBuf,
    tool_runner: PathBuf,
    kernel_runner: PathBuf,
    browser_runner: PathBuf,
    browser_script: PathBuf,
    playwright_module: PathBuf,
    chromium_executable: PathBuf,
    data_root: PathBuf,
    workspace: PathBuf,
    asset_root: PathBuf,
    report: PathBuf,
    web_bind: SocketAddr,
    provider_base_urls: Vec<String>,
    login_secret_env: String,
    openai_key_env: String,
    duration: Duration,
    sample_interval: Duration,
    workers: usize,
    children_per_session: usize,
    event_burst: usize,
    ui_iterations: usize,
    qualify: bool,
}

impl Arguments {
    fn parse() -> Result<Self, String> {
        let values = env::args().skip(1).collect::<Vec<_>>();
        if values
            .iter()
            .any(|value| matches!(value.as_str(), "--version" | "-V"))
        {
            println!("{} {}", env!("CARGO_BIN_NAME"), env!("CARGO_PKG_VERSION"));
            std::process::exit(0);
        }
        let duration_seconds = optional_value(&values, "--duration-seconds")
            .unwrap_or_else(|| "30".into())
            .parse::<u64>()
            .map_err(|_| "--duration-seconds must be an integer".to_owned())?;
        let qualify = values.iter().any(|value| value == "--qualify");
        if qualify && duration_seconds < QUALIFICATION_MINIMUM_SECONDS {
            return Err(format!(
                "qualification mode requires at least {QUALIFICATION_MINIMUM_SECONDS} real seconds"
            ));
        }
        Ok(Self {
            agentd: required_path(&values, "--agentd")?,
            agent_worker: required_path(&values, "--agent-worker")?,
            agent_web: required_path(&values, "--agent-web")?,
            tool_runner: required_path(&values, "--tool-runner")?,
            kernel_runner: required_path(&values, "--kernel-runner")?,
            browser_runner: required_path(&values, "--browser-runner")?,
            browser_script: required_path(&values, "--browser-script")?,
            playwright_module: required_path(&values, "--playwright-module")?,
            chromium_executable: required_path(&values, "--chromium-executable")?,
            data_root: required_path(&values, "--data-root")?,
            workspace: required_path(&values, "--workspace")?,
            asset_root: required_path(&values, "--asset-root")?,
            report: required_path(&values, "--report")?,
            web_bind: optional_value(&values, "--web-bind")
                .unwrap_or_else(|| "127.0.0.1:17341".into())
                .parse()
                .map_err(|_| "--web-bind must be a socket address".to_owned())?,
            provider_base_urls: repeated_values(&values, "--provider-base-url")?,
            login_secret_env: optional_value(&values, "--login-secret-env")
                .unwrap_or_else(|| "KEITH_PERFORMANCE_LOGIN_SECRET".into()),
            openai_key_env: optional_value(&values, "--openai-key-env")
                .unwrap_or_else(|| "KEITH_PERFORMANCE_OPENAI_KEY".into()),
            duration: Duration::from_secs(duration_seconds),
            sample_interval: Duration::from_millis(
                optional_value(&values, "--sample-millis")
                    .unwrap_or_else(|| "1000".into())
                    .parse()
                    .map_err(|_| "--sample-millis must be an integer".to_owned())?,
            ),
            workers: bounded_usize(&values, "--workers", 4, 1, 32)?,
            children_per_session: bounded_usize(&values, "--children-per-session", 2, 0, 16)?,
            event_burst: bounded_usize(&values, "--event-burst", 128, 1, 4_096)?,
            ui_iterations: bounded_usize(&values, "--ui-iterations", 200, 10, 10_000)?,
            qualify,
        })
    }
}

#[derive(Clone, Debug, Serialize)]
struct HostReport {
    operating_system: String,
    architecture: String,
    logical_cpus: usize,
    cpu_model: Option<String>,
    total_memory_bytes: Option<u64>,
    rust_version: Option<String>,
}

#[derive(Clone, Debug, Serialize)]
struct WorkloadReport {
    requested_duration_seconds: u64,
    actual_duration_millis: u64,
    worker_roots: usize,
    children_per_session: usize,
    recursive_depth: usize,
    event_burst_per_phase: usize,
    ui_history_messages: usize,
    ui_iterations: usize,
    browser_goal_iterations: usize,
    resource_sample_interval_millis: u64,
    provider_probe_concurrency: usize,
}

#[derive(Clone, Debug, Serialize)]
struct CheckReport {
    passed: bool,
    detail: String,
}

#[derive(Clone, Debug, Serialize)]
pub struct PerformanceReport {
    schema: String,
    mode: String,
    started_at: UtcTimestamp,
    finished_at: UtcTimestamp,
    host: HostReport,
    workload: WorkloadReport,
    latencies: BTreeMap<String, LatencySummary>,
    resources: BTreeMap<String, ResourceSummary>,
    checks: BTreeMap<String, CheckReport>,
    failures: Vec<String>,
    exclusions: Vec<String>,
    raw_latencies_micros: BTreeMap<String, Vec<u64>>,
    raw_resource_samples: Vec<ResourceSample>,
}

struct RuntimeProcesses {
    daemon: ManagedProcess,
    web: ManagedProcess,
}

#[derive(Debug, Deserialize)]
struct BrowserMeasurements {
    route_switch_micros: Vec<u64>,
    goal_event_to_render_micros: Vec<u64>,
    initial_children: usize,
    final_goals: usize,
    websocket_sequence_advanced: bool,
    final_reconnected: bool,
}

struct ActiveBrowser {
    child: Child,
    result_path: PathBuf,
    stderr_path: PathBuf,
    finished: bool,
}

fn main() {
    if let Err(error) = run() {
        eprintln!("{error}");
        std::process::exit(1);
    }
}

/// Runs the complete process, protocol, runner, UI, and resource qualification workload.
///
/// # Errors
///
/// Returns an error when configuration is invalid or any required runtime path fails.
#[allow(clippy::too_many_lines)]
pub fn run_qualification() -> Result<(), String> {
    let arguments = Arguments::parse()?;
    validate_paths_and_environment(&arguments)?;
    fs::create_dir_all(&arguments.data_root).map_err(|error| error.to_string())?;
    fs::create_dir_all(&arguments.workspace).map_err(|error| error.to_string())?;
    if let Some(parent) = arguments.report.parent() {
        fs::create_dir_all(parent).map_err(|error| error.to_string())?;
    }
    let started_at = UtcTimestamp::now().map_err(|error| error.to_string())?;
    let started = Instant::now();
    let socket = arguments.data_root.join("agentd.sock");
    let mut measurements = Measurements::default();
    let mut failures = Vec::new();
    let mut resources = Vec::new();
    let mut processes = start_runtime(&arguments, &socket, &mut measurements)?;

    let mut client = NativeClient::connect(&socket, Duration::from_secs(180))?;
    let profile = list_profile(&mut client, &mut measurements)?;
    let mut snapshots = create_roots(&mut client, &profile, arguments.workers, &mut measurements)?;
    populate_runtime(
        &socket,
        &mut client,
        &profile,
        &snapshots,
        &arguments,
        &mut measurements,
    )
    .map_err(|error| format!("runtime population failed: {error}"))?;
    verify_recursive_snapshots(&socket, &snapshots, arguments.children_per_session)
        .map_err(|error| format!("recursive snapshot verification failed: {error}"))?;
    replay_and_slow_viewer(
        &socket,
        &mut client,
        &snapshots,
        arguments.event_burst,
        &mut measurements,
    )
    .map_err(|error| format!("replay and slow-viewer workload failed: {error}"))?;
    measurements.extend(ui::benchmark(&snapshots[0], arguments.ui_iterations)?);
    let mut browser = start_browser_ui(
        &arguments,
        &snapshots[0].session.session_id,
        started,
        &mut measurements,
        &mut resources,
    )?;

    if let Err(error) = concurrent_model_probe(
        arguments.web_bind,
        &arguments.openai_key_env,
        2,
        &mut measurements,
    ) {
        failures.push(format!("initial model stream probe failed: {error}"));
    }

    exercise_bounded_runners(
        &arguments,
        started,
        &mut measurements,
        &mut resources,
        &mut failures,
    )?;
    sample_runtime(started, &processes, &arguments.data_root, &mut resources);

    recover_worker(
        &arguments.data_root,
        &socket,
        &snapshots[0],
        &mut measurements,
    )?;
    snapshots[0] = resume_snapshot(
        &socket,
        &snapshots[0].session.session_id,
        "post_worker_recovery_resume",
        &mut measurements,
    )?;
    restart_daemon(
        &arguments,
        &socket,
        &mut processes.daemon,
        &snapshots,
        &mut measurements,
    )?;
    for _ in 0..10 {
        measure_command(
            &socket,
            None,
            ClientCommand::ListSessions(SessionFilter::default()),
            "daemon_list_sessions_warmup",
            &mut measurements,
        )?;
    }

    let loop_started = Instant::now();
    let mut iteration = 0_usize;
    while loop_started.elapsed() < arguments.duration {
        sample_runtime(started, &processes, &arguments.data_root, &mut resources);
        browser.sample(started, &mut resources);
        let session = &snapshots[iteration % snapshots.len()];
        measure_command(
            &socket,
            None,
            ClientCommand::ListSessions(SessionFilter::default()),
            "daemon_list_sessions",
            &mut measurements,
        )?;
        measure_command(
            &socket,
            None,
            ClientCommand::QueryMemory(MemoryQuery {
                profile_id: profile.id.clone(),
                query: "performance continuity".into(),
                limit: 20,
            }),
            "retrieval_rebuild_and_query",
            &mut measurements,
        )?;
        measure_command(
            &socket,
            None,
            ClientCommand::ClaimDelivery {
                channel: "performance".into(),
                external_account: "performance".into(),
            },
            "delivery_outbox_claim",
            &mut measurements,
        )?;
        if iteration.is_multiple_of(10) {
            let _ = resume_snapshot(
                &socket,
                &session.session.session_id,
                "session_resume",
                &mut measurements,
            )?;
        }
        if iteration > 0
            && iteration.is_multiple_of(300)
            && let Err(error) = concurrent_model_probe(
                arguments.web_bind,
                &arguments.openai_key_env,
                2,
                &mut measurements,
            )
        {
            failures.push(format!("periodic model stream probe failed: {error}"));
        }
        if iteration > 0 && iteration.is_multiple_of(300) {
            exercise_bounded_runners(
                &arguments,
                started,
                &mut measurements,
                &mut resources,
                &mut failures,
            )?;
        }
        iteration = iteration.saturating_add(1);
        let remaining = arguments.duration.saturating_sub(loop_started.elapsed());
        thread::sleep(arguments.sample_interval.min(remaining));
    }
    sample_runtime(started, &processes, &arguments.data_root, &mut resources);
    browser.sample(started, &mut resources);
    if let Err(error) = concurrent_model_probe(
        arguments.web_bind,
        &arguments.openai_key_env,
        2,
        &mut measurements,
    ) {
        failures.push(format!("final model stream probe failed: {error}"));
    }
    if let Err(error) = browser.finish() {
        failures.push(format!("browser soak completion failed: {error}"));
    }

    let finished_at = UtcTimestamp::now().map_err(|error| error.to_string())?;
    let latency_summaries = measurements.summaries();
    let raw_latencies_micros = measurements.raw();
    let resource_summaries = summarize_resources(&resources);
    let checks = qualification_checks(&latency_summaries, &resource_summaries, &failures);
    let passed = checks.values().all(|check| check.passed);
    let report = PerformanceReport {
        schema: "keith-performance-report-v1".into(),
        mode: if arguments.qualify { "qualification" } else { "smoke" }.into(),
        started_at,
        finished_at,
        host: host_report(),
        workload: WorkloadReport {
            requested_duration_seconds: arguments.duration.as_secs(),
            actual_duration_millis: u64::try_from(started.elapsed().as_millis())
                .unwrap_or(u64::MAX),
            worker_roots: snapshots.len(),
            children_per_session: arguments.children_per_session,
            recursive_depth: if arguments.children_per_session > 0 {
                2
            } else {
                1
            },
            event_burst_per_phase: arguments.event_burst,
            ui_history_messages: 10_000,
            ui_iterations: arguments.ui_iterations,
            browser_goal_iterations: arguments.event_burst.min(64),
            resource_sample_interval_millis: u64::try_from(
                arguments.sample_interval.as_millis(),
            )
            .unwrap_or(u64::MAX),
            provider_probe_concurrency: 2,
        },
        latencies: latency_summaries,
        resources: resource_summaries,
        checks,
        failures,
        exclusions: vec![
            "external model latency is reported but excluded from the 500 ms internal-operation budget".into(),
            "browser navigation includes public-network latency and is reported separately".into(),
            "no production messaging account is mutated; channel load covers the native outbox claim boundary only".into(),
            "desktop packaging lifecycle belongs to task 13.3 and final composed desktop soak belongs to task 14.6".into(),
        ],
        raw_latencies_micros,
        raw_resource_samples: resources,
    };
    let bytes = serde_json::to_vec_pretty(&report).map_err(|error| error.to_string())?;
    fs::write(&arguments.report, bytes).map_err(|error| error.to_string())?;
    processes.web.terminate()?;
    processes.daemon.terminate()?;
    println!(
        "performance {} written to {}",
        if passed { "passed" } else { "failed" },
        arguments.report.display()
    );
    if arguments.qualify && !passed {
        return Err("performance qualification checks failed".into());
    }
    Ok(())
}

fn run() -> Result<(), String> {
    run_qualification()
}

fn validate_paths_and_environment(arguments: &Arguments) -> Result<(), String> {
    for (label, path) in [
        ("agentd", &arguments.agentd),
        ("agent-worker", &arguments.agent_worker),
        ("agent-web", &arguments.agent_web),
        ("tool-runner", &arguments.tool_runner),
        ("kernel-runner", &arguments.kernel_runner),
        ("browser-runner", &arguments.browser_runner),
        ("browser script", &arguments.browser_script),
        ("Chromium", &arguments.chromium_executable),
    ] {
        if !path.is_file() {
            return Err(format!("{label} is missing: {}", path.display()));
        }
    }
    if !arguments.asset_root.is_dir() {
        return Err(format!(
            "web asset root is missing: {}",
            arguments.asset_root.display()
        ));
    }
    if !arguments.playwright_module.is_dir() {
        return Err(format!(
            "Playwright module is missing: {}",
            arguments.playwright_module.display()
        ));
    }
    for environment in [&arguments.login_secret_env, &arguments.openai_key_env] {
        if env::var_os(environment).is_none() {
            return Err(format!(
                "required secret environment {environment} is unset"
            ));
        }
    }
    if arguments.sample_interval.is_zero() {
        return Err("resource sample interval must be non-zero".into());
    }
    Ok(())
}

fn start_runtime(
    arguments: &Arguments,
    socket: &Path,
    measurements: &mut Measurements,
) -> Result<RuntimeProcesses, String> {
    let logs = arguments.data_root.join("performance-logs");
    fs::create_dir_all(&logs).map_err(|error| error.to_string())?;
    let mut daemon_command = Command::new(&arguments.agentd);
    daemon_command
        .arg("--data-root")
        .arg(&arguments.data_root)
        .arg("--socket")
        .arg(socket)
        .arg("--worker-executable")
        .arg(&arguments.agent_worker)
        .arg("--credential-root")
        .arg(arguments.data_root.join("credentials"))
        .arg("--workspace-root")
        .arg(&arguments.workspace)
        .arg("--idle-seconds")
        .arg("86400");
    for provider in &arguments.provider_base_urls {
        daemon_command.arg("--provider-base-url").arg(provider);
    }
    let ready_started = Instant::now();
    let daemon = ManagedProcess::spawn("agentd", &mut daemon_command, &logs.join("agentd.log"))?;
    wait_for_socket(socket, Duration::from_secs(30))?;
    measurements.record(
        "daemon_readiness",
        u64::try_from(ready_started.elapsed().as_micros()).unwrap_or(u64::MAX),
    );

    let mut web_command = Command::new(&arguments.agent_web);
    web_command
        .arg("--bind")
        .arg(arguments.web_bind.to_string())
        .arg("--origin")
        .arg(format!("http://{}", arguments.web_bind))
        .arg("--socket")
        .arg(socket)
        .arg("--asset-root")
        .arg(&arguments.asset_root)
        .arg("--credential-root")
        .arg(arguments.data_root.join("credentials"))
        .arg("--login-secret-env")
        .arg(&arguments.login_secret_env)
        .arg("--openai-api-key-env")
        .arg(&arguments.openai_key_env);
    let web_started = Instant::now();
    let web = ManagedProcess::spawn("agent-web", &mut web_command, &logs.join("agent-web.log"))?;
    wait_for_tcp(arguments.web_bind, Duration::from_secs(30))?;
    measurements.record(
        "web_readiness",
        u64::try_from(web_started.elapsed().as_micros()).unwrap_or(u64::MAX),
    );
    Ok(RuntimeProcesses { daemon, web })
}

fn list_profile(
    client: &mut NativeClient,
    measurements: &mut Measurements,
) -> Result<keith_protocol::ProfileSummary, String> {
    let started = Instant::now();
    let result = client.command(None, ClientCommand::ListProfiles)?;
    measurements.record(
        "daemon_list_profiles",
        u64::try_from(started.elapsed().as_micros()).unwrap_or(u64::MAX),
    );
    match result.result {
        keith_protocol::CommandResult::Data(payload) => match *payload {
            ResponsePayload::Profiles(profiles) => profiles
                .into_iter()
                .find(|profile| profile.enabled)
                .ok_or_else(|| "no enabled profile was returned".into()),
            _ => Err("ListProfiles returned the wrong payload".into()),
        },
        result => Err(format!("ListProfiles failed: {result:?}")),
    }
}

fn create_roots(
    client: &mut NativeClient,
    profile: &keith_protocol::ProfileSummary,
    workers: usize,
    measurements: &mut Measurements,
) -> Result<Vec<SessionSnapshot>, String> {
    let mut snapshots = Vec::with_capacity(workers);
    for index in 0..workers {
        let started = Instant::now();
        let result = client.command(
            None,
            ClientCommand::CreateSession(keith_protocol::CreateSession {
                profile_id: profile.id.clone(),
                workspace_id: profile.workspace_id.clone(),
                title: Some(format!("Performance root {index}")),
            }),
        )?;
        measurements.record(
            "worker_start_and_session_create",
            u64::try_from(started.elapsed().as_micros()).unwrap_or(u64::MAX),
        );
        snapshots.push(snapshot_payload(result.result, "CreateSession")?);
    }
    Ok(snapshots)
}

fn populate_runtime(
    socket: &Path,
    client: &mut NativeClient,
    profile: &keith_protocol::ProfileSummary,
    snapshots: &[SessionSnapshot],
    arguments: &Arguments,
    measurements: &mut Measurements,
) -> Result<(), String> {
    for snapshot in snapshots {
        let session_id = snapshot.session.session_id.clone();
        let started = Instant::now();
        let attached = client.command(
            Some(session_id.clone()),
            ClientCommand::AttachSession(AttachSession {
                session_id: session_id.clone(),
                resume: None,
            }),
        )?;
        measurements.record(
            "client_attach_cold_snapshot",
            u64::try_from(started.elapsed().as_micros()).unwrap_or(u64::MAX),
        );
        let attached = snapshot_payload(attached.result, "AttachSession")?;
        let started = Instant::now();
        accepted(
            client
                .command(
                    Some(session_id.clone()),
                    ClientCommand::AcknowledgeEvents(EventAcknowledgement {
                        root_tree_id: attached.session.root_tree_id.clone(),
                        generation: attached.generation,
                        through_sequence: attached.through_sequence,
                    }),
                )?
                .result,
            "AcknowledgeEvents",
        )?;
        measurements.record(
            "client_event_acknowledgement",
            u64::try_from(started.elapsed().as_micros()).unwrap_or(u64::MAX),
        );
        create_recursive_children(
            socket,
            &session_id,
            arguments.children_per_session,
            measurements,
        )?;
        let schedule_at = UtcTimestamp::from_unix_millis(
            UtcTimestamp::now()
                .map_err(|error| error.to_string())?
                .unix_millis()
                .checked_add(24 * 60 * 60 * 1_000)
                .ok_or_else(|| "performance schedule timestamp overflowed".to_owned())?,
        );
        measure_command(
            socket,
            Some(session_id.clone()),
            ClientCommand::CreateSchedule(CreateSchedule {
                profile_id: profile.id.clone(),
                session_id: Some(session_id.clone()),
                expression: ScheduleExpression::Once(schedule_at),
                time_zone: "UTC".into(),
                prompt: "performance schedule remains dormant during the soak".into(),
                reply_route: None,
            }),
            "scheduler_create",
            measurements,
        )?;
        measure_command(
            socket,
            Some(session_id.clone()),
            ClientCommand::Export(ExportRequest {
                session_id,
                format: ExportFormat::PortableBundle,
                include_artifacts: true,
            }),
            "session_export",
            measurements,
        )?;
    }
    for index in 0..32 {
        let snapshot = &snapshots[index % snapshots.len()];
        let started = Instant::now();
        let result = client.command(
            Some(snapshot.session.session_id.clone()),
            ClientCommand::AttachSession(AttachSession {
                session_id: snapshot.session.session_id.clone(),
                resume: None,
            }),
        )?;
        let _ = snapshot_payload(result.result, "steady AttachSession")?;
        measurements.record(
            "client_attach_snapshot",
            u64::try_from(started.elapsed().as_micros()).unwrap_or(u64::MAX),
        );
    }
    Ok(())
}

fn create_recursive_children(
    socket: &Path,
    root_session_id: &SessionId,
    child_count: usize,
    measurements: &mut Measurements,
) -> Result<(), String> {
    let mut first_child_session = None;
    for child in 0..child_count {
        let result = measure_command(
            socket,
            Some(root_session_id.clone()),
            ClientCommand::CreateChild(CreateChild {
                parent_session_id: root_session_id.clone(),
                objective: format!(
                    "Reply exactly PERF_CHILD_{child}_READY without calling tools or creating a plan"
                ),
                workspace_mode: ChildWorkspaceMode::ReadOnlyParent,
                limits: GoalLimits {
                    max_turns: Some(1),
                    max_tokens: Some(2_000),
                    deadline: None,
                },
            }),
            "recursive_child_create",
            measurements,
        )?;
        if first_child_session.is_none()
            && let keith_protocol::CommandResult::Data(payload) = result
            && let ResponsePayload::Child(child) = *payload
        {
            first_child_session = Some(child.session_id);
        }
    }
    let Some(child_session_id) = first_child_session else {
        return Ok(());
    };
    let result = measure_command(
        socket,
        Some(root_session_id.clone()),
        ClientCommand::CreateChild(CreateChild {
            parent_session_id: child_session_id,
            objective:
                "Reply exactly PERF_GRANDCHILD_READY without calling tools or creating a plan"
                    .into(),
            workspace_mode: ChildWorkspaceMode::ReadOnlyParent,
            limits: GoalLimits {
                max_turns: Some(1),
                max_tokens: Some(2_000),
                deadline: None,
            },
        }),
        "recursive_grandchild_create",
        measurements,
    )?;
    if !matches!(result, keith_protocol::CommandResult::Data(payload) if matches!(*payload, ResponsePayload::Child(_)))
    {
        return Err("recursive grandchild creation returned the wrong payload".into());
    }
    Ok(())
}

fn replay_and_slow_viewer(
    socket: &Path,
    client: &mut NativeClient,
    snapshots: &[SessionSnapshot],
    event_burst: usize,
    measurements: &mut Measurements,
) -> Result<(), String> {
    let first = &snapshots[0];
    measure_goal_burst(
        client,
        snapshots,
        event_burst,
        "baseline",
        "event_flood_goal_create_baseline",
        measurements,
    )?;
    let mut slow = NativeClient::connect(socket, Duration::from_secs(180))?;
    let _ = slow.command(
        Some(first.session.session_id.clone()),
        ClientCommand::AttachSession(AttachSession {
            session_id: first.session.session_id.clone(),
            resume: None,
        }),
    )?;
    measure_goal_burst(
        client,
        snapshots,
        event_burst,
        "slow-viewer",
        "event_flood_goal_create_slow_viewer",
        measurements,
    )?;
    let mut replay = NativeClient::connect(socket, Duration::from_secs(180))?;
    let started = Instant::now();
    let recovery = replay.command(
        Some(first.session.session_id.clone()),
        ClientCommand::AttachSession(AttachSession {
            session_id: first.session.session_id.clone(),
            resume: Some(ResumeCursor {
                root_tree_id: first.session.root_tree_id.clone(),
                generation: first.generation,
                last_sequence: first.through_sequence,
            }),
        }),
    )?;
    measurements.record(
        "event_replay_after_reconnect",
        u64::try_from(started.elapsed().as_micros()).unwrap_or(u64::MAX),
    );
    if recovery.events == 0 && !matches!(recovery.result, keith_protocol::CommandResult::Data(_)) {
        return Err("replay returned neither deltas nor an authoritative snapshot".into());
    }
    drop(slow);
    Ok(())
}

fn verify_recursive_snapshots(
    socket: &Path,
    snapshots: &[SessionSnapshot],
    expected_children: usize,
) -> Result<(), String> {
    for snapshot in snapshots {
        let mut client = NativeClient::connect(socket, Duration::from_secs(180))?;
        let result = client.command(
            Some(snapshot.session.session_id.clone()),
            ClientCommand::ResumeSession {
                session_id: snapshot.session.session_id.clone(),
            },
        )?;
        let current = snapshot_payload(result.result, "recursive ResumeSession")?;
        if current.children.len() != expected_children {
            return Err(format!(
                "session {} exposed {} children after creating {expected_children}",
                current.session.session_id,
                current.children.len()
            ));
        }
    }
    Ok(())
}

fn measure_goal_burst(
    client: &mut NativeClient,
    snapshots: &[SessionSnapshot],
    event_burst: usize,
    phase: &str,
    measurement: &str,
    measurements: &mut Measurements,
) -> Result<(), String> {
    for index in 0..event_burst {
        let session = &snapshots[index % snapshots.len()].session.session_id;
        let started = Instant::now();
        let result = client.command(
            Some(session.clone()),
            ClientCommand::CreateGoal(CreateGoal {
                session_id: session.clone(),
                objective: format!("event-flood-{phase}-goal-{index}"),
                limits: GoalLimits {
                    max_turns: Some(1),
                    max_tokens: Some(1_000),
                    deadline: None,
                },
            }),
        )?;
        data_result(result.result, "CreateGoal")?;
        measurements.record(
            measurement,
            u64::try_from(started.elapsed().as_micros()).unwrap_or(u64::MAX),
        );
    }
    Ok(())
}

fn measure_command(
    socket: &Path,
    session: Option<SessionId>,
    command: ClientCommand,
    name: &str,
    measurements: &mut Measurements,
) -> Result<keith_protocol::CommandResult, String> {
    let mut client = NativeClient::connect(socket, Duration::from_secs(180))?;
    let started = Instant::now();
    let result = client.command(session, command)?.result;
    measurements.record(
        name,
        u64::try_from(started.elapsed().as_micros()).unwrap_or(u64::MAX),
    );
    if let keith_protocol::CommandResult::Rejected(error) = &result {
        return Err(format!("{name} was rejected: {}", error.error));
    }
    Ok(result)
}

fn recover_worker(
    data_root: &Path,
    socket: &Path,
    snapshot: &SessionSnapshot,
    measurements: &mut Measurements,
) -> Result<(), String> {
    let registration = read_registration(&registration_path(
        &data_root.join("runtime"),
        &snapshot.session.root_tree_id,
    ))
    .map_err(|error| format!("worker registration was unavailable before recovery: {error}"))?;
    let pid = registration.pid;
    terminate(pid, true)?;
    let started = Instant::now();
    let deadline = Instant::now() + Duration::from_secs(30);
    loop {
        if resume_snapshot(
            socket,
            &snapshot.session.session_id,
            "worker_recovery_resume_probe",
            measurements,
        )
        .is_ok()
        {
            measurements.record(
                "worker_sigkill_recovery",
                u64::try_from(started.elapsed().as_micros()).unwrap_or(u64::MAX),
            );
            return Ok(());
        }
        if Instant::now() >= deadline {
            return Err("worker did not recover within 30 seconds".into());
        }
        thread::sleep(Duration::from_millis(100));
    }
}

fn restart_daemon(
    arguments: &Arguments,
    socket: &Path,
    daemon: &mut ManagedProcess,
    snapshots: &[SessionSnapshot],
    measurements: &mut Measurements,
) -> Result<(), String> {
    terminate(daemon.pid(), true)?;
    daemon.child.wait().map_err(|error| error.to_string())?;
    let mut command = Command::new(&arguments.agentd);
    command
        .arg("--data-root")
        .arg(&arguments.data_root)
        .arg("--socket")
        .arg(socket)
        .arg("--worker-executable")
        .arg(&arguments.agent_worker)
        .arg("--credential-root")
        .arg(arguments.data_root.join("credentials"))
        .arg("--workspace-root")
        .arg(&arguments.workspace)
        .arg("--idle-seconds")
        .arg("86400");
    for provider in &arguments.provider_base_urls {
        command.arg("--provider-base-url").arg(provider);
    }
    let started = Instant::now();
    *daemon = ManagedProcess::spawn(
        "agentd",
        &mut command,
        &arguments
            .data_root
            .join("performance-logs/agentd-restarted.log"),
    )?;
    wait_for_socket(socket, Duration::from_secs(30))?;
    for snapshot in snapshots {
        let _ = resume_snapshot(
            socket,
            &snapshot.session.session_id,
            "daemon_recovery_resume_probe",
            measurements,
        )?;
    }
    measurements.record(
        "daemon_sigkill_recovery",
        u64::try_from(started.elapsed().as_micros()).unwrap_or(u64::MAX),
    );
    Ok(())
}

fn resume_snapshot(
    socket: &Path,
    session_id: &SessionId,
    measurement: &str,
    measurements: &mut Measurements,
) -> Result<SessionSnapshot, String> {
    let result = measure_command(
        socket,
        Some(session_id.clone()),
        ClientCommand::ResumeSession {
            session_id: session_id.clone(),
        },
        measurement,
        measurements,
    )?;
    snapshot_payload(result, "ResumeSession")
}

fn sample_runtime(
    started: Instant,
    processes: &RuntimeProcesses,
    data_root: &Path,
    samples: &mut Vec<ResourceSample>,
) {
    samples.push(sample_process(started, "daemon", processes.daemon.pid()));
    samples.push(sample_process(started, "web", processes.web.pid()));
    for (component, pid) in worker_pids(data_root) {
        samples.push(sample_process(started, component, pid));
    }
}

fn exercise_bounded_runners(
    arguments: &Arguments,
    suite_started: Instant,
    measurements: &mut Measurements,
    resources: &mut Vec<ResourceSample>,
    failures: &mut Vec<String>,
) -> Result<(), String> {
    let python = Path::new("/usr/bin/python3");
    let tool_arguments = [
        "--workspace".into(),
        arguments.workspace.as_os_str().to_owned(),
        "--program".into(),
        python.as_os_str().to_owned(),
        "--untrusted".into(),
        "--".into(),
        "-c".into(),
        "print('x' * 1048576)".into(),
    ];
    exercise_runner(
        "tool_runner_output_flood",
        &arguments.tool_runner,
        &tool_arguments,
        suite_started,
        measurements,
        resources,
    )?;

    let kernel_arguments = [
        "--workspace".into(),
        arguments.workspace.as_os_str().to_owned(),
        "--state".into(),
        arguments
            .data_root
            .join("performance-kernel.sqlite")
            .into_os_string(),
        "--python".into(),
        python.as_os_str().to_owned(),
        "--code".into(),
        "sum(range(100000))".into(),
    ];
    exercise_runner(
        "kernel_runner_python",
        &arguments.kernel_runner,
        &kernel_arguments,
        suite_started,
        measurements,
        resources,
    )?;

    let browser_arguments = [
        "--url".into(),
        "https://example.com/".into(),
        "--profile".into(),
        "performance".into(),
    ];
    if let Err(error) = exercise_runner(
        "browser_runner_external_navigation",
        &arguments.browser_runner,
        &browser_arguments,
        suite_started,
        measurements,
        resources,
    ) {
        failures.push(error);
    }
    Ok(())
}

fn exercise_runner(
    label: &str,
    executable: &Path,
    arguments: &[std::ffi::OsString],
    suite_started: Instant,
    measurements: &mut Measurements,
    resources: &mut Vec<ResourceSample>,
) -> Result<(), String> {
    let started = Instant::now();
    let mut child = Command::new(executable)
        .args(arguments)
        .stdin(Stdio::null())
        .stdout(Stdio::null())
        .stderr(Stdio::null())
        .spawn()
        .map_err(|error| format!("failed to start {label}: {error}"))?;
    loop {
        resources.push(sample_process(suite_started, label, child.id()));
        if let Some(status) = child.try_wait().map_err(|error| error.to_string())? {
            if !status.success() {
                return Err(format!("{label} failed with {status}"));
            }
            break;
        }
        thread::sleep(Duration::from_millis(5));
    }
    measurements.record(
        label,
        u64::try_from(started.elapsed().as_micros()).unwrap_or(u64::MAX),
    );
    Ok(())
}

fn start_browser_ui(
    arguments: &Arguments,
    root_session_id: &SessionId,
    suite_started: Instant,
    measurements: &mut Measurements,
    resources: &mut Vec<ResourceSample>,
) -> Result<ActiveBrowser, String> {
    let login_secret = env::var(&arguments.login_secret_env)
        .map_err(|_| "browser login secret is not valid UTF-8".to_owned())?;
    let result_path = arguments.data_root.join("performance-browser-result.json");
    let stderr_path = arguments.data_root.join("performance-logs/browser-ui.log");
    let stderr = fs::File::create(&stderr_path).map_err(|error| error.to_string())?;
    let mut child = Command::new("node")
        .arg(&arguments.browser_script)
        .env("KEITH_PLAYWRIGHT_MODULE", &arguments.playwright_module)
        .env("KEITH_WEB_ORIGIN", format!("http://{}", arguments.web_bind))
        .env("KEITH_BROWSER_LOGIN_SECRET", login_secret)
        .env("KEITH_BROWSER_ROOT_SESSION", root_session_id.to_string())
        .env("KEITH_BROWSER_RESULT", &result_path)
        .env("KEITH_CHROMIUM_EXECUTABLE", &arguments.chromium_executable)
        .env(
            "KEITH_BROWSER_ITERATIONS",
            arguments.event_burst.min(64).to_string(),
        )
        .stdin(Stdio::piped())
        .stdout(Stdio::null())
        .stderr(Stdio::from(stderr))
        .spawn()
        .map_err(|error| format!("failed to start browser UI qualification: {error}"))?;
    let node_pid = child.id();
    let mut browser_pids = BTreeSet::new();
    let deadline = Instant::now() + Duration::from_secs(180);
    let result = loop {
        sample_browser_processes(suite_started, node_pid, true, resources);
        browser_pids.extend(descendant_pids(node_pid));
        if let Ok(bytes) = fs::read(&result_path)
            && let Ok(result) = serde_json::from_slice::<BrowserMeasurements>(&bytes)
        {
            break result;
        }
        if let Some(status) = child.try_wait().map_err(|error| error.to_string())? {
            let detail = fs::read_to_string(&stderr_path).unwrap_or_default();
            return Err(format!(
                "browser UI qualification exited with {status}: {}",
                detail.trim()
            ));
        }
        if Instant::now() >= deadline {
            return Err("browser UI qualification did not become ready within 180 seconds".into());
        }
        thread::sleep(Duration::from_millis(10));
    };
    if browser_pids.is_empty() {
        return Err("browser UI qualification did not expose a Chromium process".into());
    }
    if result.route_switch_micros.is_empty()
        || result.goal_event_to_render_micros.is_empty()
        || result.initial_children == 0
        || result.final_goals == 0
        || !result.websocket_sequence_advanced
    {
        return Err("browser UI qualification omitted a required journey measurement".into());
    }
    for micros in result.route_switch_micros {
        measurements.record("browser_route_switch", micros);
    }
    for micros in result.goal_event_to_render_micros {
        measurements.record("browser_goal_event_to_render", micros);
    }
    Ok(ActiveBrowser {
        child,
        result_path,
        stderr_path,
        finished: false,
    })
}

fn sample_browser_processes(
    started: Instant,
    node_pid: u32,
    startup: bool,
    resources: &mut Vec<ResourceSample>,
) {
    let phase = if startup { "startup_" } else { "" };
    resources.push(sample_process(
        started,
        format!("browser_ui_{phase}node"),
        node_pid,
    ));
    for pid in descendant_pids(node_pid) {
        resources.push(sample_process(
            started,
            format!("browser_ui_{phase}chromium:{pid}"),
            pid,
        ));
    }
}

impl ActiveBrowser {
    fn sample(&self, started: Instant, resources: &mut Vec<ResourceSample>) {
        sample_browser_processes(started, self.child.id(), false, resources);
    }

    fn finish(&mut self) -> Result<(), String> {
        if self.finished {
            return Ok(());
        }
        if let Some(mut input) = self.child.stdin.take() {
            input
                .write_all(b"stop\n")
                .map_err(|error| error.to_string())?;
        }
        let deadline = Instant::now() + Duration::from_secs(45);
        loop {
            if let Some(status) = self.child.try_wait().map_err(|error| error.to_string())? {
                self.finished = true;
                if !status.success() {
                    let detail = fs::read_to_string(&self.stderr_path).unwrap_or_default();
                    return Err(format!(
                        "browser UI qualification exited with {status}: {}",
                        detail.trim()
                    ));
                }
                let result: BrowserMeasurements = serde_json::from_slice(
                    &fs::read(&self.result_path).map_err(|error| error.to_string())?,
                )
                .map_err(|error| error.to_string())?;
                return result
                    .final_reconnected
                    .then_some(())
                    .ok_or_else(|| "browser did not reconnect after the soak disruptions".into());
            }
            if Instant::now() >= deadline {
                terminate(self.child.id(), true)?;
                self.child.wait().map_err(|error| error.to_string())?;
                self.finished = true;
                return Err("browser UI qualification did not stop within 45 seconds".into());
            }
            thread::sleep(Duration::from_millis(20));
        }
    }
}

impl Drop for ActiveBrowser {
    fn drop(&mut self) {
        let _ = self.finish();
    }
}

fn concurrent_model_probe(
    address: SocketAddr,
    key_environment: &str,
    concurrency: usize,
    measurements: &mut Measurements,
) -> Result<(), String> {
    let key = env::var(key_environment)
        .map_err(|_| format!("model probe key environment {key_environment} is unavailable"))?;
    let group_started = Instant::now();
    let results = thread::scope(|scope| {
        (0..concurrency)
            .map(|index| {
                let key = &key;
                scope.spawn(move || model_probe(address, key, index))
            })
            .collect::<Vec<_>>()
            .into_iter()
            .map(|handle| {
                handle
                    .join()
                    .map_err(|_| "model probe thread panicked".to_owned())?
            })
            .collect::<Result<Vec<_>, String>>()
    })?;
    measurements.record(
        "model_probe_concurrent_group",
        u64::try_from(group_started.elapsed().as_micros()).unwrap_or(u64::MAX),
    );
    for (first_delta, complete) in results {
        measurements.record("model_stream_first_delta", first_delta);
        measurements.record("model_stream_complete", complete);
    }
    Ok(())
}

fn model_probe(address: SocketAddr, key: &str, index: usize) -> Result<(u64, u64), String> {
    let body = serde_json::to_vec(&serde_json::json!({
        "model": "keith",
        "messages": [{"role": "user", "content": format!("Reply exactly PERF_STREAM_{index}")}],
        "stream": true,
        "stream_options": {"include_usage": true}
    }))
    .map_err(|error| error.to_string())?;
    let mut stream = TcpStream::connect_timeout(&address, Duration::from_secs(5))
        .map_err(|error| error.to_string())?;
    stream
        .set_read_timeout(Some(Duration::from_secs(180)))
        .map_err(|error| error.to_string())?;
    let request = format!(
        "POST /v1/chat/completions HTTP/1.1\r\nHost: {address}\r\nAuthorization: Bearer {key}\r\nContent-Type: application/json\r\nContent-Length: {}\r\nConnection: close\r\nX-Conversation-Id: performance-stream-{index}\r\n\r\n",
        body.len()
    );
    let started = Instant::now();
    stream
        .write_all(request.as_bytes())
        .and_then(|()| stream.write_all(&body))
        .map_err(|error| error.to_string())?;
    let mut response = Vec::new();
    let mut chunk = [0_u8; 8 * 1_024];
    let mut first_delta = None;
    loop {
        let count = stream.read(&mut chunk).map_err(|error| error.to_string())?;
        if count == 0 {
            break;
        }
        response.extend_from_slice(&chunk[..count]);
        if response.len() > 2 * 1_024 * 1_024 {
            return Err("model stream exceeded the performance probe response bound".into());
        }
        if first_delta.is_none()
            && response
                .windows(b"PERF_STREAM_".len())
                .any(|window| window == b"PERF_STREAM_")
        {
            first_delta = Some(u64::try_from(started.elapsed().as_micros()).unwrap_or(u64::MAX));
        }
    }
    let text = String::from_utf8_lossy(&response);
    if !text.starts_with("HTTP/1.1 200") || !text.contains("[DONE]") {
        return Err("model stream did not return HTTP 200 with a terminal marker".into());
    }
    let complete = u64::try_from(started.elapsed().as_micros()).unwrap_or(u64::MAX);
    Ok((first_delta.unwrap_or(complete), complete))
}

#[allow(clippy::too_many_lines)]
fn qualification_checks(
    latencies: &BTreeMap<String, LatencySummary>,
    resources: &BTreeMap<String, ResourceSummary>,
    failures: &[String],
) -> BTreeMap<String, CheckReport> {
    let internal = measurements_within(
        latencies,
        STEADY_STATE_MEASUREMENTS,
        INTERNAL_P95_LIMIT_MICROS,
    );
    let lifecycle = measurements_within(
        latencies,
        LIFECYCLE_MEASUREMENTS,
        LIFECYCLE_P95_LIMIT_MICROS,
    );
    let retrieval_rebuild = measurements_within(
        latencies,
        &["retrieval_rebuild_and_query"],
        RETRIEVAL_REBUILD_P95_LIMIT_MICROS,
    );
    let ui = latencies
        .iter()
        .filter(|(name, _)| name.starts_with("tui_render") || name.starts_with("web_"));
    let ui_responsive = ui
        .clone()
        .all(|(_, summary)| summary.p99_micros <= UI_P99_LIMIT_MICROS)
        && ui.count() >= 2;
    let resident_bounded = resources.iter().all(|(component, summary)| {
        let maximum_bounded = summary
            .maximum_resident_bytes
            .is_some_and(|maximum| maximum <= PROCESS_RESIDENT_LIMIT_BYTES);
        let long_lived = component == "daemon"
            || component == "web"
            || component.starts_with("worker:")
            || (component.starts_with("browser_ui_") && !component.contains("_startup_"));
        maximum_bounded
            && (!long_lived
                || summary
                    .resident_growth_bytes
                    .is_some_and(|growth| growth <= PROCESS_GROWTH_LIMIT_BYTES))
    });
    let browser_responsive = latencies
        .get("browser_route_switch")
        .is_some_and(|summary| summary.p99_micros <= BROWSER_ROUTE_P99_LIMIT_MICROS)
        && latencies
            .get("browser_goal_event_to_render")
            .is_some_and(|summary| summary.p95_micros <= BROWSER_INTERACTION_P95_LIMIT_MICROS);
    let provider_streams = ["model_stream_first_delta", "model_stream_complete"]
        .into_iter()
        .all(|name| {
            latencies.get(name).is_some_and(|summary| {
                summary.samples >= 2 && summary.p95_micros <= MODEL_STREAM_DEADLINE_MICROS
            })
        });
    let baseline = latencies.get("event_flood_goal_create_baseline");
    let slow = latencies.get("event_flood_goal_create_slow_viewer");
    let slow_viewer_isolated = baseline.zip(slow).is_some_and(|(baseline, slow)| {
        let tolerated = baseline
            .p95_micros
            .saturating_mul(5)
            .saturating_div(4)
            .saturating_add(100_000);
        slow.p95_micros <= tolerated
    });
    BTreeMap::from([
        (
            "steady_state_latency_p95".into(),
            CheckReport {
                passed: internal,
                detail: format!(
                    "every declared steady-state operation p95 must be <= {INTERNAL_P95_LIMIT_MICROS} us"
                ),
            },
        ),
        (
            "lifecycle_latency_p95".into(),
            CheckReport {
                passed: lifecycle,
                detail: format!(
                    "every declared lifecycle, recovery, and bounded-runner operation p95 must be <= {LIFECYCLE_P95_LIMIT_MICROS} us"
                ),
            },
        ),
        (
            "retrieval_rebuild_latency_p95".into(),
            CheckReport {
                passed: retrieval_rebuild,
                detail: format!(
                    "workspace retrieval rebuild and query p95 must be <= {RETRIEVAL_REBUILD_P95_LIMIT_MICROS} us"
                ),
            },
        ),
        (
            "ui_render_p99".into(),
            CheckReport {
                passed: ui_responsive,
                detail: format!(
                    "TUI and web server render p99 must be <= {UI_P99_LIMIT_MICROS} us"
                ),
            },
        ),
        (
            "browser_responsiveness".into(),
            CheckReport {
                passed: browser_responsive,
                detail: format!(
                    "real Chromium route p99 must be <= {BROWSER_ROUTE_P99_LIMIT_MICROS} us and event-to-render p95 <= {BROWSER_INTERACTION_P95_LIMIT_MICROS} us"
                ),
            },
        ),
        (
            "slow_viewer_isolation".into(),
            CheckReport {
                passed: slow_viewer_isolated,
                detail: match (baseline, slow) {
                    (Some(baseline), Some(slow)) => format!(
                        "slow-viewer goal-create p95 {} us versus baseline {} us; allowed <= baseline * 1.25 + 100000 us",
                        slow.p95_micros, baseline.p95_micros
                    ),
                    _ => "baseline or slow-viewer measurement is missing".into(),
                },
            },
        ),
        (
            "provider_streams".into(),
            CheckReport {
                passed: provider_streams,
                detail: format!(
                    "at least two real provider streams must produce content and finish within {MODEL_STREAM_DEADLINE_MICROS} us at p95"
                ),
            },
        ),
        (
            "process_resources".into(),
            CheckReport {
                passed: resident_bounded,
                detail: format!(
                    "each sampled process must remain <= {PROCESS_RESIDENT_LIMIT_BYTES} bytes RSS; long-lived daemon, web, worker, and warmed browser processes must grow <= {PROCESS_GROWTH_LIMIT_BYTES} bytes"
                ),
            },
        ),
        (
            "workload_failures".into(),
            CheckReport {
                passed: failures.is_empty(),
                detail: if failures.is_empty() {
                    "no workload operation failed".into()
                } else {
                    format!("{} workload operation(s) failed", failures.len())
                },
            },
        ),
    ])
}

fn measurements_within(
    latencies: &BTreeMap<String, LatencySummary>,
    names: &[&str],
    limit_micros: u64,
) -> bool {
    names.iter().all(|name| {
        latencies
            .get(*name)
            .is_some_and(|summary| summary.samples > 0 && summary.p95_micros <= limit_micros)
    })
}

fn host_report() -> HostReport {
    let cpu_model = fs::read_to_string("/proc/cpuinfo")
        .ok()
        .and_then(|content| {
            content.lines().find_map(|line| {
                line.strip_prefix("model name").and_then(|value| {
                    value
                        .split_once(':')
                        .map(|(_, value)| value.trim().to_owned())
                })
            })
        });
    let total_memory_bytes = fs::read_to_string("/proc/meminfo")
        .ok()
        .and_then(|content| {
            content.lines().find_map(|line| {
                line.strip_prefix("MemTotal:")
                    .and_then(|value| value.split_whitespace().next())
                    .and_then(|value| value.parse::<u64>().ok())
                    .and_then(|value| value.checked_mul(1_024))
            })
        });
    let rust_version = Command::new("rustc")
        .arg("--version")
        .output()
        .ok()
        .filter(|output| output.status.success())
        .and_then(|output| String::from_utf8(output.stdout).ok())
        .map(|value| value.trim().to_owned());
    HostReport {
        operating_system: env::consts::OS.into(),
        architecture: env::consts::ARCH.into(),
        logical_cpus: thread::available_parallelism().map_or(1, usize::from),
        cpu_model,
        total_memory_bytes,
        rust_version,
    }
}

fn wait_for_socket(socket: &Path, timeout: Duration) -> Result<(), String> {
    let deadline = Instant::now() + timeout;
    loop {
        if NativeClient::connect(socket, Duration::from_secs(2)).is_ok() {
            return Ok(());
        }
        if Instant::now() >= deadline {
            return Err(format!("daemon socket was not ready: {}", socket.display()));
        }
        thread::sleep(Duration::from_millis(10));
    }
}

fn wait_for_tcp(address: SocketAddr, timeout: Duration) -> Result<(), String> {
    let deadline = Instant::now() + timeout;
    loop {
        if TcpStream::connect_timeout(&address, Duration::from_millis(100)).is_ok() {
            return Ok(());
        }
        if Instant::now() >= deadline {
            return Err(format!("web endpoint was not ready: {address}"));
        }
        thread::sleep(Duration::from_millis(10));
    }
}

fn snapshot_payload(
    result: keith_protocol::CommandResult,
    operation: &str,
) -> Result<SessionSnapshot, String> {
    match result {
        keith_protocol::CommandResult::Data(payload) => match *payload {
            ResponsePayload::Snapshot(snapshot) => Ok(*snapshot),
            _ => Err(format!("{operation} returned the wrong payload")),
        },
        result => Err(format!("{operation} failed: {result:?}")),
    }
}

fn data_result(result: keith_protocol::CommandResult, operation: &str) -> Result<(), String> {
    match result {
        keith_protocol::CommandResult::Data(_) => Ok(()),
        result => Err(format!("{operation} failed: {result:?}")),
    }
}

fn accepted(result: keith_protocol::CommandResult, operation: &str) -> Result<(), String> {
    match result {
        keith_protocol::CommandResult::Accepted { .. } => Ok(()),
        result => Err(format!("{operation} failed: {result:?}")),
    }
}

fn required_path(values: &[String], name: &str) -> Result<PathBuf, String> {
    optional_value(values, name)
        .map(PathBuf::from)
        .ok_or_else(|| format!("{name} is required"))
}

fn optional_value(values: &[String], name: &str) -> Option<String> {
    values
        .iter()
        .position(|value| value == name)
        .and_then(|index| values.get(index.saturating_add(1)))
        .cloned()
}

fn repeated_values(values: &[String], name: &str) -> Result<Vec<String>, String> {
    let mut result = Vec::new();
    for (index, value) in values.iter().enumerate() {
        if value == name {
            result.push(
                values
                    .get(index.saturating_add(1))
                    .cloned()
                    .ok_or_else(|| format!("{name} requires a value"))?,
            );
        }
    }
    Ok(result)
}

fn bounded_usize(
    values: &[String],
    name: &str,
    default: usize,
    minimum: usize,
    maximum: usize,
) -> Result<usize, String> {
    let value = optional_value(values, name).map_or(Ok(default), |value| {
        value
            .parse::<usize>()
            .map_err(|_| format!("{name} must be an integer"))
    })?;
    if !(minimum..=maximum).contains(&value) {
        return Err(format!("{name} must be between {minimum} and {maximum}"));
    }
    Ok(value)
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn qualification_checks_enforce_latency_and_resource_bounds() {
        let nominal = LatencySummary {
            samples: 2,
            minimum_micros: 10,
            p50_micros: 10,
            p95_micros: 10,
            p99_micros: 10,
            maximum_micros: 10,
        };
        let mut latencies = STEADY_STATE_MEASUREMENTS
            .iter()
            .chain(LIFECYCLE_MEASUREMENTS.iter())
            .map(|name| ((*name).to_owned(), nominal.clone()))
            .collect::<BTreeMap<_, _>>();
        for name in [
            "tui_render_long_history",
            "web_bootstrap_render_1000_sessions",
            "browser_route_switch",
            "browser_goal_event_to_render",
            "model_stream_first_delta",
            "model_stream_complete",
            "retrieval_rebuild_and_query",
        ] {
            latencies.insert(name.into(), nominal.clone());
        }
        let resources = BTreeMap::from([(
            "daemon".into(),
            ResourceSummary {
                samples: 2,
                first_resident_bytes: Some(1),
                maximum_resident_bytes: Some(2),
                last_resident_bytes: Some(2),
                resident_growth_bytes: Some(1),
                maximum_file_descriptors: Some(8),
            },
        )]);
        assert!(
            qualification_checks(&latencies, &resources, &[])
                .values()
                .all(|check| check.passed)
        );
        latencies
            .get_mut("client_attach_snapshot")
            .unwrap()
            .p95_micros = INTERNAL_P95_LIMIT_MICROS + 1;
        assert!(
            !qualification_checks(&latencies, &resources, &[])["steady_state_latency_p95"].passed
        );
    }

    #[test]
    fn qualification_duration_cannot_be_shortened_into_a_smoke_test() {
        assert_eq!(QUALIFICATION_MINIMUM_SECONDS, 7_200);
    }
}
