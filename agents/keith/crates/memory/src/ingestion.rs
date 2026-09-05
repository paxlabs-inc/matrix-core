//! Verified source intake and disposable replay checkpoints. Source history and
//! the evidence vault remain authoritative; this checkpoint may be rebuilt.

use std::collections::BTreeMap;
use std::fs::{self, File, OpenOptions};
use std::io::Write;
use std::path::Path;

use keith_agent_types::{
    CURRENT_SCHEMA_VERSION, EntityId, EntryId, ProfileId, SchemaVersion, SessionId, UtcTimestamp,
    canonical_json_bytes,
};
use keith_session_store::{
    CommittedSourceCursor, CommittedSourceEntry, CommittedSourcePage, CommittedSourceReference,
    ContentBlock, RetentionClass, Sensitivity, SessionEntry, SessionEntryPayload,
};
use serde::{Deserialize, Serialize};

use crate::{
    CandidateEvidenceReference, EVIDENCE_CAUSAL_VERSION, EvidenceAuthority, EvidenceCausalMetadata,
    EvidenceRecord, EvidenceSourceKind, EvidenceSourceRoot, EvidenceValidity, MemoryError,
    MemoryService, ObservatoryError, ObservatoryLimits, ObservatoryMutation, SourceLineageGap,
    SourceLineageGapReason,
};

const CHECKPOINT: &str = ".keith/memory-source-cursors.json";
const MAX_CHECKPOINT_BYTES: usize = 8 * 1024 * 1024;
const MAX_SESSIONS: usize = 4096;
const MAX_PENDING: usize = 1024;
const MAX_GAPS: usize = 256;

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum IngestionProjectionStatus {
    Ready,
    Pending,
}

#[derive(Clone, Debug, Eq, PartialEq)]
pub struct CommittedIngestionReceipt {
    pub processed_entries: usize,
    pub vault_revision: u64,
    pub cursor_advanced: bool,
    pub checkpoint_pending: bool,
    pub projection: IngestionProjectionStatus,
    pub gaps: Vec<SourceLineageGap>,
}

#[derive(Clone, Default, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
struct SessionProgress {
    cursor: Option<CommittedSourceCursor>,
    pending: BTreeMap<EntryId, SessionEntry>,
    #[serde(default, skip_serializing_if = "BTreeMap::is_empty")]
    pending_lineage: BTreeMap<EntryId, SessionEntry>,
}

#[derive(Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
struct Checkpoint {
    version: SchemaVersion,
    profile_id: ProfileId,
    sessions: BTreeMap<SessionId, SessionProgress>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    retry_after: Option<(SessionId, EntryId)>,
}

impl Checkpoint {
    fn empty(profile: &ProfileId) -> Self {
        Self {
            version: CURRENT_SCHEMA_VERSION,
            profile_id: profile.clone(),
            sessions: BTreeMap::new(),
            retry_after: None,
        }
    }
}

#[allow(clippy::missing_errors_doc)]
impl MemoryService {
    pub fn committed_source_cursor(
        &self,
        session_id: &SessionId,
    ) -> Result<Option<CommittedSourceCursor>, MemoryError> {
        let root = &self.workspace.layout().root;
        let _lock = ingestion_lock(root)?;
        let checkpoint = read_checkpoint(root, &self.profile_id)?;
        Ok(checkpoint
            .sessions
            .get(session_id)
            .and_then(|session| session.cursor.clone()))
    }

    pub fn ingest_committed_page(
        &self,
        page: &CommittedSourcePage,
        now: UtcTimestamp,
    ) -> Result<CommittedIngestionReceipt, MemoryError> {
        self.ingest_source(
            page.profile_id(),
            page.session_id(),
            page.entries(),
            Some((page.input_cursor(), page.next_cursor())),
            now,
        )
    }

    /// Current-ingress fast path. It cannot skip an older replay backlog.
    pub fn ingest_committed_entry(
        &self,
        receipt: &CommittedSourceEntry,
        now: UtcTimestamp,
    ) -> Result<CommittedIngestionReceipt, MemoryError> {
        self.ingest_source(
            receipt.profile_id(),
            receipt.session_id(),
            std::slice::from_ref(receipt.entry()),
            None,
            now,
        )
    }

    pub fn repair_ingestion_projection(
        &self,
        now: UtcTimestamp,
    ) -> Result<IngestionProjectionStatus, MemoryError> {
        let records = self.records()?;
        self.observatory.sync_memory_records(records.iter(), now)?;
        Ok(if self.observatory.health_snapshot()?.degraded {
            IngestionProjectionStatus::Pending
        } else {
            IngestionProjectionStatus::Ready
        })
    }

