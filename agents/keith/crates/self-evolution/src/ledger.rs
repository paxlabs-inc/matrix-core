use std::sync::Arc;

use keith_agent_types::{
    CURRENT_SCHEMA_VERSION, EntityId, Revision, UtcTimestamp, canonical_json_bytes,
};
use keith_state_store_core::{
    ClassifiedRepositoryError as _, EvolutionLedgerRepository, VersionedRecord, WritePrecondition,
};
use ring::signature::{Ed25519KeyPair, KeyPair};
use serde::{Deserialize, Serialize};
use sha2::{Digest, Sha256};
use thiserror::Error;

const RECORD_FORMAT: &str = "keith-evolution-ledger-v1";
const HEAD_FORMAT: &str = "keith-evolution-ledger-head-v1";
const GENESIS_DIGEST: &str = "genesis";
pub const EVOLUTION_LEDGER_EXPORT_FORMAT: &str = "keith-evolution-ledger-export";
pub const EVOLUTION_LEDGER_EXPORT_SCHEMA_VERSION: u16 = 1;
pub const MAX_GATE_SUMMARY_BYTES: usize = 16 * 1024;
pub const MAX_LEDGER_TEXT_BYTES: usize = 64 * 1024;

#[derive(Clone, Debug, Eq, PartialEq, Serialize)]
#[serde(transparent)]
pub struct LedgerText(String);

impl LedgerText {
    /// Creates bounded text after rejecting private categories and redacting supplied secrets.
    ///
    /// # Errors
    /// Returns [`LedgerError::PrivateContent`] for a forbidden content category.
    pub fn redacted(
        value: &str,
        maximum: usize,
        sensitive: &[String],
    ) -> Result<Self, LedgerError> {
        let maximum = maximum.min(MAX_LEDGER_TEXT_BYTES);
        reject_private_content(value)?;
        let mut value = value.to_owned();
        for secret in sensitive.iter().filter(|secret| !secret.is_empty()) {
            value = value.replace(secret, "[REDACTED]");
        }
        for marker in [
            "api_key=",
            "apikey=",
            "authorization:",
            "token=",
            "password=",
        ] {
            redact_marker(&mut value, marker);
        }
        let (value, _) = bounded_utf8(value, maximum);
        Ok(Self(value))
    }

    pub fn as_str(&self) -> &str {
        &self.0
    }
}

impl<'de> Deserialize<'de> for LedgerText {
    fn deserialize<D>(deserializer: D) -> Result<Self, D::Error>
    where
        D: serde::Deserializer<'de>,
    {
        let value = String::deserialize(deserializer)?;
        Self::redacted(&value, MAX_LEDGER_TEXT_BYTES, &[]).map_err(serde::de::Error::custom)
    }
}

#[derive(Clone, Debug, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct LedgerHypothesis {
    pub id: EntityId,
    pub evidence_refs: Vec<EntityId>,
    pub target_subsystem: LedgerText,
    pub metric: LedgerText,
    pub baseline: f64,
    pub target: f64,
    pub revert_threshold: f64,
    pub expires_at: UtcTimestamp,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub measurement_slice: Option<keith_telemetry::MetricSlice>,
    #[serde(default, skip_serializing_if = "Vec::is_empty")]
    pub evidence_sources: Vec<crate::EvidenceSource>,
    #[serde(default, skip_serializing_if = "Vec::is_empty")]
    pub evidence_digests: Vec<String>,
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize)]
#[serde(deny_unknown_fields)]
pub struct GateSummary {
    gate: LedgerText,
    succeeded: bool,
    exit_code: Option<i32>,
    output: LedgerText,
    truncated: bool,
}

impl GateSummary {
    /// Creates a bounded, redacted summary from real gate output.
    ///
    /// # Errors
    /// Returns [`LedgerError::PrivateContent`] for a forbidden content category.
    pub fn redacted(
        gate: impl Into<String>,
        succeeded: bool,
        exit_code: Option<i32>,
        output: &str,
        sensitive_values: &[String],
    ) -> Result<Self, LedgerError> {
        let truncated = output.len() > MAX_GATE_SUMMARY_BYTES;
        let redacted = LedgerText::redacted(output, MAX_GATE_SUMMARY_BYTES, sensitive_values)?;
        Ok(Self {
            gate: LedgerText::redacted(&gate.into(), 256, sensitive_values)?,
            succeeded,
            exit_code,
            output: redacted,
            truncated,
        })
    }

    pub fn output(&self) -> &str {
        self.output.as_str()
    }
    pub const fn truncated(&self) -> bool {
        self.truncated
    }
}

impl<'de> Deserialize<'de> for GateSummary {
    fn deserialize<D>(deserializer: D) -> Result<Self, D::Error>
    where
        D: serde::Deserializer<'de>,
    {
        #[derive(Deserialize)]
        #[serde(deny_unknown_fields)]
        struct Raw {
            gate: String,
            succeeded: bool,
            exit_code: Option<i32>,
            output: String,
            truncated: bool,
        }
        let raw = Raw::deserialize(deserializer)?;
        let mut summary = Self::redacted(raw.gate, raw.succeeded, raw.exit_code, &raw.output, &[])
            .map_err(serde::de::Error::custom)?;
        summary.truncated |= raw.truncated;
        Ok(summary)
    }
}

