#![forbid(unsafe_code)]

use std::collections::{BTreeMap, BTreeSet};
use std::fmt::Write as _;
use std::fs;
use std::path::{Path, PathBuf};
use std::str::FromStr;
use std::sync::{Arc, Mutex, MutexGuard, RwLock};

use keith_agent_types::{EntityId, ProfileId, UtcTimestamp};
use rusqlite::{Connection, OptionalExtension, params};
use serde::{Deserialize, Serialize};
use sha2::{Digest, Sha256};
use thiserror::Error;

const INDEX_FILE: &str = "retrieval.sqlite3";
const INDEX_SCHEMA: i64 = 1;

#[derive(Clone, Copy, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum SearchSourceKind {
    DurableMemory,
    DailyMemory,
    CurrentState,
    Knowledge,
    SessionSummary,
    Skill,
}

#[derive(Clone, Debug, Eq, PartialEq)]
pub struct SourceInput {
    pub source_path: String,
    pub source_version: String,
    pub source_kind: SearchSourceKind,
    pub modified_at: UtcTimestamp,
    pub text: String,
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct SearchDocument {
    pub document_id: EntityId,
    pub profile_id: ProfileId,
    pub source_path: String,
    pub source_version: String,
    pub heading_path: Vec<String>,
    pub text: String,
    pub source_kind: SearchSourceKind,
    pub modified_at: UtcTimestamp,
}

#[derive(Clone, Debug, PartialEq)]
pub struct SearchResult {
    pub document_id: EntityId,
    pub source_path: String,
    pub source_version: String,
    pub heading_path: Vec<String>,
    pub excerpt: String,
    pub source_kind: SearchSourceKind,
    pub modified_at: UtcTimestamp,
    pub lexical_score: f32,
    pub trigram_score: f32,
    pub vector_score: Option<f32>,
    pub merged_score: f32,
}

#[derive(Clone, Copy, Debug, PartialEq)]
pub struct RankWeights {
    pub lexical: f32,
    pub trigram: f32,
    pub vector: f32,
}

impl Default for RankWeights {
    fn default() -> Self {
        Self {
            lexical: 0.45,
            trigram: 0.35,
            vector: 0.20,
        }
    }
}

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub struct RetrievalLimits {
    pub max_source_bytes: usize,
    pub max_chunk_bytes: usize,
    pub max_documents_per_profile: usize,
    pub max_scan_results: usize,
    pub max_excerpt_chars: usize,
}

impl Default for RetrievalLimits {
    fn default() -> Self {
        Self {
            max_source_bytes: 4 * 1_024 * 1_024,
            max_chunk_bytes: 8 * 1_024,
            max_documents_per_profile: 100_000,
            max_scan_results: 10_000,
            max_excerpt_chars: 480,
        }
    }
}

#[derive(Clone, Debug, Eq, PartialEq)]
pub struct RetrievalHealth {
    pub degraded: bool,
    pub vector_available: bool,
    pub detail: Option<String>,
    pub quarantined_index: Option<PathBuf>,
}

#[derive(Clone, Debug, PartialEq)]
pub struct VectorRecord {
    pub document_id: EntityId,
    pub profile_id: ProfileId,
    pub source_path: String,
    pub vector: Vec<f32>,
}

#[derive(Clone, Debug, PartialEq)]
pub struct VectorHit {
    pub document_id: EntityId,
    pub score: f32,
}

pub trait Embedder: Send + Sync {
    fn dimensions(&self) -> usize;
    /// # Errors
    ///
    /// Returns an error when local or external embedding production is unavailable.
    fn embed(&self, text: &str) -> Result<Vec<f32>, RetrievalError>;
}

pub trait VectorIndex: Send + Sync {
    /// # Errors
    ///
    /// Returns an error when vector records cannot be stored.
    fn upsert(&self, records: Vec<VectorRecord>) -> Result<(), RetrievalError>;
    /// # Errors
    ///
    /// Returns an error when source projections cannot be removed.
    fn remove_source(
        &self,
        profile_id: &ProfileId,
        source_path: &str,
    ) -> Result<(), RetrievalError>;
    /// # Errors
    ///
    /// Returns an error when the vector index is unavailable or rejects the query.
    fn search(
        &self,
        profile_id: &ProfileId,
        query: &[f32],
        limit: usize,
    ) -> Result<Vec<VectorHit>, RetrievalError>;
}

#[derive(Clone)]
pub struct VectorComponents {
    pub embedder: Arc<dyn Embedder>,
    pub index: Arc<dyn VectorIndex>,
}

#[derive(Debug, Error)]
pub enum RetrievalError {
    #[error("retrieval I/O failed: {0}")]
    Io(#[from] std::io::Error),
    #[error("retrieval database failed: {0}")]
    Sql(#[from] rusqlite::Error),
    #[error("retrieval JSON failed: {0}")]
    Json(#[from] serde_json::Error),
    #[error("retrieval state lock was poisoned")]
    LockPoisoned,
    #[error("retrieval policy contains an invalid bound or weight")]
    InvalidPolicy,
    #[error("search query must be non-empty")]
    EmptyQuery,
    #[error("source is oversized, invalid, or outside the supported workspace set")]
    InvalidSource,
    #[error("stored retrieval identity is invalid")]
    InvalidIdentity,
    #[error("vector backend failed: {0}")]
    Vector(String),
}

pub struct RetrievalService {
    connection: Mutex<Connection>,
    limits: RetrievalLimits,
    weights: RankWeights,
    vectors: Option<VectorComponents>,
    health: Mutex<RetrievalHealth>,
}

impl RetrievalService {
    /// Opens or creates the derived retrieval index, quarantining corrupt database bytes.
    ///
    /// # Errors
    ///
    /// Returns an error when the index directory or fresh replacement database cannot be opened.
    pub fn open(
        index_root: impl AsRef<Path>,
        limits: RetrievalLimits,
        weights: RankWeights,
        vectors: Option<VectorComponents>,
    ) -> Result<Self, RetrievalError> {
        validate_configuration(limits, weights)?;
        fs::create_dir_all(index_root.as_ref())?;
        let index_path = index_root.as_ref().join(INDEX_FILE);
        let (connection, quarantined_index, detail) = open_or_quarantine(&index_path)?;
        let degraded = quarantined_index.is_some();
        Ok(Self {
            connection: Mutex::new(connection),
            limits,
            weights,
            health: Mutex::new(RetrievalHealth {
                degraded,
                vector_available: vectors.is_some(),
                detail,
                quarantined_index,
            }),
            vectors,
        })
    }

    /// Parses and atomically replaces all supplied source versions for one profile.
    ///
    /// # Errors
    ///
    /// Returns an error for invalid sources, database failures, or poisoned state.
    pub fn index_sources(
        &self,
        profile_id: &ProfileId,
        sources: &[SourceInput],
    ) -> Result<usize, RetrievalError> {
        let mut parsed = Vec::new();
        for source in sources {
            validate_source(source, self.limits)?;
            parsed.extend(parse_source(
                profile_id,
                source,
                self.limits.max_chunk_bytes,
            ));
        }
        if parsed.len() > self.limits.max_documents_per_profile {
            return Err(RetrievalError::InvalidSource);
        }
        self.replace_documents(profile_id, &parsed)?;
        self.index_vectors(&parsed);
        let mut health = self.health()?;
        health.degraded = false;
        if health.detail.as_deref() == Some("derived index was quarantined") {
            health.detail = None;
        }
        Ok(parsed.len())
    }

    /// Rebuilds supported memory, state, knowledge, summary, and skill sources from readable files.
    ///
    /// # Errors
    ///
    /// Returns an error for unsafe/oversized sources, filesystem failure, or index failure.
    pub fn rebuild_workspace(
        &self,
        profile_id: &ProfileId,
        workspace_root: impl AsRef<Path>,
        modified_at: UtcTimestamp,
    ) -> Result<usize, RetrievalError> {
        let sources = collect_workspace_sources(workspace_root.as_ref(), modified_at, self.limits)?;
        let mut connection = self.connection()?;
        let transaction = connection.transaction()?;
        transaction.execute(
            "DELETE FROM documents WHERE profile_id = ?1",
            params![profile_id.to_string()],
        )?;
        transaction.execute(
            "INSERT INTO documents_fts(documents_fts) VALUES('rebuild')",
            [],
        )?;
        transaction.commit()?;
        drop(connection);
        self.index_sources(profile_id, &sources)
    }

    /// Removes a source and all lexical, trigram, and vector projections for one profile.
    ///
    /// # Errors
    ///
    /// Returns an error when durable derived state cannot be updated.
    pub fn remove_source(
        &self,
        profile_id: &ProfileId,
        source_path: &str,
    ) -> Result<(), RetrievalError> {
        let mut connection = self.connection()?;
        let transaction = connection.transaction()?;
        transaction.execute(
            "DELETE FROM documents WHERE profile_id = ?1 AND source_path = ?2",
            params![profile_id.to_string(), source_path],
        )?;
        transaction.execute(
            "INSERT INTO documents_fts(documents_fts) VALUES('rebuild')",
            [],
        )?;
        transaction.commit()?;
        drop(connection);
        if let Some(vectors) = &self.vectors
            && let Err(error) = vectors.index.remove_source(profile_id, source_path)
        {
            self.vector_failed(error.to_string());
        }
        Ok(())
    }

    /// Strictly removes lexical, trigram, and vector projections for a deleted source.
    ///
    /// Unlike ordinary degraded retrieval, source deletion must not hide a vector cleanup
    /// failure because that could retain data after the source is gone.
    ///
    /// # Errors
    ///
    /// Returns an error when any associated projection cannot be removed.
    pub fn purge_source(
        &self,
        profile_id: &ProfileId,
        source_path: &str,
    ) -> Result<(), RetrievalError> {
        let mut connection = self.connection()?;
        let transaction = connection.transaction()?;
        transaction.execute(
            "DELETE FROM documents WHERE profile_id = ?1 AND source_path = ?2",
            params![profile_id.to_string(), source_path],
        )?;
        transaction.execute(
            "INSERT INTO documents_fts(documents_fts) VALUES('rebuild')",
            [],
        )?;
        transaction.commit()?;
        drop(connection);
        if let Some(vectors) = &self.vectors {
            vectors.index.remove_source(profile_id, source_path)?;
        }
        Ok(())
    }

    /// Searches one profile with normalized lexical, Unicode trigram, and optional vector ranks.
    ///
    /// The merger uses bounded scores in `[0, 1]` and renormalizes available weights when the
    /// optional vector path is absent or unhealthy.
    ///
    /// # Errors
    ///
    /// Returns an error for an empty query, database failure, or invalid persisted identity.
    pub fn search(
        &self,
        profile_id: &ProfileId,
        query: &str,
        limit: usize,
    ) -> Result<Vec<SearchResult>, RetrievalError> {
        if query.trim().is_empty() {
            return Err(RetrievalError::EmptyQuery);
        }
        let documents = self.load_documents(profile_id)?;
        let lexical = self.lexical_scores(profile_id, query)?;
        let vector = self.vector_scores(profile_id, query, limit.max(32));
        let query_normalized = normalize(query);
        let query_trigrams = trigrams(&query_normalized);
        let mut results = documents
            .into_iter()
            .filter_map(|document| {
                score_document(
                    document,
                    &query_normalized,
                    &query_trigrams,
                    &lexical,
                    vector.as_ref(),
                    self.weights,
                    self.limits.max_excerpt_chars,
                )
            })
            .collect::<Vec<_>>();
        results.sort_by(|left, right| {
            right
                .merged_score
                .total_cmp(&left.merged_score)
                .then_with(|| left.source_path.cmp(&right.source_path))
        });
        results.truncate(limit);
        Ok(results)
    }

    /// Returns current degradation, quarantine, and optional-vector availability state.
    ///
    /// # Errors
    ///
    /// Returns an error when the health lock is poisoned.
    pub fn health_snapshot(&self) -> Result<RetrievalHealth, RetrievalError> {
        Ok(self.health()?.clone())
    }

    fn replace_documents(
        &self,
        profile_id: &ProfileId,
        documents: &[SearchDocument],
    ) -> Result<(), RetrievalError> {
        let paths = documents
            .iter()
            .map(|document| document.source_path.as_str())
            .collect::<BTreeSet<_>>();
        let mut connection = self.connection()?;
        let transaction = connection.transaction()?;
        for path in paths {
            transaction.execute(
                "DELETE FROM documents WHERE profile_id = ?1 AND source_path = ?2",
                params![profile_id.to_string(), path],
            )?;
        }
        for document in documents {
            transaction.execute(
                "INSERT INTO documents (
                    document_id, profile_id, source_path, source_version, heading_path,
                    text, source_kind, modified_at
                 ) VALUES (?1, ?2, ?3, ?4, ?5, ?6, ?7, ?8)",
                params![
                    document.document_id.to_string(),
                    document.profile_id.to_string(),
                    document.source_path,
                    document.source_version,
                    serde_json::to_string(&document.heading_path)?,
                    document.text,
                    source_kind_name(document.source_kind),
                    document.modified_at.unix_millis(),
                ],
            )?;
        }
        transaction.execute(
            "INSERT INTO documents_fts(documents_fts) VALUES('rebuild')",
            [],
        )?;
        transaction.commit()?;
        Ok(())
    }

    fn index_vectors(&self, documents: &[SearchDocument]) {
        let Some(vectors) = &self.vectors else {
            return;
        };
        let records = documents
            .iter()
            .map(|document| {
                vectors
                    .embedder
                    .embed(&document.text)
                    .map(|vector| VectorRecord {
                        document_id: document.document_id.clone(),
                        profile_id: document.profile_id.clone(),
                        source_path: document.source_path.clone(),
                        vector,
                    })
            })
            .collect::<Result<Vec<_>, _>>()
            .and_then(|records| vectors.index.upsert(records));
        if let Err(error) = records {
            self.vector_failed(error.to_string());
        }
    }

    fn vector_scores(
        &self,
        profile_id: &ProfileId,
        query: &str,
        limit: usize,
    ) -> Option<BTreeMap<EntityId, f32>> {
        let vectors = self.vectors.as_ref()?;
        let result = vectors
            .embedder
            .embed(query)
            .and_then(|embedding| vectors.index.search(profile_id, &embedding, limit));
        match result {
            Ok(hits) => Some(
                hits.into_iter()
                    .map(|hit| (hit.document_id, hit.score.clamp(0.0, 1.0)))
                    .collect(),
            ),
            Err(error) => {
                self.vector_failed(error.to_string());
                None
            }
        }
    }

    fn lexical_scores(
        &self,
        profile_id: &ProfileId,
        query: &str,
    ) -> Result<BTreeMap<EntityId, f32>, RetrievalError> {
        let fts_query = query
            .split_whitespace()
            .filter(|token| !token.is_empty())
            .map(|token| format!("\"{}\"", token.replace('"', "\"\"")))
            .collect::<Vec<_>>()
            .join(" OR ");
        if fts_query.is_empty() {
            return Ok(BTreeMap::new());
        }
        let connection = self.connection()?;
        let mut statement = connection.prepare(
            "SELECT d.document_id, bm25(documents_fts)
             FROM documents_fts
             JOIN documents d ON d.row_id = documents_fts.rowid
             WHERE documents_fts MATCH ?1 AND d.profile_id = ?2
             LIMIT ?3",
        )?;
        let rows = statement.query_map(
            params![
                fts_query,
                profile_id.to_string(),
                i64::try_from(self.limits.max_scan_results).unwrap_or(i64::MAX)
            ],
            |row| Ok((row.get::<_, String>(0)?, row.get::<_, f32>(1)?)),
        )?;
        let mut scores = BTreeMap::new();
        for row in rows {
            let (id, rank) = row?;
            scores.insert(
                EntityId::from_str(&id).map_err(|_| RetrievalError::InvalidIdentity)?,
                1.0 / (1.0 + rank.abs()),
            );
        }
        Ok(scores)
    }

    fn load_documents(
        &self,
        profile_id: &ProfileId,
    ) -> Result<Vec<SearchDocument>, RetrievalError> {
        let connection = self.connection()?;
        let mut statement = connection.prepare(
            "SELECT document_id, profile_id, source_path, source_version, heading_path,
                    text, source_kind, modified_at
             FROM documents WHERE profile_id = ?1 ORDER BY row_id LIMIT ?2",
        )?;
        let rows = statement.query_map(
            params![
                profile_id.to_string(),
                i64::try_from(self.limits.max_scan_results).unwrap_or(i64::MAX)
            ],
            stored_document,
        )?;
        rows.map(|row| row.map_err(RetrievalError::from)).collect()
    }

    fn vector_failed(&self, detail: String) {
        if let Ok(mut health) = self.health.lock() {
            health.vector_available = false;
            health.detail = Some(detail);
        }
    }

    fn connection(&self) -> Result<MutexGuard<'_, Connection>, RetrievalError> {
        self.connection
            .lock()
            .map_err(|_| RetrievalError::LockPoisoned)
    }

    fn health(&self) -> Result<MutexGuard<'_, RetrievalHealth>, RetrievalError> {
        self.health.lock().map_err(|_| RetrievalError::LockPoisoned)
    }
}

#[derive(Clone, Debug)]
pub struct LocalHashEmbedder {
    dimensions: usize,
}

impl LocalHashEmbedder {
    /// Creates a deterministic local Unicode n-gram embedding model.
    ///
    /// # Errors
    ///
    /// Returns an error when the dimension is too small for useful projection.
    pub fn new(dimensions: usize) -> Result<Self, RetrievalError> {
        if dimensions < 16 {
            Err(RetrievalError::InvalidPolicy)
        } else {
            Ok(Self { dimensions })
        }
    }
}

impl Embedder for LocalHashEmbedder {
    fn dimensions(&self) -> usize {
        self.dimensions
    }

