use std::collections::BTreeMap;
use std::io;
use std::net::SocketAddr;
use std::sync::{Arc, Mutex};

use axum::Router;
use axum::body::{Body, Bytes};
use axum::extract::ws::{Message, WebSocket, WebSocketUpgrade};
use axum::extract::{DefaultBodyLimit, Path, Query, State};
use axum::http::header::{AUTHORIZATION, CACHE_CONTROL, CONTENT_TYPE, WWW_AUTHENTICATE};
use axum::http::{HeaderMap, HeaderName, HeaderValue, StatusCode};
use axum::response::{IntoResponse, Response};
use axum::routing::{get, post, put};
use futures_util::stream::{self, StreamExt as _};
use futures_util::{SinkExt as _, TryStreamExt as _};
use keith_acp::{
    AcpTransport, AcpTransportAuthenticator, AcpTransportConnection, AcpTransportError,
    AcpTransportFrame, AcpTransportFrameId,
};
use serde::{Deserialize, Serialize};
use tokio::net::TcpListener;
use tokio::sync::{Mutex as AsyncMutex, broadcast, mpsc};

use crate::{AgentRuntime, KeithAcpAgent};

const MAX_MANAGED_FRAME_BYTES: usize = 2 * 1_024 * 1_024;
const MAX_REPLAY_FRAMES: usize = 512;
const EVENT_CHANNEL_CAPACITY: usize = 512;

#[derive(Clone)]
struct ManagedState {
    runtime: Arc<AgentRuntime>,
    authenticator: AcpTransportAuthenticator,
    connections: Arc<AsyncMutex<BTreeMap<String, SseConnection>>>,
}

#[derive(Clone)]
struct SseConnection {
    ingress: mpsc::UnboundedSender<String>,
    replay: Arc<Mutex<AcpTransportConnection>>,
    events: broadcast::Sender<AcpTransportFrame>,
}

#[derive(Debug, Deserialize)]
#[serde(deny_unknown_fields)]
struct ReplayQuery {
    after: Option<u64>,
}

#[derive(Serialize)]
#[serde(rename_all = "camelCase")]
struct CreatedConnection<'a> {
    connection_id: &'a str,
    events: String,
    messages: String,
}

pub(crate) async fn serve(
    runtime: Arc<AgentRuntime>,
    listen: SocketAddr,
    bearer_token: &str,
) -> Result<(), Box<dyn std::error::Error>> {
    let state = ManagedState {
        runtime,
        authenticator: AcpTransportAuthenticator::new(bearer_token)?,
        connections: Arc::new(AsyncMutex::new(BTreeMap::new())),
    };
    let router = Router::new()
        .route("/acp/ws", get(websocket))
        .route(
            "/acp/sse/{connection_id}",
            put(create_sse).delete(close_sse),
        )
        .route("/acp/sse/{connection_id}/messages", post(post_sse_message))
        .route("/acp/sse/{connection_id}/events", get(stream_sse_events))
        .layer(DefaultBodyLimit::max(MAX_MANAGED_FRAME_BYTES))
        .with_state(state);
    let listener = TcpListener::bind(listen).await?;
    eprintln!(
        "Keith ACP managed transport listening on {}",
        listener.local_addr()?
    );
    axum::serve(listener, router).await?;
    Ok(())
}

async fn websocket(
    State(state): State<ManagedState>,
    headers: HeaderMap,
    upgrade: WebSocketUpgrade,
) -> Response {
    if authenticate(&state, &headers).is_err() {
        return unauthorized();
    }
    upgrade
        .on_upgrade(move |socket| serve_websocket(state.runtime, socket))
        .into_response()
}

async fn serve_websocket(runtime: Arc<AgentRuntime>, socket: WebSocket) {
    let (outgoing, incoming) = socket.split();
    let outgoing = outgoing
        .with(|line: String| async move { Ok::<Message, axum::Error>(Message::Text(line.into())) })
        .sink_map_err(io::Error::other);
    let incoming = incoming.filter_map(|message| async move {
        match message {
            Ok(Message::Text(text)) => Some(Ok(text.to_string())),
            Ok(Message::Close(_) | Message::Ping(_) | Message::Pong(_)) => None,
            Ok(Message::Binary(_)) => Some(Err(io::Error::new(
                io::ErrorKind::InvalidData,
                "ACP WebSocket frames must be UTF-8 JSON text",
            ))),
            Err(error) => Some(Err(io::Error::other(error))),
        }
    });
    let _ = KeithAcpAgent::from_runtime(runtime)
        .serve_lines(outgoing, incoming, AcpTransport::WebSocket)
        .await;
}

