use std::collections::VecDeque;

use serde::{Deserialize, Serialize};
use sha2::{Digest, Sha256};
use thiserror::Error;

#[derive(Clone, Copy, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum AcpTransport {
    Stdio,
    HttpSse,
    WebSocket,
}

#[derive(Clone, Copy, Debug, Eq, Ord, PartialEq, PartialOrd, Serialize, Deserialize)]
#[serde(transparent)]
pub struct AcpTransportFrameId(u64);

impl AcpTransportFrameId {
    #[must_use]
    pub const fn new(value: u64) -> Self {
        Self(value)
    }

    #[must_use]
    pub const fn get(self) -> u64 {
        self.0
    }
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct AcpTransportFrame {
    pub id: AcpTransportFrameId,
    pub json: String,
}

#[derive(Debug, Error, Eq, PartialEq)]
pub enum AcpTransportError {
    #[error("ACP managed transport authentication failed")]
    Authentication,
    #[error("ACP managed transport configuration is invalid")]
    Configuration,
    #[error("ACP managed transport frame is invalid or exceeds its bound")]
    Frame,
    #[error("ACP managed transport connection is closed")]
    Closed,
    #[error("ACP managed transport replay cursor is outside the retained connection window")]
    ReplayCursor,
}

#[derive(Clone)]
pub struct AcpTransportAuthenticator {
    token_digest: [u8; 32],
}

impl AcpTransportAuthenticator {
    /// Hashes a managed endpoint bearer without retaining its reusable plaintext.
    ///
    /// # Errors
    ///
    /// Returns an error for short or control-containing tokens.
    pub fn new(token: &str) -> Result<Self, AcpTransportError> {
        if token.len() < 32 || token.bytes().any(|byte| byte.is_ascii_control()) {
            return Err(AcpTransportError::Configuration);
        }
        Ok(Self {
            token_digest: Sha256::digest(token.as_bytes()).into(),
        })
    }

    /// Authenticates one exact bearer value using a full constant-work digest comparison.
    ///
    /// # Errors
    ///
    /// Returns an authentication error without revealing why a candidate differed.
    pub fn authenticate(&self, candidate: &str) -> Result<(), AcpTransportError> {
        let candidate: [u8; 32] = Sha256::digest(candidate.as_bytes()).into();
        let different = self
            .token_digest
            .iter()
            .zip(candidate)
            .fold(0_u8, |difference, (expected, actual)| {
                difference | (expected ^ actual)
            });
        if different == 0 {
            Ok(())
        } else {
            Err(AcpTransportError::Authentication)
        }
    }
}

pub struct AcpTransportConnection {
    transport: AcpTransport,
    max_frame_bytes: usize,
    max_replay_frames: usize,
    next_frame_id: u64,
    replay: VecDeque<AcpTransportFrame>,
    closed: bool,
}

impl AcpTransportConnection {
    /// Creates one bounded physical connection state.
    ///
    /// # Errors
    ///
    /// Returns an error for zero frame or replay bounds.
    pub fn new(
        transport: AcpTransport,
        max_frame_bytes: usize,
        max_replay_frames: usize,
    ) -> Result<Self, AcpTransportError> {
        if max_frame_bytes == 0 || max_replay_frames == 0 {
            return Err(AcpTransportError::Configuration);
        }
        Ok(Self {
            transport,
            max_frame_bytes,
            max_replay_frames,
            next_frame_id: 1,
            replay: VecDeque::new(),
            closed: false,
        })
    }

    #[must_use]
    pub const fn transport(&self) -> AcpTransport {
        self.transport
    }

