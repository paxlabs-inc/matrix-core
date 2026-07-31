package contracts

import (
	"bytes"
	"crypto/ed25519"
	"encoding/base64"
	"reflect"
	"strings"
	"testing"
	"time"
)

type mapContract struct {
	SchemaVersion string            `json:"schema_version"`
	Values        map[string]string `json:"values"`
}

func (c mapContract) Validate() error { return validateSchema(c.SchemaVersion) }

type floatContract struct {
	SchemaVersion string  `json:"schema_version"`
	Value         float64 `json:"value"`
}

func (c floatContract) Validate() error { return validateSchema(c.SchemaVersion) }

type channelContract struct {
	SchemaVersion string   `json:"schema_version"`
	Value         chan int `json:"value"`
}

func (c channelContract) Validate() error { return validateSchema(c.SchemaVersion) }

type largeContract struct {
	SchemaVersion string `json:"schema_version"`
	Value         string `json:"value"`
}

func (c largeContract) Validate() error { return validateSchema(c.SchemaVersion) }

func TestCanonicalCodec_RoundTripsWorkPacket_WithStableBytes(t *testing.T) {
	packet := validWorkPacket()
	first, err := EncodeCanonical(&packet)
	if err != nil {
		t.Fatalf("encode valid packet: %v", err)
	}
	second, err := EncodeCanonical(&packet)
	if err != nil {
		t.Fatalf("encode packet again: %v", err)
	}
	if !bytes.Equal(first, second) {
		t.Fatalf("canonical bytes changed between encodes\nfirst: %s\nsecond: %s", first, second)
	}
	decoded, err := DecodeCanonical[WorkPacket, *WorkPacket](first)
	if err != nil {
		t.Fatalf("decode canonical packet: %v", err)
	}
	if !reflect.DeepEqual(packet, decoded) {
		t.Fatalf("round trip changed packet\nwant: %#v\ngot:  %#v", packet, decoded)
	}
}

func TestCanonicalCodec_RejectsMalformedAndNonCanonicalInput_BeforeExecution(t *testing.T) {
	goal := validGoal()
	canonical, err := EncodeCanonical(&goal)
	if err != nil {
		t.Fatalf("encode valid goal: %v", err)
	}
	tests := []struct {
		name string
		data []byte
	}{
		{name: "empty", data: nil},
		{name: "invalid utf8", data: []byte{0xff, 0xfe}},
		{name: "malformed", data: []byte(`{"schema_version":`)},
		{name: "unknown field", data: append([]byte(`{"unknown":true,`), canonical[1:]...)},
		{name: "duplicate field", data: append([]byte(`{"schema_version":"workforce.v1",`), canonical[1:]...)},
		{name: "trailing value", data: append(append([]byte{}, canonical...), []byte(`{}`)...)},
		{name: "unsupported schema", data: bytes.Replace(canonical, []byte(SchemaVersionV1), []byte("workforce.v2"), 1)},
		{name: "non canonical whitespace", data: append([]byte(" "), canonical...)},
		{name: "oversized", data: bytes.Repeat([]byte(" "), MaxCanonicalBytes+1)},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := DecodeCanonical[Goal, *Goal](test.data); err == nil {
				t.Fatalf("DecodeCanonical accepted %s input", test.name)
			}
		})
	}
}

func TestCanonicalCodec_StrictDecodeAcceptsTransportWhitespace_ButCanonicalBoundaryRejectsIt(t *testing.T) {
	goal := validGoal()
	canonical, err := EncodeCanonical(&goal)
	if err != nil {
		t.Fatalf("encode valid goal: %v", err)
	}
	transport := append(append([]byte(" \n"), canonical...), '\n')
	decoded, err := DecodeStrict[Goal, *Goal](transport)
	if err != nil {
		t.Fatalf("strict transport decode: %v", err)
	}
	if !reflect.DeepEqual(goal, decoded) {
		t.Fatalf("strict transport decode changed goal")
	}
	if _, err := DecodeCanonical[Goal, *Goal](transport); err == nil {
		t.Fatal("canonical boundary accepted whitespace")
	}
}

