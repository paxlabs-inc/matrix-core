use std::fs;

use keith_agent_types::{SchemaVersion, canonical_json_bytes};
use keith_session_store::{SessionEntry, SessionEntryPayload, SessionManifest, SessionStore};
use tempfile::tempdir;

const MANIFEST: &[u8] = include_bytes!("fixtures/causal-v1/session-manifest.json");
const HISTORY: &[u8] = include_bytes!("fixtures/causal-v1/session-history.jsonl");

#[test]
fn legacy_real_session_reopens_with_original_checksums_and_finalization() {
    let root = tempdir().unwrap();
    let manifest: SessionManifest = serde_json::from_slice(MANIFEST).unwrap();
    assert_eq!(canonical_json_bytes(&manifest).unwrap(), MANIFEST);
    let directory = root
        .path()
        .join("sessions")
        .join(manifest.session_id.to_string());
    fs::create_dir_all(&directory).unwrap();
    fs::write(directory.join("manifest.json"), MANIFEST).unwrap();
    fs::write(directory.join("history.jsonl"), HISTORY).unwrap();
    let store = SessionStore::open(root.path()).unwrap();
    let reopened = store.manifest(&manifest.session_id).unwrap();
    assert_eq!(reopened, manifest);
    let index = store.load_index(&manifest.session_id).unwrap();
    let entries = index
        .ancestry(manifest.active_leaf.as_ref().unwrap())
        .unwrap();
    let mut encoded = Vec::new();
    for entry in &entries {
        entry.verify().unwrap();
        encoded.extend(canonical_json_bytes(entry).unwrap());
        encoded.push(b'\n');
    }
    assert_eq!(encoded, HISTORY);
    assert_eq!(entries.len(), 10);
    assert!(
        entries
            .iter()
            .any(|entry| matches!(entry.payload, SessionEntryPayload::AssistantFinal { .. }))
    );
    assert!(
        entries
            .iter()
            .any(|entry| matches!(entry.payload, SessionEntryPayload::TerminalTurn { .. }))
    );
    assert!(entries.iter().any(|entry| matches!(
        entry.payload,
        SessionEntryPayload::ToolResult { failure: None, .. }
    )));
    drop(store);
    assert_eq!(
        SessionStore::open(root.path())
            .unwrap()
            .load_index(&manifest.session_id)
            .unwrap()
            .len(),
        entries.len()
    );
    assert_eq!(fs::read(directory.join("history.jsonl")).unwrap(), HISTORY);
}

#[test]
fn legacy_session_future_schema_and_changed_content_fail_closed() {
    let first = HISTORY.split(|byte| *byte == b'\n').next().unwrap();
    let mut entry: SessionEntry = serde_json::from_slice(first).unwrap();
    entry.version = SchemaVersion::new(1, 1);
    assert!(entry.verify().is_err());
    let mut entry: SessionEntry = serde_json::from_slice(first).unwrap();
    entry.timestamp = keith_agent_types::UtcTimestamp::UNIX_EPOCH;
    assert!(entry.verify().is_err());
}
