// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package cortexclient

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	flatbuffers "matrix/cortexclient/internal/flatbuffers"
	"matrix/cortexclient/wire/neocortex/protocol"
	"matrix/cortexclient/wire/neocortex/schema"
)

// ConversationBytes derives the stable 16-byte wire conversation id for a
// string conversation identifier.
func ConversationBytes(conversationID string) [16]byte {
	digest := sha256.Sum256([]byte("neocortex-conversation-v1\x00" + conversationID))
	var out [16]byte
	copy(out[:], digest[:16])
	return out
}

func wrapEnvelope(event AppendEvent, connectionID [16]byte, clientSeq uint64,
	index, count uint32) []byte {
	builder := flatbuffers.NewBuilder(512)
	payloadType, payload := event.Build(builder)
	connection := builder.CreateByteVector(connectionID[:])
	schema.EventEnvelopeStart(builder)
	schema.EventEnvelopeAddSchemaVersion(builder, 1)
	schema.EventEnvelopeAddPayloadType(builder, schema.EventPayload(payloadType))
	schema.EventEnvelopeAddPayload(builder, payload)
	schema.EventEnvelopeAddConnectionId(builder, connection)
	schema.EventEnvelopeAddClientSeq(builder, clientSeq)
	schema.EventEnvelopeAddClientEventIndex(builder, index)
	schema.EventEnvelopeAddClientEventCount(builder, count)
	envelope := schema.EventEnvelopeEnd(builder)
	builder.FinishWithFileIdentifier(envelope, []byte("NCEV"))
	return builder.FinishedBytes()
}

func textEvent(kind EventKind, conversation [16]byte, payloadType schema.EventPayload,
	content string, start func(*flatbuffers.Builder), add func(*flatbuffers.Builder, flatbuffers.UOffsetT),
	end func(*flatbuffers.Builder) flatbuffers.UOffsetT) AppendEvent {
	return AppendEvent{
		Kind:         kind,
		Conversation: conversation,
		Build: func(builder *flatbuffers.Builder) (byte, flatbuffers.UOffsetT) {
			bytes := builder.CreateByteVector([]byte(content))
			start(builder)
			add(builder, bytes)
			return byte(payloadType), end(builder)
		},
	}
}

// UserMsgEvent builds a user_msg event.
func UserMsgEvent(conversation [16]byte, content string) AppendEvent {
	return textEvent(KindUserMsg, conversation, schema.EventPayloadUserMsg,
		content, schema.UserMsgStart, schema.UserMsgAddContent, schema.UserMsgEnd)
}

// DeliveredMsgEvent builds a delivered_msg event — the only representation of
// user-visible assistant output.
func DeliveredMsgEvent(conversation [16]byte, content string) AppendEvent {
	return textEvent(KindDeliveredMsg, conversation, schema.EventPayloadDeliveredMsg,
		content, schema.DeliveredMsgStart, schema.DeliveredMsgAddContent,
		schema.DeliveredMsgEnd)
}

// ReasoningEvent builds a reasoning event for undelivered assistant content.
func ReasoningEvent(conversation [16]byte, content string) AppendEvent {
	return textEvent(KindReasoning, conversation, schema.EventPayloadReasoning,
		content, schema.ReasoningStart, schema.ReasoningAddContent,
		schema.ReasoningEnd)
}

// ProviderFrameEvent builds a provider_frame event carrying exact provider bytes.
func ProviderFrameEvent(conversation [16]byte, provider string, apiContent []byte) AppendEvent {
	return AppendEvent{
		Kind:         KindProviderFrame,
		Conversation: conversation,
		Build: func(builder *flatbuffers.Builder) (byte, flatbuffers.UOffsetT) {
			name := builder.CreateString(provider)
			content := builder.CreateByteVector(apiContent)
			schema.ProviderFrameStart(builder)
			schema.ProviderFrameAddProvider(builder, name)
			schema.ProviderFrameAddApiContent(builder, content)
			return byte(schema.EventPayloadProviderFrame),
				schema.ProviderFrameEnd(builder)
		},
	}
}

// ToolCallEvent builds a tool_call event.
func ToolCallEvent(conversation [16]byte, callID, toolName string, arguments []byte) AppendEvent {
	return AppendEvent{
		Kind:         KindToolCall,
		Conversation: conversation,
		Build: func(builder *flatbuffers.Builder) (byte, flatbuffers.UOffsetT) {
			id := builder.CreateByteVector([]byte(callID))
			name := builder.CreateString(toolName)
			args := builder.CreateByteVector(arguments)
			schema.ToolCallStart(builder)
			schema.ToolCallAddCallId(builder, id)
			schema.ToolCallAddToolName(builder, name)
			schema.ToolCallAddArguments(builder, args)
			return byte(schema.EventPayloadToolCall), schema.ToolCallEnd(builder)
		},
	}
}

// ToolResultEvent builds a tool_result event referencing the durable call.
func ToolResultEvent(conversation [16]byte, callID string, toolCallLsn uint64,
	ok bool, result []byte) AppendEvent {
	return AppendEvent{
		Kind:         KindToolResult,
		Conversation: conversation,
		Build: func(builder *flatbuffers.Builder) (byte, flatbuffers.UOffsetT) {
			id := builder.CreateByteVector([]byte(callID))
			resultBytes := builder.CreateByteVector(result)
			schema.ToolResultStart(builder)
			schema.ToolResultAddCallId(builder, id)
			schema.ToolResultAddToolCallLsn(builder, toolCallLsn)
			if ok {
				schema.ToolResultAddStatus(builder, schema.ResultStatusok)
			} else {
				schema.ToolResultAddStatus(builder, schema.ResultStatuserror)
			}
			schema.ToolResultAddResult(builder, resultBytes)
			return byte(schema.EventPayloadToolResult), schema.ToolResultEnd(builder)
		},
	}
}

