//! Optional orchestration of canonical session intake. Memory owns durable progress;
//! these hints only reduce the delay before a fair replay pass visits a session.

use std::collections::VecDeque;

use keith_agent_types::{SessionId, UtcTimestamp};
use keith_session_store::{CommittedSourceLimits, SessionManifest};

use crate::{LocalRuntime, LocalRuntimeError, module_error};

const MAX_HINTS: usize = 64;
pub(super) const MAX_REPLAY_PAGES: usize = 4;
pub(super) const REPLAY_PAGE_ENTRIES: usize = 128;
pub(super) const REPLAY_PAGE_BYTES: usize = 1024 * 1024;

#[derive(Default)]
pub(super) struct MemoryIntakeState {
    hints: VecDeque<SessionId>,
    last_session: Option<SessionId>,
}

impl MemoryIntakeState {
    fn schedule(&mut self, session_id: &SessionId) {
        if !self.hints.contains(session_id) {
            if self.hints.len() == MAX_HINTS {
                self.hints.pop_front();
            }
            self.hints.push_back(session_id.clone());
        }
    }

    fn select(&mut self, sessions: &[SessionManifest]) -> Vec<SessionId> {
        let mut selected = Vec::with_capacity(MAX_REPLAY_PAGES);
        // Hints consume at most one slot; regular catalogue work cannot be starved by
        // a stream of new turns or a repeatedly failing session.
        while let Some(hint) = self.hints.pop_front() {
            if sessions.iter().any(|session| session.session_id == hint) {
                selected.push(hint);
                break;
            }
        }
        while selected.len() < MAX_REPLAY_PAGES {
            let eligible = |session: &&SessionManifest| !selected.contains(&session.session_id);
            let next = sessions
                .iter()
                .filter(eligible)
                .find(|session| {
                    self.last_session
                        .as_ref()
                        .is_none_or(|last| session.session_id > *last)
                })
                .or_else(|| sessions.iter().find(eligible));
            let Some(next) = next else {
                break;
            };
            self.last_session = Some(next.session_id.clone());
            selected.push(next.session_id.clone());
        }
        selected
    }
}

#[derive(Debug, Default)]
pub(super) struct MemoryReplayProgress {
    pub sessions_attempted: usize,
    pub pages_read: usize,
    pub entries_read: usize,
    pub bytes_read: usize,
    pub entries_processed: u64,
    pub failed_sessions: usize,
}

impl LocalRuntime {
    pub(super) fn schedule_memory_intake(&self, session_id: &SessionId) {
        // This lock protects scheduling hints only, never canonical progress. A
        // poisoned hint queue must not permanently disable authoritative replay.
        self.memory_intake
            .lock()
            .unwrap_or_else(std::sync::PoisonError::into_inner)
            .schedule(session_id);
    }

    /// Reuses the runtime's already-discovered, session-ID-sorted catalogue. Discovery
    /// itself still reads the catalogue; only source-page work is bounded here. The
    /// existing memory vault may also require a full refresh under its own lock.
    pub(super) fn replay_memory_intake(
        &self,
        sessions: &[SessionManifest],
        now: UtcTimestamp,
    ) -> MemoryReplayProgress {
        let mut progress = MemoryReplayProgress::default();
        let selected = self
            .memory_intake
            .lock()
            .unwrap_or_else(std::sync::PoisonError::into_inner)
            .select(sessions);
        for session_id in selected {
            progress.sessions_attempted += 1;
            if self
                .replay_memory_session(&session_id, now, &mut progress)
                .is_err()
            {
                // Do not consume memory's durable cursor on failure, and do not let
                // optional historical intake become a terminal turn/startup veto.
                progress.failed_sessions += 1;
            }
        }
        progress
    }

    fn replay_memory_session(
        &self,
        session_id: &SessionId,
        now: UtcTimestamp,
        progress: &mut MemoryReplayProgress,
    ) -> Result<(), LocalRuntimeError> {
        let manifest = self.owned_manifest(session_id)?;
        let profile = self.profile(&manifest.profile_id)?;
        if !profile.enabled {
            return Ok(());
        }
        if profile.profile.workspace_id != manifest.workspace_id {
            return Err(LocalRuntimeError::SessionProfileMismatch(
                session_id.clone(),
                manifest.profile_id,
            ));
        }
        let modules = self.profile_modules(&profile)?;
        let _ = modules.memory.repair_ingestion_projection(now);
        let cursor = modules
            .memory
            .committed_source_cursor(session_id)
            .map_err(module_error)?;
        let page = self.sessions.committed_source_page(
            &manifest.profile_id,
            session_id,
            cursor.as_ref(),
            CommittedSourceLimits {
                max_entries: REPLAY_PAGE_ENTRIES,
                max_bytes: REPLAY_PAGE_BYTES,
            },
        )?;
        progress.pages_read += 1;
        progress.entries_read += page.entries().len();
        progress.bytes_read += page.bytes_read();
        if page.profile_id() != &manifest.profile_id
            || page.workspace_id() != &manifest.workspace_id
            || page.root_tree_id() != &manifest.root_tree_id
            || page.session_id() != session_id
        {
            return Err(LocalRuntimeError::Invalid(
                "committed source page changed runtime scope".into(),
            ));
        }
        let receipt = modules
            .memory
            .ingest_committed_page(&page, now)
            .map_err(module_error)?;
        progress.entries_processed = progress
            .entries_processed
            .saturating_add(u64::try_from(receipt.processed_entries).unwrap_or(u64::MAX));
        Ok(())
    }
}