    fn embed(&self, text: &str) -> Result<Vec<f32>, RetrievalError> {
        let normalized = normalize(text);
        let features = semantic_features(&normalized);
        let mut vector = vec![0.0_f32; self.dimensions];
        for feature in features {
            let digest = Sha256::digest(feature.as_bytes());
            let mut bytes = [0_u8; 8];
            bytes.copy_from_slice(&digest[..8]);
            let hash = u64::from_le_bytes(bytes);
            let dimensions =
                u64::try_from(self.dimensions).map_err(|_| RetrievalError::InvalidPolicy)?;
            let index =
                usize::try_from(hash % dimensions).map_err(|_| RetrievalError::InvalidPolicy)?;
            let sign = if digest[8] & 1 == 0 { 1.0 } else { -1.0 };
            vector[index] += sign;
        }
        normalize_vector(&mut vector);
        Ok(vector)
    }
}

#[derive(Default)]
pub struct MemoryVectorIndex {
    records: RwLock<BTreeMap<EntityId, VectorRecord>>,
}

impl VectorIndex for MemoryVectorIndex {
    fn upsert(&self, records: Vec<VectorRecord>) -> Result<(), RetrievalError> {
        let mut state = self
            .records
            .write()
            .map_err(|_| RetrievalError::LockPoisoned)?;
        for record in records {
            state.insert(record.document_id.clone(), record);
        }
        Ok(())
    }

