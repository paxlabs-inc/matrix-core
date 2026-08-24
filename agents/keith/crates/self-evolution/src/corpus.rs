use keith_agent_types::ToolFailure;
#[cfg(test)]
use keith_provider_core::StopReason;
use keith_provider_core::{ModelEvent, ProviderError, Usage};
use keith_test_support::{
    RecordedTape, RegressionComparison, compare_regression, ensure_public_fixture,
};
use keith_tool_core::{TerminalState, ToolEvent, ToolOutcome};
use serde::{Deserialize, Serialize};
use sha2::{Digest, Sha256};
use std::collections::{BTreeMap, BTreeSet};
use std::fs::{self, OpenOptions};
use std::io::Write as _;
use std::path::{Component, Path, PathBuf};
use thiserror::Error;

pub const CORPUS_BYTES: &[u8] = include_bytes!("../corpus/v1.json");
pub const RUNTIME_CORPUS_REGISTRY_VERSION: u32 = 1;
const RUNTIME_CORPUS_REGISTRY_FILE: &str = "registry.json";
const RUNTIME_CORPUS_COPIES_DIR: &str = "copies";
const MAX_OUTPUT_BYTES: usize = 64 * 1024;
const MAX_TOKENS: u64 = 1_000_000;
const MAX_LATENCY_MS: u64 = 86_400_000;
const MAX_OPERATIONS: u64 = 100_000;

#[derive(Clone, Debug, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct EvaluationCorpus {
    pub version: u32,
    pub content_sha256: String,
    pub journeys: Vec<EvaluationJourney>,
}
#[derive(Clone, Debug, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct EvaluationJourney {
    pub id: String,
    pub kind: JourneyKind,
    pub trace: Vec<TraceStep>,
    pub expected: ExpectedOutcome,
    pub baseline_tokens: u64,
    pub baseline_latency_ms: u64,
    pub baseline_operations: u64,
}
#[derive(Clone, Debug, PartialEq, Serialize, Deserialize)]
#[serde(tag = "source", rename_all = "snake_case", deny_unknown_fields)]
pub enum TraceStep {
    ProviderRequest {
        fingerprint: String,
    },
    ProviderEvent {
        event: ModelEvent,
    },
    ProviderTerminal {
        result: Result<Usage, ProviderError>,
    },
    ToolInvocation {
        call_id: String,
        name: String,
        arguments: serde_json::Value,
    },
    ToolEvent {
        event: ToolEvent,
    },
    ToolOutcome {
        call_id: String,
        state: TerminalState,
        #[serde(with = "option_base64_bytes")]
        output: Option<Vec<u8>>,
        attempts: u32,
        failure: Option<ToolFailure>,
    },
    Clock {
        millis: i64,
    },
    Random {
        byte: u8,
    },
}
impl TraceStep {
    fn kind(&self) -> &'static str {
        match self {
            Self::ProviderRequest { .. } => "provider_request",
            Self::ProviderEvent { .. } => "provider_event",
            Self::ProviderTerminal { .. } => "provider_terminal",
            Self::ToolInvocation { .. } => "tool_invocation",
            Self::ToolEvent { .. } => "tool_event",
            Self::ToolOutcome { .. } => "tool_outcome",
            Self::Clock { .. } => "clock",
            Self::Random { .. } => "random",
        }
    }
}
#[derive(Clone, Copy, Debug, Eq, Hash, Ord, PartialEq, PartialOrd, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum JourneyKind {
    Direct,
    Planned,
    Delegated,
    Reviewed,
    ToolUsing,
    Recovery,
    Failure,
}
#[derive(Clone, Copy, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum ReplayOutcome {
    Completed,
    ToolUse,
    Rejected,
    Failed,
}
#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct CandidateOutcome {
    pub outcome: ReplayOutcome,
    #[serde(with = "base64_bytes")]
    pub output: Vec<u8>,
    pub tokens: u64,
    pub latency_ms: u64,
    pub operations: u64,
}
#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct ExpectedOutcome {
    pub outcome: ReplayOutcome,
    pub output_digest: String,
}
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum ReplayVerdict {
    Improved,
    Equivalent,
    Regressed,
    Inconclusive,
}
#[derive(Clone, Debug, Eq, PartialEq)]
pub struct ReplayMeasurement {
    pub outcome: ReplayOutcome,
    pub digest: String,
    pub tokens: u64,
    pub latency_ms: u64,
    pub operations: u64,
}