    fn ingest_source(
        &self,
        profile: &ProfileId,
        session: &SessionId,
        entries: &[SessionEntry],
        cursor: Option<(
            Option<&CommittedSourceCursor>,
            Option<&CommittedSourceCursor>,
        )>,
        now: UtcTimestamp,
    ) -> Result<CommittedIngestionReceipt, MemoryError> {
        if profile != &self.profile_id {
            return Err(MemoryError::InvalidIngestion);
        }
        let root = &self.workspace.layout().root;
        let _lock = ingestion_lock(root)?;
        let mut checkpoint = read_checkpoint(root, profile)?;
        if !checkpoint.sessions.contains_key(session) && checkpoint.sessions.len() >= MAX_SESSIONS {
            return Err(MemoryError::IngestionLimit);
        }
        let mut progress = checkpoint.sessions.remove(session).unwrap_or_default();
        if cursor.is_some_and(|(input, _)| input != progress.cursor.as_ref()) {
            return Err(MemoryError::IngestionCursorChanged);
        }
        let mut gaps = Vec::new();
        let revision =
            self.observatory
                .apply_source_snapshot(now, |snapshot, commitments, revision| {
                    validate_cached_sources(session, &progress, commitments)?;
                    for (cached_session, cached_progress) in &checkpoint.sessions {
                        validate_cached_sources(cached_session, cached_progress, commitments)?;
                    }
                    let mut projected = snapshot.clone();
                    let mut committed = commitments.clone();
                    let mut mutations = Vec::new();
                    for entry in entries {
                        entry
                            .verify()
                            .map_err(|_| ObservatoryError::InvalidEvidence)?;
                        let key = (session.clone(), entry.id.clone());
                        if let Some(prior) = committed.get(&key) {
                            if prior != &entry.checksum {
                                return Err(ObservatoryError::InvalidEvidence);
                            }
                        } else {
                            committed.insert(key, entry.checksum.clone());
                            mutations.push(ObservatoryMutation::CommitSource(
                                CommittedSourceReference {
                                    profile_id: profile.clone(),
                                    session_id: session.clone(),
                                    entry_id: entry.id.clone(),
                                    checksum: entry.checksum.clone(),
                                },
                            ));
                        }
                        for eligible in eligible_entries(session, entry, &mut progress, &mut gaps)?
                        {
                            project_entry(
                                profile,
                                session,
                                &eligible,
                                &mut projected,
                                &committed,
                                revision.saturating_add(mutations.len() as u64),
                                &mut mutations,
                                &mut gaps,
                            )?;
                            retain_lineage_retry(session, eligible, &projected, &mut progress)?;
                        }
                    }
                    checkpoint
                        .sessions
                        .insert(session.clone(), progress.clone());
                    retry_lineage(
                        profile,
                        &mut checkpoint,
                        &mut projected,
                        &committed,
                        revision,
                        &mut mutations,
                        &mut gaps,
                    )?;
                    Ok(mutations)
                })?;
        let progress = checkpoint
            .sessions
            .get_mut(session)
            .ok_or(MemoryError::InvalidIngestion)?;
        let mut cursor_advanced = false;
        if let Some((_, next)) = cursor {
            cursor_advanced = progress.cursor.as_ref() != next;
            progress.cursor = next.cloned();
        }
        let checkpoint_pending = write_checkpoint(root, &checkpoint).is_err();
        self.invalidate_hot_cache();
        Ok(CommittedIngestionReceipt {
            processed_entries: entries.len(),
            vault_revision: revision,
            cursor_advanced: cursor_advanced && !checkpoint_pending,
            checkpoint_pending,
            projection: self
                .repair_ingestion_projection(now)
                .unwrap_or(IngestionProjectionStatus::Pending),
            gaps,
        })
    }
}

fn validate_cached_sources(
    session: &SessionId,
    progress: &SessionProgress,
    commitments: &crate::observatory::SourceCommitments,
) -> Result<(), ObservatoryError> {
    for entry in progress
        .pending
        .values()
        .chain(progress.pending_lineage.values())
    {
        if commitments.get(&(session.clone(), entry.id.clone())) != Some(&entry.checksum) {
            return Err(ObservatoryError::InvalidEvidence);
        }
    }
    Ok(())
}

