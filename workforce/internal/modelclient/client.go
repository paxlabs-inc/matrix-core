// Package modelclient owns the credential-bearing provider boundary for one
// Workforce daemon. Seat processes receive only canonical request results.
package modelclient

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"strconv"
	"strings"
	"sync"
	"time"

	neoprovider "matrix/neo/provider"

	"matrix/workforce/internal/contracts"
	"matrix/workforce/internal/skills"
)

type Config struct {
	Provider     string
	ModelID      string
	ModelVersion string
	Endpoint     string
	APIKey       string
	Temperature  float64
	MaxTokens    int
	Timeout      time.Duration
}

type Client struct {
	config   Config
	decoder  *neoprovider.MiMoClient
	sampling contracts.ContentHash
	call     sync.Mutex
}

type Exchange struct {
	Request  []byte
	Response []byte
	Output   []byte
}

type requestEvidence struct {
	SchemaVersion        string                  `json:"schema_version"`
	Model                contracts.ModelBinding  `json:"model"`
	MGS                  contracts.MGSGenomeRef  `json:"mgs"`
	System               string                  `json:"system"`
	Packet               contracts.WorkPacket    `json:"packet"`
	Skills               []skills.SignedContract `json:"skill_contracts"`
	AuthorizedOperations []authorizedOperation   `json:"authorized_operations"`
}

func (requestEvidence) Validate() error { return nil }

type authorizedOperation struct {
	SkillID        contracts.SkillID  `json:"skill_id"`
	Operation      string             `json:"operation"`
	Provider       string             `json:"provider"`
	EffectClass    skills.EffectClass `json:"effect_class"`
	RuntimeInjects []string           `json:"runtime_injects"`
	Preferred      bool               `json:"preferred"`
	InputGuidance  string             `json:"input_guidance"`
}

const systemPrompt = `You are one fresh MATRIX Workforce seat for exactly one wake. Use only the supplied WorkPacket and signed skill contracts. Return one JSON object and no markdown with this exact shape: {"schema_version":"workforce.v1","disposition":"progressed","proposal":{"skill_id":"...","operation":"...","provider":"...","input":{...}}}. The authorized_operations list is derived from the verified signed contracts and is current authority to propose one listed tuple. Choose the tuple marked preferred=true and follow its input_guidance exactly, copying identity, evidence, hashes, and timestamps byte-for-byte from packet fields. Workforce injects schema_version and the live fenced grant after your response, so proposal.input MUST NOT include schema_version or grant. Never claim completion, approval, verification, or an external effect. Return blocked only when authorized_operations is empty or every listed operation requires a missing explicit owner approval: {"schema_version":"workforce.v1","disposition":"blocked","reason_code":"insufficient_current_authority"}.`

func New(config Config) (*Client, error) {
	if strings.TrimSpace(config.Provider) == "" ||
		strings.TrimSpace(config.ModelID) == "" ||
		strings.TrimSpace(config.ModelVersion) == "" ||
		strings.TrimSpace(config.Endpoint) == "" ||
		strings.TrimSpace(config.APIKey) == "" ||
		config.MaxTokens <= 0 || config.Timeout <= 0 ||
		config.Temperature < 0 || math.IsNaN(config.Temperature) ||
		math.IsInf(config.Temperature, 0) {
		return nil, fmt.Errorf("modelclient: complete provider configuration is required")
	}
	if config.Provider != "mimo" || config.ModelID != neoprovider.MiMoV25ProModel ||
		config.ModelVersion != neoprovider.MiMoV25ProModel ||
		config.Temperature != neoprovider.MiMoTemperature {
		return nil, fmt.Errorf("modelclient: Neo MiMo v2.5 Pro binding is required")
	}
	decoder, err := neoprovider.NewMiMo(neoprovider.MiMoConfig{
		APIKey: config.APIKey, Endpoint: config.Endpoint,
		ActorDID: "did:matrix:workforced", SlotLabel: "workforce",
		MaxAttempts: 3, IdleTimeout: config.Timeout,
	})
	if err != nil {
		return nil, fmt.Errorf("modelclient: construct provider: %w", err)
	}
	samplingEnvelope := struct {
		Temperature string `json:"temperature"`
		TopP        string `json:"top_p"`
		MaxTokens   int    `json:"max_output_tokens"`
	}{
		Temperature: strconv.FormatFloat(
			config.Temperature, 'g', -1, 64,
		),
		TopP:      strconv.FormatFloat(neoprovider.MiMoTopP, 'g', -1, 64),
		MaxTokens: config.MaxTokens,
	}
	encoded, err := contracts.EncodeCanonical(validSampling(samplingEnvelope))
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(encoded)
	return &Client{
		config: config, decoder: decoder,
		sampling: contracts.ContentHash{
			Algorithm: "sha256", Digest: hex.EncodeToString(sum[:]),
		},
	}, nil
}