// Assertion is one extracted belief write for the consolidation sink.
type Assertion struct {
	BeliefID          []byte
	Type              uint8
	CanonicalIdentity string
	Value             []byte
	ValidFromNs       int64
	ValidToNs         int64
	Provenance        [][2]uint64
	ConflictDomain    string
	NegativeExistence bool
}

func buildAssertion(builder *flatbuffers.Builder, assertion Assertion) flatbuffers.UOffsetT {
	id := builder.CreateByteVector(assertion.BeliefID)
	identity := builder.CreateString(assertion.CanonicalIdentity)
	value := builder.CreateByteVector(assertion.Value)
	var domain flatbuffers.UOffsetT
	if assertion.ConflictDomain != "" {
		domain = builder.CreateString(assertion.ConflictDomain)
	}
	schema.AssertionStartProvenanceVector(builder, len(assertion.Provenance))
	for index := len(assertion.Provenance) - 1; index >= 0; index-- {
		schema.CreateProvenanceRange(builder, assertion.Provenance[index][0],
			assertion.Provenance[index][1])
	}
	provenance := builder.EndVector(len(assertion.Provenance))
	schema.AssertionStart(builder)
	schema.AssertionAddBeliefId(builder, id)
	schema.AssertionAddBeliefType(builder, schema.BeliefType(assertion.Type))
	schema.AssertionAddCanonicalIdentity(builder, identity)
	schema.AssertionAddValue(builder, value)
	schema.AssertionAddValidFromNs(builder, assertion.ValidFromNs)
	schema.AssertionAddValidToNs(builder, assertion.ValidToNs)
	schema.AssertionAddProvenance(builder, provenance)
	if assertion.ConflictDomain != "" {
		schema.AssertionAddConflictDomain(builder, domain)
	}
	if assertion.NegativeExistence {
		schema.AssertionAddClaim(builder, schema.AssertionClaimnegative_existence)
	}
	return schema.AssertionEnd(builder)
}

// AssertionEvent builds a single assertion event.
func AssertionEvent(conversation [16]byte, assertion Assertion) AppendEvent {
	return AppendEvent{
		Kind:         KindAssertion,
		Conversation: conversation,
		Build: func(builder *flatbuffers.Builder) (byte, flatbuffers.UOffsetT) {
			return byte(schema.EventPayloadAssertion), buildAssertion(builder, assertion)
		},
	}
}

// Retraction identifies a current assertion to remove and the evidence that
// authorized the removal.
type Retraction struct {
	BeliefID   []byte
	Provenance [][2]uint64
}

// RetractionEvent builds a single typed Neocortex retraction event.
func RetractionEvent(conversation [16]byte, retraction Retraction) AppendEvent {
	return AppendEvent{
		Kind:         KindRetract,
		Conversation: conversation,
		Build: func(builder *flatbuffers.Builder) (byte, flatbuffers.UOffsetT) {
			id := builder.CreateByteVector(retraction.BeliefID)
			schema.RetractStartProvenanceVector(builder, len(retraction.Provenance))
			for index := len(retraction.Provenance) - 1; index >= 0; index-- {
				schema.CreateProvenanceRange(builder, retraction.Provenance[index][0], retraction.Provenance[index][1])
			}
			provenance := builder.EndVector(len(retraction.Provenance))
			schema.RetractStart(builder)
			schema.RetractAddBeliefId(builder, id)
			schema.RetractAddProvenance(builder, provenance)
			return byte(schema.EventPayloadRetract), schema.RetractEnd(builder)
		},
	}
}

// ConsolidationEvent builds one consolidation event carrying a batch of
// extracted assertions.
func ConsolidationEvent(conversation [16]byte, assertions []Assertion) AppendEvent {
	return AppendEvent{
		Kind:         KindConsolidation,
		Conversation: conversation,
		Build: func(builder *flatbuffers.Builder) (byte, flatbuffers.UOffsetT) {
			offsets := make([]flatbuffers.UOffsetT, len(assertions))
			for index := len(assertions) - 1; index >= 0; index-- {
				offsets[index] = buildAssertion(builder, assertions[index])
			}
			schema.ConsolidationStartAssertionsVector(builder, len(offsets))
			for index := len(offsets) - 1; index >= 0; index-- {
				builder.PrependUOffsetT(offsets[index])
			}
			vector := builder.EndVector(len(offsets))
			schema.ConsolidationStart(builder)
			schema.ConsolidationAddAssertions(builder, vector)
			return byte(schema.EventPayloadConsolidation),
				schema.ConsolidationEnd(builder)
		},
	}
}

// DefaultTokenWeights is the caller-supplied token model table shipped to the
// engine: fixed-point 1/256-token weights per byte value. The default charges
// one token per four bytes, matching the runtime's planning estimator.
func DefaultTokenWeights() []uint16 {
	weights := make([]uint16, 256)
	for index := range weights {
		weights[index] = 64
	}
	return weights
}

// Bundle mirrors the composer's canonical activation encoding (NCA1).
type Bundle struct {
	SnapshotEpoch uint64
	BudgetTokens  uint64
	SpentTokens   uint64
	Sections      [8]BundleSection
	Trimmed       []TrimmedItem
}

type BundleSection struct {
	Tier           uint8
	Tokens         uint64
	TrimmedItems   uint64
	CoarsenedItems uint64
	Items          []BundleItem
}

type BundleItem struct {
	Tier       uint8
	Coarsened  bool
	Tokens     uint64
	URI        string
	Provenance []uint64
	Content    []byte
}

type TrimmedItem struct {
	Tier uint8
	URI  string
}

type bundleCursor struct {
	bytes  []byte
	offset int
}

