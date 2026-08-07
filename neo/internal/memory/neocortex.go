// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package memory

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"matrix/codegraph/selfmodel"
	cgstore "matrix/codegraph/store"
	"matrix/cortexclient"
	"matrix/neo/internal/config"
)

const (
	stateCheckpoint           = "neo.neocortex.memory-state.v1" // read-only migration source
	recordsCheckpoint         = "neo.neocortex.semantic-records.v1"
	consentCheckpoint         = "neo.neocortex.consent.v1"
	opportunitiesCheckpoint   = "neo.neocortex.opportunities.v1"
	patternsCheckpoint        = "neo.neocortex.patterns.v1"
	personalizationCheckpoint = "neo.neocortex.personalization.v1"
	structuralCheckpoint      = "neo.neocortex.structural-identity.v1"
	diagnosticsCheckpoint     = "neo.neocortex.diagnostics.v1"
	failuresCheckpoint        = "neo.neocortex.learned-failures.v1"
	knowledgeCheckpoint       = "neo.neocortex.knowledge.v1"
	consentNotice             = "2026-07-17"
)

var ErrNeocortexUnavailable = errors.New("neo/memory: Neocortex unavailable")

type storedRecord struct {
	URI          string          `json:"uri"`
	Type         string          `json:"type"`
	Identity     string          `json:"identity"`
	Text         string          `json:"text"`
	Data         json.RawMessage `json:"data,omitempty"`
	Version      uint64          `json:"version"`
	CreatedAt    time.Time       `json:"created_at"`
	UpdatedAt    time.Time       `json:"updated_at"`
	Deleted      bool            `json:"deleted,omitempty"`
	LSN          uint64          `json:"lsn,omitempty"`
	Conversation string          `json:"conversation,omitempty"`
	SeqLo        uint64          `json:"seq_lo,omitempty"`
	SeqHi        uint64          `json:"seq_hi,omitempty"`
}

type durableState struct {
	Consent                MemoryConsentState         `json:"consent"`
	Records                map[string]storedRecord    `json:"records"`
	Opportunities          map[string]OpportunitySpec `json:"opportunities"`
	Patterns               map[string]Pattern         `json:"patterns"`
	Personalization        *PersonalizationProfile    `json:"personalization,omitempty"`
	PersonalizationVersion uint64                     `json:"personalization_version,omitempty"`
	Structural             StructuralSelf             `json:"structural"`
	StructuralURI          string                     `json:"structural_uri,omitempty"`
	Failures               []FailurePattern           `json:"failures,omitempty"`
	Deaths                 []DeathRecord              `json:"deaths,omitempty"`
	Knowledge              KnowledgeState             `json:"knowledge"`
}

type personalizationState struct {
	Profile *PersonalizationProfile `json:"profile,omitempty"`
	Version uint64                  `json:"version,omitempty"`
}

type structuralState struct {
	Structural StructuralSelf `json:"structural"`
	URI        string         `json:"uri,omitempty"`
	Record     storedRecord   `json:"record"`
}

type diagnosticState struct {
	Deaths []DeathRecord `json:"deaths,omitempty"`
}

// CapabilityProvenance is the typed, prompt-invisible Neocortex projection of
// one Capability Hub audit event. The Hub database remains the lifecycle
// authority; this checkpoint stream gives every install and state change a
// durable cognitive provenance trail without adding packages to recall or
// canonical context assembly.
type CapabilityProvenance struct {
	SchemaVersion int       `json:"schema_version"`
	AuditID       int64     `json:"audit_id"`
	Slug          string    `json:"slug"`
	Version       string    `json:"version,omitempty"`
	Action        string    `json:"action"`
	Detail        string    `json:"detail,omitempty"`
	OccurredAt    time.Time `json:"occurred_at"`
}

type Pager struct {
	cfg     config.Config
	client  *cortexclient.Client
	managed *cortexclient.Supervisor

	mu         sync.RWMutex
	writeMu    sync.Mutex
	state      durableState
	activeGoal string
}

