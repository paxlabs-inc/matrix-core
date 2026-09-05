use std::fmt::Write as _;
use std::fs::{self, File, OpenOptions};
use std::io::Write as _;
use std::path::{Path, PathBuf};
use std::sync::{Mutex, MutexGuard};

use keith_agent_types::{
    CURRENT_SCHEMA_VERSION, EntityId, EntryId, ProfileId, SchemaVersion, SessionId, UtcTimestamp,
    canonical_json_bytes,
};
use keith_session_store::{RetentionClass, Sensitivity};
use serde::{Deserialize, Serialize};
use sha2::{Digest, Sha256};
use thiserror::Error;

use crate::{
    EvidenceAuthority, EvidenceFacet, EvidenceFacetKind, EvidenceRecord, EvidenceSourceKind,
    EvidenceValidity, MemoryObservatory, ObservatoryMutation,
};

const RELATIONSHIP_LOG_PATH: &str = ".keith/relationship-events.jsonl";
const MAX_RELATIONSHIP_EVENTS: usize = 100_000;
const MAX_PREFERRED_NAME_BYTES: usize = 80;

#[derive(Clone, Copy, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum RelationshipStage {
    Unintroduced,
    AwaitingName,
    Established,
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct PreferredName {
    pub value: String,
    pub source_session: SessionId,
    pub source_entry: EntryId,
    pub source_digest: String,
    pub evidence_id: EntityId,
    pub confirmed_at: UtcTimestamp,
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct RelationshipTurnContext {
    pub stage: RelationshipStage,
    pub relationship_revision: u64,
    pub first_meeting: bool,
    pub newly_confirmed_name: bool,
    pub newly_forgotten_name: bool,
    pub preferred_name: Option<PreferredName>,
}

#[derive(Clone, Debug, Serialize, Deserialize)]
#[serde(rename_all = "snake_case", tag = "mutation")]
enum RelationshipMutation {
    IntroductionStarted {
        source_session: SessionId,
        source_entry: EntryId,
        source_digest: String,
    },
    PreferredNameConfirmed {
        name: String,
        source_session: SessionId,
        source_entry: EntryId,
        source_digest: String,
        evidence_id: EntityId,
        supersedes: Option<EntityId>,
    },
    PreferredNameForgotten {
        source_session: SessionId,
        source_entry: EntryId,
        source_digest: String,
        evidence_id: EntityId,
    },
}

#[derive(Clone, Debug, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
struct RelationshipEvent {
    version: SchemaVersion,
    sequence: u64,
    id: EntityId,
    profile_id: ProfileId,
    occurred_at: UtcTimestamp,
    previous_digest: Option<String>,
    mutation: RelationshipMutation,
    digest: String,
}

#[derive(Serialize)]
struct RelationshipEventDigest<'a> {
    version: SchemaVersion,
    sequence: u64,
    id: &'a EntityId,
    profile_id: &'a ProfileId,
    occurred_at: UtcTimestamp,
    previous_digest: &'a Option<String>,
    mutation: &'a RelationshipMutation,
}

#[derive(Clone, Debug, Default)]
struct RelationshipProjection {
    introduction: Option<(SessionId, EntryId, String)>,
    preferred_name: Option<PreferredName>,
}

impl RelationshipProjection {
    const fn stage(&self) -> RelationshipStage {
        if self.preferred_name.is_some() {
            RelationshipStage::Established
        } else if self.introduction.is_some() {
            RelationshipStage::AwaitingName
        } else {
            RelationshipStage::Unintroduced
        }
    }
}

struct RelationshipState {
    events: Vec<RelationshipEvent>,
    projection: RelationshipProjection,
    recovered_truncated_tail: bool,
}