fn retain_lineage_retry(
    session: &SessionId,
    entry: SessionEntry,
    snapshot: &BTreeMap<EntityId, EvidenceRecord>,
    progress: &mut SessionProgress,
) -> Result<(), ObservatoryError> {
    let missing = direct_source(snapshot, session, &entry.id)
        .filter(|record| {
            matches!(
                record.validity,
                EvidenceValidity::Active | EvidenceValidity::Disputed
            )
        })
        .and_then(|record| record.causal.as_ref())
        .is_some_and(|causal| {
            causal
                .gaps
                .iter()
                .any(|gap| gap.reason == SourceLineageGapReason::MissingSource)
        });
    if missing {
        if progress
            .pending
            .len()
            .saturating_add(progress.pending_lineage.len())
            >= MAX_PENDING
            && !progress.pending_lineage.contains_key(&entry.id)
        {
            return Err(ObservatoryError::InvalidEvidence);
        }
        progress.pending_lineage.insert(entry.id.clone(), entry);
    } else {
        progress.pending_lineage.remove(&entry.id);
    }
    Ok(())
}

fn retry_lineage(
    profile: &ProfileId,
    checkpoint: &mut Checkpoint,
    snapshot: &mut BTreeMap<EntityId, EvidenceRecord>,
    commitments: &crate::observatory::SourceCommitments,
    revision: u64,
    mutations: &mut Vec<ObservatoryMutation>,
    gaps: &mut Vec<SourceLineageGap>,
) -> Result<(), ObservatoryError> {
    // Checkpoint loading has a separate 8MiB bound. Only 128 pending derivations
    // are re-resolved per intake call, with a durable fair rotation cursor.
    let mut keys = checkpoint
        .sessions
        .iter()
        .flat_map(|(session, progress)| {
            progress
                .pending_lineage
                .keys()
                .map(|entry| (session.clone(), entry.clone()))
        })
        .collect::<Vec<_>>();
    if let Some(after) = &checkpoint.retry_after {
        let index = keys.partition_point(|key| key <= after);
        keys.rotate_left(index);
    }
    for (session, entry_id) in keys.into_iter().take(128) {
        let progress = checkpoint
            .sessions
            .get_mut(&session)
            .ok_or(ObservatoryError::InvalidEvidence)?;
        let entry = progress
            .pending_lineage
            .get(&entry_id)
            .cloned()
            .ok_or(ObservatoryError::InvalidEvidence)?;
        project_entry(
            profile,
            &session,
            &entry,
            snapshot,
            commitments,
            revision.saturating_add(mutations.len() as u64),
            mutations,
            gaps,
        )?;
        retain_lineage_retry(&session, entry, snapshot, progress)?;
        checkpoint.retry_after = Some((session, entry_id));
    }
    Ok(())
}

fn ingestion_lock(root: &Path) -> Result<File, MemoryError> {
    let file = OpenOptions::new()
        .read(true)
        .write(true)
        .create(true)
        .truncate(false)
        .open(root.join(".keith/memory-ingestion.lock"))?;
    fs2::FileExt::try_lock_exclusive(&file).map_err(|error| {
        if error.kind() == std::io::ErrorKind::WouldBlock {
            MemoryError::IngestionBusy
        } else {
            MemoryError::Io(error)
        }
    })?;
    Ok(file)
}

fn read_checkpoint(root: &Path, profile: &ProfileId) -> Result<Checkpoint, MemoryError> {
    let path = root.join(CHECKPOINT);
    if !path.exists() {
        return Ok(Checkpoint::empty(profile));
    }
    if fs::metadata(&path)?.len() > MAX_CHECKPOINT_BYTES as u64 {
        return Err(MemoryError::IngestionLimit);
    }
    let bytes = fs::read(&path)?;
    let checkpoint: Checkpoint = if let Ok(value) = serde_json::from_slice(&bytes) {
        value
    } else {
        fs::rename(
            &path,
            path.with_file_name(format!(
                "memory-source-cursors.corrupt-{}.json",
                EntityId::new()
            )),
        )?;
        return Ok(Checkpoint::empty(profile));
    };
    if &checkpoint.profile_id != profile
        || checkpoint.version != CURRENT_SCHEMA_VERSION
        || checkpoint.sessions.len() > MAX_SESSIONS
    {
        return Err(MemoryError::InvalidIngestion);
    }
    for (session, progress) in &checkpoint.sessions {
        if progress
            .pending
            .len()
            .saturating_add(progress.pending_lineage.len())
            > MAX_PENDING
            || progress.cursor.as_ref().is_some_and(|cursor| {
                cursor.profile_id() != profile || cursor.session_id() != session
            })
        {
            return Err(MemoryError::InvalidIngestion);
        }
        for (id, entry) in progress.pending.iter().chain(&progress.pending_lineage) {
            if id != &entry.id || entry.verify().is_err() {
                return Err(MemoryError::InvalidIngestion);
            }
        }
    }
    Ok(checkpoint)
}