func Open(cfg config.Config) (*Pager, error) {
	socket := strings.TrimSpace(cfg.NeocortexSocket)
	var managed *cortexclient.Supervisor
	if socket == "" && strings.HasPrefix(filepath.Clean(cfg.DataRoot), filepath.Clean(os.TempDir())+string(os.PathSeparator)) {
		var err error
		socket, cfg.NeocortexToken, managed, err = startManagedNeocortex(cfg.DataRoot)
		if err != nil {
			return nil, err
		}
	}
	if socket == "" {
		return nil, fmt.Errorf("%w: NEO_NEOCORTEX_SOCKET is required", ErrNeocortexUnavailable)
	}
	rawToken, err := hex.DecodeString(strings.TrimSpace(cfg.NeocortexToken))
	if err != nil || len(rawToken) != 32 {
		if managed != nil {
			managed.Stop()
		}
		return nil, fmt.Errorf("%w: NEO_NEOCORTEX_TOKEN must be 64 hexadecimal characters", ErrNeocortexUnavailable)
	}
	var token [32]byte
	copy(token[:], rawToken)
	digest := sha256.Sum256([]byte("neo-neocortex-connection-v1\x00" + cfg.NeocortexActor))
	var connection [16]byte
	copy(connection[:], digest[:16])
	client, err := cortexclient.Dial(cortexclient.Config{
		SocketPath: socket, CapabilityToken: token, ConnectionID: connection,
	})
	if err != nil {
		if managed != nil {
			managed.Stop()
		}
		return nil, fmt.Errorf("%w: %v", ErrNeocortexUnavailable, err)
	}
	p := &Pager{cfg: cfg, client: client, managed: managed}
	p.state.Records = make(map[string]storedRecord)
	p.state.Opportunities = make(map[string]OpportunitySpec)
	p.state.Patterns = make(map[string]Pattern)
	p.state.Consent = defaultConsent()
	if blob, _, loadErr := client.LatestCheckpoint(context.Background(), stateCheckpoint); loadErr == nil {
		if err := json.Unmarshal(blob, &p.state); err != nil {
			_ = client.Close()
			if managed != nil {
				managed.Stop()
			}
			return nil, fmt.Errorf("neo/memory: decode Neocortex state: %w", err)
		}
	} else if !errors.Is(loadErr, cortexclient.ErrAbsent) {
		_ = client.Close()
		if managed != nil {
			managed.Stop()
		}
		return nil, fmt.Errorf("%w: load state: %v", ErrNeocortexUnavailable, loadErr)
	}
	if p.state.Records == nil {
		p.state.Records = make(map[string]storedRecord)
	}
	if p.state.Opportunities == nil {
		p.state.Opportunities = make(map[string]OpportunitySpec)
	}
	if p.state.Patterns == nil {
		p.state.Patterns = make(map[string]Pattern)
	}
	// Overlay independently persisted domains. The legacy whole-state blob is
	// migration input only and is never rewritten.
	load := func(key string, target any) error {
		blob, _, loadErr := client.LatestCheckpoint(context.Background(), key)
		if errors.Is(loadErr, cortexclient.ErrAbsent) {
			return nil
		}
		if loadErr != nil {
			return loadErr
		}
		return json.Unmarshal(blob, target)
	}
	if err := load(recordsCheckpoint, &p.state.Records); err != nil {
		_ = p.Close()
		return nil, fmt.Errorf("neo/memory: load semantic records: %w", err)
	}
	if err := load(consentCheckpoint, &p.state.Consent); err != nil {
		_ = p.Close()
		return nil, fmt.Errorf("neo/memory: load consent: %w", err)
	}
	if err := load(opportunitiesCheckpoint, &p.state.Opportunities); err != nil {
		_ = p.Close()
		return nil, fmt.Errorf("neo/memory: load opportunities: %w", err)
	}
	if err := load(patternsCheckpoint, &p.state.Patterns); err != nil {
		_ = p.Close()
		return nil, fmt.Errorf("neo/memory: load patterns: %w", err)
	}
	var personalization personalizationState
	if err := load(personalizationCheckpoint, &personalization); err != nil {
		_ = p.Close()
		return nil, fmt.Errorf("neo/memory: load personalization: %w", err)
	}
	if personalization.Profile != nil || personalization.Version != 0 {
		p.state.Personalization = personalization.Profile
		p.state.PersonalizationVersion = personalization.Version
	}
	var structural structuralState
	if err := load(structuralCheckpoint, &structural); err != nil {
		_ = p.Close()
		return nil, fmt.Errorf("neo/memory: load structural identity: %w", err)
	}
	if structural.URI != "" {
		p.state.Structural = structural.Structural
		p.state.StructuralURI = structural.URI
		p.state.Records["structural-self"] = structural.Record
	}
	var diagnostics diagnosticState
	if err := load(diagnosticsCheckpoint, &diagnostics); err != nil {
		_ = p.Close()
		return nil, fmt.Errorf("neo/memory: load diagnostics: %w", err)
	}
	if diagnostics.Deaths != nil {
		p.state.Deaths = diagnostics.Deaths
	}
	if err := load(failuresCheckpoint, &p.state.Failures); err != nil {
		_ = p.Close()
		return nil, fmt.Errorf("neo/memory: load learned failures: %w", err)
	}
	if err := load(knowledgeCheckpoint, &p.state.Knowledge); err != nil {
		_ = p.Close()
		return nil, fmt.Errorf("neo/memory: load knowledge: %w", err)
	}
	p.state.Knowledge.initialize()
	if p.state.Records == nil {
		p.state.Records = make(map[string]storedRecord)
	}
	if p.state.Opportunities == nil {
		p.state.Opportunities = make(map[string]OpportunitySpec)
	}
	if p.state.Patterns == nil {
		p.state.Patterns = make(map[string]Pattern)
	}
	if modelPath := filepath.Join(cfg.SelfModelGraph, "self-model.json"); strings.TrimSpace(cfg.SelfModelGraph) != "" {
		if info, statErr := os.Stat(modelPath); statErr == nil && !info.IsDir() {
			if _, loadErr := p.LoadStructuralSelf(context.Background()); loadErr != nil {
				_ = p.Close()
				return nil, fmt.Errorf("neo/memory: load structural self: %w", loadErr)
			}
		}
	}
	return p, nil
}

func (p *Pager) Close() error {
	if p == nil || p.client == nil {
		return nil
	}
	err := p.client.Close()
	if p.managed != nil {
		p.managed.Stop()
	}
	return err
}

func startManagedNeocortex(root string) (string, string, *cortexclient.Supervisor, error) {
	binary, err := findNeocortexBinary()
	if err != nil {
		return "", "", nil, err
	}
	directory := filepath.Join(root, "neocortex")
	if err := os.MkdirAll(filepath.Join(directory, "data"), 0700); err != nil {
		return "", "", nil, err
	}
	randomHex := func(bytes int) (string, error) {
		raw := make([]byte, bytes)
		if _, err := rand.Read(raw); err != nil {
			return "", err
		}
		return hex.EncodeToString(raw), nil
	}
	user, err := randomHex(16)
	if err != nil {
		return "", "", nil, err
	}
	kek, err := randomHex(32)
	if err != nil {
		return "", "", nil, err
	}
	seed, err := randomHex(32)
	if err != nil {
		return "", "", nil, err
	}
	admin, err := randomHex(32)
	if err != nil {
		return "", "", nil, err
	}
	actor, err := randomHex(32)
	if err != nil {
		return "", "", nil, err
	}
	socketDigest := sha256.Sum256([]byte(directory))
	socket := filepath.Join(os.TempDir(), "neo-nc-"+hex.EncodeToString(socketDigest[:8])+".sock")
	configPath := filepath.Join(directory, "engine.conf")
	content := fmt.Sprintf("socket=%s\ndata=%s\nuser=%s\nkek=%s\nsigning_seed=%s\nadmin_token=%s\nactor=1:%s\n", socket, filepath.Join(directory, "data"), user, kek, seed, admin, actor)
	if err := os.WriteFile(configPath, []byte(content), 0600); err != nil {
		return "", "", nil, err
	}
	supervisor := &cortexclient.Supervisor{Binary: binary, ConfigPath: configPath, RestartDelay: 100 * time.Millisecond}
	if err := supervisor.Start(); err != nil {
		return "", "", nil, err
	}
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if info, err := os.Stat(socket); err == nil && info.Mode()&os.ModeSocket != 0 {
			return socket, actor, supervisor, nil
		}
		time.Sleep(20 * time.Millisecond)
	}
	supervisor.Stop()
	return "", "", nil, fmt.Errorf("%w: managed engine socket did not become ready", ErrNeocortexUnavailable)
}

func findNeocortexBinary() (string, error) {
	if path := strings.TrimSpace(os.Getenv("NEOCORTEX_CORTEXD")); path != "" {
		return path, nil
	}
	for _, path := range []string{"/opt/matrix/bin/cortexd"} {
		if info, err := os.Stat(path); err == nil && !info.IsDir() {
			return path, nil
		}
	}
	directory, _ := os.Getwd()
	for {
		for _, relative := range []string{filepath.Join("neocortex", "build", "debug", "cortexd"), filepath.Join("neocortex", "build", "repro-a", "cortexd")} {
			path := filepath.Join(directory, relative)
			if info, err := os.Stat(path); err == nil && !info.IsDir() {
				return path, nil
			}
		}
		parent := filepath.Dir(directory)
		if parent == directory {
			break
		}
		directory = parent
	}
	return "", fmt.Errorf("%w: Neocortex binary not found", ErrNeocortexUnavailable)
}

func (p *Pager) NeocortexClient() *cortexclient.Client {
	if p == nil {
		return nil
	}
	return p.client
}

func (p *Pager) NewNeocortexLoopSeam(conversationID string, budget int) (*cortexclient.LoopSeam, error) {
	if p == nil || p.client == nil {
		return nil, ErrNeocortexUnavailable
	}
	if budget <= 0 {
		budget = p.cfg.ActivationBudget()
	}
	return cortexclient.NewLoopSeam(p.client, cortexclient.SeamConfig{
		ConversationID: conversationID, BudgetTokens: uint64(budget),
	})
}

