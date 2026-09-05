use std::collections::{BTreeMap, BTreeSet};

use keith_agent_types::{ProfileId, UtcTimestamp};
use keith_cua::{
    AccessibilityNode, ActionTarget, ComputerAction, ComputerObservation, DomSnapshot,
    FocusedWindow, FrameId, NamedCredentialGrant, Point, Screenshot, SemanticTarget, Viewport,
};
use keith_platform_contracts::{ComputerSessionId, ControlOwner, RedactedText};
use keith_skills::{SkillLimits, SkillRegistry, SkillRoots, SkillSelectionRequest};
use keith_task_recipe::{
    CaptureLimits, CaptureSanitizer, MediaSanitization, RecipeCompiler, RecipeCorrection,
    RecipePublicationMode, RecipePublisher, RecipeReplay, RecipeReplayMode, RecipeReplayState,
    RecipeTarget, ReplayComputerCommand, ReplayInputValue, RetentionPolicy,
    SkillPublicationOptions, StoreLimits, TaskRecipeStore,
};
use keith_workspace::{PersonalWorkspace, PersonalWorkspaceLimits};
use tempfile::tempdir;

fn at(value: i64) -> UtcTimestamp {
    UtcTimestamp::from_unix_millis(value)
}

fn observation(
    profile_id: &ProfileId,
    computer_session_id: &ComputerSessionId,
    millis: i64,
    url: &str,
    frame_digest: &str,
    accessibility: Vec<AccessibilityNode>,
) -> ComputerObservation {
    let frame_id = FrameId::new();
    ComputerObservation {
        computer_session_id: computer_session_id.clone(),
        profile_id: profile_id.clone(),
        captured_at: at(millis),
        screenshot: Screenshot {
            frame_id: frame_id.clone(),
            content_digest: frame_digest.into(),
            media_type: "image/png".into(),
            base64_data: "not-persisted-by-the-recorder".into(),
            width: 1_280,
            height: 720,
        },
        dom: Some(DomSnapshot {
            frame_id,
            url: url.into(),
            title: "Example".into(),
            html: format!("<main>{url}</main>"),
        }),
        accessibility,
        focused_window: Some(FocusedWindow {
            title: "Example".into(),
            application: "Chromium".into(),
            window_id: "window-1".into(),
        }),
        url: Some(url.into()),
        viewport: Viewport::default(),
        cursor: Point { x: 640, y: 360 },
        dialogs: Vec::new(),
        downloads: Vec::new(),
        applications: Vec::new(),
        recent_actions: Vec::new(),
    }
}

fn login_nodes(submit_name: &str) -> Vec<AccessibilityNode> {
    vec![
        AccessibilityNode {
            role: "textbox".into(),
            name: "Password".into(),
            value: None,
            disabled: false,
            focused: true,
        },
        AccessibilityNode {
            role: "button".into(),
            name: submit_name.into(),
            value: None,
            disabled: false,
            focused: false,
        },
    ]
}

fn dashboard_nodes() -> Vec<AccessibilityNode> {
    vec![AccessibilityNode {
        role: "heading".into(),
        name: "Welcome".into(),
        value: None,
        disabled: false,
        focused: false,
    }]
}

fn replay_inputs() -> BTreeMap<String, ReplayInputValue> {
    BTreeMap::from([(
        "example-password".into(),
        ReplayInputValue::CredentialName("example-password".into()),
    )])
}

fn complete_replay(
    recipe: &keith_task_recipe::TaskRecipe,
    profile_id: &ProfileId,
    computer_session_id: &ComputerSessionId,
    login: &ComputerObservation,
    dashboard: &ComputerObservation,
) -> RecipeReplay {
    let mut replay = RecipeReplay::start(
        recipe,
        profile_id.clone(),
        computer_session_id.clone(),
        RecipeReplayMode::Shadow,
        replay_inputs(),
    )
    .unwrap();
    let credential = replay.prepare_next(login).unwrap().unwrap();
    assert!(matches!(
        credential.commands.as_slice(),
        [ReplayComputerCommand::NamedCredentialFill { parameter_name, .. }]
            if parameter_name == "example-password"
    ));
    replay.observe_result(login).unwrap();
    let submit = replay.prepare_next(login).unwrap().unwrap();
    assert!(matches!(
        submit.commands.as_slice(),
        [ReplayComputerCommand::Computer(
            ComputerAction::Click { .. }
        )]
    ));
    replay.observe_result(dashboard).unwrap();
    assert!(replay.prepare_next(dashboard).unwrap().is_none());
    assert_eq!(replay.state(), RecipeReplayState::Passed);
    replay
}

