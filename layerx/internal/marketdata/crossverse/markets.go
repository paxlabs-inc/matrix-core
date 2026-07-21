package crossverse

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
)

type MarketsRecord struct {
	Symbol                    string
	PriceCents                int64
	PerpFundingPpb            int64
	EstimatedFundingPpb       int64
	PerpNextFundingAtMs       int64
	PerpOIContracts           int64
	PerpOIMicroUSDX           int64
	PerpVolume24hContracts    int64
	PerpVolume24hMicroUSDX    int64
	PerpMarkPriceCents        int64
	PerpIndexPriceCents       int64
	PerpBasisPpb              int64
	PerpLongShortRatioPpb     int64
	PerpLiqVolume24hMicroUSDX int64
	SourceTimestampMs         int64
	ReceivedTimestampMs       int64
}

type marketRecordPayload struct {
	Symbol                 string      `json:"symbol"`
	Price                  json.Number `json:"price"`
	EstFundingRate         json.Number `json:"est_funding_rate"`
	PerpFundingRate        json.Number `json:"perp_funding_rate"`
	PerpNextFundingMs      json.Number `json:"perp_next_funding_ms"`
	PerpOIContracts        json.Number `json:"perp_oi_contracts"`
	PerpOIUSD              json.Number `json:"perp_oi_usd"`
	PerpVolume24hContracts json.Number `json:"perp_volume_24h_contracts"`
	PerpVolume24hUSD       json.Number `json:"perp_volume_24h_usd"`
	PerpMarkPrice          json.Number `json:"perp_mark_price"`
	PerpIndexPrice         json.Number `json:"perp_index_price"`
	PerpBasisBps           json.Number `json:"perp_basis_bps"`
	PerpLongShortRatio     json.Number `json:"perp_long_short_ratio"`
	PerpLiqVolume24h       json.Number `json:"perp_liq_volume_24h"`
}

type marketsListPayload struct {
	TS      json.Number           `json:"ts"`
	Markets []marketRecordPayload `json:"markets"`
}

type marketsState struct {
	mu         sync.Mutex
	snapshotID string
	all        topicState
	totals     topicState
	records    map[string]MarketsRecord
}

