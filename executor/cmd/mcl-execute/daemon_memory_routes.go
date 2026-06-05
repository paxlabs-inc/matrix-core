// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package main

// daemon_memory_routes.go — /memory/* route surface (sess#27).
//
// Routes:
//
//   GET  /memory                    cortex.Find with query params
//   POST /memory/search             same as GET, body-driven for complex queries
//   GET  /memory/:uri               cortex.Resolve a specific URI (URL-encoded)
//   GET  /memory/types              distinct memory type counts
//   GET  /memory/recent             newest memories (paginated by created_at)
//   GET  /memory/salience/top       top-N highest-salience memories
//
// All routes proxy to bridge.Adapter when the daemon was booted with
// cortex enabled. When cortex is nil, every route returns 503.
//
// The frontend mounts these as the "shared ontology" surface — every
// matrix:// chip in the UI resolves through one of these endpoints.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"

	"matrix/cortex"
	"matrix/cortex/memory"
	"matrix/cortex/query"
)

// memorySummaryDTO is the wire-shape returned for a single memory.
type memorySummaryDTO struct {
	URI         string                 `json:"uri"`
	Type        string                 `json:"type"`
	Version     uint64                 `json:"version"`
	Hash        string                 `json:"hash,omitempty"`
	CreatedAt   string                 `json:"created_at,omitempty"`
	UpdatedAt   string                 `json:"updated_at,omitempty"`
	CreatedBy   string                 `json:"created_by,omitempty"`
	Confidence  float32                `json:"confidence,omitempty"`
	Salience    float64                `json:"salience,omitempty"`
	DeclaredImp uint8                  `json:"declared_importance,omitempty"`
	Tags        []string               `json:"tags,omitempty"`
	FormShort   string                 `json:"form_short,omitempty"`
	FormMedium  string                 `json:"form_medium,omitempty"`
	Tombstoned  bool                   `json:"tombstoned,omitempty"`
	Provenance  map[string]interface{} `json:"provenance,omitempty"`
}

// memoryDetailDTO is the full-shape returned for /memory/:uri.
type memoryDetailDTO struct {
	memorySummaryDTO
	Data       json.RawMessage `json:"data,omitempty"` // typed memory body
	FrameRefs  []frameRefDTO   `json:"frames,omitempty"`
	Forms      formsDTO        `json:"forms,omitempty"`
	Visibility string          `json:"visibility,omitempty"`
	ActorScope string          `json:"actor_scope,omitempty"`
}

type frameRefDTO struct {
	Verb    string `json:"verb"`
	ObjKind string `json:"obj_kind"`
	Ref     string `json:"ref,omitempty"`
}

type formsDTO struct {
	Short  string `json:"short,omitempty"`
	Medium string `json:"medium,omitempty"`
}

// handleMemoryFind serves GET /memory.
//
// Query params (all optional):
//
//	?type=<TypeName>       Identity|Fact|Preference|Belief|Event|Goal|Constraint|Capability|Pattern
//	?tag=<single-tag>
//	?near=<text>           NL phrase for vector recall (requires embedder)
//	?limit=<int>           default 20, cap 200
//	?form=<short|medium|full>  default medium
//	?include_tombstoned=<bool> default false
//	?cursor=<opaque>       pagination cursor
func (d *daemonState) handleMemoryFind(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	if _, ok := d.requireAuthPolicy(w, r, authAny); !ok {
		return
	}
	if d.infra == nil || d.infra.cortex == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "cortex not enabled"})
		return
	}
	cur, limit, ok := pageParams(w, r, 20, 200)
	if !ok {
		return
	}
	_ = cur // pagination is offset-based for cortex.Find

	q, err := buildMemoryQuery(r, limit, d)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	res, err := d.infra.cortex.Find(q)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{
			"error": "cortex.Find: " + err.Error(),
		})
		return
	}
	out := make([]memorySummaryDTO, 0, len(res.Memories))
	for i, m := range res.Memories {
		dto := memToSummary(m)
		if i < len(res.Rendered) {
			dto.FormMedium = res.Rendered[i]
		}
		out = append(out, dto)
	}
	writePaged(w, out, "", len(out))
}