#[allow(clippy::large_enum_variant)]
#[derive(Clone, Debug, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case", tag = "kind")]
pub enum EvolutionEvent {
    Hypothesis {
        hypothesis: LedgerHypothesis,
    },
    Admission {
        hypothesis_id: EntityId,
        admitted: bool,
        reason: LedgerText,
    },
    HypothesisTransition {
        hypothesis_id: EntityId,
        from: crate::HypothesisState,
        to: crate::HypothesisState,
        revision: u64,
        reason: Option<LedgerText>,
    },
    EvidenceAttestation {
        evidence_id: EntityId,
        source: crate::EvidenceSource,
        digest: String,
        binding: Vec<u8>,
    },
    Proposal {
        hypothesis_id: EntityId,
        readable_diff: LedgerText,
    },
    Gate {
        hypothesis_id: EntityId,
        summaries: Vec<GateSummary>,
    },
    Canary {
        hypothesis_id: EntityId,
        before: f64,
        after: f64,
        passed: bool,
    },
    Consent {
        hypothesis_id: EntityId,
        approved: bool,
        acting_identity: LedgerText,
    },
    Promotion {
        hypothesis_id: EntityId,
        promotion_id: EntityId,
        artifact_id: LedgerText,
        artifact_digest: LedgerText,
    },
    Observation {
        hypothesis_id: EntityId,
        before: f64,
        after: f64,
        healthy: bool,
    },
    Revert {
        hypothesis_id: EntityId,
        reason: LedgerText,
        acting_identity: Option<LedgerText>,
        promotion_ids: Vec<EntityId>,
        restored_image_id: LedgerText,
        restored_paths: Vec<LedgerText>,
        unresolved: Option<LedgerText>,
    },
    Enable {
        acting_identity: LedgerText,
    },
    Disable {
        acting_identity: LedgerText,
        reason: LedgerText,
        unresolved_cleanup: Vec<LedgerText>,
    },
}

#[derive(Clone, Debug, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct EvolutionRecord {
    pub format: String,
    pub id: EntityId,
    pub event_id: EntityId,
    pub sequence: u64,
    pub occurred_at: UtcTimestamp,
    pub previous_digest: String,
    pub event: EvolutionEvent,
    pub signer_public_key: Vec<u8>,
    pub signature: Vec<u8>,
}

#[derive(Serialize)]
struct SignedRecord<'a> {
    format: &'a str,
    id: &'a EntityId,
    event_id: &'a EntityId,
    sequence: u64,
    occurred_at: UtcTimestamp,
    previous_digest: &'a str,
    event: &'a EvolutionEvent,
    signer_public_key: &'a [u8],
}

#[derive(Clone, Debug, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
struct EvolutionHead {
    format: String,
    count: u64,
    last_digest: String,
    signer_public_key: Vec<u8>,
    signature: Vec<u8>,
}

/// Public, signed ledger head carried by a standalone data-control export.
#[derive(Clone, Debug, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct EvolutionLedgerArchiveHead {
    pub format: String,
    pub count: u64,
    pub last_digest: String,
    pub signer_public_key: Vec<u8>,
    pub signature: Vec<u8>,
}

impl From<EvolutionHead> for EvolutionLedgerArchiveHead {
    fn from(head: EvolutionHead) -> Self {
        Self {
            format: head.format,
            count: head.count,
            last_digest: head.last_digest,
            signer_public_key: head.signer_public_key,
            signature: head.signature,
        }
    }
}

/// Versioned, readable ledger document that verifies without a running Keith installation.
#[derive(Clone, Debug, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct EvolutionLedgerArchive {
    pub format: String,
    pub schema_version: u16,
    pub records: Vec<EvolutionRecord>,
    pub authenticated_head: Option<EvolutionLedgerArchiveHead>,
}

impl EvolutionLedgerArchive {
    /// Reads storage rows in ledger order and validates their storage metadata and signed chain.
    ///
    /// # Errors
    /// Returns an error when storage is unavailable or any row/head is inconsistent.
    pub fn from_repository<R>(repository: &R) -> Result<Self, LedgerError>
    where
        R: EvolutionLedgerRepository,
    {
        let mut stored = repository
            .list_evolution_records()
            .map_err(|error| LedgerError::Store(error.to_string()))?;
        stored.sort_by_key(|row| {
            row.payload
                .get("sequence")
                .and_then(serde_json::Value::as_u64)
                .unwrap_or(u64::MAX)
        });
        let mut records = Vec::with_capacity(stored.len());
        for (position, row) in stored.into_iter().enumerate() {
            let record: EvolutionRecord = serde_json::from_value(row.payload)
                .map_err(|error| LedgerError::Quarantined(error.to_string()))?;
            if row.id != record.id
                || row.version != CURRENT_SCHEMA_VERSION
                || row.revision != Revision::ZERO
                || row.updated_at != record.occurred_at
                || record.sequence
                    != u64::try_from(position)
                        .map_err(|_| LedgerError::Quarantined("sequence overflow".into()))?
            {
                return Err(LedgerError::Quarantined(format!(
                    "invalid stored row at sequence {position}"
                )));
            }
            records.push(record);
        }
        let stored_head = repository
            .get_evolution_head()
            .map_err(|error| LedgerError::Store(error.to_string()))?;
        let authenticated_head = match stored_head {
            Some(row) => {
                let head: EvolutionHead = serde_json::from_value(row.payload)
                    .map_err(|error| LedgerError::Quarantined(error.to_string()))?;
                let count = u64::try_from(records.len())
                    .map_err(|_| LedgerError::Quarantined("record count overflow".into()))?;
                if row.id != EntityId::from_u128(0)
                    || row.version != CURRENT_SCHEMA_VERSION
                    || row.revision != Revision::new(count)
                    || records
                        .last()
                        .is_none_or(|record| row.updated_at != record.occurred_at)
                {
                    return Err(LedgerError::Quarantined(
                        "invalid stored authenticated head".into(),
                    ));
                }
                Some(head.into())
            }
            None => None,
        };
        let archive = Self {
            format: EVOLUTION_LEDGER_EXPORT_FORMAT.into(),
            schema_version: EVOLUTION_LEDGER_EXPORT_SCHEMA_VERSION,
            records,
            authenticated_head,
        };
        archive.verify()?;
        Ok(archive)
    }

