use std::collections::BTreeSet;
use std::sync::Arc;

use agent_client_protocol::schema::ProtocolVersion;
use agent_client_protocol::schema::v2::{
    AgentCapabilities, AudioContent, BlobResourceContents, CancelSessionNotification,
    CloseSessionRequest, CloseSessionResponse, Content, ContentBlock, ContentChunk,
    EmbeddedResourceResource, ForkSessionRequest, ForkSessionResponse, Implementation,
    InitializeRequest, InitializeResponse, ListSessionsRequest, ListSessionsResponse,
    McpCapabilities, McpHttpCapabilities, McpServer, McpStdioCapabilities, NewSessionRequest,
    NewSessionResponse, PlanEntry, PlanEntryPriority, PlanEntryStatus, PlanUpdate,
    PlanUpdateContent, PromptAudioCapabilities, PromptCapabilities,
    PromptEmbeddedContextCapabilities, PromptImageCapabilities, PromptRequest, PromptResponse,
    ResumeSessionRequest, ResumeSessionResponse, RunningStateUpdate,
    SessionAdditionalDirectoriesCapabilities, SessionCapabilities, SessionForkCapabilities,
    SessionId, SessionInfo, SessionListCursor, SessionUpdate, StateUpdate, StopReason, TextContent,
    ToolCallContent, ToolCallStatus, ToolCallUpdate, ToolKind, UpdateSessionNotification,
};
use agent_client_protocol::{Agent, Client, ConnectTo, ConnectionTo, Error};
use base64::Engine as _;
use keith_acp::{
    AcpBinaryContent, AcpClientCapabilities, AcpClientSessionConfig, AcpContentBlock,
    AcpCredentialBinding, AcpCredentialPlacement, AcpMcpServer, AcpMcpTransport,
    AcpProtocolVersion, AcpUpdate, AcpUpdateKind, BridgeError,
};
use keith_protocol::TurnTerminalStatus;
use serde_json::{Map, Value, json};

use crate::{
    KeithAcpAgent, POLL_INTERVAL, bind_client_session, blocking, bridge_error,
    ensure_session_version, event_meta,
};

const SESSION_PAGE_SIZE: usize = 100;