func (p *Pager) Activate(ctx context.Context, conversationID, query string, budget int) (*cortexclient.Bundle, error) {
	seam, err := p.NewNeocortexLoopSeam(conversationID, budget)
	if err != nil {
		return nil, err
	}
	return seam.ActivateBundle(ctx, cortexclient.ActivationQuery{Query: query})
}

func (p *Pager) SetActiveGoal(goal string) {
	p.mu.Lock()
	p.activeGoal = strings.TrimSpace(goal)
	p.mu.Unlock()
}
func (p *Pager) ActiveGoal() string { p.mu.RLock(); defer p.mu.RUnlock(); return p.activeGoal }
func (p *Pager) HasEmbedder() bool  { return p != nil && p.client != nil }
func (p *Pager) Embedder() Embedder { return nil }

func defaultConsent() MemoryConsentState {
	return MemoryConsentState{NoticeVersion: consentNotice, ExistingData: "retained until explicitly edited or deleted"}
}

func (p *Pager) saveDomainLocked(ctx context.Context, key string, value any) error {
	blob, err := json.Marshal(value)
	if err != nil {
		return err
	}
	_, err = p.client.WriteCheckpoint(ctx, key, blob)
	return err
}

func (p *Pager) saveRecordsLocked(ctx context.Context) error {
	return p.saveDomainLocked(ctx, recordsCheckpoint, p.state.Records)
}

func (p *Pager) WriteCapabilityProvenance(ctx context.Context, record CapabilityProvenance) (uint64, error) {
	if p == nil || p.client == nil {
		return 0, ErrNeocortexUnavailable
	}
	record.Slug = strings.TrimSpace(record.Slug)
	record.Action = strings.TrimSpace(record.Action)
	if record.AuditID <= 0 || record.Slug == "" || record.Action == "" {
		return 0, errors.New("neo/memory: capability provenance identity is required")
	}
	if record.SchemaVersion == 0 {
		record.SchemaVersion = 1
	}
	record.OccurredAt = record.OccurredAt.UTC()
	body, err := json.Marshal(record)
	if err != nil {
		return 0, err
	}
	key := fmt.Sprintf("neo.capability-provenance.%s.%d.v1", boundedIdentity(record.Slug), record.AuditID)
	return p.client.WriteCheckpoint(ctx, key, body)
}

func (p *Pager) saveStructuralLocked(ctx context.Context) error {
	return p.saveDomainLocked(ctx, structuralCheckpoint, structuralState{
		Structural: p.state.Structural,
		URI:        p.state.StructuralURI,
		Record:     p.state.Records["structural-self"],
	})
}

func beliefType(name string) uint8 {
	switch strings.ToLower(name) {
	case "preference":
		return 1
	case "constraint":
		return 2
	case "goal", "opportunity", "pattern", "outcome", "event":
		return 3
	case "identity", "capability":
		return 4
	default:
		return 0
	}
}

func beliefTypeName(name string) string {
	switch beliefType(name) {
	case 1:
		return "preference"
	case 2:
		return "constraint"
	case 3:
		return "goal"
	case 4:
		return "identity"
	default:
		return "fact"
	}
}

func beliefID(identity string) []byte {
	digest := sha256.Sum256([]byte("neo-belief-v1\x00" + identity))
	return append([]byte(nil), digest[:16]...)
}

func recordURI(identity string, version uint64) string {
	digest := sha256.Sum256([]byte(identity))
	return fmt.Sprintf("matrix://neocortex/belief/%x#%d", digest[:12], version)
}

func boundedIdentity(identity string) string {
	identity = normalizeStatement(identity)
	if len(identity) <= 200 {
		return identity
	}
	digest := sha256.Sum256([]byte(identity))
	return identity[:150] + ":sha256:" + hex.EncodeToString(digest[:16])
}

func recordType(name string) string {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "preference":
		return "Preference"
	case "constraint":
		return "Constraint"
	case "goal":
		return "Goal"
	case "identity", "capability":
		return "Identity"
	case "opportunity":
		return "Opportunity"
	case "pattern":
		return "Pattern"
	case "outcome":
		return "Outcome"
	case "event":
		return "Event"
	default:
		return "Fact"
	}
}

func (p *Pager) put(ctx context.Context, typ, identity, text string, data any) (string, error) {
	return p.putRecord(ctx, typ, identity, text, data, true)
}

func (p *Pager) putRecord(ctx context.Context, typ, identity, text string, data any, checkpointState bool) (string, error) {
	p.writeMu.Lock()
	defer p.writeMu.Unlock()
	identity = boundedIdentity(identity)
	typ = recordType(typ)
	text = strings.TrimSpace(text)
	if identity == "" || text == "" {
		return "", errors.New("neo/memory: identity and value are required")
	}
	encoded, err := json.Marshal(data)
	if err != nil {
		return "", err
	}
	p.mu.RLock()
	existing := p.state.Records[identity]
	if !existing.Deleted && existing.URI != "" && existing.Type == typ &&
		existing.Text == text && bytes.Equal(existing.Data, encoded) {
		p.mu.RUnlock()
		return existing.URI, nil
	}
	nextVersion := existing.Version + 1
	p.mu.RUnlock()
	conversation := cortexclient.ConversationBytes("neo-memory")
	sourceAck, err := p.client.Append(ctx, []cortexclient.AppendEvent{
		cortexclient.ReasoningEvent(conversation, text),
	})
	if err != nil {
		return "", err
	}
	assertion := cortexclient.Assertion{
		BeliefID: beliefID(fmt.Sprintf("%s#%d", identity, nextVersion)), Type: beliefType(typ), CanonicalIdentity: identity,
		Value: encoded, ValidFromNs: time.Now().UTC().UnixNano(), ConflictDomain: beliefTypeName(typ) + ":" + identity,
		Provenance: [][2]uint64{{sourceAck.FirstLsn, sourceAck.LastLsn}},
	}
	ack, err := p.client.Append(ctx, []cortexclient.AppendEvent{cortexclient.AssertionEvent(conversation, assertion)})
	if err != nil {
		return "", err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	now := time.Now().UTC()
	record := p.state.Records[identity]
	if record.Version == 0 {
		record.CreatedAt = now
	}
	record.Version++
	record.URI = recordURI(identity, record.Version)
	record.Type, record.Identity, record.Text, record.Data = typ, identity, text, encoded
	record.UpdatedAt, record.Deleted = now, false
	record.LSN = ack.LastLsn
	p.state.Records[identity] = record
	if checkpointState {
		if err := p.saveRecordsLocked(ctx); err != nil {
			return "", err
		}
	}
	return record.URI, nil
}

func (p *Pager) recordList() []storedRecord {
	p.mu.RLock()
	defer p.mu.RUnlock()
	out := make([]storedRecord, 0, len(p.state.Records))
	for _, record := range p.state.Records {
		if !record.Deleted {
			out = append(out, record)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].UpdatedAt.After(out[j].UpdatedAt) })
	return out
}