    /// Verifies versioning, ordering, every row signature/link, and the signed terminal head.
    ///
    /// # Errors
    /// Returns an error for an unsupported or cryptographically inconsistent archive.
    pub fn verify(&self) -> Result<(), LedgerError> {
        if self.format != EVOLUTION_LEDGER_EXPORT_FORMAT
            || self.schema_version != EVOLUTION_LEDGER_EXPORT_SCHEMA_VERSION
        {
            return Err(LedgerError::Quarantined(
                "unsupported ledger export format".into(),
            ));
        }
        match (&self.records[..], &self.authenticated_head) {
            ([], None) => return Ok(()),
            ([], Some(_)) | ([_, ..], None) => {
                return Err(LedgerError::Quarantined(
                    "ledger rows and authenticated head disagree".into(),
                ));
            }
            _ => {}
        }
        let head = self.authenticated_head.as_ref().expect("checked above");
        if head.format != HEAD_FORMAT || head.signer_public_key.len() != 32 {
            return Err(LedgerError::Quarantined(
                "authenticated head is malformed".into(),
            ));
        }
        let trusted_public_key: [u8; 32] = head
            .signer_public_key
            .as_slice()
            .try_into()
            .map_err(|_| LedgerError::Quarantined("invalid signer public key".into()))?;
        let mut previous = GENESIS_DIGEST.to_owned();
        for (position, record) in self.records.iter().enumerate() {
            let sequence = u64::try_from(position)
                .map_err(|_| LedgerError::Quarantined("sequence overflow".into()))?;
            if record.format != RECORD_FORMAT
                || record.sequence != sequence
                || record.id != EntityId::from_u128(u128::from(sequence) + 1)
                || record.previous_digest != previous
                || record.signer_public_key.as_slice() != trusted_public_key
            {
                return Err(LedgerError::Quarantined(format!(
                    "invalid chain at sequence {position}"
                )));
            }
            validate_event(&record.event)
                .map_err(|error| LedgerError::Quarantined(error.to_string()))?;
            let material = SignedRecord {
                format: &record.format,
                id: &record.id,
                event_id: &record.event_id,
                sequence: record.sequence,
                occurred_at: record.occurred_at,
                previous_digest: &record.previous_digest,
                event: &record.event,
                signer_public_key: &record.signer_public_key,
            };
            keith_release::verify_detached_signature(
                &canonical_json_bytes(&material)?,
                &record.signature,
                &trusted_public_key,
            )
            .map_err(|_| {
                LedgerError::Quarantined(format!("invalid signature at sequence {position}"))
            })?;
            previous = record_digest(record)?;
        }
        let count = u64::try_from(self.records.len())
            .map_err(|_| LedgerError::Quarantined("record count overflow".into()))?;
        if head.count != count || head.last_digest != previous {
            return Err(LedgerError::Quarantined(
                "authenticated head mismatch".into(),
            ));
        }
        let material = SignedHead {
            format: &head.format,
            count: head.count,
            last_digest: &head.last_digest,
            signer_public_key: &head.signer_public_key,
        };
        keith_release::verify_detached_signature(
            &canonical_json_bytes(&material)?,
            &head.signature,
            &trusted_public_key,
        )
        .map_err(|_| LedgerError::Quarantined("invalid head signature".into()))?;
        Ok(())
    }
}

#[derive(Serialize)]
struct SignedHead<'a> {
    format: &'a str,
    count: u64,
    last_digest: &'a str,
    signer_public_key: &'a [u8],
}

