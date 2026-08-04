package admin

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

func (h *Handler) handleShards(w http.ResponseWriter, r *http.Request) {
	if r.URL.Query().Get("format") == "prometheus" {
		h.writeShardMetrics(w, r)
		return
	}
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	shards, err := h.DB.RailwayShards(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	capacity, err := h.DB.RailwayCapacity(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	type shardView struct {
		ID            string `json:"id"`
		State         string `json:"state"`
		Capacity      int    `json:"capacity"`
		Occupied      int    `json:"occupied"`
		Available     int    `json:"available"`
		RouterHealthy bool   `json:"router_healthy"`
	}
	occupied := make(map[string]int, len(capacity))
	for _, item := range capacity {
		occupied[item.ShardID] = item.Occupied
	}
	out := make([]shardView, 0, len(shards))
	for _, shard := range shards {
		used := occupied[shard.ID]
		out = append(out, shardView{
			ID: shard.ID, State: shard.State, Capacity: shard.Capacity,
			Occupied: used, Available: max(0, shard.Capacity-used),
			RouterHealthy: probeShardHealth(r.Context(), shard.RouterURL),
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"shards": out})
}

func (h *Handler) writeShardMetrics(w http.ResponseWriter, r *http.Request) {
	capacity, err := h.DB.RailwayCapacity(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	ops, err := h.DB.NonTerminalRailwayOperations(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	pending := map[string]int{}
	var oldest time.Duration
	for index := range ops {
		op := &ops[index]
		pending[op.ShardID]++
		age := time.Since(op.UpdatedAt)
		if age > oldest {
			oldest = age
		}
	}
	w.Header().Set("Content-Type", "text/plain; version=0.0.4")
	for _, item := range capacity {
		_, _ = fmt.Fprintf(w, "matrix_railway_shard_capacity{shard=%q} %d\n", item.ShardID, item.Capacity)
		_, _ = fmt.Fprintf(w, "matrix_railway_shard_occupied{shard=%q} %d\n", item.ShardID, item.Occupied)
		_, _ = fmt.Fprintf(w, "matrix_railway_reconcile_pending{shard=%q} %d\n", item.ShardID, pending[item.ShardID])
	}
	_, _ = fmt.Fprintf(w, "matrix_railway_oldest_reconcile_seconds %.0f\n", oldest.Seconds())
}

func (h *Handler) handleShard(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/admin/shards/")
	parts := strings.Split(strings.Trim(rest, "/"), "/")
	if len(parts) != 2 {
		http.Error(w, "expected /admin/shards/{id}/{state|reconcile}", http.StatusNotFound)
		return
	}
	shardID, action := parts[0], parts[1]
	switch action {
	case "state":
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var body struct {
			State string `json:"state"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&body); err != nil {
			http.Error(w, "invalid body", http.StatusBadRequest)
			return
		}
		switch body.State {
		case "active", "draining", "disabled", "unhealthy":
		default:
			http.Error(w, "invalid shard state", http.StatusBadRequest)
			return
		}
		if body.State == "active" {
			shard, err := h.DB.Shard(r.Context(), shardID)
			if err != nil {
				http.Error(w, err.Error(), http.StatusNotFound)
				return
			}
			if h.ShardProviders == nil {
				http.Error(w, "shard registry unavailable", http.StatusServiceUnavailable)
				return
			}
			if _, ok := h.ShardProviders.Provider(shardID); !ok {
				http.Error(w, "shard absent from secret registry", http.StatusServiceUnavailable)
				return
			}
			if !probeShardHealth(r.Context(), shard.RouterURL) {
				http.Error(w, "shard router health unproven", http.StatusServiceUnavailable)
				return
			}
		}
		if err := h.DB.SetShardState(r.Context(), shardID, body.State); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"id": shardID, "state": body.State})
	case "reconcile":
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		go func() {
			defer cancel()
			if err := h.ResumeRailwayOperations(ctx); err != nil {
				h.logf("manual shard reconcile %s: %v", shardID, err)
			}
		}()
		writeJSON(w, http.StatusAccepted, map[string]string{"id": shardID, "reconcile": "started"})
	default:
		http.Error(w, "unknown shard action", http.StatusNotFound)
	}
}

func probeShardHealth(ctx context.Context, rawURL string) bool {
	base, err := url.Parse(rawURL)
	if err != nil || base.Scheme != "https" || base.Host == "" {
		return false
	}
	base.Path = "/healthz"
	probeCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(probeCtx, http.MethodGet, base.String(), http.NoBody)
	if err != nil {
		return false
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	return resp.StatusCode >= 200 && resp.StatusCode < 300
}