func matches(record storedRecord, query string, types []string) bool {
	if len(types) > 0 {
		ok := false
		for _, typ := range types {
			if strings.EqualFold(typ, record.Type) {
				ok = true
				break
			}
		}
		if !ok {
			return false
		}
	}
	query = normalizeStatement(query)
	return query == "" || tokenOverlap(query, record.Text+" "+record.Identity) > 0
}

func (p *Pager) Retrieve(ctx context.Context, query string) ([]Snippet, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	limit := p.cfg.RetrievalTopK
	if limit <= 0 {
		limit = 8
	}
	out := make([]Snippet, 0, limit)
	for _, record := range p.recordList() {
		if !matches(record, query, nil) {
			continue
		}
		out = append(out, Snippet{Text: record.Text, URI: record.URI, Type: record.Type})
		if len(out) == limit {
			break
		}
	}
	return out, nil
}

func (p *Pager) RecallHits(ctx context.Context, query string, types []string, k int, asOf *time.Time) ([]RecallHit, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if k <= 0 {
		k = p.cfg.RecallTopK
	}
	if k <= 0 {
		k = 8
	}
	out := make([]RecallHit, 0, k)
	for _, record := range p.recordList() {
		if asOf != nil && record.UpdatedAt.After(*asOf) || !matches(record, query, types) {
			continue
		}
		out = append(out, RecallHit{URI: record.URI, Type: record.Type, Text: record.Text, Score: 1})
		if len(out) == k {
			break
		}
	}
	return out, nil
}

func (p *Pager) Recall(ctx context.Context, query string, types []string, k int, asOf *time.Time) (string, error) {
	if strings.HasPrefix(strings.TrimSpace(query), "self:") {
		return p.recallSelf(ctx, strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(query), "self:")))
	}
	hits, err := p.RecallHits(ctx, query, types, k, asOf)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	for _, hit := range hits {
		fmt.Fprintf(&b, "- [%s] %s (%s)\n", hit.Type, hit.Text, hit.URI)
	}
	return strings.TrimSpace(b.String()), nil
}

func (p *Pager) Pinned(ctx context.Context, goal string) string {
	lines := p.LearnedGuidance(ctx)
	if goal = strings.TrimSpace(goal); goal != "" {
		lines = append([]string{"Active goal: " + goal}, lines...)
	}
	return strings.Join(lines, "\n")
}

func (p *Pager) UserProfile(ctx context.Context) []string {
	if err := ctx.Err(); err != nil {
		return nil
	}
	var out []string
	for _, record := range p.recordList() {
		if strings.HasPrefix(record.Identity, "user:") || strings.EqualFold(record.Type, "preference") {
			out = append(out, record.Text)
		}
	}
	return out
}

func (p *Pager) LearnedGuidance(ctx context.Context) []string {
	if err := ctx.Err(); err != nil {
		return nil
	}
	var out []string
	for _, record := range p.recordList() {
		if strings.EqualFold(record.Type, "constraint") || strings.EqualFold(record.Type, "preference") {
			out = append(out, record.Text)
		}
	}
	return out
}

func (p *Pager) RememberFact(ctx context.Context, statement string) (string, error) {
	return p.UpsertFact(ctx, "", statement, statement)
}
func (p *Pager) RememberFactRelated(ctx context.Context, statement string, _ RelationClassifier) (string, error) {
	return p.RememberFact(ctx, statement)
}
func (p *Pager) RememberUserFact(ctx context.Context, statement string) (string, error) {
	return p.UpsertUserFact(ctx, "", statement)
}
func (p *Pager) UpsertFact(ctx context.Context, subject, predicate, statement string) (string, error) {
	return p.put(ctx, "fact", "fact:"+subject+":"+predicate+":"+statement, statement, map[string]any{"subject": subject, "predicate": predicate, "statement": statement})
}
func (p *Pager) UpsertUserFact(ctx context.Context, predicate, statement string) (string, error) {
	return p.put(ctx, "user_fact", "user:"+predicate+":"+statement, statement, map[string]any{"predicate": predicate, "statement": statement})
}
func (p *Pager) RememberPreference(ctx context.Context, topic, polarity string, strength float32, rationale string) (string, error) {
	return p.UpsertPreference(ctx, topic, polarity, strength, rationale)
}
func (p *Pager) RememberPreferenceRelated(ctx context.Context, topic, polarity string, strength float32, rationale string, _ RelationClassifier) (string, error) {
	return p.UpsertPreference(ctx, topic, polarity, strength, rationale)
}
func (p *Pager) UpsertPreference(ctx context.Context, topic, polarity string, strength float32, rationale string) (string, error) {
	text := strings.TrimSpace(topic + " " + polarity + " " + rationale)
	return p.put(ctx, "preference", "preference:"+topic, text, map[string]any{"topic": topic, "polarity": polarity, "strength": strength, "rationale": rationale})
}
func (p *Pager) RememberConstraint(ctx context.Context, statement, polarity, strength string) (string, error) {
	return p.UpsertConstraint(ctx, statement, polarity, strength)
}
func (p *Pager) RememberConstraintRelated(ctx context.Context, statement, polarity, strength string, _ RelationClassifier) (string, error) {
	return p.UpsertConstraint(ctx, statement, polarity, strength)
}
func (p *Pager) UpsertConstraint(ctx context.Context, statement, polarity, strength string) (string, error) {
	return p.put(ctx, "constraint", "constraint:"+statement, statement, map[string]string{"statement": statement, "polarity": polarity, "strength": strength})
}
func (p *Pager) RecordOutcome(ctx context.Context, summary string, outcome Outcome, intent string) (string, error) {
	return p.UpsertOutcome(ctx, summary, outcome, intent)
}
func (p *Pager) UpsertOutcome(ctx context.Context, summary string, outcome Outcome, intent string) (string, error) {
	return p.put(ctx, "outcome", "outcome:"+intent+":"+summary, summary, map[string]any{"summary": summary, "outcome": outcome, "intent": intent})
}
func (p *Pager) LinkSessionProvenance(uri, conversation string, lo, hi uint64) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	for key, record := range p.state.Records {
		if record.URI == uri {
			record.Conversation, record.SeqLo, record.SeqHi = conversation, lo, hi
			p.state.Records[key] = record
			return p.saveRecordsLocked(context.Background())
		}
	}
	return errors.New("memory record not found")
}

func (p *Pager) MemoryConsent(context.Context) (MemoryConsentState, error) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.state.Consent, nil
}
func (p *Pager) MemoryConsentEnabled() bool {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.state.Consent.Enabled
}
func (p *Pager) SetMemoryConsent(ctx context.Context, enabled bool, by string) (MemoryConsentState, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.state.Consent = defaultConsent()
	p.state.Consent.Enabled = enabled
	p.state.Consent.Explicit = true
	p.state.Consent.UpdatedAt = time.Now().UTC().Format(time.RFC3339Nano)
	p.state.Consent.UpdatedBy = strings.TrimSpace(by)
	if err := p.saveDomainLocked(ctx, consentCheckpoint, p.state.Consent); err != nil {
		return MemoryConsentState{}, err
	}
	return p.state.Consent, nil
}