fn skill_registry(now: UtcTimestamp) -> (tempfile::TempDir, SkillRegistry) {
    let root = tempdir().unwrap();
    let workspace = PersonalWorkspace::open(
        root.path().join("workspace"),
        PersonalWorkspaceLimits::default(),
        now,
    )
    .unwrap();
    let registry = SkillRegistry::open(
        workspace,
        SkillRoots {
            built_in: root.path().join("built-in"),
            global: root.path().join("global"),
            project: root.path().join("project"),
        },
        SkillLimits::default(),
    )
    .unwrap();
    (root, registry)
}

#[test]
#[allow(clippy::too_many_lines)]
fn real_cua_recording_changed_layout_correction_replay_and_skill_invocation() {
    let root = tempdir().unwrap();
    let store = TaskRecipeStore::open(root.path(), StoreLimits::default()).unwrap();
    let profile_id = ProfileId::new();
    let computer_session_id = ComputerSessionId::new();
    let initial = observation(
        &profile_id,
        &computer_session_id,
        1_000,
        "https://example.test/login",
        &"1".repeat(64),
        login_nodes("Submit"),
    );
    let mut recorder = keith_task_recipe::DemonstrationRecorder::start(
        &store,
        &initial,
        b"sanitized-login-frame",
        MediaSanitization::SensitiveRegionsRedacted,
        ControlOwner::UserControl,
        "Sign in to Example",
        RetentionPolicy {
            retain_until: at(100_000),
        },
        CaptureLimits::default(),
        CaptureSanitizer::default(),
    )
    .unwrap();

    let credential_observation = observation(
        &profile_id,
        &computer_session_id,
        1_010,
        "https://example.test/login",
        &"2".repeat(64),
        login_nodes("Submit"),
    );
    recorder
        .record_cua_action(
            &store,
            &credential_observation,
            b"sanitized-credential-frame",
            MediaSanitization::SensitiveRegionsRedacted,
            &ComputerAction::CredentialFill {
                grant: NamedCredentialGrant {
                    grant_name: RedactedText::parse("example-password").unwrap(),
                    opaque_handle: RedactedText::parse("credential-handle-1").unwrap(),
                    profile_id: profile_id.clone(),
                    allowed_origin: RedactedText::parse("https://example.test/").unwrap(),
                    expires_at: at(50_000),
                },
                target: SemanticTarget::Accessibility {
                    role: "textbox".into(),
                    name: "Password".into(),
                },
            },
            ControlOwner::UserControl,
        )
        .unwrap();

    let submit_observation = observation(
        &profile_id,
        &computer_session_id,
        1_020,
        "https://example.test/login",
        &"3".repeat(64),
        login_nodes("Submit"),
    );
    recorder
        .record_cua_action(
            &store,
            &submit_observation,
            b"sanitized-submit-frame",
            MediaSanitization::SensitiveRegionsRedacted,
            &ComputerAction::Click {
                target: ActionTarget::Semantic {
                    target: SemanticTarget::Accessibility {
                        role: "button".into(),
                        name: "Submit".into(),
                    },
                },
                button: keith_cua::MouseButton::Left,
            },
            ControlOwner::UserControl,
        )
        .unwrap();
    let dashboard = observation(
        &profile_id,
        &computer_session_id,
        1_030,
        "https://example.test/dashboard",
        &"4".repeat(64),
        dashboard_nodes(),
    );
    recorder
        .record_observation(
            &store,
            &dashboard,
            b"sanitized-dashboard-frame",
            MediaSanitization::NoSensitiveContent,
            ControlOwner::UserControl,
        )
        .unwrap();
    recorder.complete(&store, at(1_040)).unwrap();
    let encoded = serde_json::to_string(recorder.demonstration()).unwrap();
    assert!(!encoded.contains("credential-handle-1"));
    assert!(!encoded.contains("not-persisted-by-the-recorder"));
    assert!(encoded.contains("example-password"));

    let mut history = RecipeCompiler::default()
        .compile(recorder.demonstration(), at(2_000))
        .unwrap();
    store.save_recipe_history(&history).unwrap();
    assert_eq!(history.active().unwrap().steps.len(), 2);
    assert_eq!(history.active().unwrap().inputs.len(), 1);

    let changed_login = observation(
        &profile_id,
        &computer_session_id,
        2_010,
        "https://example.test/login",
        &"5".repeat(64),
        login_nodes("Continue"),
    );
    let original = history.active().unwrap().clone();
    let mut failed = RecipeReplay::start(
        &original,
        profile_id.clone(),
        computer_session_id.clone(),
        RecipeReplayMode::Shadow,
        replay_inputs(),
    )
    .unwrap();
    failed.prepare_next(&changed_login).unwrap().unwrap();
    failed.observe_result(&changed_login).unwrap();
    assert!(failed.prepare_next(&changed_login).unwrap().is_none());
    assert_eq!(failed.state(), RecipeReplayState::Failed);
    let suggested = failed.last_comparison().unwrap().suggested_targets[0].clone();
    assert_eq!(
        suggested.accessible_name,
        Some(keith_task_recipe::TemplateValue::Literal("Continue".into()))
    );

    history
        .apply_correction(
            RecipeCorrection::CorrectTarget {
                step_id: "step-2".into(),
                target: RecipeTarget {
                    semantic: suggested,
                    visual_fallback: None,
                },
            },
            at(2_020),
        )
        .unwrap();
    let corrected = history.active().unwrap().clone();
    let completed = complete_replay(
        &corrected,
        &profile_id,
        &computer_session_id,
        &changed_login,
        &dashboard,
    );
    completed
        .record_qualification(&mut history, "changed-layout-recovery")
        .unwrap();
    completed
        .record_qualification(&mut history, "shadow-replay")
        .unwrap();
    history
        .active_mut()
        .unwrap()
        .qualification
        .accept(at(2_030))
        .unwrap();

    let (_registry_root, registry) = skill_registry(at(3_000));
    let package = RecipePublisher::publish(
        &mut history,
        &registry,
        SkillPublicationOptions {
            skill_id: "example-sign-in".into(),
            triggers: vec!["sign in to example".into()],
            required_tools: vec!["computer".into()],
            platforms: vec!["linux".into()],
        },
        RecipePublicationMode::Install,
        at(3_010),
    )
    .unwrap();
    assert_eq!(
        history
            .active()
            .unwrap()
            .published
            .as_ref()
            .unwrap()
            .skill_digest,
        package.provenance.digest
    );
    let retried = RecipePublisher::publish(
        &mut history,
        &registry,
        SkillPublicationOptions {
            skill_id: "example-sign-in".into(),
            triggers: vec!["sign in to example".into()],
            required_tools: vec!["computer".into()],
            platforms: vec!["linux".into()],
        },
        RecipePublicationMode::Install,
        at(3_011),
    )
    .unwrap();
    assert_eq!(retried.provenance.digest, package.provenance.digest);
    store.save_recipe_history(&history).unwrap();
    let invoked = registry
        .select(
            &SkillSelectionRequest {
                task: "Please sign in to example".into(),
                platform: "linux".into(),
                ready_tools: BTreeSet::from(["computer".into()]),
                max_prompt_bytes: 32 * 1_024,
                max_skills: 4,
            },
            at(3_020),
        )
        .unwrap();
    assert_eq!(invoked.selected.len(), 1);
    assert_eq!(invoked.selected[0].id, "example-sign-in");
    assert!(invoked.selected[0].prompt.contains("Activate Continue"));
}

