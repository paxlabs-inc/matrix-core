#![forbid(unsafe_code)]

pub mod client_facilities;
mod managed_transport;

#[cfg(feature = "unstable-acp-v2")]
mod v2;

use std::collections::{BTreeSet, HashMap};
use std::io;
use std::net::SocketAddr;
use std::path::PathBuf;
use std::str::FromStr;
use std::sync::{Arc, Mutex};
use std::time::Duration;

use agent_client_protocol::schema::ProtocolVersion;
use agent_client_protocol::schema::v1::{
    AgentCapabilities, AudioContent, BlobResourceContents, CancelNotification, ClientCapabilities,
    CloseSessionRequest, CloseSessionResponse, Content, ContentBlock, ContentChunk,
    EmbeddedResourceResource, ForkSessionRequest, ForkSessionResponse, Implementation,
    InitializeRequest, InitializeResponse, LoadSessionRequest, LoadSessionResponse,
    McpCapabilities, NewSessionRequest, NewSessionResponse, Plan, PlanEntry, PlanEntryPriority,
    PlanEntryStatus, PromptCapabilities, PromptRequest, PromptResponse, ResourceLink,
    ResumeSessionRequest, ResumeSessionResponse, SessionAdditionalDirectoriesCapabilities,
    SessionCapabilities, SessionCloseCapabilities, SessionForkCapabilities, SessionId,
    SessionNotification, SessionResumeCapabilities, SessionUpdate, StopReason, TextContent,
    ToolCall, ToolCallContent, ToolCallStatus, ToolKind,
};
use agent_client_protocol::{Agent, Client, ConnectTo, ConnectionTo, Error, Lines, Responder};
use base64::Engine as _;
use futures_util::{Sink, SinkExt as _, Stream, StreamExt as _};
use keith_acp::{
    AcpBinaryContent, AcpBridgeConfig, AcpClientCapabilities, AcpClientPolicy,
    AcpClientSessionConfig, AcpClientToolBroker, AcpContentBlock, AcpProtocolRouter,
    AcpProtocolVersion, AcpSessionBridge, AcpTransport, AcpUpdate, AcpUpdateKind, BridgeError,
};
use keith_agent_types::{ProfileId, WorkspaceId};
use keith_platform_contracts::AcpConnectionId;
use keith_protocol::TurnTerminalStatus;
use serde_json::{Map, Value, json};
use tokio::io::{AsyncBufReadExt as _, AsyncWriteExt as _};

const POLL_INTERVAL: Duration = Duration::from_millis(100);

fn agent_capabilities(policy: &AcpClientPolicy) -> AgentCapabilities {
    AgentCapabilities::new()
        .load_session(true)
        .prompt_capabilities(
            PromptCapabilities::new()
                .image(true)
                .audio(true)
                .embedded_context(true),
        )
        .session_capabilities(
            SessionCapabilities::new()
                .additional_directories(SessionAdditionalDirectoriesCapabilities::new())
                .fork(SessionForkCapabilities::new())
                .resume(SessionResumeCapabilities::new())
                .close(SessionCloseCapabilities::new()),
        )
        .mcp_capabilities(
            McpCapabilities::new()
                .http(!policy.allowed_network_hosts.is_empty())
                .sse(!policy.allowed_network_hosts.is_empty()),
        )
}

#[derive(Debug)]
struct RuntimeConfig {
    bridge: AcpBridgeConfig,
    client_policy: AcpClientPolicy,
    transport: RuntimeTransport,
    draft_v2_enabled: bool,
}

#[derive(Debug)]
enum RuntimeTransport {
    Stdio,
    Managed {
        listen: SocketAddr,
        bearer_token: String,
    },
}

struct AgentRuntime {
    bridge: Arc<AcpSessionBridge>,
    client_tools: Arc<AcpClientToolBroker>,
    protocol_router: AcpProtocolRouter,
}

struct KeithAcpAgent {
    runtime: Arc<AgentRuntime>,
    connection_id: AcpConnectionId,
    client_capabilities: Arc<Mutex<Option<ClientCapabilities>>>,
    protocol_version: AcpProtocolVersion,
    transport: AcpTransport,
}