    fn remove_source(
        &self,
        profile_id: &ProfileId,
        source_path: &str,
    ) -> Result<(), RetrievalError> {
        self.records
            .write()
            .map_err(|_| RetrievalError::LockPoisoned)?
            .retain(|_, record| {
                &record.profile_id != profile_id || record.source_path != source_path
            });
        Ok(())
    }

    fn search(
        &self,
        profile_id: &ProfileId,
        query: &[f32],
        limit: usize,
    ) -> Result<Vec<VectorHit>, RetrievalError> {
        let state = self
            .records
            .read()
            .map_err(|_| RetrievalError::LockPoisoned)?;
        let mut hits = state
            .values()
            .filter(|record| &record.profile_id == profile_id && record.vector.len() == query.len())
            .map(|record| VectorHit {
                document_id: record.document_id.clone(),
                score: f32::midpoint(cosine(query, &record.vector), 1.0).clamp(0.0, 1.0),
            })
            .collect::<Vec<_>>();
        hits.sort_by(|left, right| right.score.total_cmp(&left.score));
        hits.truncate(limit);
        Ok(hits)
    }
}

fn validate_configuration(
    limits: RetrievalLimits,
    weights: RankWeights,
) -> Result<(), RetrievalError> {
    let sum = weights.lexical + weights.trigram + weights.vector;
    if limits.max_source_bytes == 0
        || limits.max_chunk_bytes == 0
        || limits.max_documents_per_profile == 0
        || limits.max_scan_results == 0
        || limits.max_excerpt_chars == 0
        || weights.lexical < 0.0
        || weights.trigram < 0.0
        || weights.vector < 0.0
        || sum <= f32::EPSILON
    {
        Err(RetrievalError::InvalidPolicy)
    } else {
        Ok(())
    }
}

fn open_or_quarantine(
    index_path: &Path,
) -> Result<(Connection, Option<PathBuf>, Option<String>), RetrievalError> {
    match open_healthy(index_path) {
        Ok(connection) => Ok((connection, None, None)),
        Err(_) if index_path.exists() => {
            let quarantine =
                index_path.with_file_name(format!("retrieval.corrupt-{}.sqlite3", EntityId::new()));
            fs::rename(index_path, &quarantine)?;
            let connection = open_healthy(index_path)?;
            Ok((
                connection,
                Some(quarantine),
                Some("derived index was quarantined".into()),
            ))
        }
        Err(error) => Err(error),
    }
}

fn open_healthy(path: &Path) -> Result<Connection, RetrievalError> {
    let connection = Connection::open(path)?;
    connection.execute_batch(
        "PRAGMA journal_mode = WAL;
         PRAGMA foreign_keys = ON;
         CREATE TABLE IF NOT EXISTS metadata (
            key TEXT PRIMARY KEY,
            value INTEGER NOT NULL
         );
         CREATE TABLE IF NOT EXISTS documents (
            row_id INTEGER PRIMARY KEY,
            document_id TEXT NOT NULL UNIQUE,
            profile_id TEXT NOT NULL,
            source_path TEXT NOT NULL,
            source_version TEXT NOT NULL,
            heading_path TEXT NOT NULL,
            text TEXT NOT NULL,
            source_kind TEXT NOT NULL,
            modified_at INTEGER NOT NULL
         );
         CREATE INDEX IF NOT EXISTS documents_profile_path
            ON documents(profile_id, source_path);
         CREATE VIRTUAL TABLE IF NOT EXISTS documents_fts USING fts5(
            text,
            content='documents',
            content_rowid='row_id',
            tokenize='unicode61 remove_diacritics 2'
         );",
    )?;
    let existing = connection
        .query_row(
            "SELECT value FROM metadata WHERE key = 'schema'",
            [],
            |row| row.get::<_, i64>(0),
        )
        .optional()?;
    match existing {
        Some(version) if version != INDEX_SCHEMA => return Err(RetrievalError::InvalidPolicy),
        Some(_) => {}
        None => {
            connection.execute(
                "INSERT INTO metadata(key, value) VALUES('schema', ?1)",
                params![INDEX_SCHEMA],
            )?;
        }
    }
    let check = connection.query_row("PRAGMA quick_check", [], |row| row.get::<_, String>(0))?;
    if check != "ok" {
        return Err(RetrievalError::InvalidPolicy);
    }
    Ok(connection)
}

fn validate_source(source: &SourceInput, limits: RetrievalLimits) -> Result<(), RetrievalError> {
    if source.source_path.trim().is_empty()
        || source.source_version.trim().is_empty()
        || source.text.len() > limits.max_source_bytes
        || source.source_path.starts_with('/')
        || source.source_path.split('/').any(|part| part == "..")
    {
        Err(RetrievalError::InvalidSource)
    } else {
        Ok(())
    }
}

fn parse_source(
    profile_id: &ProfileId,
    source: &SourceInput,
    max_chunk_bytes: usize,
) -> Vec<SearchDocument> {
    let mut headings = Vec::<String>::new();
    let mut buffer = String::new();
    let mut documents = Vec::new();
    for line in source.text.lines() {
        if let Some((level, title)) = markdown_heading(line) {
            flush_chunks(
                profile_id,
                source,
                &headings,
                &mut buffer,
                max_chunk_bytes,
                &mut documents,
            );
            headings.truncate(level.saturating_sub(1));
            headings.push(title.to_owned());
        } else {
            if !buffer.is_empty() {
                buffer.push('\n');
            }
            buffer.push_str(line);
            if buffer.len() >= max_chunk_bytes {
                flush_chunks(
                    profile_id,
                    source,
                    &headings,
                    &mut buffer,
                    max_chunk_bytes,
                    &mut documents,
                );
            }
        }
    }
    flush_chunks(
        profile_id,
        source,
        &headings,
        &mut buffer,
        max_chunk_bytes,
        &mut documents,
    );
    documents
}

fn markdown_heading(line: &str) -> Option<(usize, &str)> {
    let hashes = line.bytes().take_while(|byte| *byte == b'#').count();
    if (1..=6).contains(&hashes) && line.as_bytes().get(hashes) == Some(&b' ') {
        let title = line[hashes + 1..].trim();
        (!title.is_empty()).then_some((hashes, title))
    } else {
        None
    }
}

fn flush_chunks(
    profile_id: &ProfileId,
    source: &SourceInput,
    headings: &[String],
    buffer: &mut String,
    max_chunk_bytes: usize,
    documents: &mut Vec<SearchDocument>,
) {
    let text = buffer.trim();
    if text.is_empty() {
        buffer.clear();
        return;
    }
    for chunk in bounded_chunks(text, max_chunk_bytes) {
        documents.push(SearchDocument {
            document_id: EntityId::new(),
            profile_id: profile_id.clone(),
            source_path: source.source_path.clone(),
            source_version: source.source_version.clone(),
            heading_path: headings.to_vec(),
            text: chunk,
            source_kind: source.source_kind,
            modified_at: source.modified_at,
        });
    }
    buffer.clear();
}

fn bounded_chunks(text: &str, max_bytes: usize) -> Vec<String> {
    let mut chunks = Vec::new();
    let mut current = String::new();
    for word in text.split_whitespace() {
        let needed = word.len() + usize::from(!current.is_empty());
        if !current.is_empty() && current.len().saturating_add(needed) > max_bytes {
            chunks.push(std::mem::take(&mut current));
        }
        if word.len() > max_bytes {
            for character in word.chars() {
                if current.len() + character.len_utf8() > max_bytes && !current.is_empty() {
                    chunks.push(std::mem::take(&mut current));
                }
                current.push(character);
            }
        } else {
            if !current.is_empty() {
                current.push(' ');
            }
            current.push_str(word);
        }
    }
    if !current.is_empty() {
        chunks.push(current);
    }
    chunks
}

fn collect_workspace_sources(
    root: &Path,
    modified_at: UtcTimestamp,
    limits: RetrievalLimits,
) -> Result<Vec<SourceInput>, RetrievalError> {
    let root = fs::canonicalize(root)?;
    let mut files = Vec::new();
    for relative in [
        "MEMORY.md",
        "memory/daily",
        "state",
        "knowledge",
        "skills",
        "summaries",
        ".keith/MEMORY.md",
        ".keith/memory/daily",
        ".keith/state",
        ".keith/knowledge",
        ".keith/skills",
        ".keith/summaries",
    ] {
        collect_supported_files(&root, Path::new(relative), &mut files, limits)?;
    }
    files.sort();
    files
        .into_iter()
        .map(|path| {
            let relative = path
                .strip_prefix(&root)
                .map_err(|_| RetrievalError::InvalidSource)?;
            let text = fs::read_to_string(&path).map_err(|error| {
                if error.kind() == std::io::ErrorKind::InvalidData {
                    RetrievalError::InvalidSource
                } else {
                    error.into()
                }
            })?;
            let source_path = relative.to_string_lossy().replace('\\', "/");
            Ok(SourceInput {
                source_version: digest_text(&text),
                source_kind: classify_source(&source_path)?,
                source_path,
                modified_at,
                text,
            })
        })
        .collect()
}

fn collect_supported_files(
    root: &Path,
    relative: &Path,
    files: &mut Vec<PathBuf>,
    limits: RetrievalLimits,
) -> Result<(), RetrievalError> {
    let path = root.join(relative);
    let metadata = match fs::symlink_metadata(&path) {
        Ok(metadata) => metadata,
        Err(error) if error.kind() == std::io::ErrorKind::NotFound => return Ok(()),
        Err(error) => return Err(error.into()),
    };
    if metadata.file_type().is_symlink() {
        return Err(RetrievalError::InvalidSource);
    }
    if metadata.is_file() {
        if metadata.len() > u64::try_from(limits.max_source_bytes).unwrap_or(u64::MAX) {
            return Err(RetrievalError::InvalidSource);
        }
        if path.extension().is_some_and(|extension| extension == "md")
            || path
                .extension()
                .is_some_and(|extension| extension == "toml")
        {
            files.push(path);
        }
        return Ok(());
    }
    if metadata.is_dir() {
        for entry in fs::read_dir(path)? {
            let entry = entry?;
            let child = relative.join(entry.file_name());
            collect_supported_files(root, &child, files, limits)?;
            if files.len() > limits.max_documents_per_profile {
                return Err(RetrievalError::InvalidSource);
            }
        }
    }
    Ok(())
}

fn classify_source(path: &str) -> Result<SearchSourceKind, RetrievalError> {
    let path = path.strip_prefix(".keith/").unwrap_or(path);
    if path == "MEMORY.md" {
        Ok(SearchSourceKind::DurableMemory)
    } else if path.starts_with("memory/daily/") {
        Ok(SearchSourceKind::DailyMemory)
    } else if path.starts_with("state/") {
        Ok(SearchSourceKind::CurrentState)
    } else if path.starts_with("knowledge/") {
        Ok(SearchSourceKind::Knowledge)
    } else if path.starts_with("summaries/") {
        Ok(SearchSourceKind::SessionSummary)
    } else if path.starts_with("skills/") {
        Ok(SearchSourceKind::Skill)
    } else {
        Err(RetrievalError::InvalidSource)
    }
}

fn digest_text(text: &str) -> String {
    Sha256::digest(text.as_bytes())
        .iter()
        .fold(String::with_capacity(64), |mut output, byte| {
            write!(output, "{byte:02x}").expect("writing to a String cannot fail");
            output
        })
}

fn stored_document(row: &rusqlite::Row<'_>) -> rusqlite::Result<SearchDocument> {
    let document_id = EntityId::from_str(&row.get::<_, String>(0)?).map_err(|_| {
        rusqlite::Error::FromSqlConversionFailure(
            0,
            rusqlite::types::Type::Text,
            Box::new(RetrievalError::InvalidIdentity),
        )
    })?;
    let profile_id = ProfileId::from_str(&row.get::<_, String>(1)?).map_err(|_| {
        rusqlite::Error::FromSqlConversionFailure(
            1,
            rusqlite::types::Type::Text,
            Box::new(RetrievalError::InvalidIdentity),
        )
    })?;
    let source_kind = parse_source_kind(&row.get::<_, String>(6)?).map_err(|error| {
        rusqlite::Error::FromSqlConversionFailure(6, rusqlite::types::Type::Text, Box::new(error))
    })?;
    let headings = serde_json::from_str(&row.get::<_, String>(4)?).map_err(|error| {
        rusqlite::Error::FromSqlConversionFailure(4, rusqlite::types::Type::Text, Box::new(error))
    })?;
    Ok(SearchDocument {
        document_id,
        profile_id,
        source_path: row.get(2)?,
        source_version: row.get(3)?,
        heading_path: headings,
        text: row.get(5)?,
        source_kind,
        modified_at: UtcTimestamp::from_unix_millis(row.get(7)?),
    })
}

const fn source_kind_name(kind: SearchSourceKind) -> &'static str {
    match kind {
        SearchSourceKind::DurableMemory => "durable_memory",
        SearchSourceKind::DailyMemory => "daily_memory",
        SearchSourceKind::CurrentState => "current_state",
        SearchSourceKind::Knowledge => "knowledge",
        SearchSourceKind::SessionSummary => "session_summary",
        SearchSourceKind::Skill => "skill",
    }
}

