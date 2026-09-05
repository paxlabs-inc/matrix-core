use std::collections::BTreeMap;
use std::fs;

use keith_agent_types::{EntityId, ProfileId, SchemaVersion, UtcTimestamp, canonical_json_bytes};
use keith_memory::{
    CandidateEvidenceReference, EVIDENCE_CAUSAL_VERSION, EvidenceCausalMetadata,
    EvidenceEffectiveInterval, EvidenceRecord, EvidenceSourceRoot, EvidenceValidity,
    MemoryObservatory, ObservatoryLimits, ObservatoryMutation, validate_activation,
};
use keith_session_store::{MemoryActivationManifest, Sensitivity};
use sha2::{Digest, Sha256};
use tempfile::{TempDir, tempdir};

const VAULT: &[u8] = include_bytes!("fixtures/causal-v1/memory-vault.jsonl");
const EVIDENCE: &[u8] = include_bytes!("fixtures/causal-v1/evidence.json");
const ACTIVATION: &[u8] = include_bytes!("fixtures/causal-v1/activation.json");

fn fixture() -> (TempDir, MemoryObservatory, MemoryActivationManifest) {
    let root = tempdir().unwrap();
    fs::create_dir(root.path().join(".keith")).unwrap();
    fs::write(root.path().join(".keith/memory-vault.jsonl"), VAULT).unwrap();
    let activation: MemoryActivationManifest = serde_json::from_slice(ACTIVATION).unwrap();
    let observatory = MemoryObservatory::open(
        root.path(),
        &activation.profile_id,
        ObservatoryLimits::default(),
        UtcTimestamp::UNIX_EPOCH,
    )
    .unwrap();
    (root, observatory, activation)
}

fn reopen(root: &TempDir, profile: &ProfileId) -> MemoryObservatory {
    MemoryObservatory::open(
        root.path(),
        profile,
        ObservatoryLimits::default(),
        UtcTimestamp::UNIX_EPOCH,
    )
    .unwrap()
}

fn metadata(record: &EvidenceRecord) -> EvidenceCausalMetadata {
    EvidenceCausalMetadata {
        version: EVIDENCE_CAUSAL_VERSION,
        derived_from: vec![],
        gaps: vec![],
        effective: Some(EvidenceEffectiveInterval {
            from: Some(UtcTimestamp::UNIX_EPOCH),
            until: None,
        }),
        source_roots: vec![EvidenceSourceRoot {
            source_session: record.source_session.clone(),
            source_entry: record.source_entries[0].clone(),
            source_digest: record.source_digests[0].clone(),
        }],
    }
}

fn reference(record: &EvidenceRecord, revision: u64) -> CandidateEvidenceReference {
    CandidateEvidenceReference {
        evidence_id: record.id.clone(),
        content_digest: record.content_digest.clone(),
        archive_revision: revision,
    }
}

#[test]
fn legacy_vault_and_activation_preserve_bytes_checksums_and_rebuild() {
    let (root, observatory, activation) = fixture();
    let records: BTreeMap<EntityId, EvidenceRecord> = serde_json::from_slice(EVIDENCE).unwrap();
    assert!(records.values().all(|record| record.causal.is_none()));
    assert_eq!(canonical_json_bytes(&records).unwrap(), EVIDENCE);
    assert_eq!(canonical_json_bytes(&activation).unwrap(), ACTIVATION);
    assert_eq!(observatory.evidence_snapshot().unwrap(), records);
    assert_eq!(observatory.revision().unwrap(), 4);
    validate_activation(&observatory, &activation, Sensitivity::Personal).unwrap();
    fs::write(
        root.path().join(".keith/memory-atlas.json"),
        b"broken projection",
    )
    .unwrap();
    drop(observatory);
    let reopened = reopen(&root, &activation.profile_id);
    assert_eq!(reopened.evidence_snapshot().unwrap(), records);
    assert!(
        reopened
            .health_snapshot()
            .unwrap()
            .quarantined_atlas
            .is_some()
    );
    validate_activation(&reopened, &activation, Sensitivity::Personal).unwrap();
    assert_eq!(
        fs::read(root.path().join(".keith/memory-vault.jsonl")).unwrap(),
        VAULT
    );
}