async fn create_sse(
    State(state): State<ManagedState>,
    Path(connection_id): Path<String>,
    headers: HeaderMap,
) -> Response {
    if authenticate(&state, &headers).is_err() {
        return unauthorized();
    }
    if !valid_connection_id(&connection_id) {
        return (StatusCode::BAD_REQUEST, "invalid connection id").into_response();
    }
    let mut connections = state.connections.lock().await;
    if connections.contains_key(&connection_id) {
        return (StatusCode::CONFLICT, "connection id already exists").into_response();
    }
    let (ingress, ingress_rx) = mpsc::unbounded_channel();
    let (events, _) = broadcast::channel(EVENT_CHANNEL_CAPACITY);
    let replay = match AcpTransportConnection::new(
        AcpTransport::HttpSse,
        MAX_MANAGED_FRAME_BYTES,
        MAX_REPLAY_FRAMES,
    ) {
        Ok(connection) => Arc::new(Mutex::new(connection)),
        Err(_) => return StatusCode::INTERNAL_SERVER_ERROR.into_response(),
    };
    connections.insert(
        connection_id.clone(),
        SseConnection {
            ingress,
            replay: Arc::clone(&replay),
            events: events.clone(),
        },
    );
    drop(connections);

    let incoming = stream::unfold(ingress_rx, |mut receiver| async move {
        receiver
            .recv()
            .await
            .map(|line| (Ok::<_, io::Error>(line), receiver))
    });
    let outgoing = futures_util::sink::unfold(
        (replay, events),
        |(replay, events), line: String| async move {
            let frame = replay
                .lock()
                .map_err(|_| io::Error::other("ACP SSE replay lock poisoned"))?
                .publish(line)
                .map_err(io::Error::other)?;
            let _ = events.send(frame);
            Ok::<_, io::Error>((replay, events))
        },
    );
    let runtime = Arc::clone(&state.runtime);
    tokio::spawn(async move {
        let _ = KeithAcpAgent::from_runtime(runtime)
            .serve_lines(outgoing, incoming, AcpTransport::HttpSse)
            .await;
    });

    (
        StatusCode::CREATED,
        axum::Json(CreatedConnection {
            connection_id: &connection_id,
            events: format!("/acp/sse/{connection_id}/events"),
            messages: format!("/acp/sse/{connection_id}/messages"),
        }),
    )
        .into_response()
}

async fn post_sse_message(
    State(state): State<ManagedState>,
    Path(connection_id): Path<String>,
    headers: HeaderMap,
    body: String,
) -> Response {
    if authenticate(&state, &headers).is_err() {
        return unauthorized();
    }
    if body.is_empty()
        || body.len() > MAX_MANAGED_FRAME_BYTES
        || serde_json::from_str::<serde_json::Value>(&body).is_err()
    {
        return (StatusCode::BAD_REQUEST, "invalid JSON-RPC frame").into_response();
    }
    let connections = state.connections.lock().await;
    let Some(connection) = connections.get(&connection_id) else {
        return StatusCode::NOT_FOUND.into_response();
    };
    if connection.ingress.send(body).is_err() {
        return (StatusCode::GONE, "connection is closed").into_response();
    }
    StatusCode::ACCEPTED.into_response()
}