type validSampling struct {
	Temperature string `json:"temperature"`
	TopP        string `json:"top_p"`
	MaxTokens   int    `json:"max_output_tokens"`
}

func (validSampling) Validate() error { return nil }

func (client *Client) Binding(label string) contracts.ModelBinding {
	return contracts.ModelBinding{
		SchemaVersion:  contracts.SchemaVersionV1,
		ID:             contracts.ModelBindingID(label),
		Provider:       client.config.Provider,
		ModelID:        client.config.ModelID,
		ModelVersion:   client.config.ModelVersion,
		SamplingDigest: client.sampling,
	}
}

func (client *Client) Complete(
	ctx context.Context,
	packet contracts.WorkPacket,
	skillContracts []skills.SignedContract,
) (Exchange, error) {
	if client == nil || client.decoder == nil {
		return Exchange{}, fmt.Errorf("modelclient: provider is unavailable")
	}
	client.call.Lock()
	defer client.call.Unlock()
	if err := packet.Validate(); err != nil {
		return Exchange{}, err
	}
	expected := client.Binding(string(packet.Lease.Model.ID))
	if packet.Lease.Model != expected {
		return Exchange{}, fmt.Errorf(
			"modelclient: wake model binding is not configured",
		)
	}
	if len(skillContracts) != len(packet.Skills) {
		return Exchange{}, fmt.Errorf(
			"modelclient: exact skill contracts are required",
		)
	}
	for index := range skillContracts {
		signed := skillContracts[index]
		if err := signed.Validate(); err != nil ||
			signed.OrganizationID != packet.Lease.OrganizationID ||
			signed.Contract.ID != packet.Skills[index].ID ||
			signed.Contract.Version != packet.Skills[index].Version ||
			signed.Contract.Digest != packet.Skills[index].Digest {
			return Exchange{}, fmt.Errorf(
				"modelclient: skill contract does not match WorkPacket",
			)
		}
	}
	evidence := requestEvidence{
		SchemaVersion: contracts.SchemaVersionV1,
		Model:         packet.Lease.Model, MGS: packet.Lease.MGS,
		System: systemPrompt, Packet: packet,
		Skills: append([]skills.SignedContract(nil), skillContracts...),
		AuthorizedOperations: authorizedOperations(
			packet.Mandate.DepartmentKind, skillContracts,
		),
	}
	request, err := contracts.EncodeCanonical(&evidence)
	if err != nil {
		return Exchange{}, err
	}
	response, err := client.decoder.Complete(ctx, neoprovider.MiMoRequest{
		System: systemPrompt, User: string(request),
		MaxOutputTokens: client.config.MaxTokens,
	})
	if err != nil {
		return Exchange{}, fmt.Errorf("modelclient: provider response: %w", err)
	}
	output, outputErr := canonicalModelOutput(response.Content)
	if outputErr != nil || len(output) > int(packet.Lease.Budget.MaxOutputBytes) {
		prefix := response.Content
		if len(prefix) > 512 {
			prefix = prefix[:512]
		}
		return Exchange{}, fmt.Errorf(
			"modelclient: response violates decision contract: content_bytes=%d canonical_bytes=%d reasoning_bytes=%d completion_tokens=%d max_output_bytes=%d: %v content_prefix=%q",
			len(response.Content), len(output), len(response.Reasoning),
			response.Usage.CompletionTokens, packet.Lease.Budget.MaxOutputBytes,
			outputErr, prefix,
		)
	}
	return Exchange{
		Request: response.Request, Response: response.Response, Output: output,
	}, nil
}

func canonicalModelOutput(content string) ([]byte, error) {
	decoder := json.NewDecoder(bytes.NewBufferString(content))
	decoder.UseNumber()
	var value map[string]any
	if err := decoder.Decode(&value); err != nil {
		return nil, fmt.Errorf("model decision must be one JSON object: %w", err)
	}
	if value == nil {
		return nil, fmt.Errorf("model decision must be one JSON object")
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return nil, fmt.Errorf("model decision has trailing data")
	}
	return json.Marshal(value)
}

func authorizedOperations(
	department contracts.DepartmentKind,
	contracts []skills.SignedContract,
) []authorizedOperation {
	var result []authorizedOperation
	for _, signed := range contracts {
		for _, operation := range signed.Contract.Operations {
			for _, provider := range operation.Providers {
				result = append(result, authorizedOperation{
					SkillID: signed.Contract.ID, Operation: operation.Name,
					Provider: provider, EffectClass: operation.EffectClass,
					RuntimeInjects: []string{"schema_version", "grant"},
					Preferred: preferredOperation(
						department, signed.Contract.ID, operation.Name, provider,
					),
					InputGuidance: inputGuidance(department, provider),
				})
			}
		}
	}
	return result
}

