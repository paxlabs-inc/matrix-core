package mission

import (
	"crypto/ed25519"
	"crypto/rand"
	"testing"
	"time"

	"centra/workforce/internal/contracts"
)

func TestActivationAuthority_VerifiesCanonicalFounderSignatures(t *testing.T) {
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	issuerPublic, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	value, err := BuildActivationDraft(
		contracts.OrganizationID("organization:test"),
		contracts.OwnerID("owner:test"),
		time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC),
		"key:founder", "key:issuer", issuerPublic, validDraft(),
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := signActivation(&value, "key:founder", privateKey); err != nil {
		t.Fatal(err)
	}
	if err := VerifyActivationAuthority(value, "key:founder", publicKey); err != nil {
		t.Fatal(err)
	}
	canonical, err := contracts.EncodeCanonical(&value.Mission)
	if err != nil || len(canonical) == 0 {
		t.Fatalf("canonical Mission = %d bytes, %v", len(canonical), err)
	}
}

func TestActivationAuthority_RejectsTamperAndWrongFounder(t *testing.T) {
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	issuerPublic, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	value, err := BuildActivationDraft(
		contracts.OrganizationID("organization:tamper"),
		contracts.OwnerID("owner:tamper"),
		time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC),
		"key:founder", "key:issuer", issuerPublic, validDraft(),
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := signActivation(&value, "key:founder", privateKey); err != nil {
		t.Fatal(err)
	}
	value.Mission.Purpose = "tampered purpose"
	if err := VerifyActivationAuthority(value, "key:founder", publicKey); err == nil {
		t.Fatal("tampered Mission was accepted")
	}
	otherPublic, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	if err := VerifyActivationAuthority(value, "key:founder", otherPublic); err == nil {
		t.Fatal("wrong founder key was accepted")
	}
}

func TestBuildActivationDraft_NormalizesSetsAndRejectsUnsafeCapital(t *testing.T) {
	issuerPublic, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	draft := validDraft()
	draft.PermittedBusinessDomains = []string{"software", "analytics"}
	value, err := BuildActivationDraft(
		contracts.OrganizationID("organization:sort"),
		contracts.OwnerID("owner:sort"),
		time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC),
		"key:founder", "key:issuer", issuerPublic, draft,
	)
	if err != nil {
		t.Fatal(err)
	}
	if value.Mission.PermittedBusinessDomains[0] != "analytics" {
		t.Fatalf("normalized domains = %#v", value.Mission.PermittedBusinessDomains)
	}
	draft.SpendCeilingMicrounits = draft.StartingMicrounits + 1
	if _, err := BuildActivationDraft(
		contracts.OrganizationID("organization:unsafe"),
		contracts.OwnerID("owner:unsafe"),
		time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC),
		"key:founder", "key:issuer", issuerPublic, draft,
	); err == nil {
		t.Fatal("spend beyond starting capital was accepted")
	}
}

func TestActivationAuthority_RejectsEveryInvalidBoundaryClass(t *testing.T) {
	issuerPublic, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	build := func(t *testing.T) ActivationAuthority {
		t.Helper()
		value, err := BuildActivationDraft(
			contracts.OrganizationID("organization:validation"),
			contracts.OwnerID("owner:validation"),
			time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC),
			"key:founder", "key:issuer", issuerPublic, validDraft(),
		)
		if err != nil {
			t.Fatal(err)
		}
		return value
	}
	tests := []struct {
		name   string
		mutate func(*ActivationAuthority)
	}{
		{"mission identity", func(value *ActivationAuthority) { value.Mission.ID = "mission:other" }},
		{"mission purpose", func(value *ActivationAuthority) { value.Mission.Purpose = "" }},
		{"mission duplicate domain", func(value *ActivationAuthority) {
			value.Mission.PermittedBusinessDomains = []string{"software", "software"}
		}},
		{"mission local time", func(value *ActivationAuthority) {
			value.Mission.EffectiveAt = value.Mission.EffectiveAt.In(time.FixedZone("offset", 3600))
		}},
		{"constitution identity", func(value *ActivationAuthority) { value.Constitution.ID = "constitution:other" }},
		{"constitution risk", func(value *ActivationAuthority) { value.Constitution.RiskTolerance = "unknown" }},
		{"constitution autonomy", func(value *ActivationAuthority) { value.Constitution.Autonomy = "unknown" }},
		{"constitution missing set", func(value *ActivationAuthority) { value.Constitution.PauseConditions = nil }},
		{"constitution missing reservation", func(value *ActivationAuthority) {
			value.Constitution.ReservedDecisions = value.Constitution.ReservedDecisions[:8]
		}},
		{"constitution reservation order", func(value *ActivationAuthority) {
			value.Constitution.ReservedDecisions[0], value.Constitution.ReservedDecisions[1] = value.Constitution.ReservedDecisions[1], value.Constitution.ReservedDecisions[0]
		}},
		{"capital identity", func(value *ActivationAuthority) { value.Capital.ID = "capital:other" }},
		{"capital currency", func(value *ActivationAuthority) { value.Capital.Currency = "" }},
		{"capital spend", func(value *ActivationAuthority) {
			value.Capital.SpendCeilingMicrounits = value.Capital.StartingMicrounits + 1
		}},
		{"capital runway", func(value *ActivationAuthority) { value.Capital.MinimumRunwayDays = 0 }},
		{"issuer identity", func(value *ActivationAuthority) { value.IssuerPolicy.ID = "issuer:other" }},
		{"issuer key", func(value *ActivationAuthority) { value.IssuerPolicy.IssuerKeyID = "" }},
		{"issuer classes", func(value *ActivationAuthority) { value.IssuerPolicy.AllowedWorkOrderClasses = nil }},
		{"issuer expiry", func(value *ActivationAuthority) { value.IssuerPolicy.ExpiresAt = value.IssuerPolicy.EffectiveAt }},
		{"cross organization", func(value *ActivationAuthority) { value.Capital.OrganizationID = "organization:other" }},
		{"cross time", func(value *ActivationAuthority) {
			value.Constitution.EffectiveAt = value.Constitution.EffectiveAt.Add(time.Second)
		}},
		{"cross capital", func(value *ActivationAuthority) {
			value.IssuerPolicy.MaxWorkOrderMicrounits = value.Capital.SpendCeilingMicrounits + 1
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			value := build(t)
			test.mutate(&value)
			if err := value.Validate(); err == nil {
				t.Fatal("invalid authority was accepted")
			}
		})
	}
	if RiskTolerance("unknown").Valid() || AutonomyLevel("unknown").Valid() ||
		ReservedDecisionKind("unknown").Valid() {
		t.Fatal("unknown closed enum was accepted")
	}
}