async fn stream_sse_events(
    State(state): State<ManagedState>,
    Path(connection_id): Path<String>,
    Query(query): Query<ReplayQuery>,
    headers: HeaderMap,
) -> Response {
    if authenticate(&state, &headers).is_err() {
        return unauthorized();
    }
    let connections = state.connections.lock().await;
    let Some(connection) = connections.get(&connection_id) else {
        return StatusCode::NOT_FOUND.into_response();
    };
    let receiver = connection.events.subscribe();
    let replay = Arc::clone(&connection.replay);
    drop(connections);
    let cursor = query.after.or_else(|| {
        headers
            .get(HeaderName::from_static("last-event-id"))
            .and_then(|value| value.to_str().ok())
            .and_then(|value| value.parse().ok())
    });
    let retained = match replay
        .lock()
        .map_err(|_| AcpTransportError::Closed)
        .and_then(|replay| replay.replay_after(cursor.map(frame_id)))
    {
        Ok(frames) => frames,
        Err(AcpTransportError::ReplayCursor) => {
            return (StatusCode::BAD_REQUEST, "invalid replay cursor").into_response();
        }
        Err(_) => return StatusCode::GONE.into_response(),
    };
    let last_replayed = retained
        .last()
        .map_or(cursor.unwrap_or(0), |frame| frame.id.get());
    let replay_stream = stream::iter(retained.into_iter().map(Ok::<_, io::Error>));
    let live_stream = stream::unfold(
        (receiver, last_replayed),
        |(mut receiver, last_seen)| async move {
            loop {
                match receiver.recv().await {
                    Ok(frame) if frame.id.get() > last_seen => {
                        let next_seen = frame.id.get();
                        return Some((Ok::<_, io::Error>(frame), (receiver, next_seen)));
                    }
                    Ok(_) => {}
                    Err(broadcast::error::RecvError::Lagged(_)) => {
                        return Some((
                            Err(io::Error::other(
                                "ACP SSE consumer exceeded replay capacity",
                            )),
                            (receiver, last_seen),
                        ));
                    }
                    Err(broadcast::error::RecvError::Closed) => return None,
                }
            }
        },
    );
    let body = Body::from_stream(
        replay_stream
            .chain(live_stream)
            .map_ok(|frame| sse_event(&frame)),
    );
    let mut response = Response::new(body);
    response.headers_mut().insert(
        CONTENT_TYPE,
        HeaderValue::from_static("text/event-stream; charset=utf-8"),
    );
    response
        .headers_mut()
        .insert(CACHE_CONTROL, HeaderValue::from_static("no-store"));
    response
}

async fn close_sse(
    State(state): State<ManagedState>,
    Path(connection_id): Path<String>,
    headers: HeaderMap,
) -> Response {
    if authenticate(&state, &headers).is_err() {
        return unauthorized();
    }
    let Some(connection) = state.connections.lock().await.remove(&connection_id) else {
        return StatusCode::NOT_FOUND.into_response();
    };
    if let Ok(mut replay) = connection.replay.lock() {
        replay.close();
    }
    drop(connection);
    StatusCode::NO_CONTENT.into_response()
}

fn authenticate(state: &ManagedState, headers: &HeaderMap) -> Result<(), AcpTransportError> {
    let bearer = headers
        .get(AUTHORIZATION)
        .and_then(|value| value.to_str().ok())
        .and_then(|value| value.strip_prefix("Bearer "))
        .ok_or(AcpTransportError::Authentication)?;
    state.authenticator.authenticate(bearer)
}

fn unauthorized() -> Response {
    let mut response = StatusCode::UNAUTHORIZED.into_response();
    response.headers_mut().insert(
        WWW_AUTHENTICATE,
        HeaderValue::from_static("Bearer realm=\"keith-acp\""),
    );
    response
}

fn valid_connection_id(value: &str) -> bool {
    !value.is_empty()
        && value.len() <= 128
        && value
            .bytes()
            .all(|byte| byte.is_ascii_alphanumeric() || matches!(byte, b'-' | b'_'))
}

fn frame_id(value: u64) -> AcpTransportFrameId {
    AcpTransportFrameId::new(value)
}

fn sse_event(frame: &AcpTransportFrame) -> Bytes {
    Bytes::from(format!("id: {}\ndata: {}\n\n", frame.id.get(), frame.json))
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn managed_connection_ids_are_bounded_tokens() {
        assert!(valid_connection_id("connection_01"));
        assert!(!valid_connection_id("../connection"));
        assert!(!valid_connection_id(""));
    }
}
