package contracts

import (
	"strings"
	"testing"
	"time"
)

func TestIdentityContracts_RejectEveryInvalidAuthorityShape(t *testing.T) {
	organizationCases := []struct {
		name   string
		mutate func(*Organization)
	}{
		{"schema", func(v *Organization) { v.SchemaVersion = "workforce.v2" }},
		{"organization id", func(v *Organization) { v.ID = "" }},
		{"owner id", func(v *Organization) { v.OwnerID = "" }},
		{"version", func(v *Organization) { v.Version = 0 }},
		{"name", func(v *Organization) { v.Name = "" }},
		{"effective time", func(v *Organization) { v.EffectiveAt = time.Time{} }},
		{"signature", func(v *Organization) { v.Signature.Value = "" }},
		{"department count", func(v *Organization) { v.Departments = v.Departments[:6] }},
		{"invalid department", func(v *Organization) { v.Departments[0].ID = "" }},
		{"department organization", func(v *Organization) { v.Departments[0].OrganizationID = "org-2" }},
		{"duplicate department", func(v *Organization) { v.Departments[6].Kind = v.Departments[0].Kind }},
	}
	for _, test := range organizationCases {
		t.Run("organization "+test.name, func(t *testing.T) {
			value := validOrganization()
			test.mutate(&value)
			assertInvalid(t, &value)
		})
	}

	departmentCases := []struct {
		name   string
		mutate func(*Department)
	}{
		{"schema", func(v *Department) { v.SchemaVersion = "bad" }},
		{"id", func(v *Department) { v.ID = "" }},
		{"organization", func(v *Department) { v.OrganizationID = "" }},
		{"kind", func(v *Department) { v.Kind = "unknown" }},
		{"seat count", func(v *Department) { v.Seats = v.Seats[:2] }},
		{"invalid seat", func(v *Department) { v.Seats[0].ID = "" }},
		{"seat ownership", func(v *Department) { v.Seats[0].DepartmentID = "other" }},
		{"duplicate role", func(v *Department) { v.Seats[2].Role = v.Seats[0].Role }},
	}
	for _, test := range departmentCases {
		t.Run("department "+test.name, func(t *testing.T) {
			value := validOrganization().Departments[0]
			test.mutate(&value)
			assertInvalid(t, &value)
		})
	}

	seatCases := []struct {
		name   string
		mutate func(*Seat)
	}{
		{"schema", func(v *Seat) { v.SchemaVersion = "bad" }},
		{"id", func(v *Seat) { v.ID = "" }},
		{"did", func(v *Seat) { v.DID = "" }},
		{"organization", func(v *Seat) { v.OrganizationID = "" }},
		{"department", func(v *Seat) { v.DepartmentID = "" }},
		{"mandate", func(v *Seat) { v.MandateID = "" }},
		{"binding", func(v *Seat) { v.BindingID = "" }},
		{"role", func(v *Seat) { v.Role = "unknown" }},
		{"versions", func(v *Seat) { v.MandateVersion = 0 }},
	}
	for _, test := range seatCases {
		t.Run("seat "+test.name, func(t *testing.T) {
			value := validSeat(SeatLead, DepartmentDeveloper)
			test.mutate(&value)
			assertInvalid(t, &value)
		})
	}
}