#[allow(clippy::too_many_lines)]
pub(crate) async fn serve(
    agent: KeithAcpAgent,
    transport: impl ConnectTo<Agent>,
) -> Result<(), Error> {
    let bridge = Arc::clone(&agent.runtime.bridge);
    let client_tools = Arc::clone(&agent.runtime.client_tools);
    let policy = client_tools.policy().clone();
    let connection_id = agent.connection_id;
    let transport_kind = agent.transport;

    Agent
        .v2()
        .name("keith-agent-acp-v2")
        .on_receive_request(
            async move |request: InitializeRequest, responder, _connection| {
                if request.protocol_version != ProtocolVersion::V2 {
                    return responder.respond_with_error(Error::invalid_params().data(json!({
                        "reason": "unsupported_protocol_version",
                        "requested": request.protocol_version,
                        "supported": ProtocolVersion::V2,
                    })));
                }
                responder.respond(
                    InitializeResponse::new(
                        ProtocolVersion::V2,
                        Implementation::new("keith", env!("CARGO_PKG_VERSION")),
                    )
                    .capabilities(agent_capabilities(&policy))
                    .meta(connection_meta(&connection_id, transport_kind)),
                )
            },
            agent_client_protocol::on_receive_request!(),
        )
        .on_receive_request(
            {
                let bridge = Arc::clone(&bridge);
                async move |request: ListSessionsRequest, responder, _connection| {
                    let bridge = Arc::clone(&bridge);
                    let result = blocking(move || {
                        let cwd = request
                            .cwd
                            .map(|path| std::fs::canonicalize(path.0))
                            .transpose()?;
                        let offset = request
                            .cursor
                            .as_ref()
                            .map(|cursor| cursor.0.parse::<usize>())
                            .transpose()
                            .map_err(|_| {
                                BridgeError::ProtocolVersion(
                                    "ACP v2 session-list cursor is invalid".to_owned(),
                                )
                            })?
                            .unwrap_or(0);
                        let mut records = bridge
                            .sessions()?
                            .into_iter()
                            .filter(|record| record.protocol_version == Some(2))
                            .filter(|record| cwd.as_ref().is_none_or(|cwd| &record.cwd == cwd))
                            .collect::<Vec<_>>();
                        records
                            .sort_by(|left, right| left.acp_session_id.cmp(&right.acp_session_id));
                        if offset > records.len() {
                            return Err(BridgeError::ProtocolVersion(
                                "ACP v2 session-list cursor is past the result set".to_owned(),
                            ));
                        }
                        let end = offset.saturating_add(SESSION_PAGE_SIZE).min(records.len());
                        let sessions = records[offset..end]
                            .iter()
                            .map(|record| {
                                SessionInfo::new(record.acp_session_id.clone(), record.cwd.clone())
                                    .additional_directories(record.additional_directories.clone())
                            })
                            .collect::<Vec<_>>();
                        let next_cursor = (end < records.len()).then(|| end.to_string());
                        Ok((sessions, next_cursor))
                    })
                    .await;
                    match result {
                        Ok((sessions, next_cursor)) => responder.respond(
                            ListSessionsResponse::new(sessions)
                                .next_cursor(next_cursor.map(SessionListCursor::new)),
                        ),
                        Err(error) => responder.respond_with_error(error),
                    }
                }
            },
            agent_client_protocol::on_receive_request!(),
        )
        .on_receive_request(
            {
                let bridge = Arc::clone(&bridge);
                let client_tools = Arc::clone(&client_tools);
                async move |request: NewSessionRequest, responder, connection| {
                    let context = match session_config(request.mcp_servers)
                        .and_then(|context| client_tools.validate_session_config(context))
                    {
                        Ok(context) => context,
                        Err(error) => return responder.respond_with_error(bridge_error(error)),
                    };
                    let bridge = Arc::clone(&bridge);
                    let client_tools = Arc::clone(&client_tools);
                    let task_connection = connection.clone();
                    connection.spawn(async move {
                        let cwd = request.cwd.0;
                        let additional_directories = request
                            .additional_directories
                            .into_iter()
                            .map(|path| path.0)
                            .collect::<Vec<_>>();
                        let result = blocking(move || {
                            let (record, updates) =
                                bridge.create_session(&cwd, &additional_directories)?;
                            let record = bind_client_session(
                                &bridge,
                                &client_tools,
                                &record,
                                context,
                                AcpProtocolVersion::DraftV2,
                            )?;
                            Ok((record, updates))
                        })
                        .await;
                        match result {
                            Ok((record, updates)) => {
                                send_updates(&task_connection, &record.acp_session_id, updates)?;
                                responder.respond(NewSessionResponse::new(record.acp_session_id))
                            }
                            Err(error) => responder.respond_with_error(error),
                        }
                    })?;
                    Ok(())
                }
            },
            agent_client_protocol::on_receive_request!(),
        )
        .on_receive_request(
            {
                let bridge = Arc::clone(&bridge);
                let client_tools = Arc::clone(&client_tools);
                async move |request: ResumeSessionRequest, responder, connection| {
                    let context = match session_config(request.mcp_servers)
                        .and_then(|context| client_tools.validate_session_config(context))
                    {
                        Ok(context) => context,
                        Err(error) => return responder.respond_with_error(bridge_error(error)),
                    };
                    let bridge = Arc::clone(&bridge);
                    let client_tools = Arc::clone(&client_tools);
                    let task_connection = connection.clone();
                    connection.spawn(async move {
                        let session_id = request.session_id.to_string();
                        let cwd = request.cwd.0;
                        let additional_directories = request
                            .additional_directories
                            .into_iter()
                            .map(|path| path.0)
                            .collect::<Vec<_>>();
                        let replay = request.replay_from.is_some();
                        let result = blocking(move || {
                            let (record, _) =
                                bridge.load_session(&session_id, &cwd, &additional_directories)?;
                            let record = bind_client_session(
                                &bridge,
                                &client_tools,
                                &record,
                                context,
                                AcpProtocolVersion::DraftV2,
                            )?;
                            let outcome = bridge.resume_session(&session_id)?;
                            Ok((record, outcome))
                        })
                        .await;
                        match result {
                            Ok((record, outcome)) => {
                                if replay {
                                    send_updates(
                                        &task_connection,
                                        &record.acp_session_id,
                                        outcome.updates,
                                    )?;
                                }
                                responder.respond(ResumeSessionResponse::new())
                            }
                            Err(error) => responder.respond_with_error(error),
                        }
                    })?;
                    Ok(())
                }
            },
            agent_client_protocol::on_receive_request!(),
        )
        .on_receive_request(
            {
                let bridge = Arc::clone(&bridge);
                let client_tools = Arc::clone(&client_tools);
                async move |request: ForkSessionRequest, responder, connection| {
                    let context = match session_config(request.mcp_servers)
                        .and_then(|context| client_tools.validate_session_config(context))
                    {
                        Ok(context) => context,
                        Err(error) => return responder.respond_with_error(bridge_error(error)),
                    };
                    let bridge = Arc::clone(&bridge);
                    let client_tools = Arc::clone(&client_tools);
                    let task_connection = connection.clone();
                    connection.spawn(async move {
                        let source_session_id = request.session_id.to_string();
                        let cwd = request.cwd.0;
                        let additional_directories = request
                            .additional_directories
                            .into_iter()
                            .map(|path| path.0)
                            .collect::<Vec<_>>();
                        let result = blocking(move || {
                            let source = bridge.session(&source_session_id)?;
                            let source_context = source.client_context.clone().unwrap_or_default();
                            bind_client_session(
                                &bridge,
                                &client_tools,
                                &source,
                                source_context,
                                AcpProtocolVersion::DraftV2,
                            )?;
                            let (record, updates) = bridge.fork_session(
                                &source_session_id,
                                &cwd,
                                &additional_directories,
                            )?;
                            let record = bind_client_session(
                                &bridge,
                                &client_tools,
                                &record,
                                context,
                                AcpProtocolVersion::DraftV2,
                            )?;
                            Ok((record, updates))
                        })
                        .await;
                        match result {
                            Ok((record, updates)) => {
                                send_updates(&task_connection, &record.acp_session_id, updates)?;
                                responder.respond(ForkSessionResponse::new(record.acp_session_id))
                            }
                            Err(error) => responder.respond_with_error(error),
                        }
                    })?;
                    Ok(())
                }
            },
            agent_client_protocol::on_receive_request!(),
        )
        .on_receive_request(
            {
                let bridge = Arc::clone(&bridge);
                async move |request: CloseSessionRequest, responder, connection| {
                    let bridge = Arc::clone(&bridge);
                    connection.spawn(async move {
                        let session_id = request.session_id.to_string();
                        match blocking(move || {
                            let record = bridge.session(&session_id)?;
                            ensure_session_version(&record, AcpProtocolVersion::DraftV2)?;
                            bridge.close_session(&session_id)
                        })
                        .await
                        {
                            Ok(()) => responder.respond(CloseSessionResponse::new()),
                            Err(error) => responder.respond_with_error(error),
                        }
                    })?;
                    Ok(())
                }
            },
            agent_client_protocol::on_receive_request!(),
        )
        .on_receive_request(
            {
                let bridge = Arc::clone(&bridge);
                async move |request: PromptRequest, responder, connection| {
                    let content = match convert_content(request.prompt) {
                        Ok(content) => content,
                        Err(error) => return responder.respond_with_error(error),
                    };
                    let session_id = request.session_id.to_string();
                    let check_bridge = Arc::clone(&bridge);
                    let check_session_id = session_id.clone();
                    if let Err(error) = blocking(move || {
                        let record = check_bridge.session(&check_session_id)?;
                        ensure_session_version(&record, AcpProtocolVersion::DraftV2)
                    })
                    .await
                    {
                        return responder.respond_with_error(error);
                    }
                    let bridge = Arc::clone(&bridge);
                    let task_connection = connection.clone();
                    let failure_connection = connection.clone();
                    let failure_session_id = session_id.clone();
                    connection.spawn(async move {
                        let result =
                            drive_prompt(bridge, session_id, content, task_connection).await;
                        if result.is_err() {
                            let _ = send_state(
                                &failure_connection,
                                &failure_session_id,
                                StateUpdate::Idle(
                                    agent_client_protocol::schema::v2::IdleStateUpdate::new()
                                        .stop_reason(StopReason::Refusal),
                                ),
                            );
                        }
                        result
                    })?;
                    responder.respond(PromptResponse::new())
                }
            },
            agent_client_protocol::on_receive_request!(),
        )
        .on_receive_notification(
            {
                let bridge = Arc::clone(&bridge);
                async move |notification: CancelSessionNotification, connection| {
                    let bridge = Arc::clone(&bridge);
                    connection.spawn(async move {
                        let session_id = notification.session_id.to_string();
                        let _ = blocking(move || {
                            let record = bridge.session(&session_id)?;
                            ensure_session_version(&record, AcpProtocolVersion::DraftV2)?;
                            bridge.cancel_session(&session_id)
                        })
                        .await;
                        Ok(())
                    })?;
                    Ok(())
                }
            },
            agent_client_protocol::on_receive_notification!(),
        )
        .connect_to(transport)
        .await
}

