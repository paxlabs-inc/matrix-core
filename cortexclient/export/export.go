// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

// Package export walks an existing cortex Pebble store with the current
// decode and vault unseal paths and replays it through the cortexd import
// gate: session transcripts become conversation events, memories become
// assertion events with synthesized provenance, tool events map one to one,
// and supersedes/contradicts edges are preserved through canonical-identity
// grouping and conflict domains. Every source record is accounted in the
// fidelity report as imported, transformed, or skipped-with-reason; an
// unaccounted record fails the export.
package export

import (
	"bytes"
	"context"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
	"time"

	"matrix/cortex"
	"matrix/cortex/journal"
	"matrix/cortex/keys"
	"matrix/cortex/memory"
	"matrix/cortex/store"
	"matrix/cortexclient"
	"matrix/vault"

	flatbuffers "matrix/cortexclient/internal/flatbuffers"
	"matrix/cortexclient/wire/neocortex/schema"
)

// kMaximumIdentifierBytes mirrors the engine's schema bound on call ids,
// belief ids, and canonical identities. Oversized source identifiers are
// skipped with a reason, never truncated or silently dropped.
const maximumIdentifierBytes = 256

const (
	memoriesConversation   = "import:memories"
	toolEventsConversation = "import:tool-events"
)

// Options configures one export run.
type Options struct {
	Root       string
	Actor      string
	Vault      *vault.Session
	VaultUser  string
	Client     *cortexclient.Client
	BatchLimit int
}

// LaneReport accounts one source namespace. Source must equal
// Imported + Transformed + the sum of Skipped counts.
type LaneReport struct {
	Source      uint64
	Imported    uint64
	Transformed uint64
	Skipped     map[string]uint64
}

func (l *LaneReport) skip(reason string) {
	if l.Skipped == nil {
		l.Skipped = make(map[string]uint64)
	}
	l.Skipped[reason]++
}

// SkippedTotal sums every skip reason.
func (l *LaneReport) SkippedTotal() uint64 {
	var total uint64
	for _, count := range l.Skipped {
		total += count
	}
	return total
}

// Unaccounted returns how many source records this lane failed to account.
func (l *LaneReport) Unaccounted() uint64 {
	accounted := l.Imported + l.Transformed + l.SkippedTotal()
	if accounted >= l.Source {
		return 0
	}
	return l.Source - accounted
}

// Report is the export fidelity report.
type Report struct {
	Sessions     LaneReport
	SessionBlobs LaneReport
	Memories     LaneReport
	ToolEvents   LaneReport
	Journal      LaneReport
	Edges        LaneReport

	EventsAppended uint64
	LastLsn        uint64
	LeafCount      uint64
	Root           [32]byte
}

func (r *Report) lanes() map[string]*LaneReport {
	return map[string]*LaneReport{
		"sessions":      &r.Sessions,
		"session_blobs": &r.SessionBlobs,
		"memories":      &r.Memories,
		"tool_events":   &r.ToolEvents,
		"journal":       &r.Journal,
		"edges":         &r.Edges,
	}
}

// Complete enforces the fidelity gate: zero unaccounted source records.
func (r *Report) Complete() error {
	var failures []string
	for name, lane := range r.lanes() {
		if missing := lane.Unaccounted(); missing > 0 {
			failures = append(failures,
				fmt.Sprintf("%s: %d unaccounted", name, missing))
		}
	}
	if len(failures) > 0 {
		sort.Strings(failures)
		return fmt.Errorf("export: fidelity gate failed: %s",
			strings.Join(failures, "; "))
	}
	return nil
}

type sessionRecord struct {
	conv   string
	seq    uint64
	record cortex.SessionRecord
}

type memoryVersion struct {
	id      memory.ID
	version memory.Version
}

type memoryGroup struct {
	root       keys.ULID
	identity   string
	versions   []memoryVersion
	retracts   []memory.ID
	conflictBy string
}

type unionFind struct {
	parent map[keys.ULID]keys.ULID
}

func (u *unionFind) find(id keys.ULID) keys.ULID {
	parent, ok := u.parent[id]
	if !ok || parent == id {
		return id
	}
	root := u.find(parent)
	u.parent[id] = root
	return root
}

func (u *unionFind) union(a, b keys.ULID) {
	rootA, rootB := u.find(a), u.find(b)
	if rootA == rootB {
		return
	}
	// Deterministic root: the byte-smaller ULID wins, independent of the
	// order edges are scanned in.
	if bytes.Compare(rootA[:], rootB[:]) < 0 {
		u.parent[rootB] = rootA
	} else {
		u.parent[rootA] = rootB
	}
}

