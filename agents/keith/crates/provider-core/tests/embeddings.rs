use keith_agent_types::{EntityId, SchemaVersion};
use keith_provider_core::{
    EMBEDDING_CONTRACT_VERSION, EmbeddingContractError, EmbeddingDescriptor, EmbeddingDistance,
    EmbeddingInput, EmbeddingLimits, EmbeddingNormalization, EmbeddingRequest, EmbeddingResponse,
    EmbeddingRole, EmbeddingSpaceIdentity, EmbeddingUsage, EmbeddingVector, ProviderCredential,
};

fn descriptor() -> EmbeddingDescriptor {
    EmbeddingDescriptor {
        space: EmbeddingSpaceIdentity {
            version: EMBEDDING_CONTRACT_VERSION,
            provider: "contract-provider".into(),
            model: "encoder".into(),
            revision: "revision-1".into(),
            dimensions: 3,
            distance: EmbeddingDistance::Cosine,
            normalization: EmbeddingNormalization::None,
            representation_version: "observed-text-v1".into(),
        },
        supported_roles: vec![EmbeddingRole::Query, EmbeddingRole::Document],
        limits: EmbeddingLimits {
            max_batch_items: 2,
            max_input_bytes: 8,
            max_batch_bytes: 12,
            max_vector_bytes: 24,
            max_dimensions: 3,
            max_timeout_ms: 100,
        },
    }
}

fn request() -> EmbeddingRequest {
    EmbeddingRequest {
        version: EMBEDDING_CONTRACT_VERSION,
        request_id: EntityId::from_u128(1),
        space: descriptor().space,
        role: EmbeddingRole::Document,
        inputs: vec![
            EmbeddingInput {
                id: EntityId::from_u128(2),
                text: "alpha".into(),
            },
            EmbeddingInput {
                id: EntityId::from_u128(3),
                text: "beta".into(),
            },
        ],
        timeout_ms: 50,
    }
}

fn response() -> EmbeddingResponse {
    let request = request();
    EmbeddingResponse {
        version: EMBEDDING_CONTRACT_VERSION,
        request_id: request.request_id,
        space: request.space,
        role: request.role,
        vectors: request
            .inputs
            .into_iter()
            .map(|input| EmbeddingVector {
                input_id: input.id,
                values: vec![1.0, 0.0, 0.0],
            })
            .collect(),
        usage: EmbeddingUsage { input_tokens: None },
    }
}

#[test]
fn version_one_contracts_round_trip_without_chat_configuration() {
    let descriptor = descriptor();
    let request = request();
    let response = response();
    descriptor.validate().unwrap();
    request.validate(&descriptor).unwrap();
    response.validate(&request, &descriptor).unwrap();
    let encoded = serde_json::to_vec(&descriptor).unwrap();
    assert_eq!(
        serde_json::from_slice::<EmbeddingDescriptor>(&encoded).unwrap(),
        descriptor
    );
    let encoded = serde_json::to_vec(&request).unwrap();
    assert_eq!(
        serde_json::from_slice::<EmbeddingRequest>(&encoded).unwrap(),
        request
    );
    let encoded = serde_json::to_vec(&response).unwrap();
    assert_eq!(
        serde_json::from_slice::<EmbeddingResponse>(&encoded).unwrap(),
        response
    );
    assert_eq!(response.usage.input_tokens, None);
}

#[test]
fn unknown_versions_roles_and_fields_fail_explicitly() {
    for version in [SchemaVersion::new(2, 0), SchemaVersion::new(1, 1)] {
        let mut identity = descriptor().space;
        identity.version = version;
        assert_eq!(
            identity.validate(),
            Err(EmbeddingContractError::UnsupportedVersion)
        );
        let mut request = request();
        request.version = version;
        assert_eq!(
            request.validate(&descriptor()),
            Err(EmbeddingContractError::UnsupportedVersion)
        );
        let mut response = response();
        response.version = version;
        assert_eq!(
            response.validate(&self::request(), &descriptor()),
            Err(EmbeddingContractError::UnsupportedVersion)
        );
    }
    assert!(serde_json::from_str::<EmbeddingRole>("\"future_role\"").is_err());
    let mut identity = serde_json::to_value(descriptor().space).unwrap();
    identity["undeclared"] = serde_json::json!(true);
    assert!(serde_json::from_value::<EmbeddingSpaceIdentity>(identity).is_err());
}