impl KeithAcpAgent {
    fn from_runtime(runtime: Arc<AgentRuntime>) -> Self {
        Self {
            runtime,
            connection_id: AcpConnectionId::new(),
            client_capabilities: Arc::new(Mutex::new(None)),
            protocol_version: AcpProtocolVersion::StableV1,
            transport: AcpTransport::Stdio,
        }
    }

    async fn serve_lines<Outgoing, Incoming>(
        mut self,
        outgoing: Outgoing,
        incoming: Incoming,
        transport: AcpTransport,
    ) -> Result<(), Error>
    where
        Outgoing: Sink<String, Error = io::Error> + Send + 'static,
        Incoming: Stream<Item = io::Result<String>> + Send + 'static,
    {
        let mut outgoing = Box::pin(outgoing);
        let mut incoming = Box::pin(incoming);
        let Some(first) = incoming.next().await else {
            return Ok(());
        };
        let first = first.map_err(|error| Error::internal_error().data(error.to_string()))?;
        let route = match self.runtime.protocol_router.route_initialize(&first) {
            Ok(route) => route,
            Err(error) => {
                outgoing
                    .as_mut()
                    .send(error.response().to_string())
                    .await
                    .map_err(|error| Error::internal_error().data(error.to_string()))?;
                outgoing
                    .as_mut()
                    .close()
                    .await
                    .map_err(|error| Error::internal_error().data(error.to_string()))?;
                return Ok(());
            }
        };
        self.protocol_version = route.version;
        self.transport = transport;
        let incoming = futures_util::stream::once(async move { Ok(first) }).chain(incoming);
        let transport = Lines::new(outgoing, incoming);
        match route.version {
            AcpProtocolVersion::StableV1 => self.serve_v1(transport).await,
            AcpProtocolVersion::DraftV2 => {
                #[cfg(feature = "unstable-acp-v2")]
                {
                    v2::serve(self, transport).await
                }
                #[cfg(not(feature = "unstable-acp-v2"))]
                {
                    Err(Error::invalid_params().data("draft ACP v2 is not compiled"))
                }
            }
        }
    }