// Run exports the store and returns the fidelity report. The report is
// returned alongside any error so a partial run stays inspectable.
func Run(ctx context.Context, opts Options) (*Report, error) {
	report := &Report{}
	if opts.Client == nil {
		return report, fmt.Errorf("export: cortexd client is required")
	}
	batchLimit := opts.BatchLimit
	if batchLimit <= 0 || batchLimit > 256 {
		batchLimit = 256
	}
	source, err := store.Open(opts.Root, opts.Actor, nil)
	if err != nil {
		return report, fmt.Errorf("export: open source store: %w", err)
	}
	defer source.Close()
	source.SetVault(opts.Vault, opts.VaultUser)

	exporter := &run{
		ctx:        ctx,
		source:     source,
		client:     opts.Client,
		report:     report,
		batchLimit: batchLimit,
	}
	if err := exporter.exportSessions(); err != nil {
		return report, err
	}
	if err := exporter.exportMemories(); err != nil {
		return report, err
	}
	if err := exporter.exportToolEvents(); err != nil {
		return report, err
	}
	if err := exporter.flush(); err != nil {
		return report, err
	}
	return report, report.Complete()
}

type run struct {
	ctx        context.Context
	source     *store.Store
	client     *cortexclient.Client
	report     *Report
	batchLimit int

	batch   []cortexclient.AppendEvent
	lastLsn uint64
}

func (r *run) append(event cortexclient.AppendEvent) error {
	r.batch = append(r.batch, event)
	if len(r.batch) >= r.batchLimit {
		return r.flush()
	}
	return nil
}

func (r *run) flush() error {
	if len(r.batch) == 0 {
		return nil
	}
	events := r.batch
	r.batch = nil
	ack, err := r.client.Append(r.ctx, events)
	if err != nil {
		return fmt.Errorf("export: import gate rejected batch: %w", err)
	}
	r.lastLsn = ack.LastLsn
	r.report.EventsAppended += uint64(len(events))
	r.report.LastLsn = ack.LastLsn
	r.report.LeafCount = ack.LeafCount
	r.report.Root = ack.Root
	return nil
}

// provenance synthesizes the [1, lastLsn] range required by the belief
// schema. Import order (sessions before memories) makes the range cover the
// conversation evidence the memory was distilled from.
func (r *run) provenance() [][2]uint64 {
	hi := r.lastLsn
	if hi == 0 {
		hi = 1
	}
	return [][2]uint64{{1, hi}}
}