func (c *bundleCursor) u8() (byte, error) {
	if c.offset+1 > len(c.bytes) {
		return 0, fmt.Errorf("%w: bundle truncated", ErrProtocol)
	}
	value := c.bytes[c.offset]
	c.offset++
	return value, nil
}

func (c *bundleCursor) u64() (uint64, error) {
	if c.offset+8 > len(c.bytes) {
		return 0, fmt.Errorf("%w: bundle truncated", ErrProtocol)
	}
	var value uint64
	for index := 0; index < 8; index++ {
		value |= uint64(c.bytes[c.offset+index]) << (8 * index)
	}
	c.offset += 8
	return value, nil
}

func (c *bundleCursor) blob() ([]byte, error) {
	length, err := c.u64()
	if err != nil {
		return nil, err
	}
	if length > uint64(len(c.bytes)-c.offset) {
		return nil, fmt.Errorf("%w: bundle blob truncated", ErrProtocol)
	}
	value := make([]byte, length)
	copy(value, c.bytes[c.offset:c.offset+int(length)])
	c.offset += int(length)
	return value, nil
}

// ParseBundle decodes the canonical NCA1 activation bytes.
func ParseBundle(encoded []byte) (*Bundle, error) {
	if len(encoded) < 4 || string(encoded[:4]) != "NCA1" {
		return nil, fmt.Errorf("%w: bundle magic", ErrProtocol)
	}
	cursor := &bundleCursor{bytes: encoded, offset: 4}
	bundle := &Bundle{}
	var err error
	if bundle.SnapshotEpoch, err = cursor.u64(); err != nil {
		return nil, err
	}
	if bundle.BudgetTokens, err = cursor.u64(); err != nil {
		return nil, err
	}
	if bundle.SpentTokens, err = cursor.u64(); err != nil {
		return nil, err
	}
	for index := range bundle.Sections {
		section := &bundle.Sections[index]
		if section.Tier, err = cursor.u8(); err != nil {
			return nil, err
		}
		if section.Tokens, err = cursor.u64(); err != nil {
			return nil, err
		}
		if section.TrimmedItems, err = cursor.u64(); err != nil {
			return nil, err
		}
		if section.CoarsenedItems, err = cursor.u64(); err != nil {
			return nil, err
		}
		count, err := cursor.u64()
		if err != nil {
			return nil, err
		}
		if count > uint64(len(encoded)) {
			return nil, fmt.Errorf("%w: bundle item count", ErrProtocol)
		}
		section.Items = make([]BundleItem, 0, count)
		for item := uint64(0); item < count; item++ {
			var entry BundleItem
			if entry.Tier, err = cursor.u8(); err != nil {
				return nil, err
			}
			coarsened, err := cursor.u8()
			if err != nil {
				return nil, err
			}
			entry.Coarsened = coarsened != 0
			if entry.Tokens, err = cursor.u64(); err != nil {
				return nil, err
			}
			uri, err := cursor.blob()
			if err != nil {
				return nil, err
			}
			entry.URI = string(uri)
			provenanceCount, err := cursor.u64()
			if err != nil {
				return nil, err
			}
			if provenanceCount > uint64(len(encoded)) {
				return nil, fmt.Errorf("%w: bundle provenance count", ErrProtocol)
			}
			entry.Provenance = make([]uint64, 0, provenanceCount)
			for p := uint64(0); p < provenanceCount; p++ {
				lsn, err := cursor.u64()
				if err != nil {
					return nil, err
				}
				entry.Provenance = append(entry.Provenance, lsn)
			}
			if entry.Content, err = cursor.blob(); err != nil {
				return nil, err
			}
			section.Items = append(section.Items, entry)
		}
	}
	trimmedCount, err := cursor.u64()
	if err != nil {
		return nil, err
	}
	if trimmedCount > uint64(len(encoded)) {
		return nil, fmt.Errorf("%w: bundle trimmed count", ErrProtocol)
	}
	for index := uint64(0); index < trimmedCount; index++ {
		tier, err := cursor.u8()
		if err != nil {
			return nil, err
		}
		uri, err := cursor.blob()
		if err != nil {
			return nil, err
		}
		bundle.Trimmed = append(bundle.Trimmed, TrimmedItem{Tier: tier, URI: string(uri)})
	}
	return bundle, nil
}

var tierNames = [8]string{
	"resident", "intent", "work_ledger", "conversation",
	"entity", "conflicts", "recall", "temporal",
}

func eventPayloadForKind(kind EventKind) (schema.EventPayload, bool) {
	switch kind {
	case KindUserMsg:
		return schema.EventPayloadUserMsg, true
	case KindDeliveredMsg:
		return schema.EventPayloadDeliveredMsg, true
	case KindToolCall:
		return schema.EventPayloadToolCall, true
	case KindToolResult:
		return schema.EventPayloadToolResult, true
	case KindReasoning:
		return schema.EventPayloadReasoning, true
	case KindProviderFrame:
		return schema.EventPayloadProviderFrame, true
	case KindMediaRef:
		return schema.EventPayloadMediaRef, true
	case KindEffect:
		return schema.EventPayloadEffect, true
	case KindApproval:
		return schema.EventPayloadApproval, true
	case KindOutcome:
		return schema.EventPayloadOutcome, true
	case KindCheckpoint:
		return schema.EventPayloadCheckpoint, true
	case KindSupervisor:
		return schema.EventPayloadSupervisor, true
	case KindRecovery:
		return schema.EventPayloadRecovery, true
	case KindIntentSet:
		return schema.EventPayloadIntentSet, true
	case KindLoopOpened:
		return schema.EventPayloadLoopOpened, true
	case KindLoopClosed:
		return schema.EventPayloadLoopClosed, true
	case KindAssertion:
		return schema.EventPayloadAssertion, true
	case KindConsolidation:
		return schema.EventPayloadConsolidation, true
	case KindEmbedding:
		return schema.EventPayloadEmbedding, true
	case KindRetract:
		return schema.EventPayloadRetract, true
	case KindAttestation:
		return schema.EventPayloadAttestation, true
	default:
		return schema.EventPayloadNONE, false
	}
}

