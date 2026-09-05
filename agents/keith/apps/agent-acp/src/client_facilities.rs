use std::sync::Arc;

use agent_client_protocol::schema::v1::{
    ClientCapabilities, CreateTerminalRequest, EnvVariable, HttpHeader, McpServer,
    PermissionOption, PermissionOptionKind, ReadTextFileRequest, ReleaseTerminalRequest,
    RequestPermissionOutcome, RequestPermissionRequest, SessionId, TerminalOutputRequest,
    TerminalOutputResponse, ToolCallUpdate, ToolCallUpdateFields, ToolKind,
    WaitForTerminalExitRequest, WriteTextFileRequest,
};
use agent_client_protocol::{Client, ConnectionTo, Error};
use keith_acp::{
    AcpClientCapabilities, AcpClientSessionConfig, AcpClientToolBroker, AcpCredentialBinding,
    AcpCredentialPlacement, AcpMcpServer, AcpMcpTransport, AcpPermissionBridge,
    AcpPermissionChallenge, AcpPermissionDecision, AcpPermissionOptionKind, AcpPermissionRequest,
    AcpTerminalRequest, BridgeError,
};

const MAX_CLIENT_FILE_BYTES: usize = 2 * 1_024 * 1_024;

pub(crate) fn session_config(
    capabilities: &ClientCapabilities,
    servers: Vec<McpServer>,
) -> Result<AcpClientSessionConfig, BridgeError> {
    let mcp_servers = servers
        .into_iter()
        .map(convert_mcp_server)
        .collect::<Result<Vec<_>, _>>()?;
    Ok(AcpClientSessionConfig {
        capabilities: AcpClientCapabilities {
            read_text_file: capabilities.fs.read_text_file,
            write_text_file: capabilities.fs.write_text_file,
            terminal: capabilities.terminal,
        },
        mcp_servers,
    })
}

pub struct AcpClientSdkTools {
    broker: Arc<AcpClientToolBroker>,
}

impl AcpClientSdkTools {
    /// Creates the ACP client-facility adapter for one policy-enforcing broker.
    pub fn new(broker: Arc<AcpClientToolBroker>) -> Self {
        Self { broker }
    }

    /// Reads a bounded UTF-8 file through the connected ACP client after root admission.
    ///
    /// # Errors
    ///
    /// Returns an ACP error for policy denial, connection failure, client refusal, or oversized
    /// output.
    pub async fn read_text_file(
        &self,
        connection: &ConnectionTo<Client>,
        session_id: &str,
        path: &std::path::Path,
        line: Option<u32>,
        limit: Option<u32>,
    ) -> Result<String, Error> {
        let path = self
            .broker
            .admit_read_path(session_id, path)
            .map_err(|error| client_tool_error(&error))?;
        let response = connection
            .send_request(
                ReadTextFileRequest::new(SessionId::new(session_id), path)
                    .line(line)
                    .limit(limit),
            )
            .block_task()
            .await?;
        if response.content.len() > MAX_CLIENT_FILE_BYTES {
            return Err(Error::invalid_params().data("ACP client file response exceeded its bound"));
        }
        Ok(response.content)
    }

    /// Writes bounded UTF-8 text through the connected ACP client after root admission.
    ///
    /// # Errors
    ///
    /// Returns an ACP error for policy denial, connection failure, client refusal, or oversized
    /// input.
    pub async fn write_text_file(
        &self,
        connection: &ConnectionTo<Client>,
        session_id: &str,
        path: &std::path::Path,
        content: String,
    ) -> Result<(), Error> {
        if content.len() > MAX_CLIENT_FILE_BYTES {
            return Err(Error::invalid_params().data("ACP client file request exceeded its bound"));
        }
        let path = self
            .broker
            .admit_write_path(session_id, path)
            .map_err(|error| client_tool_error(&error))?;
        connection
            .send_request(WriteTextFileRequest::new(
                SessionId::new(session_id),
                path,
                content,
            ))
            .block_task()
            .await?;
        Ok(())
    }

