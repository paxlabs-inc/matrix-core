use std::collections::{BTreeMap, BTreeSet};
use std::fs::{self, File, OpenOptions};
use std::io::Write;
use std::path::{Path, PathBuf};
use std::time::Duration;

use keith_agent_types::canonical_json_bytes;
use keith_provider_core::CancellationToken;
use keith_release::{BuildReport, verify_detached_signature};
use keith_tool_runner_core::{
    IsolationRequest, OutputChunk, ProcessLimits, RestrictedProcessRunner, RunError, RunRequest,
};
use ring::signature::{Ed25519KeyPair, KeyPair};
use serde::{Deserialize, Serialize};
use sha2::{Digest, Sha256};
use thiserror::Error;

use crate::{
    ArtifactManifest, ChangeClass, EvolutionGuard, EvolutionProposal, GuardError, ShadowTree,
};

const IMAGE_FORMAT: &str = "keith-worker-image-v1";
const BUILD_DIRECTORY: &str = ".keith-build";
const MAX_RECORDED_OUTPUT_BYTES: usize = 16 * 1024;
const BUILD_JOURNAL_FORMAT: &str = "keith-build-transaction-v1";
const BUILD_JOURNAL_FILE: &str = "transaction.json";
const MAX_BUILD_JOURNAL_BYTES: u64 = 16 * 1024;
const GATE_PLAN: [GateKind; 6] = [
    GateKind::Formatting,
    GateKind::StrictClippy,
    GateKind::WorkspaceTests,
    GateKind::DependencyPolicy,
    GateKind::Security,
    GateKind::Platform,
];

#[derive(Clone, Copy, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum GateKind {
    Formatting,
    StrictClippy,
    WorkspaceTests,
    DependencyPolicy,
    Security,
    Platform,
}

#[derive(Clone, Copy, Debug, Eq, Ord, PartialEq, PartialOrd, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum BuildCheckpoint {
    Prepared,
    ToolchainIdentified,
    GatesPassed,
    ArtifactRead,
    ImageSigned,
    Committed,
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
struct BuildJournalRecord {
    format: String,
    build_id: String,
    base_revision: String,
    source_manifest_sha256: String,
    checkpoint: BuildCheckpoint,
}

/// Installation-local durable record for a build whose candidate image is not live yet.
pub struct BuildCheckpointJournal {
    root: PathBuf,
    path: PathBuf,
    record: BuildJournalRecord,
}

impl BuildCheckpointJournal {
    /// Opens an identity-bound build transaction, discarding an incomplete transaction only when
    /// it belongs to the same source and base revision. A different transaction fails closed.
    pub fn open(
        root: impl Into<PathBuf>,
        build_id: impl Into<String>,
        base_revision: impl Into<String>,
        source_manifest_sha256: impl Into<String>,
    ) -> Result<Self, BuildError> {
        let root = root.into();
        fs::create_dir_all(&root)?;
        reject_unsafe_directory(&root)?;
        let path = root.join(BUILD_JOURNAL_FILE);
        let expected = BuildJournalRecord {
            format: BUILD_JOURNAL_FORMAT.into(),
            build_id: build_id.into(),
            base_revision: base_revision.into(),
            source_manifest_sha256: source_manifest_sha256.into(),
            checkpoint: BuildCheckpoint::Prepared,
        };
        validate_build_identity(&expected)?;
        if path.exists() {
            let prior = load_build_journal(&path)?;
            if prior.build_id != expected.build_id
                || prior.base_revision != expected.base_revision
                || prior.source_manifest_sha256 != expected.source_manifest_sha256
            {
                return Err(BuildError::BuildTransactionConflict);
            }
            remove_regular_file(&path)?;
            sync_directory(&root)?;
        }
        let journal = Self {
            root,
            path,
            record: expected,
        };
        journal.persist()?;
        debug_build_boundary(BuildCheckpoint::Prepared);
        Ok(journal)
    }

    /// Advances the monotonic journal before the corresponding in-memory image transition.
    pub fn checkpoint(&mut self, checkpoint: BuildCheckpoint) -> Result<(), BuildError> {
        if checkpoint <= self.record.checkpoint {
            return Err(BuildError::InvalidBuildCheckpoint);
        }
        self.record.checkpoint = checkpoint;
        self.persist()?;
        debug_build_boundary(checkpoint);
        Ok(())
    }

    #[must_use]
    pub const fn current(&self) -> BuildCheckpoint {
        self.record.checkpoint
    }

