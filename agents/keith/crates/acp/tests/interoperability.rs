use std::collections::BTreeSet;

use keith_acp::{
    AcpProtocolRouter, AcpProtocolVersion, AcpTransport, AcpTransportConnection, AcpTransportError,
};
use serde_json::{Value, json};

#[test]
fn published_acp_matrix_matches_stable_routing_and_all_transport_contracts() {
    let evidence: Value =
        serde_json::from_str(include_str!("../../../evidence/acp/qualification.json"))
            .expect("ACP qualification evidence");
    assert_eq!(evidence["schema_version"], 1);
    assert_eq!(evidence["stable_v1"]["wire_protocol_version"], 1);
    assert_eq!(evidence["official_rust_sdk"]["crate_version"], "2.0.0");
    assert_eq!(evidence["draft_v2"]["release_status"], "experimental");

    let stable = AcpProtocolRouter::new(false);
    let initialized = stable
        .route_initialize(
            r#"{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":1}}"#,
        )
        .expect("stable ACP v1 initialize");
    assert_eq!(initialized.version, AcpProtocolVersion::StableV1);
    let rejected = stable
        .route_initialize(
            r#"{"jsonrpc":"2.0","id":2,"method":"initialize","params":{"protocolVersion":2}}"#,
        )
        .expect_err("draft v2 is runtime-gated");
    assert_eq!(rejected.code, -32602);
    assert!(rejected.reason.contains("no downgrade"));

    #[cfg(feature = "unstable-acp-v2")]
    assert_eq!(
        AcpProtocolRouter::new(true)
            .route_initialize(
                r#"{"jsonrpc":"2.0","id":3,"method":"initialize","params":{"protocolVersion":2}}"#,
            )
            .expect("explicit draft v2 route")
            .version,
        AcpProtocolVersion::DraftV2
    );

    for transport in [
        AcpTransport::Stdio,
        AcpTransport::HttpSse,
        AcpTransport::WebSocket,
    ] {
        let mut connection =
            AcpTransportConnection::new(transport, 2_048, 4).expect("bounded ACP transport");
        let first = connection
            .publish(
                json!({
                    "jsonrpc": "2.0",
                    "id": 1,
                    "method": "initialize",
                    "params": { "protocolVersion": 1 }
                })
                .to_string(),
            )
            .expect("publish initialization");
        assert_eq!(connection.replay_after(None).unwrap(), vec![first.clone()]);
        assert!(connection.replay_after(Some(first.id)).unwrap().is_empty());
        connection.close();
        assert_eq!(
            connection.publish(json!({ "jsonrpc": "2.0", "id": 2, "result": {} }).to_string()),
            Err(AcpTransportError::Closed)
        );
    }

    let required = [
        "initialize",
        "session/new",
        "session/load",
        "session/fork",
        "session/prompt",
        "session/cancel",
        "session/update",
        "fs/read_text_file",
        "fs/write_text_file",
        "terminal/create",
        "session/request_permission",
        "mcp/session_scope",
    ]
    .into_iter()
    .collect::<BTreeSet<_>>();
    let recorded = evidence["stable_v1"]["operation_trace"]
        .as_array()
        .expect("operation trace")
        .iter()
        .map(|operation| operation["operation"].as_str().expect("operation name"))
        .collect::<BTreeSet<_>>();
    assert!(required.is_subset(&recorded));

    let encoded = serde_json::to_string(&evidence).expect("serializable evidence");
    for forbidden in ["Bearer ", "access_token", "authorization_value"] {
        assert!(!encoded.contains(forbidden));
    }
}