func eventTable(kind EventKind, payload []byte) (table flatbuffers.Table, err error) {
	defer func() {
		if recover() != nil {
			err = ErrProtocol
		}
	}()
	expected, ok := eventPayloadForKind(kind)
	if !ok || len(payload) < 8 ||
		!schema.EventEnvelopeBufferHasIdentifier(payload) {
		return flatbuffers.Table{}, ErrProtocol
	}
	envelope := schema.GetRootAsEventEnvelope(payload, 0)
	if envelope.SchemaVersion() != 1 || envelope.PayloadType() != expected {
		return flatbuffers.Table{}, ErrProtocol
	}
	if !envelope.Payload(&table) {
		return flatbuffers.Table{}, ErrProtocol
	}
	return table, nil
}

func isActivationEvent(kind EventKind) bool {
	return kind == KindUserMsg || kind == KindDeliveredMsg || kind == KindAssertion
}

// eventText extracts a readable line only after the envelope's actual union
// payload type has been proven to match the journal event kind.
func eventText(kind EventKind, payload []byte) string {
	table, err := eventTable(kind, payload)
	if err != nil {
		return ""
	}
	switch kind {
	case KindUserMsg:
		var value schema.UserMsg
		value.Init(table.Bytes, table.Pos)
		return "user: " + string(value.ContentBytes())
	case KindDeliveredMsg:
		var value schema.DeliveredMsg
		value.Init(table.Bytes, table.Pos)
		return "assistant: " + string(value.ContentBytes())
	case KindReasoning:
		var value schema.Reasoning
		value.Init(table.Bytes, table.Pos)
		return "assistant (working): " + string(value.ContentBytes())
	case KindToolCall:
		var value schema.ToolCall
		value.Init(table.Bytes, table.Pos)
		return "tool call " + string(value.ToolName()) + ": " +
			string(value.ArgumentsBytes())
	case KindToolResult:
		var value schema.ToolResult
		value.Init(table.Bytes, table.Pos)
		return "tool result: " + string(value.ResultBytes())
	case KindAssertion:
		var value schema.Assertion
		value.Init(table.Bytes, table.Pos)
		return "memory " + string(value.CanonicalIdentity()) + ": " +
			string(value.ValueBytes())
	default:
		return ""
	}
}

// DecodedEvent is the protocol-neutral content Neo needs when rebuilding a
// readable transcript from typed Neocortex frames.
type DecodedEvent struct {
	Content  string
	ToolName string
}

// MemoryProjection is the typed, presentation-safe view of one activated
// semantic memory. It deliberately cannot carry an opaque event envelope.
type MemoryProjection struct {
	Tier            string   `json:"tier"`
	Kind            string   `json:"kind"`
	Text            string   `json:"text"`
	URI             string   `json:"uri"`
	ConversationID  string   `json:"conversation_id,omitempty"`
	Date            string   `json:"date,omitempty"`
	SourceType      string   `json:"source_type"`
	Confidence      float64  `json:"confidence,omitempty"`
	SelectionReason string   `json:"selection_reason,omitempty"`
	Provenance      []uint64 `json:"provenance,omitempty"`
}

// DecodeEvent decodes one typed transcript frame without exposing generated
// FlatBuffers tables to Neo.
func DecodeEvent(kind EventKind, payload []byte) (out DecodedEvent, err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			err = fmt.Errorf("%w: malformed event payload", ErrProtocol)
		}
	}()
	table, err := eventTable(kind, payload)
	if err != nil {
		return DecodedEvent{}, err
	}
	switch kind {
	case KindUserMsg:
		var value schema.UserMsg
		value.Init(table.Bytes, table.Pos)
		out.Content = string(value.ContentBytes())
	case KindDeliveredMsg:
		var value schema.DeliveredMsg
		value.Init(table.Bytes, table.Pos)
		out.Content = string(value.ContentBytes())
	case KindReasoning:
		var value schema.Reasoning
		value.Init(table.Bytes, table.Pos)
		out.Content = string(value.ContentBytes())
	case KindToolCall:
		var value schema.ToolCall
		value.Init(table.Bytes, table.Pos)
		out.ToolName = string(value.ToolName())
		out.Content = string(value.ArgumentsBytes())
	case KindToolResult:
		var value schema.ToolResult
		value.Init(table.Bytes, table.Pos)
		out.Content = string(value.ResultBytes())
	case KindAssertion:
		var value schema.Assertion
		value.Init(table.Bytes, table.Pos)
		out.Content = string(value.ValueBytes())
	}
	return out, nil
}

// RenderBundle turns the structured bundle into the activation string the
// resurrection loop consumes. Rendering is the client's job by design.
func RenderBundle(bundle *Bundle, premises []string) string {
	if bundle == nil {
		return ""
	}
	var out strings.Builder
	seenURI := make(map[string]struct{})
	seenContent := make(map[[32]byte]struct{})
	for index := range bundle.Sections {
		section := &bundle.Sections[index]
		if len(section.Items) == 0 {
			continue
		}
		var rendered []string
		for _, item := range section.Items {
			line := renderItem(index, item)
			if line == "" {
				continue
			}
			digest := sha256.Sum256([]byte(line))
			if _, exists := seenURI[item.URI]; item.URI != "" && exists {
				continue
			}
			if _, exists := seenContent[digest]; exists {
				continue
			}
			if item.URI != "" {
				seenURI[item.URI] = struct{}{}
			}
			seenContent[digest] = struct{}{}
			rendered = append(rendered, line)
		}
		if len(rendered) == 0 {
			continue
		}
		out.WriteString("[" + tierNames[index] + "]\n")
		for _, line := range rendered {
			out.WriteString(line)
			out.WriteString("\n")
		}
	}
	if len(premises) > 0 {
		out.WriteString("[premises]\n")
		for _, premise := range premises {
			out.WriteString(premise)
			out.WriteString("\n")
		}
	}
	return strings.TrimRight(out.String(), "\n")
}