fn agent_capabilities(policy: &keith_acp::AcpClientPolicy) -> AgentCapabilities {
    let mut mcp = McpCapabilities::new();
    if policy.capabilities.terminal && !policy.allowed_executables.is_empty() {
        mcp = mcp.stdio(McpStdioCapabilities::new());
    }
    if !policy.allowed_network_hosts.is_empty() {
        mcp = mcp.http(McpHttpCapabilities::new());
    }
    AgentCapabilities::new().session(
        SessionCapabilities::new()
            .prompt(
                PromptCapabilities::new()
                    .image(PromptImageCapabilities::new())
                    .audio(PromptAudioCapabilities::new())
                    .embedded_context(PromptEmbeddedContextCapabilities::new()),
            )
            .mcp(mcp)
            .additional_directories(SessionAdditionalDirectoriesCapabilities::new())
            .fork(SessionForkCapabilities::new()),
    )
}

fn session_config(servers: Vec<McpServer>) -> Result<AcpClientSessionConfig, BridgeError> {
    Ok(AcpClientSessionConfig {
        capabilities: AcpClientCapabilities::default(),
        mcp_servers: servers
            .into_iter()
            .map(convert_mcp_server)
            .collect::<Result<Vec<_>, _>>()?,
    })
}

fn convert_mcp_server(server: McpServer) -> Result<AcpMcpServer, BridgeError> {
    match server {
        McpServer::Stdio(server) => Ok(AcpMcpServer {
            id: server.name,
            transport: AcpMcpTransport::Stdio {
                executable: server.command.0,
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
                credentials: server
                    .headers
                    .into_iter()
                    .map(|header| {
                        credential_binding(
                            &header.value,
                            AcpCredentialPlacement::Header(header.name),
                        )
                    })
                    .collect::<Result<Vec<_>, _>>()?,
            },
        }),
        _ => Err(BridgeError::McpPolicy(
            "unsupported ACP v2 MCP transport; no schema conversion is performed".to_owned(),
        )),
    }
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

async fn drive_prompt(
    bridge: Arc<keith_acp::AcpSessionBridge>,
    session_id: String,
    content: Vec<AcpContentBlock>,
    connection: ConnectionTo<Client>,
) -> Result<(), Error> {
    send_state(
        &connection,
        &session_id,
        StateUpdate::Running(RunningStateUpdate::new()),
    )?;
    let prompt_bridge = Arc::clone(&bridge);
    let prompt_session_id = session_id.clone();
    let mut outcome = blocking(move || {
        let record = prompt_bridge.session(&prompt_session_id)?;
        ensure_session_version(&record, AcpProtocolVersion::DraftV2)?;
        prompt_bridge.prompt(&prompt_session_id, &content)
    })
    .await?;
    let mut seen = BTreeSet::new();
    send_unseen_updates(&connection, &session_id, &mut seen, outcome.updates)?;
    loop {
        if let Some(status) = outcome.terminal {
            return send_state(
                &connection,
                &session_id,
                StateUpdate::Idle(
                    agent_client_protocol::schema::v2::IdleStateUpdate::new()
                        .stop_reason(stop_reason(status)),
                ),
            );
        }
        tokio::time::sleep(POLL_INTERVAL).await;
        let resume_bridge = Arc::clone(&bridge);
        let resume_session_id = session_id.clone();
        outcome = blocking(move || resume_bridge.resume_session(&resume_session_id)).await?;
        send_unseen_updates(&connection, &session_id, &mut seen, outcome.updates)?;
    }
}

fn send_state(
    connection: &ConnectionTo<Client>,
    session_id: &str,
    state: StateUpdate,
) -> Result<(), Error> {
    connection.send_notification(UpdateSessionNotification::new(
        SessionId::new(session_id),
        SessionUpdate::StateUpdate(state),
    ))
}

fn send_unseen_updates(
    connection: &ConnectionTo<Client>,
    session_id: &str,
    seen: &mut BTreeSet<String>,
    updates: Vec<AcpUpdate>,
) -> Result<(), Error> {
    send_updates(
        connection,
        session_id,
        updates
            .into_iter()
            .filter(|update| seen.insert(update.event_id.clone()))
            .collect(),
    )
}

fn send_updates(
    connection: &ConnectionTo<Client>,
    session_id: &str,
    updates: Vec<AcpUpdate>,
) -> Result<(), Error> {
    let plans = updates
        .iter()
        .filter_map(|update| match &update.kind {
            AcpUpdateKind::Plan {
                summary,
                state,
                terminal,
                ..
            } => Some(PlanEntry::new(
                summary.clone(),
                PlanEntryPriority::Medium,
                plan_status(state, *terminal),
            )),
            _ => None,
        })
        .collect::<Vec<_>>();
    if !plans.is_empty() {
        connection.send_notification(
            UpdateSessionNotification::new(
                SessionId::new(session_id),
                SessionUpdate::PlanUpdate(PlanUpdate::new(PlanUpdateContent::items(
                    "keith-plan",
                    plans,
                ))),
            )
            .meta(event_meta("keith-plan")),
        )?;
    }
    for update in updates {
        if matches!(update.kind, AcpUpdateKind::Plan { .. }) {
            continue;
        }
        connection.send_notification(
            UpdateSessionNotification::new(SessionId::new(session_id), project_update(&update))
                .meta(event_meta(&update.event_id)),
        )?;
    }
    Ok(())
}

fn project_update(update: &AcpUpdate) -> SessionUpdate {
    match &update.kind {
        AcpUpdateKind::AssistantMessage {
            message_id, text, ..
        } => SessionUpdate::AgentMessageChunk(ContentChunk::new(
            ContentBlock::Text(TextContent::new(text)),
            message_id.clone(),
        )),
        AcpUpdateKind::Thought { text } => thought(text, update),
        AcpUpdateKind::Tool {
            tool_call_id,
            title,
            state,
            terminal,
        } => SessionUpdate::ToolCallUpdate(
            ToolCallUpdate::new(tool_call_id.clone())
                .title(title.clone())
                .kind(ToolKind::Other)
                .status(tool_status(state, *terminal)),
        ),
        AcpUpdateKind::ToolOutput { message_id, text } => {
            tool_text(message_id, "Keith tool output", text, ToolKind::Other)
        }
        AcpUpdateKind::Diff { title, patch } => tool_text(
            &format!("keith-diff-{}", update.event_id),
            title,
            patch,
            ToolKind::Edit,
        ),
        AcpUpdateKind::Usage {
            input_tokens,
            output_tokens,
            cached_input_tokens,
            estimated_cost_microunits,
        } => thought(
            &format!(
                "Usage: input {input_tokens}, output {output_tokens}, cached input {cached_input_tokens}, estimated cost {estimated_cost_microunits} microunits"
            ),
            update,
        ),
        AcpUpdateKind::Failure { message, retryable } => thought(
            &format!(
                "Keith failure{}: {message}",
                if *retryable { " (retryable)" } else { "" }
            ),
            update,
        ),
        AcpUpdateKind::Warning { message } => thought(&format!("Keith warning: {message}"), update),
        AcpUpdateKind::Final { detail, .. } => {
            thought(detail.as_deref().unwrap_or("Keith turn finished"), update)
        }
        AcpUpdateKind::Plan { .. } => unreachable!("plans are grouped before projection"),
    }
}

fn thought(text: &str, update: &AcpUpdate) -> SessionUpdate {
    SessionUpdate::AgentThoughtChunk(ContentChunk::new(
        ContentBlock::Text(TextContent::new(text)),
        format!("keith-thought-{}", update.event_id),
    ))
}

fn tool_text(tool_call_id: &str, title: &str, text: &str, kind: ToolKind) -> SessionUpdate {
    SessionUpdate::ToolCallUpdate(
        ToolCallUpdate::new(tool_call_id.to_owned())
            .title(title.to_owned())
            .kind(kind)
            .status(ToolCallStatus::Completed)
            .content(vec![ToolCallContent::Content(Box::new(Content::new(
                ContentBlock::Text(TextContent::new(text)),
            )))]),
    )
}

fn convert_content(blocks: Vec<ContentBlock>) -> Result<Vec<AcpContentBlock>, Error> {
    blocks
        .into_iter()
        .map(|block| match block {
            ContentBlock::Text(text) => Ok(AcpContentBlock::Text(text.text)),
            ContentBlock::ResourceLink(link) => Ok(AcpContentBlock::ResourceLink {
                name: link.name,
                uri: link.uri,
            }),
            ContentBlock::Image(image) => Ok(AcpContentBlock::Binary(AcpBinaryContent {
                name: image.uri.unwrap_or_else(|| "image".to_owned()),
                media_type: image.mime_type.as_ref().to_owned(),
                bytes: decode_base64(&image.data)?,
            })),
            ContentBlock::Audio(AudioContent {
                data, mime_type, ..
            }) => Ok(AcpContentBlock::Binary(AcpBinaryContent {
                name: "audio".to_owned(),
                media_type: mime_type.as_ref().to_owned(),
                bytes: decode_base64(&data)?,
            })),
            ContentBlock::Resource(resource) => match resource.resource {
                EmbeddedResourceResource::TextResourceContents(contents) => {
                    Ok(AcpContentBlock::EmbeddedText {
                        name: contents.uri.clone(),
                        uri: contents.uri,
                        media_type: contents.mime_type.as_ref().map_or_else(
                            || "text/plain".to_owned(),
                            |value| value.as_ref().to_owned(),
                        ),
                        text: contents.text,
                    })
                }
                EmbeddedResourceResource::BlobResourceContents(BlobResourceContents {
                    uri,
                    mime_type,
                    blob,
                    ..
                }) => Ok(AcpContentBlock::Binary(AcpBinaryContent {
                    name: uri,
                    media_type: mime_type.as_ref().map_or_else(
                        || "application/octet-stream".to_owned(),
                        |value| value.as_ref().to_owned(),
                    ),
                    bytes: decode_base64(&blob)?,
                })),
                _ => Err(Error::invalid_params().data("unknown embedded resource type")),
            },
            _ => Err(Error::invalid_params().data("unknown ACP v2 content block")),
        })
        .collect()
}

fn decode_base64(value: &str) -> Result<Vec<u8>, Error> {
    base64::engine::general_purpose::STANDARD
        .decode(value)
        .map_err(|error| Error::invalid_params().data(error.to_string()))
}

fn plan_status(state: &str, terminal: bool) -> PlanEntryStatus {
    if terminal || state.eq_ignore_ascii_case("completed") || state.eq_ignore_ascii_case("done") {
        PlanEntryStatus::Completed
    } else if state.eq_ignore_ascii_case("running")
        || state.eq_ignore_ascii_case("in_progress")
        || state.eq_ignore_ascii_case("active")
    {
        PlanEntryStatus::InProgress
    } else {
        PlanEntryStatus::Pending
    }
}

fn tool_status(state: &str, terminal: bool) -> ToolCallStatus {
    if state.eq_ignore_ascii_case("failed") || state.eq_ignore_ascii_case("error") {
        ToolCallStatus::Failed
    } else if terminal
        || state.eq_ignore_ascii_case("completed")
        || state.eq_ignore_ascii_case("done")
    {
        ToolCallStatus::Completed
    } else if state.eq_ignore_ascii_case("pending") || state.eq_ignore_ascii_case("queued") {
        ToolCallStatus::Pending
    } else {
        ToolCallStatus::InProgress
    }
}

const fn stop_reason(status: TurnTerminalStatus) -> StopReason {
    match status {
        TurnTerminalStatus::Completed => StopReason::EndTurn,
        TurnTerminalStatus::Cancelled => StopReason::Cancelled,
        TurnTerminalStatus::Failed => StopReason::Refusal,
        TurnTerminalStatus::Exhausted => StopReason::MaxTokens,
    }
}

fn connection_meta(
    connection_id: &keith_platform_contracts::AcpConnectionId,
    transport: keith_acp::AcpTransport,
) -> Map<String, Value> {
    Map::from_iter([(
        "keith".to_owned(),
        json!({
            "connectionId": connection_id.to_string(),
            "protocol": "acp/v2-draft",
            "transport": transport,
            "transportAuthentication": match transport {
                keith_acp::AcpTransport::Stdio => "process_owner",
                keith_acp::AcpTransport::HttpSse | keith_acp::AcpTransport::WebSocket => "bearer",
            }
        }),
    )])
}

#[cfg(test)]
mod tests {
    use agent_client_protocol::schema::v2::{HttpHeader, McpServerHttp};

    use super::*;

    #[test]
    fn draft_v2_mcp_schema_is_parsed_without_v1_conversion() {
        let context = session_config(vec![McpServer::Http(
            McpServerHttp::new("docs", "https://tools.example/mcp").headers(vec![HttpHeader::new(
                "Authorization",
                "credential://docs-token",
            )]),
        )])
        .unwrap();
        assert_eq!(context.mcp_servers.len(), 1);
        assert!(matches!(
            context.mcp_servers[0].transport,
            AcpMcpTransport::Http { .. }
        ));
    }

    #[test]
    fn draft_v2_refuses_raw_mcp_secrets() {
        let error = session_config(vec![McpServer::Http(
            McpServerHttp::new("docs", "https://tools.example/mcp")
                .headers(vec![HttpHeader::new("Authorization", "raw-secret")]),
        )])
        .unwrap_err();
        assert!(matches!(error, BridgeError::McpPolicy(_)));
    }
}