func TestCanonicalCodec_RejectsMapFloatAndNilContracts_AtEncodeBoundary(t *testing.T) {
	if _, err := EncodeCanonical(mapContract{
		SchemaVersion: SchemaVersionV1,
		Values:        map[string]string{"a": "b"},
	}); err == nil {
		t.Fatal("canonical encoder accepted a map")
	}
	if _, err := EncodeCanonical(floatContract{
		SchemaVersion: SchemaVersionV1,
		Value:         1.5,
	}); err == nil {
		t.Fatal("canonical encoder accepted binary floating point")
	}
	var goal *Goal
	if _, err := EncodeCanonical(goal); err == nil {
		t.Fatal("canonical encoder accepted a nil contract")
	}
	if _, err := EncodeCanonical(channelContract{
		SchemaVersion: SchemaVersionV1,
		Value:         make(chan int),
	}); err == nil {
		t.Fatal("canonical encoder accepted an unsupported channel")
	}
	if _, err := EncodeCanonical(largeContract{
		SchemaVersion: SchemaVersionV1,
		Value:         strings.Repeat("x", MaxCanonicalBytes),
	}); err == nil {
		t.Fatal("canonical encoder accepted an oversized payload")
	}
	if _, err := HashCanonical(&Goal{}); err == nil {
		t.Fatal("canonical hasher accepted an invalid contract")
	}
}

func TestCanonicalHash_IsStableForKnownGoal(t *testing.T) {
	goal := validGoal()
	hash, err := HashCanonical(&goal)
	if err != nil {
		t.Fatalf("hash valid goal: %v", err)
	}
	const expected = "d3f284b0342f35f7a9b4c38e96a162e460d8d1f34b8a3e66fdc239e32ce29292"
	if hash.Algorithm != "sha256" || hash.Digest != expected {
		t.Fatalf("canonical hash changed: want sha256:%s, got %s:%s", expected, hash.Algorithm, hash.Digest)
	}
}

func TestOrganization_RequiresExactSevenDepartmentsAndThreeSeats(t *testing.T) {
	organization := validOrganization()
	if err := organization.Validate(); err != nil {
		t.Fatalf("valid organization rejected: %v", err)
	}

	missingDepartment := organization
	missingDepartment.Departments = append([]Department(nil), organization.Departments[:6]...)
	if err := missingDepartment.Validate(); err == nil {
		t.Fatal("organization accepted fewer than seven departments")
	}

	duplicateDepartment := organization
	duplicateDepartment.Departments = append([]Department(nil), organization.Departments...)
	duplicateDepartment.Departments[6].Kind = duplicateDepartment.Departments[0].Kind
	if err := duplicateDepartment.Validate(); err == nil {
		t.Fatal("organization accepted a duplicate department")
	}

	missingSeat := organization
	missingSeat.Departments = append([]Department(nil), organization.Departments...)
	department := missingSeat.Departments[0]
	department.Seats = append([]Seat(nil), department.Seats[:2]...)
	missingSeat.Departments[0] = department
	if err := missingSeat.Validate(); err == nil {
		t.Fatal("department accepted fewer than three seats")
	}

	duplicateRole := organization
	duplicateRole.Departments = append([]Department(nil), organization.Departments...)
	department = duplicateRole.Departments[0]
	department.Seats = append([]Seat(nil), department.Seats...)
	department.Seats[2].Role = SeatLead
	duplicateRole.Departments[0] = department
	if err := duplicateRole.Validate(); err == nil {
		t.Fatal("department accepted duplicate seat roles")
	}
}