#[derive(Debug, Error)]
pub enum RelationshipError {
    #[error("relationship state I/O failed: {0}")]
    Io(#[from] std::io::Error),
    #[error("relationship state JSON failed: {0}")]
    Json(#[from] serde_json::Error),
    #[error("relationship state is corrupt at line {line}: {reason}")]
    Corrupt { line: usize, reason: String },
    #[error("relationship state belongs to another profile or unsupported schema")]
    Incompatible,
    #[error("relationship event or source is invalid")]
    Invalid,
    #[error("relationship state lock was poisoned")]
    LockPoisoned,
    #[error("relationship evidence projection failed: {0}")]
    Evidence(String),
}

pub struct RelationshipService {
    root: PathBuf,
    profile_id: ProfileId,
    state: Mutex<RelationshipState>,
}

impl RelationshipService {
    /// Opens and validates the profile-scoped append-only relationship log.
    ///
    /// A truncated final line is removed. Any earlier corruption fails closed.
    ///
    /// # Errors
    ///
    /// Returns an error for I/O, incompatible identity, or corrupt durable events.
    pub fn open(root: impl AsRef<Path>, profile_id: &ProfileId) -> Result<Self, RelationshipError> {
        let root = root.as_ref().to_path_buf();
        fs::create_dir_all(root.join(".keith"))?;
        let (events, recovered_truncated_tail) = load_events(&root, profile_id)?;
        let projection = project_events(profile_id, &events)?;
        Ok(Self {
            root,
            profile_id: profile_id.clone(),
            state: Mutex::new(RelationshipState {
                events,
                projection,
                recovered_truncated_tail,
            }),
        })
    }

    /// Advances onboarding or explicit preferred-name state for one genuine user ingress.
    ///
    /// Rebuilding the same provider step returns the same context without appending another event.
    ///
    /// # Errors
    ///
    /// Returns an error if the source identity is invalid or the durable event cannot append.
    pub fn prepare_turn(
        &self,
        session_id: &SessionId,
        entry_id: &EntryId,
        source_digest: &str,
        _user_text: &str,
        now: UtcTimestamp,
    ) -> Result<RelationshipTurnContext, RelationshipError> {
        if source_digest.is_empty() {
            return Err(RelationshipError::Invalid);
        }
        let mut state = self.lock()?;
        let mut first_meeting = false;
        if state.projection.stage() == RelationshipStage::Unintroduced {
            self.append_locked(
                &mut state,
                RelationshipMutation::IntroductionStarted {
                    source_session: session_id.clone(),
                    source_entry: entry_id.clone(),
                    source_digest: source_digest.to_owned(),
                },
                now,
            )?;
            first_meeting = true;
        } else if state.projection.introduction.as_ref().is_some_and(
            |(source_session, source_entry, _)| {
                source_session == session_id && source_entry == entry_id
            },
        ) {
            first_meeting = true;
        }

        relationship_context(&state, first_meeting, false, false)
    }

    /// Appends an agent-authored preferred-name transition against exact user evidence.
    ///
    /// This method validates a typed value; it never extracts a name from natural language.
    ///
    /// # Errors
    /// Returns [`RelationshipError`] if the evidence is not an exact user-supplied value, or if the transition cannot be appended to the durable relationship log.
    pub fn confirm_preferred_name(
        &self,
        session_id: &SessionId,
        entry_id: &EntryId,
        source_digest: &str,
        name: &str,
        now: UtcTimestamp,
    ) -> Result<RelationshipTurnContext, RelationshipError> {
        if source_digest.is_empty() {
            return Err(RelationshipError::Invalid);
        }
        let name = name.trim();
        if validated_name(name).as_deref() != Some(name) {
            return Err(RelationshipError::Invalid);
        }
        let mut state = self.lock()?;
        if state
            .projection
            .preferred_name
            .as_ref()
            .is_some_and(|current| current.value == name)
        {
            return relationship_context(&state, false, false, false);
        }
        let supersedes = state
            .projection
            .preferred_name
            .as_ref()
            .map(|current| current.evidence_id.clone());
        self.append_locked(
            &mut state,
            RelationshipMutation::PreferredNameConfirmed {
                name: name.to_owned(),
                source_session: session_id.clone(),
                source_entry: entry_id.clone(),
                source_digest: source_digest.to_owned(),
                evidence_id: EntityId::new(),
                supersedes,
            },
            now,
        )?;
        relationship_context(&state, false, true, false)
    }

    /// Appends an agent-authored forgetting transition against exact user evidence.
    ///
    /// # Errors
    /// Returns [`RelationshipError`] if the forget transition cannot be appended to the durable relationship log.
    pub fn forget_preferred_name(
        &self,
        session_id: &SessionId,
        entry_id: &EntryId,
        source_digest: &str,
        now: UtcTimestamp,
    ) -> Result<RelationshipTurnContext, RelationshipError> {
        if source_digest.is_empty() {
            return Err(RelationshipError::Invalid);
        }
        let mut state = self.lock()?;
        let Some(current) = state.projection.preferred_name.as_ref() else {
            return relationship_context(&state, false, false, false);
        };
        let evidence_id = current.evidence_id.clone();
        self.append_locked(
            &mut state,
            RelationshipMutation::PreferredNameForgotten {
                source_session: session_id.clone(),
                source_entry: entry_id.clone(),
                source_digest: source_digest.to_owned(),
                evidence_id,
            },
            now,
        )?;
        relationship_context(&state, false, false, true)
    }

    /// Returns the current durable relationship projection without interpreting input.
    ///
    /// # Errors
    /// Returns [`RelationshipError`] if the durable relationship log cannot be read or replayed into a turn context.
    pub fn context(&self) -> Result<RelationshipTurnContext, RelationshipError> {
        let state = self.lock()?;
        relationship_context(&state, false, false, false)
    }

    /// Replays confirmed names, corrections, and forgetting into the evidence vault.
    ///
    /// The relationship log remains authoritative for onboarding state; this projection is
    /// idempotent and may be retried after a crash.
    ///
    /// # Errors
    ///
    /// Returns an error if relationship evidence conflicts with the current profile vault.
    pub fn sync_evidence(
        &self,
        observatory: &MemoryObservatory,
        now: UtcTimestamp,
    ) -> Result<u64, RelationshipError> {
        let events = self.lock()?.events.clone();
        for event in events {
            match &event.mutation {
                RelationshipMutation::IntroductionStarted { .. } => {}
                RelationshipMutation::PreferredNameConfirmed {
                    name,
                    source_session,
                    source_entry,
                    source_digest,
                    evidence_id,
                    supersedes,
                } => {
                    let snapshot = observatory
                        .evidence_snapshot()
                        .map_err(|error| RelationshipError::Evidence(error.to_string()))?;
                    if let Some(existing) = snapshot.get(evidence_id) {
                        let expected = name_evidence(
                            &self.profile_id,
                            &event,
                            name,
                            source_session,
                            source_entry,
                            source_digest,
                            evidence_id,
                        );
                        if existing.content_digest != expected.content_digest
                            || existing.source_identity != expected.source_identity
                        {
                            return Err(RelationshipError::Invalid);
                        }
                        continue;
                    }
                    let evidence = name_evidence(
                        &self.profile_id,
                        &event,
                        name,
                        source_session,
                        source_entry,
                        source_digest,
                        evidence_id,
                    );
                    let mutation = if let Some(prior_id) = supersedes {
                        let prior = snapshot.get(prior_id).ok_or(RelationshipError::Invalid)?;
                        if !matches!(
                            prior.validity,
                            EvidenceValidity::Active | EvidenceValidity::Disputed
                        ) {
                            return Err(RelationshipError::Invalid);
                        }
                        ObservatoryMutation::Supersede {
                            prior_id: prior_id.clone(),
                            replacement: evidence,
                        }
                    } else {
                        ObservatoryMutation::Observe(evidence)
                    };
                    observatory
                        .apply(vec![mutation], now)
                        .map_err(|error| RelationshipError::Evidence(error.to_string()))?;
                }
                RelationshipMutation::PreferredNameForgotten {
                    source_entry,
                    source_digest,
                    evidence_id,
                    ..
                } => {
                    let snapshot = observatory
                        .evidence_snapshot()
                        .map_err(|error| RelationshipError::Evidence(error.to_string()))?;
                    let evidence = snapshot
                        .get(evidence_id)
                        .ok_or(RelationshipError::Invalid)?;
                    if evidence.validity == EvidenceValidity::Deleted {
                        continue;
                    }
                    if !matches!(
                        evidence.validity,
                        EvidenceValidity::Active | EvidenceValidity::Disputed
                    ) {
                        return Err(RelationshipError::Invalid);
                    }
                    observatory
                        .apply(
                            vec![ObservatoryMutation::Delete {
                                evidence_id: evidence_id.clone(),
                                source_entries: vec![source_entry.clone()],
                                source_digests: vec![source_digest.clone()],
                            }],
                            now,
                        )
                        .map_err(|error| RelationshipError::Evidence(error.to_string()))?;
                }
            }
        }
        observatory
            .revision()
            .map_err(|error| RelationshipError::Evidence(error.to_string()))
    }

    /// Reports whether startup discarded a torn final event line.
    ///
    /// # Errors
    ///
    /// Returns an error if the relationship state lock was poisoned.
    pub fn recovered_truncated_tail(&self) -> Result<bool, RelationshipError> {
        Ok(self.lock()?.recovered_truncated_tail)
    }

    fn append_locked(
        &self,
        state: &mut RelationshipState,
        mutation: RelationshipMutation,
        now: UtcTimestamp,
    ) -> Result<(), RelationshipError> {
        if state.events.len() >= MAX_RELATIONSHIP_EVENTS {
            return Err(RelationshipError::Invalid);
        }
        let sequence =
            u64::try_from(state.events.len() + 1).map_err(|_| RelationshipError::Invalid)?;
        let mut event = RelationshipEvent {
            version: CURRENT_SCHEMA_VERSION,
            sequence,
            id: EntityId::new(),
            profile_id: self.profile_id.clone(),
            occurred_at: now,
            previous_digest: state.events.last().map(|event| event.digest.clone()),
            mutation,
            digest: String::new(),
        };
        event.digest = event_digest(&event)?;
        let mut projected = state.projection.clone();
        apply_event(&self.profile_id, &mut projected, &event)?;
        append_event(&self.root, &event)?;
        state.events.push(event);
        state.projection = projected;
        Ok(())
    }

    fn lock(&self) -> Result<MutexGuard<'_, RelationshipState>, RelationshipError> {
        self.state
            .lock()
            .map_err(|_| RelationshipError::LockPoisoned)
    }
}

fn relationship_context(
    state: &RelationshipState,
    first_meeting: bool,
    newly_confirmed_name: bool,
    newly_forgotten_name: bool,
) -> Result<RelationshipTurnContext, RelationshipError> {
    Ok(RelationshipTurnContext {
        stage: state.projection.stage(),
        relationship_revision: u64::try_from(state.events.len())
            .map_err(|_| RelationshipError::Invalid)?,
        first_meeting,
        newly_confirmed_name,
        newly_forgotten_name,
        preferred_name: state.projection.preferred_name.clone(),
    })
}

fn validated_name(candidate: &str) -> Option<String> {
    let candidate = candidate.trim();
    if candidate.is_empty()
        || candidate.len() > MAX_PREFERRED_NAME_BYTES
        || candidate.split_whitespace().count() > 4
        || candidate.chars().any(|character| {
            !(character.is_alphabetic()
                || character.is_whitespace()
                || matches!(character, '-' | '\'' | '’' | '.'))
        })
    {
        return None;
    }
    Some(candidate.to_owned())
}

fn name_evidence(
    profile_id: &ProfileId,
    event: &RelationshipEvent,
    name: &str,
    source_session: &SessionId,
    source_entry: &EntryId,
    source_digest: &str,
    evidence_id: &EntityId,
) -> EvidenceRecord {
    let mut evidence = EvidenceRecord::new(
        profile_id.clone(),
        source_session.clone(),
        vec![source_entry.clone()],
        vec![source_digest.to_owned()],
        format!("relationship:{}:preferred-name", event.id),
        None,
        EvidenceSourceKind::DurableMemory,
        EvidenceAuthority::UserAsserted,
        format!("Preferred name: {name}"),
        event.occurred_at,
        Sensitivity::Personal,
        RetentionClass::Durable,
        vec![
            EvidenceFacet {
                kind: EvidenceFacetKind::Theme,
                value: "personal_fact".into(),
            },
            EvidenceFacet {
                kind: EvidenceFacetKind::Theme,
                value: "relationship".into(),
            },
            EvidenceFacet {
                kind: EvidenceFacetKind::Entity,
                value: name.to_owned(),
            },
            EvidenceFacet {
                kind: EvidenceFacetKind::Tag,
                value: "preferred_name".into(),
            },
        ],
    );
    evidence.id = evidence_id.clone();
    evidence
}

fn load_events(
    root: &Path,
    profile_id: &ProfileId,
) -> Result<(Vec<RelationshipEvent>, bool), RelationshipError> {
    let path = root.join(RELATIONSHIP_LOG_PATH);
    if !path.exists() {
        return Ok((Vec::new(), false));
    }
    let mut bytes = fs::read(&path)?;
    let mut recovered = false;
    if !bytes.is_empty() && !bytes.ends_with(b"\n") {
        let boundary = bytes
            .iter()
            .rposition(|byte| *byte == b'\n')
            .map_or(0, |position| position + 1);
        let tail = &bytes[boundary..];
        if serde_json::from_slice::<RelationshipEvent>(tail).is_err() {
            let file = OpenOptions::new().write(true).open(&path)?;
            file.set_len(u64::try_from(boundary).map_err(|_| RelationshipError::Invalid)?)?;
            file.sync_all()?;
            bytes.truncate(boundary);
            recovered = true;
        }
    }
    let mut events = Vec::new();
    let mut previous = None;
    for (index, line) in bytes.split(|byte| *byte == b'\n').enumerate() {
        if line.is_empty() {
            continue;
        }
        let event = serde_json::from_slice::<RelationshipEvent>(line).map_err(|error| {
            RelationshipError::Corrupt {
                line: index + 1,
                reason: error.to_string(),
            }
        })?;
        let expected_sequence =
            u64::try_from(events.len() + 1).map_err(|_| RelationshipError::Invalid)?;
        if &event.profile_id != profile_id
            || event.version.major != CURRENT_SCHEMA_VERSION.major
            || event.version.minor > CURRENT_SCHEMA_VERSION.minor
            || event.sequence != expected_sequence
            || event.previous_digest != previous
            || event.digest != event_digest(&event)?
        {
            return Err(RelationshipError::Corrupt {
                line: index + 1,
                reason: "event identity, chain, version, or digest mismatch".into(),
            });
        }
        previous = Some(event.digest.clone());
        events.push(event);
    }
    if events.len() > MAX_RELATIONSHIP_EVENTS {
        return Err(RelationshipError::Invalid);
    }
    Ok((events, recovered))
}

fn project_events(
    profile_id: &ProfileId,
    events: &[RelationshipEvent],
) -> Result<RelationshipProjection, RelationshipError> {
    let mut projection = RelationshipProjection::default();
    for event in events {
        apply_event(profile_id, &mut projection, event)?;
    }
    Ok(projection)
}

fn apply_event(
    profile_id: &ProfileId,
    projection: &mut RelationshipProjection,
    event: &RelationshipEvent,
) -> Result<(), RelationshipError> {
    if &event.profile_id != profile_id {
        return Err(RelationshipError::Incompatible);
    }
    match &event.mutation {
        RelationshipMutation::IntroductionStarted {
            source_session,
            source_entry,
            source_digest,
        } => {
            if projection.introduction.is_some() || source_digest.is_empty() {
                return Err(RelationshipError::Invalid);
            }
            projection.introduction = Some((
                source_session.clone(),
                source_entry.clone(),
                source_digest.clone(),
            ));
        }
        RelationshipMutation::PreferredNameConfirmed {
            name,
            source_session,
            source_entry,
            source_digest,
            evidence_id,
            supersedes,
        } => {
            if projection.introduction.is_none()
                || validated_name(name).as_deref() != Some(name.as_str())
                || source_digest.is_empty()
                || supersedes
                    != &projection
                        .preferred_name
                        .as_ref()
                        .map(|current| current.evidence_id.clone())
            {
                return Err(RelationshipError::Invalid);
            }
            projection.preferred_name = Some(PreferredName {
                value: name.clone(),
                source_session: source_session.clone(),
                source_entry: source_entry.clone(),
                source_digest: source_digest.clone(),
                evidence_id: evidence_id.clone(),
                confirmed_at: event.occurred_at,
            });
        }
        RelationshipMutation::PreferredNameForgotten {
            source_digest,
            evidence_id,
            ..
        } => {
            if source_digest.is_empty()
                || projection
                    .preferred_name
                    .as_ref()
                    .is_none_or(|current| &current.evidence_id != evidence_id)
            {
                return Err(RelationshipError::Invalid);
            }
            projection.preferred_name = None;
        }
    }
    Ok(())
}

fn append_event(root: &Path, event: &RelationshipEvent) -> Result<(), RelationshipError> {
    let path = root.join(RELATIONSHIP_LOG_PATH);
    let parent = path.parent().ok_or(RelationshipError::Invalid)?;
    fs::create_dir_all(parent)?;
    let mut bytes = canonical_json_bytes(event)?;
    bytes.push(b'\n');
    let mut file = OpenOptions::new().create(true).append(true).open(&path)?;
    file.write_all(&bytes)?;
    file.sync_all()?;
    File::open(parent)?.sync_all()?;
    Ok(())
}

fn event_digest(event: &RelationshipEvent) -> Result<String, RelationshipError> {
    let bytes = canonical_json_bytes(&RelationshipEventDigest {
        version: event.version,
        sequence: event.sequence,
        id: &event.id,
        profile_id: &event.profile_id,
        occurred_at: event.occurred_at,
        previous_digest: &event.previous_digest,
        mutation: &event.mutation,
    })?;
    Ok(hex_digest(&bytes))
}

fn hex_digest(bytes: &[u8]) -> String {
    let digest = Sha256::digest(bytes);
    let mut output = String::with_capacity(digest.len() * 2);
    for byte in digest {
        write!(&mut output, "{byte:02x}").expect("writing to a string cannot fail");
    }
    output
}

#[cfg(test)]
mod tests {
    use tempfile::tempdir;