func TestMandateContracts_RejectMissingLimitsAndInvalidClauses(t *testing.T) {
	mandateCases := []struct {
		name   string
		mutate func(*Mandate)
	}{
		{"schema", func(v *Mandate) { v.SchemaVersion = "bad" }},
		{"id", func(v *Mandate) { v.ID = "" }},
		{"organization", func(v *Mandate) { v.OrganizationID = "" }},
		{"version", func(v *Mandate) { v.Version = 0 }},
		{"department", func(v *Mandate) { v.DepartmentKind = "unknown" }},
		{"role", func(v *Mandate) { v.SeatRole = "unknown" }},
		{"skills empty", func(v *Mandate) { v.AllowedSkills = nil }},
		{"skill invalid", func(v *Mandate) { v.AllowedSkills = []SkillID{""} }},
		{"skills unsorted", func(v *Mandate) { v.AllowedSkills = []SkillID{"plan", "implement"} }},
		{"skills duplicate", func(v *Mandate) { v.AllowedSkills = []SkillID{"plan", "plan"} }},
		{"scopes empty", func(v *Mandate) { v.DataScopes = nil }},
		{"scope invalid", func(v *Mandate) { v.DataScopes[0].Name = "" }},
		{"escalation invalid", func(v *Mandate) { v.EscalationRules[0].Condition = "" }},
		{"prohibition invalid", func(v *Mandate) { v.Prohibitions[0].ClauseID = "" }},
		{"effective time", func(v *Mandate) { v.EffectiveAt = time.Time{} }},
		{"expiry zone", func(v *Mandate) {
			value := validTime().In(time.FixedZone("bad", 3600))
			v.ExpiresAt = &value
		}},
		{"expiry order", func(v *Mandate) {
			value := v.EffectiveAt.Add(-time.Second)
			v.ExpiresAt = &value
		}},
		{"signature", func(v *Mandate) { v.Signature.Value = "" }},
	}
	for _, test := range mandateCases {
		t.Run(test.name, func(t *testing.T) {
			value := validMandate()
			test.mutate(&value)
			assertInvalid(t, &value)
		})
	}

	scope := DataScope{Name: "source", Classification: ClassificationProject, Purpose: "read"}
	if err := scope.Validate(); err != nil {
		t.Fatalf("valid scope: %v", err)
	}
	scope.Classification = "bad"
	assertInvalid(t, scope)
	scope = DataScope{Name: "source", Classification: ClassificationProject, Purpose: ""}
	assertInvalid(t, scope)

	assertInvalid(t, EscalationRule{Condition: "", Action: "escalate"})
	assertInvalid(t, EscalationRule{Condition: "blocked", Action: ""})
	assertInvalid(t, Prohibition{ClauseID: "", Description: "no"})
	assertInvalid(t, Prohibition{ClauseID: "no-effect", Description: ""})
}

