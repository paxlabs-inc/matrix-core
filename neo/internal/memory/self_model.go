package memory

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"matrix/codegraph/selfmodel"
	cgstore "matrix/codegraph/store"
	"matrix/cortex"
	cmem "matrix/cortex/memory"
	"matrix/cortex/query"
	cxself "matrix/cortex/self"
)

const (
	// selfModelSubject and structuralSelfCapability are the shared cortex keys
	// owned by the faculty-neutral self package, so the conversation faculty
	// writes the exact records the resolver (and every other faculty) reads.
	selfModelSubject               = cxself.Subject
	structuralSelfCapability       = cxself.StructuralCapability
	structuralSelfImportance uint8 = 8
	failurePatternImportance uint8 = 7

	// selfLookupPrefix routes a memory_recall query to the structural
	// self-graph instead of the ordinary cortex index: "self:<symbol>" pages
	// the fragment for that self-package symbol, and a bare "self:" returns the
	// resident structural self-summary. Keeping it on the existing recall verb
	// (rather than a new tool) satisfies req.2.2 — the full self-graph is
	// reachable on demand through the retrieval verb the agent already has.
	selfLookupPrefix = "self:"
)

// StructuralSelf, FailurePattern, and SelfModel are the faculty-neutral
// self-model types, owned by matrix/cortex/self and aliased here so the
// conversation faculty and the execution faculty share ONE definition rather
// than divergent per-faculty copies (self-model req.6.1/6.4).
type StructuralSelf = cxself.StructuralSelf

type FailurePattern = cxself.FailurePattern

type SelfModel = cxself.SelfModel

func (p *Pager) LoadStructuralSelf(ctx context.Context) (string, error) {
	path := filepath.Join(p.cfg.SelfModelGraph, "self-model.json")
	encoded, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	var artifact selfmodel.Artifact
	if err := json.Unmarshal(encoded, &artifact); err != nil {
		return "", err
	}
	structural := StructuralSelf{
		Summary:     artifact.Summary,
		GraphURI:    artifact.Merkle,
		Scope:       artifact.Scope,
		TokenBudget: len(strings.Fields(artifact.Summary)),
		// The agent's own context limit is part of "how I am built": recording it
		// in the derived self-model (from the live config, never a magic constant)
		// is what lets the decomposition router (self-model task 4.2) surface a
		// self-model-grounded limit at the spawn_subagents decision point.
		ContextLimit: p.cfg.ContextWindowTokens,
	}
	// Preserve the capability-surface facet across artifact reloads: the surface
	// is written by the serving layer (WriteSurfaceFacts), not the artifact, and
	// a boot-time reload must not drop it (epistemic-core req.2.1).
	if current, ok := p.currentStructural(); ok {
		structural.Surface = current.Surface
	}
	return p.WriteStructuralSelf(ctx, structural)
}

// recallSelf renders the on-demand self-graph paging behind a "self:<symbol>"
// memory_recall query (req.2.2). A concrete symbol returns its compact
// codegraph fragment (location + one-line summary, never raw source); a bare
// "self:" returns the resident structural self-summary the agent carries. A
// lookup miss degrades to a readable in-band message with a nil error so the
// tool surfaces it to the model rather than treating it as a transport failure.
func (p *Pager) recallSelf(ctx context.Context, symbol string) (string, error) {
	if symbol == "" {
		model, err := p.SelfModel(ctx)
		if err != nil {
			return fmt.Sprintf("self-model summary unavailable: %v", err), nil
		}
		summary := strings.TrimSpace(model.Structural.Summary)
		if summary == "" {
			return "No structural self-summary is loaded yet. Regenerate the self-graph (codegraph self-model) to populate it.", nil
		}
		return "Structural self-summary (derived from codegraph):\n" + summary + "\n\nPage a specific symbol with memory_recall query \"self:<Symbol>\".", nil
	}
	fragment, err := p.LookupStructuralSelf(symbol)
	if err != nil {
		return fmt.Sprintf("self-model lookup for %q is unavailable: %v", symbol, err), nil
	}
	fragment = strings.TrimSpace(fragment)
	if fragment == "" || strings.Contains(fragment, "matches=0") {
		return fmt.Sprintf("No self-model fragment matched %q. It may not be one of your own packages, or the self-graph needs regenerating.", symbol), nil
	}
	return "Self-model fragment (structural, from codegraph):\n" + fragment, nil
}

func (p *Pager) LookupStructuralSelf(symbol string) (string, error) {
	index, _, err := cgstore.Load(p.cfg.SelfModelGraph)
	if err != nil {
		return "", err
	}
	return selfmodel.Lookup(index, symbol)
}

