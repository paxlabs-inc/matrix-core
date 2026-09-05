use std::collections::BTreeSet;

use keith_channel_adapters::{ChannelAdapterCatalog, ChannelAdapterKind};
use serde_json::Value;

#[test]
fn published_qualification_matrix_traces_every_adapter_without_claiming_blocked_accounts() {
    let evidence: Value = serde_json::from_str(include_str!(
        "../../../evidence/channels/qualification.json"
    ))
    .expect("channel qualification evidence");
    assert_eq!(evidence["schema_version"], 1);
    assert_eq!(
        evidence["qualification_result"],
        "local_conformance_passed_external_accounts_blocked"
    );
    assert_eq!(
        evidence["credential_inventory"]["secret_values_read"],
        false
    );

    let platforms = evidence["platforms"]
        .as_array()
        .expect("platform evidence array");
    let published = platforms
        .iter()
        .map(|platform| {
            platform["platform"]
                .as_str()
                .expect("platform identity")
                .to_owned()
        })
        .collect::<BTreeSet<_>>();
    let expected = ChannelAdapterKind::ALL
        .into_iter()
        .map(|kind| kind.channel().to_owned())
        .collect::<BTreeSet<_>>();
    assert_eq!(published, expected);

    let catalog = ChannelAdapterCatalog::built_in();
    for platform in platforms {
        assert_eq!(
            platform["real_account_status"],
            "blocked_external_credentials_and_owner_authorization"
        );
        assert!(
            !platform["external_blockers"]
                .as_array()
                .expect("external blockers")
                .is_empty()
        );
        assert!(
            !platform["unsupported_capabilities"]
                .as_array()
                .expect("unsupported capability list")
                .is_empty()
        );
        let kind = ChannelAdapterKind::ALL
            .into_iter()
            .find(|kind| kind.channel() == platform["platform"].as_str().unwrap_or_default())
            .expect("known adapter kind");
        let definition = catalog.definition(kind).expect("built-in definition");
        let recorded = platform["required_credential_references"]
            .as_array()
            .expect("required credential references")
            .iter()
            .map(|name| name.as_str().expect("credential name").to_owned())
            .collect::<BTreeSet<_>>();
        assert_eq!(recorded, definition.required_credential_names);
        assert!(
            platform["implementation"]
                .as_str()
                .is_some_and(|path| path.starts_with("crates/channel-adapters/src/"))
        );
    }

    let encoded = serde_json::to_string(&evidence).expect("serializable evidence");
    for forbidden in ["xoxb-", "Bearer ", "bot_token_value", "access_token_value"] {
        assert!(!encoded.contains(forbidden));
    }
}