fn write_checkpoint(root: &Path, checkpoint: &Checkpoint) -> Result<(), MemoryError> {
    let bytes = canonical_json_bytes(checkpoint)?;
    if bytes.len() > MAX_CHECKPOINT_BYTES {
        return Err(MemoryError::IngestionLimit);
    }
    let path = root.join(CHECKPOINT);
    let temporary = path.with_file_name(format!("memory-source-cursors.{}.tmp", EntityId::new()));
    let mut file = OpenOptions::new()
        .write(true)
        .create_new(true)
        .open(&temporary)?;
    file.write_all(&bytes)?;
    file.sync_all()?;
    fs::rename(&temporary, &path)?;
    File::open(path.parent().ok_or(MemoryError::InvalidIngestion)?)?.sync_all()?;
    Ok(())
}

pub(crate) fn gap(
    gaps: &mut Vec<SourceLineageGap>,
    session: &SessionId,
    entry: &EntryId,
    reason: SourceLineageGapReason,
) {
    let item = SourceLineageGap {
        source_session: session.clone(),
        source_entry: entry.clone(),
        reason,
    };
    if !gaps.contains(&item) {
        if gaps.len() < MAX_GAPS {
            gaps.push(item);
        } else if !gaps
            .iter()
            .any(|prior| prior.reason == SourceLineageGapReason::Limit)
        {
            gaps[MAX_GAPS - 1] = SourceLineageGap {
                reason: SourceLineageGapReason::Limit,
                ..item
            };
        }
    }
}

fn stage(entry: &SessionEntry, progress: &mut SessionProgress) -> Result<(), ObservatoryError> {
    if progress
        .pending
        .get(&entry.id)
        .is_some_and(|prior| prior.checksum != entry.checksum)
        || progress
            .pending
            .len()
            .saturating_add(progress.pending_lineage.len())
            >= MAX_PENDING
            && !progress.pending.contains_key(&entry.id)
    {
        return Err(ObservatoryError::InvalidEvidence);
    }
    progress.pending.insert(entry.id.clone(), entry.clone());
    Ok(())
}

#[allow(clippy::too_many_lines)]
fn eligible_entries(
    session: &SessionId,
    entry: &SessionEntry,
    progress: &mut SessionProgress,
    gaps: &mut Vec<SourceLineageGap>,
) -> Result<Vec<SessionEntry>, ObservatoryError> {
    match &entry.payload {
        SessionEntryPayload::AssistantFinal { .. } => {
            stage(entry, progress)?;
            gap(
                gaps,
                session,
                &entry.id,
                SourceLineageGapReason::PendingFinal,
            );
            Ok(vec![])
        }
        SessionEntryPayload::CompactionSummary { .. } => {
            stage(entry, progress)?;
            gap(
                gaps,
                session,
                &entry.id,
                SourceLineageGapReason::PendingCompaction,
            );
            Ok(vec![])
        }
        SessionEntryPayload::TurnDeliveryOutbox { .. }
        | SessionEntryPayload::AuthoritativeSnapshot { .. } => {
            stage(entry, progress)?;
            Ok(vec![])
        }
        SessionEntryPayload::TerminalTurn {
            turn_id,
            final_id,
            final_created,
            delivery_outbox_id,
            authoritative_snapshot_id,
            ..
        } => {
            let Some(final_entry) = progress.pending.get(final_id) else {
                gap(
                    gaps,
                    session,
                    final_id,
                    SourceLineageGapReason::MissingSource,
                );
                return Ok(vec![]);
            };
            let valid = *final_created && matches!(&final_entry.payload, SessionEntryPayload::AssistantFinal { turn_id: final_turn, .. } if final_turn == turn_id)
                && delivery_outbox_id.as_ref().is_none_or(|id| matches!(progress.pending.get(id).map(|entry| &entry.payload), Some(SessionEntryPayload::TurnDeliveryOutbox { turn_id: outbox_turn, final_id: outbox_final, .. }) if outbox_turn == turn_id && outbox_final == final_id))
                && authoritative_snapshot_id.as_ref().is_none_or(|id| matches!(progress.pending.get(id).map(|entry| &entry.payload), Some(SessionEntryPayload::AuthoritativeSnapshot { snapshot }) if snapshot.session_id == *session && snapshot.turn_id == *turn_id && snapshot.final_id == *final_id && snapshot.terminal_id == entry.id));
            if !valid {
                gap(
                    gaps,
                    session,
                    final_id,
                    SourceLineageGapReason::ConflictingSource,
                );
                return Ok(vec![]);
            }
            let final_entry = progress
                .pending
                .remove(final_id)
                .ok_or(ObservatoryError::InvalidEvidence)?;
            if let Some(id) = delivery_outbox_id {
                progress.pending.remove(id);
            }
            if let Some(id) = authoritative_snapshot_id {
                progress.pending.remove(id);
            }
            gaps.retain(|gap| {
                gap.source_entry != *final_id || gap.reason != SourceLineageGapReason::PendingFinal
            });
            Ok(vec![final_entry])
        }
        SessionEntryPayload::CompactionCheckpoint {
            compaction_id,
            summary_id,
            summary,
            source_entries,
            ..
        } => {
            let Some(source) = progress.pending.get(summary_id) else {
                gap(
                    gaps,
                    session,
                    summary_id,
                    SourceLineageGapReason::MissingSource,
                );
                return Ok(vec![]);
            };
            if !matches!(&source.payload, SessionEntryPayload::CompactionSummary { compaction_id: source_id, summary: source_text, source_entries: source_ids, .. } if compaction_id == source_id && summary == source_text && source_entries == source_ids)
            {
                gap(
                    gaps,
                    session,
                    summary_id,
                    SourceLineageGapReason::ConflictingSource,
                );
                return Ok(vec![]);
            }
            gaps.retain(|gap| {
                gap.source_entry != *summary_id
                    || gap.reason != SourceLineageGapReason::PendingCompaction
            });
            Ok(vec![
                progress
                    .pending
                    .remove(summary_id)
                    .ok_or(ObservatoryError::InvalidEvidence)?,
                entry.clone(),
            ])
        }
        SessionEntryPayload::AssistantFinalCandidate { .. } => Ok(vec![]),

        _ => Ok(vec![entry.clone()]),
    }
}