fn parse_source_kind(value: &str) -> Result<SearchSourceKind, RetrievalError> {
    match value {
        "durable_memory" => Ok(SearchSourceKind::DurableMemory),
        "daily_memory" => Ok(SearchSourceKind::DailyMemory),
        "current_state" => Ok(SearchSourceKind::CurrentState),
        "knowledge" => Ok(SearchSourceKind::Knowledge),
        "session_summary" => Ok(SearchSourceKind::SessionSummary),
        "skill" => Ok(SearchSourceKind::Skill),
        _ => Err(RetrievalError::InvalidSource),
    }
}

fn score_document(
    document: SearchDocument,
    query: &str,
    query_trigrams: &BTreeSet<String>,
    lexical: &BTreeMap<EntityId, f32>,
    vector: Option<&BTreeMap<EntityId, f32>>,
    weights: RankWeights,
    excerpt_chars: usize,
) -> Option<SearchResult> {
    let text = normalize(&document.text);
    let manual_lexical = lexical_overlap(query, &text);
    let lexical_score = lexical
        .get(&document.document_id)
        .copied()
        .unwrap_or(0.0)
        .max(manual_lexical);
    let trigram_score = jaccard(query_trigrams, &trigrams(&text));
    let vector_score = vector.and_then(|scores| scores.get(&document.document_id).copied());
    let (weighted, available) = vector_score.map_or_else(
        || {
            (
                lexical_score * weights.lexical + trigram_score * weights.trigram,
                weights.lexical + weights.trigram,
            )
        },
        |score| {
            (
                lexical_score * weights.lexical
                    + trigram_score * weights.trigram
                    + score * weights.vector,
                weights.lexical + weights.trigram + weights.vector,
            )
        },
    );
    let merged_score = if available > f32::EPSILON {
        weighted / available
    } else {
        0.0
    };
    (merged_score > 0.0).then(|| SearchResult {
        document_id: document.document_id,
        source_path: document.source_path,
        source_version: document.source_version,
        heading_path: document.heading_path,
        excerpt: excerpt(&document.text, query, excerpt_chars),
        source_kind: document.source_kind,
        modified_at: document.modified_at,
        lexical_score,
        trigram_score,
        vector_score,
        merged_score,
    })
}