    /// Runs an explicitly admitted executable through the connected ACP client and releases it.
    ///
    /// # Errors
    ///
    /// Returns an ACP error for policy denial, connection failure, client refusal, or oversized
    /// terminal output.
    pub async fn terminal(
        &self,
        connection: &ConnectionTo<Client>,
        session_id: &str,
        request: AcpTerminalRequest,
    ) -> Result<TerminalOutputResponse, Error> {
        let request = self
            .broker
            .admit_terminal(session_id, request)
            .map_err(|error| client_tool_error(&error))?;
        let environment = request
            .environment
            .into_iter()
            .map(|(name, reference)| EnvVariable::new(name, format!("credential://{reference}")))
            .collect();
        let created = connection
            .send_request(
                CreateTerminalRequest::new(
                    SessionId::new(session_id),
                    request.executable.to_string_lossy().into_owned(),
                )
                .args(request.args)
                .env(environment)
                .cwd(request.cwd)
                .output_byte_limit(request.output_byte_limit),
            )
            .block_task()
            .await?;
        let terminal_id = created.terminal_id;
        let waited = connection
            .send_request(WaitForTerminalExitRequest::new(
                SessionId::new(session_id),
                terminal_id.clone(),
            ))
            .block_task()
            .await;
        let output = if waited.is_ok() {
            connection
                .send_request(TerminalOutputRequest::new(
                    SessionId::new(session_id),
                    terminal_id.clone(),
                ))
                .block_task()
                .await
        } else {
            Err(waited.expect_err("branch checked"))
        };
        let release = connection
            .send_request(ReleaseTerminalRequest::new(
                SessionId::new(session_id),
                terminal_id,
            ))
            .block_task()
            .await;
        let output = output?;
        release?;
        if output.output.len() > usize::try_from(request.output_byte_limit).unwrap_or(usize::MAX) {
            return Err(Error::invalid_params().data("ACP terminal output exceeded its bound"));
        }
        Ok(output)
    }

    /// Presents a prevalidated Keith permission challenge through ACP and reduces the response.
    ///
    /// # Errors
    ///
    /// Returns an ACP error for connection failure, an unknown client choice, or any response
    /// that no longer matches the exact action and policy-approved options.
    pub async fn request_permission(
        &self,
        permission: &AcpPermissionBridge,
        connection: &ConnectionTo<Client>,
        request: &AcpPermissionRequest,
        challenge: &AcpPermissionChallenge,
    ) -> Result<AcpPermissionDecision, Error> {
        let options = challenge
            .options
            .iter()
            .map(|option| {
                PermissionOption::new(
                    option.id.clone(),
                    option.label.clone(),
                    match option.kind {
                        AcpPermissionOptionKind::AllowOnce => PermissionOptionKind::AllowOnce,
                        AcpPermissionOptionKind::AllowForSession => {
                            PermissionOptionKind::AllowAlways
                        }
                        AcpPermissionOptionKind::RejectOnce => PermissionOptionKind::RejectOnce,
                        AcpPermissionOptionKind::RejectForSession => {
                            PermissionOptionKind::RejectAlways
                        }
                    },
                )
            })
            .collect();
        let tool_call = ToolCallUpdate::new(
            request.tool_call_id.clone(),
            ToolCallUpdateFields::new()
                .title(request.title.clone())
                .kind(ToolKind::Other),
        );
        let response = connection
            .send_request(RequestPermissionRequest::new(
                SessionId::new(request.action.session_id.to_string()),
                tool_call,
                options,
            ))
            .block_task()
            .await?;
        let selected = match response.outcome {
            RequestPermissionOutcome::Cancelled => None,
            RequestPermissionOutcome::Selected(selected) => Some(selected.option_id.0.to_string()),
            _ => return Err(Error::invalid_params().data("unknown ACP permission outcome")),
        };
        permission
            .complete(request, challenge, selected.as_deref())
            .map_err(|error| client_tool_error(&error))
    }
}

