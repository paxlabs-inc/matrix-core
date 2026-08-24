use std::collections::BTreeSet;
use std::fs::{self, File, OpenOptions};
use std::io::Write;
use std::path::{Component, Path, PathBuf};

use keith_agent_types::{EntityId, Generation, RootTreeId};
use keith_runtime_api::{
    CandidateCanaryMeasurement, CandidateCanaryRequest, CandidateCanaryVerdict, RuntimeRequest,
    RuntimeResponse,
};
use keith_supervisor::{InstalledImage, SupervisorError, SupervisorOptions, WorkerSupervisor};
use keith_telemetry::MetricName;
use serde::{Deserialize, Serialize};
use thiserror::Error;

use crate::{CorpusError, EvaluationCorpus, ReplayVerdict, TraceReplay};

const MANIFEST: &str = "canary.json";
const MANIFEST_TEMPORARY: &str = ".canary.json.tmp";
const JOURNAL: &str = "canary.journal.json";
const JOURNAL_TEMPORARY: &str = ".canary.journal.json.tmp";

#[derive(Clone, Debug, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case", tag = "verdict", content = "reason")]
pub enum CanaryVerdict {
    Passed,
    Rejected(CanaryFailure),
}

#[derive(Clone, Debug, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum CanaryFailure {
    Startup,
    Crash,
    Lease,
    ProtocolMismatch,
    ReplayInconclusive,
    ReplayRegressed,
    ThresholdUnmet,
    Cleanup,
}

#[derive(Clone, Debug, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct CanaryEvaluation {
    pub transaction_id: EntityId,
    pub image_id: String,
    pub generation: Option<Generation>,
    pub metric: MetricName,
    pub baseline: f64,
    pub measured: Option<f64>,
    pub target_threshold: f64,
    pub latency_ms: u64,
    pub token_cost: u64,
    pub operations: u64,
    pub measurements: Vec<CandidateCanaryMeasurement>,
    pub verdict: CanaryVerdict,
}

#[derive(Clone, Debug, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
struct CanaryManifest {
    version: u32,
    transaction_id: EntityId,
    canary_root: RootTreeId,
    image: InstalledImage,
    generation: Option<Generation>,
    pid: Option<u32>,
}

#[derive(Clone, Copy, Debug, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
enum CanaryCheckpoint {
    ManifestPersisted,
    ImagePinned,
    WorkerStarted,
    ReplayCompleted,
    CleanupStarted,
    WorkerDrained,
}

#[derive(Debug, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
struct CanaryJournal {
    version: u32,
    transaction_id: EntityId,
    image_id: String,
    checkpoint: CanaryCheckpoint,
}