// ProjectBundle returns only decoded semantic memories suitable for a UI.
// Operational work, checkpoints, supervisor/recovery records, and malformed or
// mismatched envelopes are omitted while remaining retrievable through their
// dedicated recovery APIs.
func ProjectBundle(bundle *Bundle) []MemoryProjection {
	if bundle == nil {
		return nil
	}
	seen := make(map[[32]byte]struct{})
	var projected []MemoryProjection
	for sectionIndex := range bundle.Sections {
		if sectionIndex != 0 && sectionIndex != 1 && sectionIndex != 4 &&
			sectionIndex != 5 && sectionIndex != 6 {
			continue
		}
		for _, item := range bundle.Sections[sectionIndex].Items {
			entry := MemoryProjection{
				Tier: tierNames[sectionIndex], URI: item.URI,
				Provenance: append([]uint64(nil), item.Provenance...),
			}
			if sectionIndex == 0 || sectionIndex == 1 || sectionIndex == 5 {
				if !utf8.Valid(item.Content) || internalActivationContent(item.Content) {
					continue
				}
				entry.Text = strings.TrimSpace(string(item.Content))
				entry.Kind = "belief"
				entry.SourceType = "semantic_belief"
			} else {
				if item.Coarsened || len(item.Content) < 2 {
					continue
				}
				kind := EventKind(item.Content[0])
				if !isActivationEvent(kind) {
					continue
				}
				entry.Text = eventText(kind, item.Content[1:])
				if entry.Text == "" {
					continue
				}
				payloadType, _ := eventPayloadForKind(kind)
				entry.Kind = strings.ToLower(payloadType.String())
				entry.SourceType = payloadType.String()
				if sectionIndex == 6 {
					applyRecallProvenance(&entry)
				}
			}
			if entry.Text == "" {
				continue
			}
			digest := sha256.Sum256([]byte(entry.SourceType + "\x00" + entry.Text))
			if _, exists := seen[digest]; exists {
				continue
			}
			seen[digest] = struct{}{}
			projected = append(projected, entry)
		}
	}
	return projected
}

func applyRecallProvenance(entry *MemoryProjection) {
	parsed, err := url.Parse(entry.URI)
	if err != nil || parsed.Scheme != "recall" {
		return
	}
	entry.ConversationID = parsed.Host
	query := parsed.Query()
	entry.SelectionReason = strings.ReplaceAll(query.Get("why"), "_", " ")
	if entry.SelectionReason == "" {
		entry.SelectionReason = "relevance ranked cross-conversation recall"
	}
	if timestamp, err := strconv.ParseInt(query.Get("at_ns"), 10, 64); err == nil && timestamp > 0 {
		entry.Date = time.Unix(0, timestamp).UTC().Format("2006-01-02")
	}
	if entry.Date == "" {
		entry.Date = "unknown"
	}
	vectorRank, _ := strconv.ParseUint(query.Get("vector_rank"), 10, 32)
	lexicalRank, _ := strconv.ParseUint(query.Get("lexical_rank"), 10, 32)
	rank := vectorRank
	if rank == 0 || lexicalRank != 0 && lexicalRank < rank {
		rank = lexicalRank
	}
	if rank > 0 {
		entry.Confidence = 1 / float64(rank)
	} else {
		entry.Confidence = 0.25
	}
}

func renderItem(sectionIndex int, item BundleItem) string {
	switch sectionIndex {
	case 0, 1, 5:
		if !utf8.Valid(item.Content) || internalActivationContent(item.Content) {
			return ""
		}
		return strings.TrimSpace(string(item.Content))
	case 2:
		return renderWork(item.Content)
	case 3, 4, 6:
		if item.Coarsened {
			return "(coarsened " + item.URI + ")"
		}
		if len(item.Content) < 2 {
			return ""
		}
		kind := EventKind(item.Content[0])
		if !isActivationEvent(kind) {
			return ""
		}
		if text := eventText(kind, item.Content[1:]); text != "" {
			if sectionIndex == 6 {
				return "past memory [" + item.URI + "]: " + text
			}
			return text
		}
		return ""
	case 7:
		return "(time window " + item.URI + " members " +
			strconv.Itoa(len(item.Provenance)) + ")"
	default:
		return item.URI
	}
}

func internalActivationContent(content []byte) bool {
	text := strings.ToLower(string(content))
	for _, marker := range []string{
		"ncev", "neo.neocortex.memory-state.v1", "turn checkpoint",
		"neo.o1.", "supervisor record", "recovery record",
	} {
		if strings.Contains(text, marker) {
			return true
		}
	}
	return false
}

func renderWork(content []byte) string {
	cursor := &bundleCursor{bytes: content}
	kind, err1 := cursor.u8()
	state, err2 := cursor.u8()
	reconcile, err3 := cursor.u8()
	_, err4 := cursor.u64()
	_, err5 := cursor.u64()
	if err1 != nil || err2 != nil || err3 != nil || err4 != nil || err5 != nil {
		return ""
	}
	_, _ = cursor.blob()
	toolName, err := cursor.blob()
	if err != nil {
		return ""
	}
	states := [4]string{"dispatched", "committed", "returned", "outcome_unknown"}
	label := "effect"
	if kind == 0 {
		label = "call"
	}
	line := "work " + label + " " + string(toolName)
	if int(state) < len(states) {
		line += " " + states[state]
	}
	if reconcile != 0 {
		line += " NEEDS-RECONCILIATION"
	}
	return line
}