fn convert_mcp_server(server: McpServer) -> Result<AcpMcpServer, BridgeError> {
    match server {
        McpServer::Stdio(server) => Ok(AcpMcpServer {
            id: server.name,
            transport: AcpMcpTransport::Stdio {
                executable: server.command,
                args: server.args,
                credentials: server
                    .env
                    .into_iter()
                    .map(|variable| {
                        credential_binding(
                            &variable.value,
                            AcpCredentialPlacement::Environment(variable.name),
                        )
                    })
                    .collect::<Result<Vec<_>, _>>()?,
            },
        }),
        McpServer::Http(server) => Ok(AcpMcpServer {
            id: server.name,
            transport: AcpMcpTransport::Http {
                endpoint: server.url,
                credentials: convert_headers(server.headers)?,
            },
        }),
        McpServer::Sse(server) => Ok(AcpMcpServer {
            id: server.name,
            transport: AcpMcpTransport::Sse {
                endpoint: server.url,
                credentials: convert_headers(server.headers)?,
            },
        }),
        _ => Err(BridgeError::McpPolicy(
            "unsupported ACP MCP transport; no schema conversion is performed".to_owned(),
        )),
    }
}

fn convert_headers(headers: Vec<HttpHeader>) -> Result<Vec<AcpCredentialBinding>, BridgeError> {
    headers
        .into_iter()
        .map(|header| {
            credential_binding(&header.value, AcpCredentialPlacement::Header(header.name))
        })
        .collect()
}

fn credential_binding(
    value: &str,
    placement: AcpCredentialPlacement,
) -> Result<AcpCredentialBinding, BridgeError> {
    let reference = value.strip_prefix("credential://").ok_or_else(|| {
        BridgeError::McpPolicy(
            "raw MCP credential values are forbidden; use credential://reference".to_owned(),
        )
    })?;
    Ok(AcpCredentialBinding {
        reference: reference.to_owned(),
        placement,
    })
}

fn client_tool_error(error: &BridgeError) -> Error {
    Error::invalid_params().data(error.to_string())
}

#[cfg(test)]
mod tests {
    use std::collections::{BTreeMap, BTreeSet};
    use std::sync::Mutex;

    use agent_client_protocol::Agent;
    use agent_client_protocol::schema::v1::{
        CreateTerminalResponse, FileSystemCapabilities, McpServerHttp, ReadTextFileResponse,
        ReleaseTerminalResponse, RequestPermissionResponse, SelectedPermissionOutcome,
        TerminalExitStatus, TerminalOutputResponse, WaitForTerminalExitResponse,
        WriteTextFileResponse,
    };
    use keith_acp::{AcpSessionRecord, AcpSessionStatus};
    use keith_agent_types::{ProfileId, SessionId as KeithSessionId, UtcTimestamp, WorkspaceId};
    use keith_platform_contracts::{
        ActionRisk, ApprovalEnvelope, ApprovalState, AuditCorrelationId, AuthorityBoundary,
        CancellationId, Capability, CapabilityGrant, ExternalAction, ExternalEffect,
        ExternalPrincipalId, RedactedText,
    };
    use tempfile::tempdir;

    use super::*;

    #[test]
    fn client_capabilities_and_credential_references_map_without_raw_secrets() {
        let capabilities = ClientCapabilities::new()
            .fs(FileSystemCapabilities::new()
                .read_text_file(true)
                .write_text_file(true))
            .terminal(true);
        let config = session_config(
            &capabilities,
            vec![McpServer::Http(
                McpServerHttp::new("docs", "https://tools.example/mcp").headers(vec![
                    HttpHeader::new("Authorization", "credential://docs-token"),
                ]),
            )],
        )
        .unwrap();
        assert_eq!(
            config.capabilities,
            AcpClientCapabilities {
                read_text_file: true,
                write_text_file: true,
                terminal: true,
            }
        );
        assert_eq!(config.mcp_servers.len(), 1);
        let serialized = serde_json::to_string(&config).unwrap();
        assert!(!serialized.contains("Bearer "));
        assert!(serialized.contains("docs-token"));
    }

