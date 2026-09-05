use std::collections::BTreeMap;
use std::fs::OpenOptions;
use std::io::Write;
#[cfg(unix)]
use std::os::unix::fs::{OpenOptionsExt, PermissionsExt};
use std::path::{Path, PathBuf};

use serde::{Deserialize, Serialize};

use crate::{AcpSessionRecord, BridgeError};

const STORE_SCHEMA_VERSION: u16 = 1;

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
struct Registry {
    schema_version: u16,
    sessions: BTreeMap<String, AcpSessionRecord>,
}

impl Default for Registry {
    fn default() -> Self {
        Self {
            schema_version: STORE_SCHEMA_VERSION,
            sessions: BTreeMap::new(),
        }
    }
}

pub struct AcpSessionStore {
    path: PathBuf,
    registry: Registry,
}

impl AcpSessionStore {
    /// Opens and validates the durable ACP session registry.
    ///
    /// # Errors
    ///
    /// Returns an error when the state directory cannot be secured or persisted state is malformed.
    pub fn open(state_root: impl AsRef<Path>) -> Result<Self, BridgeError> {
        let state_root = state_root.as_ref();
        std::fs::create_dir_all(state_root)?;
        #[cfg(unix)]
        std::fs::set_permissions(state_root, std::fs::Permissions::from_mode(0o700))?;
        let path = state_root.join("sessions-v1.json");
        let registry = match std::fs::read(&path) {
            Ok(bytes) => {
                let registry: Registry = serde_json::from_slice(&bytes)?;
                if registry.schema_version != STORE_SCHEMA_VERSION {
                    return Err(BridgeError::UnexpectedResponse(
                        "unsupported ACP state schema version",
                    ));
                }
                registry
            }
            Err(error) if error.kind() == std::io::ErrorKind::NotFound => Registry::default(),
            Err(error) => return Err(error.into()),
        };
        validate_registry(&registry)?;
        Ok(Self { path, registry })
    }

    pub fn get(&self, session_id: &str) -> Option<&AcpSessionRecord> {
        self.registry.sessions.get(session_id)
    }

    /// Inserts a session record after validating its fork lineage.
    ///
    /// # Errors
    ///
    /// Returns an error when lineage is invalid or the updated registry cannot be persisted.
    pub fn insert(&mut self, record: AcpSessionRecord) -> Result<(), BridgeError> {
        validate_record(&self.registry, &record)?;
        self.registry
            .sessions
            .insert(record.acp_session_id.clone(), record);
        self.save()
    }

    /// Applies and durably persists one session-record mutation.
    ///
    /// # Errors
    ///
    /// Returns an error when the session is unknown, the mutation fails, or state cannot be saved.
    pub fn update(
        &mut self,
        session_id: &str,
        update: impl FnOnce(&mut AcpSessionRecord) -> Result<(), BridgeError>,
    ) -> Result<AcpSessionRecord, BridgeError> {
        let record = self
            .registry
            .sessions
            .get_mut(session_id)
            .ok_or_else(|| BridgeError::UnknownSession(session_id.to_owned()))?;
        update(record)?;
        let record = record.clone();
        self.save()?;
        Ok(record)
    }

    pub fn records(&self) -> impl Iterator<Item = &AcpSessionRecord> {
        self.registry.sessions.values()
    }

    fn save(&self) -> Result<(), BridgeError> {
        let bytes = serde_json::to_vec_pretty(&self.registry)?;
        let temporary = self.path.with_extension("json.tmp");
        let mut options = OpenOptions::new();
        options.write(true).create(true).truncate(true);
        #[cfg(unix)]
        options.mode(0o600);
        let mut file = options.open(&temporary)?;
        file.write_all(&bytes)?;
        file.sync_all()?;
        std::fs::rename(&temporary, &self.path)?;
        if let Some(parent) = self.path.parent() {
            OpenOptions::new().read(true).open(parent)?.sync_all()?;
        }
        Ok(())
    }
}

