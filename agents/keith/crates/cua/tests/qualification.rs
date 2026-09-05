use std::fs;
use std::path::Path;

use keith_agent_types::{ProfileId, UtcTimestamp};
use keith_cua::{
    ActionTarget, ComputerAction, ComputerResourceLimits, MouseButton, NamedCredentialGrant,
    SemanticTarget,
};
use keith_platform_contracts::RedactedText;
use serde_json::Value;

fn semantic() -> SemanticTarget {
    SemanticTarget::Accessibility {
        role: "button".to_owned(),
        name: "Continue".to_owned(),
    }
}

fn target() -> ActionTarget {
    ActionTarget::Semantic { target: semantic() }
}

#[test]
fn complete_visual_action_vocabulary_is_bounded_before_runtime_execution() {
    let profile = ProfileId::new();
    let limits = ComputerResourceLimits::default();
    let actions = vec![
        ComputerAction::Move { target: target() },
        ComputerAction::Click {
            target: target(),
            button: MouseButton::Left,
        },
        ComputerAction::DoubleClick {
            target: target(),
            button: MouseButton::Left,
        },
        ComputerAction::Drag {
            from: target(),
            to: ActionTarget::Semantic {
                target: SemanticTarget::Text {
                    text: "Destination".to_owned(),
                },
            },
            duration_ms: 200,
        },
        ComputerAction::Scroll {
            delta_x: 0,
            delta_y: 640,
        },
        ComputerAction::Key {
            key: "Enter".to_owned(),
        },
        ComputerAction::Text {
            text: "bounded text".to_owned(),
        },
        ComputerAction::Shortcut {
            keys: vec!["Control".to_owned(), "L".to_owned()],
        },
        ComputerAction::ClipboardRead,
        ComputerAction::ClipboardWrite {
            text: "bounded clipboard".to_owned(),
        },
        ComputerAction::FileUpload {
            target: semantic(),
            relative_path: "uploads/report.pdf".to_owned(),
        },
        ComputerAction::Download {
            target: semantic(),
            expected_file_name: Some("report.pdf".to_owned()),
        },
        ComputerAction::NewTab {
            url: Some("https://example.com/".to_owned()),
        },
        ComputerAction::CloseTab,
        ComputerAction::SwitchTab { index: 1 },
        ComputerAction::NewWindow { url: None },
        ComputerAction::CloseWindow,
        ComputerAction::FocusWindow {
            window_id: "chromium-main".to_owned(),
        },
        ComputerAction::Navigate {
            url: "https://example.com/".to_owned(),
        },
        ComputerAction::Wait { duration_ms: 250 },
        ComputerAction::CredentialFill {
            grant: NamedCredentialGrant {
                grant_name: RedactedText::parse("primary-login").expect("grant name"),
                opaque_handle: RedactedText::parse("vault:primary-login").expect("opaque handle"),
                profile_id: profile,
                allowed_origin: RedactedText::parse("https://example.com/")
                    .expect("allowed origin"),
                expires_at: UtcTimestamp::from_unix_millis(60_000),
            },
            target: SemanticTarget::Css {
                selector: "input[type=password]".to_owned(),
            },
        },
    ];
    assert_eq!(actions.len(), 21);
    for action in actions {
        action.validate(&limits).expect("bounded action");
    }

    assert!(matches!(
        ComputerAction::Scroll {
            delta_x: 0,
            delta_y: 100_001,
        }
        .validate(&limits),
        Err(keith_cua::ComputerError::InvalidAction)
    ));
}

#[test]
fn qualification_manifest_traces_real_process_teaching_and_measured_safety_journeys() {
    let workspace = Path::new(env!("CARGO_MANIFEST_DIR")).join("../..");
    let evidence: Value = serde_json::from_slice(
        &fs::read(workspace.join("evidence/cua/qualification.json"))
            .expect("CUA qualification evidence"),
    )
    .expect("CUA qualification JSON");
    assert!(matches!(
        evidence["qualification_result"].as_str(),
        Some("pending_runtime_gates" | "passed_local_process_and_teaching_conformance")
    ));
    assert_eq!(
        evidence["credential_inventory"]["secret_values_read"],
        false
    );
    assert!(evidence["journeys"].as_array().is_some_and(|items| {
        items.len() >= 16
            && items.iter().all(|item| {
                let implementation = item["implementation"].as_str().unwrap_or_default();
                let test = item["test"].as_str().unwrap_or_default();
                workspace
                    .join(implementation.split("::").next().unwrap_or_default())
                    .exists()
                    && workspace
                        .join(test.split("::").next().unwrap_or_default())
                        .exists()
            })
    }));
}
