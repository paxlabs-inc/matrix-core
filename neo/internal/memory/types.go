// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package memory

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

type Snippet struct {
	Text string
	URI  string
	Type string
	Note string
}

type RecallHit struct {
	URI        string
	Type       string
	Text       string
	Score      float32
	ValidUntil *time.Time
	Note       string
}

type Outcome string

const (
	OutcomeSuccess Outcome = "success"
	OutcomeFailure Outcome = "failure"
	OutcomePartial Outcome = "partial"
)

type Relation string

const (
	RelationNew         Relation = "new"
	RelationReinforces  Relation = "reinforces"
	RelationUpdates     Relation = "updates"
	RelationContradicts Relation = "contradicts"
	RelationSupersedes  Relation = "supersedes"
	RelationRelates     Relation = "relates"
)

func ParseRelation(value string) Relation {
	relation := Relation(normalizeStatement(value))
	switch relation {
	case RelationReinforces, RelationUpdates, RelationContradicts, RelationSupersedes, RelationRelates:
		return relation
	default:
		return RelationNew
	}
}

type Embedder interface {
	Embed(string) ([]float32, error)
}

type Neighbor struct {
	URI  string
	Text string
	Type string
}

type RelationClassifier func(context.Context, string, []Neighbor) (Relation, string, string)

type PatternSpec struct {
	Name            string   `json:"name,omitempty"`
	Trigger         string   `json:"trigger,omitempty"`
	Preconditions   []string `json:"preconditions,omitempty"`
	Steps           []string `json:"steps,omitempty"`
	Gotchas         []string `json:"gotchas,omitempty"`
	SuccessCriteria []string `json:"success_criteria,omitempty"`
}

func (s PatternSpec) Encode() string {
	b, err := json.Marshal(s)
	if err != nil {
		return strings.TrimSpace(s.Name + " " + strings.Join(s.Steps, "; "))
	}
	return "neo.pattern.v2:" + string(b)
}

func DecodePatternSpec(statement string) PatternSpec {
	if raw, ok := strings.CutPrefix(strings.TrimSpace(statement), "neo.pattern.v2:"); ok {
		var spec PatternSpec
		if json.Unmarshal([]byte(raw), &spec) == nil {
			return spec
		}
	}
	return PatternSpec{Steps: []string{strings.TrimSpace(statement)}}
}

func (s PatternSpec) IsEmpty() bool {
	return strings.TrimSpace(s.Trigger+strings.Join(s.Steps, "")) == ""
}

type Pattern struct {
	Spec       PatternSpec
	Confidence float32
	Coverage   int
	URI        string
}

func (p Pattern) Render() string {
	parts := make([]string, 0, 5)
	if p.Spec.Trigger != "" {
		parts = append(parts, "when: "+p.Spec.Trigger)
	}
	if len(p.Spec.Steps) > 0 {
		parts = append(parts, "steps: "+strings.Join(p.Spec.Steps, " -> "))
	}
	if len(p.Spec.Gotchas) > 0 {
		parts = append(parts, "gotchas: "+strings.Join(p.Spec.Gotchas, "; "))
	}
	if len(p.Spec.SuccessCriteria) > 0 {
		parts = append(parts, "success: "+strings.Join(p.Spec.SuccessCriteria, "; "))
	}
	name := strings.TrimSpace(p.Spec.Name)
	if name == "" {
		name = "procedure"
	}
	return name + " · " + strings.Join(parts, " · ")
}

type OpportunityStatus string

const (
	OpportunityPending    OpportunityStatus = "pending"
	OpportunityScheduled  OpportunityStatus = "scheduled"
	OpportunityInProgress OpportunityStatus = "in_progress"
	OpportunityDone       OpportunityStatus = "done"
	OpportunityDismissed  OpportunityStatus = "dismissed"
)

func (s OpportunityStatus) Valid() bool {
	switch s {
	case OpportunityPending, OpportunityScheduled, OpportunityInProgress, OpportunityDone, OpportunityDismissed:
		return true
	}
	return false
}

func ParseOpportunityStatus(s string) (OpportunityStatus, bool) {
	status := OpportunityStatus(strings.ToLower(strings.TrimSpace(s)))
	return status, status.Valid()
}