func TestWorkPacket_RejectsAuthorityMismatchAndAuditorProjectMemory(t *testing.T) {
	packet := validWorkPacket()
	if err := packet.Validate(); err != nil {
		t.Fatalf("valid work packet rejected: %v", err)
	}

	wrongSeat := packet
	wrongSeat.Seat.ID = "seat-other"
	if err := wrongSeat.Validate(); err == nil {
		t.Fatal("work packet accepted a seat outside lease authority")
	}

	wrongMandate := packet
	wrongMandate.Mandate.Version++
	if err := wrongMandate.Validate(); err == nil {
		t.Fatal("work packet accepted a mandate outside lease authority")
	}

	wrongTenant := packet
	wrongTenant.Intent.OrganizationID = "org-other"
	if err := wrongTenant.Validate(); err == nil {
		t.Fatal("work packet accepted another tenant's intent")
	}

	wrongMailbox := packet
	wrongMailbox.Inbox = append([]MessageEnvelope(nil), packet.Inbox...)
	wrongMailbox.Inbox[0].To = []SeatAddress{{
		OrganizationID: packet.Seat.OrganizationID,
		DepartmentID:   packet.Seat.DepartmentID,
		SeatID:         "developer-executor",
	}}
	if err := wrongMailbox.Validate(); err == nil {
		t.Fatal("work packet exposed another seat's mailbox")
	}

	auditorMemory := packet
	auditorMemory.Seat.Role = SeatAuditor
	if err := auditorMemory.Validate(); err == nil {
		t.Fatal("work packet exposed Project Brain to an Auditor")
	}

	nonDeveloperMemory := packet
	nonDeveloperMemory.Mandate.DepartmentKind = DepartmentExecutive
	if err := nonDeveloperMemory.Validate(); err == nil {
		t.Fatal("work packet exposed Project Brain outside Developer")
	}
}

func TestCoreContracts_ValidateRealCanonicalValues(t *testing.T) {
	values := []Validatable{
		ptr(validGoal()),
		ptr(validIntent()),
		ptr(validRecord()),
		ptr(validMessage()),
		ptr(validLease()),
		ptr(validWorkPacket()),
		ptr(validReceipt()),
		ptr(validVerdict()),
		ptr(validPolicy()),
		ptr(validOrganization()),
	}
	for _, value := range values {
		if err := value.Validate(); err != nil {
			t.Fatalf("%T rejected valid value: %v", value, err)
		}
		if _, err := EncodeCanonical(value); err != nil {
			t.Fatalf("%T failed canonical encoding: %v", value, err)
		}
	}
}

func TestCoreContracts_RejectInvalidEnumsHashesAndTimes(t *testing.T) {
	if DepartmentKind("unknown").Valid() || SeatRole("unknown").Valid() ||
		Classification("unknown").Valid() || RecordKind("unknown").Valid() ||
		Validity("unknown").Valid() || MessageKind("unknown").Valid() ||
		TimeoutAction("unknown").Valid() || WakeDisposition("unknown").Valid() ||
		VerdictOutcome("unknown").Valid() {
		t.Fatal("closed enum accepted an unknown value")
	}
	if err := (ContentHash{Algorithm: "sha1", Digest: strings.Repeat("a", 64)}).Validate(); err == nil {
		t.Fatal("hash accepted a non-SHA-256 algorithm")
	}
	if err := (ContentHash{Algorithm: "sha256", Digest: "abc"}).Validate(); err == nil {
		t.Fatal("hash accepted the wrong digest length")
	}
	if err := (ContentHash{Algorithm: "sha256", Digest: strings.Repeat("A", 64)}).Validate(); err == nil {
		t.Fatal("hash accepted uppercase hexadecimal")
	}
	if err := (Signature{Algorithm: "rsa", KeyID: "owner-key", Value: validSignature().Value}).Validate(); err == nil {
		t.Fatal("signature accepted an unsupported algorithm")
	}
	if err := (FenceToken(0)).Validate(); err == nil {
		t.Fatal("fence accepted zero")
	}

	goal := validGoal()
	goal.CreatedAt = goal.CreatedAt.In(time.FixedZone("not-utc", 3600))
	if err := goal.Validate(); err == nil {
		t.Fatal("goal accepted a non-UTC timestamp")
	}
}

func ptr[T any](value T) *T {
	return &value
}

func validHash(character string) ContentHash {
	return ContentHash{Algorithm: "sha256", Digest: strings.Repeat(character, 64)}
}

func validSignature() Signature {
	return Signature{
		Algorithm: "ed25519",
		KeyID:     "owner-key",
		Value:     base64.RawURLEncoding.EncodeToString(make([]byte, ed25519.SignatureSize)),
	}
}

func validTime() time.Time {
	return time.Date(2026, time.July, 30, 12, 0, 0, 123, time.UTC)
}

func validGoal() Goal {
	return Goal{
		SchemaVersion:   SchemaVersionV1,
		ID:              "goal-1",
		OrganizationID:  "org-1",
		WorkOrderID:     "order-1",
		Title:           "Create canonical Workforce contracts",
		SuccessCriteria: []string{"Canonical round trip is stable"},
		CreatedAt:       validTime(),
	}
}