func (p *Pager) ExportMemories(ctx context.Context) (MemoryExport, error) {
	consent, _ := p.MemoryConsent(ctx)
	out := MemoryExport{SchemaVersion: 1, ExportedAt: time.Now().UTC().Format(time.RFC3339Nano), Consent: consent}
	for _, record := range p.recordList() {
		out.Items = append(out.Items, MemoryExportItem{URI: record.URI, Type: record.Type, CreatedAt: record.CreatedAt.Format(time.RFC3339Nano), UpdatedAt: record.UpdatedAt.Format(time.RFC3339Nano), Data: json.RawMessage(record.Data)})
	}
	return out, nil
}

func (p *Pager) Timeline(spec TimelineQuery) ([]TimelineEntry, int, error) {
	records := p.recordList()
	limit := spec.Limit
	if limit <= 0 || limit > 200 {
		limit = 100
	}
	out := make([]TimelineEntry, 0, limit)
	for _, record := range records {
		if !matches(record, spec.Near, spec.Types) {
			continue
		}
		out = append(out, TimelineEntry{URI: record.URI, Type: record.Type, Version: record.Version, CreatedAt: record.CreatedAt.Format(time.RFC3339Nano), UpdatedAt: record.UpdatedAt.Format(time.RFC3339Nano), CreatedBy: p.cfg.NeocortexActor, FormShort: record.Text, FormMedium: record.Text})
		if len(out) == limit {
			break
		}
	}
	return out, len(out), nil
}

func (p *Pager) TimelineTypes() ([]TimelineTypeCount, error) {
	counts := map[string]int{}
	for _, record := range p.recordList() {
		counts[record.Type]++
	}
	out := make([]TimelineTypeCount, 0, len(counts))
	for typ, count := range counts {
		out = append(out, TimelineTypeCount{Type: typ, Count: count})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Count > out[j].Count })
	return out, nil
}

func (p *Pager) Mutate(ctx context.Context, req MutationRequest) (MutationBatchResult, error) {
	if len(req.Items) == 0 || len(req.Items) > 20 {
		return MutationBatchResult{}, errors.New("memory mutation requires 1 to 20 items")
	}
	out := MutationBatchResult{}
	for _, item := range req.Items {
		switch item.Operation {
		case MutationCreate:
			if item.Value == nil {
				return MutationBatchResult{}, errors.New("create requires value")
			}
			uri, err := p.put(ctx, item.Value.Type, item.Value.Type+":"+item.Value.Subject+":"+item.Value.Predicate+":"+item.Value.Content, item.Value.Content, item.Value)
			if err != nil {
				return MutationBatchResult{}, err
			}
			out.Results = append(out.Results, MutationResult{Operation: item.Operation, Description: "created", URI: uri})
		case MutationDelete, MutationSupersede, MutationUpdate:
			if item.Target == nil {
				return MutationBatchResult{}, errors.New("mutation target is required")
			}
			record, found, ambiguous := p.resolveMutationTarget(*item.Target)
			if ambiguous {
				return MutationBatchResult{}, errors.New("memory target is ambiguous")
			}
			if !found {
				if strings.TrimSpace(item.Target.Query) != "" {
					return MutationBatchResult{}, errors.New("semantic target matched no current memory")
				}
				return MutationBatchResult{}, errors.New("memory target not found")
			}
			if item.Operation == MutationDelete {
				conversation := cortexclient.ConversationBytes("neo-memory")
				sourceAck, err := p.client.Append(ctx, []cortexclient.AppendEvent{
					cortexclient.ReasoningEvent(conversation, "Delete durable memory: "+record.Text),
				})
				if err != nil {
					return MutationBatchResult{}, err
				}
				if _, err := p.client.Append(ctx, []cortexclient.AppendEvent{
					cortexclient.RetractionEvent(conversation, cortexclient.Retraction{
						BeliefID:   beliefID(fmt.Sprintf("%s#%d", record.Identity, record.Version)),
						Provenance: [][2]uint64{{sourceAck.FirstLsn, sourceAck.LastLsn}},
					}),
				}); err != nil {
					return MutationBatchResult{}, err
				}
				p.mu.Lock()
				record.Deleted = true
				record.UpdatedAt = time.Now().UTC()
				p.state.Records[record.Identity] = record
				err = p.saveRecordsLocked(ctx)
				p.mu.Unlock()
				if err != nil {
					return MutationBatchResult{}, err
				}
			} else {
				if item.Value == nil || strings.TrimSpace(item.Value.Content) == "" {
					return MutationBatchResult{}, errors.New("update and supersede require a replacement value")
				}
				typ := strings.TrimSpace(item.Value.Type)
				if typ == "" {
					typ = record.Type
				}
				uri, err := p.put(ctx, typ, record.Identity, item.Value.Content, item.Value)
				if err != nil {
					return MutationBatchResult{}, err
				}
				description := fmt.Sprintf("%s: %s", item.Operation, strings.TrimSpace(item.Value.Content))
				out.Results = append(out.Results, MutationResult{Operation: item.Operation, Description: description, URI: uri})
				continue
			}
			out.Results = append(out.Results, MutationResult{Operation: item.Operation, Description: string(item.Operation)})
		default:
			return MutationBatchResult{}, fmt.Errorf("unknown mutation operation %q", item.Operation)
		}
	}
	return out, nil
}

func (p *Pager) resolveMutationTarget(target MutationTarget) (storedRecord, bool, bool) {
	records := p.recordList()
	if uri := strings.TrimSpace(target.URI); uri != "" {
		for _, record := range records {
			if record.URI == uri {
				return record, true, false
			}
		}
		return storedRecord{}, false, false
	}
	query := strings.TrimSpace(target.Query)
	if query == "" {
		return storedRecord{}, false, false
	}
	bestScore := 0
	var best []storedRecord
	for _, record := range records {
		if len(target.Types) > 0 {
			allowed := false
			for _, typ := range target.Types {
				if strings.EqualFold(typ, record.Type) {
					allowed = true
					break
				}
			}
			if !allowed {
				continue
			}
		}
		score := tokenOverlap(query, record.Text+" "+record.Identity)
		if score > bestScore {
			bestScore = score
			best = []storedRecord{record}
		} else if score == bestScore && score > 0 {
			best = append(best, record)
		}
	}
	if bestScore == 0 {
		return storedRecord{}, false, false
	}
	if len(best) != 1 {
		return storedRecord{}, false, true
	}
	return best[0], true, false
}

func tokenOverlap(a, b string) int {
	stop := map[string]bool{"the": true, "a": true, "an": true, "is": true, "our": true, "current": true, "existing": true, "durable": true, "memory": true, "record": true, "of": true, "for": true, "to": true, "and": true}
	tokens := map[string]bool{}
	for _, token := range strings.FieldsFunc(normalizeStatement(a), func(r rune) bool { return r < 'a' || r > 'z' }) {
		if len(token) > 2 && !stop[token] {
			tokens[token] = true
		}
	}
	score := 0
	seen := map[string]bool{}
	for _, token := range strings.FieldsFunc(normalizeStatement(b), func(r rune) bool { return r < 'a' || r > 'z' }) {
		if tokens[token] && !seen[token] {
			score++
			seen[token] = true
		}
	}
	return score
}

