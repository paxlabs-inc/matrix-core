// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package exa

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"
)

type ServiceConfig struct {
	Client          *Client
	Store           Store
	Now             func() time.Time
	DailySpendLimit float64
	MaxConcurrent   int
	SearchTTL       time.Duration
	ContentsTTL     time.Duration
	ResearchTTL     time.Duration
}

type Service struct {
	client        *Client
	store         Store
	now           func() time.Time
	dailyLimit    float64
	searchTTL     time.Duration
	contentsTTL   time.Duration
	researchTTL   time.Duration
	meter         *Meter
	mu            sync.Mutex
	activeByUser  map[string]int
	maxConcurrent int
	inflight      map[string]*flight
}

type flight struct {
	done chan struct{}
	data []byte
	err  error
}

type ResponseMeta struct {
	CacheHit    bool      `json:"cache_hit"`
	RetrievedAt time.Time `json:"retrieved_at"`
}

type SearchEnvelope struct {
	Data *SearchResponse `json:"data,omitempty"`
	Meta ResponseMeta    `json:"meta"`
}

type ContentsEnvelope struct {
	Data  *ContentsResponse `json:"data,omitempty"`
	Meta  ResponseMeta      `json:"meta"`
	Error *ErrorBody        `json:"error,omitempty"`
}

type RunEnvelope struct {
	Run      *AgentRun    `json:"run"`
	Workflow string       `json:"workflow,omitempty"`
	Subject  string       `json:"subject,omitempty"`
	Meta     ResponseMeta `json:"meta"`
}

func NewService(cfg ServiceConfig) *Service {
	now := cfg.Now
	if now == nil {
		now = time.Now
	}
	store := cfg.Store
	if store == nil {
		store = NewMemoryStore()
	}
	limit := cfg.DailySpendLimit
	if limit <= 0 {
		limit = 2
	}
	concurrent := cfg.MaxConcurrent
	if concurrent <= 0 {
		concurrent = 2
	}
	searchTTL, contentsTTL, researchTTL := cfg.SearchTTL, cfg.ContentsTTL, cfg.ResearchTTL
	if searchTTL <= 0 {
		searchTTL = 15 * time.Minute
	}
	if contentsTTL <= 0 {
		contentsTTL = time.Hour
	}
	if researchTTL <= 0 {
		researchTTL = 6 * time.Hour
	}
	client := cfg.Client
	if client == nil {
		client = NewClient(ClientConfig{APIKey: os.Getenv(APIKeyEnv), BaseURL: os.Getenv("EXA_API_URL")})
	}
	return &Service{client: client, store: store, now: now, dailyLimit: limit, searchTTL: searchTTL, contentsTTL: contentsTTL, researchTTL: researchTTL, meter: NewMeter(now()), activeByUser: map[string]int{}, maxConcurrent: concurrent, inflight: map[string]*flight{}}
}

func (s *Service) Stats() Stats { return s.meter.Snapshot() }

func (s *Service) Search(ctx context.Context, user string, input SearchRequest) (*SearchEnvelope, error) {
	if err := validateSearch(&input); err != nil {
		return nil, err
	}
	var response SearchResponse
	meta, err := s.cached(ctx, user, "search", input, s.searchTTL, .02, func() (any, float64, error) {
		out, callErr := s.client.Search(ctx, input)
		if out == nil {
			return nil, 0, callErr
		}
		return out, out.Cost.Total, callErr
	}, &response)
	if err != nil {
		return nil, err
	}
	return &SearchEnvelope{Data: &response, Meta: meta}, nil
}

func (s *Service) Contents(ctx context.Context, user string, input ContentsRequest) (*ContentsEnvelope, error) {
	if err := validateContents(&input); err != nil {
		return nil, err
	}
	var response ContentsResponse
	meta, err := s.cached(ctx, user, "contents", input, s.contentsTTL, .02, func() (any, float64, error) {
		out, callErr := s.client.Contents(ctx, input)
		if out == nil {
			return nil, 0, callErr
		}
		return out, out.Cost.Total, callErr
	}, &response)
	if err != nil {
		if KindOf(err) == FailurePartial && len(response.Statuses) > 0 {
			return &ContentsEnvelope{Data: &response, Meta: meta, Error: errorBody(err)}, nil
		}
		return nil, err
	}
	return &ContentsEnvelope{Data: &response, Meta: meta}, nil
}