#[derive(Debug, Error)]
pub enum LedgerError {
    #[error("evolution ledger persistence failed: {0}")]
    Store(String),
    #[error("evolution ledger is quarantined: {0}")]
    Quarantined(String),
    #[error("evolution ledger signing key is invalid")]
    InvalidSigningKey,
    #[error("private content category is forbidden in the evolution ledger")]
    PrivateContent,
    #[error("evolution ledger serialization failed: {0}")]
    Serialization(#[from] serde_json::Error),
}

pub struct EvolutionLedger<R> {
    repository: Arc<R>,
    signing_key: Ed25519KeyPair,
    trusted_public_key: [u8; 32],
}

impl<R> EvolutionLedger<R>
where
    R: EvolutionLedgerRepository,
{
    #[must_use]
    pub const fn trusted_public_key(&self) -> &[u8; 32] {
        &self.trusted_public_key
    }
    /// Opens and verifies the complete ledger using a real Ed25519 signing seed.
    ///
    /// # Errors
    /// Returns an error for an invalid key, persistence failure, or corrupt ledger.
    pub fn from_seed(repository: Arc<R>, seed: &[u8; 32]) -> Result<Self, LedgerError> {
        let signing_key = Ed25519KeyPair::from_seed_unchecked(seed)
            .map_err(|_| LedgerError::InvalidSigningKey)?;
        let trusted_public_key = signing_key
            .public_key()
            .as_ref()
            .try_into()
            .map_err(|_| LedgerError::InvalidSigningKey)?;
        let ledger = Self {
            repository,
            signing_key,
            trusted_public_key,
        };
        ledger.records()?;
        Ok(ledger)
    }

    /// Appends one signed record after validating the existing chain.
    ///
    /// # Errors
    /// Returns an error for forbidden content, corruption, signing, or persistence failure.
    #[allow(clippy::needless_pass_by_value, clippy::too_many_lines)]
    pub fn append(
        &self,
        event_id: EntityId,
        occurred_at: UtcTimestamp,
        event: EvolutionEvent,
    ) -> Result<EvolutionRecord, LedgerError> {
        self.append_checked(event_id, occurred_at, event, |_| Ok(()))
    }

    /// Appends after revalidating caller-owned invariants on every contention retry.
    ///
    /// # Errors
    /// Returns an error when validation, signing, persistence, or chain verification fails.
    #[allow(clippy::needless_pass_by_value, clippy::too_many_lines)]
    pub fn append_checked<F>(
        &self,
        event_id: EntityId,
        occurred_at: UtcTimestamp,
        event: EvolutionEvent,
        mut validate: F,
    ) -> Result<EvolutionRecord, LedgerError>
    where
        F: FnMut(&[EvolutionRecord]) -> Result<(), LedgerError>,
    {
        validate_event(&event)?;
        for _ in 0..16 {
            let records = self.records()?;
            if let Some(existing) = records.iter().find(|record| record.event_id == event_id) {
                if existing.event == event {
                    return Ok(existing.clone());
                }
                return Err(LedgerError::Store(
                    "event identity was reused with different content".into(),
                ));
            }
            validate(&records)?;
            let sequence = u64::try_from(records.len())
                .map_err(|_| LedgerError::Quarantined("record count overflow".into()))?;
            let previous_digest = records
                .last()
                .map(record_digest)
                .transpose()?
                .unwrap_or_else(|| GENESIS_DIGEST.into());
            let id = EntityId::from_u128(u128::from(sequence) + 1);
            let public_key = self.trusted_public_key.to_vec();
            let material = SignedRecord {
                format: RECORD_FORMAT,
                id: &id,
                event_id: &event_id,
                sequence,
                occurred_at,
                previous_digest: &previous_digest,
                event: &event,
                signer_public_key: &public_key,
            };
            let signature = self
                .signing_key
                .sign(&canonical_json_bytes(&material)?)
                .as_ref()
                .to_vec();
            let record = EvolutionRecord {
                format: RECORD_FORMAT.into(),
                id: id.clone(),
                event_id: event_id.clone(),
                sequence,
                occurred_at,
                previous_digest,
                event: event.clone(),
                signer_public_key: public_key.clone(),
                signature,
            };
            let payload = serde_json::to_value(&record)?;
            let last_digest = record_digest(&record)?;
            let head_material = SignedHead {
                format: HEAD_FORMAT,
                count: sequence + 1,
                last_digest: &last_digest,
                signer_public_key: &public_key,
            };
            let head_signature = self
                .signing_key
                .sign(&canonical_json_bytes(&head_material)?)
                .as_ref()
                .to_vec();
            let head = EvolutionHead {
                format: HEAD_FORMAT.into(),
                count: sequence + 1,
                last_digest,
                signer_public_key: public_key,
                signature: head_signature,
            };
            match self.repository.append_evolution_record(
                VersionedRecord {
                    version: CURRENT_SCHEMA_VERSION,
                    id: id.clone(),
                    revision: Revision::ZERO,
                    updated_at: occurred_at,
                    payload,
                },
                VersionedRecord {
                    version: CURRENT_SCHEMA_VERSION,
                    id: EntityId::from_u128(0),
                    revision: Revision::new(sequence + 1),
                    updated_at: occurred_at,
                    payload: serde_json::to_value(&head)?,
                },
                if sequence == 0 {
                    WritePrecondition::Missing
                } else {
                    WritePrecondition::Exact(Revision::new(sequence))
                },
            ) {
                Ok(_) => return Ok(record),
                Err(error) => {
                    let recovered = self
                        .repository
                        .get_evolution_record(&id)
                        .map_err(|read_error| LedgerError::Store(read_error.to_string()))?;
                    if recovered.is_some_and(|stored| {
                        serde_json::from_value::<EvolutionRecord>(stored.payload)
                            .is_ok_and(|stored| stored == record)
                    }) {
                        return Ok(record);
                    }
                    if !error.is_conflict() {
                        return Err(LedgerError::Store(error.to_string()));
                    }
                }
            }
        }
        Err(LedgerError::Store(
            "append contention exceeded retry limit".into(),
        ))
    }

    /// Reads and cryptographically verifies every record and chain link.
    ///
    /// # Errors
    /// Returns an error for persistence failure or any corrupt, missing, reordered, or unsigned
    /// record.
    #[allow(clippy::too_many_lines)]
    pub fn records(&self) -> Result<Vec<EvolutionRecord>, LedgerError> {
        let stored = self
            .repository
            .list_evolution_records()
            .map_err(|error| LedgerError::Store(error.to_string()))?;
        let stored_head = self
            .repository
            .get_evolution_head()
            .map_err(|error| LedgerError::Store(error.to_string()))?;
        let mut records = stored
            .into_iter()
            .map(|record| {
                let decoded = serde_json::from_value::<EvolutionRecord>(record.payload.clone())
                    .map_err(|error| LedgerError::Quarantined(error.to_string()))?;
                Ok::<_, LedgerError>((record, decoded))
            })
            .collect::<Result<Vec<_>, _>>()?;
        records.sort_by_key(|(_, record)| record.sequence);
        let mut previous = GENESIS_DIGEST.to_owned();
        for (expected, (stored, record)) in records.iter().enumerate() {
            let expected_sequence = u64::try_from(expected)
                .map_err(|_| LedgerError::Quarantined("sequence overflow".into()))?;
            let deterministic_id = EntityId::from_u128(u128::from(expected_sequence) + 1);
            if record.format != RECORD_FORMAT
                || record.sequence != expected_sequence
                || record.id != deterministic_id
                || stored.id != record.id
                || stored.version != CURRENT_SCHEMA_VERSION
                || stored.revision != Revision::ZERO
                || stored.updated_at != record.occurred_at
                || record.previous_digest != previous
                || record.signer_public_key.as_slice() != self.trusted_public_key
            {
                return Err(LedgerError::Quarantined(format!(
                    "invalid chain at sequence {expected}"
                )));
            }
            validate_event(&record.event)
                .map_err(|error| LedgerError::Quarantined(error.to_string()))?;
            let material = SignedRecord {
                format: &record.format,
                id: &record.id,
                event_id: &record.event_id,
                sequence: record.sequence,
                occurred_at: record.occurred_at,
                previous_digest: &record.previous_digest,
                event: &record.event,
                signer_public_key: &record.signer_public_key,
            };
            keith_release::verify_detached_signature(
                &canonical_json_bytes(&material)?,
                &record.signature,
                &self.trusted_public_key,
            )
            .map_err(|_| {
                LedgerError::Quarantined(format!("invalid signature at sequence {expected}"))
            })?;
            previous = record_digest(record)?;
        }
        match (records.is_empty(), stored_head) {
            (true, None) => {}
            (false, Some(stored)) => {
                let head: EvolutionHead = serde_json::from_value(stored.payload.clone())
                    .map_err(|error| LedgerError::Quarantined(error.to_string()))?;
                let count = u64::try_from(records.len())
                    .map_err(|_| LedgerError::Quarantined("record count overflow".into()))?;
                if stored.id != EntityId::from_u128(0)
                    || stored.version != CURRENT_SCHEMA_VERSION
                    || stored.revision != Revision::new(count)
                    || head.format != HEAD_FORMAT
                    || head.count != count
                    || head.last_digest != previous
                    || head.signer_public_key.as_slice() != self.trusted_public_key
                {
                    return Err(LedgerError::Quarantined(
                        "authenticated head mismatch".into(),
                    ));
                }
                let material = SignedHead {
                    format: &head.format,
                    count: head.count,
                    last_digest: &head.last_digest,
                    signer_public_key: &head.signer_public_key,
                };
                keith_release::verify_detached_signature(
                    &canonical_json_bytes(&material)?,
                    &head.signature,
                    &self.trusted_public_key,
                )
                .map_err(|_| LedgerError::Quarantined("invalid head signature".into()))?;
                if records
                    .last()
                    .is_none_or(|(_, record)| stored.updated_at != record.occurred_at)
                {
                    return Err(LedgerError::Quarantined("head timestamp mismatch".into()));
                }
            }
            _ => {
                return Err(LedgerError::Quarantined(
                    "ledger rows and authenticated head disagree".into(),
                ));
            }
        }
        Ok(records.into_iter().map(|(_, record)| record).collect())
    }
}

fn record_digest(record: &EvolutionRecord) -> Result<String, LedgerError> {
    Ok(hex(&Sha256::digest(canonical_json_bytes(record)?)))
}

#[allow(clippy::collapsible_if)]
fn validate_event(event: &EvolutionEvent) -> Result<(), LedgerError> {
    if let EvolutionEvent::EvidenceAttestation {
        source,
        digest,
        binding,
        ..
    } = event
    {
        if !matches!(
            source,
            crate::EvidenceSource::CrashReconciliation | crate::EvidenceSource::ExplicitUserReport
        ) || digest.len() != 64
            || !digest
                .bytes()
                .all(|byte| byte.is_ascii_hexdigit() && !byte.is_ascii_uppercase())
            || binding.len() != 32
        {
            return Err(LedgerError::PrivateContent);
        }
    }
    if let EvolutionEvent::Hypothesis { hypothesis } = event {
        if !hypothesis.baseline.is_finite()
            || !hypothesis.target.is_finite()
            || !hypothesis.revert_threshold.is_finite()
            || ((hypothesis.measurement_slice.is_some()
                || !hypothesis.evidence_sources.is_empty()
                || !hypothesis.evidence_digests.is_empty())
                && (hypothesis.evidence_refs.len() != hypothesis.evidence_sources.len()
                    || hypothesis.evidence_refs.len() != hypothesis.evidence_digests.len()))
            || hypothesis.evidence_digests.iter().any(|digest| {
                digest.len() != 64
                    || !digest
                        .bytes()
                        .all(|byte| byte.is_ascii_hexdigit() && !byte.is_ascii_uppercase())
            })
        {
            return Err(LedgerError::PrivateContent);
        }
    }
    if let EvolutionEvent::Promotion {
        artifact_id,
        artifact_digest,
        ..
    } = event
        && (artifact_id.as_str().is_empty()
            || artifact_digest.as_str().len() != 64
            || !artifact_digest
                .as_str()
                .bytes()
                .all(|byte| byte.is_ascii_hexdigit() && !byte.is_ascii_uppercase()))
    {
        return Err(LedgerError::PrivateContent);
    }
    reject_private_content(&serde_json::to_string(event)?)
}

fn reject_private_content(value: &str) -> Result<(), LedgerError> {
    let lower = value.to_ascii_lowercase();
    if [
        "prompt text",
        "private reasoning",
        "personal memory",
        "channel content",
    ]
    .iter()
    .any(|term| lower.contains(term))
    {
        Err(LedgerError::PrivateContent)
    } else {
        Ok(())
    }
}

fn redact_marker(value: &mut String, marker: &str) {
    let mut start = 0;
    while let Some(relative) = value[start..].to_ascii_lowercase().find(marker) {
        let secret_start = start + relative + marker.len();
        let secret_end = if marker.ends_with(':') {
            value[secret_start..]
                .find(['\r', '\n'])
                .map_or(value.len(), |offset| secret_start + offset)
        } else {
            value[secret_start..]
                .find(char::is_whitespace)
                .map_or(value.len(), |offset| secret_start + offset)
        };
        value.replace_range(secret_start..secret_end, "[REDACTED]");
        start = secret_start + "[REDACTED]".len();
    }
}

fn bounded_utf8(mut value: String, maximum: usize) -> (String, bool) {
    if value.len() <= maximum {
        return (value, false);
    }
    let mut end = maximum;
    while !value.is_char_boundary(end) {
        end -= 1;
    }
    value.truncate(end);
    (value, true)
}

fn hex(bytes: &[u8]) -> String {
    use std::fmt::Write as _;
    bytes.iter().fold(String::new(), |mut encoded, byte| {
        let _ = write!(encoded, "{byte:02x}");
        encoded
    })
}

#[cfg(test)]
mod tests {
    use super::*;
    use keith_state_store::{EmbeddedStore, FaultPoint};
    use keith_state_store_core::{
        AtomicStateRepository, Collection, RecordMutation, WritePrecondition,
    };
    use rusqlite::{Connection, params};
    use std::thread;
    use tempfile::tempdir;