func (p *Pager) PersonalizationProfile(context.Context) (PersonalizationProfile, string, bool, error) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if p.state.Personalization == nil {
		return PersonalizationProfile{SchemaVersion: 1}, "", false, nil
	}
	return *p.state.Personalization, recordURI("personalization", p.state.PersonalizationVersion), true, nil
}
func (p *Pager) SavePersonalizationProfile(ctx context.Context, profile PersonalizationProfile) (string, error) {
	if profile.SchemaVersion == 0 {
		profile.SchemaVersion = 1
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.state.Personalization = &profile
	p.state.PersonalizationVersion++
	if err := p.saveDomainLocked(ctx, personalizationCheckpoint, personalizationState{
		Profile: p.state.Personalization, Version: p.state.PersonalizationVersion,
	}); err != nil {
		return "", err
	}
	return recordURI("personalization", p.state.PersonalizationVersion), nil
}
func (p *Pager) DeletePersonalization(ctx context.Context) (bool, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	existed := p.state.Personalization != nil
	p.state.Personalization = nil
	if existed {
		if err := p.saveDomainLocked(ctx, personalizationCheckpoint, personalizationState{
			Profile: p.state.Personalization, Version: p.state.PersonalizationVersion,
		}); err != nil {
			return false, err
		}
	}
	return existed, nil
}
func (p *Pager) PersonalizationExport(ctx context.Context) (PersonalizationRecord, error) {
	profile, uri, ok, err := p.PersonalizationProfile(ctx)
	p.mu.RLock()
	version := p.state.PersonalizationVersion
	p.mu.RUnlock()
	return PersonalizationRecord{Profile: profile, URI: uri, Version: version, CreatedBy: p.cfg.NeocortexActor, Exists: ok}, err
}

func (p *Pager) RememberOpportunity(ctx context.Context, spec OpportunitySpec) (string, error) {
	if strings.TrimSpace(spec.Summary) == "" {
		return "", errors.New("opportunity summary is required")
	}
	if !spec.Status.Valid() {
		spec.Status = OpportunityPending
	}
	now := time.Now().UTC()
	if spec.CreatedAt.IsZero() {
		spec.CreatedAt = now
	}
	spec.UpdatedAt = now
	identity := "opportunity:" + normalizeStatement(spec.Summary)
	uri, err := p.put(ctx, "opportunity", identity, spec.Summary, spec)
	if err != nil {
		return "", err
	}
	spec.URI = uri
	p.mu.Lock()
	p.state.Opportunities[identity] = spec
	err = p.saveDomainLocked(ctx, opportunitiesCheckpoint, p.state.Opportunities)
	p.mu.Unlock()
	return uri, err
}
func (p *Pager) opportunities(status OpportunityStatus, limit int) []OpportunitySpec {
	p.mu.RLock()
	defer p.mu.RUnlock()
	var out []OpportunitySpec
	for _, spec := range p.state.Opportunities {
		if spec.Status == status {
			out = append(out, spec)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].UpdatedAt.Before(out[j].UpdatedAt) })
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out
}
func (p *Pager) PendingOpportunities(_ context.Context, limit int) ([]OpportunitySpec, error) {
	return p.opportunities(OpportunityPending, limit), nil
}
func (p *Pager) QueuedOpportunities(_ context.Context, limit int) ([]OpportunitySpec, error) {
	return p.opportunities(OpportunityPending, limit), nil
}
func (p *Pager) InProgressOpportunities(_ context.Context, limit int) ([]OpportunitySpec, error) {
	return p.opportunities(OpportunityInProgress, limit), nil
}
func (p *Pager) OpportunityByURI(_ context.Context, uri string) (OpportunitySpec, bool, error) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	for _, spec := range p.state.Opportunities {
		if spec.URI == uri {
			return spec, true, nil
		}
	}
	return OpportunitySpec{}, false, nil
}
func (p *Pager) SetOpportunityStatus(ctx context.Context, uri string, status OpportunityStatus) error {
	if !status.Valid() {
		return errors.New("invalid opportunity status")
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	for key, spec := range p.state.Opportunities {
		if spec.URI == uri {
			spec.Status = status
			spec.UpdatedAt = time.Now().UTC()
			p.state.Opportunities[key] = spec
			return p.saveDomainLocked(ctx, opportunitiesCheckpoint, p.state.Opportunities)
		}
	}
	return errors.New("opportunity not found")
}
func (p *Pager) FailOpportunityAttempt(ctx context.Context, uri string, max int) (OpportunityStatus, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for key, spec := range p.state.Opportunities {
		if spec.URI == uri {
			spec.Attempts++
			if max > 0 && spec.Attempts >= max {
				spec.Status = OpportunityDismissed
			} else {
				spec.Status = OpportunityPending
			}
			spec.UpdatedAt = time.Now().UTC()
			p.state.Opportunities[key] = spec
			return spec.Status, p.saveDomainLocked(ctx, opportunitiesCheckpoint, p.state.Opportunities)
		}
	}
	return "", errors.New("opportunity not found")
}

func (p *Pager) WritePattern(ctx context.Context, spec PatternSpec, confidence float32, coverage int, _ []string) (string, error) {
	if spec.IsEmpty() {
		return "", errors.New("pattern is empty")
	}
	identity := "pattern:" + normalizeStatement(spec.Trigger+strings.Join(spec.Steps, " "))
	uri, err := p.put(ctx, "pattern", identity, spec.Encode(), spec)
	if err != nil {
		return "", err
	}
	p.mu.Lock()
	p.state.Patterns[identity] = Pattern{Spec: spec, Confidence: confidence, Coverage: coverage, URI: uri}
	err = p.saveDomainLocked(ctx, patternsCheckpoint, p.state.Patterns)
	p.mu.Unlock()
	return uri, err
}
func (p *Pager) ReinforcePattern(ctx context.Context, spec PatternSpec, derived []string) (string, error) {
	identity := "pattern:" + normalizeStatement(spec.Trigger+strings.Join(spec.Steps, " "))
	p.mu.RLock()
	current := p.state.Patterns[identity]
	p.mu.RUnlock()
	return p.WritePattern(ctx, spec, max(current.Confidence, 0.5), current.Coverage+1, derived)
}
func (p *Pager) Procedural(context.Context, string) ([]Pattern, error) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	out := make([]Pattern, 0, len(p.state.Patterns))
	for _, pattern := range p.state.Patterns {
		out = append(out, pattern)
	}
	return out, nil
}
func (p *Pager) TriggeredGuidance(ctx context.Context, turn string) []Snippet {
	patterns, _ := p.Procedural(ctx, turn)
	var out []Snippet
	for _, pattern := range patterns {
		if pattern.Spec.Trigger != "" && strings.Contains(normalizeStatement(turn), normalizeStatement(pattern.Spec.Trigger)) {
			out = append(out, Snippet{Text: pattern.Render(), URI: pattern.URI, Type: "pattern"})
		}
	}
	return out
}