func TestWorkContracts_RejectInvalidGraphRecordArtifactAndPolicyValues(t *testing.T) {
	goalCases := []func(*Goal){
		func(v *Goal) { v.ID = "" },
		func(v *Goal) { v.OrganizationID = "" },
		func(v *Goal) { v.WorkOrderID = "" },
		func(v *Goal) { v.Title = "" },
		func(v *Goal) { v.SuccessCriteria = nil },
		func(v *Goal) { v.SuccessCriteria[0] = "" },
		func(v *Goal) { v.CreatedAt = time.Time{} },
	}
	for i, mutate := range goalCases {
		t.Run("goal "+string(rune('a'+i)), func(t *testing.T) {
			value := validGoal()
			mutate(&value)
			assertInvalid(t, &value)
		})
	}

	intentCases := []func(*Intent){
		func(v *Intent) { v.SchemaVersion = "bad" },
		func(v *Intent) { v.ID = "" },
		func(v *Intent) { v.OrganizationID = "" },
		func(v *Intent) { v.GoalID = "" },
		func(v *Intent) { v.OwnerSeatID = "" },
		func(v *Intent) { value := IntentID(""); v.ParentIntentID = &value },
		func(v *Intent) { value := v.ID; v.ParentIntentID = &value },
		func(v *Intent) { v.Summary = "" },
		func(v *Intent) { v.Priority = 1001 },
		func(v *Intent) { v.CreatedAt = time.Time{} },
		func(v *Intent) { value := v.CreatedAt; v.Deadline = &value },
	}
	for i, mutate := range intentCases {
		t.Run("intent "+string(rune('a'+i)), func(t *testing.T) {
			value := validIntent()
			mutate(&value)
			assertInvalid(t, &value)
		})
	}

	ref := RecordRef{ID: "record-1", Kind: RecordGoal, Hash: validHash("a")}
	if err := ref.Validate(); err != nil {
		t.Fatalf("valid record ref: %v", err)
	}
	ref.ID = ""
	assertInvalid(t, ref)
	ref = RecordRef{ID: "record-1", Kind: "bad", Hash: validHash("a")}
	assertInvalid(t, ref)
	ref = RecordRef{ID: "record-1", Kind: RecordGoal, Hash: ContentHash{}}
	assertInvalid(t, ref)

	artifactCases := []func(*ArtifactRef){
		func(v *ArtifactRef) { v.SchemaVersion = "bad" },
		func(v *ArtifactRef) { v.ID = "" },
		func(v *ArtifactRef) { v.Hash = ContentHash{} },
		func(v *ArtifactRef) { v.MediaType = "" },
		func(v *ArtifactRef) { v.SizeBytes = 0 },
	}
	for _, mutate := range artifactCases {
		value := validArtifact("artifact-1")
		mutate(&value)
		assertInvalid(t, &value)
	}

	evidenceCases := []func(*EvidenceRef){
		func(v *EvidenceRef) { v.SchemaVersion = "bad" },
		func(v *EvidenceRef) { v.ID = "" },
		func(v *EvidenceRef) { v.Hash = ContentHash{} },
		func(v *EvidenceRef) { v.Kind = "" },
		func(v *EvidenceRef) { v.ObservedAt = time.Time{} },
	}
	for _, mutate := range evidenceCases {
		value := validEvidence()
		mutate(&value)
		assertInvalid(t, &value)
	}

	recordCases := []func(*Record){
		func(v *Record) { v.SchemaVersion = "bad" },
		func(v *Record) { v.ID = "" },
		func(v *Record) { v.OrganizationID = "" },
		func(v *Record) { v.AuthorSeatID = "" },
		func(v *Record) { value := DepartmentID(""); v.DepartmentID = &value },
		func(v *Record) { value := SeatID(""); v.AccessSeatID = &value },
		func(v *Record) { value := ProjectID(""); v.ProjectID = &value },
		func(v *Record) { v.Purpose = "" },
		func(v *Record) { v.ParentIntentID = "" },
		func(v *Record) { v.Kind = "bad" },
		func(v *Record) { v.Validity = "bad" },
		func(v *Record) { v.Classification = "bad" },
		func(v *Record) { v.Classification = ClassificationDepartment; v.DepartmentID = nil },
		func(v *Record) { v.Classification = ClassificationSeat; v.AccessSeatID = nil },
		func(v *Record) { v.Classification = ClassificationProject; v.ProjectID = nil },
		func(v *Record) { v.PayloadSchema = "bad" },
		func(v *Record) { v.CreatedAt = time.Time{} },
		func(v *Record) { v.Payload.ID = "" },
		func(v *Record) { v.ContentHash = ContentHash{} },
		func(v *Record) { v.Provenance = []RecordRef{{}} },
		func(v *Record) { value := RecordID(""); v.Supersedes = &value },
		func(v *Record) { value := RecordID(""); v.Retracts = &value },
		func(v *Record) {
			supersedes := RecordID("record-old")
			retracts := RecordID("record-retracted")
			v.Supersedes = &supersedes
			v.Retracts = &retracts
		},
		func(v *Record) { v.Signature.Value = "" },
	}
	for _, mutate := range recordCases {
		value := validRecord()
		mutate(&value)
		assertInvalid(t, &value)
	}

	policyRef := validPolicyRef()
	policyRef.ID = ""
	assertInvalid(t, policyRef)
	policyRef = validPolicyRef()
	policyRef.Version = 0
	assertInvalid(t, policyRef)
	policyRef = validPolicyRef()
	policyRef.Hash = ContentHash{}
	assertInvalid(t, policyRef)

	assertInvalid(t, PolicyRule{ClauseID: "", Outcome: "deny", Scope: "all"})
	assertInvalid(t, PolicyRule{ClauseID: "rule", Outcome: "bad", Scope: "all"})
	assertInvalid(t, PolicyRule{ClauseID: "rule", Outcome: "deny", Scope: ""})
	policyCases := []func(*Policy){
		func(v *Policy) { v.SchemaVersion = "bad" },
		func(v *Policy) { v.ID = "" },
		func(v *Policy) { v.OrganizationID = "" },
		func(v *Policy) { v.Version = 0 },
		func(v *Policy) { v.Kind = "" },
		func(v *Policy) { v.EffectiveAt = time.Time{} },
		func(v *Policy) { value := v.EffectiveAt; v.ExpiresAt = &value },
		func(v *Policy) { v.Rules = nil },
		func(v *Policy) { v.Rules[0].ClauseID = "" },
		func(v *Policy) { v.Signature.Value = "" },
	}
	for _, mutate := range policyCases {
		value := validPolicy()
		mutate(&value)
		assertInvalid(t, &value)
	}
}

