use std::collections::{BTreeMap, BTreeSet};

use keith_agent_types::{ProfileId, UtcTimestamp};
use keith_platform_contracts::{
    ActionRisk, Capability, ComputerSessionId, ControlOwner, DemonstrationId,
};
use keith_skills::{SkillLimits, SkillRegistry, SkillRoots};
use keith_task_recipe::{
    ApprovalRequirement, CaptureLimits, CaptureSanitizer, ClipboardOperation, Demonstration,
    DemonstrationEventKind, DemonstrationExport, DemonstrationState, FieldMetadata, FrameReference,
    KeyPhase, MediaSanitization, ObservationKind, ObservationMatcher, ParameterReference,
    ParameterSource, RawCaptureContext, RawDemonstrationEvent, RawDemonstrationEventKind,
    RawSemanticTarget, RawValue, RecipeAction, RecipeCheckpoint, RecipeCorrection, RecipeInput,
    RecipeInputKind, RecipeStep, RecipeTarget, RecoveryBranch, Rectangle, RedactionPolicy,
    RetentionPolicy, SemanticTargetSelector, SkillPublicationOptions, StoreLimits, TaskRecipe,
    TaskRecipeError, TaskRecipeHistory, TaskRecipeStore, TemplateValue, VisualFallback,
};
use keith_workspace::{PersonalWorkspace, PersonalWorkspaceLimits};
use tempfile::tempdir;

fn timestamp(value: i64) -> UtcTimestamp {
    UtcTimestamp::from_unix_millis(value)
}

fn store() -> (tempfile::TempDir, TaskRecipeStore) {
    let directory = tempdir().unwrap();
    let store = TaskRecipeStore::open(directory.path(), StoreLimits::default()).unwrap();
    (directory, store)
}

fn sanitizer() -> CaptureSanitizer {
    let mut policy = RedactionPolicy::default();
    policy
        .bind(
            "password",
            ParameterReference::new("github-password", ParameterSource::NamedCredential).unwrap(),
        )
        .unwrap();
    CaptureSanitizer::new(policy)
}

fn frame(store: &TaskRecipeStore) -> FrameReference {
    FrameReference {
        frame_id: "frame-1".into(),
        media: store
            .put_media(
                b"sanitized-frame-pixels",
                "image/png",
                MediaSanitization::SensitiveRegionsRedacted,
            )
            .unwrap(),
        width: 1_280,
        height: 720,
    }
}

fn context(frame: &FrameReference) -> RawCaptureContext {
    RawCaptureContext {
        frame: Some(frame.clone()),
        semantic_target: None,
        url: Some(RawValue::new("https://example.test/login")),
        window: Some(RawValue::new("Example Login")),
        application: Some(RawValue::new("Chromium")),
        control_owner: ControlOwner::UserControl,
    }
}

fn completed_demonstration(store: &TaskRecipeStore) -> Demonstration {
    let frame = frame(store);
    let mut demonstration = Demonstration::new(
        ProfileId::new(),
        ComputerSessionId::new(),
        "Sign in to example",
        timestamp(1_000),
        RetentionPolicy {
            retain_until: timestamp(100_000),
        },
        CaptureLimits::default(),
    )
    .unwrap();
    demonstration
        .record(
            RawDemonstrationEvent {
                captured_at: timestamp(1_000),
                context: context(&frame),
                kind: RawDemonstrationEventKind::FrameCaptured(frame.clone()),
            },
            &sanitizer(),
        )
        .unwrap();
    demonstration
        .record(
            RawDemonstrationEvent {
                captured_at: timestamp(1_010),
                context: RawCaptureContext {
                    semantic_target: Some(RawSemanticTarget {
                        role: "textbox".into(),
                        accessible_name: RawValue::new("Password"),
                        stable_attributes: BTreeMap::from([
                            ("id".into(), RawValue::new("password")),
                            (
                                "data-token".into(),
                                RawValue::new("Bearer semantic-target-secret"),
                            ),
                        ]),
                        bounds: Rectangle {
                            x: 30,
                            y: 40,
                            width: 200,
                            height: 30,
                        },
                        field: FieldMetadata {
                            name: Some("password".into()),
                            role: Some("textbox".into()),
                            autocomplete: Some("current-password".into()),
                            user_marked_sensitive: false,
                        },
                    }),
                    ..context(&frame)
                },
                kind: RawDemonstrationEventKind::Keyboard {
                    phase: KeyPhase::Down,
                    key: RawValue::new("correct horse battery staple"),
                    code: "Unidentified".into(),
                    modifiers: Vec::new(),
                    field: FieldMetadata {
                        name: Some("password".into()),
                        role: Some("textbox".into()),
                        autocomplete: Some("current-password".into()),
                        user_marked_sensitive: false,
                    },
                },
            },
            &sanitizer(),
        )
        .unwrap();
    demonstration
        .record(
            RawDemonstrationEvent {
                captured_at: timestamp(1_020),
                context: context(&frame),
                kind: RawDemonstrationEventKind::Clipboard {
                    operation: ClipboardOperation::Write,
                    value: Some(RawValue::new("Bearer ultra-secret-token")),
                    field: FieldMetadata::named("clipboard"),
                },
            },
            &sanitizer(),
        )
        .unwrap();
    demonstration.complete(timestamp(1_030)).unwrap();
    demonstration
}