func (p *Pager) RecordLoopDeath(ctx context.Context, summary, intent string) (string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	identity := boundedIdentity("death:" + intent + ":" + summary)
	uri := recordURI(identity, uint64(len(p.state.Deaths)+1))
	p.state.Deaths = append(p.state.Deaths, DeathRecord{URI: uri, IntentID: intent, Summary: summary, CreatedAt: time.Now().UTC()})
	if len(p.state.Deaths) > 256 {
		p.state.Deaths = append([]DeathRecord(nil), p.state.Deaths[len(p.state.Deaths)-256:]...)
	}
	return uri, p.saveDomainLocked(ctx, diagnosticsCheckpoint, diagnosticState{Deaths: p.state.Deaths})
}
func (p *Pager) DeathJournal(_ context.Context, limit int) ([]DeathRecord, error) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	out := append([]DeathRecord(nil), p.state.Deaths...)
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}
func (p *Pager) ConsolidateDeathJournal(ctx context.Context) (int, error) {
	// Deaths are operational diagnostics, never cognitive evidence. This
	// compatibility entrypoint intentionally performs no promotion; a lesson can
	// only enter the self-model through WriteFailureLesson's explicit gate.
	return 0, nil
}

func (p *Pager) WriteStructuralSelf(ctx context.Context, structural StructuralSelf) (string, error) {
	p.mu.RLock()
	if structural.Surface == nil {
		structural.Surface = p.state.Structural.Surface
	}
	existing := p.state.Records["structural-self"]
	existingURI := p.state.StructuralURI
	p.mu.RUnlock()
	encoded, err := json.Marshal(structural)
	if err != nil {
		return "", err
	}
	if existingURI != "" && !existing.Deleted &&
		existing.Text == strings.TrimSpace(structural.Summary) &&
		bytes.Equal(existing.Data, encoded) {
		return existingURI, nil
	}
	uri, err := p.putRecord(ctx, "capability", "structural-self", structural.Summary, structural, false)
	if err != nil {
		return "", err
	}
	p.mu.Lock()
	p.state.Structural = structural
	p.state.StructuralURI = uri
	err = p.saveStructuralLocked(ctx)
	p.mu.Unlock()
	return uri, err
}
func (p *Pager) WriteSurfaceFacts(ctx context.Context, facts SurfaceFacts) (string, error) {
	p.mu.RLock()
	structural := p.state.Structural
	p.mu.RUnlock()
	structural.Surface = &facts
	return p.WriteStructuralSelf(ctx, structural)
}
func (p *Pager) WriteFailurePattern(ctx context.Context, statement string, derived []string) (string, error) {
	return "", fmt.Errorf("memory: failure lessons require confirmation, independent evidence, scope, usefulness, and expiry")
}

func (p *Pager) WriteFailureLesson(ctx context.Context, lesson FailureLesson) (string, error) {
	lesson.Statement = strings.TrimSpace(lesson.Statement)
	lesson.Scope = strings.TrimSpace(lesson.Scope)
	lesson.Usefulness = strings.TrimSpace(lesson.Usefulness)
	now := time.Now().UTC()
	uniqueEvidence := make(map[string]struct{})
	derived := make([]string, 0, len(lesson.DerivedFrom))
	for _, identity := range lesson.DerivedFrom {
		identity = strings.TrimSpace(identity)
		if identity == "" {
			continue
		}
		if _, exists := uniqueEvidence[identity]; exists {
			continue
		}
		uniqueEvidence[identity] = struct{}{}
		derived = append(derived, identity)
	}
	if !lesson.Confirmed || lesson.Statement == "" || lesson.Scope == "" ||
		lesson.Usefulness == "" || len(derived) < 2 ||
		!lesson.ExpiresAt.After(now) || lesson.ExpiresAt.After(now.Add(180*24*time.Hour)) {
		return "", fmt.Errorf("memory: failure lesson did not pass confirmation, evidence, scope, usefulness, and expiry gates")
	}
	uri, err := p.put(ctx, "constraint", "failure:"+lesson.Statement, lesson.Statement, map[string]any{
		"statement": lesson.Statement, "derived_from": derived,
		"scope": lesson.Scope, "usefulness": lesson.Usefulness,
		"confirmed_at": now, "expires_at": lesson.ExpiresAt.UTC(),
	})
	if err != nil {
		return "", err
	}
	pattern := FailurePattern{
		URI: uri, Statement: lesson.Statement, DerivedFrom: derived,
		Scope: lesson.Scope, Usefulness: lesson.Usefulness,
		ConfirmedAt: now, ExpiresAt: lesson.ExpiresAt.UTC(),
	}
	p.mu.Lock()
	replaced := false
	for index, existing := range p.state.Failures {
		if normalizeStatement(existing.Statement) == normalizeStatement(lesson.Statement) {
			p.state.Failures[index] = pattern
			replaced = true
			break
		}
	}
	if !replaced {
		p.state.Failures = append(p.state.Failures, pattern)
	}
	if len(p.state.Failures) > 32 {
		p.state.Failures = append([]FailurePattern(nil), p.state.Failures[len(p.state.Failures)-32:]...)
	}
	err = p.saveDomainLocked(ctx, failuresCheckpoint, p.state.Failures)
	p.mu.Unlock()
	return uri, err
}

func (p *Pager) RetireFailurePattern(ctx context.Context, uri string) error {
	uri = strings.TrimSpace(uri)
	p.mu.Lock()
	kept := p.state.Failures[:0]
	for _, pattern := range p.state.Failures {
		if pattern.URI != uri {
			kept = append(kept, pattern)
		}
	}
	p.state.Failures = append([]FailurePattern(nil), kept...)
	err := p.saveDomainLocked(ctx, failuresCheckpoint, p.state.Failures)
	p.mu.Unlock()
	return err
}

func (p *Pager) SelfModel(ctx context.Context) (SelfModel, error) {
	p.mu.Lock()
	now := time.Now().UTC()
	active := make([]FailurePattern, 0, len(p.state.Failures))
	for _, pattern := range p.state.Failures {
		if pattern.ExpiresAt.IsZero() || pattern.ExpiresAt.After(now) {
			active = append(active, pattern)
		}
	}
	if len(active) != len(p.state.Failures) {
		p.state.Failures = append([]FailurePattern(nil), active...)
		if err := p.saveDomainLocked(ctx, failuresCheckpoint, p.state.Failures); err != nil {
			p.mu.Unlock()
			return SelfModel{}, err
		}
	}
	model := SelfModel{Identity: p.cfg.AgentName, Structural: p.state.Structural, StructuralURI: p.state.StructuralURI, FailurePatterns: append([]FailurePattern(nil), active...)}
	p.mu.Unlock()
	return model, nil
}
func (p *Pager) LoadStructuralSelf(ctx context.Context) (string, error) {
	encoded, err := os.ReadFile(filepath.Join(p.cfg.SelfModelGraph, "self-model.json"))
	if err != nil {
		return "", err
	}
	var artifact selfmodel.Artifact
	if err := json.Unmarshal(encoded, &artifact); err != nil {
		return "", err
	}
	return p.WriteStructuralSelf(ctx, StructuralSelf{Summary: artifact.Summary, GraphURI: artifact.Merkle, Scope: artifact.Scope, TokenBudget: len(strings.Fields(artifact.Summary)), ContextLimit: p.cfg.ContextWindowTokens})
}
func (p *Pager) LookupStructuralSelf(symbol string) (string, error) {
	index, _, err := cgstore.Load(p.cfg.SelfModelGraph)
	if err != nil {
		return "", err
	}
	return selfmodel.Lookup(index, symbol)
}
func (p *Pager) recallSelf(ctx context.Context, symbol string) (string, error) {
	if symbol == "" {
		model, err := p.SelfModel(ctx)
		return model.Structural.Summary, err
	}
	return p.LookupStructuralSelf(symbol)
}