fn normalize(text: &str) -> String {
    text.chars()
        .flat_map(char::to_lowercase)
        .map(|character| {
            if character.is_alphanumeric() || character == '_' || character == '-' {
                character
            } else {
                ' '
            }
        })
        .collect::<String>()
        .split_whitespace()
        .collect::<Vec<_>>()
        .join(" ")
}

fn trigrams(text: &str) -> BTreeSet<String> {
    let characters = text.chars().collect::<Vec<_>>();
    if characters.is_empty() {
        return BTreeSet::new();
    }
    let width = characters.len().min(3);
    characters
        .windows(width)
        .map(|window| window.iter().collect())
        .collect()
}

fn lexical_overlap(query: &str, text: &str) -> f32 {
    if text.contains(query) {
        return 1.0;
    }
    let query_tokens = query.split_whitespace().collect::<BTreeSet<_>>();
    if query_tokens.is_empty() {
        return 0.0;
    }
    let text_tokens = text.split_whitespace().collect::<BTreeSet<_>>();
    let matches = query_tokens.intersection(&text_tokens).count();
    ratio(matches, query_tokens.len())
}

fn jaccard(left: &BTreeSet<String>, right: &BTreeSet<String>) -> f32 {
    if left.is_empty() || right.is_empty() {
        return 0.0;
    }
    let intersection = left.intersection(right).count();
    let union = left.union(right).count();
    ratio(intersection, union)
}