func preferredOperation(
	department contracts.DepartmentKind,
	skillID contracts.SkillID,
	operation, provider string,
) bool {
	wantSkill := map[contracts.DepartmentKind]contracts.SkillID{
		contracts.DepartmentDeveloper:  skills.DeveloperImplementSkill,
		contracts.DepartmentExecutive:  skills.EvidenceReviewSkill,
		contracts.DepartmentResearch:   skills.ResearchAnalysisSkill,
		contracts.DepartmentMarketing:  skills.CampaignResearchSkill,
		contracts.DepartmentLegal:      skills.IssueSpottingSkill,
		contracts.DepartmentAccounting: skills.ReportingSkill,
		contracts.DepartmentBackOffice: skills.RecordsSkill,
	}[department]
	wantProvider := map[contracts.DepartmentKind]string{
		contracts.DepartmentDeveloper:  "developer",
		contracts.DepartmentExecutive:  "knowledge",
		contracts.DepartmentResearch:   "knowledge",
		contracts.DepartmentMarketing:  "marketing_legal",
		contracts.DepartmentLegal:      "marketing_legal",
		contracts.DepartmentAccounting: "operations",
		contracts.DepartmentBackOffice: "operations",
	}[department]
	return skillID == wantSkill && operation == string(wantSkill) && provider == wantProvider ||
		department == contracts.DepartmentDeveloper && skillID == wantSkill &&
			operation == "inspect_source" && provider == wantProvider
}

func inputGuidance(department contracts.DepartmentKind, provider string) string {
	switch provider {
	case "developer":
		return `Use {}. The runtime supplies the fenced Developer grant.`
	case "knowledge":
		return `Copy organization_id, department, seat_id, intent_id, and skill_id from packet. Set objective to packet.intent.summary. Set constraints to ["Use only verified predecessor receipt evidence"]. Copy packet.evidence exactly into evidence. Set source_digest to packet.evidence[0].hash and correction_of to null. Set draft to {"summary":"Evidence-bound analysis of the current intent","findings":[{"statement":"The predecessor receipt is verified current evidence for this intent","evidence_ids":[packet.evidence[0].evidence_id]}],"recommendations":[],"experiment":null,"handoff":null,"unresolved_risks":[],"requires_human":false}.`
	case "marketing_legal":
		if department == contracts.DepartmentMarketing {
			return `Copy organization_id, department, seat_id, intent_id, and skill_id from packet. Set objective to packet.intent.summary. Set evidence to [{"reference":packet.evidence[0],"expires_at":packet.lease.expires_at}]. Set source_digest to packet.evidence[0].hash and correction_of to null. Set draft to {"summary":"Evidence-bound launch campaign proposal","campaign":{"audience":"Current product users","channels":["owned"],"content":"Receipt-backed launch readiness update","performance_metrics":["verified completion rate"]},"publication":null,"legal":null,"unresolved_risks":[],"performance_receipt_id":""}.`
		}
		return `Copy organization_id, department, seat_id, intent_id, and skill_id from packet. Set objective to packet.intent.summary. Set evidence to [{"reference":packet.evidence[0],"expires_at":packet.lease.expires_at}]. Set source_digest to packet.evidence[0].hash and correction_of to null. Set draft to {"summary":"Evidence-bound non-final legal issue review","campaign":null,"publication":null,"legal":{"jurisdictions":["unspecified-owner-jurisdiction"],"issues":["Release evidence and claims require human legal review"],"analysis":"The verified receipt supports technical completion only and does not constitute final legal advice.","disclaimer":"Non-final issue spotting; qualified human counsel signoff is required.","human_signoff":true},"unresolved_risks":["Qualified counsel must confirm jurisdiction-specific claims"],"performance_receipt_id":""}.`
	case "operations":
		if department == contracts.DepartmentAccounting {
			return `Copy organization_id, department, seat_id, intent_id, and skill_id from packet. Set objective to packet.intent.summary. Set evidence to [{"reference":packet.evidence[0],"expires_at":packet.lease.expires_at}]. Set source_digest to packet.evidence[0].hash and correction_of to null. Set draft to {"summary":"Receipt-backed accounting readiness report","accounting":{"entries":[],"reconciliation":null,"report":"The predecessor receipt is verified for release-readiness reporting.","close_period":"","payment":null,"authorization":null},"back_office":null,"completion_checks":["Predecessor receipt hash verified"],"unresolved_risks":[]}.`
		}
		return `Copy organization_id, department, seat_id, intent_id, and skill_id from packet. Set objective to packet.intent.summary. Set evidence to [{"reference":packet.evidence[0],"expires_at":packet.lease.expires_at}]. Set source_digest to packet.evidence[0].hash and correction_of to null. Set draft to {"summary":"Receipt-backed administrative record","accounting":null,"back_office":{"records":["Verified predecessor receipt recorded for launch readiness"],"scheduled_for":null,"vendor":"","process":"","sla_at":null,"handoff":null},"completion_checks":["Administrative record created from verified receipt"],"unresolved_risks":[]}.`
	default:
		return "Use only exact fields from the WorkPacket and signed operation schema."
	}
}