// ToolExecution mirrors the loop's committed tool execution evidence.
type ToolExecution struct {
	CallID         string
	ToolName       string
	Arguments      json.RawMessage
	Result         json.RawMessage
	Error          string
	Expect         string
	IdempotencyKey string
	MatchVerdict   string
	SubgoalID      string
}

// ToolEventCitation pins acknowledged execution evidence to a Neocortex MMR
// prefix. It is a Neocortex wire contract, independent of every legacy store.
type ToolEventCitation struct {
	Seq         uint64   `json:"seq"`
	LeafCount   uint64   `json:"leaf_count"`
	LeafHash    [32]byte `json:"leaf_hash"`
	JournalRoot [32]byte `json:"journal_root"`
}

// ToolEventPayload is the execution evidence Neo verifies against a citation.
type ToolEventPayload struct {
	SchemaVersion  uint8
	CallID         string
	ToolName       string
	Arguments      []byte
	ResultDigest   [32]byte
	Error          string
	Expect         string
	MatchVerdict   string
	SubgoalID      string
	IdempotencyKey string
}

var ErrToolCitationMismatch = errors.New("neocortex: tool-event citation mismatch")

const (
	ToolMatchMatched    = "matched"
	ToolMatchMismatched = "mismatched"
	ToolMatchUnknown    = "unknown"
)

// ActivationQuery mirrors the loop's activation request.
type ActivationQuery struct {
	Query    string
	Premises []string
}

// LoopSeam implements the resurrection loop seam roles for ONE conversation
// over the cortexd protocol: activation source, turn recorder, evidence
// journal, consolidation sink, and turn-checkpoint store.
type LoopSeam struct {
	client         *Client
	conversationID string
	conversation   [16]byte
	tokenWeights   []uint16
	tokenOverhead  uint32
	budgetTokens   uint64

	mu          sync.Mutex
	firstLsn    uint64
	lastLsn     uint64
	recordError error
}

// SeamConfig bounds one LoopSeam.
type SeamConfig struct {
	ConversationID string
	BudgetTokens   uint64
	TokenWeights   []uint16
	TokenOverhead  uint32
}

// NewLoopSeam binds a seam to a client and conversation.
func NewLoopSeam(client *Client, cfg SeamConfig) (*LoopSeam, error) {
	if client == nil || cfg.ConversationID == "" || cfg.BudgetTokens == 0 {
		return nil, fmt.Errorf("%w: seam config", ErrProtocol)
	}
	weights := cfg.TokenWeights
	if len(weights) == 0 {
		weights = DefaultTokenWeights()
	}
	if len(weights) != 256 {
		return nil, fmt.Errorf("%w: token table must have 256 entries", ErrProtocol)
	}
	overhead := cfg.TokenOverhead
	if overhead == 0 {
		overhead = 4
	}
	return &LoopSeam{
		client:         client,
		conversationID: cfg.ConversationID,
		conversation:   ConversationBytes(cfg.ConversationID),
		tokenWeights:   weights,
		tokenOverhead:  overhead,
		budgetTokens:   cfg.BudgetTokens,
	}, nil
}

// Conversation returns the derived wire conversation id.
func (s *LoopSeam) Conversation() [16]byte { return s.conversation }

// Activate composes and renders the activation for this conversation.
func (s *LoopSeam) Activate(ctx context.Context, query ActivationQuery) (string, error) {
	bundle, err := s.ActivateBundle(ctx, query)
	if err != nil {
		return "", err
	}
	return RenderBundle(bundle, query.Premises), nil
}

// ActivateBundle returns the structured activation bundle.
func (s *LoopSeam) ActivateBundle(ctx context.Context, query ActivationQuery) (*Bundle, error) {
	turnText := strings.Join(query.Premises, "\n")
	table, _, err := s.client.request(ctx,
		func(builder *flatbuffers.Builder, requestID uint64) []byte {
			conversation := builder.CreateByteVector(s.conversation[:])
			queryBytes := builder.CreateByteVector([]byte(query.Query))
			var turnBytes flatbuffers.UOffsetT
			if turnText != "" {
				turnBytes = builder.CreateByteVector([]byte(turnText))
			}
			protocol.ActivateStartTokenWeightsVector(builder, len(s.tokenWeights))
			for index := len(s.tokenWeights) - 1; index >= 0; index-- {
				builder.PrependUint16(s.tokenWeights[index])
			}
			weights := builder.EndVector(len(s.tokenWeights))
			protocol.ActivateStart(builder)
			protocol.ActivateAddConversation(builder, conversation)
			protocol.ActivateAddQuery(builder, queryBytes)
			if turnText != "" {
				protocol.ActivateAddTurnText(builder, turnBytes)
			}
			protocol.ActivateAddBudgetTokens(builder, s.budgetTokens)
			protocol.ActivateAddTemporalFromNs(builder, 0)
			protocol.ActivateAddTemporalToNs(builder, int64(1)<<62)
			protocol.ActivateAddTokenWeights(builder, weights)
			protocol.ActivateAddTokenItemOverhead(builder, s.tokenOverhead)
			activate := protocol.ActivateEnd(builder)
			return finishRequest(builder, requestID,
				protocol.RequestPayloadActivate, activate)
		}, protocol.ResponsePayloadBytesResult)
	if err != nil {
		return nil, err
	}
	var result protocol.BytesResult
	result.Init(table.Bytes, table.Pos)
	return ParseBundle(result.BytesBytes())
}

func (s *LoopSeam) noteSpan(ack AppendAck, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err != nil {
		s.recordError = err
		return
	}
	if s.firstLsn == 0 || ack.FirstLsn < s.firstLsn {
		s.firstLsn = ack.FirstLsn
	}
	if ack.LastLsn > s.lastLsn {
		s.lastLsn = ack.LastLsn
	}
}