#[test]
fn checkpoint_resume_is_bounded_to_replayable_checkpoints() {
    let root = tempdir().unwrap();
    let store = TaskRecipeStore::open(root.path(), StoreLimits::default()).unwrap();
    let profile_id = ProfileId::new();
    let computer_session_id = ComputerSessionId::new();
    let initial = observation(
        &profile_id,
        &computer_session_id,
        10,
        "https://example.test/login",
        &"a".repeat(64),
        login_nodes("Submit"),
    );
    let mut recorder = keith_task_recipe::DemonstrationRecorder::start(
        &store,
        &initial,
        b"frame",
        MediaSanitization::NoSensitiveContent,
        ControlOwner::UserControl,
        "Wait for Example",
        RetentionPolicy {
            retain_until: at(1_000),
        },
        CaptureLimits::default(),
        CaptureSanitizer::default(),
    )
    .unwrap();
    recorder
        .record_cua_action(
            &store,
            &observation(
                &profile_id,
                &computer_session_id,
                20,
                "https://example.test/login",
                &"b".repeat(64),
                login_nodes("Submit"),
            ),
            b"frame-2",
            MediaSanitization::NoSensitiveContent,
            &ComputerAction::Wait { duration_ms: 50 },
            ControlOwner::UserControl,
        )
        .unwrap();
    recorder.complete(&store, at(30)).unwrap();
    let history = RecipeCompiler::default()
        .compile(recorder.demonstration(), at(40))
        .unwrap();
    let recipe = history.active().unwrap();
    let replay = RecipeReplay::from_checkpoint(
        recipe,
        profile_id,
        computer_session_id,
        RecipeReplayMode::ExplicitTest,
        BTreeMap::new(),
        "step-1-done",
    )
    .unwrap();
    assert_eq!(replay.state(), RecipeReplayState::Ready);
}