func (r *run) exportSessions() error {
	blobs := make(map[string][]byte)
	if err := r.source.PrefixIter(keys.PrefixSessionBlob,
		func(key, value []byte) error {
			r.report.SessionBlobs.Source++
			blobs[string(append([]byte(nil), key...))] =
				append([]byte(nil), value...)
			return nil
		}); err != nil {
		return fmt.Errorf("export: scan session blobs: %w", err)
	}
	blobUsed := make(map[string]bool, len(blobs))

	var records []sessionRecord
	if err := r.source.PrefixIter(keys.PrefixSession,
		func(key, value []byte) error {
			r.report.Sessions.Source++
			conv, seq, err := keys.ParseSessionKey(key)
			if err != nil {
				r.report.Sessions.skip("undecodable session key")
				return nil
			}
			var record cortex.SessionRecord
			if err := cortex.DecodeSessionRecord(value, &record); err != nil {
				r.report.Sessions.skip("undecodable session record")
				return nil
			}
			records = append(records, sessionRecord{
				conv: conv, seq: seq, record: record,
			})
			return nil
		}); err != nil {
		return fmt.Errorf("export: scan sessions: %w", err)
	}
	sort.Slice(records, func(i, j int) bool {
		if records[i].conv != records[j].conv {
			return records[i].conv < records[j].conv
		}
		return records[i].seq < records[j].seq
	})

	callLsn := make(map[string]uint64)
	callID := make(map[string]string)
	for _, entry := range records {
		conversation := cortexclient.ConversationBytes(entry.conv)
		record := entry.record
		switch cortex.Role(record.Role) {
		case cortex.RoleUser:
			if record.Content == "" {
				r.report.Sessions.skip("empty user message")
				continue
			}
			if err := r.append(cortexclient.UserMsgEvent(
				conversation, record.Content)); err != nil {
				return err
			}
			r.report.Sessions.Transformed++
		case cortex.RoleAssistant:
			if record.Content == "" {
				r.report.Sessions.skip("empty assistant message")
				continue
			}
			if err := r.append(cortexclient.DeliveredMsgEvent(
				conversation, record.Content)); err != nil {
				return err
			}
			r.report.Sessions.Transformed++
		case cortex.RoleToolCall:
			id := fmt.Sprintf("import:%s:%d", entry.conv, entry.seq)
			if len(id) > maximumIdentifierBytes {
				r.report.Sessions.skip("call id exceeds identifier bound")
				continue
			}
			arguments := record.ToolArgs
			if len(arguments) == 0 {
				arguments = []byte("{}")
			}
			if err := r.append(cortexclient.ToolCallEvent(
				conversation, id, record.ToolName, arguments)); err != nil {
				return err
			}
			// Flush so the acknowledged tail lsn IS this call's lsn; the
			// paired result event references it exactly.
			if err := r.flush(); err != nil {
				return err
			}
			callLsn[entry.conv] = r.lastLsn
			callID[entry.conv] = id
			r.report.Sessions.Transformed++
		case cortex.RoleToolResult:
			content := []byte(record.Content)
			resolvedSpill := false
			if record.ToolResultRef != "" {
				blobKey, err := keys.SessionBlobKey(entry.conv, entry.seq)
				if err == nil {
					if blob, ok := blobs[string(blobKey)]; ok {
						content = blob
						blobUsed[string(blobKey)] = true
						resolvedSpill = true
					}
				}
				if !resolvedSpill {
					r.report.Sessions.skip("spilled tool result blob missing")
					continue
				}
			}
			if len(content) == 0 {
				content = []byte("null")
			}
			id := callID[entry.conv]
			if id == "" {
				id = fmt.Sprintf("import:%s:%d", entry.conv, entry.seq)
			}
			if len(id) > maximumIdentifierBytes {
				r.report.Sessions.skip("call id exceeds identifier bound")
				continue
			}
			if err := r.append(cortexclient.ToolResultEvent(
				conversation, id, callLsn[entry.conv], true,
				content)); err != nil {
				return err
			}
			r.report.Sessions.Transformed++
		case cortex.RoleSystem:
			r.report.Sessions.skip("system role has no evidence kind")
		default:
			r.report.Sessions.skip("unknown session role")
		}
	}
	if err := r.flush(); err != nil {
		return err
	}

	for key := range blobs {
		if blobUsed[key] {
			r.report.SessionBlobs.Transformed++
		} else {
			r.report.SessionBlobs.skip("orphan session blob")
		}
	}
	return nil
}

func beliefType(t memory.Type) (uint8, bool) {
	switch t {
	case memory.TypeIdentity:
		return uint8(schema.BeliefTypeidentity), true
	case memory.TypeFact, memory.TypeBelief, memory.TypeEvent,
		memory.TypeCapability, memory.TypePattern:
		return uint8(schema.BeliefTypefact), true
	case memory.TypePreference:
		return uint8(schema.BeliefTypepreference), true
	case memory.TypeGoal:
		return uint8(schema.BeliefTypegoal), true
	case memory.TypeConstraint:
		return uint8(schema.BeliefTypeconstraint), true
	default:
		return 0, false
	}
}

func assertionValue(version memory.Version) []byte {
	if medium := strings.TrimSpace(version.Forms.Medium); medium != "" {
		return []byte(medium)
	}
	if short := strings.TrimSpace(version.Forms.Short); short != "" {
		return []byte(short)
	}
	return version.Data
}