    fn event(label: &str) -> EvolutionEvent {
        EvolutionEvent::Admission {
            hypothesis_id: EntityId::from_u128(7),
            admitted: true,
            reason: LedgerText::redacted(label, 256, &[]).unwrap(),
        }
    }

    #[test]
    fn ledger_signs_chains_and_survives_restart() {
        let directory = tempdir().unwrap();
        let path = directory.path().join("state.sqlite");
        let seed = [7; 32];
        {
            let ledger = EvolutionLedger::from_seed(
                Arc::new(EmbeddedStore::open(&path, None).unwrap()),
                &seed,
            )
            .unwrap();
            ledger
                .append(
                    EntityId::new(),
                    UtcTimestamp::from_unix_millis(1),
                    event("admitted"),
                )
                .unwrap();
            ledger
                .append(
                    EntityId::new(),
                    UtcTimestamp::from_unix_millis(2),
                    EvolutionEvent::Enable {
                        acting_identity: LedgerText::redacted("installation-owner", 256, &[])
                            .unwrap(),
                    },
                )
                .unwrap();
            assert_eq!(ledger.records().unwrap().len(), 2);
        }
        let reopened =
            EvolutionLedger::from_seed(Arc::new(EmbeddedStore::open(&path, None).unwrap()), &seed)
                .unwrap();
        assert_eq!(reopened.records().unwrap()[1].sequence, 1);
    }