// handleMemorySearch is the POST counterpart to handleMemoryFind so
// the frontend can express complex queries that don't fit in URL.
func (d *daemonState) handleMemorySearch(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	if _, ok := d.requireAuthPolicy(w, r, authAny); !ok {
		return
	}
	if d.infra == nil || d.infra.cortex == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "cortex not enabled"})
		return
	}
	var body struct {
		Type              []string `json:"type,omitempty"`
		Tag               string   `json:"tag,omitempty"`
		Near              string   `json:"near,omitempty"`
		NearURI           string   `json:"near_uri,omitempty"`
		Limit             int      `json:"limit,omitempty"`
		Form              string   `json:"form,omitempty"`
		IncludeTombstoned bool     `json:"include_tombstoned,omitempty"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "decode: " + err.Error()})
		return
	}
	if body.Limit <= 0 {
		body.Limit = 20
	}
	if body.Limit > 200 {
		body.Limit = 200
	}
	q := query.Query{
		Limit:             body.Limit,
		Form:              query.FormKind(strings.ToLower(body.Form)),
		Near:              body.Near,
		IncludeTombstoned: body.IncludeTombstoned,
	}
	if q.Form == "" {
		q.Form = query.FormMedium
	}
	for _, tname := range body.Type {
		if t := parseTypeNameDTO(tname); t != 0 {
			q.Type = append(q.Type, t)
		}
	}
	if body.NearURI != "" {
		uri := memory.URI(body.NearURI)
		q.NearURI = &uri
	}
	res, err := d.infra.cortex.Find(q)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{
			"error": "cortex.Find: " + err.Error(),
		})
		return
	}
	out := make([]memorySummaryDTO, 0, len(res.Memories))
	for i, m := range res.Memories {
		dto := memToSummary(m)
		if i < len(res.Rendered) {
			dto.FormMedium = res.Rendered[i]
		}
		out = append(out, dto)
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"items":             out,
		"trimmed_by_budget": res.TrimmedByBudget,
		"total":             res.Total,
		"total_estimate":    len(out),
	})
}

// handleMemoryResolve serves GET /memory/<url-encoded-uri>.
//
// The URI lives in the path component immediately after /memory/.
// Trailing path segments are accepted as-is; the URI is URL-decoded
// before passing to cortex.Resolve.
func (d *daemonState) handleMemoryResolve(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	if _, ok := d.requireAuthPolicy(w, r, authAny); !ok {
		return
	}
	if d.infra == nil || d.infra.cortex == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "cortex not enabled"})
		return
	}
	encoded := strings.TrimPrefix(r.URL.Path, "/memory/")
	encoded = strings.TrimSuffix(encoded, "/")
	if encoded == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "uri required"})
		return
	}
	uri, err := url.PathUnescape(encoded)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "uri decode: " + err.Error()})
		return
	}
	if !strings.HasPrefix(uri, "matrix://cortex/") {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "uri must start with matrix://cortex/",
		})
		return
	}
	mem, err := d.infra.cortex.Resolve(memory.URI(uri))
	if err != nil {
		if err == memory.ErrNotFound {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found"})
			return
		}
		writeJSON(w, http.StatusInternalServerError, map[string]string{
			"error": "cortex.Resolve: " + err.Error(),
		})
		return
	}
	writeJSON(w, http.StatusOK, memToDetail(mem))
}

// handleMemoryTypes serves GET /memory/types.
//
// Returns one entry per memory type with a count derived from
// cortex.ListByType. Counts are best-effort (an active embedder might
// be appending while we count); for an exact figure use /cortex/stats.
func (d *daemonState) handleMemoryTypes(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	if _, ok := d.requireAuthPolicy(w, r, authAny); !ok {
		return
	}
	if d.infra == nil || d.infra.cortex == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "cortex not enabled"})
		return
	}
	type typeCount struct {
		Type  string `json:"type"`
		Count int    `json:"count"`
	}
	out := make([]typeCount, 0, 9)
	for _, t := range allMemoryTypes() {
		ids, err := d.infra.cortex.ListByType(t, 0)
		if err != nil {
			continue
		}
		out = append(out, typeCount{
			Type:  t.String(),
			Count: len(ids),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Count > out[j].Count })
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"items": out,
	})
}

// handleMemoryRecent serves GET /memory/recent.
//
// Returns memories ordered by version.created_at descending across all
// types. Pagination by offset (limit only); cursor support reserved.
func (d *daemonState) handleMemoryRecent(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	if _, ok := d.requireAuthPolicy(w, r, authAny); !ok {
		return
	}
	if d.infra == nil || d.infra.cortex == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "cortex not enabled"})
		return
	}
	_, limit, ok := pageParams(w, r, 50, 200)
	if !ok {
		return
	}
	q := query.Query{
		Type:  allMemoryTypes(),
		Limit: limit,
		Form:  query.FormMedium,
		OrderBy: []query.OrderClause{
			{Field: query.OrderCreatedAt, Direction: query.OrderDesc},
		},
	}
	res, err := d.infra.cortex.Find(q)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{
			"error": "cortex.Find: " + err.Error(),
		})
		return
	}
	out := make([]memorySummaryDTO, 0, len(res.Memories))
	for i, m := range res.Memories {
		dto := memToSummary(m)
		if i < len(res.Rendered) {
			dto.FormMedium = res.Rendered[i]
		}
		out = append(out, dto)
	}
	writePaged(w, out, "", len(out))
}

// handleMemorySalienceTop serves GET /memory/salience/top.
func (d *daemonState) handleMemorySalienceTop(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	if _, ok := d.requireAuthPolicy(w, r, authAny); !ok {
		return
	}
	if d.infra == nil || d.infra.cortex == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "cortex not enabled"})
		return
	}
	_, limit, ok := pageParams(w, r, 20, 200)
	if !ok {
		return
	}
	q := query.Query{
		Type:  allMemoryTypes(),
		Limit: limit,
		Form:  query.FormMedium,
		OrderBy: []query.OrderClause{
			{Field: query.OrderSalience, Direction: query.OrderDesc},
		},
	}
	res, err := d.infra.cortex.Find(q)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{
			"error": "cortex.Find: " + err.Error(),
		})
		return
	}
	out := make([]memorySummaryDTO, 0, len(res.Memories))
	for i, m := range res.Memories {
		dto := memToSummary(m)
		if i < len(res.Rendered) {
			dto.FormMedium = res.Rendered[i]
		}
		out = append(out, dto)
	}
	writePaged(w, out, "", len(out))
}

// ---- helpers ----

// buildMemoryQuery translates URL params into a query.Query.
func buildMemoryQuery(r *http.Request, limit int, d *daemonState) (query.Query, error) {
	q := query.Query{
		Limit:             limit,
		Form:              query.FormMedium,
		IncludeTombstoned: queryBool(r, "include_tombstoned", false),
		Near:              queryString(r, "near", ""),
	}
	if t := parseTypeNameDTO(queryString(r, "type", "")); t != 0 {
		q.Type = append(q.Type, t)
	}
	if q.Near == "" && len(q.Type) == 0 {
		// Adjust to all-types so the engine doesn't reject the call;
		// the explicit "list everything" frontend query is handled
		// via /memory/recent which sets all types explicitly.
		q.Type = allMemoryTypes()
	}
	if form := queryString(r, "form", ""); form != "" {
		q.Form = query.FormKind(strings.ToLower(form))
	}
	return q, nil
}

// memToSummary converts memory.Memory to the summary DTO.
func memToSummary(m *memory.Memory) memorySummaryDTO {
	out := memorySummaryDTO{
		URI:         string(cortex.BuildURI(m.Head.Type, m.Head.ID, m.Head.CurrentVersion)),
		Type:        m.Head.Type.String(),
		Version:     m.Head.CurrentVersion,
		Hash:        hexFromHashArray(m.Version.Hash),
		Confidence:  m.Version.Confidence,
		DeclaredImp: m.Head.DeclaredImportance,
		FormShort:   m.Version.Forms.Short,
		FormMedium:  m.Version.Forms.Medium,
		CreatedBy:   m.Version.CreatedBy,
		Tombstoned:  m.Head.Tombstoned != nil,
	}
	if !m.Version.CreatedAt.IsZero() {
		out.CreatedAt = m.Version.CreatedAt.UTC().Format("2006-01-02T15:04:05.000000000Z")
	}
	if !m.Head.LastUpdatedAt.IsZero() {
		out.UpdatedAt = m.Head.LastUpdatedAt.UTC().Format("2006-01-02T15:04:05.000000000Z")
	}
	for _, t := range m.Head.Tags {
		out.Tags = append(out.Tags, string(t))
	}
	if m.Version.Provenance.Source != "" {
		out.Provenance = map[string]interface{}{
			"source": string(m.Version.Provenance.Source),
		}
	}
	return out
}

// memToDetail extends memToSummary with the typed Data body.
func memToDetail(m *memory.Memory) memoryDetailDTO {
	out := memoryDetailDTO{
		memorySummaryDTO: memToSummary(m),
		Data:             json.RawMessage(m.Version.Data),
		Forms: formsDTO{
			Short:  m.Version.Forms.Short,
			Medium: m.Version.Forms.Medium,
		},
		ActorScope: m.Head.ActorScope,
	}
	switch m.Head.Visibility {
	case memory.VisPrivate:
		out.Visibility = "private"
	case memory.VisScoped:
		out.Visibility = "scoped"
	case memory.VisActorPublic:
		out.Visibility = "actor_public"
	}
	for _, fr := range m.Head.Frames {
		out.FrameRefs = append(out.FrameRefs, frameRefDTO{
			Verb:    fmt.Sprintf("0x%02x", byte(fr.Verb)),
			ObjKind: fmt.Sprintf("0x%02x", byte(fr.ObjKind)),
			Ref:     fr.ObjRef,
		})
	}
	return out
}

// hexFromHashArray formats a 32-byte sha256 array as lowercase hex
// without reaching for encoding/hex (one allocation per call). Mirrors
// attest.go:hexFromRoot.
func hexFromHashArray(b [32]byte) string {
	const digits = "0123456789abcdef"
	out := make([]byte, 64)
	for i, x := range b {
		out[i*2] = digits[x>>4]
		out[i*2+1] = digits[x&0x0f]
	}
	return string(out)
}

// parseTypeNameDTO mirrors cortex.parseTypeName but is local so
// memory routes don't reach into private package symbols.
func parseTypeNameDTO(name string) memory.Type {
	switch strings.ToLower(name) {
	case "identity":
		return memory.TypeIdentity
	case "fact":
		return memory.TypeFact
	case "preference":
		return memory.TypePreference
	case "belief":
		return memory.TypeBelief
	case "event":
		return memory.TypeEvent
	case "goal":
		return memory.TypeGoal
	case "constraint":
		return memory.TypeConstraint
	case "capability":
		return memory.TypeCapability
	case "pattern":
		return memory.TypePattern
	}
	return 0
}

func allMemoryTypes() []memory.Type {
	return []memory.Type{
		memory.TypeIdentity,
		memory.TypeFact,
		memory.TypePreference,
		memory.TypeBelief,
		memory.TypeEvent,
		memory.TypeGoal,
		memory.TypeConstraint,
		memory.TypeCapability,
		memory.TypePattern,
	}
}

// Copyright © 2026 Paxlabs Inc. All rights reserved.
