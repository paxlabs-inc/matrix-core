package chain

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum/common"
)

const maxAnchorHistoryResponse = 1 << 20

type BlockscoutAnchorHistory struct {
	base   *url.URL
	client *http.Client
}

func NewBlockscoutAnchorHistory(baseURL string, client *http.Client) (*BlockscoutAnchorHistory, error) {
	base, err := url.Parse(strings.TrimSpace(baseURL))
	if err != nil {
		return nil, fmt.Errorf("chain: parse anchor history url: %w", err)
	}
	if (base.Scheme != "http" && base.Scheme != "https") || base.Host == "" || base.User != nil {
		return nil, fmt.Errorf("chain: anchor history url must be an http(s) origin without userinfo")
	}
	if base.RawQuery != "" || base.Fragment != "" {
		return nil, fmt.Errorf("chain: anchor history url must not contain query or fragment")
	}
	base.Path = strings.TrimRight(base.Path, "/")
	if client == nil {
		client = &http.Client{Timeout: 15 * time.Second}
	}
	return &BlockscoutAnchorHistory{base: base, client: client}, nil
}

func (h *BlockscoutAnchorHistory) FindAnchorTx(ctx context.Context, anchor common.Address, eventID common.Hash, batch Batch) (string, error) {
	u := *h.base
	u.Path += "/api"
	q := u.Query()
	q.Set("module", "logs")
	q.Set("action", "getLogs")
	q.Set("fromBlock", "0")
	q.Set("toBlock", "latest")
	q.Set("address", anchor.Hex())
	q.Set("topic0", eventID.Hex())
	q.Set("topic1", common.BytesToHash(batch.BatchId[:]).Hex())
	q.Set("topic0_1_opr", "and")
	u.RawQuery = q.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return "", fmt.Errorf("chain: build anchor history request: %w", err)
	}
	resp, err := h.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("chain: query anchor history: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		return "", fmt.Errorf("chain: anchor history returned status %d", resp.StatusCode)
	}

	var body struct {
		Status  string `json:"status"`
		Message string `json:"message"`
		Result  []struct {
			Address         string    `json:"address"`
			Data            string    `json:"data"`
			Topics          []*string `json:"topics"`
			TransactionHash string    `json:"transactionHash"`
		} `json:"result"`
	}
	dec := json.NewDecoder(io.LimitReader(resp.Body, maxAnchorHistoryResponse))
	if err := dec.Decode(&body); err != nil {
		return "", fmt.Errorf("chain: decode anchor history: %w", err)
	}
	if body.Status != "1" {
		return "", fmt.Errorf("chain: anchor history lookup failed: %s", body.Message)
	}
	if len(body.Result) != 1 {
		return "", fmt.Errorf("chain: anchor history returned %d matching logs, want 1", len(body.Result))
	}
	log := body.Result[0]
	if !common.IsHexAddress(log.Address) || common.HexToAddress(log.Address) != anchor {
		return "", fmt.Errorf("chain: anchor history returned wrong contract %q", log.Address)
	}
	if len(log.Topics) < 2 || log.Topics[0] == nil || log.Topics[1] == nil {
		return "", fmt.Errorf("chain: anchor history returned incomplete topics")
	}
	if common.HexToHash(*log.Topics[0]) != eventID || common.HexToHash(*log.Topics[1]) != common.BytesToHash(batch.BatchId[:]) {
		return "", fmt.Errorf("chain: anchor history returned mismatched topics")
	}
	data := common.FromHex(log.Data)
	if len(data) < common.HashLength || common.BytesToHash(data[:common.HashLength]) != common.BytesToHash(batch.Root[:]) {
		return "", fmt.Errorf("chain: anchor history returned mismatched root")
	}
	txBytes := common.FromHex(log.TransactionHash)
	if len(txBytes) != common.HashLength || common.BytesToHash(txBytes) == (common.Hash{}) {
		return "", fmt.Errorf("chain: anchor history returned invalid transaction hash %q", log.TransactionHash)
	}
	return common.BytesToHash(txBytes).Hex(), nil
}