func TestWireContracts_RejectInvalidLineageLeaseAndPacketValues(t *testing.T) {
	modelCases := []func(*ModelBinding){
		func(v *ModelBinding) { v.SchemaVersion = "bad" },
		func(v *ModelBinding) { v.ID = "" },
		func(v *ModelBinding) { v.Provider = "" },
		func(v *ModelBinding) { v.ModelID = "" },
		func(v *ModelBinding) { v.ModelVersion = "" },
		func(v *ModelBinding) { value := ContentHash{}; v.WeightsDigest = &value },
		func(v *ModelBinding) { v.SamplingDigest = ContentHash{} },
	}
	for _, mutate := range modelCases {
		value := validModel()
		mutate(&value)
		assertInvalid(t, &value)
	}
	assertInvalid(t, MGSGenomeRef{Reference: "", Digest: validHash("a")})
	assertInvalid(t, MGSGenomeRef{Reference: "mgs", Digest: ContentHash{}})
	assertInvalid(t, RuntimeBinding{
		BuildDigest: ContentHash{}, AuditorBuildDigest: validHash("a"),
		OperationRegistryDigest: validHash("b"),
	})
	assertInvalid(t, RuntimeBinding{
		BuildDigest: validHash("a"), AuditorBuildDigest: ContentHash{},
		OperationRegistryDigest: validHash("b"),
	})
	assertInvalid(t, RuntimeBinding{
		BuildDigest: validHash("a"), AuditorBuildDigest: validHash("b"),
		OperationRegistryDigest: ContentHash{},
	})
	assertInvalid(t, SourceState{RootDigest: ContentHash{}, GraphGeneration: 1})
	assertInvalid(t, SourceState{RootDigest: validHash("a"), GraphGeneration: 0})

	budget := validLease().Budget
	budget.MaxDurationMillis = 0
	assertInvalid(t, budget)
	budget = validLease().Budget
	budget.MaxSteps = 0
	assertInvalid(t, budget)
	budget = validLease().Budget
	budget.MaxModelCalls = 26
	assertInvalid(t, budget)
	budget = validLease().Budget
	budget.MaxCostMinor = -1
	assertInvalid(t, budget)
	budget = validLease().Budget
	budget.Currency = ""
	assertInvalid(t, budget)
	budget = validLease().Budget
	budget.MaxOutputBytes = 0
	assertInvalid(t, budget)

	leaseCases := []func(*WakeLease){
		func(v *WakeLease) { v.SchemaVersion = "bad" },
		func(v *WakeLease) { v.ID = "" },
		func(v *WakeLease) { v.WakeID = "" },
		func(v *WakeLease) { v.OrganizationID = "" },
		func(v *WakeLease) { v.SeatID = "" },
		func(v *WakeLease) { v.SeatDID = "" },
		func(v *WakeLease) { v.MandateID = "" },
		func(v *WakeLease) { v.Reason = "" },
		func(v *WakeLease) { v.MandateVersion = 0 },
		func(v *WakeLease) { v.Policies = nil },
		func(v *WakeLease) { v.Policies[0].ID = "" },
		func(v *WakeLease) { v.GraphScope = nil },
		func(v *WakeLease) { v.GraphScope[0] = "" },
		func(v *WakeLease) { v.Model.ID = "" },
		func(v *WakeLease) { v.MGS.Reference = "" },
		func(v *WakeLease) { v.Runtime.BuildDigest = ContentHash{} },
		func(v *WakeLease) { v.Runtime.AuditorBuildDigest = ContentHash{} },
		func(v *WakeLease) { v.SkillCatalogDigest = ContentHash{} },
		func(v *WakeLease) { v.Budget.MaxSteps = 0 },
		func(v *WakeLease) { v.IssuedAt = time.Time{} },
		func(v *WakeLease) {
			v.Budget.MaxDurationMillis = uint64((2 * time.Hour) / time.Millisecond)
			v.ExpiresAt = v.IssuedAt.Add(time.Hour)
		},
		func(v *WakeLease) { v.Fence = 0 },
		func(v *WakeLease) { v.Signature.Value = "" },
	}
	for _, mutate := range leaseCases {
		value := validLease()
		mutate(&value)
		assertInvalid(t, &value)
	}

	assertInvalid(t, ToolRef{Name: "", SchemaDigest: validHash("a")})
	assertInvalid(t, ToolRef{Name: "tool", SchemaDigest: ContentHash{}})
	assertInvalid(t, SkillRef{ID: "", Version: 1, Digest: validHash("a")})
	assertInvalid(t, SkillRef{ID: "skill", Version: 0, Digest: validHash("a")})
	assertInvalid(t, SkillRef{ID: "skill", Version: 1, Digest: ContentHash{}})
	assertInvalid(t, RequiredOutput{Kind: "", SuccessPredicate: "pass"})
	assertInvalid(t, RequiredOutput{Kind: "artifact", SuccessPredicate: ""})

	packetCases := []func(*WorkPacket){
		func(v *WorkPacket) { v.SchemaVersion = "bad" },
		func(v *WorkPacket) { v.Lease.ID = "" },
		func(v *WorkPacket) { v.Seat.ID = "" },
		func(v *WorkPacket) { v.Mandate.ID = "" },
		func(v *WorkPacket) { v.Goal.ID = "" },
		func(v *WorkPacket) { v.Intent.ID = "" },
		func(v *WorkPacket) { v.Seat.Role = SeatAuditor; v.ProjectBrain = nil },
		func(v *WorkPacket) { v.Seat.ID = "other" },
		func(v *WorkPacket) { v.Mandate.Version = 2 },
		func(v *WorkPacket) { v.Intent.GoalID = "other" },
		func(v *WorkPacket) { v.AssembledAt = v.Lease.ExpiresAt },
		func(v *WorkPacket) { v.RequiredOutputs = nil },
		func(v *WorkPacket) { v.Policies[0].Version = 2 },
		func(v *WorkPacket) { v.VerifiedState = []RecordRef{{}} },
		func(v *WorkPacket) { v.Dependencies = []IntentID{""} },
		func(v *WorkPacket) { v.Artifacts[0].ID = "" },
		func(v *WorkPacket) { v.Evidence[0].ID = "" },
		func(v *WorkPacket) { v.Inbox[0].ID = "" },
		func(v *WorkPacket) { v.Tools[0].Name = "" },
		func(v *WorkPacket) { v.Skills[0].ID = "" },
		func(v *WorkPacket) { v.Skills[0].ID = "unapproved" },
		func(v *WorkPacket) { v.Policies[0].ID = "" },
		func(v *WorkPacket) { v.RequiredOutputs[0].Kind = "" },
		func(v *WorkPacket) { v.ProjectBrain.ProjectID = "" },
		func(v *WorkPacket) { v.ProjectBrain.ExpiresAt = v.AssembledAt },
	}
	for _, mutate := range packetCases {
		value := validWorkPacket()
		mutate(&value)
		assertInvalid(t, &value)
	}
}