func (p *Pager) WriteStructuralSelf(_ context.Context, structural StructuralSelf) (string, error) {
	structural.Summary = strings.TrimSpace(structural.Summary)
	structural.GraphURI = strings.TrimSpace(structural.GraphURI)
	if structural.Summary == "" {
		return "", nil
	}
	params, err := json.Marshal(structural)
	if err != nil {
		return "", err
	}
	data := cmem.CapabilityData{
		SchemaVersion: 1,
		Subject:       selfModelSubject,
		Capability:    structuralSelfCapability,
		Parameters:    params,
		Verified:      true,
		LastObserved:  time.Now().UTC(),
	}
	if current, ok := p.structuralSelfMemory(); ok {
		uri := cortex.BuildURI(current.Head.Type, current.Head.ID, current.Head.CurrentVersion)
		updated, updateErr := p.cortex.Update(uri, data, p.writeMeta())
		return string(updated), updateErr
	}
	uri, err := p.cortex.Write(p.head(structuralSelfImportance), data, p.writeMeta())
	return string(uri), err
}

// WriteSurfaceFacts merges the derived capability-surface facts (routes,
// is/is-not architectural facts — epistemic-core req.2.1) into the durable
// structural self record, preserving the artifact-derived fields. It writes
// even when no structural summary is loaded yet (a fresh install), so the true
// external surface is never dropped just because the self-graph is missing.
func (p *Pager) WriteSurfaceFacts(_ context.Context, facts cxself.SurfaceFacts) (string, error) {
	structural, _ := p.currentStructural()
	structural.Surface = &facts
	params, err := json.Marshal(structural)
	if err != nil {
		return "", err
	}
	data := cmem.CapabilityData{
		SchemaVersion: 1,
		Subject:       selfModelSubject,
		Capability:    structuralSelfCapability,
		Parameters:    params,
		Verified:      true,
		LastObserved:  time.Now().UTC(),
	}
	if current, ok := p.structuralSelfMemory(); ok {
		uri := cortex.BuildURI(current.Head.Type, current.Head.ID, current.Head.CurrentVersion)
		updated, updateErr := p.cortex.Update(uri, data, p.writeMeta())
		return string(updated), updateErr
	}
	uri, err := p.cortex.Write(p.head(structuralSelfImportance), data, p.writeMeta())
	return string(uri), err
}

func (p *Pager) WriteFailurePattern(_ context.Context, statement string, derivedFrom []string) (string, error) {
	statement = strings.TrimSpace(statement)
	if statement == "" {
		return "", nil
	}
	uri, err := p.cortex.Write(
		p.head(failurePatternImportance),
		cmem.BeliefData{
			SchemaVersion: 1,
			Statement:     statement,
			Subject:       selfModelSubject,
			Stance:        cmem.StanceBelieve,
			EvidenceFor:   cleanStrings(derivedFrom),
		},
		p.writeMeta(),
	)
	return string(uri), err
}

// SelfModel resolves the shared self-model artifact. It delegates to the
// faculty-neutral resolver (matrix/cortex/self) over this pager's cortex store,
// so the conversation faculty and every other faculty that reads the same store
// observe the byte-identical identity, structural summary, and failure patterns
// (self-model req.6.4) rather than a per-faculty reimplementation that can drift.
func (p *Pager) SelfModel(_ context.Context) (SelfModel, error) {
	return cxself.Resolve(p.cortex, p.cfg.AgentName)
}

// currentStructural decodes the durable structural self record, if any.
func (p *Pager) currentStructural() (StructuralSelf, bool) {
	var structural StructuralSelf
	current, ok := p.structuralSelfMemory()
	if !ok {
		return structural, false
	}
	decoded, err := cmem.DecodeData(current.Version.Type, current.Version.Data)
	if err != nil {
		return structural, false
	}
	data, isCap := decoded.(cmem.CapabilityData)
	if !isCap {
		return structural, false
	}
	return structural, json.Unmarshal(data.Parameters, &structural) == nil
}

func (p *Pager) structuralSelfMemory() (*cmem.Memory, bool) {
	res, err := p.cortex.Find(query.Query{Type: []cmem.Type{cmem.TypeCapability}, Limit: 64})
	if err != nil || res == nil {
		return nil, false
	}
	for _, item := range res.Memories {
		decoded, decodeErr := cmem.DecodeData(item.Version.Type, item.Version.Data)
		if decodeErr != nil {
			continue
		}
		data, ok := decoded.(cmem.CapabilityData)
		if ok && data.Subject == selfModelSubject && data.Capability == structuralSelfCapability {
			return item, true
		}
	}
	return nil, false
}

func cleanStrings(values []string) []string {
	out := make([]string, 0, len(values))
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		out = append(out, value)
	}
	return out
}