pub(crate) fn direct_source<'a>(
    snapshot: &'a BTreeMap<EntityId, EvidenceRecord>,
    session: &SessionId,
    entry: &EntryId,
) -> Option<&'a EvidenceRecord> {
    let identity = format!("session:{session}:entry:{entry}");
    let legacy_summary = format!("session:{session}:compaction-summary:{entry}");
    snapshot.values().find(|record| {
        record.source_identity == identity || record.source_identity == legacy_summary
    })
}

pub(crate) fn context_lineage(source: &EvidenceRecord, revision: u64) -> EvidenceCausalMetadata {
    let mut metadata = source
        .causal
        .clone()
        .unwrap_or_else(|| EvidenceCausalMetadata {
            version: EVIDENCE_CAUSAL_VERSION,
            effective: None,
            source_roots: vec![],
            derived_from: vec![],
            gaps: vec![],
        });
    if source.causal.is_none() {
        if !matches!(
            source.source_kind,
            EvidenceSourceKind::CompactionSummary
                | EvidenceSourceKind::DailyMemory
                | EvidenceSourceKind::CurrentState
                | EvidenceSourceKind::DurableMemory
        ) && source.source_entries.len() == 1
            && source.source_identity
                == format!(
                    "session:{}:entry:{}",
                    source.source_session, source.source_entries[0]
                )
            && source
                .source_digests
                .first()
                .is_some_and(|value| crate::causal::valid_digest(value))
        {
            metadata.source_roots.push(EvidenceSourceRoot {
                source_session: source.source_session.clone(),
                source_entry: source.source_entries[0].clone(),
                source_digest: source.source_digests[0].clone(),
            });
        } else if let Some(entry) = source.source_entries.first() {
            gap(
                &mut metadata.gaps,
                &source.source_session,
                entry,
                SourceLineageGapReason::MissingSource,
            );
        }
    }
    metadata.effective = None;
    if !metadata
        .derived_from
        .iter()
        .any(|reference| reference.evidence_id == source.id)
    {
        if metadata.derived_from.len() >= 256 {
            if let Some(entry) = source.source_entries.first() {
                gap(
                    &mut metadata.gaps,
                    &source.source_session,
                    entry,
                    SourceLineageGapReason::Limit,
                );
            }
            return metadata;
        }
        metadata.derived_from.push(CandidateEvidenceReference {
            evidence_id: source.id.clone(),
            content_digest: source.content_digest.clone(),
            archive_revision: revision.max(1),
        });
    }
    metadata
}