#[derive(Debug, Error)]
pub enum CorpusError {
    #[error("evaluation corpus is invalid: {0}")]
    Invalid(String),
    #[error("replay tape expected {expected} at step {position}, found {found}")]
    Order {
        position: usize,
        expected: &'static str,
        found: &'static str,
    },
    #[error("replay tape ended at step {position}; expected {expected}")]
    Eof {
        position: usize,
        expected: &'static str,
    },
    #[error("replay tape has {remaining} unconsumed step(s)")]
    Exhaustion { remaining: usize },
    #[error("candidate replay failed: {0}")]
    Candidate(String),
    #[error("evaluation corpus encoding failed: {0}")]
    Json(#[from] serde_json::Error),
    #[error("evaluation corpus storage failed: {0}")]
    Io(#[from] std::io::Error),
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
struct RuntimeCorpusRegistration {
    sha256: String,
    bytes: u64,
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
struct RuntimeCorpusRegistry {
    version: u32,
    copies: BTreeMap<String, RuntimeCorpusRegistration>,
}

/// Truthful inventory of immutable built-in corpus data and optional installation-owned copies.
#[derive(Clone, Debug, Eq, PartialEq)]
pub struct CorpusDataInventory {
    pub registry_root: PathBuf,
    pub embedded_sha256: String,
    pub embedded_immutable: bool,
    pub owned_copy_ids: Vec<String>,
    pub relative_files: Vec<PathBuf>,
}

/// Registers a validated public corpus copy as installation-owned runtime data.
///
/// The checked-in corpus remains embedded in the executable and is never made deletable by this
/// operation; only the explicit copy and its registry entry become data-control scope.
///
/// # Errors
/// Returns an error for unsafe identity, private/invalid corpus content, or durable I/O failure.
pub fn register_runtime_corpus_copy(
    root: impl AsRef<Path>,
    copy_id: &str,
    bytes: &[u8],
) -> Result<(), CorpusError> {
    validate_copy_id(copy_id)?;
    TraceReplay::from_bytes(bytes)?;
    fs::create_dir_all(root.as_ref().join(RUNTIME_CORPUS_COPIES_DIR))?;
    let root = fs::canonicalize(root.as_ref())?;
    let mut registry = load_runtime_corpus_registry(&root)?;
    let sha256 = hex(&Sha256::digest(bytes));
    let byte_count = u64::try_from(bytes.len())
        .map_err(|_| CorpusError::Invalid("runtime corpus is too large".into()))?;
    if let Some(existing) = registry.copies.get(copy_id) {
        if existing.sha256 == sha256
            && existing.bytes == byte_count
            && fs::read(
                root.join(RUNTIME_CORPUS_COPIES_DIR)
                    .join(format!("{copy_id}.json")),
            )? == bytes
        {
            return Ok(());
        }
        return Err(CorpusError::Invalid(
            "runtime corpus identity was reused with different content".into(),
        ));
    }
    let copy_path = root
        .join(RUNTIME_CORPUS_COPIES_DIR)
        .join(format!("{copy_id}.json"));
    let mut file = OpenOptions::new()
        .create_new(true)
        .write(true)
        .open(&copy_path)?;
    file.write_all(bytes)?;
    file.sync_all()?;
    registry.copies.insert(
        copy_id.into(),
        RuntimeCorpusRegistration {
            sha256,
            bytes: byte_count,
        },
    );
    if let Err(error) = store_runtime_corpus_registry(&root, &registry) {
        let _ = fs::remove_file(copy_path);
        return Err(error);
    }
    Ok(())
}

/// Reads the runtime corpus registry and reports only files it authoritatively owns.
///
/// # Errors
/// Returns an error for registry corruption, unknown remnants, symlinks, or digest mismatch.
pub fn corpus_data_inventory(root: impl AsRef<Path>) -> Result<CorpusDataInventory, CorpusError> {
    let requested = root.as_ref();
    let embedded_sha256 = hex(&Sha256::digest(CORPUS_BYTES));
    if !requested.exists() {
        return Ok(CorpusDataInventory {
            registry_root: requested.to_path_buf(),
            embedded_sha256,
            embedded_immutable: true,
            owned_copy_ids: Vec::new(),
            relative_files: Vec::new(),
        });
    }
    let root = fs::canonicalize(requested)?;
    let registry_path = root.join(RUNTIME_CORPUS_REGISTRY_FILE);
    if !registry_path.exists() {
        let copies = root.join(RUNTIME_CORPUS_COPIES_DIR);
        let copies_empty = !copies.exists() || fs::read_dir(&copies)?.next().is_none();
        let only_empty_copies = fs::read_dir(&root)?
            .all(|entry| entry.is_ok_and(|entry| entry.file_name() == RUNTIME_CORPUS_COPIES_DIR));
        if copies_empty && only_empty_copies {
            return Ok(CorpusDataInventory {
                registry_root: root,
                embedded_sha256,
                embedded_immutable: true,
                owned_copy_ids: Vec::new(),
                relative_files: Vec::new(),
            });
        }
        return Err(CorpusError::Invalid(
            "runtime corpus data exists without registry ownership".into(),
        ));
    }
    let registry = load_runtime_corpus_registry(&root)?;
    let copies_root = root.join(RUNTIME_CORPUS_COPIES_DIR);
    let mut expected = BTreeSet::new();
    let mut relative_files = vec![PathBuf::from(RUNTIME_CORPUS_REGISTRY_FILE)];
    for (id, registration) in &registry.copies {
        validate_copy_id(id)?;
        let relative = PathBuf::from(RUNTIME_CORPUS_COPIES_DIR).join(format!("{id}.json"));
        let path = root.join(&relative);
        let metadata = fs::symlink_metadata(&path)?;
        if !metadata.is_file()
            || metadata.file_type().is_symlink()
            || metadata.len() != registration.bytes
            || hex(&Sha256::digest(fs::read(&path)?)) != registration.sha256
        {
            return Err(CorpusError::Invalid(format!(
                "runtime corpus copy {id} does not match its registry"
            )));
        }
        expected.insert(format!("{id}.json"));
        relative_files.push(relative);
    }
    let mut actual = BTreeSet::new();
    for entry in fs::read_dir(&copies_root)? {
        let entry = entry?;
        let kind = entry.file_type()?;
        if kind.is_symlink() || !kind.is_file() {
            return Err(CorpusError::Invalid(
                "runtime corpus directory contains an unsupported entry".into(),
            ));
        }
        actual.insert(entry.file_name().to_string_lossy().into_owned());
    }
    if actual != expected {
        return Err(CorpusError::Invalid(
            "runtime corpus registry omits owned files".into(),
        ));
    }
    relative_files.sort();
    Ok(CorpusDataInventory {
        registry_root: root,
        embedded_sha256,
        embedded_immutable: true,
        owned_copy_ids: registry.copies.keys().cloned().collect(),
        relative_files,
    })
}

fn validate_copy_id(copy_id: &str) -> Result<(), CorpusError> {
    let path = Path::new(copy_id);
    if copy_id.is_empty()
        || copy_id.len() > 128
        || path
            .components()
            .any(|part| !matches!(part, Component::Normal(_)))
        || !copy_id
            .bytes()
            .all(|byte| byte.is_ascii_alphanumeric() || matches!(byte, b'-' | b'_'))
    {
        return Err(CorpusError::Invalid(
            "runtime corpus copy identity is unsafe".into(),
        ));
    }
    Ok(())
}

fn load_runtime_corpus_registry(root: &Path) -> Result<RuntimeCorpusRegistry, CorpusError> {
    let path = root.join(RUNTIME_CORPUS_REGISTRY_FILE);
    if !path.exists() {
        return Ok(RuntimeCorpusRegistry {
            version: RUNTIME_CORPUS_REGISTRY_VERSION,
            copies: BTreeMap::new(),
        });
    }
    let metadata = fs::symlink_metadata(&path)?;
    if !metadata.is_file() || metadata.file_type().is_symlink() {
        return Err(CorpusError::Invalid(
            "runtime corpus registry is not a regular file".into(),
        ));
    }
    let registry: RuntimeCorpusRegistry = serde_json::from_slice(&fs::read(path)?)?;
    if registry.version != RUNTIME_CORPUS_REGISTRY_VERSION {
        return Err(CorpusError::Invalid(
            "runtime corpus registry version is unsupported".into(),
        ));
    }
    Ok(registry)
}

fn store_runtime_corpus_registry(
    root: &Path,
    registry: &RuntimeCorpusRegistry,
) -> Result<(), CorpusError> {
    let temporary = root.join(".registry.json.tmp");
    let bytes = serde_json::to_vec(registry)?;
    let mut file = OpenOptions::new()
        .create(true)
        .truncate(true)
        .write(true)
        .open(&temporary)?;
    file.write_all(&bytes)?;
    file.sync_all()?;
    keith_platform::replace_file(&temporary, &root.join(RUNTIME_CORPUS_REGISTRY_FILE))?;
    std::fs::File::open(root)?.sync_all()?;
    Ok(())
}

pub struct ReplayTape {
    steps: RecordedTape<TraceStep>,
    position: usize,
    last_clock: Option<i64>,
    open_tools: BTreeSet<String>,
}
#[allow(clippy::missing_errors_doc)]
impl ReplayTape {
    fn take(&mut self, expected: &'static str) -> Result<TraceStep, CorpusError> {
        let Some(found) = self.steps.take_next() else {
            return Err(CorpusError::Eof {
                position: self.position,
                expected,
            });
        };
        if found.kind() != expected {
            return Err(CorpusError::Order {
                position: self.position,
                expected,
                found: found.kind(),
            });
        }
        self.position += 1;
        Ok(found)
    }
    fn next_provider_request(&mut self) -> Result<String, CorpusError> {
        match self.take("provider_request")? {
            TraceStep::ProviderRequest { fingerprint } => Ok(fingerprint),
            _ => unreachable!(),
        }
    }
    pub fn expect_provider_request(&mut self, actual_fingerprint: &str) -> Result<(), CorpusError> {
        let recorded = self.next_provider_request()?;
        if recorded != actual_fingerprint {
            return Err(CorpusError::Candidate(
                "provider request fingerprint differs from recording".into(),
            ));
        }
        Ok(())
    }
    pub fn next_provider_event(&mut self) -> Result<ModelEvent, CorpusError> {
        match self.take("provider_event")? {
            TraceStep::ProviderEvent { event } => Ok(event),
            _ => unreachable!(),
        }
    }
    pub fn next_provider_terminal(&mut self) -> Result<Result<Usage, ProviderError>, CorpusError> {
        match self.take("provider_terminal")? {
            TraceStep::ProviderTerminal { result } => Ok(result),
            _ => unreachable!(),
        }
    }
    fn next_tool_invocation(&mut self) -> Result<(String, String, serde_json::Value), CorpusError> {
        match self.take("tool_invocation")? {
            TraceStep::ToolInvocation {
                call_id,
                name,
                arguments,
            } => {
                if !self.open_tools.insert(call_id.clone()) {
                    return Err(CorpusError::Candidate("duplicate tool invocation".into()));
                }
                Ok((call_id, name, arguments))
            }
            _ => unreachable!(),
        }
    }
    pub fn expect_tool_invocation(
        &mut self,
        actual_call_id: &str,
        actual_name: &str,
        actual_arguments: &serde_json::Value,
    ) -> Result<(), CorpusError> {
        let (call_id, name, arguments) = self.next_tool_invocation()?;
        if call_id != actual_call_id || name != actual_name || arguments != *actual_arguments {
            return Err(CorpusError::Candidate(
                "tool invocation differs from recording".into(),
            ));
        }
        Ok(())
    }
    pub fn next_tool_event(&mut self) -> Result<ToolEvent, CorpusError> {
        match self.take("tool_event")? {
            TraceStep::ToolEvent { event } => Ok(event),
            _ => unreachable!(),
        }
    }
    pub fn next_tool_outcome(&mut self) -> Result<(String, ToolOutcome), CorpusError> {
        match self.take("tool_outcome")? {
            TraceStep::ToolOutcome {
                call_id,
                state,
                output,
                attempts,
                failure,
            } => {
                if !self.open_tools.remove(&call_id) {
                    return Err(CorpusError::Candidate(
                        "tool outcome has no unique open invocation".into(),
                    ));
                }
                Ok((
                    call_id,
                    ToolOutcome {
                        state,
                        output,
                        attempts,
                        failure,
                    },
                ))
            }
            _ => unreachable!(),
        }
    }
    pub fn next_clock_millis(&mut self) -> Result<i64, CorpusError> {
        match self.take("clock")? {
            TraceStep::Clock { millis } => {
                if self.last_clock.is_some_and(|previous| millis < previous) {
                    return Err(CorpusError::Candidate("recorded clock regressed".into()));
                }
                self.last_clock = Some(millis);
                Ok(millis)
            }
            _ => unreachable!(),
        }
    }
    pub fn next_random_byte(&mut self) -> Result<u8, CorpusError> {
        match self.take("random")? {
            TraceStep::Random { byte } => Ok(byte),
            _ => unreachable!(),
        }
    }
    #[must_use]
    pub fn peek_kind(&self) -> Option<&'static str> {
        self.steps.clone().take_next().map(|s| s.kind())
    }
    fn finish(self, o: &CandidateOutcome) -> Result<ReplayMeasurement, CorpusError> {
        if !self.steps.is_exhausted() {
            return Err(CorpusError::Exhaustion {
                remaining: self.steps.remaining(),
            });
        }
        if !self.open_tools.is_empty() {
            return Err(CorpusError::Candidate(
                "tool invocation lacks an outcome".into(),
            ));
        }
        if o.output.len() > MAX_OUTPUT_BYTES
            || o.tokens > MAX_TOKENS
            || o.latency_ms > MAX_LATENCY_MS
            || o.operations > MAX_OPERATIONS
        {
            return Err(CorpusError::Candidate(
                "candidate measurement exceeds replay bound".into(),
            ));
        }
        Ok(ReplayMeasurement {
            outcome: o.outcome,
            digest: hex(&Sha256::digest(&o.output)),
            tokens: o.tokens,
            latency_ms: o.latency_ms,
            operations: o.operations,
        })
    }
}

pub struct TraceReplay {
    corpus: EvaluationCorpus,
}
#[allow(clippy::missing_errors_doc)]
impl TraceReplay {
    pub fn checked_in() -> Result<Self, CorpusError> {
        Self::from_bytes(CORPUS_BYTES)
    }
    pub fn from_bytes(bytes: &[u8]) -> Result<Self, CorpusError> {
        scan_private(bytes)?;
        let value = serde_json::from_slice(bytes)?;
        ensure_public_fixture(&value)
            .map_err(|_| CorpusError::Invalid("fixture contains private content".into()))?;
        let corpus = serde_json::from_value(value)?;
        validate(&corpus)?;
        Ok(Self { corpus })
    }
    #[must_use]
    pub const fn corpus(&self) -> &EvaluationCorpus {
        &self.corpus
    }
    pub fn replay<F>(
        &self,
        id: &str,
        mut candidate: F,
    ) -> Result<(ReplayMeasurement, ReplayVerdict), CorpusError>
    where
        F: FnMut(&mut ReplayTape) -> Result<CandidateOutcome, CorpusError>,
    {
        let j = self
            .corpus
            .journeys
            .iter()
            .find(|j| j.id == id)
            .ok_or_else(|| CorpusError::Invalid("journey is absent".into()))?;
        let first = run_once(j, &mut candidate);
        let second = run_once(j, &mut candidate);
        match (first, second) {
            (Ok(a), Ok(b)) if a == b => {
                let v = self.compare(j, &a, &a);
                Ok((a, v))
            }
            (Err(first), Err(second)) if first.to_string() == second.to_string() => Err(first),
            _ => Ok((
                ReplayMeasurement {
                    outcome: ReplayOutcome::Failed,
                    digest: hex(&Sha256::digest([])),
                    tokens: 0,
                    latency_ms: 0,
                    operations: 0,
                },
                ReplayVerdict::Inconclusive,
            )),
        }
    }
    #[must_use]
    pub fn compare(
        &self,
        j: &EvaluationJourney,
        a: &ReplayMeasurement,
        b: &ReplayMeasurement,
    ) -> ReplayVerdict {
        if a != b {
            return ReplayVerdict::Inconclusive;
        }
        if a.outcome != j.expected.outcome || a.digest != j.expected.output_digest {
            return ReplayVerdict::Regressed;
        }
        let primary = compare_regression(
            j.baseline_tokens,
            j.baseline_latency_ms,
            a.tokens,
            a.latency_ms,
        );
        let comparison = if a.operations > j.baseline_operations {
            RegressionComparison::Regressed
        } else if a.operations < j.baseline_operations
            && primary == RegressionComparison::Equivalent
        {
            RegressionComparison::Improved
        } else {
            primary
        };
        match comparison {
            RegressionComparison::Improved => ReplayVerdict::Improved,
            RegressionComparison::Equivalent => ReplayVerdict::Equivalent,
            RegressionComparison::Regressed => ReplayVerdict::Regressed,
        }
    }
}
fn run_once<F>(j: &EvaluationJourney, c: &mut F) -> Result<ReplayMeasurement, CorpusError>
where
    F: FnMut(&mut ReplayTape) -> Result<CandidateOutcome, CorpusError>,
{
    let mut t = ReplayTape {
        steps: RecordedTape::new(j.trace.clone()),
        position: 0,
        last_clock: None,
        open_tools: BTreeSet::new(),
    };
    let o = c(&mut t)?;
    t.finish(&o)
}

#[allow(clippy::too_many_lines)]
fn validate(c: &EvaluationCorpus) -> Result<(), CorpusError> {
    let kinds = c.journeys.iter().map(|j| j.kind).collect::<BTreeSet<_>>();
    let ids = c.journeys.iter().map(|j| &j.id).collect::<BTreeSet<_>>();
    let actual = hex(&Sha256::digest(serde_json::to_vec(&c.journeys)?));
    if c.version != 1
        || c.content_sha256 != actual
        || c.journeys.len() != 7
        || kinds.len() != 7
        || ids.len() != 7
    {
        return Err(CorpusError::Invalid(
            "version 1 must contain seven unique, integrity-checked journeys".into(),
        ));
    }
    for j in &c.journeys {
        let mut last_rank = 0_u8;
        for step in &j.trace {
            let rank = match step {
                TraceStep::ProviderRequest { .. } => 0,
                TraceStep::ProviderEvent { .. } => 1,
                TraceStep::ProviderTerminal { .. } => 2,
                TraceStep::ToolInvocation { .. } => 3,
                TraceStep::ToolEvent { .. } => 4,
                TraceStep::ToolOutcome { output, .. } => {
                    if let Some(output) = output {
                        scan_private(output)?;
                    }
                    5
                }
                TraceStep::Clock { .. } => 6,
                TraceStep::Random { .. } => 7,
            };
            if rank < last_rank {
                return Err(CorpusError::Invalid(format!(
                    "journey {} has a non-monotonic lifecycle",
                    j.id
                )));
            }
            last_rank = rank;
        }
        let fp = j
            .trace
            .iter()
            .filter_map(|s| {
                if let TraceStep::ProviderRequest { fingerprint } = s {
                    Some(fingerprint)
                } else {
                    None
                }
            })
            .collect::<Vec<_>>();
        let events = j
            .trace
            .iter()
            .filter_map(|s| {
                if let TraceStep::ProviderEvent { event } = s {
                    Some(event)
                } else {
                    None
                }
            })
            .collect::<Vec<_>>();
        let usage = events
            .iter()
            .filter(|e| matches!(e, ModelEvent::Usage { .. }))
            .count();
        let finished = events
            .iter()
            .filter(|e| matches!(e, ModelEvent::Finished { .. }))
            .count();
        let terminals = j
            .trace
            .iter()
            .filter_map(|step| {
                if let TraceStep::ProviderTerminal { result } = step {
                    Some(result)
                } else {
                    None
                }
            })
            .collect::<Vec<_>>();
        let clock_values = j
            .trace
            .iter()
            .filter_map(|s| match s {
                TraceStep::Clock { millis } => Some(*millis),
                _ => None,
            })
            .collect::<Vec<_>>();
        let random = j
            .trace
            .iter()
            .filter(|s| matches!(s, TraceStep::Random { .. }))
            .count();
        let digest = |s: &str| s.len() == 64 && s.bytes().all(|b| b.is_ascii_hexdigit());
        if j.id.trim().is_empty()
            || fp.len() != 1
            || !digest(fp[0])
            || usage != 1
            || finished != 1
            || terminals.len() != 1
            || terminals[0].as_ref().is_ok_and(|terminal| {
                !events
                    .iter()
                    .any(|event| matches!(event, ModelEvent::Usage { usage } if usage == terminal))
            })
            || !matches!(events.last(), Some(ModelEvent::Finished { .. }))
            || clock_values.len() < 2
            || clock_values.windows(2).any(|pair| pair[1] < pair[0])
            || random == 0
            || !digest(&j.expected.output_digest)
            || j.baseline_tokens == 0
            || j.baseline_operations == 0
        {
            return Err(CorpusError::Invalid(format!(
                "journey {} has an incomplete provider trace",
                j.id
            )));
        }
        let inv = j
            .trace
            .iter()
            .filter_map(|s| match s {
                TraceStep::ToolInvocation { call_id, .. } => Some(call_id),
                _ => None,
            })
            .collect::<Vec<_>>();
        let out = j
            .trace
            .iter()
            .filter_map(|s| match s {
                TraceStep::ToolOutcome { call_id, .. } => Some(call_id),
                _ => None,
            })
            .collect::<Vec<_>>();
        if inv.len() != out.len()
            || inv.iter().collect::<BTreeSet<_>>().len() != inv.len()
            || out.iter().collect::<BTreeSet<_>>().len() != out.len()
            || inv
                .iter()
                .zip(&out)
                .any(|(invocation, outcome)| invocation != outcome)
        {
            return Err(CorpusError::Invalid(format!(
                "journey {} lacks a full tool outcome",
                j.id
            )));
        }
    }
    Ok(())
}
fn scan_private(bytes: &[u8]) -> Result<(), CorpusError> {
    let s = String::from_utf8_lossy(bytes).to_ascii_lowercase();
    if [
        "authorization:",
        "bearer ",
        "api_key",
        "api-key",
        "password=",
        "secret=",
        "personal memory",
        "private user",
        "private reasoning",
        "-----begin private key-----",
    ]
    .iter()
    .any(|m| s.contains(m))
    {
        return Err(CorpusError::Invalid("private or credential content".into()));
    }
    Ok(())
}
mod base64_bytes {
    use base64::{Engine as _, engine::general_purpose::STANDARD};
    use serde::{Deserialize, Deserializer, Serializer};

    pub fn serialize<S: Serializer>(bytes: &[u8], serializer: S) -> Result<S::Ok, S::Error> {
        serializer.serialize_str(&STANDARD.encode(bytes))
    }
    pub fn deserialize<'de, D: Deserializer<'de>>(deserializer: D) -> Result<Vec<u8>, D::Error> {
        let encoded = String::deserialize(deserializer)?;
        STANDARD.decode(encoded).map_err(serde::de::Error::custom)
    }
}
mod option_base64_bytes {
    use base64::{Engine as _, engine::general_purpose::STANDARD};
    use serde::{Deserialize, Deserializer, Serialize, Serializer};

    #[allow(clippy::ref_option)]
    pub fn serialize<S: Serializer>(
        bytes: &Option<Vec<u8>>,
        serializer: S,
    ) -> Result<S::Ok, S::Error> {
        bytes
            .as_ref()
            .map(|value| STANDARD.encode(value))
            .serialize(serializer)
    }
    pub fn deserialize<'de, D: Deserializer<'de>>(
        deserializer: D,
    ) -> Result<Option<Vec<u8>>, D::Error> {
        Option::<String>::deserialize(deserializer)?
            .map(|encoded| STANDARD.decode(encoded).map_err(serde::de::Error::custom))
            .transpose()
    }
}
#[cfg(test)]
fn recorded_candidate(t: &mut ReplayTape) -> Result<CandidateOutcome, CorpusError> {
    let _ = t.next_provider_request()?;
    let mut outcome = ReplayOutcome::Failed;
    let mut output = Vec::new();
    let mut tokens = 0;
    let mut operations = 0;
    while t.peek_kind() == Some("provider_event") {
        match t.next_provider_event()? {
            ModelEvent::TextDelta { text } => output.extend_from_slice(text.as_bytes()),
            ModelEvent::Usage { usage } => tokens = usage.total_tokens(),
            ModelEvent::Finished { reason } => {
                outcome = match reason {
                    StopReason::EndTurn => ReplayOutcome::Completed,
                    StopReason::ToolUse => ReplayOutcome::ToolUse,
                    StopReason::ContentRejected => ReplayOutcome::Rejected,
                    _ => ReplayOutcome::Failed,
                };
            }
            _ => {}
        }
    }
    let terminal = t.next_provider_terminal()?;
    if let Ok(usage) = terminal {
        tokens = usage.total_tokens();
    }
    let mut first_clock = None;
    let mut last_clock = None;
    while let Some(k) = t.peek_kind() {
        match k {
            "tool_invocation" => {
                let _ = t.next_tool_invocation()?;
                operations += 1;
            }
            "tool_event" => {
                let _ = t.next_tool_event()?;
            }
            "tool_outcome" => {
                let (_, tool_outcome) = t.next_tool_outcome()?;
                if let Some(bytes) = tool_outcome.output {
                    output.extend_from_slice(&bytes);
                }
            }
            "clock" => {
                let now = t.next_clock_millis()?;
                first_clock.get_or_insert(now);
                last_clock = Some(now);
            }
            "random" => {
                let _ = t.next_random_byte()?;
            }
            _ => unreachable!(),
        }
    }
    let latency_ms = last_clock
        .unwrap_or_default()
        .checked_sub(first_clock.unwrap_or_default())
        .ok_or_else(|| CorpusError::Candidate("recorded clock regressed".into()))?
        .try_into()
        .map_err(|_| CorpusError::Candidate("recorded latency is invalid".into()))?;
    Ok(CandidateOutcome {
        outcome,
        output,
        tokens,
        latency_ms,
        operations: operations + 1,
    })
}
fn hex(bytes: &[u8]) -> String {
    bytes.iter().fold(String::new(), |mut s, b| {
        use std::fmt::Write as _;
        let _ = write!(s, "{b:02x}");
        s
    })
}

#[cfg(test)]
mod tests {
    use super::*;
    fn tape(steps: impl IntoIterator<Item = TraceStep>) -> ReplayTape {
        ReplayTape {
            steps: RecordedTape::new(steps),
            position: 0,
            last_clock: None,
            open_tools: BTreeSet::new(),
        }
    }
    fn candidate(outcome: ReplayOutcome) -> CandidateOutcome {
        CandidateOutcome {
            outcome,
            output: Vec::new(),
            tokens: 0,
            latency_ms: 0,
            operations: 0,
        }
    }
    fn encode(mut corpus: EvaluationCorpus) -> Vec<u8> {
        corpus.content_sha256 = hex(&Sha256::digest(
            serde_json::to_vec(&corpus.journeys).unwrap(),
        ));
        serde_json::to_vec(&corpus).unwrap()
    }
    #[test]
    fn corpus_valid() {
        let r = TraceReplay::checked_in().unwrap();
        assert_eq!(r.corpus().journeys.len(), 7);
        assert_eq!(
            r.corpus()
                .journeys
                .iter()
                .map(|journey| journey.kind)
                .collect::<BTreeSet<_>>(),
            BTreeSet::from([
                JourneyKind::Direct,
                JourneyKind::Planned,
                JourneyKind::Delegated,
                JourneyKind::Reviewed,
                JourneyKind::ToolUsing,
                JourneyKind::Recovery,
                JourneyKind::Failure,
            ])
        );
    }
    #[test]
    fn exact_replay() {
        let r = TraceReplay::checked_in().unwrap();
        for j in &r.corpus().journeys {
            let (m, v) = r.replay(&j.id, recorded_candidate).unwrap();
            assert_eq!(m.outcome, j.expected.outcome);
            assert_eq!(m.digest, j.expected.output_digest);
            assert_eq!(m.tokens, j.baseline_tokens);
            assert_eq!(m.latency_ms, j.baseline_latency_ms);
            assert_eq!(m.operations, j.baseline_operations);
            assert_eq!(v, ReplayVerdict::Equivalent);
        }
    }
    #[test]
    fn seven_journey_branches_have_recorded_semantics() {
        let replay = TraceReplay::checked_in().unwrap();
        let branches = replay
            .corpus()
            .journeys
            .iter()
            .map(|journey| (journey.kind, journey.expected.outcome))
            .collect::<Vec<_>>();
        assert_eq!(branches.len(), 7);
        assert!(branches.contains(&(JourneyKind::Direct, ReplayOutcome::Completed)));
        assert!(branches.contains(&(JourneyKind::Planned, ReplayOutcome::Completed)));
        assert!(branches.contains(&(JourneyKind::Delegated, ReplayOutcome::Completed)));
        assert!(branches.contains(&(JourneyKind::Reviewed, ReplayOutcome::Completed)));
        assert!(branches.contains(&(JourneyKind::ToolUsing, ReplayOutcome::ToolUse)));
        assert!(branches.contains(&(JourneyKind::Recovery, ReplayOutcome::Completed)));
        assert!(branches.contains(&(JourneyKind::Failure, ReplayOutcome::Rejected)));
    }

    #[test]
    fn candidate_measurements_drive_real_improvement_and_regression_verdicts() {
        let replay = TraceReplay::checked_in().unwrap();
        let (_, improved) = replay
            .replay("direct-v1", |tape| {
                let mut result = recorded_candidate(tape)?;
                result.tokens -= 1;
                Ok(result)
            })
            .unwrap();
        assert_eq!(improved, ReplayVerdict::Improved);

        let (_, regressed) = replay
            .replay("direct-v1", |tape| {
                let mut result = recorded_candidate(tape)?;
                result.operations += 1;
                Ok(result)
            })
            .unwrap();
        assert_eq!(regressed, ReplayVerdict::Regressed);
    }
    #[test]
    fn order_eof_exhaustion() {
        let r = TraceReplay::checked_in().unwrap();
        assert!(matches!(
            r.replay("direct-v1", |t| {
                t.next_clock_millis()?;
                unreachable!()
            }),
            Err(CorpusError::Order { .. })
        ));
        assert!(matches!(
            r.replay("direct-v1", |_| Ok(candidate(ReplayOutcome::Completed))),
            Err(CorpusError::Exhaustion { .. })
        ));
        let mut t = tape([]);
        assert!(matches!(t.next_random_byte(), Err(CorpusError::Eof { .. })));
    }
    #[test]
    fn nondeterminism() {
        let r = TraceReplay::checked_in().unwrap();
        let mut n = 0;
        let (_, v) = r
            .replay("direct-v1", |t| {
                n += 1;
                let mut o = recorded_candidate(t)?;
                if n == 2 {
                    o.outcome = ReplayOutcome::Failed;
                }
                Ok(o)
            })
            .unwrap();
        assert_eq!(v, ReplayVerdict::Inconclusive);
    }

    #[test]
    fn typed_run_error_divergence_is_inconclusive() {
        let r = TraceReplay::checked_in().unwrap();
        let mut run = 0;
        let (_, verdict) = r
            .replay("direct-v1", |t| {
                run += 1;
                if run == 1 {
                    return Err(CorpusError::Candidate("first".into()));
                }
                t.next_clock_millis()?;
                unreachable!()
            })
            .unwrap();
        assert_eq!(verdict, ReplayVerdict::Inconclusive);

        let mut run = 0;
        let (_, verdict) = r
            .replay("direct-v1", |_| {
                run += 1;
                Err(CorpusError::Candidate(format!("payload-{run}")))
            })
            .unwrap();
        assert_eq!(verdict, ReplayVerdict::Inconclusive);
    }

    #[test]
    fn identity_mismatch_overread_and_tampering_are_rejected() {
        let r = TraceReplay::checked_in().unwrap();
        assert!(matches!(
            r.replay("direct-v1", |t| {
                t.expect_provider_request(&"0".repeat(64))?;
                unreachable!()
            }),
            Err(CorpusError::Candidate(_))
        ));

        let mut corpus = r.corpus().clone();
        corpus.journeys[0].baseline_tokens += 1;
        let bytes = serde_json::to_vec(&corpus).unwrap();
        assert!(matches!(
            TraceReplay::from_bytes(&bytes),
            Err(CorpusError::Invalid(_))
        ));

        let mut replay_tape = tape([TraceStep::Random { byte: 1 }]);
        assert_eq!(replay_tape.next_random_byte().unwrap(), 1);
        assert!(matches!(
            replay_tape.next_random_byte(),
            Err(CorpusError::Eof { .. })
        ));

        let mut invocation = tape([TraceStep::ToolInvocation {
            call_id: "recorded-call".into(),
            name: "search".into(),
            arguments: serde_json::json!({"query":"public"}),
        }]);
        assert!(matches!(
            invocation.expect_tool_invocation(
                "different-call",
                "search",
                &serde_json::json!({"query":"public"})
            ),
            Err(CorpusError::Candidate(_))
        ));
    }

    #[test]
    fn expected_digest_mismatch_and_real_performance_regression_are_reported() {
        let r = TraceReplay::checked_in().unwrap();
        let journey = &r.corpus().journeys[0];
        let baseline = ReplayMeasurement {
            outcome: journey.expected.outcome,
            digest: journey.expected.output_digest.clone(),
            tokens: journey.baseline_tokens,
            latency_ms: journey.baseline_latency_ms,
            operations: journey.baseline_operations,
        };
        let mismatch = ReplayMeasurement {
            digest: "0".repeat(64),
            ..baseline.clone()
        };
        assert_eq!(
            r.compare(journey, &mismatch, &mismatch),
            ReplayVerdict::Regressed
        );
        let slower = ReplayMeasurement {
            latency_ms: baseline.latency_ms + 1,
            ..baseline
        };
        assert_eq!(
            r.compare(journey, &slower, &slower),
            ReplayVerdict::Regressed
        );
    }

    #[test]
    fn tool_success_and_failure_outputs_are_semantically_recorded() {
        for (state, failure) in [
            (TerminalState::Succeeded, None),
            (
                TerminalState::Failed,
                Some(ToolFailure::execution("recorded failure", false)),
            ),
        ] {
            let mut replay_tape = tape([
                TraceStep::ToolInvocation {
                    call_id: "call".into(),
                    name: "search".into(),
                    arguments: serde_json::json!({}),
                },
                TraceStep::ToolOutcome {
                    call_id: "call".into(),
                    state,
                    output: Some(b"semantic".to_vec()),
                    attempts: 1,
                    failure,
                },
            ]);
            let _ = replay_tape.next_tool_invocation().unwrap();
            let (_, outcome) = replay_tape.next_tool_outcome().unwrap();
            assert_eq!(outcome.state, state);
            assert_eq!(outcome.output, Some(b"semantic".to_vec()));
            let measurement = replay_tape
                .finish(&CandidateOutcome {
                    outcome: ReplayOutcome::ToolUse,
                    output: b"semantic".to_vec(),
                    tokens: 1,
                    latency_ms: 1,
                    operations: 1,
                })
                .unwrap();
            assert_eq!(measurement.digest, hex(&Sha256::digest(b"semantic")));
        }
        let mut replay_tape = tape([
            TraceStep::ToolInvocation {
                call_id: "none".into(),
                name: "search".into(),
                arguments: serde_json::json!({}),
            },
            TraceStep::ToolOutcome {
                call_id: "none".into(),
                state: TerminalState::Succeeded,
                output: None,
                attempts: 1,
                failure: None,
            },
        ]);
        let _ = replay_tape.next_tool_invocation().unwrap();
        assert_eq!(replay_tape.next_tool_outcome().unwrap().1.output, None);
    }

    #[test]
    fn provider_terminal_preserves_typed_success_and_failure() {
        let failure = ProviderError::new(
            keith_provider_core::ProviderErrorKind::Unavailable,
            "recorded provider failure",
        );
        for terminal in [
            Ok(Usage {
                input_tokens: 2,
                output_tokens: 3,
                cached_input_tokens: 0,
            }),
            Err(failure),
        ] {
            let mut replay_tape = tape([TraceStep::ProviderTerminal {
                result: terminal.clone(),
            }]);
            assert_eq!(replay_tape.next_provider_terminal().unwrap(), terminal);
        }
    }

    #[test]
    fn decoded_base64_private_bytes_and_non_monotonic_lifecycle_are_rejected() {
        let r = TraceReplay::checked_in().unwrap();
        let mut private = r.corpus().clone();
        let tool = private
            .journeys
            .iter_mut()
            .flat_map(|j| &mut j.trace)
            .find_map(|step| {
                if let TraceStep::ToolOutcome { output, .. } = step {
                    Some(output)
                } else {
                    None
                }
            })
            .unwrap();
        *tool = Some(b"password=fixture-secret".to_vec());
        assert!(matches!(
            TraceReplay::from_bytes(&encode(private)),
            Err(CorpusError::Invalid(_))
        ));

        let mut unordered = r.corpus().clone();
        unordered.journeys[0].trace.swap(0, 1);
        assert!(matches!(
            TraceReplay::from_bytes(&encode(unordered)),
            Err(CorpusError::Invalid(_))
        ));
    }

    #[test]
    fn reusable_support_tape_remains_available_to_ordinary_tests() {
        let mut tape = RecordedTape::new(["ordinary", "regression"]);
        assert_eq!(tape.take_next(), Some("ordinary"));
        assert_eq!(tape.observed(), &["ordinary"]);
    }
}