    /// Reopens the durable record. Incomplete records describe an uncommitted image and are
    /// removed; committed records are retained as proof of the exact image transaction.
    pub fn recover(root: impl AsRef<Path>) -> Result<Option<BuildCheckpoint>, BuildError> {
        let root = root.as_ref();
        reject_unsafe_directory(root)?;
        let path = root.join(BUILD_JOURNAL_FILE);
        if !path.exists() {
            return Ok(None);
        }
        let record = load_build_journal(&path)?;
        if record.checkpoint != BuildCheckpoint::Committed {
            remove_regular_file(&path)?;
            sync_directory(root)?;
        }
        Ok(Some(record.checkpoint))
    }

    fn persist(&self) -> Result<(), BuildError> {
        let bytes = serde_json::to_vec(&self.record).map_err(ImageError::from)?;
        let temporary = self
            .root
            .join(format!(".{BUILD_JOURNAL_FILE}.{}.tmp", std::process::id()));
        remove_regular_file_if_present(&temporary)?;
        let mut file = OpenOptions::new()
            .create_new(true)
            .write(true)
            .open(&temporary)?;
        file.write_all(&bytes)?;
        file.sync_all()?;
        keith_platform::replace_file(&temporary, &self.path)?;
        sync_directory(&self.root)?;
        Ok(())
    }
}

impl GateKind {
    fn arguments(self) -> &'static [&'static str] {
        match self {
            Self::Formatting => &["fmt", "--all", "--", "--check"],
            Self::StrictClippy => &[
                "clippy",
                "--workspace",
                "--all-targets",
                "--offline",
                "--locked",
                "--",
                "-D",
                "warnings",
            ],
            Self::WorkspaceTests => &["test", "--workspace", "--offline", "--locked"],
            Self::DependencyPolicy => &[
                "run",
                "--offline",
                "--locked",
                "-p",
                "keith-xtask",
                "--",
                "dependency-policy",
            ],
            Self::Security => &[
                "run",
                "--offline",
                "--locked",
                "-p",
                "keith-xtask",
                "--",
                "security-gate",
            ],
            Self::Platform => &[
                "run",
                "--offline",
                "--locked",
                "-p",
                "keith-xtask",
                "--",
                "platform-gate",
            ],
        }
    }
}

#[derive(Clone, Debug, Eq, PartialEq)]
pub struct BuildSandbox {
    cargo: PathBuf,
    rustc: PathBuf,
    cargo_home: PathBuf,
    rustup_home: PathBuf,
    limits: ProcessLimits,
}

impl BuildSandbox {
    /// Creates a fail-closed build sandbox using explicit toolchain paths and limits.
    ///
    /// # Errors
    /// Returns an error unless every path is absolute, canonical, and the requested limits include
    /// CPU, memory, output, wall-time, process-tree, filesystem, and network containment.
    pub fn new(
        cargo: impl Into<PathBuf>,
        rustc: impl Into<PathBuf>,
        cargo_home: impl Into<PathBuf>,
        rustup_home: impl Into<PathBuf>,
        limits: ProcessLimits,
    ) -> Result<Self, BuildError> {
        if !limits.deny_network
            || limits.cpu_seconds.is_none()
            || limits.memory_bytes.is_none()
            || limits.timeout.is_zero()
            || limits.output_bytes == 0
        {
            return Err(BuildError::IncompleteLimits);
        }
        Ok(Self {
            cargo: canonical_file(cargo.into())?,
            rustc: canonical_file(rustc.into())?,
            cargo_home: canonical_directory(cargo_home.into())?,
            rustup_home: canonical_directory(rustup_home.into())?,
            limits,
        })
    }

    #[must_use]
    pub fn production(
        cargo: impl Into<PathBuf>,
        rustc: impl Into<PathBuf>,
        cargo_home: impl Into<PathBuf>,
        rustup_home: impl Into<PathBuf>,
    ) -> Result<Self, BuildError> {
        Self::new(
            cargo,
            rustc,
            cargo_home,
            rustup_home,
            ProcessLimits {
                timeout: Duration::from_secs(60 * 60),
                cancellation_grace: Duration::from_secs(2),
                output_bytes: 32 * 1024 * 1024,
                cpu_seconds: Some(60 * 60),
                memory_bytes: Some(16 * 1024 * 1024 * 1024),
                deny_network: true,
            },
        )
    }

