package lxp

import (
	"bytes"
	"context"
	"net/http"
)

// PriceFunc prices a request. ok=false means the route is free and passes
// through untouched. Returning an error yields a 500 without execution.
type PriceFunc func(r *http.Request) (Price, bool, error)

// Middleware wraps next behind an LXP paywall speaking the full lxp/1
// protocol:
//
//   - no X-LayerX-Payment -> 402 with terms (nonce prefetched for the caller's
//     X-Caller-DID; without that header, 402 reason identify_payer)
//   - invalid / mismatched / badly-signed payment -> fresh 402 (new nonce)
//     with a machine-readable reason — never execution
//   - exact mode: settle the full amount, then execute, receipt on the response
//   - hold mode: reserve -> execute buffered -> capture on 2xx (receipt on the
//     response) / release on anything else — no charge for failed executions
//   - layerxd unreachable -> 503 payment_unavailable, never a free call
func (s *Server) Middleware(price PriceFunc) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			p, priced, err := price(r)
			if err != nil {
				http.Error(w, "pricing failed", http.StatusInternalServerError)
				return
			}
			if !priced {
				next.ServeHTTP(w, r)
				return
			}
			mode := p.Mode
			if mode == "" {
				mode = ModeExact
			}

			header := r.Header.Get(HeaderPayment)
			if header == "" {
				s.challengeOr503(w, r.Context(), payerDID(r, Payment{}), p, ReasonPaymentRequired)
				return
			}
			pay, perr := ParsePayment(header)
			if perr != nil {
				s.challengeOr503(w, r.Context(), payerDID(r, Payment{}), p, ReasonInvalidPayment)
				return
			}
			if reason, verr := s.VerifyAgainstTerms(pay, p); verr != nil {
				s.challengeOr503(w, r.Context(), payerDID(r, pay), p, reason)
				return
			}

			switch mode {
			case ModeExact:
				receipt, serr := s.SettleExact(r.Context(), pay)
				if serr != nil {
					reason, unavailable := SettleReason(serr)
					if unavailable {
						WriteUnavailable(w)
						return
					}
					s.challengeOr503(w, r.Context(), payerDID(r, pay), p, reason)
					return
				}
				w.Header().Set(HeaderReceipt, EncodeReceipt(receipt))
				next.ServeHTTP(w, r)

			case ModeHold:
				hold, herr := s.OpenHold(r.Context(), pay, p.TTLSeconds)
				if herr != nil {
					reason, unavailable := SettleReason(herr)
					if unavailable {
						WriteUnavailable(w)
						return
					}
					s.challengeOr503(w, r.Context(), payerDID(r, pay), p, reason)
					return
				}
				// Execute buffered so a failed execution is released, not charged.
				rec := &bufferedResponse{header: http.Header{}, status: http.StatusOK}
				next.ServeHTTP(rec, r)
				if rec.status >= 200 && rec.status < 300 {
					receipt, cerr := s.Capture(r.Context(), hold.HoldID, pay.AmountUSDX)
					if cerr != nil {
						// The work is done but the capture failed — release so no
						// funds strand, then surface the rail failure.
						_ = s.Release(r.Context(), hold.HoldID)
						WriteUnavailable(w)
						return
					}
					rec.header.Set(HeaderReceipt, EncodeReceipt(receipt))
				} else {
					_ = s.Release(r.Context(), hold.HoldID)
				}
				rec.flush(w)
			}
		})
	}
}

// payerDID resolves whose DID to prefetch the fresh-challenge nonce for: the
// attempted payment's from_did when present, else the X-Caller-DID header.
func payerDID(r *http.Request, pay Payment) string {
	if pay.FromDID != "" {
		return pay.FromDID
	}
	return r.Header.Get(HeaderCallerDID)
}

// challengeOr503 answers with a fresh 402 challenge for did, identify_payer
// when no DID is known, or 503 when the nonce prefetch itself cannot reach
// layerxd.
func (s *Server) challengeOr503(w http.ResponseWriter, ctx context.Context, did string, p Price, reason string) {
	if !didRe.MatchString(did) {
		WriteIdentifyPayer(w)
		return
	}
	terms, err := s.Challenge(ctx, did, p)
	if err != nil {
		WriteUnavailable(w)
		return
	}
	WriteChallenge(w, terms, reason)
}

// bufferedResponse captures a handler's full response so hold-mode can decide
// capture-vs-release before a byte reaches the client.
type bufferedResponse struct {
	header http.Header
	status int
	body   bytes.Buffer
	wrote  bool
}

func (b *bufferedResponse) Header() http.Header { return b.header }

func (b *bufferedResponse) WriteHeader(status int) {
	if !b.wrote {
		b.status = status
		b.wrote = true
	}
}

func (b *bufferedResponse) Write(p []byte) (int, error) {
	b.wrote = true
	return b.body.Write(p)
}

func (b *bufferedResponse) flush(w http.ResponseWriter) {
	dst := w.Header()
	for k, vs := range b.header {
		dst[k] = vs
	}
	w.WriteHeader(b.status)
	_, _ = w.Write(b.body.Bytes())
}