    #[test]
    fn pre_hypothesis_lifecycle_payload_reopens_without_rewriting_signed_bytes() {
        let directory = tempdir().unwrap();
        let path = directory.path().join("legacy.sqlite");
        let seed = [61; 32];
        let ledger =
            EvolutionLedger::from_seed(Arc::new(EmbeddedStore::open(&path, None).unwrap()), &seed)
                .unwrap();
        let event = EvolutionEvent::Hypothesis {
            hypothesis: LedgerHypothesis {
                id: EntityId::from_u128(301),
                evidence_refs: vec![EntityId::from_u128(302)],
                target_subsystem: LedgerText::redacted("worker", 32, &[]).unwrap(),
                metric: LedgerText::redacted("latency", 32, &[]).unwrap(),
                baseline: 10.0,
                target: 8.0,
                revert_threshold: 12.0,
                expires_at: UtcTimestamp::from_unix_millis(99),
                measurement_slice: None,
                evidence_sources: Vec::new(),
                evidence_digests: Vec::new(),
            },
        };
        let json = serde_json::to_string(&event).unwrap();
        assert!(!json.contains("measurement_slice"));
        ledger
            .append(
                EntityId::from_u128(303),
                UtcTimestamp::from_unix_millis(1),
                event,
            )
            .unwrap();
        let before = ledger.records().unwrap()[0].signature.clone();
        drop(ledger);
        let reopened =
            EvolutionLedger::from_seed(Arc::new(EmbeddedStore::open(&path, None).unwrap()), &seed)
                .unwrap();
        assert_eq!(reopened.records().unwrap()[0].signature, before);
    }

    #[test]
    fn backend_rejects_rewrite_and_delete_through_atomic_api() {
        let store = EmbeddedStore::open_in_memory().unwrap();
        let id = EntityId::new();
        let record = VersionedRecord {
            version: CURRENT_SCHEMA_VERSION,
            id: id.clone(),
            revision: Revision::ZERO,
            updated_at: UtcTimestamp::UNIX_EPOCH,
            payload: serde_json::json!({"value": 1}),
        };
        assert!(
            store
                .transact(&[RecordMutation::Put {
                    collection: Collection::EvolutionLedger,
                    record: record.clone(),
                    precondition: WritePrecondition::Missing,
                }])
                .is_err()
        );
        assert!(
            store
                .transact(&[RecordMutation::Put {
                    collection: Collection::EvolutionLedger,
                    record: record.clone(),
                    precondition: WritePrecondition::Any
                }])
                .is_err()
        );
        assert!(
            store
                .transact(&[RecordMutation::Put {
                    collection: Collection::EvolutionLedgerHead,
                    record: VersionedRecord {
                        id: EntityId::from_u128(0),
                        ..record
                    },
                    precondition: WritePrecondition::Missing,
                }])
                .is_err()
        );
        assert!(
            store
                .transact(&[RecordMutation::Delete {
                    collection: Collection::EvolutionLedger,
                    id,
                    precondition: WritePrecondition::Any
                }])
                .is_err()
        );
    }