fn ratio(numerator: usize, denominator: usize) -> f32 {
    let numerator = u16::try_from(numerator.min(usize::from(u16::MAX))).unwrap_or(u16::MAX);
    let denominator = u16::try_from(denominator.min(usize::from(u16::MAX))).unwrap_or(u16::MAX);
    f32::from(numerator) / f32::from(denominator)
}

fn excerpt(text: &str, query: &str, max_chars: usize) -> String {
    let lower = text.to_lowercase();
    let query = query.to_lowercase();
    let byte_start = lower.find(&query).unwrap_or(0);
    let prefix_chars = text[..byte_start].chars().count();
    let start_chars = prefix_chars.saturating_sub(max_chars / 4);
    text.chars().skip(start_chars).take(max_chars).collect()
}

fn semantic_features(text: &str) -> BTreeSet<String> {
    let mut features = trigrams(text);
    features.extend(text.split_whitespace().map(|token| {
        let prefix = token.chars().take(5).collect::<String>();
        format!("word:{prefix}")
    }));
    features
}

fn normalize_vector(vector: &mut [f32]) {
    let norm = vector.iter().map(|value| value * value).sum::<f32>().sqrt();
    if norm > f32::EPSILON {
        for value in vector {
            *value /= norm;
        }
    }
}

fn cosine(left: &[f32], right: &[f32]) -> f32 {
    left.iter()
        .zip(right)
        .map(|(left, right)| left * right)
        .sum::<f32>()
        .clamp(-1.0, 1.0)
}

