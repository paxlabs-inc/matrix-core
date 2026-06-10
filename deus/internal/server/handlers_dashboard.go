package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/paxlabs-inc/deus/internal/auth"
	"github.com/paxlabs-inc/deus/internal/store"
	"github.com/paxlabs-inc/deus/pkg/types"
)

// mountDashboardRoutes serves the developer-dashboard read/control surface:
// account identity, caller spend, owner-scoped listings, earnings, and the
// per-service lifecycle/observability endpoints the marketplace consumes.
func (s *Server) mountDashboardRoutes(r chi.Router) {
	r.Route("/v1/me", func(r chi.Router) {
		r.Get("/", s.handleMe)
		r.Get("/spend", s.handleMySpend)
		r.Group(func(r chi.Router) {
			r.Use(DevDeveloperAuth(s.deps.DevMode))
			r.Get("/services", s.handleMyServices)
			r.Get("/earnings", s.handleMyEarnings)
		})
	})
}

// resolveServiceID maps a slug to its service uuid so every /{id} route accepts
// both forms (public pages link by slug). Unresolvable input passes through and
// fails downstream with the usual not-found.
func (s *Server) resolveServiceID(r *http.Request, idOrSlug string) string {
	if store.LooksLikeUUID(idOrSlug) || s.deps.Store == nil {
		return idOrSlug
	}
	row, err := s.deps.Store.GetServiceBySlug(r.Context(), idOrSlug)
	if err != nil {
		return idOrSlug
	}
	return row.ID
}

// requireServiceOwner loads the service and enforces that the authenticated
// developer wallet owns it.
func (s *Server) requireServiceOwner(w http.ResponseWriter, r *http.Request) (store.ServiceRow, bool) {
	if s.deps.Store == nil {
		writeAPIError(w, http.StatusServiceUnavailable, "internal_error", "store not configured", nil)
		return store.ServiceRow{}, false
	}
	idOrSlug := chi.URLParam(r, "id")
	row, err := s.deps.Store.GetServiceByIDOrSlug(r.Context(), idOrSlug)
	if err != nil {
		writeAPIError(w, http.StatusNotFound, "not_found", "service not found", nil)
		return store.ServiceRow{}, false
	}
	wallet := DeveloperWalletFromContext(r.Context())
	owner, err := s.deps.Store.DeveloperWalletByID(r.Context(), row.DeveloperID)
	if err != nil || !strings.EqualFold(owner, wallet) {
		writeAPIError(w, http.StatusForbidden, "forbidden", "not your service", nil)
		return store.ServiceRow{}, false
	}
	return row, true
}

func (s *Server) handleSetServiceStatus(status string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		row, ok := s.requireServiceOwner(w, r)
		if !ok {
			return
		}
		if err := s.deps.Store.UpdateServiceStatus(r.Context(), row.ID, status); err != nil {
			writeAPIError(w, http.StatusInternalServerError, "internal_error", err.Error(), nil)
			return
		}
		writeJSON(w, http.StatusOK, types.ServiceStatusResponse{ID: row.ID, Status: status})
	}
}

func (s *Server) handleServiceLogs(w http.ResponseWriter, r *http.Request) {
	row, ok := s.requireServiceOwner(w, r)
	if !ok {
		return
	}
	rows, err := s.deps.Store.RecentInvocationLogs(r.Context(), row.ID, 50)
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "internal_error", err.Error(), nil)
		return
	}
	logs := make([]types.LogLine, 0, len(rows))
	for _, l := range rows {
		level := "info"
		if l.Outcome != "ok" {
			level = "error"
		}
		latency := 0
		if l.LatencyMS != nil {
			latency = *l.LatencyMS
		}
		op := l.Operation
		if op == "" {
			op = "unknown"
		}
		logs = append(logs, types.LogLine{
			TS:    l.CreatedAt,
			Level: level,
			Message: fmt.Sprintf("invoke op=%s units=%s latency=%dms outcome=%s",
				op, l.Units, latency, l.Outcome),
		})
	}
	writeJSON(w, http.StatusOK, types.LogsResponse{Logs: logs})
}

