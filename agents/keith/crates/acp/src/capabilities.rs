use std::collections::{BTreeMap, BTreeSet};
use std::path::{Path, PathBuf};
use std::sync::Mutex;

use serde::{Deserialize, Serialize};
use serde_json::{Value, json};
use url::Url;

use crate::{AcpSessionRecord, BridgeError};

const MAX_NAME_BYTES: usize = 128;
const MAX_ARGUMENTS: usize = 256;
const MAX_ARGUMENT_BYTES: usize = 64 * 1_024;
const MAX_ENVIRONMENT: usize = 64;

#[derive(Clone, Copy, Debug, Default, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct AcpClientCapabilities {
    pub read_text_file: bool,
    pub write_text_file: bool,
    pub terminal: bool,
}

impl AcpClientCapabilities {
    #[must_use]
    pub const fn intersect(self, other: Self) -> Self {
        Self {
            read_text_file: self.read_text_file && other.read_text_file,
            write_text_file: self.write_text_file && other.write_text_file,
            terminal: self.terminal && other.terminal,
        }
    }
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case", tag = "placement", content = "name")]
pub enum AcpCredentialPlacement {
    Header(String),
    Environment(String),
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct AcpCredentialBinding {
    pub reference: String,
    pub placement: AcpCredentialPlacement,
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case", tag = "transport")]
pub enum AcpMcpTransport {
    Stdio {
        executable: PathBuf,
        args: Vec<String>,
        credentials: Vec<AcpCredentialBinding>,
    },
    Http {
        endpoint: String,
        credentials: Vec<AcpCredentialBinding>,
    },
    Sse {
        endpoint: String,
        credentials: Vec<AcpCredentialBinding>,
    },
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct AcpMcpServer {
    pub id: String,
    pub transport: AcpMcpTransport,
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case", tag = "state", content = "safe_error")]
pub enum AcpMcpHealth {
    Pending,
    Healthy,
    Unhealthy(String),
}

#[derive(Clone, Debug, Default, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct AcpClientSessionConfig {
    pub capabilities: AcpClientCapabilities,
    pub mcp_servers: Vec<AcpMcpServer>,
}

impl AcpClientSessionConfig {
    /// Returns the least authority shared by a durable session and a reconnecting client.
    ///
    /// # Errors
    ///
    /// Returns an error if the client reuses a server id with different configuration.
    pub fn restrict_with(&self, reconnect: &Self) -> Result<Self, BridgeError> {
        let reconnect_servers = reconnect
            .mcp_servers
            .iter()
            .map(|server| (server.id.as_str(), server))
            .collect::<BTreeMap<_, _>>();
        let mut mcp_servers = Vec::new();
        for server in &self.mcp_servers {
            let Some(candidate) = reconnect_servers.get(server.id.as_str()) else {
                continue;
            };
            if *candidate != server {
                return Err(BridgeError::McpPolicy(format!(
                    "MCP server {} changed configuration during reconnect",
                    server.id
                )));
            }
            mcp_servers.push(server.clone());
        }
        Ok(Self {
            capabilities: self.capabilities.intersect(reconnect.capabilities),
            mcp_servers,
        })
    }
}

#[derive(Clone, Debug, Eq, PartialEq)]
pub struct AcpClientPolicy {
    pub capabilities: AcpClientCapabilities,
    pub allowed_executables: BTreeSet<PathBuf>,
    pub allowed_network_hosts: BTreeSet<String>,
    pub allowed_credential_references: BTreeSet<String>,
    pub max_mcp_servers: usize,
    pub max_mcp_schema_bytes: usize,
    pub max_terminal_output_bytes: u64,
}

impl Default for AcpClientPolicy {
    fn default() -> Self {
        Self {
            capabilities: AcpClientCapabilities {
                read_text_file: true,
                write_text_file: false,
                terminal: false,
            },
            allowed_executables: BTreeSet::new(),
            allowed_network_hosts: BTreeSet::new(),
            allowed_credential_references: BTreeSet::new(),
            max_mcp_servers: 16,
            max_mcp_schema_bytes: 128 * 1_024,
            max_terminal_output_bytes: 2 * 1_024 * 1_024,
        }
    }
}

#[derive(Clone, Debug, Eq, PartialEq)]
pub enum AcpClientToolKind {
    ReadTextFile,
    WriteTextFile,
    Terminal,
    Mcp { server_id: String },
}

#[derive(Clone, Debug, PartialEq)]
pub struct AcpClientTool {
    pub name: String,
    pub description: String,
    pub input_schema: Value,
    pub kind: AcpClientToolKind,
    pub available: bool,
}

#[derive(Clone, Debug, Eq, PartialEq)]
pub struct AcpTerminalRequest {
    pub executable: PathBuf,
    pub args: Vec<String>,
    pub cwd: PathBuf,
    pub environment: BTreeMap<String, String>,
    pub output_byte_limit: u64,
}

#[derive(Clone, Debug)]
struct BrokerSession {
    roots: Vec<PathBuf>,
    config: AcpClientSessionConfig,
    mcp_health: BTreeMap<String, AcpMcpHealth>,
}

pub struct AcpClientToolBroker {
    policy: AcpClientPolicy,
    sessions: Mutex<BTreeMap<String, BrokerSession>>,
}

impl AcpClientToolBroker {
    /// Constructs a client-tool broker with an explicit profile ceiling.
    ///
    /// # Errors
    ///
    /// Returns an error if a configured executable cannot be canonicalized or a bound is zero.
    pub fn new(mut policy: AcpClientPolicy) -> Result<Self, BridgeError> {
        if policy.max_mcp_servers == 0
            || policy.max_mcp_schema_bytes == 0
            || policy.max_terminal_output_bytes == 0
        {
            return Err(BridgeError::ClientCapability(
                "client-tool policy limits must be non-zero".to_owned(),
            ));
        }
        policy.allowed_executables = policy
            .allowed_executables
            .iter()
            .map(std::fs::canonicalize)
            .collect::<Result<_, _>>()?;
        Ok(Self {
            policy,
            sessions: Mutex::new(BTreeMap::new()),
        })
    }

    #[must_use]
    pub fn policy(&self) -> &AcpClientPolicy {
        &self.policy
    }

    /// Intersects a requested client context with the profile ceiling before any session exists.
    ///
    /// This is the preflight seam for protocol adapters: rejected MCP configuration never creates
    /// a durable session, while accepted configuration can later be registered against canonical
    /// session roots.
    ///
    /// # Errors
    ///
    /// Returns an error when a requested capability or MCP server exceeds the profile ceiling.
    pub fn validate_session_config(
        &self,
        requested: AcpClientSessionConfig,
    ) -> Result<AcpClientSessionConfig, BridgeError> {
        self.validate_config(requested)
    }

    /// Registers or narrows a session's client facilities and MCP servers.
    ///
    /// # Errors
    ///
    /// Returns an error for invalid roots, widened/replaced MCP configuration, or policy denial.
    pub fn register_session(
        &self,
        record: &AcpSessionRecord,
        requested: AcpClientSessionConfig,
    ) -> Result<AcpClientSessionConfig, BridgeError> {
        let mut roots = Vec::with_capacity(1 + record.additional_directories.len());
        roots.push(std::fs::canonicalize(&record.cwd)?);
        for root in &record.additional_directories {
            roots.push(std::fs::canonicalize(root)?);
        }
        let requested = self.validate_session_config(requested)?;
        let mut sessions = self
            .sessions
            .lock()
            .map_err(|_| BridgeError::LockPoisoned)?;
        let effective = sessions.get(&record.acp_session_id).map_or_else(
            || Ok(requested.clone()),
            |current| current.config.restrict_with(&requested),
        )?;
        let mcp_health = effective
            .mcp_servers
            .iter()
            .map(|server| {
                let health = sessions
                    .get(&record.acp_session_id)
                    .and_then(|current| current.mcp_health.get(&server.id))
                    .cloned()
                    .unwrap_or(AcpMcpHealth::Pending);
                (server.id.clone(), health)
            })
            .collect();
        sessions.insert(
            record.acp_session_id.clone(),
            BrokerSession {
                roots,
                config: effective.clone(),
                mcp_health,
            },
        );
        Ok(effective)
    }

    /// Lists the tools that survived both client negotiation and the profile ceiling.
    ///
    /// # Errors
    ///
    /// Returns an error when the session is unknown or broker state is unavailable.
    pub fn tools(&self, session_id: &str) -> Result<Vec<AcpClientTool>, BridgeError> {
        let sessions = self
            .sessions
            .lock()
            .map_err(|_| BridgeError::LockPoisoned)?;
        let session = sessions
            .get(session_id)
            .ok_or_else(|| BridgeError::UnknownSession(session_id.to_owned()))?;
        let mut tools = Vec::new();
        if session.config.capabilities.read_text_file {
            tools.push(AcpClientTool {
                name: "acp_client_read_text_file".to_owned(),
                description: "Read a UTF-8 file through the ACP client inside this session's admitted roots".to_owned(),
                input_schema: json!({"type":"object","required":["path"],"properties":{"path":{"type":"string"}},"additionalProperties":false}),
                kind: AcpClientToolKind::ReadTextFile,
                available: true,
            });
        }
        if session.config.capabilities.write_text_file {
            tools.push(AcpClientTool {
                name: "acp_client_write_text_file".to_owned(),
                description: "Write UTF-8 text through the ACP client inside this session's admitted roots".to_owned(),
                input_schema: json!({"type":"object","required":["path","content"],"properties":{"path":{"type":"string"},"content":{"type":"string"}},"additionalProperties":false}),
                kind: AcpClientToolKind::WriteTextFile,
                available: true,
            });
        }
        if session.config.capabilities.terminal {
            tools.push(AcpClientTool {
                name: "acp_client_terminal".to_owned(),
                description: "Run an explicitly admitted executable through the ACP client".to_owned(),
                input_schema: json!({"type":"object","required":["executable","args","cwd"],"properties":{"executable":{"type":"string"},"args":{"type":"array","items":{"type":"string"}},"cwd":{"type":"string"}},"additionalProperties":false}),
                kind: AcpClientToolKind::Terminal,
                available: true,
            });
        }
        for server in &session.config.mcp_servers {
            tools.push(AcpClientTool {
                name: format!("acp_mcp_{}", bounded_name(&server.id)),
                description: format!("Call the session-scoped ACP MCP server {}", server.id),
                input_schema: json!({"type":"object","required":["tool","arguments"],"properties":{"tool":{"type":"string"},"arguments":{"type":"object"}},"additionalProperties":false}),
                kind: AcpClientToolKind::Mcp {
                    server_id: server.id.clone(),
                },
                available: matches!(
                    session.mcp_health.get(&server.id),
                    Some(AcpMcpHealth::Healthy)
                ),
            });
        }
        Ok(tools)
    }

    /// Admits a client filesystem path without escaping the session roots.
    ///
    /// # Errors
    ///
    /// Returns an error when the capability was not negotiated or the path escapes its roots.
    pub fn admit_read_path(&self, session_id: &str, path: &Path) -> Result<PathBuf, BridgeError> {
        self.admit_path(session_id, path, false)
    }

    /// Admits an existing file or a new file whose canonical parent is inside the session roots.
    ///
    /// # Errors
    ///
    /// Returns an error when write authority is absent or the target escapes its roots.
    pub fn admit_write_path(&self, session_id: &str, path: &Path) -> Result<PathBuf, BridgeError> {
        self.admit_path(session_id, path, true)
    }

    /// Validates a terminal request against negotiated capability and the profile process ceiling.
    ///
    /// # Errors
    ///
    /// Returns an error for an unapproved executable, cwd, environment reference, or bound.
    pub fn admit_terminal(
        &self,
        session_id: &str,
        mut request: AcpTerminalRequest,
    ) -> Result<AcpTerminalRequest, BridgeError> {
        let sessions = self
            .sessions
            .lock()
            .map_err(|_| BridgeError::LockPoisoned)?;
        let session = sessions
            .get(session_id)
            .ok_or_else(|| BridgeError::UnknownSession(session_id.to_owned()))?;
        if !session.config.capabilities.terminal {
            return Err(BridgeError::ClientCapability(
                "ACP client terminal capability is not admitted".to_owned(),
            ));
        }
        request.executable = std::fs::canonicalize(&request.executable)?;
        request.cwd = std::fs::canonicalize(&request.cwd)?;
        if !self
            .policy
            .allowed_executables
            .contains(&request.executable)
            || !inside_roots(&request.cwd, &session.roots)
            || request.args.len() > MAX_ARGUMENTS
            || request.args.iter().map(String::len).sum::<usize>() > MAX_ARGUMENT_BYTES
            || request.environment.len() > MAX_ENVIRONMENT
            || request.output_byte_limit == 0
            || request.output_byte_limit > self.policy.max_terminal_output_bytes
        {
            return Err(BridgeError::ClientCapability(
                "ACP terminal request exceeds the profile process ceiling".to_owned(),
            ));
        }
        for (name, reference) in &request.environment {
            if !valid_environment_name(name) {
                return Err(BridgeError::ClientCapability(
                    "ACP terminal environment name is invalid".to_owned(),
                ));
            }
            validate_credential_reference(reference, &self.policy)?;
        }
        Ok(request)
    }

    /// Records the result of the required MCP health check.
    ///
    /// # Errors
    ///
    /// Returns an error for an unknown session/server or unavailable broker state.
    pub fn mark_mcp_health(
        &self,
        session_id: &str,
        server_id: &str,
        health: AcpMcpHealth,
    ) -> Result<(), BridgeError> {
        let mut sessions = self
            .sessions
            .lock()
            .map_err(|_| BridgeError::LockPoisoned)?;
        let session = sessions
            .get_mut(session_id)
            .ok_or_else(|| BridgeError::UnknownSession(session_id.to_owned()))?;
        let entry = session.mcp_health.get_mut(server_id).ok_or_else(|| {
            BridgeError::McpPolicy("unknown session-scoped MCP server".to_owned())
        })?;
        *entry = health;
        Ok(())
    }

    /// Admits a discovered MCP schema only after health and byte-bound checks.
    ///
    /// # Errors
    ///
    /// Returns an error when the server is unavailable or its schema exceeds the profile bound.
    pub fn admit_mcp_schema(
        &self,
        session_id: &str,
        server_id: &str,
        schema: &Value,
    ) -> Result<(), BridgeError> {
        let sessions = self
            .sessions
            .lock()
            .map_err(|_| BridgeError::LockPoisoned)?;
        let session = sessions
            .get(session_id)
            .ok_or_else(|| BridgeError::UnknownSession(session_id.to_owned()))?;
        if !matches!(
            session.mcp_health.get(server_id),
            Some(AcpMcpHealth::Healthy)
        ) || serde_json::to_vec(schema)?.len() > self.policy.max_mcp_schema_bytes
            || !schema.is_object()
        {
            return Err(BridgeError::McpPolicy(
                "MCP schema is unavailable, malformed, or exceeds its bound".to_owned(),
            ));
        }
        Ok(())
    }

    fn validate_config(
        &self,
        mut requested: AcpClientSessionConfig,
    ) -> Result<AcpClientSessionConfig, BridgeError> {
        requested.capabilities = requested.capabilities.intersect(self.policy.capabilities);
        if requested.mcp_servers.len() > self.policy.max_mcp_servers {
            return Err(BridgeError::McpPolicy(
                "too many session-scoped MCP servers".to_owned(),
            ));
        }
        let mut ids = BTreeSet::new();
        for server in &mut requested.mcp_servers {
            validate_name(&server.id)?;
            if !ids.insert(server.id.clone()) {
                return Err(BridgeError::McpPolicy(
                    "duplicate session-scoped MCP server id".to_owned(),
                ));
            }
            self.validate_mcp_transport(&mut server.transport)?;
        }
        Ok(requested)
    }

    fn validate_mcp_transport(&self, transport: &mut AcpMcpTransport) -> Result<(), BridgeError> {
        match transport {
            AcpMcpTransport::Stdio {
                executable,
                args,
                credentials,
            } => {
                if !self.policy.capabilities.terminal {
                    return Err(BridgeError::McpPolicy(
                        "stdio MCP is outside the profile process ceiling".to_owned(),
                    ));
                }
                *executable = std::fs::canonicalize(&*executable)?;
                if !self.policy.allowed_executables.contains(executable)
                    || args.len() > MAX_ARGUMENTS
                    || args.iter().map(String::len).sum::<usize>() > MAX_ARGUMENT_BYTES
                {
                    return Err(BridgeError::McpPolicy(
                        "stdio MCP executable or arguments are not admitted".to_owned(),
                    ));
                }
                validate_credentials(credentials, &self.policy)
            }
            AcpMcpTransport::Http {
                endpoint,
                credentials,
            }
            | AcpMcpTransport::Sse {
                endpoint,
                credentials,
            } => {
                let url = Url::parse(endpoint).map_err(|_| {
                    BridgeError::McpPolicy("MCP endpoint is not a valid URL".to_owned())
                })?;
                let host = url
                    .host_str()
                    .ok_or_else(|| BridgeError::McpPolicy("MCP endpoint has no host".to_owned()))?;
                if url.scheme() != "https"
                    || !url.username().is_empty()
                    || url.password().is_some()
                    || url.fragment().is_some()
                    || !self.policy.allowed_network_hosts.contains(host)
                {
                    return Err(BridgeError::McpPolicy(
                        "MCP endpoint is outside the profile network ceiling".to_owned(),
                    ));
                }
                validate_credentials(credentials, &self.policy)
            }
        }
    }

    fn admit_path(
        &self,
        session_id: &str,
        path: &Path,
        write: bool,
    ) -> Result<PathBuf, BridgeError> {
        let sessions = self
            .sessions
            .lock()
            .map_err(|_| BridgeError::LockPoisoned)?;
        let session = sessions
            .get(session_id)
            .ok_or_else(|| BridgeError::UnknownSession(session_id.to_owned()))?;
        let admitted = if write && !path.exists() {
            let name = path
                .file_name()
                .ok_or_else(|| BridgeError::WorkspaceBoundary(path.to_path_buf()))?;
            let parent = std::fs::canonicalize(
                path.parent()
                    .ok_or_else(|| BridgeError::WorkspaceBoundary(path.to_path_buf()))?,
            )?;
            parent.join(name)
        } else {
            std::fs::canonicalize(path)?
        };
        let capability = if write {
            session.config.capabilities.write_text_file
        } else {
            session.config.capabilities.read_text_file
        };
        if !capability || !inside_roots(&admitted, &session.roots) {
            return Err(BridgeError::WorkspaceBoundary(admitted));
        }
        Ok(admitted)
    }
}

fn validate_credentials(
    credentials: &[AcpCredentialBinding],
    policy: &AcpClientPolicy,
) -> Result<(), BridgeError> {
    if credentials.len() > MAX_ENVIRONMENT {
        return Err(BridgeError::McpPolicy(
            "too many MCP credential references".to_owned(),
        ));
    }
    let mut placements = BTreeSet::new();
    for credential in credentials {
        validate_credential_reference(&credential.reference, policy)?;
        let (valid, placement) = match &credential.placement {
            AcpCredentialPlacement::Header(name) => (
                valid_header_name(name),
                format!("header:{}", name.to_ascii_lowercase()),
            ),
            AcpCredentialPlacement::Environment(name) => {
                (valid_environment_name(name), format!("environment:{name}"))
            }
        };
        if !valid || !placements.insert(placement) {
            return Err(BridgeError::McpPolicy(
                "invalid or duplicate MCP credential placement".to_owned(),
            ));
        }
    }
    Ok(())
}

fn validate_credential_reference(
    reference: &str,
    policy: &AcpClientPolicy,
) -> Result<(), BridgeError> {
    if !valid_token(reference) || !policy.allowed_credential_references.contains(reference) {
        return Err(BridgeError::McpPolicy(
            "MCP credential reference is not admitted by the profile".to_owned(),
        ));
    }
    Ok(())
}

fn validate_name(name: &str) -> Result<(), BridgeError> {
    if !valid_token(name) || name.len() > MAX_NAME_BYTES {
        return Err(BridgeError::McpPolicy(
            "MCP server id must be a bounded ASCII token".to_owned(),
        ));
    }
    Ok(())
}

fn valid_token(value: &str) -> bool {
    !value.is_empty()
        && value
            .bytes()
            .all(|byte| byte.is_ascii_alphanumeric() || matches!(byte, b'_' | b'-' | b'.'))
}

fn valid_environment_name(value: &str) -> bool {
    let mut bytes = value.bytes();
    bytes
        .next()
        .is_some_and(|byte| byte.is_ascii_alphabetic() || byte == b'_')
        && bytes.all(|byte| byte.is_ascii_alphanumeric() || byte == b'_')
}

fn valid_header_name(value: &str) -> bool {
    !value.is_empty()
        && value.bytes().all(|byte| {
            byte.is_ascii_alphanumeric()
                || matches!(
                    byte,
                    b'!' | b'#'
                        | b'$'
                        | b'%'
                        | b'&'
                        | b'\''
                        | b'*'
                        | b'+'
                        | b'-'
                        | b'.'
                        | b'^'
                        | b'_'
                        | b'`'
                        | b'|'
                        | b'~'
                )
        })
}

fn inside_roots(path: &Path, roots: &[PathBuf]) -> bool {
    roots.iter().any(|root| path.starts_with(root))
}

fn bounded_name(value: &str) -> String {
    value
        .bytes()
        .map(|byte| {
            if byte.is_ascii_alphanumeric() || byte == b'_' {
                char::from(byte.to_ascii_lowercase())
            } else {
                '_'
            }
        })
        .take(48)
        .collect()
}

#[cfg(test)]
mod tests {
    use keith_agent_types::{ProfileId, SessionId, WorkspaceId};
    use tempfile::tempdir;

    use super::*;
    use crate::AcpSessionStatus;

    fn record(root: &Path) -> AcpSessionRecord {
        AcpSessionRecord {
            acp_session_id: "session".to_owned(),
            keith_session_id: SessionId::new(),
            profile_id: ProfileId::new(),
            workspace_id: WorkspaceId::new(),
            cwd: root.to_path_buf(),
            additional_directories: Vec::new(),
            status: AcpSessionStatus::Ready,
            cursor: None,
            next_prompt_ordinal: 0,
            in_flight_prompt: None,
            forked_from: None,
            client_context: None,
            protocol_version: None,
        }
    }

    #[test]
    fn negotiated_filesystem_never_escapes_profile_or_session_scope() {
        let root = tempdir().unwrap();
        let outside = tempdir().unwrap();
        let file = root.path().join("inside.txt");
        std::fs::write(&file, "inside").unwrap();
        let outside_file = outside.path().join("outside.txt");
        std::fs::write(&outside_file, "outside").unwrap();
        let broker = AcpClientToolBroker::new(AcpClientPolicy {
            capabilities: AcpClientCapabilities {
                read_text_file: true,
                write_text_file: false,
                terminal: false,
            },
            ..AcpClientPolicy::default()
        })
        .unwrap();
        broker
            .register_session(
                &record(root.path()),
                AcpClientSessionConfig {
                    capabilities: AcpClientCapabilities {
                        read_text_file: true,
                        write_text_file: true,
                        terminal: true,
                    },
                    mcp_servers: Vec::new(),
                },
            )
            .unwrap();
        assert_eq!(broker.admit_read_path("session", &file).unwrap(), file);
        assert!(matches!(
            broker.admit_read_path("session", &outside_file),
            Err(BridgeError::WorkspaceBoundary(_))
        ));
        assert!(broker.admit_write_path("session", &file).is_err());
        assert!(
            broker
                .admit_terminal(
                    "session",
                    AcpTerminalRequest {
                        executable: PathBuf::from("/bin/sh"),
                        args: Vec::new(),
                        cwd: root.path().to_path_buf(),
                        environment: BTreeMap::new(),
                        output_byte_limit: 1024,
                    }
                )
                .is_err()
        );
    }

    #[test]
    fn reconnecting_client_can_only_reduce_durable_authority() {
        let root = tempdir().unwrap();
        let broker = AcpClientToolBroker::new(AcpClientPolicy {
            capabilities: AcpClientCapabilities {
                read_text_file: true,
                write_text_file: true,
                terminal: false,
            },
            ..AcpClientPolicy::default()
        })
        .unwrap();
        let session = record(root.path());
        broker
            .register_session(
                &session,
                AcpClientSessionConfig {
                    capabilities: AcpClientCapabilities {
                        read_text_file: true,
                        write_text_file: false,
                        terminal: false,
                    },
                    mcp_servers: Vec::new(),
                },
            )
            .unwrap();
        let effective = broker
            .register_session(
                &session,
                AcpClientSessionConfig {
                    capabilities: AcpClientCapabilities {
                        read_text_file: true,
                        write_text_file: true,
                        terminal: false,
                    },
                    mcp_servers: Vec::new(),
                },
            )
            .unwrap();
        assert!(effective.capabilities.read_text_file);
        assert!(!effective.capabilities.write_text_file);
        assert!(!effective.capabilities.terminal);
    }

    #[test]
    fn mcp_requires_exact_policy_credential_refs_health_and_schema_bounds() {
        let root = tempdir().unwrap();
        let broker = AcpClientToolBroker::new(AcpClientPolicy {
            allowed_network_hosts: BTreeSet::from(["tools.example".to_owned()]),
            allowed_credential_references: BTreeSet::from(["mcp-token".to_owned()]),
            max_mcp_schema_bytes: 64,
            ..AcpClientPolicy::default()
        })
        .unwrap();
        let config = AcpClientSessionConfig {
            capabilities: AcpClientCapabilities::default(),
            mcp_servers: vec![AcpMcpServer {
                id: "docs".to_owned(),
                transport: AcpMcpTransport::Http {
                    endpoint: "https://tools.example/mcp".to_owned(),
                    credentials: vec![AcpCredentialBinding {
                        reference: "mcp-token".to_owned(),
                        placement: AcpCredentialPlacement::Header("Authorization".to_owned()),
                    }],
                },
            }],
        };
        broker
            .register_session(&record(root.path()), config)
            .unwrap();
        assert!(!broker.tools("session").unwrap()[0].available);
        assert!(
            broker
                .admit_mcp_schema("session", "docs", &json!({"type":"object"}))
                .is_err()
        );
        broker
            .mark_mcp_health("session", "docs", AcpMcpHealth::Healthy)
            .unwrap();
        broker
            .admit_mcp_schema("session", "docs", &json!({"type":"object"}))
            .unwrap();
        assert!(broker.tools("session").unwrap()[0].available);
    }
}