func validIntent() Intent {
	return Intent{
		SchemaVersion:  SchemaVersionV1,
		ID:             "intent-1",
		OrganizationID: "org-1",
		GoalID:         "goal-1",
		OwnerSeatID:    "developer-lead",
		Summary:        "Define production contract types",
		Priority:       10,
		CreatedAt:      validTime(),
	}
}

func validArtifact(id ArtifactID) ArtifactRef {
	return ArtifactRef{
		SchemaVersion: SchemaVersionV1,
		ID:            id,
		Hash:          validHash("a"),
		MediaType:     "application/json",
		SizeBytes:     128,
	}
}

func validEvidence() EvidenceRef {
	return EvidenceRef{
		SchemaVersion: SchemaVersionV1,
		ID:            "evidence-1",
		Hash:          validHash("b"),
		Kind:          "test_result",
		ObservedAt:    validTime().Add(time.Minute),
	}
}

func validPolicyRef() PolicyRef {
	return PolicyRef{ID: "policy-1", Version: 1, Hash: validHash("c")}
}

func validModel() ModelBinding {
	return ModelBinding{
		SchemaVersion:  SchemaVersionV1,
		ID:             "model-binding-1",
		Provider:       "mimo",
		ModelID:        "mimo-v2.5-pro",
		ModelVersion:   "mimo-v2.5-pro",
		SamplingDigest: validHash("d"),
	}
}

func validRuntime() RuntimeBinding {
	return RuntimeBinding{
		BuildDigest:             validHash("e"),
		AuditorBuildDigest:      validHash("f"),
		OperationRegistryDigest: validHash("a"),
	}
}

func validMGS() MGSGenomeRef {
	return MGSGenomeRef{Reference: "mgs-1", Digest: validHash("1")}
}

func validSeat(role SeatRole, kind DepartmentKind) Seat {
	return Seat{
		SchemaVersion:  SchemaVersionV1,
		ID:             SeatID(string(kind) + "-" + string(role)),
		Version:        1,
		DID:            SeatDID("did:matrix:" + string(kind) + ":" + string(role)),
		OrganizationID: "org-1",
		DepartmentID:   DepartmentID("department-" + string(kind)),
		Role:           role,
		MandateID:      MandateID("mandate-" + string(kind) + "-" + string(role)),
		MandateVersion: 1,
		BindingID:      SeatBindingID("binding-" + string(kind) + "-" + string(role)),
		BindingVersion: 1,
		EffectiveAt:    validTime(),
		Signature:      validSignature(),
	}
}

func validMandate() Mandate {
	return Mandate{
		SchemaVersion:  SchemaVersionV1,
		ID:             "mandate-developer-lead",
		Version:        1,
		OrganizationID: "org-1",
		DepartmentKind: DepartmentDeveloper,
		SeatRole:       SeatLead,
		AllowedSkills:  []SkillID{"implement", "plan"},
		DataScopes: []DataScope{{
			Name:           "source",
			Classification: ClassificationProject,
			Purpose:        "Implement the selected intent",
		}},
		EscalationRules: []EscalationRule{{
			Condition: "Authority is missing",
			Action:    "Escalate to the human owner",
		}},
		Prohibitions: []Prohibition{{
			ClauseID:    "no-production",
			Description: "Cannot deploy production",
		}},
		EffectiveAt: validTime().Add(-time.Hour),
		Signature:   validSignature(),
	}
}

func validLease() WakeLease {
	return WakeLease{
		SchemaVersion:      SchemaVersionV1,
		ID:                 "lease-1",
		WakeID:             "wake-1",
		OrganizationID:     "org-1",
		SeatID:             "developer-lead",
		SeatDID:            "did:matrix:developer:lead",
		Reason:             "eligible_work",
		MandateID:          "mandate-developer-lead",
		MandateVersion:     1,
		Policies:           []PolicyRef{validPolicyRef()},
		GraphScope:         []IntentID{"intent-1"},
		Model:              validModel(),
		MGS:                validMGS(),
		Runtime:            validRuntime(),
		SkillCatalogDigest: validHash("2"),
		Budget: WakeBudget{
			MaxDurationMillis: uint64((30 * time.Minute) / time.Millisecond),
			MaxSteps:          20,
			MaxModelCalls:     10,
			MaxToolCalls:      40,
			MaxCostMinor:      1000,
			Currency:          "USD",
			MaxOutputBytes:    1 << 20,
		},
		IssuedAt:  validTime(),
		ExpiresAt: validTime().Add(time.Hour),
		Fence:     1,
		Signature: validSignature(),
	}
}