    use super::*;
    use crate::ObservatoryLimits;

    #[test]
    fn onboarding_is_exact_once_and_name_survives_restart_with_exact_evidence() {
        let root = tempdir().unwrap();
        let profile_id = ProfileId::new();
        let first_session = SessionId::new();
        let first_entry = EntryId::new();
        let service = RelationshipService::open(root.path(), &profile_id).unwrap();
        let first = service
            .prepare_turn(
                &first_session,
                &first_entry,
                "first-digest",
                "hello",
                UtcTimestamp::UNIX_EPOCH,
            )
            .unwrap();
        assert!(first.first_meeting);
        assert_eq!(first.stage, RelationshipStage::AwaitingName);
        let repeated = service
            .prepare_turn(
                &first_session,
                &first_entry,
                "first-digest",
                "hello",
                UtcTimestamp::from_unix_millis(1),
            )
            .unwrap();
        assert!(repeated.first_meeting);
        assert_eq!(repeated.relationship_revision, first.relationship_revision);

        let name_session = SessionId::new();
        let name_entry = EntryId::new();
        let named = service
            .confirm_preferred_name(
                &name_session,
                &name_entry,
                "name-digest",
                "Neo",
                UtcTimestamp::from_unix_millis(2),
            )
            .unwrap();
        assert!(named.newly_confirmed_name);
        assert_eq!(named.stage, RelationshipStage::Established);
        assert_eq!(named.preferred_name.as_ref().unwrap().value, "Neo");
        drop(service);

        let reopened = RelationshipService::open(root.path(), &profile_id).unwrap();
        let observatory = MemoryObservatory::open(
            root.path(),
            &profile_id,
            ObservatoryLimits::default(),
            UtcTimestamp::from_unix_millis(3),
        )
        .unwrap();
        reopened
            .sync_evidence(&observatory, UtcTimestamp::from_unix_millis(3))
            .unwrap();
        let evidence = observatory
            .evidence_snapshot()
            .unwrap()
            .into_values()
            .find(|record| record.text == "Preferred name: Neo")
            .unwrap();
        assert_eq!(evidence.authority, EvidenceAuthority::UserAsserted);
        assert_eq!(evidence.source_session, name_session);
        assert_eq!(evidence.source_entries, vec![name_entry]);
        assert_eq!(evidence.source_digests, vec!["name-digest"]);
    }