fn matcher(description: &str, expected: &str) -> ObservationMatcher {
    ObservationMatcher {
        kind: ObservationKind::VisibleText,
        description: description.into(),
        expected: TemplateValue::Literal(expected.into()),
        timeout_ms: 5_000,
    }
}

fn target(frame_digest: &str, accessible_name: &str) -> RecipeTarget {
    RecipeTarget {
        semantic: SemanticTargetSelector {
            role: "button".into(),
            accessible_name: Some(TemplateValue::Literal(accessible_name.into())),
            stable_attributes: BTreeMap::from([("type".into(), "submit".into())]),
        },
        visual_fallback: Some(VisualFallback {
            source_frame_digest: frame_digest.into(),
            normalized_bounds: Rectangle {
                x: 30,
                y: 90,
                width: 120,
                height: 30,
            },
            match_threshold_percent: 85,
        }),
    }
}

fn recipe(demonstration: &Demonstration) -> TaskRecipe {
    let frame_digest = match &demonstration.events()[0].kind {
        DemonstrationEventKind::FrameCaptured(frame) => frame.media.digest.as_str(),
        _ => panic!("first event must be a frame"),
    };
    TaskRecipe::new(
        demonstration.id.clone(),
        "Sign in to example",
        "Use a named credential to sign in and verify the account landing page.",
        vec![RecipeInput {
            name: "github-password".into(),
            label: "GitHub password".into(),
            kind: RecipeInputKind::Credential,
            required: true,
        }],
        vec![matcher("Login page is visible", "Sign in")],
        vec![RecipeStep {
            id: "submit-login".into(),
            title: "Submit the sign-in form".into(),
            action: RecipeAction::Activate {
                target: target(frame_digest, "Sign in"),
            },
            expected_observations: vec![matcher("Account landing page appears", "Welcome")],
            checkpoint: Some(RecipeCheckpoint {
                name: "signed-in".into(),
                description: "The account landing page is open".into(),
                replayable: true,
            }),
            approval: None,
            recovery: vec![RecoveryBranch {
                when: matcher("Login form remains visible", "Sign in"),
                retry_step_id: Some("submit-login".into()),
                resume_checkpoint: None,
                max_attempts: 1,
            }],
        }],
        vec![matcher("Signed-in landing page is visible", "Welcome")],
        BTreeSet::from(["layout-recovery".into(), "shadow-replay".into()]),
        timestamp(2_000),
    )
    .unwrap()
}

#[test]
fn synchronized_capture_substitutes_credentials_and_never_serializes_raw_secrets() {
    let (_directory, store) = store();
    let demonstration = completed_demonstration(&store);
    assert_eq!(demonstration.events().len(), 3);
    assert_eq!(demonstration.events()[1].sequence, 1);
    assert_eq!(demonstration.events()[1].elapsed_ms, 10);
    let DemonstrationEventKind::Keyboard(input) = &demonstration.events()[1].kind else {
        panic!("expected keyboard event");
    };
    assert!(matches!(
        &input.key,
        keith_task_recipe::CapturedValue::Parameter(reference)
            if reference.name == "github-password"
                && reference.source == ParameterSource::NamedCredential
    ));
    let encoded = serde_json::to_string(&demonstration).unwrap();
    assert!(!encoded.contains("correct horse"));
    assert!(!encoded.contains("ultra-secret-token"));
    assert!(!encoded.contains("semantic-target-secret"));
    assert!(encoded.contains("github-password"));
    assert!(format!("{:?}", RawValue::new("do-not-log-me")).contains("[REDACTED]"));
    assert!(!format!("{:?}", RawValue::new("do-not-log-me")).contains("do-not-log-me"));
}