func TestSigning_RejectsNilKeysAndMalformedSignatures(t *testing.T) {
	if err := SignFounderMission(nil, "key", nil); err == nil {
		t.Fatal("nil Mission was signed")
	}
	if err := SignCompanyConstitution(nil, "key", nil); err == nil {
		t.Fatal("nil Constitution was signed")
	}
	if err := SignCapitalEnvelope(nil, "key", nil); err == nil {
		t.Fatal("nil capital was signed")
	}
	if err := SignCompanyIssuerPolicy(nil, "key", nil); err == nil {
		t.Fatal("nil issuer policy was signed")
	}
	issuerPublic, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	value, err := BuildActivationDraft(
		contracts.OrganizationID("organization:signing"),
		contracts.OwnerID("owner:signing"),
		time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC),
		"key:founder", "key:issuer", issuerPublic, validDraft(),
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := SignFounderMission(&value.Mission, "", nil); err == nil {
		t.Fatal("invalid private key was accepted")
	}
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	if err := signActivation(&value, "key:founder", privateKey); err != nil {
		t.Fatal(err)
	}
	value.Mission.Signature.KeyID = "key:other"
	if err := VerifyActivationAuthority(value, "key:founder", publicKey); err == nil {
		t.Fatal("signature key substitution was accepted")
	}
	value.Mission.Signature.KeyID = "key:founder"
	value.Mission.Signature.Value = "not-base64"
	if err := VerifyActivationAuthority(value, "key:founder", publicKey); err == nil {
		t.Fatal("malformed founder signature was accepted")
	}
	if _, err := BuildAuthorityDraft(
		contracts.OrganizationID("organization:signing"),
		contracts.OwnerID("owner:signing"),
		time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC),
		"key:founder", "key:issuer", issuerPublic, 0, validDraft(),
	); err == nil {
		t.Fatal("zero authority version was accepted")
	}
}

func signActivation(value *ActivationAuthority, keyID string, privateKey ed25519.PrivateKey) error {
	if err := SignFounderMission(&value.Mission, keyID, privateKey); err != nil {
		return err
	}
	if err := SignCompanyConstitution(&value.Constitution, keyID, privateKey); err != nil {
		return err
	}
	if err := SignCapitalEnvelope(&value.Capital, keyID, privateKey); err != nil {
		return err
	}
	if err := SignCompanyIssuerPolicy(&value.IssuerPolicy, keyID, privateKey); err != nil {
		return err
	}
	return SignOrganizationV2(&value.Organization, keyID, privateKey)
}

func validDraft() ActivationDraft {
	return ActivationDraft{
		Purpose:                  "Build verified software for approved customers",
		PermittedBusinessDomains: []string{"software"},
		StrategicPrinciples:      []string{"evidence before expansion"},
		TargetOutcomes:           []string{"verified customer value"},
		SuccessConditions:        []string{"authoritative customer outcome"},
		FailureConditions:        []string{"unreconciled external state"},
		LegalProhibitions:        []string{"no unlawful activity"},
		EthicalProhibitions:      []string{"no deceptive claims"},
		PermittedJurisdictions:   []string{"DE"},
		DataBoundaries:           []string{"purpose-bound customer data"},
		PermittedCounterparties:  []string{"owner-approved"},
		OperatingScopes: []OperatingScope{{
			Kind: OperatingScopeProject, ScopeID: "project:matrix",
			Purpose:             "Build and verify the approved Centra AI workspace",
			AllowedActions:      []string{"build", "read", "test", "write"},
			DataClassifications: []string{"internal-source"},
			Jurisdictions:       []string{"DE"},
		}},
		RiskTolerance: RiskToleranceLow, Autonomy: AutonomyReviewRequired,
		EscalationConditions: []string{"unverifiable material claim"},
		PauseConditions:      []string{"authority uncertainty"},
		ShutdownConditions:   []string{"founder emergency stop"},
		Currency:             "EUR", StartingMicrounits: 1_000_000_000,
		SpendCeilingMicrounits:    100_000_000,
		ExposureCeilingMicrounits: 100_000_000,
		MinimumRunwayDays:         180, MaxWorkOrderMicrounits: 10_000_000,
	}
}
