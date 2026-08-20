use std::collections::{BTreeMap, BTreeSet};
use std::fs::{self, OpenOptions};
use std::io::Write;
use std::path::{Path, PathBuf};
use std::sync::{Mutex, MutexGuard};
use std::time::{Duration, Instant};

use keith_agent_types::{
    CURRENT_SCHEMA_VERSION, EntityId, ProfileId, SchemaVersion, SessionId, UtcTimestamp,
    canonical_json_bytes,
};
use keith_provider_core::CancellationToken;
use keith_session_store::{MemoryRecallLink, Sensitivity};
use keith_subagents::{MemoryScoutLimits, MemoryScoutScopeManifest, MemoryScoutSpec};
use serde::{Deserialize, Serialize};
use sha2::{Digest, Sha256};
use thiserror::Error;

use crate::{
    AtlasCoverage, AtlasSearchRequest, EvidenceAuthority, EvidenceRecord, EvidenceValidity,
    MemoryObservatory, ObservatoryError,
};

const RECALL_JOURNAL_PATH: &str = ".keith/memory-recalls.jsonl";
pub const RECALL_SELECTOR_VERSION: &str = "memory-scout-v1";

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct RecallRequest {
    pub query_identity: String,
    pub query: String,
    pub spec: MemoryScoutSpec,
    pub requested_at: UtcTimestamp,
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct RecallClaim {
    pub text: String,
    pub supporting_evidence: Vec<EntityId>,
    pub contradictory_evidence: Vec<EntityId>,
    pub uncertainty: Option<String>,
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct RecallContradiction {
    pub left_evidence: EntityId,
    pub right_evidence: EntityId,
    pub reason: String,
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct RecallCoverage {
    pub scoped_evidence: usize,
    pub analyzed_evidence: usize,
    pub scouts_started: u32,
    pub deepest_level: u16,
    pub peak_concurrency: u16,
    pub search: AtlasCoverage,
    pub truncated: bool,
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct MemoryScoutFinding {
    pub query_identity: String,
    pub archive_revision: u64,
    pub claims: Vec<RecallClaim>,
    pub contradictions: Vec<RecallContradiction>,
    pub coverage: RecallCoverage,
    pub uncertainty: Vec<String>,
    pub unexplored_regions: Vec<String>,
    pub token_price: u64,
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct RecallCapsule {
    pub capsule_id: EntityId,
    pub query_identity: String,
    pub query: String,
    pub profile_id: ProfileId,
    pub calling_session_id: SessionId,
    pub archive_revision: u64,
    pub source_ids: Vec<EntityId>,
    pub source_digests: BTreeMap<EntityId, String>,
    pub session_link: MemoryRecallLink,
    pub claims: Vec<RecallClaim>,
    pub contradictions: Vec<RecallContradiction>,
    pub gaps: Vec<String>,
    pub coverage: RecallCoverage,
    pub selector_version: String,
    pub token_price: u64,
    pub byte_price: usize,
    pub created_at: UtcTimestamp,
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case", tag = "event")]
enum RecallJournalMutation {
    Started {
        request: RecallRequest,
    },
    Completed {
        query_identity: String,
        capsule: Box<RecallCapsule>,
    },
    Failed {
        query_identity: String,
        code: String,
    },
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
struct RecallJournalEvent {
    version: SchemaVersion,
    sequence: u64,
    profile_id: ProfileId,
    previous_digest: Option<String>,
    digest: String,
    recorded_at: UtcTimestamp,
    mutation: RecallJournalMutation,
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize)]
struct RecallJournalDigest<'a> {
    version: SchemaVersion,
    sequence: u64,
    profile_id: &'a ProfileId,
    previous_digest: Option<&'a str>,
    recorded_at: UtcTimestamp,
    mutation: &'a RecallJournalMutation,
}

#[derive(Default)]
struct RecallState {
    events: Vec<RecallJournalEvent>,
    started: BTreeMap<String, RecallRequest>,
    completed: BTreeMap<String, RecallCapsule>,
    failed: BTreeSet<String>,
}

pub struct RecallService {
    root: PathBuf,
    profile_id: ProfileId,
    state: Mutex<RecallState>,
}

#[derive(Clone, Debug, Error, Eq, PartialEq)]
pub enum RecallError {
    #[error("recall I/O failed: {0}")]
    Io(String),
    #[error("recall JSON failed: {0}")]
    Json(String),
    #[error("recall journal is corrupt")]
    CorruptJournal,
    #[error("recall belongs to another profile or revision")]
    ScopeChanged,
    #[error("recall query is empty or no evidence matched")]
    NoEvidence,
    #[error("memory scout contract is invalid")]
    InvalidContract,
    #[error("memory scout result contains unsupported or stale claims")]
    InvalidFinding,
    #[error("memory scout exceeded a hard budget")]
    BudgetExceeded,
    #[error("memory scout was cancelled")]
    Cancelled,
    #[error("memory scout timed out")]
    TimedOut,
    #[error("memory scout state lock was poisoned")]
    LockPoisoned,
    #[error("memory observatory failed: {0}")]
    Observatory(String),
}

impl From<ObservatoryError> for RecallError {
    fn from(error: ObservatoryError) -> Self {
        Self::Observatory(error.to_string())
    }
}

#[allow(clippy::missing_errors_doc)]
impl RecallService {
    pub fn open(root: impl AsRef<Path>, profile_id: &ProfileId) -> Result<Self, RecallError> {
        let root = root.as_ref().to_path_buf();
        fs::create_dir_all(root.join(".keith")).map_err(io_error)?;
        let events = load_journal(&root, profile_id)?;
        let mut state = RecallState {
            events,
            ..RecallState::default()
        };
        project_journal(&mut state)?;
        Ok(Self {
            root,
            profile_id: profile_id.clone(),
            state: Mutex::new(state),
        })
    }

    #[allow(clippy::too_many_arguments)]
    pub fn prepare(
        &self,
        observatory: &MemoryObservatory,
        calling_session_id: &SessionId,
        query: &str,
        sensitivity: Sensitivity,
        limits: MemoryScoutLimits,
        cancellation: &CancellationToken,
        now: UtcTimestamp,
    ) -> Result<RecallRequest, RecallError> {
        limits
            .validate()
            .map_err(|_| RecallError::InvalidContract)?;
        check_cancelled(cancellation)?;
        if query.trim().is_empty() || query.len() > 16 * 1_024 {
            return Err(RecallError::NoEvidence);
        }
        let revision = observatory.revision()?;
        let search_limit = limits
            .max_evidence_per_scout
            .saturating_mul(usize::from(limits.max_children))
            .min(128);
        let (results, _) = observatory.search(&AtlasSearchRequest {
            query: query.to_owned(),
            limit: search_limit,
            max_sensitivity: sensitivity,
            include_disputed: true,
        })?;
        if results.is_empty() {
            return Err(RecallError::NoEvidence);
        }
        let evidence_digests = results
            .into_iter()
            .map(|result| (result.evidence.id, result.evidence.content_digest))
            .collect::<BTreeMap<_, _>>();
        let scope = MemoryScoutScopeManifest {
            profile_id: self.profile_id.clone(),
            calling_session_id: calling_session_id.clone(),
            archive_revision: revision,
            evidence_digests,
            sensitivity_ceiling: sensitivity_name(sensitivity).into(),
            selector_version: RECALL_SELECTOR_VERSION.into(),
        };
        let spec = MemoryScoutSpec::new(scope, limits).map_err(|_| RecallError::InvalidContract)?;
        let query_identity = stable_query_identity(query, &spec)?;
        Ok(RecallRequest {
            query_identity,
            query: query.to_owned(),
            spec,
            requested_at: now,
        })
    }

    pub fn begin(&self, request: &RecallRequest, now: UtcTimestamp) -> Result<(), RecallError> {
        validate_request_identity(request)?;
        let mut state = self.lock()?;
        if state.started.contains_key(&request.query_identity)
            || state.completed.contains_key(&request.query_identity)
            || state.failed.contains(&request.query_identity)
        {
            return Ok(());
        }
        append_mutation(
            &self.root,
            &self.profile_id,
            &mut state,
            RecallJournalMutation::Started {
                request: request.clone(),
            },
            now,
        )?;
        state
            .started
            .insert(request.query_identity.clone(), request.clone());
        Ok(())
    }

    pub fn execute(
        &self,
        observatory: &MemoryObservatory,
        request: &RecallRequest,
        cancellation: &CancellationToken,
        now: UtcTimestamp,
    ) -> Result<RecallCapsule, RecallError> {
        validate_request_identity(request)?;
        let mut state = self.lock()?;
        if let Some(capsule) = state.completed.get(&request.query_identity) {
            return Ok(capsule.clone());
        }
        if state.failed.contains(&request.query_identity) {
            return Err(RecallError::InvalidFinding);
        }
        if let Some(started) = state.started.get(&request.query_identity) {
            if started != request {
                return Err(RecallError::InvalidContract);
            }
        } else {
            append_mutation(
                &self.root,
                &self.profile_id,
                &mut state,
                RecallJournalMutation::Started {
                    request: request.clone(),
                },
                now,
            )?;
            state
                .started
                .insert(request.query_identity.clone(), request.clone());
        }
        let result = run_scout_tree(observatory, request, cancellation, Instant::now())
            .and_then(|finding| self.validate_finding(observatory, request, finding, now));
        match result {
            Ok(capsule) => {
                append_mutation(
                    &self.root,
                    &self.profile_id,
                    &mut state,
                    RecallJournalMutation::Completed {
                        query_identity: request.query_identity.clone(),
                        capsule: Box::new(capsule.clone()),
                    },
                    now,
                )?;
                state.started.remove(&request.query_identity);
                state
                    .completed
                    .insert(request.query_identity.clone(), capsule.clone());
                Ok(capsule)
            }
            Err(error) => {
                append_mutation(
                    &self.root,
                    &self.profile_id,
                    &mut state,
                    RecallJournalMutation::Failed {
                        query_identity: request.query_identity.clone(),
                        code: error_code(&error).into(),
                    },
                    now,
                )?;
                state.started.remove(&request.query_identity);
                state.failed.insert(request.query_identity.clone());
                Err(error)
            }
        }
    }

    pub fn validate_finding(
        &self,
        observatory: &MemoryObservatory,
        request: &RecallRequest,
        finding: MemoryScoutFinding,
        now: UtcTimestamp,
    ) -> Result<RecallCapsule, RecallError> {
        let scope = &request.spec.scope;
        if scope.profile_id != self.profile_id
            || finding.query_identity != request.query_identity
            || finding.archive_revision != scope.archive_revision
            || observatory.revision()? != scope.archive_revision
            || scope.selector_version != RECALL_SELECTOR_VERSION
        {
            return Err(RecallError::ScopeChanged);
        }
        let evidence = observatory.evidence_snapshot()?;
        let sensitivity = parse_sensitivity(&scope.sensitivity_ceiling)?;
        for (id, expected_digest) in &scope.evidence_digests {
            let record = evidence.get(id).ok_or(RecallError::InvalidFinding)?;
            if record.profile_id != scope.profile_id
                || record.content_digest != *expected_digest
                || record.validity == EvidenceValidity::Deleted
                || sensitivity_rank(record.sensitivity) > sensitivity_rank(sensitivity)
            {
                return Err(RecallError::InvalidFinding);
            }
        }
        let allowed = scope
            .evidence_digests
            .keys()
            .cloned()
            .collect::<BTreeSet<_>>();
        if finding.claims.is_empty()
            || finding.claims.len() > request.spec.limits.max_claims
            || finding.coverage.scouts_started > request.spec.limits.max_total_scouts
            || finding.coverage.deepest_level > request.spec.limits.max_depth
            || finding.coverage.peak_concurrency > request.spec.limits.max_concurrency
            || finding.token_price > request.spec.limits.max_tokens
        {
            return Err(RecallError::BudgetExceeded);
        }
        for claim in &finding.claims {
            if claim.text.trim().is_empty()
                || claim.supporting_evidence.is_empty()
                || claim
                    .supporting_evidence
                    .iter()
                    .chain(&claim.contradictory_evidence)
                    .any(|id| !allowed.contains(id))
            {
                return Err(RecallError::InvalidFinding);
            }
        }
        if finding.contradictions.iter().any(|contradiction| {
            !allowed.contains(&contradiction.left_evidence)
                || !allowed.contains(&contradiction.right_evidence)
                || contradiction.left_evidence == contradiction.right_evidence
        }) {
            return Err(RecallError::InvalidFinding);
        }
        let capsule_id = EntityId::new();
        let source_entries = allowed
            .iter()
            .filter_map(|id| evidence.get(id))
            .flat_map(|record| record.source_entries.iter().cloned())
            .collect::<BTreeSet<_>>()
            .into_iter()
            .collect();
        let mut capsule = RecallCapsule {
            capsule_id: capsule_id.clone(),
            query_identity: request.query_identity.clone(),
            query: request.query.clone(),
            profile_id: scope.profile_id.clone(),
            calling_session_id: scope.calling_session_id.clone(),
            archive_revision: scope.archive_revision,
            source_ids: allowed.into_iter().collect(),
            source_digests: scope.evidence_digests.clone(),
            session_link: MemoryRecallLink {
                query_identity: request.query_identity.clone(),
                archive_revision: scope.archive_revision,
                source_entries,
                result_id: capsule_id,
            },
            claims: finding.claims,
            contradictions: finding.contradictions,
            gaps: finding.unexplored_regions,
            coverage: finding.coverage,
            selector_version: scope.selector_version.clone(),
            token_price: finding.token_price,
            byte_price: 0,
            created_at: now,
        };
        capsule.byte_price = canonical_json_bytes(&capsule)
            .map_err(|error| RecallError::Json(error.to_string()))?
            .len();
        if capsule.byte_price > request.spec.limits.max_result_bytes {
            return Err(RecallError::BudgetExceeded);
        }
        Ok(capsule)
    }

    fn lock(&self) -> Result<MutexGuard<'_, RecallState>, RecallError> {
        self.state.lock().map_err(|_| RecallError::LockPoisoned)
    }
}

struct ScoutAccumulator {
    claims: Vec<RecallClaim>,
    contradictions: Vec<RecallContradiction>,
    uncertainty: Vec<String>,
    unexplored: Vec<String>,
    analyzed: usize,
    scouts: u32,
    deepest: u16,
    token_price: u64,
    truncated: bool,
}

fn run_scout_tree(
    observatory: &MemoryObservatory,
    request: &RecallRequest,
    cancellation: &CancellationToken,
    started_at: Instant,
) -> Result<MemoryScoutFinding, RecallError> {
    check_runtime(request, cancellation, started_at)?;
    if observatory.revision()? != request.spec.scope.archive_revision {
        return Err(RecallError::ScopeChanged);
    }
    let sensitivity = parse_sensitivity(&request.spec.scope.sensitivity_ceiling)?;
    let ids = request
        .spec
        .scope
        .evidence_digests
        .keys()
        .cloned()
        .collect::<Vec<_>>();
    let evidence = observatory.evidence(&ids, sensitivity)?;
    let mut accumulator = ScoutAccumulator {
        claims: Vec::new(),
        contradictions: contradictions(&evidence),
        uncertainty: Vec::new(),
        unexplored: Vec::new(),
        analyzed: 0,
        scouts: 0,
        deepest: 0,
        token_price: 0,
        truncated: false,
    };
    scout_partition(
        &evidence,
        1,
        "root",
        request,
        cancellation,
        started_at,
        &mut accumulator,
    )?;
    let search = AtlasCoverage {
        total_active: evidence.len(),
        inspected: evidence.len(),
        matched: evidence.len(),
        returned: accumulator.analyzed,
        truncated: accumulator.truncated,
    };
    Ok(MemoryScoutFinding {
        query_identity: request.query_identity.clone(),
        archive_revision: request.spec.scope.archive_revision,
        claims: accumulator.claims,
        contradictions: accumulator.contradictions,
        coverage: RecallCoverage {
            scoped_evidence: evidence.len(),
            analyzed_evidence: accumulator.analyzed,
            scouts_started: accumulator.scouts,
            deepest_level: accumulator.deepest,
            peak_concurrency: 1,
            search,
            truncated: accumulator.truncated,
        },
        uncertainty: accumulator.uncertainty,
        unexplored_regions: accumulator.unexplored,
        token_price: accumulator.token_price,
    })
}

#[allow(clippy::too_many_arguments)]
fn scout_partition(
    evidence: &[EvidenceRecord],
    depth: u16,
    path: &str,
    request: &RecallRequest,
    cancellation: &CancellationToken,
    started_at: Instant,
    accumulator: &mut ScoutAccumulator,
) -> Result<(), RecallError> {
    check_runtime(request, cancellation, started_at)?;
    if accumulator.scouts >= request.spec.limits.max_total_scouts {
        accumulator.truncated = true;
        accumulator
            .unexplored
            .push(format!("{path}: scout count budget exhausted"));
        return Ok(());
    }
    accumulator.scouts += 1;
    accumulator.deepest = accumulator.deepest.max(depth);
    if evidence.len() > request.spec.limits.max_evidence_per_scout
        && depth < request.spec.limits.max_depth
    {
        let child_count = usize::from(request.spec.limits.max_children).min(evidence.len());
        let chunk_size = evidence.len().div_ceil(child_count);
        for (index, chunk) in evidence.chunks(chunk_size).enumerate() {
            scout_partition(
                chunk,
                depth + 1,
                &format!("{path}.{index}"),
                request,
                cancellation,
                started_at,
                accumulator,
            )?;
        }
        return Ok(());
    }
    if evidence.len() > request.spec.limits.max_evidence_per_scout {
        accumulator.truncated = true;
        accumulator
            .unexplored
            .push(format!("{path}: depth budget exhausted"));
    }
    for record in evidence
        .iter()
        .take(request.spec.limits.max_evidence_per_scout)
    {
        check_runtime(request, cancellation, started_at)?;
        if accumulator.claims.len() >= request.spec.limits.max_claims {
            accumulator.truncated = true;
            accumulator
                .unexplored
                .push(format!("{path}: claim budget exhausted"));
            break;
        }
        let text = bounded_claim_text(&record.text);
        let price = token_price(&text);
        if accumulator.token_price.saturating_add(price) > request.spec.limits.max_tokens {
            accumulator.truncated = true;
            accumulator
                .unexplored
                .push(format!("{path}: token budget exhausted"));
            break;
        }
        let contradictory_evidence = accumulator
            .contradictions
            .iter()
            .filter_map(|contradiction| {
                if contradiction.left_evidence == record.id {
                    Some(contradiction.right_evidence.clone())
                } else if contradiction.right_evidence == record.id {
                    Some(contradiction.left_evidence.clone())
                } else {
                    None
                }
            })
            .collect();
        let uncertainty = if record.validity == EvidenceValidity::Disputed
            || record.authority == EvidenceAuthority::DerivedInference
        {
            Some("source is disputed or inferential".into())
        } else {
            None
        };
        accumulator.token_price = accumulator.token_price.saturating_add(price);
        accumulator.analyzed += 1;
        accumulator.claims.push(RecallClaim {
            text,
            supporting_evidence: vec![record.id.clone()],
            contradictory_evidence,
            uncertainty,
        });
    }
    Ok(())
}

fn contradictions(evidence: &[EvidenceRecord]) -> Vec<RecallContradiction> {
    let ids = evidence
        .iter()
        .map(|record| record.id.clone())
        .collect::<BTreeSet<_>>();
    let mut pairs = BTreeSet::new();
    for record in evidence {
        for other in [record.supersedes.as_ref(), record.superseded_by.as_ref()]
            .into_iter()
            .flatten()
        {
            if ids.contains(other) {
                let pair = if record.id < *other {
                    (record.id.clone(), other.clone())
                } else {
                    (other.clone(), record.id.clone())
                };
                pairs.insert(pair);
            }
        }
    }
    pairs
        .into_iter()
        .map(|(left_evidence, right_evidence)| RecallContradiction {
            left_evidence,
            right_evidence,
            reason: "source supersession requires correction-aware interpretation".into(),
        })
        .collect()
}

fn check_runtime(
    request: &RecallRequest,
    cancellation: &CancellationToken,
    started_at: Instant,
) -> Result<(), RecallError> {
    check_cancelled(cancellation)?;
    if request.spec.limits.max_runtime_ms < 2
        || started_at.elapsed() > Duration::from_millis(request.spec.limits.max_runtime_ms)
    {
        return Err(RecallError::TimedOut);
    }
    Ok(())
}

fn check_cancelled(cancellation: &CancellationToken) -> Result<(), RecallError> {
    if cancellation.is_cancelled() {
        Err(RecallError::Cancelled)
    } else {
        Ok(())
    }
}

fn validate_request_identity(request: &RecallRequest) -> Result<(), RecallError> {
    request
        .spec
        .limits
        .validate()
        .map_err(|_| RecallError::InvalidContract)?;
    if request.query.trim().is_empty()
        || request.query_identity != stable_query_identity(&request.query, &request.spec)?
    {
        return Err(RecallError::InvalidContract);
    }
    Ok(())
}

fn stable_query_identity(query: &str, spec: &MemoryScoutSpec) -> Result<String, RecallError> {
    #[derive(Serialize)]
    struct Identity<'a> {
        query: &'a str,
        scope: &'a MemoryScoutScopeManifest,
        limits: MemoryScoutLimits,
    }
    let bytes = canonical_json_bytes(&Identity {
        query,
        scope: &spec.scope,
        limits: spec.limits,
    })
    .map_err(|error| RecallError::Json(error.to_string()))?;
    Ok(hex_digest(&bytes))
}

fn append_mutation(
    root: &Path,
    profile_id: &ProfileId,
    state: &mut RecallState,
    mutation: RecallJournalMutation,
    now: UtcTimestamp,
) -> Result<(), RecallError> {
    let sequence = u64::try_from(state.events.len())
        .map_err(|_| RecallError::CorruptJournal)?
        .saturating_add(1);
    let previous_digest = state.events.last().map(|event| event.digest.clone());
    let digest = journal_digest(
        sequence,
        profile_id,
        previous_digest.as_deref(),
        now,
        &mutation,
    )?;
    let event = RecallJournalEvent {
        version: CURRENT_SCHEMA_VERSION,
        sequence,
        profile_id: profile_id.clone(),
        previous_digest,
        digest,
        recorded_at: now,
        mutation,
    };
    let mut bytes =
        canonical_json_bytes(&event).map_err(|error| RecallError::Json(error.to_string()))?;
    bytes.push(b'\n');
    let path = root.join(RECALL_JOURNAL_PATH);
    let mut options = OpenOptions::new();
    options.create(true).append(true);
    let mut file = options.open(path).map_err(io_error)?;
    file.write_all(&bytes).map_err(io_error)?;
    file.sync_data().map_err(io_error)?;
    state.events.push(event);
    Ok(())
}

fn load_journal(
    root: &Path,
    profile_id: &ProfileId,
) -> Result<Vec<RecallJournalEvent>, RecallError> {
    let path = root.join(RECALL_JOURNAL_PATH);
    if !path.exists() {
        return Ok(Vec::new());
    }
    let bytes = fs::read(&path).map_err(io_error)?;
    let mut accepted = 0;
    let mut events = Vec::new();
    for line in bytes.split_inclusive(|byte| *byte == b'\n') {
        if line.iter().all(u8::is_ascii_whitespace) {
            accepted += line.len();
            continue;
        }
        let payload = line.strip_suffix(b"\n").unwrap_or(line);
        match serde_json::from_slice::<RecallJournalEvent>(payload) {
            Ok(event) => {
                validate_journal_event(profile_id, &events, &event)?;
                events.push(event);
                accepted += line.len();
            }
            Err(_) if accepted + line.len() == bytes.len() && !line.ends_with(b"\n") => {
                let file = OpenOptions::new()
                    .write(true)
                    .open(&path)
                    .map_err(io_error)?;
                file.set_len(u64::try_from(accepted).map_err(|_| RecallError::CorruptJournal)?)
                    .map_err(io_error)?;
                file.sync_data().map_err(io_error)?;
            }
            Err(_) => return Err(RecallError::CorruptJournal),
        }
    }
    Ok(events)
}

fn validate_journal_event(
    profile_id: &ProfileId,
    prior: &[RecallJournalEvent],
    event: &RecallJournalEvent,
) -> Result<(), RecallError> {
    let expected_sequence = u64::try_from(prior.len())
        .map_err(|_| RecallError::CorruptJournal)?
        .saturating_add(1);
    let expected_previous = prior.last().map(|item| item.digest.clone());
    if event.profile_id != *profile_id
        || event.sequence != expected_sequence
        || event.previous_digest != expected_previous
        || event.version.major != CURRENT_SCHEMA_VERSION.major
        || event.digest
            != journal_digest(
                event.sequence,
                &event.profile_id,
                event.previous_digest.as_deref(),
                event.recorded_at,
                &event.mutation,
            )?
    {
        return Err(RecallError::CorruptJournal);
    }
    Ok(())
}

fn journal_digest(
    sequence: u64,
    profile_id: &ProfileId,
    previous_digest: Option<&str>,
    recorded_at: UtcTimestamp,
    mutation: &RecallJournalMutation,
) -> Result<String, RecallError> {
    let bytes = canonical_json_bytes(&RecallJournalDigest {
        version: CURRENT_SCHEMA_VERSION,
        sequence,
        profile_id,
        previous_digest,
        recorded_at,
        mutation,
    })
    .map_err(|error| RecallError::Json(error.to_string()))?;
    Ok(hex_digest(&bytes))
}

fn project_journal(state: &mut RecallState) -> Result<(), RecallError> {
    let events = state.events.clone();
    for event in events {
        match event.mutation {
            RecallJournalMutation::Started { request } => {
                validate_request_identity(&request)?;
                if state.completed.contains_key(&request.query_identity)
                    || state.failed.contains(&request.query_identity)
                    || state
                        .started
                        .insert(request.query_identity.clone(), request)
                        .is_some()
                {
                    return Err(RecallError::CorruptJournal);
                }
            }
            RecallJournalMutation::Completed {
                query_identity,
                capsule,
            } => {
                if state.started.remove(&query_identity).is_none()
                    || state.completed.insert(query_identity, *capsule).is_some()
                {
                    return Err(RecallError::CorruptJournal);
                }
            }
            RecallJournalMutation::Failed { query_identity, .. } => {
                if state.started.remove(&query_identity).is_none()
                    || !state.failed.insert(query_identity)
                {
                    return Err(RecallError::CorruptJournal);
                }
            }
        }
    }
    Ok(())
}

fn parse_sensitivity(value: &str) -> Result<Sensitivity, RecallError> {
    match value {
        "public" => Ok(Sensitivity::Public),
        "personal" => Ok(Sensitivity::Personal),
        "sensitive" => Ok(Sensitivity::Sensitive),
        "secret" => Ok(Sensitivity::Secret),
        _ => Err(RecallError::InvalidContract),
    }
}

const fn sensitivity_name(value: Sensitivity) -> &'static str {
    match value {
        Sensitivity::Public => "public",
        Sensitivity::Personal => "personal",
        Sensitivity::Sensitive => "sensitive",
        Sensitivity::Secret => "secret",
    }
}

const fn sensitivity_rank(value: Sensitivity) -> u8 {
    match value {
        Sensitivity::Public => 0,
        Sensitivity::Personal => 1,
        Sensitivity::Sensitive => 2,
        Sensitivity::Secret => 3,
    }
}

fn bounded_claim_text(text: &str) -> String {
    let one_line = text.split_whitespace().collect::<Vec<_>>().join(" ");
    one_line.chars().take(480).collect()
}

fn token_price(text: &str) -> u64 {
    u64::try_from(text.len().saturating_add(3) / 4).unwrap_or(u64::MAX)
}

fn hex_digest(bytes: &[u8]) -> String {
    let digest = Sha256::digest(bytes);
    let mut value = String::with_capacity(digest.len() * 2);
    for byte in digest {
        use std::fmt::Write as _;
        let _ = write!(value, "{byte:02x}");
    }
    value
}

const fn error_code(error: &RecallError) -> &'static str {
    match error {
        RecallError::Io(_) => "io",
        RecallError::Json(_) => "json",
        RecallError::CorruptJournal => "journal",
        RecallError::ScopeChanged => "scope_changed",
        RecallError::NoEvidence => "no_evidence",
        RecallError::InvalidContract => "contract",
        RecallError::InvalidFinding => "finding",
        RecallError::BudgetExceeded => "budget",
        RecallError::Cancelled => "cancelled",
        RecallError::TimedOut => "timeout",
        RecallError::LockPoisoned => "lock",
        RecallError::Observatory(_) => "observatory",
    }
}

#[allow(clippy::needless_pass_by_value)]
fn io_error(error: std::io::Error) -> RecallError {
    RecallError::Io(error.to_string())
}

#[cfg(test)]
mod tests {
    use keith_session_store::RetentionClass;
    use tempfile::TempDir;

    use super::*;
    use crate::{EvidenceSourceKind, ObservatoryLimits, ObservatoryMutation};

    fn evidence(
        profile_id: &ProfileId,
        session_id: &SessionId,
        identity: &str,
        text: &str,
        at: i64,
    ) -> EvidenceRecord {
        EvidenceRecord::new(
            profile_id.clone(),
            session_id.clone(),
            vec![keith_agent_types::EntryId::new()],
            vec![format!("digest-{identity}")],
            identity.into(),
            None,
            EvidenceSourceKind::UserMessage,
            EvidenceAuthority::UserAsserted,
            text.into(),
            UtcTimestamp::from_unix_millis(at),
            Sensitivity::Personal,
            RetentionClass::Durable,
            Vec::new(),
        )
    }

    fn limits() -> MemoryScoutLimits {
        MemoryScoutLimits {
            max_depth: 3,
            max_children: 2,
            max_total_scouts: 8,
            max_concurrency: 2,
            max_evidence_per_scout: 2,
            max_claims: 16,
            max_tokens: 4_000,
            max_result_bytes: 48 * 1_024,
            max_runtime_ms: 5_000,
        }
    }

    #[test]
    fn nested_recall_is_validated_deduplicated_and_crash_replayed() {
        let root = TempDir::new().unwrap();
        let profile = ProfileId::new();
        let session = SessionId::new();
        let observatory = MemoryObservatory::open(
            root.path(),
            &profile,
            ObservatoryLimits::default(),
            UtcTimestamp::UNIX_EPOCH,
        )
        .unwrap();
        let mut mutations = (0..8)
            .map(|index| {
                ObservatoryMutation::Observe(evidence(
                    &profile,
                    &session,
                    &format!("routing-{index}"),
                    &format!("routing decision {index} used a durable causal boundary"),
                    index,
                ))
            })
            .collect::<Vec<_>>();
        mutations.push(ObservatoryMutation::Observe(evidence(
            &profile,
            &session,
            "crash-replay",
            "crash replay preserved the same memory recall",
            20,
        )));
        observatory
            .apply(mutations, UtcTimestamp::from_unix_millis(21))
            .unwrap();
        let recall = RecallService::open(root.path(), &profile).unwrap();
        let request = recall
            .prepare(
                &observatory,
                &session,
                "routing decision",
                Sensitivity::Personal,
                limits(),
                &CancellationToken::default(),
                UtcTimestamp::from_unix_millis(22),
            )
            .unwrap();
        let capsule = recall
            .execute(
                &observatory,
                &request,
                &CancellationToken::default(),
                UtcTimestamp::from_unix_millis(23),
            )
            .unwrap();
        assert!(capsule.coverage.deepest_level > 1);
        assert!(capsule.coverage.scouts_started <= limits().max_total_scouts);
        let repeated = recall
            .execute(
                &observatory,
                &request,
                &CancellationToken::default(),
                UtcTimestamp::from_unix_millis(24),
            )
            .unwrap();
        assert_eq!(capsule.capsule_id, repeated.capsule_id);

        let crash_request = recall
            .prepare(
                &observatory,
                &session,
                "crash replay",
                Sensitivity::Personal,
                limits(),
                &CancellationToken::default(),
                UtcTimestamp::from_unix_millis(25),
            )
            .unwrap();
        recall
            .begin(&crash_request, UtcTimestamp::from_unix_millis(26))
            .unwrap();
        drop(recall);
        let recovered = RecallService::open(root.path(), &profile).unwrap();
        let recovered_capsule = recovered
            .execute(
                &observatory,
                &crash_request,
                &CancellationToken::default(),
                UtcTimestamp::from_unix_millis(27),
            )
            .unwrap();
        assert_eq!(
            recovered_capsule.query_identity,
            crash_request.query_identity
        );
    }

    #[test]
    #[allow(clippy::too_many_lines)]
    fn unsupported_stale_cross_profile_cancelled_timeout_and_explosion_fail_closed() {
        let root = TempDir::new().unwrap();
        let profile = ProfileId::new();
        let other_profile = ProfileId::new();
        let session = SessionId::new();
        let observatory = MemoryObservatory::open(
            root.path(),
            &profile,
            ObservatoryLimits::default(),
            UtcTimestamp::UNIX_EPOCH,
        )
        .unwrap();
        let records = (0..6)
            .map(|index| {
                evidence(
                    &profile,
                    &session,
                    &format!("scope-{index}"),
                    &format!("scoped memory subject {index}"),
                    index,
                )
            })
            .collect::<Vec<_>>();
        let delete_id = records[0].id.clone();
        observatory
            .apply(
                records
                    .into_iter()
                    .map(ObservatoryMutation::Observe)
                    .collect(),
                UtcTimestamp::from_unix_millis(7),
            )
            .unwrap();
        let recall = RecallService::open(root.path(), &profile).unwrap();
        let request = recall
            .prepare(
                &observatory,
                &session,
                "scoped memory",
                Sensitivity::Personal,
                limits(),
                &CancellationToken::default(),
                UtcTimestamp::from_unix_millis(8),
            )
            .unwrap();
        let unsupported = MemoryScoutFinding {
            query_identity: request.query_identity.clone(),
            archive_revision: request.spec.scope.archive_revision,
            claims: vec![RecallClaim {
                text: "unsupported invention".into(),
                supporting_evidence: vec![EntityId::new()],
                contradictory_evidence: Vec::new(),
                uncertainty: None,
            }],
            contradictions: Vec::new(),
            coverage: RecallCoverage {
                scoped_evidence: request.spec.scope.evidence_digests.len(),
                analyzed_evidence: 1,
                scouts_started: 1,
                deepest_level: 1,
                peak_concurrency: 1,
                search: AtlasCoverage {
                    total_active: 6,
                    inspected: 1,
                    matched: 1,
                    returned: 1,
                    truncated: false,
                },
                truncated: false,
            },
            uncertainty: Vec::new(),
            unexplored_regions: Vec::new(),
            token_price: 8,
        };
        assert_eq!(
            recall
                .validate_finding(
                    &observatory,
                    &request,
                    unsupported.clone(),
                    UtcTimestamp::from_unix_millis(9)
                )
                .unwrap_err(),
            RecallError::InvalidFinding
        );
        let other_root = TempDir::new().unwrap();
        let other = RecallService::open(other_root.path(), &other_profile).unwrap();
        assert_eq!(
            other
                .validate_finding(
                    &observatory,
                    &request,
                    unsupported,
                    UtcTimestamp::from_unix_millis(9)
                )
                .unwrap_err(),
            RecallError::ScopeChanged
        );

        observatory
            .apply(
                vec![ObservatoryMutation::Delete {
                    evidence_id: delete_id,
                    source_entries: Vec::new(),
                    source_digests: Vec::new(),
                }],
                UtcTimestamp::from_unix_millis(10),
            )
            .unwrap();
        assert_eq!(
            recall
                .execute(
                    &observatory,
                    &request,
                    &CancellationToken::default(),
                    UtcTimestamp::from_unix_millis(11)
                )
                .unwrap_err(),
            RecallError::ScopeChanged
        );

        let cancellation_request = recall
            .prepare(
                &observatory,
                &session,
                "subject 2",
                Sensitivity::Personal,
                limits(),
                &CancellationToken::default(),
                UtcTimestamp::from_unix_millis(12),
            )
            .unwrap();
        let cancelled = CancellationToken::default();
        cancelled.cancel();
        assert_eq!(
            recall
                .execute(
                    &observatory,
                    &cancellation_request,
                    &cancelled,
                    UtcTimestamp::from_unix_millis(13)
                )
                .unwrap_err(),
            RecallError::Cancelled
        );

        let mut timeout_limits = limits();
        timeout_limits.max_runtime_ms = 1;
        let timeout_request = recall
            .prepare(
                &observatory,
                &session,
                "subject 3",
                Sensitivity::Personal,
                timeout_limits,
                &CancellationToken::default(),
                UtcTimestamp::from_unix_millis(14),
            )
            .unwrap();
        assert_eq!(
            recall
                .execute(
                    &observatory,
                    &timeout_request,
                    &CancellationToken::default(),
                    UtcTimestamp::from_unix_millis(15)
                )
                .unwrap_err(),
            RecallError::TimedOut
        );

        let mut explosion_limits = limits();
        explosion_limits.max_total_scouts = 2;
        explosion_limits.max_concurrency = 1;
        let explosion_request = recall
            .prepare(
                &observatory,
                &session,
                "scoped memory",
                Sensitivity::Personal,
                explosion_limits,
                &CancellationToken::default(),
                UtcTimestamp::from_unix_millis(16),
            )
            .unwrap();
        let bounded = recall
            .execute(
                &observatory,
                &explosion_request,
                &CancellationToken::default(),
                UtcTimestamp::from_unix_millis(17),
            )
            .unwrap();
        assert!(bounded.coverage.truncated);
        assert!(bounded.coverage.scouts_started <= 2);
    }
}