#[test]
fn canonical_candidate_resolution_checks_scope_sensitivity_and_content() {
    let (_root, observatory, activation) = fixture();
    let prior = observatory
        .evidence_snapshot()
        .unwrap()
        .into_values()
        .next()
        .unwrap();
    let initial_revision = observatory.revision().unwrap();
    let mut prior_ref = reference(&prior, initial_revision);
    assert_eq!(
        observatory
            .resolve_candidate(
                &prior_ref,
                &activation.profile_id,
                initial_revision,
                Sensitivity::Personal
            )
            .unwrap(),
        prior
    );
    assert!(
        observatory
            .resolve_candidate(
                &prior_ref,
                &ProfileId::new(),
                initial_revision,
                Sensitivity::Personal
            )
            .is_err()
    );
    assert!(
        observatory
            .resolve_candidate(
                &prior_ref,
                &activation.profile_id,
                initial_revision,
                Sensitivity::Public
            )
            .is_err()
    );
    prior_ref.content_digest = "f".repeat(64);
    assert!(
        observatory
            .resolve_candidate(
                &prior_ref,
                &activation.profile_id,
                initial_revision,
                Sensitivity::Personal
            )
            .is_err()
    );
}

#[test]
fn new_metadata_roundtrips_through_real_vault_correction_delete_and_rebuild() {
    let (root, observatory, activation) = fixture();
    let prior = observatory
        .evidence_snapshot()
        .unwrap()
        .into_values()
        .next()
        .unwrap();
    let initial_revision = observatory.revision().unwrap();
    let prior_ref = reference(&prior, initial_revision);
    let mut replacement = prior.clone();
    replacement.id = EntityId::new();
    replacement.source_identity = format!("correction:{}", replacement.id);
    replacement.causal = Some(metadata(&prior));
    let replacement_id = replacement.id.clone();
    let roundtrip: EvidenceRecord =
        serde_json::from_slice(&canonical_json_bytes(&replacement).unwrap()).unwrap();
    assert_eq!(replacement, roundtrip);
    observatory
        .apply(
            vec![ObservatoryMutation::Supersede {
                prior_id: prior.id.clone(),
                replacement,
            }],
            UtcTimestamp::from_unix_millis(3_000),
        )
        .unwrap();
    assert!(
        observatory
            .resolve_candidate(
                &prior_ref,
                &activation.profile_id,
                initial_revision,
                Sensitivity::Personal
            )
            .is_err()
    );
    let revision = observatory.revision().unwrap();
    assert!(
        observatory
            .resolve_candidate(
                &prior_ref,
                &activation.profile_id,
                revision,
                Sensitivity::Personal
            )
            .is_err()
    );
    assert!(validate_activation(&observatory, &activation, Sensitivity::Personal).is_err());
    drop(observatory);
    let reopened = reopen(&root, &activation.profile_id);
    let replacement = reopened
        .evidence(std::slice::from_ref(&replacement_id), Sensitivity::Personal)
        .unwrap()
        .remove(0);
    assert_eq!(replacement.causal, Some(metadata(&prior)));
    assert_eq!(replacement.supersedes, Some(prior.id.clone()));
    let replacement_ref = reference(&replacement, revision);
    reopened
        .resolve_candidate(
            &replacement_ref,
            &activation.profile_id,
            revision,
            Sensitivity::Personal,
        )
        .unwrap();
    reopened
        .apply(
            vec![ObservatoryMutation::Delete {
                evidence_id: replacement_id.clone(),
                source_entries: vec![],
                source_digests: vec![],
            }],
            UtcTimestamp::from_unix_millis(4_000),
        )
        .unwrap();
    fs::remove_file(root.path().join(".keith/memory-atlas.json")).unwrap();
    drop(reopened);
    let rebuilt = reopen(&root, &activation.profile_id);
    let snapshot = rebuilt.evidence_snapshot().unwrap();
    assert_eq!(snapshot[&prior.id].validity, EvidenceValidity::Superseded);
    assert_eq!(
        snapshot[&replacement_id].validity,
        EvidenceValidity::Deleted
    );
    assert!(
        rebuilt
            .resolve_candidate(
                &replacement_ref,
                &activation.profile_id,
                rebuilt.revision().unwrap(),
                Sensitivity::Personal
            )
            .is_err()
    );
}

