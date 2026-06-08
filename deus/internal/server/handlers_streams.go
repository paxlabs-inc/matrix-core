package server

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/paxlabs-inc/deus/internal/auth"
	"github.com/paxlabs-inc/deus/internal/streams"
	"github.com/paxlabs-inc/deus/pkg/types"
)

func (s *Server) mountStreamRoutes(r chi.Router) {
	r.Group(func(r chi.Router) {
		r.Use(auth.Middleware(s.deps.DevMode))
		r.Post("/v1/streams", s.handleOpenStream)
		r.Get("/v1/streams/{id}", s.handleGetStream)
		r.Post("/v1/streams/{id}/settle", s.handleSettleStream)
		r.Post("/v1/streams/{id}/close", s.handleCloseStream)
	})
}

func (s *Server) handleOpenStream(w http.ResponseWriter, r *http.Request) {
	if s.deps.Streams == nil {
		writeAPIError(w, http.StatusServiceUnavailable, "internal_error", "streams not configured", nil)
		return
	}
	caller, ok := auth.FromContext(r.Context())
	if !ok {
		writeAPIError(w, http.StatusUnauthorized, "unauthorized", "caller required", nil)
		return
	}
	var body types.OpenStreamRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid_request", "invalid json body", nil)
		return
	}
	res, err := s.deps.Streams.Open(r.Context(), caller, streams.OpenInput{
		ServiceID: body.ServiceID,
		Operation: body.Operation,
		CapWei:    body.CapWei,
		StopTime:  body.StopTime,
	})
	if err != nil {
		s.writeStreamsErr(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, types.OpenStreamResponse{
		StreamID:         res.StreamID,
		ChainStreamID:    res.ChainStreamID,
		ServiceID:        res.ServiceID,
		RatePerSecondWei: res.RatePerSecondWei,
		CapWei:           res.CapWei,
		Status:           res.Status,
		OpenTx:           res.OpenTx,
	})
}

func (s *Server) handleGetStream(w http.ResponseWriter, r *http.Request) {
	if s.deps.Streams == nil {
		writeAPIError(w, http.StatusServiceUnavailable, "internal_error", "streams not configured", nil)
		return
	}
	caller, ok := auth.FromContext(r.Context())
	if !ok {
		writeAPIError(w, http.StatusUnauthorized, "unauthorized", "caller required", nil)
		return
	}
	st, err := s.deps.Streams.Get(r.Context(), caller, chi.URLParam(r, "id"))
	if err != nil {
		s.writeStreamsErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, streamStateToTypes(st))
}

func (s *Server) handleSettleStream(w http.ResponseWriter, r *http.Request) {
	if s.deps.Streams == nil {
		writeAPIError(w, http.StatusServiceUnavailable, "internal_error", "streams not configured", nil)
		return
	}
	caller, ok := auth.FromContext(r.Context())
	if !ok {
		writeAPIError(w, http.StatusUnauthorized, "unauthorized", "caller required", nil)
		return
	}
	st, err := s.deps.Streams.Settle(r.Context(), caller, chi.URLParam(r, "id"))
	if err != nil {
		s.writeStreamsErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, streamStateToTypes(st))
}

func (s *Server) handleCloseStream(w http.ResponseWriter, r *http.Request) {
	if s.deps.Streams == nil {
		writeAPIError(w, http.StatusServiceUnavailable, "internal_error", "streams not configured", nil)
		return
	}
	caller, ok := auth.FromContext(r.Context())
	if !ok {
		writeAPIError(w, http.StatusUnauthorized, "unauthorized", "caller required", nil)
		return
	}
	st, refund, err := s.deps.Streams.Close(r.Context(), caller, chi.URLParam(r, "id"))
	if err != nil {
		s.writeStreamsErr(w, err)
		return
	}
	resp := streamStateToTypes(st)
	resp.RefundWei = refund
	writeJSON(w, http.StatusOK, resp)
}

func streamStateToTypes(st streams.State) types.StreamStateResponse {
	return types.StreamStateResponse{
		StreamID:         st.StreamID,
		ChainStreamID:    st.ChainStreamID,
		ServiceID:        st.ServiceID,
		RatePerSecondWei: st.RatePerSecondWei,
		CapWei:           st.CapWei,
		AccruedWei:       st.AccruedWei,
		SettledWei:       st.SettledWei,
		MeteredWei:       st.MeteredWei,
		Status:           st.Status,
		OpenTx:           st.OpenTx,
		LastSettleTx:     st.LastSettleTx,
		CloseTx:          st.CloseTx,
	}
}

func (s *Server) writeStreamsErr(w http.ResponseWriter, err error) {
	var se *streams.Error
	if errors.As(err, &se) {
		status := se.HTTPStatus
		if status == 0 {
			status = http.StatusBadRequest
		}
		writeAPIError(w, status, se.Code, se.Message, nil)
		return
	}
	writeAPIError(w, http.StatusInternalServerError, "internal_error", err.Error(), nil)
}