func validMessage() MessageEnvelope {
	return MessageEnvelope{
		SchemaVersion: SchemaVersionV1,
		ID:            "message-1",
		ThreadID:      "thread-1",
		From: SeatAddress{
			OrganizationID: "org-1",
			DepartmentID:   "department-developer",
			SeatID:         "developer-lead",
		},
		To: []SeatAddress{{
			OrganizationID: "org-1",
			DepartmentID:   "department-developer",
			SeatID:         "developer-executor",
		}},
		CC:      []SeatAddress{},
		Kind:    MessageRequest,
		Subject: "Implement canonical contracts",
		Payload: MessagePayloadRef{
			SchemaID: "workforce.mail.request.v1",
			Artifact: validArtifact("artifact-message-payload"),
		},
		ParentIntentID: "intent-1",
		RequiredAction: "Return typed artifacts and evidence",
		Artifacts:      []ArtifactRef{},
		Evidence:       []EvidenceRef{},
		Priority:       10,
		TimeoutAction:  TimeoutEscalate,
		Classification: ClassificationDepartment,
		IdempotencyKey: "message-idempotency-1",
		CreatedAt:      validTime(),
		ExpiresAt:      validTime().Add(time.Hour),
		Signature:      validSignature(),
	}
}

func validProjectBrain() ProjectBrainRef {
	return ProjectBrainRef{
		SchemaVersion: SchemaVersionV1,
		ProjectID:     "project-1",
		WorkspaceID:   "workspace-1",
		Source: SourceState{
			RootDigest:      validHash("3"),
			GraphGeneration: 1,
			LedgerCursor:    1,
		},
		ViewDigest:   validHash("4"),
		Fresh:        true,
		PendingFiles: []string{},
		ExpiresAt:    validTime().Add(time.Hour),
	}
}

func validWorkPacket() WorkPacket {
	packet := WorkPacket{
		SchemaVersion:   SchemaVersionV1,
		Lease:           validLease(),
		Seat:            validSeat(SeatLead, DepartmentDeveloper),
		Mandate:         validMandate(),
		Goal:            validGoal(),
		Intent:          validIntent(),
		VerifiedState:   []RecordRef{},
		Dependencies:    []IntentID{},
		Artifacts:       []ArtifactRef{validArtifact("artifact-1")},
		Evidence:        []EvidenceRef{validEvidence()},
		Inbox:           []MessageEnvelope{validMessage()},
		Tools:           []ToolRef{{Name: "codegraph", SchemaDigest: validHash("5")}},
		Skills:          []SkillRef{{ID: "implement", Version: 1, Digest: validHash("6")}},
		Policies:        []PolicyRef{validPolicyRef()},
		RequiredOutputs: []RequiredOutput{{Kind: "source_change", SuccessPredicate: "go test passes"}},
		ProjectBrain:    ptr(validProjectBrain()),
		AssembledAt:     validTime().Add(time.Minute),
	}
	packet.Inbox[0].To[0] = SeatAddress{
		OrganizationID: packet.Seat.OrganizationID,
		DepartmentID:   packet.Seat.DepartmentID,
		SeatID:         packet.Seat.ID,
	}
	return packet
}

func validRecord() Record {
	departmentID := DepartmentID("department-developer")
	projectID := ProjectID("project-1")
	return Record{
		SchemaVersion:  SchemaVersionV1,
		ID:             "record-1",
		OrganizationID: "org-1",
		Kind:           RecordArtifact,
		AuthorSeatID:   "developer-lead",
		DepartmentID:   &departmentID,
		ProjectID:      &projectID,
		Purpose:        "Implement the selected project intent",
		ParentIntentID: "intent-1",
		CreatedAt:      validTime(),
		EffectiveAt:    validTime(),
		Validity:       ValidityActive,
		PayloadSchema:  "workforce.record.artifact.v1",
		Payload:        validArtifact("artifact-record-payload"),
		ContentHash:    validHash("7"),
		Provenance:     []RecordRef{},
		Classification: ClassificationProject,
		Signature:      validSignature(),
	}
}