#[test]
fn capture_rejects_unsynchronized_input_and_invalid_lifecycle_transitions() {
    let (_directory, store) = store();
    let mut demonstration = Demonstration::new(
        ProfileId::new(),
        ComputerSessionId::new(),
        "Bounded capture",
        timestamp(100),
        RetentionPolicy {
            retain_until: timestamp(1_000),
        },
        CaptureLimits::default(),
    )
    .unwrap();
    let result = demonstration.record(
        RawDemonstrationEvent {
            captured_at: timestamp(101),
            context: RawCaptureContext {
                frame: None,
                semantic_target: None,
                url: None,
                window: None,
                application: None,
                control_owner: ControlOwner::UserControl,
            },
            kind: RawDemonstrationEventKind::Keyboard {
                phase: KeyPhase::Down,
                key: RawValue::new("a"),
                code: "KeyA".into(),
                modifiers: Vec::new(),
                field: FieldMetadata::default(),
            },
        },
        &sanitizer(),
    );
    assert!(matches!(
        result,
        Err(TaskRecipeError::InvalidDemonstration(_))
    ));
    assert!(demonstration.complete(timestamp(102)).is_err());
    drop(store);
}

#[test]
fn pause_resume_and_control_events_preserve_the_recording_lifecycle() {
    let (_directory, store) = store();
    let frame = frame(&store);
    let mut demonstration = Demonstration::new(
        ProfileId::new(),
        ComputerSessionId::new(),
        "Control transition capture",
        timestamp(500),
        RetentionPolicy {
            retain_until: timestamp(5_000),
        },
        CaptureLimits::default(),
    )
    .unwrap();
    demonstration
        .record(
            RawDemonstrationEvent {
                captured_at: timestamp(500),
                context: context(&frame),
                kind: RawDemonstrationEventKind::FrameCaptured(frame.clone()),
            },
            &sanitizer(),
        )
        .unwrap();
    demonstration
        .record(
            RawDemonstrationEvent {
                captured_at: timestamp(510),
                context: context(&frame),
                kind: RawDemonstrationEventKind::Pause {
                    reason: RawValue::new("User took control"),
                },
            },
            &sanitizer(),
        )
        .unwrap();
    assert_eq!(demonstration.state, DemonstrationState::Paused);
    let blocked = demonstration.record(
        RawDemonstrationEvent {
            captured_at: timestamp(511),
            context: context(&frame),
            kind: RawDemonstrationEventKind::Narration(RawValue::new("must not record")),
        },
        &sanitizer(),
    );
    assert!(matches!(blocked, Err(TaskRecipeError::InvalidState)));
    demonstration
        .record(
            RawDemonstrationEvent {
                captured_at: timestamp(520),
                context: context(&frame),
                kind: RawDemonstrationEventKind::Resume,
            },
            &sanitizer(),
        )
        .unwrap();
    demonstration
        .record(
            RawDemonstrationEvent {
                captured_at: timestamp(530),
                context: RawCaptureContext {
                    control_owner: ControlOwner::KeithControl,
                    ..context(&frame)
                },
                kind: RawDemonstrationEventKind::ControlChanged(ControlOwner::KeithControl),
            },
            &sanitizer(),
        )
        .unwrap();
    demonstration.complete(timestamp(540)).unwrap();
    demonstration.validate().unwrap();
    assert_eq!(demonstration.events().len(), 4);
}

#[test]
fn corrections_create_immutable_versions_and_rollback_creates_a_new_head() {
    let (_directory, store) = store();
    let demonstration = completed_demonstration(&store);
    let mut history = TaskRecipeHistory::new(recipe(&demonstration)).unwrap();
    history
        .apply_correction(
            RecipeCorrection::LabelParameter {
                input_name: "github-password".into(),
                label: "Account password credential".into(),
            },
            timestamp(2_010),
        )
        .unwrap();
    let frame_digest = match &demonstration.events()[0].kind {
        DemonstrationEventKind::FrameCaptured(frame) => frame.media.digest.as_str(),
        _ => unreachable!(),
    };
    history
        .apply_correction(
            RecipeCorrection::CorrectTarget {
                step_id: "submit-login".into(),
                target: target(frame_digest, "Continue"),
            },
            timestamp(2_020),
        )
        .unwrap();
    history
        .apply_correction(
            RecipeCorrection::AddConfirmation {
                step_id: "submit-login".into(),
                approval: ApprovalRequirement {
                    capability: Capability::ComputerControl,
                    risk: ActionRisk::IrreversibleComputerInput,
                    reason: "Confirm the account and current submit target".into(),
                    invalidate_on_target_change: true,
                },
            },
            timestamp(2_030),
        )
        .unwrap();
    assert_eq!(history.active().unwrap().revision, 4);
    assert_eq!(
        history.version(1).unwrap().inputs[0].label,
        "GitHub password"
    );
    let rolled_back = history.rollback(1, timestamp(2_040)).unwrap();
    assert_eq!(rolled_back.revision, 5);
    assert_eq!(rolled_back.rollback_of, Some(1));
    assert_eq!(rolled_back.inputs[0].label, "GitHub password");
    assert!(!rolled_back.qualification.is_publishable());
    let decoded: TaskRecipeHistory =
        serde_json::from_slice(&serde_json::to_vec(&history).unwrap()).unwrap();
    decoded.validate().unwrap();
    assert_eq!(decoded, history);
}