pub(crate) fn merge_lineage(target: &mut EvidenceCausalMetadata, source: EvidenceCausalMetadata) {
    for root in source.source_roots {
        if !target.source_roots.contains(&root) {
            if target.source_roots.len() < 256 {
                target.source_roots.push(root);
            } else {
                gap(
                    &mut target.gaps,
                    &root.source_session,
                    &root.source_entry,
                    SourceLineageGapReason::Limit,
                );
            }
        }
    }
    for reference in source.derived_from {
        if !target
            .derived_from
            .iter()
            .any(|prior| prior.evidence_id == reference.evidence_id)
        {
            if target.derived_from.len() < 256 {
                target.derived_from.push(reference);
            } else if let Some(root) = target.source_roots.first() {
                gap(
                    &mut target.gaps,
                    &root.source_session,
                    &root.source_entry,
                    SourceLineageGapReason::Limit,
                );
            }
        }
    }
    for item in source.gaps {
        gap(
            &mut target.gaps,
            &item.source_session,
            &item.source_entry,
            item.reason,
        );
    }
    target.source_roots.sort();
}

fn record_for_entry(
    profile: &ProfileId,
    session: &SessionId,
    entry: &SessionEntry,
) -> Result<Option<EvidenceRecord>, ObservatoryError> {
    if let SessionEntryPayload::CompactionSummary { summary, .. }
    | SessionEntryPayload::CompactionCheckpoint { summary, .. }
    | SessionEntryPayload::Compaction { summary, .. } = &entry.payload
    {
        if summary.trim().is_empty()
            || summary.len() > ObservatoryLimits::default().max_record_bytes
        {
            return Ok(None);
        }
        return Ok(Some(EvidenceRecord::new(
            profile.clone(),
            session.clone(),
            vec![entry.id.clone()],
            vec![entry.checksum.clone()],
            format!("session:{session}:entry:{}", entry.id),
            entry.parent_id.clone(),
            EvidenceSourceKind::CompactionSummary,
            EvidenceAuthority::DerivedInference,
            summary.clone(),
            entry.timestamp,
            Sensitivity::Personal,
            RetentionClass::CurrentState,
            vec![],
        )));
    }
    crate::observatory::evidence_from_session_entry(
        profile,
        session,
        entry,
        ObservatoryLimits::default(),
    )
}