func parseMarketRecord(p marketRecordPayload, sourceTs, receivedMs int64) (MarketsRecord, error) {
	symbol := strings.ToUpper(strings.TrimSpace(p.Symbol))
	if symbol == "" {
		return MarketsRecord{}, fmt.Errorf("crossverse markets record has no symbol")
	}
	rec := MarketsRecord{Symbol: symbol, SourceTimestampMs: sourceTs, ReceivedTimestampMs: receivedMs}
	var err error
	if p.Price.String() != "" {
		if rec.PriceCents, err = ParsePriceCents(p.Price.String()); err != nil {
			return MarketsRecord{}, fmt.Errorf("crossverse markets %s: price: %w", symbol, err)
		}
	}
	if p.PerpFundingRate.String() != "" {
		if rec.PerpFundingPpb, err = ParseSignedPpb(p.PerpFundingRate.String()); err != nil {
			return MarketsRecord{}, fmt.Errorf("crossverse markets %s: perp funding rate: %w", symbol, err)
		}
	}
	if p.EstFundingRate.String() != "" {
		if rec.EstimatedFundingPpb, err = ParseSignedPpb(p.EstFundingRate.String()); err != nil {
			return MarketsRecord{}, fmt.Errorf("crossverse markets %s: est funding rate: %w", symbol, err)
		}
	}
	if p.PerpNextFundingMs.String() != "" {
		if rec.PerpNextFundingAtMs, err = NextFundingAtMs(p.PerpNextFundingMs.String(), sourceTs); err != nil {
			return MarketsRecord{}, fmt.Errorf("crossverse markets %s: next funding: %w", symbol, err)
		}
	}
	if p.PerpOIContracts.String() != "" {
		if rec.PerpOIContracts, err = ParseScaled(p.PerpOIContracts.String(), 1, RoundTrunc); err != nil || rec.PerpOIContracts < 0 {
			return MarketsRecord{}, fmt.Errorf("crossverse markets %s: oi contracts %q is invalid", symbol, p.PerpOIContracts.String())
		}
	}
	if p.PerpOIUSD.String() != "" {
		if rec.PerpOIMicroUSDX, err = ParseMicroUSDX(p.PerpOIUSD.String()); err != nil {
			return MarketsRecord{}, fmt.Errorf("crossverse markets %s: oi usd: %w", symbol, err)
		}
	}
	if p.PerpVolume24hContracts.String() != "" {
		if rec.PerpVolume24hContracts, err = ParseScaled(p.PerpVolume24hContracts.String(), 1, RoundTrunc); err != nil || rec.PerpVolume24hContracts < 0 {
			return MarketsRecord{}, fmt.Errorf("crossverse markets %s: volume contracts %q is invalid", symbol, p.PerpVolume24hContracts.String())
		}
	}
	if p.PerpVolume24hUSD.String() != "" {
		if rec.PerpVolume24hMicroUSDX, err = ParseMicroUSDX(p.PerpVolume24hUSD.String()); err != nil {
			return MarketsRecord{}, fmt.Errorf("crossverse markets %s: volume usd: %w", symbol, err)
		}
	}
	if p.PerpMarkPrice.String() != "" {
		if rec.PerpMarkPriceCents, err = ParsePriceCents(p.PerpMarkPrice.String()); err != nil {
			return MarketsRecord{}, fmt.Errorf("crossverse markets %s: mark price: %w", symbol, err)
		}
	}
	if p.PerpIndexPrice.String() != "" {
		if rec.PerpIndexPriceCents, err = ParsePriceCents(p.PerpIndexPrice.String()); err != nil {
			return MarketsRecord{}, fmt.Errorf("crossverse markets %s: index price: %w", symbol, err)
		}
	}
	if p.PerpBasisBps.String() != "" {
		if rec.PerpBasisPpb, err = ParseBpsToPpb(p.PerpBasisBps.String()); err != nil {
			return MarketsRecord{}, fmt.Errorf("crossverse markets %s: basis bps: %w", symbol, err)
		}
	}
	if p.PerpLongShortRatio.String() != "" {
		if rec.PerpLongShortRatioPpb, err = ParseSignedPpb(p.PerpLongShortRatio.String()); err != nil {
			return MarketsRecord{}, fmt.Errorf("crossverse markets %s: long short ratio: %w", symbol, err)
		}
	}
	if p.PerpLiqVolume24h.String() != "" {
		if rec.PerpLiqVolume24hMicroUSDX, err = ParseMicroUSDX(p.PerpLiqVolume24h.String()); err != nil {
			return MarketsRecord{}, fmt.Errorf("crossverse markets %s: liq volume: %w", symbol, err)
		}
	}
	return rec, nil
}

// applyMarketsList accepts both the REST wrapper {ts, markets:[...]} and the
// bare record array the deployed markets@all frames carry in data.
func (m *Manager) applyMarketsList(data []byte, sourceTs, receivedMs int64) error {
	var rows []marketRecordPayload
	trimmed := strings.TrimSpace(string(data))
	if strings.HasPrefix(trimmed, "[") {
		if err := decodeNumbers(data, &rows); err != nil {
			return fmt.Errorf("crossverse markets: invalid markets payload: %w", err)
		}
	} else {
		var p marketsListPayload
		if err := decodeNumbers(data, &p); err != nil {
			return fmt.Errorf("crossverse markets: invalid markets payload: %w", err)
		}
		rows = p.Markets
		if p.TS.String() != "" {
			if ts, err := ParseTimestampMs(p.TS.String()); err == nil && ts > 0 {
				sourceTs = ts
			}
		}
	}
	recs := make(map[string]MarketsRecord, len(rows))
	for _, raw := range rows {
		rec, err := parseMarketRecord(raw, sourceTs, receivedMs)
		if err != nil {
			return err
		}
		recs[rec.Symbol] = rec
	}
	m.markets.mu.Lock()
	if m.markets.records == nil {
		m.markets.records = make(map[string]MarketsRecord, len(recs))
	}
	for sym, rec := range recs {
		m.markets.records[sym] = rec
	}
	m.markets.mu.Unlock()
	m.aggregateReceivedMs.Store(receivedMs)
	return nil
}