    fn runner(
        &self,
        shadow: &ShadowTree,
        build_id: &str,
        additional_programs: &[PathBuf],
    ) -> Result<RestrictedProcessRunner, BuildError> {
        let owned = shadow.root().join(BUILD_DIRECTORY);
        let home = owned.join("home");
        let target = owned.join("target");
        let temporary = owned.join("tmp");
        for directory in [&home, &target, &temporary] {
            fs::create_dir_all(directory)?;
        }
        let path = self.cargo.parent().map_or_else(
            || "/usr/bin:/bin".into(),
            |parent| format!("{}:/usr/bin:/bin", parent.display()),
        );
        let environment = BTreeMap::from([
            ("CARGO_HOME".into(), self.cargo_home.display().to_string()),
            ("RUSTUP_HOME".into(), self.rustup_home.display().to_string()),
            ("CARGO_TARGET_DIR".into(), target.display().to_string()),
            ("CARGO_NET_OFFLINE".into(), "true".into()),
            ("HOME".into(), home.display().to_string()),
            ("TMPDIR".into(), temporary.display().to_string()),
            ("PATH".into(), path),
            ("LANG".into(), "C".into()),
            ("LC_ALL".into(), "C".into()),
            ("KEITH_BUILD_ID".into(), build_id.to_owned()),
        ]);
        let mut allowed_programs = vec![self.cargo.clone(), self.rustc.clone()];
        allowed_programs.extend_from_slice(additional_programs);
        let runner = RestrictedProcessRunner::new_with_read_only_paths(
            shadow.root(),
            allowed_programs,
            BTreeSet::new(),
            environment,
            [self.cargo_home.clone(), self.rustup_home.clone()],
        )?;
        let status = runner.sandbox_status();
        if !status.supports_untrusted()
            || !status.network_isolation
            || !status.cpu_limit
            || !status.memory_limit
        {
            return Err(BuildError::SandboxUnavailable);
        }
        Ok(runner)
    }
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct ToolchainIdentity {
    pub rustc: String,
    pub cargo: String,
    pub target: String,
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct GateResult {
    pub gate: GateKind,
    pub exit_code: i32,
    pub elapsed_millis: u64,
    pub output: String,
    pub output_sha256: String,
    pub sandbox: keith_sandbox::SandboxStatus,
}

impl GateResult {
    #[must_use]
    pub const fn succeeded(&self) -> bool {
        self.exit_code == 0
    }
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum GateFailureKind {
    Exit,
    Timeout,
    Resource,
    Cancelled,
    Sandbox,
    SecretLeak,
    Process,
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct GateFailure {
    pub gate: GateKind,
    pub kind: GateFailureKind,
    pub exit_code: Option<i32>,
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct WorkerImageManifest {
    pub format: String,
    pub build_id: String,
    pub base_revision: String,
    pub source_manifest_sha256: String,
    pub executable_sha256: String,
    pub executable_bytes: u64,
    pub toolchain: ToolchainIdentity,
    pub worker_report: BuildReport,
    pub gates: Vec<GateResult>,
    pub artifact_source_paths: Vec<PathBuf>,
    pub change_class: String,
}

#[derive(Clone, Debug, Eq, PartialEq)]
pub struct WorkerImage {
    manifest: WorkerImageManifest,
    executable: Vec<u8>,
    signature: Vec<u8>,
    signing_public_key: [u8; 32],
}

impl WorkerImage {
    /// Reconstructs an image received from durable or IPC storage and authenticates every field
    /// before returning it to promotion code.
    ///
    /// # Errors
    /// Returns an error for an untrusted signer, invalid signature, incomplete gates, or changed
    /// executable bytes.
    pub fn from_signed_parts(
        manifest: WorkerImageManifest,
        executable: Vec<u8>,
        signature: Vec<u8>,
        signing_public_key: [u8; 32],
        trusted_public_key: &[u8; 32],
    ) -> Result<Self, ImageError> {
        let image = Self {
            manifest,
            executable,
            signature,
            signing_public_key,
        };
        image.verify(trusted_public_key)?;
        Ok(image)
    }

    #[must_use]
    pub fn manifest(&self) -> &WorkerImageManifest {
        &self.manifest
    }

    #[must_use]
    pub fn executable(&self) -> &[u8] {
        &self.executable
    }

    #[must_use]
    pub fn signature(&self) -> &[u8] {
        &self.signature
    }

    /// Returns the exact canonical manifest bytes covered by [`Self::signature`].
    ///
    /// # Errors
    /// Returns an error only when the typed manifest cannot be serialized.
    pub fn manifest_bytes(&self) -> Result<Vec<u8>, ImageError> {
        Ok(canonical_json_bytes(&self.manifest)?)
    }

    /// Verifies the detached signature, exact gate plan, and executable bytes against an
    /// independently trusted public key.
    ///
    /// # Errors
    /// Returns an error for an untrusted signer, malformed/tampered signature, incomplete gate,
    /// invalid identity, or changed executable.
    pub fn verify(&self, trusted_public_key: &[u8; 32]) -> Result<(), ImageError> {
        if &self.signing_public_key != trusted_public_key {
            return Err(ImageError::UntrustedSigner);
        }
        let manifest = canonical_json_bytes(&self.manifest)?;
        verify_detached_signature(&manifest, &self.signature, trusted_public_key)
            .map_err(|_| ImageError::InvalidSignature)?;
        if self.manifest.format != IMAGE_FORMAT
            || self.manifest.build_id.trim().is_empty()
            || !matches!(
                self.manifest.worker_report.component.as_str(),
                "worker" | "daemon"
            )
            || self.manifest.worker_report.build_id != self.manifest.build_id
            || self
                .manifest
                .worker_report
                .protocol_version
                .trim()
                .is_empty()
            || self.manifest.worker_report.storage_schema.trim().is_empty()
            || self.manifest.worker_report.enabled_features.is_empty()
            || self.manifest.base_revision.len() != 40
            || self.manifest.source_manifest_sha256.len() != 64
            || self.manifest.gates.len() != GATE_PLAN.len()
            || !self
                .manifest
                .gates
                .iter()
                .zip(GATE_PLAN)
                .all(|(actual, expected)| actual.gate == expected && actual.succeeded())
        {
            return Err(ImageError::InvalidManifest);
        }
        if u64::try_from(self.executable.len()).ok() != Some(self.manifest.executable_bytes)
            || digest(&self.executable) != self.manifest.executable_sha256
        {
            return Err(ImageError::ArtifactMismatch);
        }
        Ok(())
    }
}

pub struct WorkerImageSigner {
    key: Ed25519KeyPair,
}

impl WorkerImageSigner {
    /// Creates the installation-owned signer from a seed that is never passed to a build process.
    ///
    /// # Errors
    /// Returns an error unless the seed is a valid Ed25519 seed.
    pub fn from_seed(seed: &[u8; 32]) -> Result<Self, ImageError> {
        Ok(Self {
            key: Ed25519KeyPair::from_seed_unchecked(seed)
                .map_err(|_| ImageError::InvalidSigningKey)?,
        })
    }

    #[must_use]
    pub fn public_key(&self) -> [u8; 32] {
        self.key
            .public_key()
            .as_ref()
            .try_into()
            .expect("Ed25519 public keys are 32 bytes")
    }

    fn sign(&self, manifest: WorkerImageManifest, executable: Vec<u8>) -> WorkerImage {
        let bytes = canonical_json_bytes(&manifest).expect("serializable worker image manifest");
        WorkerImage {
            signature: self.key.sign(&bytes).as_ref().to_vec(),
            signing_public_key: self.public_key(),
            manifest,
            executable,
        }
    }
}

pub struct VerificationGate {
    sandbox: BuildSandbox,
    signer: WorkerImageSigner,
    sensitive_values: Vec<Vec<u8>>,
}

impl VerificationGate {
    #[must_use]
    pub fn new(
        sandbox: BuildSandbox,
        signer: WorkerImageSigner,
        sensitive_values: Vec<Vec<u8>>,
    ) -> Self {
        Self {
            sandbox,
            signer,
            sensitive_values,
        }
    }

    /// Executes the complete immutable gate and signs a candidate only after every real process
    /// succeeds and the produced bytes pass leak and manifest checks.
    ///
    /// # Errors
    /// Returns a precondition or local I/O error. Gate process failures are represented by a
    /// non-overridable [`BuildVerdict::Failed`].
    pub fn verify(
        &self,
        shadow: &ShadowTree,
        proposal: &EvolutionProposal,
        cancellation: &CancellationToken,
    ) -> Result<BuildVerdict, BuildError> {
        let guard = EvolutionGuard::new(shadow.root())?;
        let proposal_class = guard.recheck_before_build(&proposal.changes)?;
        let source_digest = digest_source_tree(shadow.root())?;
        let build_id = format!("evolution-{}", &source_digest[..24]);
        let journal_root = shadow.root().join(BUILD_DIRECTORY).join("journal");
        let mut journal = BuildCheckpointJournal::open(
            journal_root,
            &build_id,
            shadow.base_revision(),
            &source_digest,
        )?;
        let runner = self.sandbox.runner(shadow, &build_id, &[])?;
        let toolchain = ToolchainIdentity {
            cargo: self.tool_version(&runner, &self.sandbox.cargo, cancellation)?,
            rustc: self.tool_version(&runner, &self.sandbox.rustc, cancellation)?,
            target: format!("{}-{}", std::env::consts::ARCH, std::env::consts::OS),
        };
        journal.checkpoint(BuildCheckpoint::ToolchainIdentified)?;
        let mut results = Vec::with_capacity(GATE_PLAN.len());
        for gate in GATE_PLAN {
            match self.run_gate(&runner, gate, cancellation) {
                Ok(result) if result.succeeded() => results.push(result),
                Ok(result) => {
                    let failure = GateFailure {
                        gate,
                        kind: GateFailureKind::Exit,
                        exit_code: Some(result.exit_code),
                    };
                    results.push(result);
                    return Ok(BuildVerdict::Failed { results, failure });
                }
                Err(failure) => return Ok(BuildVerdict::Failed { results, failure }),
            }
        }
        journal.checkpoint(BuildCheckpoint::GatesPassed)?;

        let executable_path = shadow
            .root()
            .join(BUILD_DIRECTORY)
            .join("target")
            .join("release")
            .join(format!("agent-worker{}", std::env::consts::EXE_SUFFIX));
        let metadata = fs::symlink_metadata(&executable_path)?;
        if !metadata.is_file() || metadata.file_type().is_symlink() {
            return Err(BuildError::MissingWorkerImage);
        }
        let executable = fs::read(&executable_path)?;
        journal.checkpoint(BuildCheckpoint::ArtifactRead)?;
        if contains_sensitive(&executable, &self.sensitive_values) {
            return Ok(BuildVerdict::Failed {
                results,
                failure: GateFailure {
                    gate: GateKind::Security,
                    kind: GateFailureKind::SecretLeak,
                    exit_code: None,
                },
            });
        }
        let inspection_runner =
            self.sandbox
                .runner(shadow, &build_id, std::slice::from_ref(&executable_path))?;
        let worker_report = self.inspect_worker(
            &inspection_runner,
            &executable_path,
            &build_id,
            cancellation,
        )?;
        let source_paths = artifact_paths(proposal);
        let artifact = ArtifactManifest {
            source_paths: source_paths.clone(),
            ..ArtifactManifest::default()
        };
        let classification = guard.recompute(&proposal.changes, &artifact)?;
        let effective_class = proposal_class.max(classification.artifact);
        let manifest = WorkerImageManifest {
            format: IMAGE_FORMAT.into(),
            build_id,
            base_revision: shadow.base_revision().to_owned(),
            source_manifest_sha256: source_digest,
            executable_sha256: digest(&executable),
            executable_bytes: u64::try_from(executable.len())
                .map_err(|_| BuildError::MissingWorkerImage)?,
            toolchain,
            worker_report,
            gates: results.clone(),
            artifact_source_paths: source_paths,
            change_class: class_name(effective_class).into(),
        };
        let image = self.signer.sign(manifest, executable);
        image.verify(&self.signer.public_key())?;
        journal.checkpoint(BuildCheckpoint::ImageSigned)?;
        journal.checkpoint(BuildCheckpoint::Committed)?;
        Ok(BuildVerdict::Passed { results, image })
    }

    fn tool_version(
        &self,
        runner: &RestrictedProcessRunner,
        program: &Path,
        cancellation: &CancellationToken,
    ) -> Result<String, BuildError> {
        let request = self.request(program, ["--version"]);
        let result = runner.run(&request, cancellation, &mut discard_output)?;
        if result.exit_code != Some(0) || contains_sensitive(&result.stdout, &self.sensitive_values)
        {
            return Err(BuildError::ToolchainIdentity);
        }
        let version = String::from_utf8_lossy(&result.stdout).trim().to_owned();
        if version.is_empty() || version.len() > 512 {
            return Err(BuildError::ToolchainIdentity);
        }
        Ok(version)
    }

    fn inspect_worker(
        &self,
        runner: &RestrictedProcessRunner,
        executable: &Path,
        expected_build_id: &str,
        cancellation: &CancellationToken,
    ) -> Result<BuildReport, BuildError> {
        let request = self.request(executable, ["--build-info"]);
        let result = runner.run(&request, cancellation, &mut discard_output)?;
        if result.exit_code != Some(0)
            || !result.stderr.is_empty()
            || contains_sensitive(&result.stdout, &self.sensitive_values)
        {
            return Err(BuildError::WorkerInspection);
        }
        let report: BuildReport =
            serde_json::from_slice(&result.stdout).map_err(|_| BuildError::WorkerInspection)?;
        if report.component != "worker"
            || report.build_id != expected_build_id
            || report.protocol_version.trim().is_empty()
            || report.storage_schema.trim().is_empty()
            || report.enabled_features.is_empty()
        {
            return Err(BuildError::WorkerInspection);
        }
        Ok(report)
    }

    fn run_gate(
        &self,
        runner: &RestrictedProcessRunner,
        gate: GateKind,
        cancellation: &CancellationToken,
    ) -> Result<GateResult, GateFailure> {
        let request = self.request(&self.sandbox.cargo, gate.arguments().iter().copied());
        let result = runner
            .run(&request, cancellation, &mut discard_output)
            .map_err(|error| GateFailure {
                gate,
                kind: failure_kind(&error),
                exit_code: None,
            })?;
        let mut raw = result.stdout;
        raw.extend_from_slice(&result.stderr);
        if contains_sensitive(&raw, &self.sensitive_values) {
            return Err(GateFailure {
                gate,
                kind: GateFailureKind::SecretLeak,
                exit_code: result.exit_code,
            });
        }
        let output_sha256 = digest(&raw);
        raw.truncate(MAX_RECORDED_OUTPUT_BYTES);
        Ok(GateResult {
            gate,
            exit_code: result.exit_code.unwrap_or(-1),
            elapsed_millis: u64::try_from(result.elapsed.as_millis()).unwrap_or(u64::MAX),
            output: String::from_utf8_lossy(&raw).into_owned(),
            output_sha256,
            sandbox: result.sandbox,
        })
    }

    fn request<'a>(
        &self,
        program: &Path,
        arguments: impl IntoIterator<Item = &'a str>,
    ) -> RunRequest {
        RunRequest {
            program: program.to_path_buf(),
            arguments: arguments.into_iter().map(str::to_owned).collect(),
            working_directory: PathBuf::from("."),
            environment: BTreeMap::new(),
            isolation: IsolationRequest::UntrustedWorkspace,
            limits: self.sandbox.limits.clone(),
        }
    }
}

pub enum BuildVerdict {
    Passed {
        results: Vec<GateResult>,
        image: WorkerImage,
    },
    Failed {
        results: Vec<GateResult>,
        failure: GateFailure,
    },
}

#[derive(Debug, Error)]
pub enum BuildError {
    #[error("build sandbox limits are incomplete")]
    IncompleteLimits,
    #[error("strong build sandbox is unavailable")]
    SandboxUnavailable,
    #[error("build toolchain identity is unavailable")]
    ToolchainIdentity,
    #[error("candidate worker executable is missing or invalid")]
    MissingWorkerImage,
    #[error("candidate worker build report is missing or inconsistent")]
    WorkerInspection,
    #[error("another source-bound build transaction is present")]
    BuildTransactionConflict,
    #[error("build transaction checkpoint is invalid")]
    InvalidBuildCheckpoint,
    #[error("build transaction journal is invalid")]
    InvalidBuildJournal,
    #[error("build path is not a canonical file or directory")]
    InvalidPath,
    #[error("build I/O failed: {0}")]
    Io(#[from] std::io::Error),
    #[error("build process failed: {0}")]
    Process(#[from] RunError),
    #[error("evolution guard rejected build: {0}")]
    Guard(#[from] GuardError),
    #[error("worker image is invalid: {0}")]
    Image(#[from] ImageError),
}

#[derive(Debug, Error)]
pub enum ImageError {
    #[error("worker image signing key is invalid")]
    InvalidSigningKey,
    #[error("worker image signer is not trusted")]
    UntrustedSigner,
    #[error("worker image signature is invalid")]
    InvalidSignature,
    #[error("worker image manifest is incomplete")]
    InvalidManifest,
    #[error("worker image artifact does not match its manifest")]
    ArtifactMismatch,
    #[error("worker image serialization failed: {0}")]
    Serialization(#[from] serde_json::Error),
}

fn canonical_file(path: PathBuf) -> Result<PathBuf, BuildError> {
    if !path.is_absolute() {
        return Err(BuildError::InvalidPath);
    }
    let canonical = fs::canonicalize(&path).map_err(|_| BuildError::InvalidPath)?;
    canonical
        .is_file()
        .then_some(path)
        .ok_or(BuildError::InvalidPath)
}

fn canonical_directory(path: PathBuf) -> Result<PathBuf, BuildError> {
    let path = fs::canonicalize(path).map_err(|_| BuildError::InvalidPath)?;
    path.is_dir().then_some(path).ok_or(BuildError::InvalidPath)
}

fn validate_build_identity(record: &BuildJournalRecord) -> Result<(), BuildError> {
    if record.format != BUILD_JOURNAL_FORMAT
        || record.build_id.trim().is_empty()
        || record.base_revision.len() != 40
        || record.source_manifest_sha256.len() != 64
        || !record
            .base_revision
            .bytes()
            .all(|byte| byte.is_ascii_hexdigit())
        || !record
            .source_manifest_sha256
            .bytes()
            .all(|byte| byte.is_ascii_hexdigit())
    {
        return Err(BuildError::InvalidBuildJournal);
    }
    Ok(())
}

fn reject_unsafe_directory(path: &Path) -> Result<(), BuildError> {
    let metadata = fs::symlink_metadata(path)?;
    if metadata.file_type().is_symlink() || !metadata.is_dir() {
        return Err(BuildError::InvalidBuildJournal);
    }
    Ok(())
}

fn load_build_journal(path: &Path) -> Result<BuildJournalRecord, BuildError> {
    let metadata = fs::symlink_metadata(path)?;
    if metadata.file_type().is_symlink()
        || !metadata.is_file()
        || metadata.len() > MAX_BUILD_JOURNAL_BYTES
    {
        return Err(BuildError::InvalidBuildJournal);
    }
    let record: BuildJournalRecord =
        serde_json::from_slice(&fs::read(path)?).map_err(|_| BuildError::InvalidBuildJournal)?;
    validate_build_identity(&record)?;
    Ok(record)
}

fn remove_regular_file(path: &Path) -> Result<(), BuildError> {
    let metadata = fs::symlink_metadata(path)?;
    if metadata.file_type().is_symlink() || !metadata.is_file() {
        return Err(BuildError::InvalidBuildJournal);
    }
    fs::remove_file(path)?;
    Ok(())
}

fn remove_regular_file_if_present(path: &Path) -> Result<(), BuildError> {
    match fs::symlink_metadata(path) {
        Ok(metadata) if metadata.file_type().is_symlink() || !metadata.is_file() => {
            Err(BuildError::InvalidBuildJournal)
        }
        Ok(_) => {
            fs::remove_file(path)?;
            Ok(())
        }
        Err(error) if error.kind() == std::io::ErrorKind::NotFound => Ok(()),
        Err(error) => Err(error.into()),
    }
}

fn sync_directory(path: &Path) -> Result<(), BuildError> {
    File::open(path)?.sync_all()?;
    Ok(())
}

#[cfg(debug_assertions)]
fn debug_build_boundary(checkpoint: BuildCheckpoint) {
    let expected = serde_json::to_string(&checkpoint).expect("checkpoint serializes");
    let name = expected.trim_matches('"');
    if std::env::var("KEITH_BUILD_CRASH_AT").as_deref() == Ok(name) {
        std::process::exit(86);
    }
    if std::env::var("KEITH_BUILD_PAUSE_AT").as_deref() == Ok(name) {
        loop {
            std::thread::park_timeout(Duration::from_secs(1));
        }
    }
}

#[cfg(not(debug_assertions))]
fn debug_build_boundary(_checkpoint: BuildCheckpoint) {}

fn failure_kind(error: &RunError) -> GateFailureKind {
    match error {
        RunError::Timeout => GateFailureKind::Timeout,
        RunError::OutputLimit | RunError::LimitUnavailable(_) => GateFailureKind::Resource,
        RunError::Cancelled => GateFailureKind::Cancelled,
        RunError::StrongIsolationUnavailable => GateFailureKind::Sandbox,
        _ => GateFailureKind::Process,
    }
}

fn discard_output(_chunk: &OutputChunk) {}

fn artifact_paths(proposal: &EvolutionProposal) -> Vec<PathBuf> {
    let mut paths = BTreeSet::new();
    for change in &proposal.changes {
        match change {
            crate::ChangedPath::Write(path) | crate::ChangedPath::Delete(path) => {
                paths.insert(path.clone());
            }
            crate::ChangedPath::Rename { from, to } => {
                paths.insert(from.clone());
                paths.insert(to.clone());
            }
        }
    }
    paths.into_iter().collect()
}

fn class_name(class: ChangeClass) -> &'static str {
    match class {
        ChangeClass::A => "a",
        ChangeClass::B => "b",
        ChangeClass::C => "c",
        ChangeClass::D => "d",
    }
}

fn contains_sensitive(bytes: &[u8], sensitive: &[Vec<u8>]) -> bool {
    sensitive
        .iter()
        .filter(|pattern| !pattern.is_empty())
        .any(|pattern| bytes.windows(pattern.len()).any(|window| window == pattern))
}

fn digest(bytes: &[u8]) -> String {
    format!("{:x}", Sha256::digest(bytes))
}

fn digest_source_tree(root: &Path) -> Result<String, BuildError> {
    let mut files = Vec::new();
    collect_source_files(root, root, &mut files)?;
    files.sort();
    let mut digest = Sha256::new();
    for path in files {
        let relative = path
            .strip_prefix(root)
            .map_err(|_| BuildError::InvalidPath)?;
        digest.update(relative.as_os_str().as_encoded_bytes());
        digest.update([0]);
        digest.update(fs::read(path)?);
        digest.update([0]);
    }
    Ok(format!("{:x}", digest.finalize()))
}

fn collect_source_files(
    root: &Path,
    directory: &Path,
    files: &mut Vec<PathBuf>,
) -> Result<(), BuildError> {
    for entry in fs::read_dir(directory)? {
        let entry = entry?;
        let path = entry.path();
        let kind = entry.file_type()?;
        let relative = path
            .strip_prefix(root)
            .map_err(|_| BuildError::InvalidPath)?;
        if relative == Path::new(BUILD_DIRECTORY)
            || relative == Path::new("target")
            || relative == Path::new(".git")
        {
            continue;
        }
        if kind.is_symlink() || (!kind.is_file() && !kind.is_dir()) {
            return Err(BuildError::InvalidPath);
        }
        if kind.is_dir() {
            collect_source_files(root, &path, files)?;
        } else {
            files.push(path);
        }
    }
    Ok(())
}

#[cfg(test)]
mod tests {
    use super::*;

    fn successful_result(gate: GateKind) -> GateResult {
        GateResult {
            gate,
            exit_code: 0,
            elapsed_millis: 1,
            output: "real exit status: success".into(),
            output_sha256: digest(b"real exit status: success"),
            sandbox: keith_sandbox::SandboxStatus::detect(),
        }
    }

    fn signed_image() -> (WorkerImage, [u8; 32]) {
        let signer = WorkerImageSigner::from_seed(&[7; 32]).unwrap();
        let key = signer.public_key();
        let executable = b"candidate-worker-binary".to_vec();
        let manifest = WorkerImageManifest {
            format: IMAGE_FORMAT.into(),
            build_id: "evolution-build".into(),
            base_revision: "a".repeat(40),
            source_manifest_sha256: "b".repeat(64),
            executable_sha256: digest(&executable),
            executable_bytes: executable.len() as u64,
            toolchain: ToolchainIdentity {
                rustc: "rustc test".into(),
                cargo: "cargo test".into(),
                target: "test-host".into(),
            },
            worker_report: BuildReport {
                component: "worker".into(),
                package_version: "0.1.0".into(),
                build_id: "evolution-build".into(),
                protocol_version: "1.0".into(),
                storage_schema: "1.0".into(),
                enabled_features: BTreeSet::from(["runtime".into()]),
            },
            gates: GATE_PLAN.into_iter().map(successful_result).collect(),
            artifact_source_paths: vec!["crates/tools/src/lib.rs".into()],
            change_class: "b".into(),
        };
        (signer.sign(manifest, executable), key)
    }

    #[test]
    fn fixed_gate_plan_cannot_be_skipped_reordered_or_faked() {
        assert_eq!(
            GATE_PLAN,
            [
                GateKind::Formatting,
                GateKind::StrictClippy,
                GateKind::WorkspaceTests,
                GateKind::DependencyPolicy,
                GateKind::Security,
                GateKind::Platform,
            ]
        );
        let (mut image, key) = signed_image();
        image.verify(&key).unwrap();
        image.manifest.gates.swap(0, 1);
        assert!(matches!(
            image.verify(&key),
            Err(ImageError::InvalidSignature)
        ));

        let (mut image, key) = signed_image();
        image.manifest.gates[2].exit_code = 1;
        assert!(matches!(
            image.verify(&key),
            Err(ImageError::InvalidSignature)
        ));
    }

    #[test]
    fn unsigned_wrongly_signed_and_tampered_worker_images_are_rejected() {
        let (mut image, key) = signed_image();
        image.verify(&key).unwrap();
        assert!(matches!(
            image.verify(&[9; 32]),
            Err(ImageError::UntrustedSigner)
        ));
        image.signature.clear();
        assert!(matches!(
            image.verify(&key),
            Err(ImageError::InvalidSignature)
        ));

        let (mut image, key) = signed_image();
        image.executable.push(0);
        assert!(matches!(
            image.verify(&key),
            Err(ImageError::ArtifactMismatch)
        ));
    }

    #[test]
    fn sandbox_requires_every_non_overridable_resource_boundary() {
        let mut limits = ProcessLimits::default();
        limits.deny_network = true;
        limits.cpu_seconds = Some(1);
        assert!(matches!(
            BuildSandbox::new("cargo", "rustc", ".", ".", limits),
            Err(BuildError::IncompleteLimits)
        ));
    }

    #[test]
    fn secret_scanner_rejects_exact_values_in_output_or_artifacts() {
        let secret = b"seeded-build-secret".to_vec();
        assert!(contains_sensitive(
            b"diagnostic seeded-build-secret leaked",
            std::slice::from_ref(&secret)
        ));
        assert!(!contains_sensitive(
            b"ordinary compiler diagnostic",
            std::slice::from_ref(&secret)
        ));
    }
}