type OpportunitySpec struct {
	Summary              string
	Rationale            string
	Status               OpportunityStatus
	EligibleAutonomous   bool
	Confidence           float32
	OriginConversationID string
	Attempts             int
	CreatedAt            time.Time
	UpdatedAt            time.Time
	URI                  string
}

type MutationOperation string

const (
	MutationCreate    MutationOperation = "create"
	MutationUpdate    MutationOperation = "update"
	MutationSupersede MutationOperation = "supersede"
	MutationDelete    MutationOperation = "delete"
)

type MutationTarget struct {
	URI   string   `json:"uri,omitempty"`
	Query string   `json:"query,omitempty"`
	Types []string `json:"types,omitempty"`
}

type MutationValue struct {
	Type      string  `json:"type,omitempty"`
	Content   string  `json:"content,omitempty"`
	Subject   string  `json:"subject,omitempty"`
	Predicate string  `json:"predicate,omitempty"`
	Polarity  string  `json:"polarity,omitempty"`
	Strength  float32 `json:"strength,omitempty"`
	Rationale string  `json:"rationale,omitempty"`
}

type MutationItem struct {
	Operation      MutationOperation `json:"operation"`
	Target         *MutationTarget   `json:"target,omitempty"`
	ReplacementURI string            `json:"replacement_uri,omitempty"`
	Value          *MutationValue    `json:"value,omitempty"`
	Reason         string            `json:"reason,omitempty"`
}

type MutationRequest struct {
	Items              []MutationItem `json:"items"`
	IncludeInternalIDs bool           `json:"include_internal_ids,omitempty"`
}

type MutationResult struct {
	Operation   MutationOperation `json:"operation"`
	Description string            `json:"description"`
	URI         string            `json:"uri,omitempty"`
}

type MutationBatchResult struct {
	Results []MutationResult `json:"results"`
}

type TimelineEntry struct {
	URI                string   `json:"uri"`
	Type               string   `json:"type"`
	Version            uint64   `json:"version"`
	CreatedAt          string   `json:"created_at,omitempty"`
	UpdatedAt          string   `json:"updated_at,omitempty"`
	CreatedBy          string   `json:"created_by,omitempty"`
	Confidence         float32  `json:"confidence,omitempty"`
	Salience           float32  `json:"salience,omitempty"`
	DeclaredImportance uint8    `json:"declared_importance,omitempty"`
	Tags               []string `json:"tags,omitempty"`
	FormShort          string   `json:"form_short,omitempty"`
	FormMedium         string   `json:"form_medium,omitempty"`
	Tombstoned         bool     `json:"tombstoned,omitempty"`
}

type TimelineTypeCount struct {
	Type  string `json:"type"`
	Count int    `json:"count"`
}

type TimelineQuery struct {
	Near  string
	Types []string
	AsOf  *time.Time
	Limit int
}

type MemoryConsentState struct {
	Enabled       bool   `json:"enabled"`
	Explicit      bool   `json:"explicit"`
	NoticeVersion string `json:"notice_version"`
	UpdatedAt     string `json:"updated_at,omitempty"`
	UpdatedBy     string `json:"updated_by,omitempty"`
	ExistingData  string `json:"existing_data"`
}

type MemoryExportItem struct {
	URI       string      `json:"uri"`
	Type      string      `json:"type"`
	CreatedAt string      `json:"created_at,omitempty"`
	UpdatedAt string      `json:"updated_at,omitempty"`
	Data      interface{} `json:"data"`
}

type MemoryExport struct {
	SchemaVersion int                `json:"schema_version"`
	ExportedAt    string             `json:"exported_at"`
	Consent       MemoryConsentState `json:"consent"`
	Items         []MemoryExportItem `json:"items"`
}