#[allow(clippy::too_many_lines, clippy::too_many_arguments)]
fn project_entry(
    profile: &ProfileId,
    session: &SessionId,
    entry: &SessionEntry,
    snapshot: &mut BTreeMap<EntityId, EvidenceRecord>,
    commitments: &crate::observatory::SourceCommitments,
    revision: u64,
    mutations: &mut Vec<ObservatoryMutation>,
    gaps: &mut Vec<SourceLineageGap>,
) -> Result<(), ObservatoryError> {
    let mut record = match record_for_entry(profile, session, entry) {
        Ok(Some(record)) => record,
        Ok(None) => return Ok(()),
        Err(ObservatoryError::InvalidEvidence) => {
            gap(gaps, session, &entry.id, SourceLineageGapReason::Limit);
            return Ok(());
        }
        Err(error) => return Err(error),
    };
    let mut metadata = EvidenceCausalMetadata {
        version: EVIDENCE_CAUSAL_VERSION,
        effective: None,
        source_roots: vec![],
        derived_from: vec![],
        gaps: vec![],
    };
    if let Some(origin) = &entry.copied_from {
        let source = direct_source(snapshot, &origin.session_id, &origin.entry_id);
        if origin.profile_id != *profile {
            gap(
                &mut metadata.gaps,
                &origin.session_id,
                &origin.entry_id,
                SourceLineageGapReason::CrossProfile,
            );
        } else if let Some(source) = source {
            if matches!(
                source.validity,
                EvidenceValidity::Deleted | EvidenceValidity::Superseded
            ) {
                gap(
                    &mut metadata.gaps,
                    &origin.session_id,
                    &origin.entry_id,
                    SourceLineageGapReason::DeletedSource,
                );
            } else if commitments
                .get(&(origin.session_id.clone(), origin.entry_id.clone()))
                .or_else(|| source.source_digests.first())
                != Some(&origin.checksum)
            {
                gap(
                    &mut metadata.gaps,
                    &origin.session_id,
                    &origin.entry_id,
                    SourceLineageGapReason::ConflictingSource,
                );
            } else {
                metadata = context_lineage(source, revision);
                record.authority = source.authority;
            }
        } else {
            gap(
                &mut metadata.gaps,
                &origin.session_id,
                &origin.entry_id,
                if commitments.contains_key(&(origin.session_id.clone(), origin.entry_id.clone())) {
                    SourceLineageGapReason::UnsupportedSource
                } else {
                    SourceLineageGapReason::MissingSource
                },
            );
        }
        if !metadata.gaps.is_empty() {
            record.authority = EvidenceAuthority::DerivedInference;
        }
    } else if let SessionEntryPayload::CompactionSummary { source_entries, .. }
    | SessionEntryPayload::CompactionCheckpoint { source_entries, .. } = &entry.payload
    {
        for id in source_entries {
            if let Some(source) = direct_source(snapshot, session, id) {
                if matches!(
                    source.validity,
                    EvidenceValidity::Deleted | EvidenceValidity::Superseded
                ) {
                    gap(
                        &mut metadata.gaps,
                        session,
                        id,
                        SourceLineageGapReason::DeletedSource,
                    );
                } else {
                    merge_lineage(&mut metadata, context_lineage(source, revision));
                }
            } else {
                gap(
                    &mut metadata.gaps,
                    session,
                    id,
                    if commitments.contains_key(&(session.clone(), id.clone())) {
                        SourceLineageGapReason::UnsupportedSource
                    } else {
                        SourceLineageGapReason::MissingSource
                    },
                );
            }
        }
    } else if matches!(&entry.payload, SessionEntryPayload::Compaction { .. }) {
        gap(
            &mut metadata.gaps,
            session,
            &entry.id,
            SourceLineageGapReason::UnsupportedSource,
        );
    } else if let SessionEntryPayload::ToolResult {
        call_id, content, ..
    } = &entry.payload
    {
        let internal = snapshot.values().any(|source| {
            source.source_session == *session
                && source.source_kind == EvidenceSourceKind::ToolCall
                && [
                    "memory_create",
                    "memory_search",
                    "memory_get",
                    "memory_correct",
                    "memory_context",
                    "memory_forget",
                ]
                .iter()
                .any(|name| {
                    source
                        .text
                        .starts_with(&format!("Tool {name} call {call_id}: "))
                })
        });
        let known_call = snapshot.values().any(|source| {
            source.source_session == *session
                && source.source_kind == EvidenceSourceKind::ToolCall
                && source.text.contains(&format!(" call {call_id}: "))
        });
        if !known_call {
            record.authority = EvidenceAuthority::DerivedInference;
            gap(
                &mut metadata.gaps,
                session,
                entry.parent_id.as_ref().unwrap_or(&entry.id),
                SourceLineageGapReason::MissingSource,
            );
        }
        if internal {
            record.authority = EvidenceAuthority::DerivedInference;
            for block in content {
                if let ContentBlock::Text { text } = block
                    && let Ok(value) = serde_json::from_str::<serde_json::Value>(text)
                {
                    transcluded_lineage(
                        &value,
                        profile,
                        session,
                        &entry.id,
                        snapshot,
                        revision,
                        &mut metadata,
                        0,
                    );
                }
            }
            if metadata.source_roots.is_empty() {
                gap(
                    &mut metadata.gaps,
                    session,
                    &entry.id,
                    SourceLineageGapReason::UnsupportedSource,
                );
            }
        }
    }
    if entry.copied_from.is_none()
        && !matches!(
            entry.payload,
            SessionEntryPayload::CompactionSummary { .. }
                | SessionEntryPayload::CompactionCheckpoint { .. }
                | SessionEntryPayload::Compaction { .. }
        )
        && metadata.source_roots.is_empty()
        && metadata.gaps.is_empty()
    {
        metadata.source_roots.push(EvidenceSourceRoot {
            source_session: session.clone(),
            source_entry: entry.id.clone(),
            source_digest: entry.checksum.clone(),
        });
    }
    for item in &metadata.gaps {
        gap(gaps, &item.source_session, &item.source_entry, item.reason);
    }
    if let Some(existing) = direct_source(snapshot, session, &entry.id).cloned() {
        let legacy_summary_matches = matches!(&entry.payload, SessionEntryPayload::CompactionSummary { summary, source_entries, .. }
            if existing.source_identity == format!("session:{session}:compaction-summary:{}", entry.id)
                && existing.text == *summary && existing.source_entries == *source_entries
                && existing.source_digests == source_entries.iter().map(|id| format!("{}:{id}", entry.checksum)).collect::<Vec<_>>());
        if existing.source_digests.first() != Some(&entry.checksum) && !legacy_summary_matches {
            return Err(ObservatoryError::InvalidEvidence);
        }
        if matches!(
            existing.validity,
            EvidenceValidity::Deleted | EvidenceValidity::Superseded
        ) {
            gap(
                gaps,
                session,
                &entry.id,
                SourceLineageGapReason::DeletedSource,
            );
            return Ok(());
        }
        // Re-resolution may add known roots and remove resolved gaps, never upgrade
        // a previously generated claim or discard established original roots.
        if let Some(prior) = &existing.causal {
            metadata.effective.clone_from(&prior.effective);
            let resolved_roots =
                std::mem::replace(&mut metadata.source_roots, prior.source_roots.clone());
            for root in resolved_roots {
                if !metadata.source_roots.contains(&root) && metadata.source_roots.len() < 256 {
                    metadata.source_roots.push(root.clone());
                } else if !metadata.source_roots.contains(&root) {
                    gap(
                        &mut metadata.gaps,
                        &root.source_session,
                        &root.source_entry,
                        SourceLineageGapReason::Limit,
                    );
                }
            }
            for reference in &mut metadata.derived_from {
                if let Some(prior_ref) = prior.derived_from.iter().find(|prior_ref| {
                    prior_ref.evidence_id == reference.evidence_id
                        && prior_ref.content_digest == reference.content_digest
                }) {
                    *reference = prior_ref.clone();
                }
            }
        }
        metadata.source_roots.sort();
        let authority = (record.authority == EvidenceAuthority::DerivedInference
            && existing.authority != EvidenceAuthority::DerivedInference)
            .then_some(EvidenceAuthority::DerivedInference);
        if existing.causal.as_ref() != Some(&metadata) || authority.is_some() {
            mutations.push(ObservatoryMutation::AnnotateProvenance {
                evidence_id: existing.id.clone(),
                metadata: metadata.clone(),
                authority,
            });
            let mut updated = existing;
            updated.causal = Some(metadata);
            if let Some(authority) = authority {
                updated.authority = authority;
            }
            snapshot.insert(updated.id.clone(), updated);
        }
        return Ok(());
    }
    record.causal = Some(metadata);
    snapshot.insert(record.id.clone(), record.clone());
    mutations.push(ObservatoryMutation::Observe(record));
    Ok(())
}