#[test]
fn metadata_rejects_unknown_versions_duplicate_roots_and_invalid_intervals() {
    let records: BTreeMap<EntityId, EvidenceRecord> = serde_json::from_slice(EVIDENCE).unwrap();
    let record = records.values().next().unwrap();
    let original = metadata(record);
    for version in [
        SchemaVersion::new(0, 0),
        SchemaVersion::new(1, 1),
        SchemaVersion::new(2, 0),
    ] {
        let mut invalid = original.clone();
        invalid.version = version;
        assert!(invalid.validate().is_err());
        assert!(
            serde_json::from_slice::<EvidenceCausalMetadata>(
                &canonical_json_bytes(&invalid).unwrap()
            )
            .is_err()
        );
    }
    let mut invalid = original.clone();
    invalid.source_roots.push(invalid.source_roots[0].clone());
    assert!(invalid.validate().is_err());
    let mut invalid = original;
    invalid.effective.as_mut().unwrap().until = Some(UtcTimestamp::UNIX_EPOCH);
    assert!(invalid.validate().is_err());
}

#[test]
fn complete_future_metadata_record_is_not_discarded_as_a_torn_tail() {
    let (root, observatory, activation) = fixture();
    drop(observatory);
    let mut lines: Vec<serde_json::Value> = VAULT
        .split(|byte| *byte == b'\n')
        .filter(|line| !line.is_empty())
        .map(|line| serde_json::from_slice(line).unwrap())
        .collect();
    let last = lines.last_mut().unwrap();
    last["mutation"]["evidence"]["causal"] =
        serde_json::json!({"version":{"major":2,"minor":0},"source_roots":[]});
    last.as_object_mut().unwrap().remove("digest");
    let digest = format!("{:x}", Sha256::digest(canonical_json_bytes(last).unwrap()));
    last["digest"] = digest.into();
    let bytes = lines
        .iter()
        .map(|line| String::from_utf8(canonical_json_bytes(line).unwrap()).unwrap())
        .collect::<Vec<_>>()
        .join("\n")
        .into_bytes();
    let path = root.path().join(".keith/memory-vault.jsonl");
    fs::write(&path, &bytes).unwrap();
    assert!(
        MemoryObservatory::open(
            root.path(),
            &activation.profile_id,
            ObservatoryLimits::default(),
            UtcTimestamp::UNIX_EPOCH
        )
        .is_err()
    );
    assert_eq!(fs::read(path).unwrap(), bytes);
}

#[test]
fn invalid_metadata_cannot_enter_the_canonical_vault() {
    let (root, observatory, _) = fixture();
    let mut record = observatory
        .evidence_snapshot()
        .unwrap()
        .into_values()
        .next()
        .unwrap();
    record.id = EntityId::new();
    record.source_identity = format!("invalid:{}", record.id);
    record.causal = Some(metadata(&record));
    record.causal.as_mut().unwrap().version = SchemaVersion::new(2, 0);
    assert!(
        observatory
            .apply(
                vec![ObservatoryMutation::Observe(record)],
                UtcTimestamp::UNIX_EPOCH
            )
            .is_err()
    );
    assert_eq!(
        fs::read(root.path().join(".keith/memory-vault.jsonl")).unwrap(),
        VAULT
    );
}
