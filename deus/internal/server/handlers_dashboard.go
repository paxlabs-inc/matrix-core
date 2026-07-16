package server

import (
	"fmt"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/paxlabs-inc/deus/internal/auth"
	"github.com/paxlabs-inc/deus/internal/store"
	"github.com/paxlabs-inc/deus/pkg/pricingmath"
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
			r.Use(s.requireDeveloperAuth())
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

// requireServiceOwner loads the service and enforces account or legacy-wallet ownership.
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
	principal := DeveloperPrincipalFromContext(r.Context())
	dev, err := s.developerForPrincipal(r.Context(), principal)
	if err != nil || dev.ID != row.DeveloperID {
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
		TotalRevenueUSDX: totals.TotalRevenueUSDX,
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
			RevenueUSDX:  d.RevenueUSDX,
			AvgLatencyMS: d.AvgLatencyMS,
			SuccessRate:  d.SuccessRate,
		})
	}
	for _, t := range tops {
		resp.TopOperations = append(resp.TopOperations, types.TopOperation{
			Operation:   t.Operation,
			Invocations: t.Invocations,
			RevenueWei:  t.RevenueWei,
			RevenueUSDX: t.RevenueUSDX,
		})
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) handleMe(w http.ResponseWriter, r *http.Request) {
	caller, callerErr := auth.ResolveRequest(r, s.deps.DevMode)
	principal, devErr := s.resolveDeveloperPrincipal(r)
	if callerErr != nil && devErr != nil {
		writeAPIError(w, http.StatusUnauthorized, "unauthorized", "caller or developer identity required", nil)
		return
	}
	resp := types.MeResponse{}
	if callerErr == nil {
		resp.DID = caller.DID
		resp.Wallet = caller.Wallet
	}
	if devErr == nil {
		if principal.Kind == DeveloperPrincipalWallet && resp.Wallet == "" {
			resp.Wallet = principal.Subject
		}
		if principal.Kind == DeveloperPrincipalAccount {
			resp.DID = principal.Owner
		}
		resp.DisplayName = principal.DisplayName
		if dev, err := s.developerForPrincipal(r.Context(), principal); err == nil && dev.DisplayName != "" {
			resp.DisplayName = dev.DisplayName
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
	totalWei, totalUSDX, rows, err := s.deps.Store.SpendByCaller(r.Context(), caller.DID, caller.Wallet)
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
			TotalUSDX:   e.TotalUSDX,
		})
	}
	writeJSON(w, http.StatusOK, types.SpendResponse{TotalSpentWei: totalWei, TotalSpentUSDX: totalUSDX, Entries: entries})
}

func (s *Server) handleMyServices(w http.ResponseWriter, r *http.Request) {
	if s.deps.Store == nil {
		writeAPIError(w, http.StatusServiceUnavailable, "internal_error", "store not configured", nil)
		return
	}
	principal := DeveloperPrincipalFromContext(r.Context())
	dev, err := s.developerForPrincipal(r.Context(), principal)
	if err != nil {
		writeJSON(w, http.StatusOK, types.MyServicesResponse{Services: []types.MyService{}})
		return
	}
	rows, err := s.deps.Store.ListServicesByDeveloperID(r.Context(), dev.ID)
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
			RevenueUSDX: row.RevenueUSDX,
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
	principal := DeveloperPrincipalFromContext(ctx)
	dev, err := s.developerForPrincipal(ctx, principal)
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
		LayerX:         s.layerxEarnings(r, dev),
	})
}

// layerxEarnings assembles the LXP-rail earnings block: deus invocation
// aggregates joined with a live LayerX account read for the developer's payee
// DID, plus the withdraw link-out (deus has no payout code — earnings already
// sit in the payee's LayerX account). Nil when the LayerX rail is off; the
// account read is best-effort so a layerxd outage never breaks the dashboard.
func (s *Server) layerxEarnings(r *http.Request, dev store.DeveloperRow) *types.LayerXEarnings {
	if s.deps.Gateway == nil || !s.deps.Gateway.LXPEnabled() {
		return nil
	}
	ctx := r.Context()
	lx := s.deps.Gateway.LXP()
	block := &types.LayerXEarnings{
		PayeeDID:    dev.PayeeDID,
		EarnedUSDX:  "0.000000",
		LayerXURL:   lx.LayerXURL(),
		WithdrawURL: lx.LayerXURL() + "/v1/withdraw",
	}
	if totals, err := s.deps.Store.LayerXEarningsForDeveloper(ctx, dev.ID); err == nil {
		if micro, perr := pricingmath.ParseUSDX(totals.EarnedUSDX); perr == nil {
			block.EarnedUSDX = pricingmath.FormatUSDX(micro)
		}
		block.Invocations = totals.Invocations
	}
	if dev.PayeeDID != "" {
		if acct, err := lx.Client().Account(ctx, dev.PayeeDID); err == nil {
			block.BalanceUSDX = acct.BalanceUSDX
			block.EscrowUSDX = acct.EscrowUSDX
		}
	}
	return block
}