#[allow(clippy::too_many_arguments)]
fn transcluded_lineage(
    value: &serde_json::Value,
    profile: &ProfileId,
    session: &SessionId,
    entry: &EntryId,
    snapshot: &BTreeMap<EntityId, EvidenceRecord>,
    revision: u64,
    metadata: &mut EvidenceCausalMetadata,
    depth: usize,
) {
    if depth > 8 {
        gap(
            &mut metadata.gaps,
            session,
            entry,
            SourceLineageGapReason::Limit,
        );
        return;
    }
    if let Ok(proposed) = serde_json::from_value::<EvidenceRecord>(value.clone()) {
        if proposed.profile_id != *profile {
            gap(
                &mut metadata.gaps,
                session,
                entry,
                SourceLineageGapReason::CrossProfile,
            );
        } else if let Some(source) = snapshot.get(&proposed.id) {
            if matches!(
                source.validity,
                EvidenceValidity::Deleted | EvidenceValidity::Superseded
            ) {
                gap(
                    &mut metadata.gaps,
                    session,
                    entry,
                    SourceLineageGapReason::DeletedSource,
                );
            } else if source.content_digest == proposed.content_digest
                && source.text == proposed.text
                && source.authority == proposed.authority
            {
                merge_lineage(metadata, context_lineage(source, revision));
            } else {
                gap(
                    &mut metadata.gaps,
                    session,
                    entry,
                    SourceLineageGapReason::ConflictingSource,
                );
            }
        } else {
            gap(
                &mut metadata.gaps,
                session,
                entry,
                SourceLineageGapReason::MissingSource,
            );
        }
    } else {
        match value {
            serde_json::Value::Array(values) => {
                if values.len() > 256 {
                    gap(
                        &mut metadata.gaps,
                        session,
                        entry,
                        SourceLineageGapReason::Limit,
                    );
                }
                for child in values.iter().take(256) {
                    transcluded_lineage(
                        child,
                        profile,
                        session,
                        entry,
                        snapshot,
                        revision,
                        metadata,
                        depth + 1,
                    );
                }
            }
            serde_json::Value::Object(values) => {
                if values.len() > 256 {
                    gap(
                        &mut metadata.gaps,
                        session,
                        entry,
                        SourceLineageGapReason::Limit,
                    );
                }
                for child in values.values().take(256) {
                    transcluded_lineage(
                        child,
                        profile,
                        session,
                        entry,
                        snapshot,
                        revision,
                        metadata,
                        depth + 1,
                    );
                }
            }
            _ => {}
        }
    }
}

#[cfg(test)]
#[path = "ingestion_tests.rs"]
mod tests;