// RecordUser records the inbound user message.
func (s *LoopSeam) RecordUser(content string) {
	if content == "" {
		return
	}
	ack, queued, err := s.client.queueAppend(context.Background(),
		[]AppendEvent{UserMsgEvent(s.conversation, content)}, s.noteSpan)
	if !queued {
		s.noteSpan(ack, err)
	}
}

// RecordAssistantWorking records undelivered assistant content as reasoning
// evidence; only RecordDelivery mints a delivered_msg.
func (s *LoopSeam) RecordAssistantWorking(content string) {
	if content == "" {
		return
	}
	ack, queued, err := s.client.queueAppend(context.Background(),
		[]AppendEvent{ReasoningEvent(s.conversation, content)}, s.noteSpan)
	if !queued {
		s.noteSpan(ack, err)
	}
}

// RecordDelivery records the delivered assistant answer.
func (s *LoopSeam) RecordDelivery(content string) {
	if content == "" {
		return
	}
	ack, queued, err := s.client.queueAppend(context.Background(),
		[]AppendEvent{DeliveredMsgEvent(s.conversation, content)}, s.noteSpan)
	if !queued {
		s.noteSpan(ack, err)
	}
}

// ProvenanceRange reports the conversation and LSN span recorded this turn.
func (s *LoopSeam) ProvenanceRange() (string, uint64, uint64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.conversationID, s.firstLsn, s.lastLsn
}

// RecordError surfaces the most recent degraded-mode recording failure, if
// any, so callers can report honestly.
func (s *LoopSeam) RecordError() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.recordError
}

// CommitToolExecution appends the tool call and its result as typed evidence
// and returns a citation anchored in the engine's MMR.
func (s *LoopSeam) CommitToolExecution(ctx context.Context, execution ToolExecution) (ToolEventCitation, error) {
	if execution.CallID == "" || execution.ToolName == "" {
		return ToolEventCitation{}, fmt.Errorf("%w: tool execution requires call id and tool name", ErrProtocol)
	}
	arguments := execution.Arguments
	if len(arguments) == 0 {
		arguments = json.RawMessage(`{}`)
	}
	callAck, err := s.client.Append(ctx, []AppendEvent{
		ToolCallEvent(s.conversation, execution.CallID, execution.ToolName,
			arguments)})
	if err != nil {
		return ToolEventCitation{}, err
	}
	result := execution.Result
	ok := execution.Error == ""
	if !ok {
		result = json.RawMessage(strconv.AppendQuote(nil, execution.Error))
	}
	if len(result) == 0 {
		result = json.RawMessage(`null`)
	}
	resultAck, err := s.client.Append(ctx, []AppendEvent{
		ToolResultEvent(s.conversation, execution.CallID, callAck.FirstLsn, ok,
			result)})
	if err != nil {
		return ToolEventCitation{}, err
	}
	s.noteSpan(callAck, nil)
	s.noteSpan(resultAck, nil)
	return ToolEventCitation{
		Seq:         resultAck.LastLsn,
		LeafCount:   resultAck.LeafCount,
		LeafHash:    resultAck.LastLeafHash,
		JournalRoot: resultAck.Root,
	}, nil
}

// Consolidate appends one consolidation event carrying extracted assertions.
// The extraction itself stays in Go policy code; this is the sink.
func (s *LoopSeam) Consolidate(ctx context.Context, assertions []Assertion) (AppendAck, error) {
	if len(assertions) == 0 {
		return AppendAck{}, fmt.Errorf("%w: empty consolidation", ErrProtocol)
	}
	return s.client.Append(ctx, []AppendEvent{
		ConsolidationEvent(s.conversation, assertions)})
}

// SaveTurnCheckpoint durably stores the latest checkpoint blob for a turn.
func (s *LoopSeam) SaveTurnCheckpoint(ctx context.Context, turnID string, blob []byte) (uint64, error) {
	return s.client.WriteCheckpoint(ctx, turnID, blob)
}

// LatestTurnCheckpoint returns the newest checkpoint blob for a turn, or
// ErrAbsent.
func (s *LoopSeam) LatestTurnCheckpoint(ctx context.Context, turnID string) ([]byte, uint64, error) {
	return s.client.LatestCheckpoint(ctx, turnID)
}