#[test]
fn every_space_identity_field_matters_even_at_equal_dimensions() {
    let original = descriptor();
    let mut alternatives = Vec::new();
    for field in ["provider", "model", "revision", "representation_version"] {
        let mut value = serde_json::to_value(&original.space).unwrap();
        value[field] = serde_json::json!("changed-identity");
        alternatives.push(serde_json::from_value::<EmbeddingSpaceIdentity>(value).unwrap());
    }
    let mut identity = original.space.clone();
    identity.distance = EmbeddingDistance::DotProduct;
    alternatives.push(identity);
    let mut identity = original.space.clone();
    identity.normalization = EmbeddingNormalization::UnitL2;
    alternatives.push(identity);
    for identity in alternatives {
        assert_eq!(identity.dimensions, original.space.dimensions);
        let mut request = request();
        request.space = identity;
        assert_eq!(
            request.validate(&original),
            Err(EmbeddingContractError::RequestSpaceMismatch)
        );
    }
}

#[test]
fn identities_and_resource_limits_must_be_bounded() {
    for model in [
        String::new(),
        " model".into(),
        "model\n".into(),
        "x".repeat(513),
    ] {
        let mut identity = descriptor().space;
        identity.model = model;
        assert_eq!(
            identity.validate(),
            Err(EmbeddingContractError::InvalidIdentity)
        );
    }
    for dimensions in [0, 65_537] {
        let mut identity = descriptor().space;
        identity.dimensions = dimensions;
        assert_eq!(
            identity.validate(),
            Err(EmbeddingContractError::InvalidIdentity)
        );
    }
    for field in [
        "max_batch_items",
        "max_input_bytes",
        "max_batch_bytes",
        "max_vector_bytes",
        "max_dimensions",
        "max_timeout_ms",
    ] {
        let mut value = serde_json::to_value(descriptor().limits).unwrap();
        value[field] = serde_json::json!(0);
        let limits: EmbeddingLimits = serde_json::from_value(value).unwrap();
        assert_eq!(
            limits.validate(),
            Err(EmbeddingContractError::InvalidLimits)
        );
    }
    let mut descriptor = descriptor();
    descriptor.limits.max_dimensions = 2;
    assert_eq!(
        descriptor.validate(),
        Err(EmbeddingContractError::InvalidLimits)
    );
}

#[test]
fn role_support_is_explicit_and_unique() {
    let mut descriptor = descriptor();
    descriptor.supported_roles = vec![EmbeddingRole::Query];
    assert_eq!(
        request().validate(&descriptor),
        Err(EmbeddingContractError::UnsupportedRole)
    );
    descriptor.supported_roles.push(EmbeddingRole::Query);
    assert_eq!(
        descriptor.validate(),
        Err(EmbeddingContractError::UnsupportedRole)
    );
    descriptor.supported_roles.clear();
    assert_eq!(
        descriptor.validate(),
        Err(EmbeddingContractError::UnsupportedRole)
    );
}

#[test]
fn input_limits_count_utf8_bytes_and_never_truncate() {
    let mut request = request();
    request.inputs[0].text = "ééééé".into();
    let original = request.clone();
    assert_eq!(
        request.validate(&descriptor()),
        Err(EmbeddingContractError::InvalidRequest)
    );
    assert_eq!(request, original);
    request.inputs[0].text = "12345678".into();
    request.inputs[1].text = "12345".into();
    assert_eq!(
        request.validate(&descriptor()),
        Err(EmbeddingContractError::InvalidRequest)
    );
    request.inputs[1].text = "1234".into();
    request.validate(&descriptor()).unwrap();
    let mut descriptor = descriptor();
    descriptor.limits.max_vector_bytes = 12;
    assert_eq!(
        request.validate(&descriptor),
        Err(EmbeddingContractError::InvalidRequest)
    );
    request.inputs.pop();
    request.validate(&descriptor).unwrap();
}

#[test]
fn empty_duplicate_excess_inputs_and_invalid_timeouts_are_rejected() {
    let base = request();
    let mut invalid = Vec::new();
    let mut request = base.clone();
    request.inputs.clear();
    invalid.push(request);
    let mut request = base.clone();
    request.inputs[1].id = request.inputs[0].id.clone();
    invalid.push(request);
    let mut request = base.clone();
    request.inputs.push(EmbeddingInput {
        id: EntityId::from_u128(4),
        text: "x".into(),
    });
    invalid.push(request);
    let mut request = base.clone();
    request.inputs[0].text = " \n".into();
    invalid.push(request);
    for timeout_ms in [0, 101] {
        let mut request = base.clone();
        request.timeout_ms = timeout_ms;
        invalid.push(request);
    }
    for request in invalid {
        assert_eq!(
            request.validate(&descriptor()),
            Err(EmbeddingContractError::InvalidRequest)
        );
    }
}