func (s *Service) StartRun(ctx context.Context, user, workflow, subject string, input AgentRequest) (*RunEnvelope, error) {
	if err := validateAgent(&input); err != nil {
		return nil, err
	}
	cacheKey, err := hashRequest(workflow+"\x00"+subject, input)
	if err != nil {
		return nil, err
	}
	if cached, ok, storeErr := s.store.GetCache(ctx, user, cacheKey, s.now()); storeErr != nil {
		return nil, &Failure{Kind: FailureUpstream, Endpoint: "cache", Message: "Grounded research cache could not be read.", Detail: storeErr.Error()}
	} else if ok {
		var run AgentRun
		if json.Unmarshal(cached.Payload, &run) == nil {
			s.meter.Observe(MeterRecord{Endpoint: "agent/cache", User: user, CacheHit: true})
			return &RunEnvelope{Run: &run, Workflow: workflow, Subject: subject, Meta: ResponseMeta{CacheHit: true, RetrievedAt: s.now().UTC()}}, nil
		}
	}
	acquired, acquireErr := s.acquire(ctx, user)
	if acquireErr != nil {
		return nil, acquireErr
	}
	if !acquired {
		return nil, &Failure{Kind: FailureRateLimited, Endpoint: "agent/runs", Message: "Too many grounded research runs are active for this user."}
	}
	defer s.release(user)
	reserve := effortReserve(input.Effort)
	allowed, err := s.store.ReserveSpend(ctx, user, s.now(), reserve, s.dailyLimit)
	if err != nil {
		return nil, &Failure{Kind: FailureUpstream, Endpoint: "spend", Message: "Research spend could not be checked.", Detail: err.Error()}
	}
	if !allowed {
		return nil, &Failure{Kind: FailureRateLimited, Endpoint: "spend", Message: "This user's daily grounded research budget has been reached."}
	}
	started := s.now()
	run, callErr := s.client.CreateRun(ctx, input)
	s.meter.Observe(MeterRecord{Endpoint: "agent/start", User: user, Latency: s.now().Sub(started), Err: callErr})
	if callErr != nil {
		return nil, callErr
	}
	record := RunRecord{ID: run.ID, User: user, Workflow: workflow, Subject: subject, CacheKey: cacheKey, Status: run.Status, CreatedAt: s.now().UTC(), UpdatedAt: s.now().UTC()}
	if err := s.store.PutRun(ctx, record); err != nil {
		return nil, &Failure{Kind: FailureUpstream, Endpoint: "agent/runs", Message: "The research run could not be registered.", Detail: err.Error()}
	}
	return &RunEnvelope{Run: run, Workflow: workflow, Subject: subject, Meta: ResponseMeta{RetrievedAt: s.now().UTC()}}, nil
}

func (s *Service) GetRun(ctx context.Context, user, id string) (*RunEnvelope, error) {
	record, ok, err := s.store.GetRun(ctx, user, strings.TrimSpace(id))
	if err != nil {
		return nil, &Failure{Kind: FailureUpstream, Endpoint: "agent/runs", Message: "The research run could not be authorized.", Detail: err.Error()}
	}
	if !ok {
		return nil, &Failure{Kind: FailureNotFound, Endpoint: "agent/runs", Message: "That research run does not exist for this user."}
	}
	started := s.now()
	run, callErr := s.client.GetRun(ctx, id)
	cost := 0.0
	if run != nil && run.Cost.Total > record.Cost {
		cost = run.Cost.Total - record.Cost
	}
	s.meter.Observe(MeterRecord{Endpoint: "agent/get", User: user, Latency: s.now().Sub(started), Cost: cost, Err: callErr})
	if callErr != nil && !(KindOf(callErr) == FailureUngrounded && run != nil) {
		return nil, callErr
	}
	if run != nil {
		record.Status, record.UpdatedAt = run.Status, s.now().UTC()
		if run.Cost.Total > record.Cost {
			record.Cost = run.Cost.Total
		}
		_ = s.store.PutRun(ctx, record)
		if run.Status == "completed" && callErr == nil {
			payload, _ := json.Marshal(run)
			_ = s.store.PutCache(ctx, CacheRecord{Key: record.CacheKey, User: user, Payload: payload, Cost: run.Cost.Total, ExpiresAt: s.now().Add(s.researchTTL)})
		}
	}
	if callErr != nil {
		return &RunEnvelope{Run: run, Workflow: record.Workflow, Subject: record.Subject, Meta: ResponseMeta{RetrievedAt: s.now().UTC()}}, callErr
	}
	return &RunEnvelope{Run: run, Workflow: record.Workflow, Subject: record.Subject, Meta: ResponseMeta{RetrievedAt: s.now().UTC()}}, nil
}