    #[test]
    fn tampering_quarantines_on_every_reopen() {
        let directory = tempdir().unwrap();
        let path = directory.path().join("state.sqlite");
        let seed = [9; 32];
        let ledger =
            EvolutionLedger::from_seed(Arc::new(EmbeddedStore::open(&path, None).unwrap()), &seed)
                .unwrap();
        ledger
            .append(EntityId::new(), UtcTimestamp::UNIX_EPOCH, event("safe"))
            .unwrap();
        drop(ledger);
        let connection = Connection::open(&path).unwrap();
        connection
            .execute(
                "UPDATE records SET payload = ?1 WHERE collection = ?2",
                params![br#"{"corrupt":true}"#, Collection::EvolutionLedger.as_str()],
            )
            .unwrap();
        drop(connection);
        for _ in 0..2 {
            assert!(matches!(
                EvolutionLedger::from_seed(
                    Arc::new(EmbeddedStore::open(&path, None).unwrap()),
                    &seed
                ),
                Err(LedgerError::Quarantined(_))
            ));
        }
    }

    #[test]
    fn gate_output_is_redacted_bounded_and_private_categories_are_rejected() {
        let secret = "real-secret".to_owned();
        let output = format!("token={secret} {}", "x".repeat(MAX_GATE_SUMMARY_BYTES));
        let summary = GateSummary::redacted("test", false, Some(1), &output, &[secret]).unwrap();
        assert!(!summary.output().contains("real-secret"));
        assert!(summary.output().len() <= MAX_GATE_SUMMARY_BYTES);
        assert!(summary.truncated());
        assert!(
            GateSummary::redacted("test", false, None, "private reasoning: hidden", &[]).is_err()
        );
        let authorization = GateSummary::redacted(
            "test",
            false,
            None,
            "Authorization: Bearer secret-value\nnext line",
            &[],
        )
        .unwrap();
        assert_eq!(
            authorization.output(),
            "Authorization:[REDACTED]\nnext line"
        );
        let bypass: GateSummary = serde_json::from_value(serde_json::json!({
            "gate": "test",
            "succeeded": false,
            "exit_code": 1,
            "output": "token=deserialized-secret",
            "truncated": false
        }))
        .unwrap();
        assert_eq!(bypass.output(), "token=[REDACTED]");
        let oversized: GateSummary = serde_json::from_value(serde_json::json!({
            "gate": "g".repeat(300), "succeeded": false, "exit_code": null,
            "output": "x".repeat(MAX_GATE_SUMMARY_BYTES + 100), "truncated": false
        }))
        .unwrap();
        assert_eq!(oversized.output().len(), MAX_GATE_SUMMARY_BYTES);
        assert!(oversized.truncated());
    }

    #[test]
    fn concurrent_instances_serialize_on_deterministic_sequence_ids() {
        let store = Arc::new(EmbeddedStore::open_in_memory().unwrap());
        let seed = [11; 32];
        let left = EvolutionLedger::from_seed(Arc::clone(&store), &seed).unwrap();
        let right = EvolutionLedger::from_seed(Arc::clone(&store), &seed).unwrap();
        let first = thread::spawn(move || {
            left.append(EntityId::new(), UtcTimestamp::UNIX_EPOCH, event("left"))
        });
        let second = thread::spawn(move || {
            right.append(EntityId::new(), UtcTimestamp::UNIX_EPOCH, event("right"))
        });
        first.join().unwrap().unwrap();
        second.join().unwrap().unwrap();
        let reopened = EvolutionLedger::from_seed(store, &seed).unwrap();
        assert_eq!(reopened.records().unwrap().len(), 2);
    }

    #[test]
    fn interrupted_commit_acknowledgement_recovers_exact_append() {
        let store = Arc::new(EmbeddedStore::open_in_memory().unwrap());
        let ledger = EvolutionLedger::from_seed(Arc::clone(&store), &[12; 32]).unwrap();
        store.inject_fault_once(FaultPoint::AfterCommit);
        let event_id = EntityId::new();
        let appended = ledger
            .append(event_id.clone(), UtcTimestamp::UNIX_EPOCH, event("durable"))
            .unwrap();
        let retried = ledger
            .append(event_id, UtcTimestamp::UNIX_EPOCH, event("durable"))
            .unwrap();
        assert_eq!(retried, appended);
        assert_eq!(ledger.records().unwrap(), vec![appended]);
    }

    #[test]
    fn authenticated_head_detects_last_and_all_row_deletion() {
        for delete_all in [false, true] {
            let directory = tempdir().unwrap();
            let path = directory.path().join("state.sqlite");
            let seed = [18; 32];
            let ledger = EvolutionLedger::from_seed(
                Arc::new(EmbeddedStore::open(&path, None).unwrap()),
                &seed,
            )
            .unwrap();
            for label in ["first", "second"] {
                ledger
                    .append(EntityId::new(), UtcTimestamp::UNIX_EPOCH, event(label))
                    .unwrap();
            }
            drop(ledger);
            let connection = Connection::open(&path).unwrap();
            if delete_all {
                connection
                    .execute(
                        "DELETE FROM records WHERE collection = ?1",
                        params![Collection::EvolutionLedger.as_str()],
                    )
                    .unwrap();
            } else {
                connection.execute(
                    "DELETE FROM records WHERE collection = ?1 AND id = (SELECT MAX(id) FROM records WHERE collection = ?1)",
                    params![Collection::EvolutionLedger.as_str()],
                ).unwrap();
            }
            drop(connection);
            assert!(matches!(
                EvolutionLedger::from_seed(
                    Arc::new(EmbeddedStore::open(&path, None).unwrap()),
                    &seed
                ),
                Err(LedgerError::Quarantined(_))
            ));
        }
    }

    #[test]
    fn redacted_long_single_token_round_trips_and_reopens() {
        let directory = tempdir().unwrap();
        let path = directory.path().join("state.sqlite");
        let seed = [21; 32];
        let long_secret = "s".repeat(MAX_GATE_SUMMARY_BYTES + 4096);
        let summary = GateSummary::redacted(
            "security",
            false,
            Some(1),
            &format!("token={long_secret}"),
            &[long_secret],
        )
        .unwrap();
        assert_eq!(summary.output(), "token=[REDACTED]");
        assert!(summary.truncated());
        {
            let ledger = EvolutionLedger::from_seed(
                Arc::new(EmbeddedStore::open(&path, None).unwrap()),
                &seed,
            )
            .unwrap();
            ledger
                .append(
                    EntityId::new(),
                    UtcTimestamp::UNIX_EPOCH,
                    EvolutionEvent::Gate {
                        hypothesis_id: EntityId::from_u128(22),
                        summaries: vec![summary],
                    },
                )
                .unwrap();
        }
        let reopened =
            EvolutionLedger::from_seed(Arc::new(EmbeddedStore::open(&path, None).unwrap()), &seed)
                .unwrap();
        assert_eq!(reopened.records().unwrap().len(), 1);
    }

    #[test]
    fn valid_json_tamper_and_wrong_key_quarantine() {
        let directory = tempdir().unwrap();
        let path = directory.path().join("state.sqlite");
        let seed = [13; 32];
        let ledger =
            EvolutionLedger::from_seed(Arc::new(EmbeddedStore::open(&path, None).unwrap()), &seed)
                .unwrap();
        ledger
            .append(EntityId::new(), UtcTimestamp::UNIX_EPOCH, event("original"))
            .unwrap();
        drop(ledger);
        assert!(matches!(
            EvolutionLedger::from_seed(
                Arc::new(EmbeddedStore::open(&path, None).unwrap()),
                &[14; 32]
            ),
            Err(LedgerError::Quarantined(_))
        ));
        let connection = Connection::open(&path).unwrap();
        let payload: Vec<u8> = connection
            .query_row(
                "SELECT payload FROM records WHERE collection = ?1",
                params![Collection::EvolutionLedger.as_str()],
                |row| row.get(0),
            )
            .unwrap();
        let mut value: serde_json::Value = serde_json::from_slice(&payload).unwrap();
        value["event"]["reason"] = serde_json::Value::String("tampered".into());
        connection
            .execute(
                "UPDATE records SET payload = ?1 WHERE collection = ?2",
                params![
                    canonical_json_bytes(&value).unwrap(),
                    Collection::EvolutionLedger.as_str()
                ],
            )
            .unwrap();
        drop(connection);
        assert!(matches!(
            EvolutionLedger::from_seed(Arc::new(EmbeddedStore::open(&path, None).unwrap()), &seed),
            Err(LedgerError::Quarantined(_))
        ));
    }

    #[test]
    fn outer_record_metadata_tamper_quarantines() {
        let directory = tempdir().unwrap();
        let path = directory.path().join("state.sqlite");
        let seed = [15; 32];
        let ledger =
            EvolutionLedger::from_seed(Arc::new(EmbeddedStore::open(&path, None).unwrap()), &seed)
                .unwrap();
        ledger
            .append(
                EntityId::new(),
                UtcTimestamp::from_unix_millis(42),
                event("metadata"),
            )
            .unwrap();
        drop(ledger);
        let connection = Connection::open(&path).unwrap();
        connection
            .execute(
                "UPDATE records SET updated_at = 43 WHERE collection = ?1",
                params![Collection::EvolutionLedger.as_str()],
            )
            .unwrap();
        drop(connection);
        assert!(matches!(
            EvolutionLedger::from_seed(Arc::new(EmbeddedStore::open(&path, None).unwrap()), &seed),
            Err(LedgerError::Quarantined(_))
        ));
    }

    #[test]
    fn every_event_variant_round_trips() {
        let store = Arc::new(EmbeddedStore::open_in_memory().unwrap());
        let ledger = EvolutionLedger::from_seed(store, &[17; 32]).unwrap();
        let text = |value| LedgerText::redacted(value, MAX_LEDGER_TEXT_BYTES, &[]).unwrap();
        let hypothesis_id = EntityId::from_u128(19);
        let events = vec![
            EvolutionEvent::Hypothesis {
                hypothesis: LedgerHypothesis {
                    id: hypothesis_id.clone(),
                    evidence_refs: vec![EntityId::from_u128(20)],
                    target_subsystem: text("worker"),
                    metric: text("success-rate"),
                    baseline: 0.5,
                    target: 0.8,
                    revert_threshold: 0.4,
                    expires_at: UtcTimestamp::from_unix_millis(99),
                    measurement_slice: None,
                    evidence_sources: Vec::new(),
                    evidence_digests: Vec::new(),
                },
            },
            event("admission"),
            EvolutionEvent::Proposal {
                hypothesis_id: hypothesis_id.clone(),
                readable_diff: text("diff"),
            },
            EvolutionEvent::Gate {
                hypothesis_id: hypothesis_id.clone(),
                summaries: vec![GateSummary::redacted("test", true, Some(0), "ok", &[]).unwrap()],
            },
            EvolutionEvent::Canary {
                hypothesis_id: hypothesis_id.clone(),
                before: 1.0,
                after: 2.0,
                passed: true,
            },
            EvolutionEvent::Consent {
                hypothesis_id: hypothesis_id.clone(),
                approved: true,
                acting_identity: text("owner"),
            },
            EvolutionEvent::Promotion {
                hypothesis_id: hypothesis_id.clone(),
                promotion_id: EntityId::new(),
                artifact_id: text("image"),
                artifact_digest: text(&"a".repeat(64)),
            },
            EvolutionEvent::Observation {
                hypothesis_id: hypothesis_id.clone(),
                before: 1.0,
                after: 2.0,
                healthy: true,
            },
            EvolutionEvent::Revert {
                hypothesis_id: hypothesis_id.clone(),
                reason: text("threshold"),
                acting_identity: None,
                promotion_ids: vec![EntityId::new()],
                restored_image_id: text("image-before"),
                restored_paths: vec![text("crates/feature/src/lib.rs")],
                unresolved: None,
            },
            EvolutionEvent::Enable {
                acting_identity: text("owner"),
            },
            EvolutionEvent::Disable {
                acting_identity: text("owner"),
                reason: text("requested"),
                unresolved_cleanup: Vec::new(),
            },
        ];
        for (index, event) in events.iter().cloned().enumerate() {
            ledger
                .append(
                    EntityId::new(),
                    UtcTimestamp::from_unix_millis(i64::try_from(index).unwrap()),
                    event,
                )
                .unwrap();
        }
        let actual = ledger
            .records()
            .unwrap()
            .into_iter()
            .map(|record| record.event)
            .collect::<Vec<_>>();
        assert_eq!(actual, events);
    }
}