    #[test]
    fn raw_mcp_credentials_are_refused_instead_of_persisted() {
        let error = session_config(
            &ClientCapabilities::new(),
            vec![McpServer::Http(
                McpServerHttp::new("docs", "https://tools.example/mcp")
                    .headers(vec![HttpHeader::new("Authorization", "secret-value")]),
            )],
        )
        .unwrap_err();
        assert!(matches!(error, BridgeError::McpPolicy(_)));
    }

    #[tokio::test]
    #[allow(clippy::too_many_lines)]
    async fn real_sdk_client_serves_files_terminal_and_permission_requests() {
        let root = tempdir().unwrap();
        let read_path = root.path().join("read.txt");
        let write_path = root.path().join("write.txt");
        std::fs::write(&read_path, "client supplied text").unwrap();
        let executable = std::fs::canonicalize("/bin/echo").unwrap();
        let profile_id = ProfileId::new();
        let record = AcpSessionRecord {
            acp_session_id: "sdk-session".to_owned(),
            keith_session_id: KeithSessionId::new(),
            profile_id: profile_id.clone(),
            workspace_id: WorkspaceId::new(),
            cwd: root.path().to_path_buf(),
            additional_directories: Vec::new(),
            status: AcpSessionStatus::Ready,
            cursor: None,
            next_prompt_ordinal: 0,
            in_flight_prompt: None,
            forked_from: None,
            client_context: None,
            protocol_version: Some(1),
        };
        let broker = Arc::new(
            AcpClientToolBroker::new(keith_acp::AcpClientPolicy {
                capabilities: AcpClientCapabilities {
                    read_text_file: true,
                    write_text_file: true,
                    terminal: true,
                },
                allowed_executables: BTreeSet::from([executable.clone()]),
                ..keith_acp::AcpClientPolicy::default()
            })
            .unwrap(),
        );
        broker
            .register_session(
                &record,
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
        let tools = AcpClientSdkTools::new(Arc::clone(&broker));
        let terminal_result = Arc::new(Mutex::new(None::<(String, u32)>));

        let client = Client
            .builder()
            .on_receive_request(
                async |request: ReadTextFileRequest, responder, _connection| {
                    let content = std::fs::read_to_string(request.path)
                        .map_err(Error::into_internal_error)?;
                    responder.respond(ReadTextFileResponse::new(content))
                },
                agent_client_protocol::on_receive_request!(),
            )
            .on_receive_request(
                async |request: WriteTextFileRequest, responder, _connection| {
                    std::fs::write(request.path, request.content)
                        .map_err(Error::into_internal_error)?;
                    responder.respond(WriteTextFileResponse::new())
                },
                agent_client_protocol::on_receive_request!(),
            )
            .on_receive_request(
                {
                    let terminal_result = Arc::clone(&terminal_result);
                    async move |request: CreateTerminalRequest, responder, _connection| {
                        let mut command = std::process::Command::new(request.command);
                        command.args(request.args);
                        if let Some(cwd) = request.cwd {
                            command.current_dir(cwd);
                        }
                        let output = command.output().map_err(Error::into_internal_error)?;
                        let exit_code = u32::try_from(output.status.code().unwrap_or(1))
                            .map_err(Error::into_internal_error)?;
                        *terminal_result
                            .lock()
                            .map_err(|_| Error::internal_error())? = Some((
                            String::from_utf8(output.stdout).map_err(Error::into_internal_error)?,
                            exit_code,
                        ));
                        responder.respond(CreateTerminalResponse::new("terminal-1"))
                    }
                },
                agent_client_protocol::on_receive_request!(),
            )
            .on_receive_request(
                {
                    let terminal_result = Arc::clone(&terminal_result);
                    async move |_request: WaitForTerminalExitRequest, responder, _connection| {
                        let exit_code = terminal_result
                            .lock()
                            .map_err(|_| Error::internal_error())?
                            .as_ref()
                            .map_or(1, |(_, status)| *status);
                        responder.respond(WaitForTerminalExitResponse::new(
                            TerminalExitStatus::new().exit_code(exit_code),
                        ))
                    }
                },
                agent_client_protocol::on_receive_request!(),
            )
            .on_receive_request(
                {
                    let terminal_result = Arc::clone(&terminal_result);
                    async move |_request: TerminalOutputRequest, responder, _connection| {
                        let (output, exit_code) = terminal_result
                            .lock()
                            .map_err(|_| Error::internal_error())?
                            .clone()
                            .ok_or_else(Error::internal_error)?;
                        responder.respond(
                            TerminalOutputResponse::new(output, false)
                                .exit_status(TerminalExitStatus::new().exit_code(exit_code)),
                        )
                    }
                },
                agent_client_protocol::on_receive_request!(),
            )
            .on_receive_request(
                {
                    let terminal_result = Arc::clone(&terminal_result);
                    async move |_request: ReleaseTerminalRequest, responder, _connection| {
                        terminal_result
                            .lock()
                            .map_err(|_| Error::internal_error())?
                            .take();
                        responder.respond(ReleaseTerminalResponse::new())
                    }
                },
                agent_client_protocol::on_receive_request!(),
            )
            .on_receive_request(
                async |request: RequestPermissionRequest, responder, _connection| {
                    assert!(
                        request
                            .options
                            .iter()
                            .any(|option| option.option_id.0.as_ref() == "allow_once")
                    );
                    responder.respond(RequestPermissionResponse::new(
                        RequestPermissionOutcome::Selected(SelectedPermissionOutcome::new(
                            "allow_once",
                        )),
                    ))
                },
                agent_client_protocol::on_receive_request!(),
            );

        let target = RedactedText::parse("workspace:file").unwrap();
        let permission = AcpPermissionBridge::new(
            AuthorityBoundary {
                profile_id: profile_id.clone(),
                allowed: BTreeSet::from([CapabilityGrant {
                    capability: Capability::LocalWrite,
                    resource: target.clone(),
                    expires_at: None,
                }]),
                denied: BTreeSet::new(),
                max_automatic_risk: ActionRisk::ReversibleLocalWrite,
            },
            BTreeSet::new(),
        );
        let permission_request = AcpPermissionRequest {
            tool_call_id: "tool-1".to_owned(),
            title: "Write file".to_owned(),
            action: ExternalAction {
                profile_id,
                session_id: record.keith_session_id.clone(),
                acting_principal: ExternalPrincipalId::new(),
                requested_capability: Capability::LocalWrite,
                risk: ActionRisk::ReversibleLocalWrite,
                approval: ApprovalEnvelope {
                    risk: ActionRisk::ReversibleLocalWrite,
                    state: ApprovalState::Required,
                },
                target,
                target_digest: RedactedText::parse("sha256:target").unwrap(),
                cancellation_id: CancellationId::new(),
                reply_route: None,
                audit_correlation: AuditCorrelationId::new(),
                external_effect: ExternalEffect::Repeatable,
            },
        };
        let challenge = permission
            .begin(&permission_request, UtcTimestamp::from_unix_millis(1))
            .unwrap();
        let written_path = write_path.clone();
        let terminal_cwd = root.path().to_path_buf();
        Agent
            .builder()
            .connect_with(client, async move |connection| {
                assert_eq!(
                    tools
                        .read_text_file(&connection, "sdk-session", &read_path, None, None)
                        .await?,
                    "client supplied text"
                );
                tools
                    .write_text_file(
                        &connection,
                        "sdk-session",
                        &write_path,
                        "written through ACP".to_owned(),
                    )
                    .await?;
                let terminal = tools
                    .terminal(
                        &connection,
                        "sdk-session",
                        AcpTerminalRequest {
                            executable,
                            args: vec!["hello from ACP".to_owned()],
                            cwd: terminal_cwd,
                            environment: BTreeMap::new(),
                            output_byte_limit: 1_024,
                        },
                    )
                    .await?;
                assert_eq!(terminal.output, "hello from ACP\n");
                assert_eq!(
                    tools
                        .request_permission(
                            &permission,
                            &connection,
                            &permission_request,
                            &challenge,
                        )
                        .await?,
                    AcpPermissionDecision::AllowOnce
                );
                Ok(())
            })
            .await
            .unwrap();
        assert_eq!(
            std::fs::read_to_string(written_path).unwrap(),
            "written through ACP"
        );
    }
}