    /// Retains one exact JSON-RPC single or batch frame for managed reconnect.
    ///
    /// # Errors
    ///
    /// Returns an error for closure, malformed/empty JSON-RPC, or a byte-limit violation.
    pub fn publish(&mut self, json: String) -> Result<AcpTransportFrame, AcpTransportError> {
        if self.closed {
            return Err(AcpTransportError::Closed);
        }
        if json.is_empty() || json.len() > self.max_frame_bytes {
            return Err(AcpTransportError::Frame);
        }
        let value: serde_json::Value =
            serde_json::from_str(&json).map_err(|_| AcpTransportError::Frame)?;
        let valid = value.as_object().is_some_and(valid_json_rpc_object)
            || value.as_array().is_some_and(|entries| {
                !entries.is_empty()
                    && entries
                        .iter()
                        .all(|entry| entry.as_object().is_some_and(valid_json_rpc_object))
            });
        if !valid {
            return Err(AcpTransportError::Frame);
        }
        let frame = AcpTransportFrame {
            id: AcpTransportFrameId::new(self.next_frame_id),
            json,
        };
        self.next_frame_id = self
            .next_frame_id
            .checked_add(1)
            .ok_or(AcpTransportError::Frame)?;
        self.replay.push_back(frame.clone());
        while self.replay.len() > self.max_replay_frames {
            self.replay.pop_front();
        }
        Ok(frame)
    }

    /// Returns retained frames strictly newer than a reconnect cursor.
    ///
    /// # Errors
    ///
    /// Returns an error when the cursor is ahead of publication or behind retained history.
    pub fn replay_after(
        &self,
        cursor: Option<AcpTransportFrameId>,
    ) -> Result<Vec<AcpTransportFrame>, AcpTransportError> {
        let after = cursor.map_or(0, AcpTransportFrameId::get);
        if let Some(cursor) = cursor
            && (cursor.get() >= self.next_frame_id
                || self.replay.front().is_some_and(|first| {
                    cursor
                        .get()
                        .checked_add(1)
                        .is_none_or(|next| next < first.id.get())
                }))
        {
            return Err(AcpTransportError::ReplayCursor);
        }
        Ok(self
            .replay
            .iter()
            .filter(|frame| frame.id.get() > after)
            .cloned()
            .collect())
    }

    pub fn close(&mut self) {
        self.closed = true;
    }

    #[must_use]
    pub const fn is_closed(&self) -> bool {
        self.closed
    }
}

fn valid_json_rpc_object(object: &serde_json::Map<String, serde_json::Value>) -> bool {
    if object.get("jsonrpc").and_then(serde_json::Value::as_str) != Some("2.0") {
        return false;
    }
    let request = object
        .get("method")
        .and_then(serde_json::Value::as_str)
        .is_some();
    let response =
        object.contains_key("id") && (object.contains_key("result") ^ object.contains_key("error"));
    request ^ response
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn authentication_replay_batch_and_closure_have_one_bounded_contract() {
        let auth =
            AcpTransportAuthenticator::new("managed-transport-token-at-least-thirty-two-bytes")
                .unwrap();
        auth.authenticate("managed-transport-token-at-least-thirty-two-bytes")
            .unwrap();
        assert_eq!(
            auth.authenticate("managed-transport-token-at-least-thirty-two-bytez"),
            Err(AcpTransportError::Authentication)
        );

        let mut connection = AcpTransportConnection::new(AcpTransport::HttpSse, 1024, 2).unwrap();
        let first = connection
            .publish(r#"{"jsonrpc":"2.0","id":1,"result":{}}"#.to_owned())
            .unwrap();
        let second = connection
            .publish(
                r#"[{"jsonrpc":"2.0","id":2,"result":{}},{"jsonrpc":"2.0","method":"session/cancel","params":{"sessionId":"s"}}]"#
                    .to_owned(),
            )
            .unwrap();
        let third = connection
            .publish(r#"{"jsonrpc":"2.0","id":3,"result":{}}"#.to_owned())
            .unwrap();
        assert_eq!(
            connection.replay_after(Some(first.id)).unwrap(),
            vec![second, third]
        );
        assert_eq!(
            connection.publish("{}".to_owned()),
            Err(AcpTransportError::Frame)
        );
        connection
            .publish(r#"{"jsonrpc":"2.0","id":4,"result":{}}"#.to_owned())
            .unwrap();
        assert_eq!(
            connection.replay_after(Some(first.id)),
            Err(AcpTransportError::ReplayCursor)
        );
        connection.close();
        assert_eq!(
            connection.publish("{}".to_owned()),
            Err(AcpTransportError::Closed)
        );
    }
}