#[cfg(test)]
mod tests {
    use tempfile::tempdir;

    use super::*;

    fn service(root: &Path, vectors: bool) -> RetrievalService {
        let components = vectors.then(|| VectorComponents {
            embedder: Arc::new(LocalHashEmbedder::new(128).unwrap()),
            index: Arc::new(MemoryVectorIndex::default()),
        });
        RetrievalService::open(
            root,
            RetrievalLimits::default(),
            RankWeights::default(),
            components,
        )
        .unwrap()
    }

    fn source(path: &str, kind: SearchSourceKind, text: &str) -> SourceInput {
        SourceInput {
            source_path: path.into(),
            source_version: digest_text(text),
            source_kind: kind,
            modified_at: UtcTimestamp::UNIX_EPOCH,
            text: text.into(),
        }
    }

    #[test]
    fn multilingual_identifier_misspelling_and_vector_corpus_is_ranked_with_citations() {
        let directory = tempdir().unwrap();
        let profile = ProfileId::new();
        let retrieval = service(directory.path(), true);
        retrieval
            .index_sources(
                &profile,
                &[
                    source(
                        "MEMORY.md",
                        SearchSourceKind::DurableMemory,
                        "# Preferences\nUse release_calendar_v2 for every launch schedule.",
                    ),
                    source(
                        "knowledge/agent.md",
                        SearchSourceKind::Knowledge,
                        "# 系统\n机器学习代理使用混合检索。",
                    ),
                    source(
                        "state/config.md",
                        SearchSourceKind::CurrentState,
                        "The application configuration controls provider selection.",
                    ),
                ],
            )
            .unwrap();
        let identifier = retrieval
            .search(&profile, "release_calendar_v2", 5)
            .unwrap();
        assert_eq!(identifier[0].source_path, "MEMORY.md");
        assert_eq!(identifier[0].heading_path, vec!["Preferences"]);
        assert!(!identifier[0].source_version.is_empty());
        let chinese = retrieval.search(&profile, "机器学习", 5).unwrap();
        assert_eq!(chinese[0].source_path, "knowledge/agent.md");
        let misspelled = retrieval.search(&profile, "calender", 5).unwrap();
        assert_eq!(misspelled[0].source_path, "MEMORY.md");
        assert!(misspelled[0].trigram_score > 0.0);
        let semantic = retrieval
            .search(&profile, "configure providers", 5)
            .unwrap();
        assert_eq!(semantic[0].source_path, "state/config.md");
        assert!(semantic[0].vector_score.is_some());
    }