func (s *Server) handleServiceAnalytics(w http.ResponseWriter, r *http.Request) {
	row, ok := s.requireServiceOwner(w, r)
	if !ok {
		return
	}
	ctx := r.Context()
	totals, err := s.deps.Store.ServiceAnalyticsTotals(ctx, row.ID)
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "internal_error", err.Error(), nil)
		return
	}
	series, err := s.deps.Store.ServiceAnalyticsSeries(ctx, row.ID, 30)
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "internal_error", err.Error(), nil)
		return
	}
	tops, err := s.deps.Store.ServiceTopOperations(ctx, row.ID, 10)
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "internal_error", err.Error(), nil)
		return
	}
	resp := types.ServiceAnalyticsResponse{
		ServiceID:        row.ID,
		TotalInvocations: totals.TotalInvocations,
		TotalRevenueWei:  totals.TotalRevenueWei,
		AvgLatencyMS:     totals.AvgLatencyMS,
		SuccessRate:      totals.SuccessRate,
		Series:           make([]types.AnalyticsPoint, 0, len(series)),
		TopOperations:    make([]types.TopOperation, 0, len(tops)),
	}
	if row.UptimeBPS != nil {
		resp.UptimeBPS = *row.UptimeBPS
	}
	for _, d := range series {
		resp.Series = append(resp.Series, types.AnalyticsPoint{
			Date:         d.Date,
			Invocations:  d.Invocations,
			RevenueWei:   d.RevenueWei,
			AvgLatencyMS: d.AvgLatencyMS,
			SuccessRate:  d.SuccessRate,
		})
	}
	for _, t := range tops {
		resp.TopOperations = append(resp.TopOperations, types.TopOperation{
			Operation:   t.Operation,
			Invocations: t.Invocations,
			RevenueWei:  t.RevenueWei,
		})
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) handleServicePayout(w http.ResponseWriter, r *http.Request) {
	row, ok := s.requireServiceOwner(w, r)
	if !ok {
		return
	}
	var body types.PayoutRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid_request", "invalid json body", nil)
		return
	}
	payout := strings.TrimSpace(body.PayoutAddress)
	if payout == "" {
		writeAPIError(w, http.StatusBadRequest, "invalid_request", "payout_address required", nil)
		return
	}
	if err := s.deps.Store.UpdateDeveloperPayoutAddress(r.Context(), row.DeveloperID, payout); err != nil {
		writeAPIError(w, http.StatusInternalServerError, "internal_error", err.Error(), nil)
		return
	}
	if s.deps.Settler == nil {
		writeAPIError(w, http.StatusServiceUnavailable, "internal_error", "settlement not configured", nil)
		return
	}
	res, err := s.deps.Settler.RunWindow(r.Context(), row.DeveloperID, payout)
	if err != nil {
		if strings.Contains(err.Error(), "nothing to settle") {
			writeAPIError(w, http.StatusConflict, "nothing_to_settle",
				"no unsettled invocations to pay out yet", nil)
			return
		}
		writeAPIError(w, http.StatusInternalServerError, "internal_error", err.Error(), nil)
		return
	}
	writeJSON(w, http.StatusOK, types.PayoutResponse{SettlementID: res.SettlementID})
}