func (p *Pager) Transcript(conversationID string, since uint64, limit int) ([]TranscriptMessage, error) {
	if p == nil || p.client == nil {
		return nil, ErrNeocortexUnavailable
	}
	if limit <= 0 {
		limit = 16_384
	}
	records, _, err := p.client.Transcript(context.Background(), cortexclient.ConversationBytes(conversationID), since, uint32(limit))
	if err != nil {
		return nil, err
	}
	out := make([]TranscriptMessage, 0, len(records))
	for _, record := range records {
		decoded, err := cortexclient.DecodeEvent(record.Kind, record.Payload)
		if err != nil {
			return nil, err
		}
		role := "event"
		switch record.Kind {
		case cortexclient.KindUserMsg:
			role = "user"
		case cortexclient.KindDeliveredMsg, cortexclient.KindReasoning:
			role = "assistant"
		case cortexclient.KindToolCall:
			role = "tool_call"
		case cortexclient.KindToolResult:
			role = "tool_result"
		}
		out = append(out, TranscriptMessage{Seq: record.Lsn, Role: role, ToolName: decoded.ToolName, Content: decoded.Content, TS: record.WallTimestampNs})
	}
	return out, nil
}

func (p *Pager) RejectionCandidates(turn string, surfaced []Snippet) []string {
	normalized := normalizeStatement(turn)
	var out []string
	for _, snippet := range surfaced {
		if snippet.URI != "" && !strings.Contains(normalized, normalizeStatement(snippet.Text)) {
			out = append(out, snippet.URI)
		}
	}
	return out
}

func (p *Pager) attest(ctx context.Context, uris []string, used bool) {
	p.mu.RLock()
	var lsns []uint64
	for _, uri := range uris {
		for _, record := range p.state.Records {
			if record.URI == uri && record.LSN != 0 {
				lsns = append(lsns, record.LSN)
			}
		}
	}
	p.mu.RUnlock()
	if len(lsns) == 0 {
		return
	}
	if used {
		_, _ = p.client.Attest(ctx, lsns, nil)
	} else {
		_, _ = p.client.Attest(ctx, nil, lsns)
	}
}

func (p *Pager) AttestUsed(ctx context.Context, _ string, uris []string, _ bool) {
	p.attest(ctx, uris, true)
}
func (p *Pager) AttestRejected(ctx context.Context, _ string, uris []string) {
	p.attest(ctx, uris, false)
}
func (p *Pager) EpisodicRetrieve(ctx context.Context, query string, window EpisodicTimeWindow, budget EpisodicBudget, current []EpisodicCurrentHit) []EpisodicExcerpt {
	if strings.TrimSpace(query) == "" || ctx.Err() != nil {
		return nil
	}
	limit := budget.Hits
	if limit <= 0 {
		limit = p.cfg.RetrievalTopK
	}
	if limit <= 0 {
		limit = 8
	}
	currentText := ""
	exclusions := cortexclient.ProjectionExclusions{
		SourceIdentities: make(map[string]struct{}),
		ContentHashes:    make(map[[32]byte]struct{}),
	}
	for _, hit := range current {
		currentText += " " + normalizeStatement(hit.Text)
		if identity := strings.TrimSpace(hit.SourceIdentity); identity != "" {
			exclusions.SourceIdentities[identity] = struct{}{}
		}
		if text := strings.TrimSpace(hit.Text); text != "" {
			exclusions.ContentHashes[sha256.Sum256([]byte(text))] = struct{}{}
		}
	}
	var out []EpisodicExcerpt
	for _, record := range p.recordList() {
		if !matches(record, query, nil) || strings.Contains(currentText, normalizeStatement(record.Text)) {
			continue
		}
		if !window.From.IsZero() && record.UpdatedAt.Before(window.From) || !window.Until.IsZero() && record.UpdatedAt.After(window.Until) {
			continue
		}
		conversation := record.Conversation
		if conversation == "" {
			conversation = "neo-memory"
		}
		if conversation == budget.ExcludeConversation {
			continue
		}
		lo, hi := record.SeqLo, record.SeqHi
		if lo == 0 {
			lo, hi = record.LSN, record.LSN
		}
		out = append(out, EpisodicExcerpt{Ref: record.URI, ConversationID: conversation, Date: record.UpdatedAt, SeqLo: lo, SeqHi: hi, Exact: true, Text: record.Text, RelatedMemories: []Snippet{{Text: record.Text, URI: record.URI, Type: record.Type}}})
		if len(out) == limit {
			return out
		}
	}
	activationCtx := ctx
	var cancel context.CancelFunc
	if budget.Deadline > 0 {
		activationCtx, cancel = context.WithTimeout(ctx, budget.Deadline)
		defer cancel()
	}
	bundle, err := p.Activate(activationCtx, "neo-episodic-retrieval", query, p.cfg.ActivationBudget())
	if err != nil || bundle == nil {
		return out
	}
	seen := make(map[string]bool, len(out))
	for _, excerpt := range out {
		seen[excerpt.Text] = true
	}
	for _, item := range cortexclient.ProjectBundleExcluding(bundle, exclusions) {
		text := strings.TrimSpace(item.Text)
		if text == "" || seen[text] || tokenOverlap(query, text) == 0 || strings.Contains(currentText, normalizeStatement(text)) {
			continue
		}
		seen[text] = true
		var lo, hi uint64
		if len(item.Provenance) > 0 {
			lo, hi = item.Provenance[0], item.Provenance[len(item.Provenance)-1]
		}
		conversation := strings.TrimSpace(item.ConversationID)
		if conversation == "" {
			conversation = "neocortex-activation"
		}
		occurredAt := time.Now().UTC()
		if parsed, parseErr := time.Parse("2006-01-02", item.Date); parseErr == nil {
			occurredAt = parsed.UTC()
		}
		out = append(out, EpisodicExcerpt{
			Ref: item.URI, ConversationID: conversation, Date: occurredAt,
			SeqLo: lo, SeqHi: hi, Exact: len(item.Provenance) > 0, Text: text,
		})
		if len(out) == limit {
			return out
		}
	}
	return out
}