#[derive(Debug, Error)]
pub enum CanaryError {
    #[error("canary configuration is invalid: {0}")]
    Invalid(String),
    #[error("canary filesystem failed: {0}")]
    Io(#[from] std::io::Error),
    #[error("canary manifest failed: {0}")]
    Manifest(#[from] serde_json::Error),
    #[error("canary supervisor failed: {0}")]
    Supervisor(#[from] keith_supervisor::SupervisorError),
    #[error("canary corpus failed: {0}")]
    Corpus(#[from] CorpusError),
}

pub struct CanaryRunner {
    root: PathBuf,
    options: SupervisorOptions,
}

impl CanaryRunner {
    /// Opens the installation-owned canary root and reclaims any process left by an interrupted
    /// daemon before accepting another candidate.
    pub fn open(root: impl Into<PathBuf>, options: SupervisorOptions) -> Result<Self, CanaryError> {
        let root = root.into();
        validate_root(&root)?;
        fs::create_dir_all(&root)?;
        let runner = Self { root, options };
        runner.reconcile()?;
        Ok(runner)
    }

    /// Executes the checked-in corpus inside the exact installed candidate process.
    #[allow(clippy::too_many_arguments)]
    pub fn evaluate(
        &self,
        image: &InstalledImage,
        metric: MetricName,
        baseline: f64,
        target_threshold: f64,
    ) -> Result<CanaryEvaluation, CanaryError> {
        if !baseline.is_finite() || !target_threshold.is_finite() || !image.verified {
            return Err(CanaryError::Invalid(
                "candidate or hypothesis is not verified".into(),
            ));
        }
        let replay = TraceReplay::checked_in()?;
        let transaction_id = EntityId::new();
        let transaction_root = self.root.join(transaction_id.to_string());
        fs::create_dir(&transaction_root)?;
        let canary_root = RootTreeId::new();
        let mut manifest = CanaryManifest {
            version: 1,
            transaction_id: transaction_id.clone(),
            canary_root: canary_root.clone(),
            image: image.clone(),
            generation: None,
            pid: None,
        };
        persist_manifest(&transaction_root, &manifest)?;
        persist_checkpoint(
            &transaction_root,
            &manifest,
            CanaryCheckpoint::ManifestPersisted,
        )?;
        crash_boundary("manifest_persisted", &manifest);

        let evaluation = self.evaluate_inner(
            &transaction_root,
            &mut manifest,
            replay.corpus(),
            metric,
            baseline,
            target_threshold,
        );
        let cleanup = cleanup_transaction(&transaction_root, &manifest, &self.options);
        match (evaluation, cleanup) {
            (Ok(result), Ok(())) => Ok(result),
            (Ok(mut result), Err(_)) => {
                result.verdict = CanaryVerdict::Rejected(CanaryFailure::Cleanup);
                Ok(result)
            }
            (Err(error), Ok(())) => Err(error),
            (Err(_), Err(error)) => Err(error),
        }
    }

    fn evaluate_inner(
        &self,
        transaction_root: &Path,
        manifest: &mut CanaryManifest,
        corpus: &EvaluationCorpus,
        metric: MetricName,
        baseline: f64,
        target_threshold: f64,
    ) -> Result<CanaryEvaluation, CanaryError> {
        let mut supervisor = WorkerSupervisor::open(
            transaction_root.join("runtime"),
            &manifest.image.executable,
            self.options.clone(),
        )?;
        supervisor.pin_canary_image(manifest.image.clone())?;
        persist_checkpoint(transaction_root, manifest, CanaryCheckpoint::ImagePinned)?;
        crash_boundary("image_pinned", manifest);
        let status =
            match supervisor.start_pinned(manifest.canary_root.clone(), &manifest.image.image_id) {
                Ok(status) => status,
                Err(error) => {
                    let failure = match error {
                        SupervisorError::Lease(_) => CanaryFailure::Lease,
                        SupervisorError::StartupExit { .. } => CanaryFailure::Crash,
                        SupervisorError::StartupTimeout { .. } => CanaryFailure::Startup,
                        _ => CanaryFailure::Startup,
                    };
                    return Ok(rejected(
                        manifest,
                        metric,
                        baseline,
                        target_threshold,
                        failure,
                    ));
                }
            };
        manifest.generation = Some(status.generation);
        manifest.pid = Some(status.pid);
        persist_manifest(transaction_root, manifest)?;
        persist_checkpoint(transaction_root, manifest, CanaryCheckpoint::WorkerStarted)?;
        crash_boundary("worker_started", manifest);
        if status.image_id != manifest.image.image_id
            || status.image_manifest_sha256 != manifest.image.manifest_sha256
            || status.source_manifest_sha256 != manifest.image.source_manifest_sha256
        {
            return Ok(rejected(
                manifest,
                metric,
                baseline,
                target_threshold,
                CanaryFailure::ProtocolMismatch,
            ));
        }
        let response = supervisor.execute(
            &manifest.canary_root,
            status.generation,
            RuntimeRequest::CandidateCanary(CandidateCanaryRequest {
                corpus_version: corpus.version,
                corpus_sha256: corpus.content_sha256.clone(),
            }),
        );
        let _ = supervisor.drain(&manifest.canary_root);
        let report = match response {
            Ok(RuntimeResponse::CandidateCanary(report)) => report,
            Ok(_) => {
                return Ok(rejected(
                    manifest,
                    metric,
                    baseline,
                    target_threshold,
                    CanaryFailure::ProtocolMismatch,
                ));
            }
            Err(error) => {
                let failure = if error.to_string().contains("lease") {
                    CanaryFailure::Lease
                } else {
                    CanaryFailure::Crash
                };
                return Ok(rejected(
                    manifest,
                    metric,
                    baseline,
                    target_threshold,
                    failure,
                ));
            }
        };
        persist_checkpoint(
            transaction_root,
            manifest,
            CanaryCheckpoint::ReplayCompleted,
        )?;
        crash_boundary("replay_completed", manifest);
        if report.corpus_version != corpus.version || report.corpus_sha256 != corpus.content_sha256
        {
            return Ok(rejected(
                manifest,
                metric,
                baseline,
                target_threshold,
                CanaryFailure::ProtocolMismatch,
            ));
        }
        validate_measurements(corpus, &report.measurements)?;
        let latency_ms = report.measurements.iter().map(|m| m.latency_ms).sum();
        let token_cost = report.measurements.iter().map(|m| m.tokens).sum();
        let operations = report.measurements.iter().map(|m| m.operations).sum();
        let measured = measured_metric(metric, &report.measurements);
        let failure = if report
            .measurements
            .iter()
            .any(|m| m.verdict == CandidateCanaryVerdict::Inconclusive)
        {
            Some(CanaryFailure::ReplayInconclusive)
        } else if report
            .measurements
            .iter()
            .any(|m| m.verdict == CandidateCanaryVerdict::Regressed)
        {
            Some(CanaryFailure::ReplayRegressed)
        } else if !threshold_met(metric, measured, target_threshold) {
            Some(CanaryFailure::ThresholdUnmet)
        } else {
            None
        };
        Ok(CanaryEvaluation {
            transaction_id: manifest.transaction_id.clone(),
            image_id: manifest.image.image_id.clone(),
            generation: manifest.generation,
            metric,
            baseline,
            measured: Some(measured),
            target_threshold,
            latency_ms,
            token_cost,
            operations,
            measurements: report.measurements,
            verdict: failure.map_or(CanaryVerdict::Passed, CanaryVerdict::Rejected),
        })
    }

    pub fn reconcile(&self) -> Result<(), CanaryError> {
        for entry in fs::read_dir(&self.root)? {
            let entry = entry?;
            if !entry.file_type()?.is_dir() {
                return Err(CanaryError::Invalid(
                    "non-directory entry in canary root".into(),
                ));
            }
            let bytes = read_manifest_for_recovery(&entry.path())?;
            let manifest: CanaryManifest = serde_json::from_slice(&bytes)?;
            validate_journal_for_recovery(&entry.path(), &manifest)?;
            cleanup_transaction(&entry.path(), &manifest, &self.options)?;
        }
        Ok(())
    }
}

fn rejected(
    manifest: &CanaryManifest,
    metric: MetricName,
    baseline: f64,
    target_threshold: f64,
    reason: CanaryFailure,
) -> CanaryEvaluation {
    CanaryEvaluation {
        transaction_id: manifest.transaction_id.clone(),
        image_id: manifest.image.image_id.clone(),
        generation: manifest.generation,
        metric,
        baseline,
        measured: None,
        target_threshold,
        latency_ms: 0,
        token_cost: 0,
        operations: 0,
        measurements: Vec::new(),
        verdict: CanaryVerdict::Rejected(reason),
    }
}

fn validate_measurements(
    corpus: &EvaluationCorpus,
    measurements: &[CandidateCanaryMeasurement],
) -> Result<(), CanaryError> {
    let expected = corpus
        .journeys
        .iter()
        .map(|j| j.id.as_str())
        .collect::<BTreeSet<_>>();
    let actual = measurements
        .iter()
        .map(|m| m.journey_id.as_str())
        .collect::<BTreeSet<_>>();
    if measurements.len() != corpus.journeys.len() || actual != expected {
        return Err(CanaryError::Invalid(
            "candidate omitted or duplicated a corpus journey".into(),
        ));
    }
    Ok(())
}

fn measured_metric(metric: MetricName, values: &[CandidateCanaryMeasurement]) -> f64 {
    match metric {
        MetricName::ModelLatency | MetricName::RetrievalLatency | MetricName::SchedulerLag => {
            values.iter().map(|m| m.latency_ms as f64).sum::<f64>() / values.len() as f64
        }
        MetricName::Workers => 1.0,
        _ => values.iter().map(|m| m.operations as f64).sum(),
    }
}

const fn threshold_met(metric: MetricName, measured: f64, threshold: f64) -> bool {
    match metric {
        MetricName::ModelLatency
        | MetricName::RetrievalLatency
        | MetricName::SchedulerLag
        | MetricName::ActionQueueDepth => measured <= threshold,
        _ => measured >= threshold,
    }
}

fn cleanup_transaction(
    root: &Path,
    manifest: &CanaryManifest,
    options: &SupervisorOptions,
) -> Result<(), CanaryError> {
    if root.file_name().and_then(|v| v.to_str())
        != Some(manifest.transaction_id.to_string().as_str())
    {
        return Err(CanaryError::Invalid(
            "canary manifest path identity mismatch".into(),
        ));
    }
    persist_checkpoint(root, manifest, CanaryCheckpoint::CleanupStarted)?;
    crash_boundary("cleanup_started", manifest);
    let mut supervisor = WorkerSupervisor::open(
        root.join("runtime"),
        &manifest.image.executable,
        options.clone(),
    )?;
    supervisor.pin_canary_image(manifest.image.clone())?;
    let _ = supervisor.adopt_existing();
    supervisor.drain_all()?;
    persist_checkpoint(root, manifest, CanaryCheckpoint::WorkerDrained)?;
    crash_boundary("worker_drained", manifest);
    fs::remove_dir_all(root)?;
    if let Some(parent) = root.parent() {
        File::open(parent)?.sync_all()?;
    }
    crash_boundary("transaction_removed", manifest);
    Ok(())
}

fn persist_manifest(root: &Path, manifest: &CanaryManifest) -> Result<(), CanaryError> {
    persist_json(root, MANIFEST, MANIFEST_TEMPORARY, manifest)
}

fn persist_checkpoint(
    root: &Path,
    manifest: &CanaryManifest,
    checkpoint: CanaryCheckpoint,
) -> Result<(), CanaryError> {
    persist_json(
        root,
        JOURNAL,
        JOURNAL_TEMPORARY,
        &CanaryJournal {
            version: 1,
            transaction_id: manifest.transaction_id.clone(),
            image_id: manifest.image.image_id.clone(),
            checkpoint,
        },
    )
}

fn persist_json<T: Serialize>(
    root: &Path,
    destination: &str,
    temporary: &str,
    value: &T,
) -> Result<(), CanaryError> {
    let temporary = root.join(temporary);
    let mut file = OpenOptions::new()
        .create(true)
        .truncate(true)
        .write(true)
        .open(&temporary)?;
    file.write_all(&serde_json::to_vec(value)?)?;
    file.sync_all()?;
    fs::rename(&temporary, root.join(destination))?;
    File::open(root)?.sync_all()?;
    Ok(())
}

fn read_manifest_for_recovery(root: &Path) -> Result<Vec<u8>, CanaryError> {
    match fs::read(root.join(MANIFEST)) {
        Ok(bytes) => Ok(bytes),
        Err(error) if error.kind() == std::io::ErrorKind::NotFound => {
            Ok(fs::read(root.join(MANIFEST_TEMPORARY))?)
        }
        Err(error) => Err(error.into()),
    }
}

fn validate_journal_for_recovery(
    root: &Path,
    manifest: &CanaryManifest,
) -> Result<(), CanaryError> {
    let bytes = match fs::read(root.join(JOURNAL)) {
        Ok(bytes) => bytes,
        Err(error) if error.kind() == std::io::ErrorKind::NotFound => {
            match fs::read(root.join(JOURNAL_TEMPORARY)) {
                Ok(bytes) => bytes,
                Err(error) if error.kind() == std::io::ErrorKind::NotFound => return Ok(()),
                Err(error) => return Err(error.into()),
            }
        }
        Err(error) => return Err(error.into()),
    };
    let journal: CanaryJournal = serde_json::from_slice(&bytes)?;
    if journal.version != 1
        || journal.transaction_id != manifest.transaction_id
        || journal.image_id != manifest.image.image_id
    {
        return Err(CanaryError::Invalid(
            "canary journal does not match its manifest".into(),
        ));
    }
    Ok(())
}

fn validate_root(path: &Path) -> Result<(), CanaryError> {
    if path.as_os_str().is_empty()
        || path
            .components()
            .any(|component| matches!(component, Component::ParentDir))
        || path.parent().is_none()
    {
        return Err(CanaryError::Invalid("canary root is unsafe".into()));
    }
    Ok(())
}

impl From<ReplayVerdict> for CandidateCanaryVerdict {
    fn from(value: ReplayVerdict) -> Self {
        match value {
            ReplayVerdict::Improved => Self::Improved,
            ReplayVerdict::Equivalent => Self::Equivalent,
            ReplayVerdict::Regressed => Self::Regressed,
            ReplayVerdict::Inconclusive => Self::Inconclusive,
        }
    }
}

#[cfg(debug_assertions)]
fn crash_boundary(boundary: &str, manifest: &CanaryManifest) {
    if std::env::var("KEITH_CANARY_CRASH_AT").as_deref() != Ok(boundary) {
        return;
    }
    let image_matches = std::env::var("KEITH_CANARY_CRASH_IMAGE_ID")
        .as_deref()
        .is_ok_and(|value| value == manifest.image.image_id);
    let transaction_matches = std::env::var("KEITH_CANARY_CRASH_TRANSACTION_ID")
        .as_deref()
        .is_ok_and(|value| value == manifest.transaction_id.to_string());
    if image_matches || transaction_matches {
        std::process::exit(88);
    }
}

#[cfg(not(debug_assertions))]
fn crash_boundary(_boundary: &str, _manifest: &CanaryManifest) {}