func (m *Manager) marketsDisconnected() {
	m.markets.mu.Lock()
	m.markets.all = topicState{}
	m.markets.totals = topicState{}
	m.markets.mu.Unlock()
}

func (m *Manager) handleAggregateMessage(raw []byte) error {
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.UseNumber()
	var fr frame
	if err := dec.Decode(&fr); err != nil {
		return fmt.Errorf("crossverse aggregate: invalid frame: %w", err)
	}
	if fr.Event != "" {
		switch fr.Event {
		case "welcome", "subscribed", "pong":
			return nil
		case "unsubscribed":
			return fmt.Errorf("crossverse aggregate: unexpected unsubscribe for %q", fr.Topic)
		case "error":
			return fmt.Errorf("crossverse aggregate: server error %q", fr.Message)
		default:
			return fmt.Errorf("crossverse aggregate: unknown control event %q", fr.Event)
		}
	}
	if fr.Topic != "markets@all" && fr.Topic != "markets@totals" {
		return fmt.Errorf("crossverse aggregate: unexpected topic %q", fr.Topic)
	}
	seq, err := ParseScaled(fr.Seq.String(), 1, RoundExact)
	if err != nil || seq < 0 {
		return fmt.Errorf("crossverse aggregate: frame seq %q is invalid", fr.Seq.String())
	}
	sourceTs, err := ParseTimestampMs(fr.TS.String())
	if err != nil {
		return fmt.Errorf("crossverse aggregate: frame ts %q is invalid", fr.TS.String())
	}
	if fr.SnapshotID == "" {
		return fmt.Errorf("crossverse aggregate: frame on %q has no snapshotId", fr.Topic)
	}
	receivedMs := m.nowMs()

	m.markets.mu.Lock()
	if m.markets.snapshotID == "" {
		m.markets.snapshotID = fr.SnapshotID
	} else if m.markets.snapshotID != fr.SnapshotID {
		m.markets.snapshotID = fr.SnapshotID
		m.markets.all = topicState{}
		m.markets.totals = topicState{}
		if !fr.Snapshot {
			m.markets.mu.Unlock()
			return errResubscribe
		}
	}
	topic := &m.markets.all
	if fr.Topic == "markets@totals" {
		topic = &m.markets.totals
	}
	if !fr.Snapshot {
		if !topic.snapshotAccepted {
			m.markets.mu.Unlock()
			return nil
		}
		if seq <= topic.lastSeq {
			m.markets.mu.Unlock()
			return nil
		}
		if seq > topic.lastSeq+1 {
			m.markets.all = topicState{}
			m.markets.totals = topicState{}
			m.markets.mu.Unlock()
			return errResubscribe
		}
	}
	topic.lastSeq = seq
	topic.snapshotAccepted = true
	m.markets.mu.Unlock()

	if fr.Topic == "markets@all" {
		return m.applyMarketsList(fr.Data, sourceTs, receivedMs)
	}
	m.aggregateReceivedMs.Store(receivedMs)
	return nil
}

func (m *Manager) MarketsRecord(symbol string) (MarketsRecord, bool) {
	sym := strings.ToUpper(strings.TrimSpace(symbol))
	m.markets.mu.Lock()
	defer m.markets.mu.Unlock()
	rec, ok := m.markets.records[sym]
	return rec, ok
}

func (m *Manager) FetchMarkets(ctx context.Context) error {
	target := m.marketsRESTBase() + "/markets"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return err
	}
	resp, err := m.httpClient().Do(req)
	if err != nil {
		return fmt.Errorf("crossverse markets: request: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxRestBodyBytes))
	if err != nil {
		return fmt.Errorf("crossverse markets: body: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("crossverse markets: status %d", resp.StatusCode)
	}
	now := m.nowMs()
	return m.applyMarketsList(body, now, now)
}