    #[allow(clippy::too_many_lines)]
    async fn serve_v1(self, transport: impl ConnectTo<Agent>) -> Result<(), Error> {
        let bridge = Arc::clone(&self.runtime.bridge);
        let client_tools = Arc::clone(&self.runtime.client_tools);
        let client_capabilities = Arc::clone(&self.client_capabilities);
        let policy = self.runtime.client_tools.policy().clone();
        let connection_id = self.connection_id;
        let transport_kind = self.transport;

        Agent
            .builder()
            .name("keith-agent-acp")
            .on_receive_request(
                {
                    let connection_id = connection_id.clone();
                    let client_capabilities = Arc::clone(&client_capabilities);
                    async move |request: InitializeRequest, responder, _connection| {
                        if request.protocol_version != ProtocolVersion::V1 {
                            return responder.respond_with_error(Error::invalid_params().data(
                                json!({
                                    "reason": "unsupported_protocol_version",
                                    "requested": request.protocol_version,
                                    "supported": ProtocolVersion::V1,
                                }),
                            ));
                        }
                        let mut stored = client_capabilities.lock().map_err(|_| {
                            Error::internal_error().data("ACP client capability lock poisoned")
                        })?;
                        *stored = Some(request.client_capabilities);
                        drop(stored);
                        let capabilities = agent_capabilities(&policy)
                            .meta(connection_meta(&connection_id, transport_kind));
                        responder.respond(
                            InitializeResponse::new(ProtocolVersion::V1)
                                .agent_capabilities(capabilities)
                                .agent_info(Implementation::new("keith", env!("CARGO_PKG_VERSION")))
                                .meta(connection_meta(&connection_id, transport_kind)),
                        )
                    }
                },
                agent_client_protocol::on_receive_request!(),
            )
            .on_receive_request(
                {
                    let bridge = Arc::clone(&bridge);
                    let client_tools = Arc::clone(&client_tools);
                    let client_capabilities = Arc::clone(&client_capabilities);
                    async move |request: NewSessionRequest, responder, connection| {
                        let capabilities = match negotiated_capabilities(&client_capabilities) {
                            Ok(capabilities) => capabilities,
                            Err(error) => return responder.respond_with_error(error),
                        };
                        let context = match client_facilities::session_config(
                            &capabilities,
                            request.mcp_servers,
                        )
                        .and_then(|context| client_tools.validate_session_config(context))
                        {
                            Ok(context) => context,
                            Err(error) => return responder.respond_with_error(bridge_error(error)),
                        };
                        let bridge = Arc::clone(&bridge);
                        let client_tools = Arc::clone(&client_tools);
                        let task_connection = connection.clone();
                        connection.spawn(async move {
                            let result = blocking(move || {
                                let (record, updates) = bridge.create_session(
                                    &request.cwd,
                                    &request.additional_directories,
                                )?;
                                let record = bind_client_session(
                                    &bridge,
                                    &client_tools,
                                    &record,
                                    context,
                                    AcpProtocolVersion::StableV1,
                                )?;
                                Ok((record, updates))
                            })
                            .await;
                            match result {
                                Ok((record, updates)) => {
                                    send_updates(
                                        &task_connection,
                                        &record.acp_session_id,
                                        updates,
                                    )?;
                                    responder
                                        .respond(NewSessionResponse::new(record.acp_session_id))
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
                    let client_capabilities = Arc::clone(&client_capabilities);
                    async move |request: LoadSessionRequest, responder, connection| {
                        let capabilities = match negotiated_capabilities(&client_capabilities) {
                            Ok(capabilities) => capabilities,
                            Err(error) => return responder.respond_with_error(error),
                        };
                        let context = match client_facilities::session_config(
                            &capabilities,
                            request.mcp_servers,
                        )
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
                            let result = blocking(move || {
                                let (record, updates) = bridge.load_session(
                                    &session_id,
                                    &request.cwd,
                                    &request.additional_directories,
                                )?;
                                let record = bind_client_session(
                                    &bridge,
                                    &client_tools,
                                    &record,
                                    context,
                                    AcpProtocolVersion::StableV1,
                                )?;
                                Ok((record, updates))
                            })
                            .await;
                            match result {
                                Ok((record, updates)) => {
                                    send_updates(
                                        &task_connection,
                                        &record.acp_session_id,
                                        updates,
                                    )?;
                                    responder.respond(LoadSessionResponse::new())
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
                    let client_capabilities = Arc::clone(&client_capabilities);
                    async move |request: ResumeSessionRequest, responder, connection| {
                        let capabilities = match negotiated_capabilities(&client_capabilities) {
                            Ok(capabilities) => capabilities,
                            Err(error) => return responder.respond_with_error(error),
                        };
                        let context = match client_facilities::session_config(
                            &capabilities,
                            request.mcp_servers,
                        )
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
                            let result = blocking(move || {
                                let (record, _) = bridge.load_session(
                                    &session_id,
                                    &request.cwd,
                                    &request.additional_directories,
                                )?;
                                let record = bind_client_session(
                                    &bridge,
                                    &client_tools,
                                    &record,
                                    context,
                                    AcpProtocolVersion::StableV1,
                                )?;
                                let outcome = bridge.resume_session(&session_id)?;
                                Ok((record, outcome))
                            })
                            .await;
                            match result {
                                Ok((record, outcome)) => {
                                    send_updates(
                                        &task_connection,
                                        &record.acp_session_id,
                                        outcome.updates,
                                    )?;
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
                    let client_capabilities = Arc::clone(&client_capabilities);
                    async move |request: ForkSessionRequest, responder, connection| {
                        let capabilities = match negotiated_capabilities(&client_capabilities) {
                            Ok(capabilities) => capabilities,
                            Err(error) => return responder.respond_with_error(error),
                        };
                        let context = match client_facilities::session_config(
                            &capabilities,
                            request.mcp_servers,
                        )
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
                            let result = blocking(move || {
                                let source = bridge.session(&source_session_id)?;
                                let source_context =
                                    source.client_context.clone().unwrap_or_default();
                                bind_client_session(
                                    &bridge,
                                    &client_tools,
                                    &source,
                                    source_context,
                                    AcpProtocolVersion::StableV1,
                                )?;
                                let (record, updates) = bridge.fork_session(
                                    &source_session_id,
                                    &request.cwd,
                                    &request.additional_directories,
                                )?;
                                let record = bind_client_session(
                                    &bridge,
                                    &client_tools,
                                    &record,
                                    context,
                                    AcpProtocolVersion::StableV1,
                                )?;
                                Ok((record, updates))
                            })
                            .await;
                            match result {
                                Ok((record, updates)) => {
                                    send_updates(
                                        &task_connection,
                                        &record.acp_session_id,
                                        updates,
                                    )?;
                                    responder
                                        .respond(ForkSessionResponse::new(record.acp_session_id))
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
                                ensure_session_version(&record, AcpProtocolVersion::StableV1)?;
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
                    async move |request: PromptRequest,
                                responder: Responder<PromptResponse>,
                                connection| {
                        let bridge = Arc::clone(&bridge);
                        let cancellation = responder.cancellation();
                        let task_connection = connection.clone();
                        connection.spawn(async move {
                            drive_prompt(
                                bridge,
                                request,
                                responder,
                                task_connection,
                                cancellation,
                                AcpProtocolVersion::StableV1,
                            )
                            .await
                        })?;
                        Ok(())
                    }
                },
                agent_client_protocol::on_receive_request!(),
            )
            .on_receive_notification(
                {
                    let bridge = Arc::clone(&bridge);
                    async move |notification: CancelNotification, connection| {
                        let bridge = Arc::clone(&bridge);
                        connection.spawn(async move {
                            let session_id = notification.session_id.to_string();
                            let _ = blocking(move || {
                                let record = bridge.session(&session_id)?;
                                ensure_session_version(&record, AcpProtocolVersion::StableV1)?;
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
}

async fn drive_prompt(
    bridge: Arc<AcpSessionBridge>,
    request: PromptRequest,
    responder: Responder<PromptResponse>,
    connection: ConnectionTo<Client>,
    cancellation: agent_client_protocol::RequestCancellation,
    protocol_version: AcpProtocolVersion,
) -> Result<(), Error> {
    let session_id = request.session_id.to_string();
    let content = match convert_content(request.prompt) {
        Ok(content) => content,
        Err(error) => return responder.respond_with_error(error),
    };
    let prompt_bridge = Arc::clone(&bridge);
    let prompt_session_id = session_id.clone();
    let mut outcome = match blocking(move || {
        let record = prompt_bridge.session(&prompt_session_id)?;
        ensure_session_version(&record, protocol_version)?;
        prompt_bridge.prompt(&prompt_session_id, &content)
    })
    .await
    {
        Ok(outcome) => outcome,
        Err(error) => return responder.respond_with_error(error),
    };
    let mut seen = BTreeSet::new();
    send_unseen_updates(&connection, &session_id, &mut seen, outcome.updates)?;

    loop {
        if cancellation.is_cancelled() {
            let cancel_bridge = Arc::clone(&bridge);
            let cancel_session_id = session_id.clone();
            let updates =
                match blocking(move || cancel_bridge.cancel_session(&cancel_session_id)).await {
                    Ok(updates) => updates,
                    Err(error) => return responder.respond_with_error(error),
                };
            send_unseen_updates(&connection, &session_id, &mut seen, updates)?;
            return responder.respond_with_error(Error::request_cancelled());
        }
        if let Some(status) = outcome.terminal {
            return responder.respond(PromptResponse::new(stop_reason(status)));
        }
        tokio::time::sleep(POLL_INTERVAL).await;
        let resume_bridge = Arc::clone(&bridge);
        let resume_session_id = session_id.clone();
        outcome = match blocking(move || resume_bridge.resume_session(&resume_session_id)).await {
            Ok(outcome) => outcome,
            Err(error) => return responder.respond_with_error(error),
        };
        send_unseen_updates(&connection, &session_id, &mut seen, outcome.updates)?;
    }
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
        connection.send_notification(SessionNotification::new(
            SessionId::new(session_id),
            SessionUpdate::Plan(Plan::new(plans)),
        ))?;
    }
    for update in updates {
        if matches!(update.kind, AcpUpdateKind::Plan { .. }) {
            continue;
        }
        connection.send_notification(
            SessionNotification::new(SessionId::new(session_id), project_update(&update))
                .meta(event_meta(&update.event_id)),
        )?;
    }
    Ok(())
}

fn project_update(update: &AcpUpdate) -> SessionUpdate {
    match &update.kind {
        AcpUpdateKind::AssistantMessage { text, .. } => SessionUpdate::AgentMessageChunk(
            ContentChunk::new(ContentBlock::Text(TextContent::new(text.clone())))
                .meta(event_meta(&update.event_id)),
        ),
        AcpUpdateKind::Thought { text } => thought(text, update),
        AcpUpdateKind::Tool {
            tool_call_id,
            title,
            state,
            terminal,
        } => SessionUpdate::ToolCall(
            ToolCall::new(tool_call_id.clone(), title.clone())
                .kind(ToolKind::Other)
                .status(tool_status(state, *terminal))
                .meta(event_meta(&update.event_id)),
        ),
        AcpUpdateKind::ToolOutput { message_id, text } => tool_text(
            message_id,
            "Keith tool output",
            text,
            ToolKind::Other,
            update,
        ),
        AcpUpdateKind::Diff { title, patch } => tool_text(
            &format!("keith-diff-{}", update.event_id),
            title,
            patch,
            ToolKind::Edit,
            update,
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
    SessionUpdate::AgentThoughtChunk(
        ContentChunk::new(ContentBlock::Text(TextContent::new(text)))
            .meta(event_meta(&update.event_id)),
    )
}

fn tool_text(
    tool_call_id: &str,
    title: &str,
    text: &str,
    kind: ToolKind,
    update: &AcpUpdate,
) -> SessionUpdate {
    SessionUpdate::ToolCall(
        ToolCall::new(tool_call_id.to_owned(), title.to_owned())
            .kind(kind)
            .status(ToolCallStatus::Completed)
            .content(vec![ToolCallContent::Content(Content::new(
                ContentBlock::Text(TextContent::new(text)),
            ))])
            .meta(event_meta(&update.event_id)),
    )
}

fn convert_content(blocks: Vec<ContentBlock>) -> Result<Vec<AcpContentBlock>, Error> {
    blocks
        .into_iter()
        .map(|block| match block {
            ContentBlock::Text(text) => Ok(AcpContentBlock::Text(text.text)),
            ContentBlock::ResourceLink(ResourceLink { name, uri, .. }) => {
                Ok(AcpContentBlock::ResourceLink { name, uri })
            }
            ContentBlock::Image(image) => Ok(AcpContentBlock::Binary(AcpBinaryContent {
                name: image.uri.unwrap_or_else(|| "image".to_owned()),
                media_type: image.mime_type,
                bytes: decode_base64(&image.data)?,
            })),
            ContentBlock::Audio(AudioContent {
                data, mime_type, ..
            }) => Ok(AcpContentBlock::Binary(AcpBinaryContent {
                name: "audio".to_owned(),
                media_type: mime_type,
                bytes: decode_base64(&data)?,
            })),
            ContentBlock::Resource(resource) => match resource.resource {
                EmbeddedResourceResource::TextResourceContents(contents) => {
                    Ok(AcpContentBlock::EmbeddedText {
                        name: contents.uri.clone(),
                        uri: contents.uri,
                        media_type: contents
                            .mime_type
                            .unwrap_or_else(|| "text/plain".to_owned()),
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
                    media_type: mime_type.unwrap_or_else(|| "application/octet-stream".to_owned()),
                    bytes: decode_base64(&blob)?,
                })),
                _ => Err(unsupported_content("unknown embedded resource type")),
            },
            _ => Err(unsupported_content("unknown ACP content block")),
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

fn connection_meta(connection_id: &AcpConnectionId, transport: AcpTransport) -> Map<String, Value> {
    Map::from_iter([(
        "keith".to_owned(),
        json!({
            "connectionId": connection_id.to_string(),
            "protocol": "acp/v1",
            "transport": transport,
            "transportAuthentication": match transport {
                AcpTransport::Stdio => "process_owner",
                AcpTransport::HttpSse | AcpTransport::WebSocket => "bearer",
            }
        }),
    )])
}

fn event_meta(event_id: &str) -> Map<String, Value> {
    Map::from_iter([("keith".to_owned(), json!({ "eventId": event_id }))])
}

fn unsupported_content(message: &str) -> Error {
    Error::invalid_params().data(message.to_owned())
}

async fn blocking<T>(
    operation: impl FnOnce() -> Result<T, BridgeError> + Send + 'static,
) -> Result<T, Error>
where
    T: Send + 'static,
{
    tokio::task::spawn_blocking(operation)
        .await
        .map_err(|error| Error::internal_error().data(error.to_string()))?
        .map_err(bridge_error)
}

fn negotiated_capabilities(
    capabilities: &Mutex<Option<ClientCapabilities>>,
) -> Result<ClientCapabilities, Error> {
    capabilities
        .lock()
        .map_err(|_| Error::internal_error().data("ACP client capability lock poisoned"))?
        .clone()
        .ok_or_else(|| Error::invalid_request().data("ACP initialize must complete first"))
}

fn bind_client_session(
    bridge: &AcpSessionBridge,
    tools: &AcpClientToolBroker,
    record: &keith_acp::AcpSessionRecord,
    context: AcpClientSessionConfig,
    version: AcpProtocolVersion,
) -> Result<keith_acp::AcpSessionRecord, BridgeError> {
    ensure_session_version(record, version)?;
    let validated = tools.validate_session_config(context)?;
    let bound = bridge.bind_client_context(&record.acp_session_id, validated, version.wire())?;
    let effective = bound.client_context.clone().ok_or_else(|| {
        BridgeError::ClientCapability("durable client context was not retained".to_owned())
    })?;
    tools.register_session(&bound, effective)?;
    Ok(bound)
}

fn ensure_session_version(
    record: &keith_acp::AcpSessionRecord,
    version: AcpProtocolVersion,
) -> Result<(), BridgeError> {
    if let Some(bound) = record.protocol_version
        && bound != version.wire()
    {
        return Err(BridgeError::ProtocolVersion(format!(
            "session belongs to ACP v{bound}, not ACP v{}",
            version.wire(),
        )));
    }
    Ok(())
}

fn bridge_error(error: BridgeError) -> Error {
    match error {
        BridgeError::UnknownSession(session_id) => {
            Error::resource_not_found(Some(session_id)).data("unknown Keith ACP session")
        }
        BridgeError::WorkspaceBoundary(path) => Error::invalid_params().data(json!({
            "reason": "workspace_boundary",
            "path": path
        })),
        BridgeError::UnsupportedContent(message)
        | BridgeError::ClientCapability(message)
        | BridgeError::McpPolicy(message)
        | BridgeError::PermissionDenied(message)
        | BridgeError::ProtocolVersion(message) => Error::invalid_params().data(message),
        BridgeError::ContentLimit | BridgeError::AttachmentLimit => {
            Error::invalid_params().data(error.to_string())
        }
        _ => Error::internal_error().data(error.to_string()),
    }
}

impl RuntimeConfig {
    #[allow(clippy::too_many_lines)]
    fn parse(arguments: impl IntoIterator<Item = String>) -> Result<Self, String> {
        let mut values = arguments.into_iter();
        let _program = values.next();
        let mut options = HashMap::<String, Vec<String>>::new();
        while let Some(option) = values.next() {
            if !option.starts_with("--") {
                return Err(format!("unexpected positional argument: {option}"));
            }
            let value = values
                .next()
                .ok_or_else(|| format!("missing value for {option}"))?;
            options.entry(option).or_default().push(value);
        }
        let endpoint = one(&mut options, "--socket")?;
        let state_root = one(&mut options, "--state-root")?;
        let staging_root = optional_one(&mut options, "--staging-root")?
            .map_or_else(|| PathBuf::from(&state_root).join("staging"), PathBuf::from);
        let profile_id = ProfileId::from_str(&one(&mut options, "--profile")?)
            .map_err(|error| error.to_string())?;
        let workspace_id = WorkspaceId::from_str(&one(&mut options, "--workspace")?)
            .map_err(|error| error.to_string())?;
        let workspace_roots = options
            .remove("--workspace-root")
            .ok_or_else(|| "missing --workspace-root".to_owned())?
            .into_iter()
            .map(PathBuf::from)
            .collect();
        let allow_client_write = parse_bool_option(
            optional_one(&mut options, "--allow-client-write")?.as_deref(),
            false,
            "--allow-client-write",
        )?;
        let allow_client_terminal = parse_bool_option(
            optional_one(&mut options, "--allow-client-terminal")?.as_deref(),
            false,
            "--allow-client-terminal",
        )?;
        let draft_v2_enabled = parse_bool_option(
            optional_one(&mut options, "--unstable-acp-v2")?.as_deref(),
            false,
            "--unstable-acp-v2",
        )?;
        if draft_v2_enabled && !cfg!(feature = "unstable-acp-v2") {
            return Err(
                "--unstable-acp-v2 requires the unstable-acp-v2 compile-time feature".to_owned(),
            );
        }
        let allowed_executables = options
            .remove("--allow-client-executable")
            .unwrap_or_default()
            .into_iter()
            .map(PathBuf::from)
            .collect();
        let allowed_network_hosts = options
            .remove("--allow-client-network-host")
            .unwrap_or_default()
            .into_iter()
            .collect();
        let allowed_credential_references = options
            .remove("--allow-client-credential-ref")
            .unwrap_or_default()
            .into_iter()
            .collect();
        let transport_name =
            optional_one(&mut options, "--transport")?.unwrap_or_else(|| "stdio".to_owned());
        let transport = match transport_name.as_str() {
            "stdio" => {
                if options.contains_key("--listen") || options.contains_key("--bearer-token-env") {
                    return Err("managed transport options require --transport managed".to_owned());
                }
                RuntimeTransport::Stdio
            }
            "managed" => {
                let listen = one(&mut options, "--listen")?
                    .parse::<SocketAddr>()
                    .map_err(|error| format!("invalid --listen address: {error}"))?;
                let token_environment = one(&mut options, "--bearer-token-env")?;
                if token_environment.is_empty()
                    || !token_environment
                        .bytes()
                        .all(|byte| byte.is_ascii_alphanumeric() || matches!(byte, b'_' | b'-'))
                {
                    return Err("--bearer-token-env must name one environment variable".to_owned());
                }
                let bearer_token = std::env::var(&token_environment).map_err(|_| {
                    format!("managed bearer environment variable {token_environment} is missing")
                })?;
                RuntimeTransport::Managed {
                    listen,
                    bearer_token,
                }
            }
            _ => return Err("--transport must be stdio or managed".to_owned()),
        };
        if let Some(option) = options.keys().next() {
            return Err(format!("unknown option: {option}"));
        }
        Ok(Self {
            bridge: AcpBridgeConfig {
                daemon_endpoint: PathBuf::from(endpoint),
                state_root: PathBuf::from(state_root),
                staging_root,
                profile_id,
                workspace_id,
                workspace_roots,
                max_prompt_bytes: 2 * 1_024 * 1_024,
                max_attachments: 16,
                max_attachment_bytes: 25 * 1_024 * 1_024,
                max_total_attachment_bytes: 50 * 1_024 * 1_024,
            },
            client_policy: AcpClientPolicy {
                capabilities: AcpClientCapabilities {
                    read_text_file: true,
                    write_text_file: allow_client_write,
                    terminal: allow_client_terminal,
                },
                allowed_executables,
                allowed_network_hosts,
                allowed_credential_references,
                ..AcpClientPolicy::default()
            },
            transport,
            draft_v2_enabled,
        })
    }
}

fn one(options: &mut HashMap<String, Vec<String>>, name: &str) -> Result<String, String> {
    let values = options
        .remove(name)
        .ok_or_else(|| format!("missing {name}"))?;
    if values.len() != 1 {
        return Err(format!("{name} must be specified exactly once"));
    }
    Ok(values.into_iter().next().expect("length checked"))
}

fn optional_one(
    options: &mut HashMap<String, Vec<String>>,
    name: &str,
) -> Result<Option<String>, String> {
    let Some(values) = options.remove(name) else {
        return Ok(None);
    };
    if values.len() != 1 {
        return Err(format!("{name} must be specified at most once"));
    }
    Ok(values.into_iter().next())
}

fn parse_bool_option(value: Option<&str>, default: bool, name: &str) -> Result<bool, String> {
    match value {
        None => Ok(default),
        Some("true") => Ok(true),
        Some("false") => Ok(false),
        Some(_) => Err(format!("{name} must be true or false")),
    }
}

async fn serve_stdio(agent: KeithAcpAgent) -> Result<(), Error> {
    let incoming = futures_util::stream::unfold(
        tokio::io::BufReader::new(tokio::io::stdin()).lines(),
        |mut lines| async move {
            match lines.next_line().await {
                Ok(Some(line)) => Some((Ok(line), lines)),
                Ok(None) => None,
                Err(error) => Some((Err(error), lines)),
            }
        },
    );
    let outgoing =
        futures_util::sink::unfold(tokio::io::stdout(), |mut stdout, line: String| async move {
            stdout.write_all(line.as_bytes()).await?;
            stdout.write_all(b"\n").await?;
            stdout.flush().await?;
            Ok::<_, io::Error>(stdout)
        });
    agent
        .serve_lines(outgoing, incoming, AcpTransport::Stdio)
        .await
}

#[tokio::main]
async fn main() -> Result<(), Box<dyn std::error::Error>> {
    let config = RuntimeConfig::parse(std::env::args()).map_err(std::io::Error::other)?;
    let runtime = Arc::new(AgentRuntime {
        bridge: Arc::new(AcpSessionBridge::open(config.bridge)?),
        client_tools: Arc::new(AcpClientToolBroker::new(config.client_policy)?),
        protocol_router: AcpProtocolRouter::new(config.draft_v2_enabled),
    });
    match config.transport {
        RuntimeTransport::Stdio => serve_stdio(KeithAcpAgent::from_runtime(runtime)).await?,
        RuntimeTransport::Managed {
            listen,
            bearer_token,
        } => managed_transport::serve(runtime, listen, &bearer_token).await?,
    }
    Ok(())
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn arguments_require_an_explicit_workspace_root() {
        let error = RuntimeConfig::parse(vec![
            "keith-agent-acp".to_owned(),
            "--socket".to_owned(),
            "/tmp/keith.sock".to_owned(),
            "--state-root".to_owned(),
            "/tmp/state".to_owned(),
            "--profile".to_owned(),
            ProfileId::new().to_string(),
            "--workspace".to_owned(),
            WorkspaceId::new().to_string(),
        ])
        .expect_err("workspace roots are mandatory");
        assert_eq!(error, "missing --workspace-root");
    }

    #[test]
    fn status_projection_preserves_terminal_failures() {
        assert_eq!(tool_status("failed", true), ToolCallStatus::Failed);
        assert_eq!(plan_status("active", false), PlanEntryStatus::InProgress);
        assert_eq!(
            stop_reason(TurnTerminalStatus::Cancelled),
            StopReason::Cancelled
        );
    }

    #[test]
    fn stable_v1_capabilities_advertise_the_implemented_session_fork() {
        let capabilities =
            serde_json::to_value(agent_capabilities(&AcpClientPolicy::default())).unwrap();
        assert!(capabilities["sessionCapabilities"]["fork"].is_object());
    }
}