func TestMailReceiptVerdictAndProjectContracts_RejectInvalidValues(t *testing.T) {
	address := validMessage().From
	address.OrganizationID = ""
	assertInvalid(t, address)
	address = validMessage().From
	address.DepartmentID = ""
	assertInvalid(t, address)
	address = validMessage().From
	address.SeatID = ""
	assertInvalid(t, address)
	payload := validMessage().Payload
	payload.SchemaID = ""
	assertInvalid(t, payload)
	payload = validMessage().Payload
	payload.Artifact.ID = ""
	assertInvalid(t, payload)

	messageCases := []func(*MessageEnvelope){
		func(v *MessageEnvelope) { v.SchemaVersion = "bad" },
		func(v *MessageEnvelope) { v.ID = "" },
		func(v *MessageEnvelope) { v.ThreadID = "" },
		func(v *MessageEnvelope) { value := MessageID(""); v.InReplyTo = &value },
		func(v *MessageEnvelope) { value := v.ID; v.InReplyTo = &value },
		func(v *MessageEnvelope) { v.From.SeatID = "" },
		func(v *MessageEnvelope) { v.To = nil },
		func(v *MessageEnvelope) { v.To[0].SeatID = "" },
		func(v *MessageEnvelope) { v.CC = []SeatAddress{{}} },
		func(v *MessageEnvelope) { v.Kind = "bad" },
		func(v *MessageEnvelope) { v.Subject = "" },
		func(v *MessageEnvelope) { v.Payload.SchemaID = "" },
		func(v *MessageEnvelope) { v.Payload.SchemaID = "workforce.mail.answer.v1" },
		func(v *MessageEnvelope) { v.ParentIntentID = "" },
		func(v *MessageEnvelope) { v.RequiredAction = "" },
		func(v *MessageEnvelope) { v.Artifacts = []ArtifactRef{{}} },
		func(v *MessageEnvelope) { v.Evidence = []EvidenceRef{{}} },
		func(v *MessageEnvelope) { v.Priority = 1001 },
		func(v *MessageEnvelope) { value := v.CreatedAt; v.Deadline = &value },
		func(v *MessageEnvelope) { v.TimeoutAction = "bad" },
		func(v *MessageEnvelope) { v.Classification = "bad" },
		func(v *MessageEnvelope) { v.IdempotencyKey = "" },
		func(v *MessageEnvelope) { v.CreatedAt = time.Time{} },
		func(v *MessageEnvelope) { v.Signature.Value = "" },
	}
	for _, mutate := range messageCases {
		value := validMessage()
		mutate(&value)
		assertInvalid(t, &value)
	}

	receiptCases := []func(*Receipt){
		func(v *Receipt) { v.SchemaVersion = "bad" },
		func(v *Receipt) { v.ID = "" },
		func(v *Receipt) { v.OrganizationID = "" },
		func(v *Receipt) { v.WakeID = "" },
		func(v *Receipt) { v.LeaseID = "" },
		func(v *Receipt) { v.SeatID = "" },
		func(v *Receipt) { v.ParentIntentID = "" },
		func(v *Receipt) { v.ChildIntentIDs = []IntentID{""} },
		func(v *Receipt) { v.Inputs = []RecordRef{{}} },
		func(v *Receipt) { v.Policies[0].ID = "" },
		func(v *Receipt) { v.Artifacts[0].ID = "" },
		func(v *Receipt) { v.Evidence[0].ID = "" },
		func(v *Receipt) { v.Model.ID = "" },
		func(v *Receipt) { v.MGS.Reference = "" },
		func(v *Receipt) { v.Runtime.BuildDigest = ContentHash{} },
		func(v *Receipt) { v.Runtime.AuditorBuildDigest = ContentHash{} },
		func(v *Receipt) { v.Source.GraphGeneration = 0 },
		func(v *Receipt) { v.Skill.ID = "" },
		func(v *Receipt) { v.VerifierDigest = ContentHash{} },
		func(v *Receipt) { value := VerdictID(""); v.VerdictID = &value },
		func(v *Receipt) { v.CostMinor = -1 },
		func(v *Receipt) { v.Currency = "" },
		func(v *Receipt) { v.Disposition = "bad" },
		func(v *Receipt) { v.UnresolvedRisk = strings.Repeat("x", 2049) },
		func(v *Receipt) { v.CreatedAt = time.Time{} },
		func(v *Receipt) { v.ContentHash = ContentHash{} },
		func(v *Receipt) { v.Signature.Value = "" },
	}
	for _, mutate := range receiptCases {
		value := validReceipt()
		mutate(&value)
		assertInvalid(t, &value)
	}

	verdictCases := []func(*Verdict){
		func(v *Verdict) { v.SchemaVersion = "bad" },
		func(v *Verdict) { v.ID = "" },
		func(v *Verdict) { v.OrganizationID = "" },
		func(v *Verdict) { v.IntentID = "" },
		func(v *Verdict) { v.AuditorSeatID = "" },
		func(v *Verdict) { v.Outcome = "bad" },
		func(v *Verdict) { v.VerifierDigest = ContentHash{} },
		func(v *Verdict) { v.Evidence[0].ID = "" },
		func(v *Verdict) { v.ReasonCode = "" },
		func(v *Verdict) { v.CreatedAt = time.Time{} },
		func(v *Verdict) { v.Signature.Value = "" },
	}
	for _, mutate := range verdictCases {
		value := validVerdict()
		mutate(&value)
		assertInvalid(t, &value)
	}

	projectCases := []func(*ProjectBrainRef){
		func(v *ProjectBrainRef) { v.SchemaVersion = "bad" },
		func(v *ProjectBrainRef) { v.ProjectID = "" },
		func(v *ProjectBrainRef) { v.WorkspaceID = "" },
		func(v *ProjectBrainRef) { v.Source.GraphGeneration = 0 },
		func(v *ProjectBrainRef) { v.ViewDigest = ContentHash{} },
		func(v *ProjectBrainRef) { v.PendingFiles = []string{"pending.go"} },
		func(v *ProjectBrainRef) { v.Fresh = false; v.PendingFiles = nil },
		func(v *ProjectBrainRef) { v.Fresh = false; v.PendingFiles = []string{""} },
		func(v *ProjectBrainRef) { v.ExpiresAt = time.Time{} },
	}
	for _, mutate := range projectCases {
		value := validProjectBrain()
		mutate(&value)
		assertInvalid(t, &value)
	}
}