// WriteCheckpoint stores a turn checkpoint blob as a checkpoint event through
// the same idempotent sequence chain as Append: a resend after a connection
// break acknowledges the committed lsn instead of double-applying.
func (c *Client) WriteCheckpoint(ctx context.Context, turnID string, blob []byte) (uint64, error) {
	if turnID == "" || len(blob) == 0 {
		return 0, fmt.Errorf("%w: checkpoint requires turn id and blob", ErrProtocol)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if err := c.ensureLocked(); err != nil {
		return 0, err
	}
	c.clientSeq++
	var lsn uint64
	send := func(clientSeq uint64) error {
		requestID := c.nextRequestIDLocked()
		builder := flatbuffers.NewBuilder(512)
		turn := builder.CreateByteVector([]byte(turnID))
		payload := builder.CreateByteVector(blob)
		protocol.CheckpointStart(builder)
		protocol.CheckpointAddTurnId(builder, turn)
		protocol.CheckpointAddBlob(builder, payload)
		protocol.CheckpointAddClientSeq(builder, clientSeq)
		checkpoint := protocol.CheckpointEnd(builder)
		finished := finishRequest(builder, requestID,
			protocol.RequestPayloadCheckpoint, checkpoint)
		response, _, err := c.roundTripLocked(requestID, finished)
		if err != nil {
			return err
		}
		table, err := responseTable(response, protocol.ResponsePayloadCheckpointAck)
		if err != nil {
			return err
		}
		var ack protocol.CheckpointAck
		ack.Init(table.Bytes, table.Pos)
		lsn = ack.Lsn()
		return nil
	}
	err := c.writeAdmittedLocked(send)
	var rejected *rejectedWriteError
	if err != nil && !errors.Is(err, ErrSequenceRecovery) &&
		!errors.As(err, &rejected) {
		err = c.retainPendingLocked(send, err, nil, nil)
	}
	return lsn, err
}

// LatestCheckpoint reads the newest checkpoint blob for a turn.
func (c *Client) LatestCheckpoint(ctx context.Context, turnID string) ([]byte, uint64, error) {
	if turnID == "" {
		return nil, 0, fmt.Errorf("%w: checkpoint requires turn id", ErrProtocol)
	}
	table, _, err := c.request(ctx,
		func(builder *flatbuffers.Builder, requestID uint64) []byte {
			turn := builder.CreateByteVector([]byte(turnID))
			protocol.LatestCheckpointStart(builder)
			protocol.LatestCheckpointAddTurnId(builder, turn)
			latest := protocol.LatestCheckpointEnd(builder)
			return finishRequest(builder, requestID,
				protocol.RequestPayloadLatestCheckpoint, latest)
		}, protocol.ResponsePayloadCheckpointResult)
	if err != nil {
		return nil, 0, err
	}
	var result protocol.CheckpointResult
	result.Init(table.Bytes, table.Pos)
	if !result.Present() {
		return nil, 0, ErrAbsent
	}
	blob := make([]byte, len(result.BlobBytes()))
	copy(blob, result.BlobBytes())
	return blob, result.Lsn(), nil
}

// TranscriptRecord is one log frame served by the Transcript read.
type TranscriptRecord struct {
	Lsn             uint64
	Kind            EventKind
	WallTimestampNs int64
	Payload         []byte
}

// Transcript reads conversation frames after sinceLsn.
func (c *Client) Transcript(ctx context.Context, conversation [16]byte,
	sinceLsn uint64, limit uint32) ([]TranscriptRecord, bool, error) {
	table, _, err := c.request(ctx,
		func(builder *flatbuffers.Builder, requestID uint64) []byte {
			conversationOffset := builder.CreateByteVector(conversation[:])
			protocol.TranscriptStart(builder)
			protocol.TranscriptAddConversation(builder, conversationOffset)
			protocol.TranscriptAddSinceLsn(builder, sinceLsn)
			protocol.TranscriptAddLimit(builder, limit)
			transcript := protocol.TranscriptEnd(builder)
			return finishRequest(builder, requestID,
				protocol.RequestPayloadTranscript, transcript)
		}, protocol.ResponsePayloadTranscriptResult)
	if err != nil {
		return nil, false, err
	}
	var result protocol.TranscriptResult
	result.Init(table.Bytes, table.Pos)
	records := make([]TranscriptRecord, 0, result.RecordsLength())
	for index := 0; index < result.RecordsLength(); index++ {
		var record protocol.FrameRecord
		if !result.Records(&record, index) {
			return nil, false, ErrProtocol
		}
		payload := make([]byte, len(record.PayloadBytes()))
		copy(payload, record.PayloadBytes())
		records = append(records, TranscriptRecord{
			Lsn:             record.Lsn(),
			Kind:            EventKind(record.Kind()),
			WallTimestampNs: record.WallTimestampNs(),
			Payload:         payload,
		})
	}
	return records, result.Truncated(), nil
}

// Attest returns surfaced-memory attestations as events through the same
// idempotent sequence chain as Append.
func (c *Client) Attest(ctx context.Context, used, ignored []uint64) (uint32, error) {
	if len(used)+len(ignored) == 0 {
		return 0, fmt.Errorf("%w: empty attestation", ErrProtocol)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if err := c.ensureLocked(); err != nil {
		return 0, err
	}
	c.clientSeq++
	var count uint32
	send := func(clientSeq uint64) error {
		requestID := c.nextRequestIDLocked()
		builder := flatbuffers.NewBuilder(512)
		var usedOffset, ignoredOffset flatbuffers.UOffsetT
		if len(used) > 0 {
			protocol.AttestStartUsedVector(builder, len(used))
			for index := len(used) - 1; index >= 0; index-- {
				builder.PrependUint64(used[index])
			}
			usedOffset = builder.EndVector(len(used))
		}
		if len(ignored) > 0 {
			protocol.AttestStartIgnoredVector(builder, len(ignored))
			for index := len(ignored) - 1; index >= 0; index-- {
				builder.PrependUint64(ignored[index])
			}
			ignoredOffset = builder.EndVector(len(ignored))
		}
		protocol.AttestStart(builder)
		if len(used) > 0 {
			protocol.AttestAddUsed(builder, usedOffset)
		}
		if len(ignored) > 0 {
			protocol.AttestAddIgnored(builder, ignoredOffset)
		}
		protocol.AttestAddClientSeq(builder, clientSeq)
		attest := protocol.AttestEnd(builder)
		finished := finishRequest(builder, requestID,
			protocol.RequestPayloadAttest, attest)
		response, _, err := c.roundTripLocked(requestID, finished)
		if err != nil {
			return err
		}
		table, err := responseTable(response, protocol.ResponsePayloadAttestAck)
		if err != nil {
			return err
		}
		var ack protocol.AttestAck
		ack.Init(table.Bytes, table.Pos)
		count = ack.Count()
		return nil
	}
	err := c.writeAdmittedLocked(send)
	var rejected *rejectedWriteError
	if err != nil && !errors.Is(err, ErrSequenceRecovery) &&
		!errors.As(err, &rejected) {
		err = c.retainPendingLocked(send, err, nil, nil)
	}
	return count, err
}
