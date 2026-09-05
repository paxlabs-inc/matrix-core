use serde_json::{Value, json};

use crate::BridgeError;

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum AcpProtocolVersion {
    StableV1,
    DraftV2,
}

impl AcpProtocolVersion {
    #[must_use]
    pub const fn wire(self) -> u16 {
        match self {
            Self::StableV1 => 1,
            Self::DraftV2 => 2,
        }
    }
}

#[derive(Clone, Debug, PartialEq)]
pub struct AcpProtocolRoute {
    pub version: AcpProtocolVersion,
    pub request_id: Value,
}

#[derive(Clone, Debug, Eq, PartialEq)]
pub struct AcpProtocolRouteError {
    pub code: i32,
    pub reason: Box<str>,
    pub request_id: Box<Value>,
    pub supported_versions: &'static [u16],
}

impl AcpProtocolRouteError {
    #[must_use]
    pub fn response(&self) -> Value {
        json!({
            "jsonrpc": "2.0",
            "id": &self.request_id,
            "error": {
                "code": self.code,
                "message": self.reason,
                "data": {
                    "reason": "unsupported_or_invalid_protocol_version",
                    "supported": self.supported_versions,
                }
            }
        })
    }
}

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub struct AcpProtocolRouter {
    draft_v2_runtime_enabled: bool,
}

impl AcpProtocolRouter {
    #[must_use]
    pub const fn new(draft_v2_runtime_enabled: bool) -> Self {
        Self {
            draft_v2_runtime_enabled,
        }
    }

    #[must_use]
    pub const fn supported_versions() -> &'static [u16] {
        if cfg!(feature = "unstable-acp-v2") {
            &[1, 2]
        } else {
            &[1]
        }
    }

    #[must_use]
    pub const fn enabled_versions(&self) -> &'static [u16] {
        if cfg!(feature = "unstable-acp-v2") && self.draft_v2_runtime_enabled {
            &[1, 2]
        } else {
            &[1]
        }
    }

    fn route_error(
        self,
        code: i32,
        reason: impl Into<Box<str>>,
        request_id: Value,
    ) -> AcpProtocolRouteError {
        AcpProtocolRouteError {
            code,
            reason: reason.into(),
            request_id: Box::new(request_id),
            supported_versions: self.enabled_versions(),
        }
    }

    /// Routes an untouched first JSON-RPC frame to an exact, separately compiled handler.
    ///
    /// # Errors
    ///
    /// Returns a JSON-RPC-shaped error for malformed input, batched/non-initialize first frames,
    /// unsupported versions, or draft v2 without both compile-time and runtime gates.
    pub fn route_initialize(&self, line: &str) -> Result<AcpProtocolRoute, AcpProtocolRouteError> {
        let value: Value = serde_json::from_str(line).map_err(|error| {
            self.route_error(-32700, format!("invalid ACP JSON: {error}"), Value::Null)
        })?;
        let object = value.as_object().ok_or_else(|| {
            self.route_error(
                -32600,
                "the first ACP frame must be one initialize request, never a batch",
                Value::Null,
            )
        })?;
        let request_id = object.get("id").cloned().unwrap_or(Value::Null);
        if object.get("jsonrpc").and_then(Value::as_str) != Some("2.0")
            || object.get("method").and_then(Value::as_str) != Some("initialize")
            || request_id.is_null()
        {
            return Err(self.route_error(
                -32600,
                "the first ACP frame must be an initialize request with an id",
                request_id,
            ));
        }
        let version = object
            .get("params")
            .and_then(Value::as_object)
            .and_then(|params| params.get("protocolVersion"))
            .and_then(Value::as_u64)
            .and_then(|version| u16::try_from(version).ok())
            .ok_or_else(|| {
                self.route_error(
                    -32602,
                    "initialize.protocolVersion must be an exact integer version",
                    request_id.clone(),
                )
            })?;
        let version = match version {
            1 => AcpProtocolVersion::StableV1,
            2 if cfg!(feature = "unstable-acp-v2") && self.draft_v2_runtime_enabled => {
                AcpProtocolVersion::DraftV2
            }
            requested => {
                return Err(self.route_error(
                    -32602,
                    format!(
                        "ACP protocol version {requested} is not enabled on this endpoint; no downgrade is performed"
                    ),
                    request_id,
                ));
            }
        };
        Ok(AcpProtocolRoute {
            version,
            request_id,
        })
    }

    /// Checks a durable session's exact protocol binding before continuation.
    ///
    /// # Errors
    ///
    /// Returns an error when a session created by one version is presented to another.
    pub fn admit_session_version(
        &self,
        stored: Option<u16>,
        current: AcpProtocolVersion,
    ) -> Result<u16, BridgeError> {
        if let Some(stored) = stored
            && stored != current.wire()
        {
            return Err(BridgeError::ProtocolVersion(format!(
                "session belongs to ACP v{stored}, not ACP v{}",
                current.wire()
            )));
        }
        Ok(current.wire())
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn router_never_downgrades_or_converts_an_initialize_frame() {
        let router = AcpProtocolRouter::new(false);
        assert_eq!(
            router
                .route_initialize(
                    r#"{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":1}}"#,
                )
                .unwrap()
                .version,
            AcpProtocolVersion::StableV1
        );
        let v2 = router
            .route_initialize(
                r#"{"jsonrpc":"2.0","id":2,"method":"initialize","params":{"protocolVersion":2}}"#,
            )
            .unwrap_err();
        assert_eq!(v2.code, -32602);
        assert!(v2.reason.contains("no downgrade"));
        assert_eq!(v2.response()["error"]["data"]["supported"], json!([1]));
        assert!(router
            .route_initialize(
                r#"{"jsonrpc":"2.0","id":3,"method":"initialize","params":{"protocolVersion":3}}"#,
            )
            .is_err());
    }

    #[test]
    fn continuation_is_bound_to_its_original_protocol_version() {
        let router = AcpProtocolRouter::new(cfg!(feature = "unstable-acp-v2"));
        assert_eq!(
            router
                .admit_session_version(None, AcpProtocolVersion::StableV1)
                .unwrap(),
            1
        );
        assert!(
            router
                .admit_session_version(Some(1), AcpProtocolVersion::DraftV2)
                .is_err()
        );
    }
}