func (s *Server) handleMe(w http.ResponseWriter, r *http.Request) {
	caller, callerErr := auth.ResolveRequest(r, s.deps.DevMode)
	devWallet := strings.TrimSpace(r.Header.Get("X-Developer-Wallet"))
	if devWallet == "" && s.deps.DevMode {
		devWallet = strings.TrimSpace(r.Header.Get("X-Developer-Address"))
	}
	if callerErr != nil && devWallet == "" {
		writeAPIError(w, http.StatusUnauthorized, "unauthorized", "caller or developer identity required", nil)
		return
	}
	resp := types.MeResponse{}
	if callerErr == nil {
		resp.DID = caller.DID
		resp.Wallet = caller.Wallet
	}
	if devWallet != "" {
		if resp.Wallet == "" {
			resp.Wallet = devWallet
		}
		if s.deps.Store != nil {
			if dev, err := s.deps.Store.DeveloperByWallet(r.Context(), devWallet); err == nil {
				resp.DisplayName = dev.DisplayName
			}
		}
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) handleMySpend(w http.ResponseWriter, r *http.Request) {
	caller, err := auth.ResolveRequest(r, s.deps.DevMode)
	if err != nil {
		writeAPIError(w, http.StatusUnauthorized, "unauthorized", "agent bearer required", nil)
		return
	}
	if s.deps.Store == nil {
		writeAPIError(w, http.StatusServiceUnavailable, "internal_error", "store not configured", nil)
		return
	}
	total, rows, err := s.deps.Store.SpendByCaller(r.Context(), caller.DID, caller.Wallet)
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "internal_error", err.Error(), nil)
		return
	}
	entries := make([]types.SpendEntry, 0, len(rows))
	for _, e := range rows {
		entries = append(entries, types.SpendEntry{
			ServiceID:   e.ServiceID,
			DisplayName: e.DisplayName,
			Invocations: e.Invocations,
			TotalWei:    e.TotalWei,
		})
	}
	writeJSON(w, http.StatusOK, types.SpendResponse{TotalSpentWei: total, Entries: entries})
}

func (s *Server) handleMyServices(w http.ResponseWriter, r *http.Request) {
	if s.deps.Store == nil {
		writeAPIError(w, http.StatusServiceUnavailable, "internal_error", "store not configured", nil)
		return
	}
	wallet := DeveloperWalletFromContext(r.Context())
	rows, err := s.deps.Store.ListServicesByDeveloperWallet(r.Context(), wallet)
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "internal_error", err.Error(), nil)
		return
	}
	services := make([]types.MyService, 0, len(rows))
	for _, row := range rows {
		item := types.MyService{
			ID:          row.ID,
			Slug:        row.Slug,
			DisplayName: row.DisplayName,
			Status:      row.Status,
			Kind:        row.Kind,
			Mode:        row.Mode,
			Invocations: row.Invocations,
			RevenueWei:  row.RevenueWei,
		}
		if row.UptimeBPS != nil {
			item.UptimeBPS = *row.UptimeBPS
		}
		if row.QualityScore != nil {
			item.QualityScore = *row.QualityScore
		}
		services = append(services, item)
	}
	writeJSON(w, http.StatusOK, types.MyServicesResponse{Services: services})
}

func (s *Server) handleMyEarnings(w http.ResponseWriter, r *http.Request) {
	if s.deps.Store == nil {
		writeAPIError(w, http.StatusServiceUnavailable, "internal_error", "store not configured", nil)
		return
	}
	ctx := r.Context()
	wallet := DeveloperWalletFromContext(ctx)
	dev, err := s.deps.Store.DeveloperByWallet(ctx, wallet)
	if err != nil {
		// A developer with no listings yet has no earnings — empty, not an error.
		writeJSON(w, http.StatusOK, types.EarningsResponse{
			TotalEarnedWei: "0", PendingWei: "0", AvailableWei: "0",
			Settlements: []types.SettlementSummary{},
		})
		return
	}
	totals, err := s.deps.Store.EarningsForDeveloper(ctx, dev.ID)
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "internal_error", err.Error(), nil)
		return
	}
	rows, err := s.deps.Store.ListSettlementsForDeveloper(ctx, dev.ID, 50)
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "internal_error", err.Error(), nil)
		return
	}
	settlements := make([]types.SettlementSummary, 0, len(rows))
	for _, row := range rows {
		item := types.SettlementSummary{
			ID:          row.ID,
			WindowStart: row.WindowStart,
			WindowEnd:   row.WindowEnd,
			AmountWei:   row.TotalWei,
			Status:      row.Status,
		}
		if row.TxHash != nil {
			item.TxHash = *row.TxHash
		}
		settlements = append(settlements, item)
	}
	writeJSON(w, http.StatusOK, types.EarningsResponse{
		TotalEarnedWei: totals.TotalEarnedWei,
		PendingWei:     totals.PendingWei,
		AvailableWei:   totals.SettledWei,
		PayoutAddress:  dev.PayoutAddress,
		Settlements:    settlements,
	})
}