fn validate_registry(registry: &Registry) -> Result<(), BridgeError> {
    for (session_id, record) in &registry.sessions {
        if session_id != &record.acp_session_id {
            return Err(BridgeError::UnexpectedResponse(
                "ACP state key does not match its session record",
            ));
        }
        validate_record(registry, record)?;
    }
    Ok(())
}

fn validate_record(registry: &Registry, record: &AcpSessionRecord) -> Result<(), BridgeError> {
    if record
        .protocol_version
        .is_some_and(|version| !matches!(version, 1 | 2))
    {
        return Err(BridgeError::UnexpectedResponse(
            "ACP state contains an unsupported protocol version",
        ));
    }
    if let Some(context) = &record.client_context {
        let mut server_ids = std::collections::BTreeSet::new();
        if context
            .mcp_servers
            .iter()
            .any(|server| !server_ids.insert(&server.id))
        {
            return Err(BridgeError::UnexpectedResponse(
                "ACP state contains duplicate session-scoped MCP server ids",
            ));
        }
    }
    if record.forked_from.as_deref() == Some(record.acp_session_id.as_str()) {
        return Err(BridgeError::UnexpectedResponse(
            "ACP fork lineage cannot reference itself",
        ));
    }
    if let Some(source) = &record.forked_from
        && !registry.sessions.contains_key(source)
    {
        return Err(BridgeError::UnknownSession(source.clone()));
    }
    Ok(())
}

#[cfg(test)]
mod tests {
    use keith_agent_types::{ProfileId, SessionId, WorkspaceId};
    use tempfile::tempdir;

    use super::*;
    use crate::AcpSessionStatus;

    #[test]
    fn state_survives_process_style_reopen() {
        let directory = tempdir().unwrap();
        let record = AcpSessionRecord {
            acp_session_id: "session".to_owned(),
            keith_session_id: SessionId::new(),
            profile_id: ProfileId::new(),
            workspace_id: WorkspaceId::new(),
            cwd: directory.path().to_path_buf(),
            additional_directories: Vec::new(),
            status: AcpSessionStatus::Ready,
            cursor: None,
            next_prompt_ordinal: 0,
            in_flight_prompt: None,
            forked_from: None,
            client_context: None,
            protocol_version: None,
        };
        AcpSessionStore::open(directory.path())
            .unwrap()
            .insert(record.clone())
            .unwrap();
        let fork = AcpSessionRecord {
            acp_session_id: "fork".to_owned(),
            keith_session_id: SessionId::new(),
            forked_from: Some(record.acp_session_id.clone()),
            ..record.clone()
        };
        AcpSessionStore::open(directory.path())
            .unwrap()
            .insert(fork.clone())
            .unwrap();
        let reopened = AcpSessionStore::open(directory.path()).unwrap();
        assert_eq!(reopened.get("session"), Some(&record));
        assert_eq!(reopened.get("fork"), Some(&fork));
    }

    #[test]
    fn state_rejects_missing_or_self_referential_fork_lineage() {
        let directory = tempdir().unwrap();
        let record = AcpSessionRecord {
            acp_session_id: "fork".to_owned(),
            keith_session_id: SessionId::new(),
            profile_id: ProfileId::new(),
            workspace_id: WorkspaceId::new(),
            cwd: directory.path().to_path_buf(),
            additional_directories: Vec::new(),
            status: AcpSessionStatus::Ready,
            cursor: None,
            next_prompt_ordinal: 0,
            in_flight_prompt: None,
            forked_from: Some("missing".to_owned()),
            client_context: None,
            protocol_version: None,
        };
        let mut store = AcpSessionStore::open(directory.path()).unwrap();
        assert!(matches!(
            store.insert(record.clone()),
            Err(BridgeError::UnknownSession(session)) if session == "missing"
        ));
        let self_referential = AcpSessionRecord {
            forked_from: Some(record.acp_session_id.clone()),
            ..record
        };
        assert!(matches!(
            store.insert(self_referential),
            Err(BridgeError::UnexpectedResponse(
                "ACP fork lineage cannot reference itself"
            ))
        ));
    }
}