#[test]
fn reordered_results_are_matched_by_input_identity() {
    let mut response = response();
    response.vectors.reverse();
    response.validate(&request(), &descriptor()).unwrap();
}

#[test]
fn response_coverage_rejects_missing_extra_duplicate_and_unknown_inputs() {
    let original = response();
    let mut invalid = Vec::new();
    let mut response = original.clone();
    response.vectors.pop();
    invalid.push(response);
    let mut response = original.clone();
    response.vectors.push(response.vectors[0].clone());
    invalid.push(response);
    let mut response = original.clone();
    response.vectors[1].input_id = response.vectors[0].input_id.clone();
    invalid.push(response);
    let mut response = original;
    response.vectors[1].input_id = EntityId::from_u128(99);
    invalid.push(response);
    for response in invalid {
        assert_eq!(
            response.validate(&request(), &descriptor()),
            Err(EmbeddingContractError::ResponseCoverage)
        );
    }
}

#[test]
fn response_identity_must_match_the_exact_request_space_and_role() {
    let mut response = response();
    response.request_id = EntityId::from_u128(99);
    assert_eq!(
        response.validate(&request(), &descriptor()),
        Err(EmbeddingContractError::ResponseIdentityMismatch)
    );
    response = self::response();
    response.role = EmbeddingRole::Query;
    assert_eq!(
        response.validate(&request(), &descriptor()),
        Err(EmbeddingContractError::ResponseIdentityMismatch)
    );
    response = self::response();
    response.space.revision = "other-revision".into();
    assert_eq!(
        response.validate(&request(), &descriptor()),
        Err(EmbeddingContractError::ResponseIdentityMismatch)
    );
}

#[test]
fn vector_dimensions_cannot_be_silently_padded_or_truncated() {
    for values in [vec![], vec![1.0, 0.0], vec![1.0, 0.0, 0.0, 0.0]] {
        let mut response = response();
        response.vectors[0].values = values;
        let original = response.clone();
        assert_eq!(
            response.validate(&request(), &descriptor()),
            Err(EmbeddingContractError::DimensionMismatch)
        );
        assert_eq!(response, original);
    }
}

#[test]
fn zero_and_nonfinite_vectors_are_rejected() {
    for values in [
        vec![0.0, 0.0, 0.0],
        vec![f32::NAN, 1.0, 0.0],
        vec![f32::INFINITY, 1.0, 0.0],
        vec![f32::NEG_INFINITY, 1.0, 0.0],
    ] {
        let mut response = response();
        response.vectors[0].values = values;
        assert_eq!(
            response.validate(&request(), &descriptor()),
            Err(EmbeddingContractError::InvalidVector)
        );
    }
}

#[test]
fn declared_normalization_is_checked_without_rewriting_the_vector() {
    let mut descriptor = descriptor();
    descriptor.space.normalization = EmbeddingNormalization::UnitL2;
    let mut request = request();
    request.space = descriptor.space.clone();
    let mut response = response();
    response.space = descriptor.space.clone();
    response.vectors[0].values = vec![0.6, 0.8, 0.0];
    response.validate(&request, &descriptor).unwrap();
    response.vectors[0].values = vec![3.0, 4.0, 0.0];
    assert_eq!(
        response.validate(&request, &descriptor),
        Err(EmbeddingContractError::NormalizationMismatch)
    );
    assert_eq!(response.vectors[0].values, vec![3.0, 4.0, 0.0]);
}

#[test]
fn debug_and_contract_errors_do_not_expose_input_or_credentials() {
    let mut request = request();
    request.inputs[0].text = "private-source-marker".into();
    assert!(!format!("{request:?}").contains("private-source-marker"));
    let error = request.validate(&descriptor()).unwrap_err();
    assert!(!error.to_string().contains("private-source-marker"));
    let credential = ProviderCredential::new("credential-marker").unwrap();
    assert!(!format!("{credential:?}").contains("credential-marker"));
    assert!(!format!("{:?}", response().vectors[0]).contains("1.0"));
}