func (r *run) exportMemories() error {
	edges := &unionFind{parent: make(map[keys.ULID]keys.ULID)}
	type conflictEdge struct {
		src keys.ULID
		dst keys.ULID
	}
	var conflicts []conflictEdge
	if err := r.source.PrefixIter(keys.PrefixEdgeFrom,
		func(key, value []byte) error {
			r.report.Edges.Source++
			src, edgeType, dst, err := keys.ParseEdgeFromKey(key)
			if err != nil {
				r.report.Edges.skip("undecodable edge key")
				return nil
			}
			var record memory.EdgeRecord
			if err := memory.DecodeEdge(value, &record); err != nil {
				r.report.Edges.skip("undecodable edge record")
				return nil
			}
			if record.Tombstoned {
				r.report.Edges.skip("tombstoned edge")
				return nil
			}
			switch memory.EdgeType(edgeType) {
			case memory.EdgeSupersedes:
				edges.union(src, dst)
				r.report.Edges.Transformed++
			case memory.EdgeContradicts:
				conflicts = append(conflicts, conflictEdge{src: src, dst: dst})
			default:
				r.report.Edges.skip(fmt.Sprintf(
					"edge type 0x%02x beyond the neocortex taxonomy",
					edgeType))
			}
			return nil
		}); err != nil {
		return fmt.Errorf("export: scan edges: %w", err)
	}

	groups := make(map[keys.ULID]*memoryGroup)
	groupFor := func(id keys.ULID) *memoryGroup {
		root := edges.find(id)
		group, ok := groups[root]
		if !ok {
			group = &memoryGroup{
				root:     root,
				identity: "import:" + hex.EncodeToString(root[:]),
			}
			groups[root] = group
		}
		return group
	}

	heads := make(map[keys.ULID]memory.Head)
	if err := r.source.PrefixIter(keys.PrefixMemoryHead,
		func(key, value []byte) error {
			r.report.Memories.Source++
			if len(key) != len(keys.PrefixMemoryHead)+keys.ULIDSize {
				r.report.Memories.skip("undecodable memory head key")
				return nil
			}
			var id keys.ULID
			copy(id[:], key[len(keys.PrefixMemoryHead):])
			var head memory.Head
			if err := memory.DecodeHead(value, &head); err != nil {
				r.report.Memories.skip("undecodable memory head")
				return nil
			}
			heads[id] = head
			return nil
		}); err != nil {
		return fmt.Errorf("export: scan memory heads: %w", err)
	}

	versionsSeen := make(map[keys.ULID]uint64)
	if err := r.source.PrefixIter(keys.PrefixMemoryVersion,
		func(key, value []byte) error {
			r.report.Memories.Source++
			var version memory.Version
			if err := memory.DecodeVersion(value, &version); err != nil {
				r.report.Memories.skip("undecodable memory version")
				return nil
			}
			var id keys.ULID
			copy(id[:], version.ID[:])
			if _, ok := beliefType(version.Type); !ok {
				r.report.Memories.skip(fmt.Sprintf(
					"memory type 0x%02x beyond the belief taxonomy",
					uint8(version.Type)))
				return nil
			}
			if len(assertionValue(version)) == 0 {
				r.report.Memories.skip("empty memory value")
				return nil
			}
			group := groupFor(id)
			group.versions = append(group.versions, memoryVersion{
				id: version.ID, version: version,
			})
			versionsSeen[id]++
			return nil
		}); err != nil {
		return fmt.Errorf("export: scan memory versions: %w", err)
	}

	for _, conflict := range conflicts {
		srcGroup := groupFor(conflict.src)
		dstGroup := groupFor(conflict.dst)
		if srcGroup == dstGroup {
			r.report.Edges.skip("contradicts edge inside one supersession chain")
			continue
		}
		if len(srcGroup.versions) == 0 || len(dstGroup.versions) == 0 {
			r.report.Edges.skip("contradicts edge endpoint has no importable versions")
			continue
		}
		if srcGroup.conflictBy != "" || dstGroup.conflictBy != "" {
			r.report.Edges.skip("conflict domain slot occupied")
			continue
		}
		lo, hi := conflict.src, conflict.dst
		if bytes.Compare(hi[:], lo[:]) < 0 {
			lo, hi = hi, lo
		}
		domain := "import:conflict:" + hex.EncodeToString(lo[:]) + ":" +
			hex.EncodeToString(hi[:])
		srcGroup.conflictBy = domain
		dstGroup.conflictBy = domain
		r.report.Edges.Transformed++
	}

	for id, head := range heads {
		if versionsSeen[id] == 0 {
			r.report.Memories.skip("memory head without importable versions")
			continue
		}
		if head.Tombstoned != nil {
			groupFor(id).retracts = append(groupFor(id).retracts, head.ID)
		}
		r.report.Memories.Transformed++
	}

	roots := make([]keys.ULID, 0, len(groups))
	for root := range groups {
		roots = append(roots, root)
	}
	sort.Slice(roots, func(i, j int) bool {
		return bytes.Compare(roots[i][:], roots[j][:]) < 0
	})
	conversation := cortexclient.ConversationBytes(memoriesConversation)
	for _, root := range roots {
		group := groups[root]
		sort.Slice(group.versions, func(i, j int) bool {
			left, right := group.versions[i], group.versions[j]
			if !left.version.CreatedAt.Equal(right.version.CreatedAt) {
				return left.version.CreatedAt.Before(right.version.CreatedAt)
			}
			if compared := bytes.Compare(left.id[:], right.id[:]); compared != 0 {
				return compared < 0
			}
			return left.version.Version < right.version.Version
		})
		for _, entry := range group.versions {
			mapped, _ := beliefType(entry.version.Type)
			validFrom := entry.version.CreatedAt
			if entry.version.ValidFrom != nil {
				validFrom = *entry.version.ValidFrom
			}
			var validTo int64
			if entry.version.ValidUntil != nil {
				validTo = entry.version.ValidUntil.UnixNano()
			}
			assertion := cortexclient.Assertion{
				BeliefID:          append([]byte(nil), entry.id[:]...),
				Type:              mapped,
				CanonicalIdentity: group.identity,
				Value:             assertionValue(entry.version),
				ValidFromNs:       clampNanos(validFrom),
				ValidToNs:         validTo,
				Provenance:        r.provenance(),
				ConflictDomain:    group.conflictBy,
			}
			if err := r.append(cortexclient.AssertionEvent(
				conversation, assertion)); err != nil {
				return err
			}
			r.report.Memories.Transformed++
		}
		sort.Slice(group.retracts, func(i, j int) bool {
			return bytes.Compare(group.retracts[i][:], group.retracts[j][:]) < 0
		})
		for _, beliefID := range group.retracts {
			if err := r.flush(); err != nil {
				return err
			}
			if err := r.append(retractEvent(
				conversation, append([]byte(nil), beliefID[:]...),
				r.provenance())); err != nil {
				return err
			}
		}
	}
	return r.flush()
}