func (s *Service) ContinueRun(ctx context.Context, user, id, query, effort string) (*RunEnvelope, error) {
	record, ok, err := s.store.GetRun(ctx, user, id)
	if err != nil || !ok {
		return nil, &Failure{Kind: FailureNotFound, Endpoint: "agent/runs", Message: "That previous research run does not exist for this user."}
	}
	return s.StartRun(ctx, user, record.Workflow+".continue", record.Subject, AgentRequest{Query: query, Effort: effort, PreviousRunID: id})
}

func (s *Service) CancelRun(ctx context.Context, user, id string) (*RunEnvelope, error) {
	record, ok, err := s.store.GetRun(ctx, user, id)
	if err != nil || !ok {
		return nil, &Failure{Kind: FailureNotFound, Endpoint: "agent/runs", Message: "That research run does not exist for this user."}
	}
	run, callErr := s.client.CancelRun(ctx, id)
	s.meter.Observe(MeterRecord{Endpoint: "agent/cancel", User: user, Err: callErr})
	if callErr != nil {
		return nil, callErr
	}
	record.Status, record.UpdatedAt = run.Status, s.now().UTC()
	_ = s.store.PutRun(ctx, record)
	return &RunEnvelope{Run: run, Workflow: record.Workflow, Subject: record.Subject, Meta: ResponseMeta{RetrievedAt: s.now().UTC()}}, nil
}

func (s *Service) cached(ctx context.Context, user, endpoint string, request any, ttl time.Duration, reserve float64, load func() (any, float64, error), output any) (ResponseMeta, error) {
	key, err := hashRequest(endpoint, request)
	if err != nil {
		return ResponseMeta{}, err
	}
	if record, ok, storeErr := s.store.GetCache(ctx, user, key, s.now()); storeErr != nil {
		return ResponseMeta{}, &Failure{Kind: FailureUpstream, Endpoint: "cache", Message: "Grounded research cache could not be read.", Detail: storeErr.Error()}
	} else if ok && json.Unmarshal(record.Payload, output) == nil {
		s.meter.Observe(MeterRecord{Endpoint: endpoint, User: user, CacheHit: true})
		return ResponseMeta{CacheHit: true, RetrievedAt: s.now().UTC()}, nil
	}

	s.mu.Lock()
	if existing := s.inflight[user+"\x00"+key]; existing != nil {
		s.mu.Unlock()
		select {
		case <-ctx.Done():
			return ResponseMeta{}, ctx.Err()
		case <-existing.done:
		}
		if existing.err != nil {
			return ResponseMeta{}, existing.err
		}
		if err := json.Unmarshal(existing.data, output); err != nil {
			return ResponseMeta{}, err
		}
		s.meter.Observe(MeterRecord{Endpoint: endpoint, User: user, CacheHit: true})
		return ResponseMeta{CacheHit: true, RetrievedAt: s.now().UTC()}, nil
	}
	f := &flight{done: make(chan struct{})}
	s.inflight[user+"\x00"+key] = f
	s.mu.Unlock()
	defer func() { s.mu.Lock(); delete(s.inflight, user+"\x00"+key); close(f.done); s.mu.Unlock() }()

	allowed, spendErr := s.store.ReserveSpend(ctx, user, s.now(), reserve, s.dailyLimit)
	if spendErr != nil {
		f.err = spendErr
		return ResponseMeta{}, spendErr
	}
	if !allowed {
		f.err = &Failure{Kind: FailureRateLimited, Endpoint: "spend", Message: "This user's daily grounded research budget has been reached."}
		return ResponseMeta{}, f.err
	}
	started := s.now()
	value, cost, callErr := load()
	s.meter.Observe(MeterRecord{Endpoint: endpoint, User: user, Latency: s.now().Sub(started), Cost: cost, Err: callErr})
	if value != nil {
		data, marshalErr := json.Marshal(value)
		if marshalErr == nil {
			f.data = data
			_ = json.Unmarshal(data, output)
			if callErr == nil {
				_ = s.store.PutCache(ctx, CacheRecord{Key: key, User: user, Payload: data, Cost: cost, ExpiresAt: s.now().Add(ttl)})
			}
		}
	}
	f.err = callErr
	return ResponseMeta{RetrievedAt: s.now().UTC()}, callErr
}