#[test]
fn publication_requires_real_checks_and_installs_through_the_skill_registry() {
    let (_directory, store) = store();
    let demonstration = completed_demonstration(&store);
    let mut recipe = recipe(&demonstration);
    let options = SkillPublicationOptions {
        skill_id: "example-sign-in".into(),
        triggers: vec!["sign in to example".into()],
        required_tools: vec!["computer".into()],
        platforms: vec!["linux".into()],
    };
    assert!(matches!(
        recipe.skill_publication(options.clone()),
        Err(TaskRecipeError::PublicationNotReady)
    ));
    for check in ["layout-recovery", "shadow-replay"] {
        recipe
            .qualification
            .record(check, true, "a".repeat(64), timestamp(3_000))
            .unwrap();
    }
    recipe.qualification.accept(timestamp(3_001)).unwrap();
    let publication = recipe.skill_publication(options).unwrap();
    assert!(publication.source.contains("# Sign in to example"));
    assert!(!publication.source.contains("correct horse"));

    let registry_root = tempdir().unwrap();
    let workspace = PersonalWorkspace::open(
        registry_root.path().join("workspace"),
        PersonalWorkspaceLimits::default(),
        timestamp(3_002),
    )
    .unwrap();
    let registry = SkillRegistry::open(
        workspace,
        SkillRoots {
            built_in: registry_root.path().join("built-in"),
            global: registry_root.path().join("global"),
            project: registry_root.path().join("project"),
        },
        SkillLimits::default(),
    )
    .unwrap();
    let installed = publication.install(&registry, timestamp(3_003)).unwrap();
    assert_eq!(installed.manifest.id, "example-sign-in");
    assert_eq!(installed.manifest.version, "1");
    assert_eq!(installed.manifest.validation.len(), 2);
}

#[test]
fn filesystem_store_exports_sanitized_data_and_cascades_complete_deletion() {
    let (_directory, store) = store();
    let demonstration = completed_demonstration(&store);
    store.save_demonstration(&demonstration).unwrap();
    let history = TaskRecipeHistory::new(recipe(&demonstration)).unwrap();
    store.save_recipe_history(&history).unwrap();

    let export = store.export_demonstration(&demonstration.id).unwrap();
    assert!(!String::from_utf8_lossy(&export).contains("correct horse"));
    let decoded: DemonstrationExport = serde_json::from_slice(&export).unwrap();
    assert_eq!(decoded.demonstration.id, demonstration.id);
    assert_eq!(decoded.derived_recipes.len(), 1);
    assert_eq!(decoded.media.len(), 1);

    let report = store.delete_demonstration(&demonstration.id).unwrap();
    assert!(report.demonstration_removed);
    assert_eq!(report.removed_recipe_ids, vec![history.recipe_id.clone()]);
    assert_eq!(report.removed_media_digests.len(), 1);
    assert!(report.retained_shared_media_digests.is_empty());
    assert!(matches!(
        store.load_demonstration(&demonstration.id),
        Err(TaskRecipeError::NotFound)
    ));
    assert!(matches!(
        store.load_recipe_history(&history.recipe_id),
        Err(TaskRecipeError::NotFound)
    ));
    assert!(
        store
            .root()
            .join("media")
            .join(format!("{}.bin", report.removed_media_digests[0]))
            .try_exists()
            .is_ok_and(|exists| !exists)
    );
}

#[test]
fn retention_pruning_removes_expired_recordings_and_derived_recipes() {
    let (_directory, store) = store();
    let mut demonstration = completed_demonstration(&store);
    demonstration.retention.retain_until = timestamp(2_000);
    store.save_demonstration(&demonstration).unwrap();
    let history = TaskRecipeHistory::new(recipe(&demonstration)).unwrap();
    store.save_recipe_history(&history).unwrap();
    let reports = store.prune_expired(timestamp(2_001)).unwrap();
    assert_eq!(reports.len(), 1);
    assert_eq!(reports[0].removed_recipe_ids, vec![history.recipe_id]);
}

#[test]
fn recipe_identity_cannot_point_at_an_unknown_demonstration() {
    let (_directory, store) = store();
    let demonstration = completed_demonstration(&store);
    let mut recipe = recipe(&demonstration);
    recipe.source_demonstration_id = DemonstrationId::new();
    let history = TaskRecipeHistory::new(recipe).unwrap();
    assert!(matches!(
        store.save_recipe_history(&history),
        Err(TaskRecipeError::NotFound)
    ));
}