func clampNanos(t time.Time) int64 {
	nanos := t.UnixNano()
	if nanos < 1 {
		return 1
	}
	return nanos
}

func (r *run) exportToolEvents() error {
	conversation := cortexclient.ConversationBytes(toolEventsConversation)
	type pending struct {
		payload journal.ToolEventPayload
	}
	var calls []pending
	if err := r.source.IterJournal(func(entry *journal.Entry) error {
		if entry.Kind != journal.KindToolEvent {
			r.report.Journal.Source++
			r.report.Journal.skip(fmt.Sprintf(
				"journal kind %q represented by primary-lane export",
				string(entry.Kind)))
			return nil
		}
		r.report.ToolEvents.Source++
		var payload journal.ToolEventPayload
		if err := journal.DecodeToolEventPayload(entry.Payload, &payload); err != nil {
			r.report.ToolEvents.skip("undecodable tool event payload")
			return nil
		}
		if payload.CallID == "" || len(payload.CallID) > maximumIdentifierBytes {
			r.report.ToolEvents.skip("call id exceeds identifier bound")
			return nil
		}
		calls = append(calls, pending{payload: payload})
		return nil
	}); err != nil {
		return fmt.Errorf("export: scan journal: %w", err)
	}

	for _, entry := range calls {
		payload := entry.payload
		arguments := payload.Arguments
		if len(arguments) == 0 {
			arguments = []byte("{}")
		}
		if err := r.append(cortexclient.ToolCallEvent(
			conversation, payload.CallID, payload.ToolName,
			arguments)); err != nil {
			return err
		}
		if err := r.flush(); err != nil {
			return err
		}
		// The source journal stores only the result digest; the imported
		// result event carries that digest explicitly so the transform is
		// visible, not silent.
		result := []byte(fmt.Sprintf(`{"result_digest":"%s"}`,
			hex.EncodeToString(payload.ResultDigest[:])))
		if err := r.append(cortexclient.ToolResultEvent(
			conversation, payload.CallID, r.lastLsn,
			payload.Error == "", result)); err != nil {
			return err
		}
		r.report.ToolEvents.Imported++
	}
	return r.flush()
}

func retractEvent(
	conversation [16]byte,
	beliefID []byte,
	provenance [][2]uint64,
) cortexclient.AppendEvent {
	return cortexclient.AppendEvent{
		Kind:         cortexclient.KindRetract,
		Conversation: conversation,
		Build: func(builder *flatbuffers.Builder) (byte, flatbuffers.UOffsetT) {
			id := builder.CreateByteVector(beliefID)
			schema.RetractStartProvenanceVector(builder, len(provenance))
			for index := len(provenance) - 1; index >= 0; index-- {
				schema.CreateProvenanceRange(builder,
					provenance[index][0], provenance[index][1])
			}
			ranges := builder.EndVector(len(provenance))
			schema.RetractStart(builder)
			schema.RetractAddBeliefId(builder, id)
			schema.RetractAddProvenance(builder, ranges)
			return byte(schema.EventPayloadRetract), schema.RetractEnd(builder)
		},
	}
}
