use keith_agent_types::ProfileId;
use keith_channel_adapters::email::{EmailAdapter, EmailConfig, EmailCursor};
use keith_channel_adapters::google_chat::{GoogleChatAdapter, GoogleChatConfig, GoogleChatCursor};
use keith_channel_adapters::matrix::{MatrixAdapter, MatrixConfig, MatrixCursor};
use keith_channel_adapters::slack::{SlackAdapter, SlackConfig, SlackCursor};
use keith_channel_adapters::teams::{TeamsAdapter, TeamsConfig, TeamsCursor};
use keith_channel_adapters::telegram::{
    TelegramAdapter, TelegramConfig, TelegramCursor, TelegramIngress,
};
use keith_channel_adapters::whatsapp::{WhatsAppCloudAdapter, WhatsAppCloudConfig, WhatsAppCursor};
use keith_channel_adapters::{
    ChannelAdapterCatalog, ChannelAdapterKind, DiscordAdapter, DiscordConfig, DiscordCursor,
    ManagedChannelAdapter,
};
use keith_channel_core::ChannelConformanceV2;
use keith_credentials::{CredentialOwner, CredentialRef, SecretValue};

fn secret(value: &str) -> SecretValue {
    SecretValue::new(value.as_bytes().to_vec()).expect("test secret")
}

fn credential(account: &str, name: &str) -> CredentialRef {
    CredentialRef::new(name, CredentialOwner::Channel(account.to_owned()))
        .expect("credential reference")
}

fn register_conformant(catalog: &mut ChannelAdapterCatalog, adapter: &dyn ManagedChannelAdapter) {
    let setup = adapter.account_setup();
    setup.validate().expect("valid shared account setup");
    let capabilities = adapter.capabilities_v2();
    capabilities
        .validate()
        .expect("complete shared capability contract");
    if let Some(cursor) = adapter.reconnect_cursor_v2() {
        ChannelConformanceV2::admit_cursor(&cursor, cursor.observed_at, 1)
            .expect("current adapter cursor");
    }
    catalog
        .register_adapter(adapter)
        .expect("shared catalog registration");
}

#[test]
#[allow(clippy::too_many_lines)]
fn built_in_catalog_registers_every_real_adapter_with_its_exact_v2_contract() {
    let mut catalog = ChannelAdapterCatalog::built_in();
    assert_eq!(catalog.definitions().count(), ChannelAdapterKind::ALL.len());
    assert_eq!(
        catalog
            .definitions()
            .map(|definition| definition.kind)
            .collect::<Vec<_>>(),
        ChannelAdapterKind::ALL
    );

    let discord = DiscordAdapter::new(
        DiscordConfig::production("discord-1", 1 << 9),
        secret("discord-token"),
        DiscordCursor::default(),
    )
    .expect("Discord adapter");
    register_conformant(&mut catalog, &discord);

    let slack = SlackAdapter::new(
        SlackConfig::production("slack-1", "slack-bot"),
        secret("xoxb-token"),
        None,
        secret("slack-signing"),
        SlackCursor::default(),
    )
    .expect("Slack adapter");
    register_conformant(&mut catalog, &slack);

    let telegram = TelegramAdapter::new(
        TelegramConfig::production("123", TelegramIngress::Webhook),
        secret("123:telegram-token"),
        secret("telegram_webhook"),
        TelegramCursor::default(),
    )
    .expect("Telegram adapter");
    register_conformant(&mut catalog, &telegram);

    let whatsapp = WhatsAppCloudAdapter::new(
        WhatsAppCloudConfig::production("v22.0", "444", "555"),
        secret("whatsapp-access"),
        secret("whatsapp-app"),
        secret("whatsapp-verify"),
        WhatsAppCursor::default(),
    )
    .expect("WhatsApp adapter");
    register_conformant(&mut catalog, &whatsapp);

    let profile = ProfileId::new();
    let teams = TeamsAdapter::new(
        TeamsConfig::production(
            "teams-app",
            "teams-1",
            profile.clone(),
            credential("teams-1", "teams-access"),
        ),
        secret("teams-access"),
        TeamsCursor::default(),
    )
    .expect("Teams adapter");
    register_conformant(&mut catalog, &teams);

    let google = GoogleChatAdapter::new(
        GoogleChatConfig::production(
            "https://keith.example/chat",
            "google-1",
            profile.clone(),
            credential("google-1", "google-access"),
        ),
        secret("google-access"),
        GoogleChatCursor::default(),
    )
    .expect("Google Chat adapter");
    register_conformant(&mut catalog, &google);

    let email = EmailAdapter::new(
        EmailConfig::provider(
            "mail-provider",
            "https://mail.example",
            "https://keith.example/mail",
            "email-1",
            "keith@example.com",
            profile.clone(),
            credential("email-1", "email-access"),
        ),
        secret("email-access"),
        EmailCursor::default(),
    )
    .expect("email adapter");
    register_conformant(&mut catalog, &email);

    let matrix = MatrixAdapter::new(
        MatrixConfig::production(
            "https://matrix.example",
            "matrix-1",
            "@keith:example.com",
            profile,
            credential("matrix-1", "matrix-access"),
        ),
        secret("matrix-access"),
        MatrixCursor::default(),
    )
    .expect("Matrix adapter");
    register_conformant(&mut catalog, &matrix);

    assert_eq!(catalog.accounts().count(), 8);
    for registration in catalog.accounts() {
        registration
            .capabilities
            .validate()
            .expect("complete capability declaration");
        registration.setup.validate().expect("valid account setup");
        assert_eq!(registration.account_id, registration.setup.account_id);
    }
}