func validPolicy() Policy {
	return Policy{
		SchemaVersion:  SchemaVersionV1,
		ID:             "policy-1",
		Version:        1,
		OrganizationID: "org-1",
		Kind:           "autonomy",
		EffectiveAt:    validTime(),
		Rules: []PolicyRule{{
			ClauseID: "deny-production",
			Outcome:  "deny",
			Scope:    "production effects",
		}},
		Signature: validSignature(),
	}
}

func validReceipt() Receipt {
	return Receipt{
		SchemaVersion:  SchemaVersionV1,
		ID:             "receipt-1",
		OrganizationID: "org-1",
		DepartmentID:   "department-developer",
		WakeID:         "wake-1",
		LeaseID:        "lease-1",
		SeatID:         "developer-lead",
		SeatDID:        "did:matrix:developer:lead",
		MandateID:      "mandate-developer-lead",
		MandateVersion: 1,
		ParentIntentID: "intent-1",
		ChildIntentIDs: []IntentID{},
		Inputs:         []RecordRef{},
		Constraints:    []string{"lease and policy bounds"},
		Approvals:      []ApprovalID{},
		Policies:       []PolicyRef{validPolicyRef()},
		Operations: []OperationLineage{{
			Name: "inspect", EffectClass: "read", Digest: validHash("d"),
			Outcome: "succeeded",
		}},
		Artifacts:      []ArtifactRef{validArtifact("artifact-1")},
		Evidence:       []EvidenceRef{validEvidence()},
		Reconciliation: []ReconciliationLineage{},
		Model:          validModel(),
		MGS:            validMGS(),
		Runtime:        validRuntime(),
		Source: SourceState{
			RootDigest:      validHash("8"),
			GraphGeneration: 1,
			LedgerCursor:    1,
		},
		Skill:             SkillRef{ID: "implement", Version: 1, Digest: validHash("9")},
		VerifierDigest:    validHash("a"),
		ModelRequestHash:  validHash("d"),
		ModelResponseHash: validHash("e"),
		CostMinor:         10,
		Currency:          "USD",
		LatencyMillis:     50,
		Disposition:       DispositionProgressed,
		UnresolvedRisk:    "",
		CreatedAt:         validTime().Add(time.Minute),
		ContentHash:       validHash("b"),
		Signature:         validSignature(),
	}
}

func validVerdict() Verdict {
	return Verdict{
		SchemaVersion:  SchemaVersionV1,
		ID:             "verdict-1",
		OrganizationID: "org-1",
		IntentID:       "intent-1",
		AuditorSeatID:  "developer-auditor",
		Outcome:        VerdictPass,
		VerifierDigest: validHash("c"),
		Evidence:       []EvidenceRef{validEvidence()},
		ReasonCode:     "verified",
		CreatedAt:      validTime().Add(time.Minute),
		Signature:      validSignature(),
	}
}

func validOrganization() Organization {
	departments := make([]Department, 0, len(AllDepartmentKinds()))
	for _, kind := range AllDepartmentKinds() {
		departmentID := DepartmentID("department-" + string(kind))
		seats := make([]Seat, 0, len(AllSeatRoles()))
		for _, role := range AllSeatRoles() {
			seat := validSeat(role, kind)
			seat.DepartmentID = departmentID
			seats = append(seats, seat)
		}
		departments = append(departments, Department{
			SchemaVersion:  SchemaVersionV1,
			ID:             departmentID,
			OrganizationID: "org-1",
			Kind:           kind,
			Seats:          seats,
			Enabled:        true,
		})
	}
	return Organization{
		SchemaVersion: SchemaVersionV1,
		ID:            "org-1",
		OwnerID:       "owner-1",
		Version:       1,
		Name:          "Pax Labs",
		Departments:   departments,
		EffectiveAt:   validTime(),
		Signature:     validSignature(),
	}
}