    #[test]
    fn preparation_never_infers_while_explicit_correction_and_forgetting_update_evidence() {
        let root = tempdir().unwrap();
        let profile_id = ProfileId::new();
        let service = RelationshipService::open(root.path(), &profile_id).unwrap();
        service
            .prepare_turn(
                &SessionId::new(),
                &EntryId::new(),
                "intro",
                "hello",
                UtcTimestamp::UNIX_EPOCH,
            )
            .unwrap();
        let weak = service
            .prepare_turn(
                &SessionId::new(),
                &EntryId::new(),
                "weak",
                "not sure",
                UtcTimestamp::from_unix_millis(1),
            )
            .unwrap();
        assert_eq!(weak.stage, RelationshipStage::AwaitingName);
        assert!(weak.preferred_name.is_none());
        let command = service
            .prepare_turn(
                &SessionId::new(),
                &EntryId::new(),
                "command",
                "continue",
                UtcTimestamp::from_unix_millis(2),
            )
            .unwrap();
        assert_eq!(command.stage, RelationshipStage::AwaitingName);
        assert!(command.preferred_name.is_none());
        let neo_session = SessionId::new();
        let neo_entry = EntryId::new();
        let neo = service
            .confirm_preferred_name(
                &neo_session,
                &neo_entry,
                "neo",
                "Neo",
                UtcTimestamp::from_unix_millis(3),
            )
            .unwrap();
        let neo_id = neo.preferred_name.unwrap().evidence_id;
        let trinity_session = SessionId::new();
        let trinity_entry = EntryId::new();
        let trinity = service
            .confirm_preferred_name(
                &trinity_session,
                &trinity_entry,
                "trinity",
                "Trinity",
                UtcTimestamp::from_unix_millis(4),
            )
            .unwrap();
        assert_eq!(trinity.preferred_name.as_ref().unwrap().value, "Trinity");
        let trinity_id = trinity.preferred_name.unwrap().evidence_id;
        let forget_session = SessionId::new();
        let forget_entry = EntryId::new();
        let forgotten = service
            .forget_preferred_name(
                &forget_session,
                &forget_entry,
                "forget",
                UtcTimestamp::from_unix_millis(5),
            )
            .unwrap();
        assert!(forgotten.newly_forgotten_name);
        assert_eq!(forgotten.stage, RelationshipStage::AwaitingName);

        let observatory = MemoryObservatory::open(
            root.path(),
            &profile_id,
            ObservatoryLimits::default(),
            UtcTimestamp::from_unix_millis(6),
        )
        .unwrap();
        service
            .sync_evidence(&observatory, UtcTimestamp::from_unix_millis(6))
            .unwrap();
        let snapshot = observatory.evidence_snapshot().unwrap();
        assert_eq!(snapshot[&neo_id].validity, EvidenceValidity::Superseded);
        assert_eq!(snapshot[&trinity_id].validity, EvidenceValidity::Deleted);
    }

    #[test]
    fn truncated_tail_recovers_but_cross_profile_open_fails() {
        let root = tempdir().unwrap();
        let profile_id = ProfileId::new();
        let service = RelationshipService::open(root.path(), &profile_id).unwrap();
        service
            .prepare_turn(
                &SessionId::new(),
                &EntryId::new(),
                "intro",
                "hello",
                UtcTimestamp::UNIX_EPOCH,
            )
            .unwrap();
        drop(service);
        let path = root.path().join(RELATIONSHIP_LOG_PATH);
        OpenOptions::new()
            .append(true)
            .open(&path)
            .unwrap()
            .write_all(b"{\"truncated\":")
            .unwrap();
        let recovered = RelationshipService::open(root.path(), &profile_id).unwrap();
        assert!(recovered.recovered_truncated_tail().unwrap());
        assert!(matches!(
            RelationshipService::open(root.path(), &ProfileId::new()),
            Err(RelationshipError::Corrupt { .. })
        ));
    }
}
