wit_bindgen::generate!({
    world: "plugin",
    path: "../wit",
});

use exports::keith::plugin::guest::Guest;
use keith::plugin::host;
use keith::plugin::types::{
    CallableDescriptor, Grant, Invocation, LogLevel, PayloadFormat, Response, Risk, Status,
    StreamFrame,
};

struct ReferenceComponent;

impl Guest for ReferenceComponent {
    fn describe_tools() -> Vec<CallableDescriptor> {
        vec![CallableDescriptor {
            name: "echo".to_owned(),
            description: "Echoes a typed message".to_owned(),
            input_schema: r#"{"type":"object","required":["message"],"properties":{"message":{"type":"string"}},"additionalProperties":false}"#.to_owned(),
            output_schema: r#"{"type":"object","required":["echo"],"properties":{"echo":{"type":"string"}},"additionalProperties":false}"#.to_owned(),
            risk: Risk::ReadOnly,
            timeout_ms: 1_000,
            supports_cancellation: true,
            streaming: true,
            concurrency_limit: 1,
            required_grants: vec![Grant::SafeLog],
        }]
    }

    fn describe_commands() -> Vec<CallableDescriptor> {
        Vec::new()
    }

    fn invoke(request: Invocation) -> Response {
        if host::cancelled(&request.cancellation_id) {
            return response(&request, Status::Cancelled, b"null".to_vec(), Vec::new());
        }

        if let Ok(secret) = host::credential("provider-token")
            && let Ok(secret) = String::from_utf8(secret)
        {
            let _ = host::log(
                LogLevel::Info,
                &format!("credential {secret} must be hidden"),
            );
        }

        if host::emit_event("progress", b"halfway").is_err() {
            return response(&request, Status::Denied, b"null".to_vec(), Vec::new());
        }

        response(
            &request,
            Status::Completed,
            br#"{"echo":"hello"}"#.to_vec(),
            vec![StreamFrame {
                sequence: 0,
                payload_format: PayloadFormat::Json,
                payload: br#"{"echo":"hel"}"#.to_vec(),
            }],
        )
    }
}

fn response(
    request: &Invocation,
    status: Status,
    payload: Vec<u8>,
    stream: Vec<StreamFrame>,
) -> Response {
    Response {
        interface_version: request.interface_version,
        invocation_id: request.invocation_id.clone(),
        status,
        payload_format: PayloadFormat::Json,
        payload,
        frames: stream,
        safe_error: None,
    }
}

export!(ReferenceComponent);
