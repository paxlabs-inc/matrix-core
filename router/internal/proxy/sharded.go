package proxy

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strconv"
	"strings"
	"time"

	"matrix/router/internal/db"
)

const (
	headerCentralUser     = "X-Matrix-Central-User"
	headerShardCredential = "X-Matrix-Shard-Credential"
	headerRequestID       = "X-Matrix-Request-ID"
	headerIssuedAt        = "X-Matrix-Issued-At"
	headerReplayID        = "X-Matrix-Replay-ID"
	headerKeyID           = "X-Matrix-Key-ID"
)

type CentralProxy struct {
	DB      *db.DB
	Resolve func(shardID string) (keyID string, key []byte, routerURL string, ok bool)
	Direct  http.Handler
}

func (h *CentralProxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	sub := Subject(r.Context())
	h.ForwardUser(w, r, sub)
}

func (h *CentralProxy) ForwardUser(w http.ResponseWriter, r *http.Request, sub string) {
	user, err := h.DB.LookupForRoute(r.Context(), sub)
	if err != nil {
		if h.Direct != nil {
			h.Direct.ServeHTTP(w, r)
			return
		}
		http.Error(w, "shard assignment unavailable", http.StatusServiceUnavailable)
		return
	}
	if user.Provider != "railway" {
		if h.Direct != nil {
			h.Direct.ServeHTTP(w, r)
			return
		}
		http.Error(w, "direct provider unavailable", http.StatusServiceUnavailable)
		return
	}
	if user.RailwayShardID == "" {
		http.Error(w, "shard assignment unavailable", http.StatusServiceUnavailable)
		return
	}
	shard, err := h.DB.Shard(r.Context(), user.RailwayShardID)
	if err != nil || (shard.State != "active" && shard.State != "draining") {
		http.Error(w, "assigned shard unavailable", http.StatusServiceUnavailable)
		return
	}
	keyID, key, rawURL, ok := h.Resolve(user.RailwayShardID)
	if !ok {
		http.Error(w, "assigned shard unavailable", http.StatusServiceUnavailable)
		return
	}
	target, err := url.Parse(rawURL)
	if err != nil {
		http.Error(w, "assigned shard endpoint invalid", http.StatusInternalServerError)
		return
	}
	requestID := r.Header.Get("X-Request-ID")
	if requestID == "" {
		requestID = randomID()
	}
	rp := httputil.NewSingleHostReverseProxy(target)
	rp.FlushInterval = -1
	rp.ModifyResponse = func(resp *http.Response) error {
		stripCORSResponseHeaders(resp.Header)
		return nil
	}
	director := rp.Director
	rp.Director = func(q *http.Request) {
		director(q)
		q.Host = target.Host
		q.Header.Set(headerShardCredential, string(key))
		q.Header.Set(headerKeyID, keyID)
		q.Header.Set(headerCentralUser, sub)
		q.Header.Set(headerRequestID, requestID)
		q.Header.Set(headerIssuedAt, strconv.FormatInt(time.Now().Unix(), 10))
		q.Header.Set(headerReplayID, randomID())
	}
	rp.ErrorHandler = func(w http.ResponseWriter, _ *http.Request, _ error) {
		http.Error(w, "assigned shard unreachable", http.StatusBadGateway)
	}
	rp.ServeHTTP(w, r)
}

func (h *CentralProxy) WakeHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
		if err != nil {
			http.Error(w, "wake body too large", http.StatusBadRequest)
			return
		}
		var req WakeRequest
		if err := json.Unmarshal(body, &req); err != nil || strings.TrimSpace(req.UserID) == "" {
			http.Error(w, "valid user_id required", http.StatusBadRequest)
			return
		}
		r.Body = io.NopCloser(bytes.NewReader(body))
		h.ForwardUser(w, r, strings.TrimSpace(req.UserID))
	})
}

type ShardIngress struct {
	DB      *db.DB
	ShardID string
	Keys    map[string][]byte
	Next    http.Handler
	Window  time.Duration
	Logf    func(string, ...any)
}

func (h *ShardIngress) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	window := h.Window
	if window <= 0 {
		window = 2 * time.Minute
	}
	expected, known := h.Keys[r.Header.Get(headerKeyID)]
	got := r.Header.Get(headerShardCredential)
	gotHash, expectedHash := sha256.Sum256([]byte(got)), sha256.Sum256(expected)
	if !known || subtle.ConstantTimeCompare(gotHash[:], expectedHash[:]) != 1 {
		h.reject("credential", r.Header.Get(headerKeyID), r)
		http.Error(w, "central authentication rejected", http.StatusUnauthorized)
		return
	}
	issued, err := strconv.ParseInt(r.Header.Get(headerIssuedAt), 10, 64)
	if err != nil || time.Since(time.Unix(issued, 0)) > window || time.Until(time.Unix(issued, 0)) > 30*time.Second {
		h.reject("timestamp", r.Header.Get(headerKeyID), r)
		http.Error(w, "central request expired", http.StatusUnauthorized)
		return
	}
	sub, replayID := r.Header.Get(headerCentralUser), r.Header.Get(headerReplayID)
	claimed, err := h.DB.ClaimIngressReplay(r.Context(), h.ShardID, replayID, time.Now().Add(window))
	if sub == "" || replayID == "" || err != nil || !claimed {
		h.reject("replay", r.Header.Get(headerKeyID), r)
		http.Error(w, "central replay rejected", http.StatusConflict)
		return
	}
	user, err := h.DB.LookupForRoute(r.Context(), sub)
	if err != nil || user.RailwayShardID != h.ShardID {
		h.reject("assignment", r.Header.Get(headerKeyID), r)
		http.Error(w, "cross-shard assignment rejected", http.StatusForbidden)
		return
	}
	requestID := r.Header.Get(headerRequestID)
	for _, name := range []string{headerShardCredential, headerKeyID, headerCentralUser, headerRequestID, headerIssuedAt, headerReplayID} {
		r.Header.Del(name)
	}
	if requestID != "" {
		r.Header.Set("X-Request-ID", requestID)
	}
	h.Next.ServeHTTP(w, r.WithContext(WithSubject(r.Context(), sub)))
}

func (h *ShardIngress) reject(class, keyID string, r *http.Request) {
	if h.Logf != nil {
		h.Logf("shard ingress rejected class=%s shard=%s key_id=%s request_id=%s", class, h.ShardID, keyID, r.Header.Get(headerRequestID))
	}
}

func randomID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic("crypto/rand unavailable")
	}
	return hex.EncodeToString(b[:])
}
