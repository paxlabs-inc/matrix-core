use std::collections::BTreeSet;
use std::fs;
use std::path::Path;

use serde_json::Value;

#[test]
fn production_qualification_traces_every_required_real_component_and_lifecycle_journey() {
    let workspace = Path::new(env!("CARGO_MANIFEST_DIR")).join("../..");
    let evidence_path = workspace.join("evidence/plugins/qualification.json");
    let evidence: Value =
        serde_json::from_slice(&fs::read(&evidence_path).expect("plugin qualification evidence"))
            .expect("plugin qualification JSON");
    assert_eq!(evidence["qualification_result"], "passed_local_conformance");
    assert_eq!(evidence["real_wasi_component"], true);
    assert_eq!(evidence["secret_values_read"], false);

    let required = BTreeSet::from([
        "payload",
        "stream",
        "cancellation",
        "resource_denial",
        "update",
        "migration",
        "rollback",
        "quarantine",
        "safe_mode",
        "uninstall",
        "signature_refusal",
        "traversal_denial",
        "network_denial",
        "credential_denial",
        "crash_isolation",
        "secret_redaction",
    ]);
    let operations = evidence["operation_trace"]
        .as_array()
        .expect("operation trace")
        .iter()
        .map(|operation| {
            let name = operation["operation"].as_str().expect("operation name");
            let implementation = operation["implementation"]
                .as_str()
                .expect("operation implementation");
            let test = operation["test"].as_str().expect("operation test");
            assert!(
                workspace
                    .join(implementation.split("::").next().unwrap())
                    .exists()
            );
            assert!(workspace.join(test.split("::").next().unwrap()).exists());
            name
        })
        .collect::<BTreeSet<_>>();
    assert_eq!(operations, required);
}