type MediaTaste struct {
	Liked    []string `json:"liked,omitempty"`
	Disliked []string `json:"disliked,omitempty"`
}
type MediaTastes struct {
	Music    MediaTaste `json:"music,omitempty"`
	Films    MediaTaste `json:"films,omitempty"`
	Shows    MediaTaste `json:"shows,omitempty"`
	Books    MediaTaste `json:"books,omitempty"`
	Games    MediaTaste `json:"games,omitempty"`
	Creators MediaTaste `json:"creators,omitempty"`
}
type BriefPreferences struct {
	Length   string   `json:"length,omitempty"`
	Sections []string `json:"sections,omitempty"`
}
type PersonalizationProfile struct {
	SchemaVersion    int              `json:"schema_version"`
	Interests        []string         `json:"interests,omitempty"`
	DayToDayGoals    []string         `json:"day_to_day_goals,omitempty"`
	Media            MediaTastes      `json:"media,omitempty"`
	Adventurousness  string           `json:"adventurousness,omitempty"`
	BriefPreferences BriefPreferences `json:"brief_preferences,omitempty"`
}

func (p PersonalizationProfile) Empty() bool {
	media := []MediaTaste{p.Media.Music, p.Media.Films, p.Media.Shows, p.Media.Books, p.Media.Games, p.Media.Creators}
	for _, taste := range media {
		if len(taste.Liked) > 0 || len(taste.Disliked) > 0 {
			return false
		}
	}
	return len(p.Interests) == 0 && len(p.DayToDayGoals) == 0 && p.Adventurousness == "" && p.BriefPreferences.Length == "" && len(p.BriefPreferences.Sections) == 0
}

func (p PersonalizationProfile) RenderForInterview() string {
	encoded, _ := json.MarshalIndent(p, "", "  ")
	return string(encoded)
}

type PersonalizationRecord struct {
	Profile   PersonalizationProfile `json:"profile"`
	URI       string                 `json:"uri,omitempty"`
	Version   uint64                 `json:"version,omitempty"`
	UpdatedAt string                 `json:"updated_at,omitempty"`
	CreatedBy string                 `json:"created_by,omitempty"`
	Exists    bool                   `json:"exists"`
}

type StructuralSelf struct {
	Summary      string        `json:"summary"`
	GraphURI     string        `json:"graph_uri,omitempty"`
	Scope        []string      `json:"scope,omitempty"`
	TokenBudget  int           `json:"token_budget,omitempty"`
	ContextLimit int           `json:"context_limit,omitempty"`
	Surface      *SurfaceFacts `json:"surface,omitempty"`
}
type SurfaceFacts struct {
	API   []string `json:"api,omitempty"`
	Is    []string `json:"is,omitempty"`
	IsNot []string `json:"is_not,omitempty"`
}
type FailurePattern struct {
	URI         string
	Statement   string
	DerivedFrom []string
	Scope       string
	Usefulness  string
	ConfirmedAt time.Time
	ExpiresAt   time.Time
}

type FailureLesson struct {
	Statement   string
	DerivedFrom []string
	Scope       string
	Usefulness  string
	Confirmed   bool
	ExpiresAt   time.Time
}
type SelfModel struct {
	Identity        string
	Structural      StructuralSelf
	StructuralURI   string
	FailurePatterns []FailurePattern
}

type DeathRecord struct {
	URI       string
	IntentID  string
	Summary   string
	CreatedAt time.Time
}
type EpisodicTimeWindow struct {
	From  time.Time
	Until time.Time
}
type EpisodicBudget struct {
	Tokens              int
	Hits                int
	Deadline            time.Duration
	Radius              int
	ExcludeConversation string
}
type EpisodicCurrentHit struct {
	Role           string
	Text           string
	SourceIdentity string
}
type EpisodicExcerpt struct {
	Ref             string
	ConversationID  string
	Date            time.Time
	SeqLo           uint64
	SeqHi           uint64
	Exact           bool
	Text            string
	RelatedMemories []Snippet
}

type TranscriptMessage struct {
	Seq      uint64
	Role     string
	ToolName string
	Content  string
	TS       int64
}

func EstimateTokens(s string) int { return (len(s) + 3) / 4 }

func normalizeStatement(s string) string {
	return strings.Join(strings.Fields(strings.ToLower(s)), " ")
}

func requiredText(label, value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", fmt.Errorf("%s is required", label)
	}
	return value, nil
}