    #[test]
    fn deletion_profile_isolation_and_lexical_fallback_are_enforced() {
        let directory = tempdir().unwrap();
        let first = ProfileId::new();
        let second = ProfileId::new();
        let retrieval = service(directory.path(), false);
        retrieval
            .index_sources(
                &first,
                &[source(
                    "MEMORY.md",
                    SearchSourceKind::DurableMemory,
                    "private alpha launch",
                )],
            )
            .unwrap();
        retrieval
            .index_sources(
                &second,
                &[source(
                    "MEMORY.md",
                    SearchSourceKind::DurableMemory,
                    "private beta launch",
                )],
            )
            .unwrap();
        let results = retrieval.search(&first, "private launch", 10).unwrap();
        assert_eq!(results.len(), 1);
        assert!(results[0].excerpt.contains("alpha"));
        assert!(!retrieval.health_snapshot().unwrap().vector_available);
        retrieval.remove_source(&first, "MEMORY.md").unwrap();
        assert!(retrieval.search(&first, "alpha", 10).unwrap().is_empty());
        assert_eq!(retrieval.search(&second, "beta", 10).unwrap().len(), 1);
    }

    #[test]
    fn corrupt_index_is_quarantined_and_rebuilt_from_readable_sources() {
        let directory = tempdir().unwrap();
        let index_root = directory.path().join("index");
        let workspace = directory.path().join("workspace");
        fs::create_dir_all(workspace.join("knowledge")).unwrap();
        fs::write(
            workspace.join("knowledge/recovery.md"),
            "# Recovery\nReadable source survives derived-index loss.",
        )
        .unwrap();
        let profile = ProfileId::new();
        let initial = service(&index_root, false);
        initial
            .index_sources(
                &profile,
                &[source(
                    "knowledge/old.md",
                    SearchSourceKind::Knowledge,
                    "old derived content",
                )],
            )
            .unwrap();
        drop(initial);
        fs::write(index_root.join(INDEX_FILE), b"not a sqlite database").unwrap();
        let recovered = service(&index_root, false);
        let health = recovered.health_snapshot().unwrap();
        assert!(health.degraded);
        assert!(health.quarantined_index.unwrap().exists());
        recovered
            .rebuild_workspace(&profile, &workspace, UtcTimestamp::UNIX_EPOCH)
            .unwrap();
        assert!(!recovered.health_snapshot().unwrap().degraded);
        let results = recovered.search(&profile, "derived-index loss", 5).unwrap();
        assert_eq!(results[0].source_path, "knowledge/recovery.md");
    }

    #[test]
    fn hidden_profile_layout_is_indexed_with_truthful_source_paths() {
        let directory = tempdir().unwrap();
        let index_root = directory.path().join("index");
        let workspace = directory.path().join("workspace");
        fs::create_dir_all(workspace.join(".keith/memory/daily")).unwrap();
        fs::create_dir_all(workspace.join(".keith/skills/release")).unwrap();
        fs::write(
            workspace.join(".keith/MEMORY.md"),
            "# Preference\nUse the stable release channel.",
        )
        .unwrap();
        fs::write(
            workspace.join(".keith/memory/daily/2026-08-16.md"),
            "# Today\nVerified the packaged daemon.",
        )
        .unwrap();
        fs::write(
            workspace.join(".keith/skills/release/SKILL.md"),
            "# Release\nValidate artifacts before activation.",
        )
        .unwrap();

        let profile = ProfileId::new();
        let retrieval = service(&index_root, false);
        retrieval
            .rebuild_workspace(&profile, &workspace, UtcTimestamp::UNIX_EPOCH)
            .unwrap();

        let memory = retrieval.search(&profile, "stable release", 5).unwrap();
        assert_eq!(memory[0].source_path, ".keith/MEMORY.md");
        assert_eq!(memory[0].source_kind, SearchSourceKind::DurableMemory);
        let daily = retrieval.search(&profile, "packaged daemon", 5).unwrap();
        assert_eq!(daily[0].source_kind, SearchSourceKind::DailyMemory);
        let skill = retrieval
            .search(&profile, "artifacts activation", 5)
            .unwrap();
        assert_eq!(skill[0].source_kind, SearchSourceKind::Skill);
    }
}