func (s *Service) acquire(ctx context.Context, user string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	active, err := s.store.ActiveRuns(ctx, user)
	if err != nil {
		return false, &Failure{Kind: FailureUpstream, Endpoint: "agent/runs", Message: "Active research runs could not be checked.", Detail: err.Error()}
	}
	if active+s.activeByUser[user] >= s.maxConcurrent {
		return false, nil
	}
	s.activeByUser[user]++
	return true, nil
}
func (s *Service) release(user string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.activeByUser[user] > 0 {
		s.activeByUser[user]--
	}
}

func effortReserve(effort string) float64 {
	switch effort {
	case "minimal":
		return .02
	case "low":
		return .04
	case "high":
		return .60
	case "xhigh":
		return 1.10
	default:
		return .20
	}
}

func hashRequest(prefix string, value any) (string, error) {
	data, err := json.Marshal(value)
	if err != nil {
		return "", badRequest(prefix, "That request could not be normalized.")
	}
	sum := sha256.Sum256(append([]byte(prefix+"\n"), data...))
	return hex.EncodeToString(sum[:]), nil
}

func validateSearch(input *SearchRequest) error {
	input.Query = strings.TrimSpace(input.Query)
	if input.Query == "" || len(input.Query) > 4000 {
		return badRequest("search", "The search query must contain between 1 and 4000 characters.")
	}
	if input.NumResults <= 0 {
		input.NumResults = 8
	}
	if input.NumResults > 20 {
		return badRequest("search", "A search may return at most 20 results.")
	}
	allowed := map[string]bool{"": true, "auto": true, "fast": true, "instant": true, "deep-lite": true, "deep": true, "deep-reasoning": true}
	if !allowed[input.Type] {
		return badRequest("search", "That search type is not supported.")
	}
	if data, _ := json.Marshal(input.OutputSchema); len(data) > 32768 {
		return badRequest("search", "The output schema is too large.")
	}
	return nil
}

func validateContents(input *ContentsRequest) error {
	if len(input.URLs) == 0 || len(input.URLs) > 10 {
		return badRequest("contents", "Contents requests require between 1 and 10 URLs.")
	}
	for _, raw := range input.URLs {
		if canonicalURL(raw) == "" {
			return badRequest("contents", "Every contents URL must be an HTTP or HTTPS URL.")
		}
	}
	if input.LivecrawlTimeout < 0 || input.LivecrawlTimeout > 15000 {
		return badRequest("contents", "Live crawl timeout may not exceed 15000 milliseconds.")
	}
	if input.Subpages < 0 || input.Subpages > 10 {
		return badRequest("contents", "At most 10 subpages may be retrieved per URL.")
	}
	return nil
}

func validateAgent(input *AgentRequest) error {
	input.Query = strings.TrimSpace(input.Query)
	if input.Query == "" || len(input.Query) > 8000 {
		return badRequest("agent/runs", "The research query must contain between 1 and 8000 characters.")
	}
	allowed := map[string]bool{"": true, "minimal": true, "low": true, "medium": true, "high": true, "xhigh": true}
	if !allowed[input.Effort] {
		return badRequest("agent/runs", "That research effort is not allowed.")
	}
	if input.Effort == "" {
		input.Effort = "medium"
	}
	if data, _ := json.Marshal(input.OutputSchema); len(data) > 65536 {
		return badRequest("agent/runs", "The research output schema is too large.")
	}
	return nil
}

func serviceError(err error, endpoint string) error {
	if err == nil {
		return nil
	}
	if FailureOf(err) != nil {
		return err
	}
	return &Failure{Kind: FailureUpstream, Endpoint: endpoint, Message: "Grounded research could not be completed.", Detail: fmt.Sprint(err)}
}