func TestLowLevelValidation_RejectsInvalidIdentifiersAndSignatures(t *testing.T) {
	if err := validateID("id", strings.Repeat("a", 129)); err == nil {
		t.Fatal("identifier accepted more than 128 bytes")
	}
	if err := validateID("id", "contains space"); err == nil {
		t.Fatal("identifier accepted an invalid character")
	}
	if err := validateID("id", "valid:id_1.2-3"); err != nil {
		t.Fatalf("valid identifier rejected: %v", err)
	}
	if err := (Signature{Algorithm: "ed25519", KeyID: "", Value: validSignature().Value}).Validate(); err == nil {
		t.Fatal("signature accepted an empty key id")
	}
	if err := (Signature{Algorithm: "ed25519", KeyID: "key", Value: ""}).Validate(); err == nil {
		t.Fatal("signature accepted an empty value")
	}
	if err := (Signature{Algorithm: "ed25519", KeyID: "key", Value: "***"}).Validate(); err == nil {
		t.Fatal("signature accepted non-base64url data")
	}
	if !hasAdjacentDuplicate([]string{"a", "a"}) {
		t.Fatal("duplicate detector missed adjacent duplicate")
	}
	if hasAdjacentDuplicate([]string{"a", "b"}) {
		t.Fatal("duplicate detector rejected unique values")
	}
}

func assertInvalid(t *testing.T, value Validatable) {
	t.Helper()
	if err := value.Validate(); err == nil {
		t.Fatalf("%T accepted invalid value: %#v", value, value)
	}
}
