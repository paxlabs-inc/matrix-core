package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
)

const (
	pageSize    = 1000
	maxAttempts = 10
)

type scalarInt64 int64

func (v *scalarInt64) UnmarshalJSON(raw []byte) error {
	var number json.Number
	if len(raw) > 0 && raw[0] == '"' {
		var text string
		if err := json.Unmarshal(raw, &text); err != nil {
			return err
		}
		number = json.Number(text)
	} else {
		number = json.Number(string(raw))
	}
	n, err := strconv.ParseInt(number.String(), 10, 64)
	if err != nil {
		return err
	}
	*v = scalarInt64(n)
	return nil
}

type globalStats struct {
	IndexerBlock  scalarInt64 `json:"indexerBlock"`
	OpenPositions scalarInt64 `json:"openPositions"`
}

type snapshot struct {
	Version                      int               `json:"version"`
	CapturedAt                   time.Time         `json:"captured_at"`
	DiamondAddress               string            `json:"diamond_address"`
	IndexerBlock                 int64             `json:"indexer_block"`
	IndexerBlockHash             string            `json:"indexer_block_hash"`
	IndexerReportedOpenPositions int64             `json:"indexer_reported_open_positions"`
	Positions                    []json.RawMessage `json:"positions"`
	ActiveOrders                 []json.RawMessage `json:"active_orders"`
}

type client struct {
	indexer string
	rpc     string
	http    *http.Client
}

func main() {
	if err := run(context.Background()); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(ctx context.Context) error {
	out := flag.String("out", "", "destination snapshot JSON file")
	flag.Parse()
	if strings.TrimSpace(*out) == "" {
		return errors.New("-out is required")
	}
	c := client{
		indexer: strings.TrimSpace(os.Getenv("LAYERX_LEGACY_INDEXER_URL")),
		rpc:     strings.TrimSpace(os.Getenv("LAYERX_LEGACY_RPC_URL")),
		http:    &http.Client{Timeout: 30 * time.Second},
	}
	diamond := strings.TrimSpace(os.Getenv("LAYERX_LEGACY_DIAMOND"))
	if c.indexer == "" || c.rpc == "" || diamond == "" {
		return errors.New("LAYERX_LEGACY_INDEXER_URL, LAYERX_LEGACY_RPC_URL, and LAYERX_LEGACY_DIAMOND are required")
	}

	var artifact snapshot
	var err error
	for attempt := 0; attempt < maxAttempts; attempt++ {
		artifact, err = c.capture(ctx, diamond)
		if err == nil {
			break
		}
	}
	if err != nil {
		return err
	}
	body, err := json.MarshalIndent(artifact, "", "  ")
	if err != nil {
		return fmt.Errorf("encode snapshot: %w", err)
	}
	body = append(body, '\n')
	if err := os.WriteFile(*out, body, 0o600); err != nil {
		return fmt.Errorf("write snapshot: %w", err)
	}
	sum := sha256.Sum256(body)
	return json.NewEncoder(os.Stdout).Encode(map[string]any{
		"path": *out, "sha256": hex.EncodeToString(sum[:]),
		"indexer_block": artifact.IndexerBlock,
		"block_hash":    artifact.IndexerBlockHash,
		"positions":     len(artifact.Positions), "active_orders": len(artifact.ActiveOrders),
	})
}

func (c client) capture(ctx context.Context, diamond string) (snapshot, error) {
	before, err := c.stats(ctx)
	if err != nil {
		return snapshot{}, err
	}
	block := int64(before.IndexerBlock)
	if block <= 0 {
		return snapshot{}, errors.New("legacy snapshot: indexer returned no finalized block")
	}
	blockHash, err := c.blockHash(ctx, block)
	if err != nil {
		return snapshot{}, err
	}
	positions, err := c.pages(ctx, "positions", `
		query LegacyPositions($limit:Int!,$offset:Int!) {
		  positions(status:"open",limit:$limit,offset:$offset) {
		    positionId userAddress marketId isLong sizeUsd leverage entryPrice
		    collateralToken collateralAmount collateralUsd status openedAt openTxHash
		  }
		}`)
	if err != nil {
		return snapshot{}, err
	}
	orders, err := c.pages(ctx, "orders", `
		query LegacyOrders($limit:Int!,$offset:Int!) {
		  orders(status:"active",limit:$limit,offset:$offset) {
		    orderId userAddress marketId orderType orderTypeName isLong triggerPrice
		    sizeUsd status positionId placedAt placedTxHash
		  }
		}`)
	if err != nil {
		return snapshot{}, err
	}
	after, err := c.stats(ctx)
	if err != nil {
		return snapshot{}, err
	}
	if int64(after.IndexerBlock) != block {
		return snapshot{}, errors.New("legacy snapshot: indexer advanced during capture")
	}
	if int64(after.OpenPositions) != int64(before.OpenPositions) ||
		int64(before.OpenPositions) != int64(len(positions)) {
		return snapshot{}, errors.New("legacy snapshot: open-position count did not reconcile")
	}
	return snapshot{
		Version: 1, CapturedAt: time.Now().UTC(), DiamondAddress: diamond,
		IndexerBlock: block, IndexerBlockHash: blockHash,
		IndexerReportedOpenPositions: int64(before.OpenPositions),
		Positions:                    positions, ActiveOrders: orders,
	}, nil
}

func (c client) stats(ctx context.Context) (globalStats, error) {
	var data struct {
		GlobalStats globalStats `json:"globalStats"`
	}
	err := c.graphql(ctx, `
		query LegacyGlobalStats {
		  globalStats { indexerBlock openPositions }
		}`, nil, &data)
	return data.GlobalStats, err
}

func (c client) pages(ctx context.Context, field, query string) ([]json.RawMessage, error) {
	var all []json.RawMessage
	for offset := 0; ; offset += pageSize {
		var data map[string]json.RawMessage
		if err := c.graphql(ctx, query, map[string]any{
			"limit": pageSize, "offset": offset,
		}, &data); err != nil {
			return nil, err
		}
		raw, ok := data[field]
		if !ok {
			return nil, fmt.Errorf("legacy snapshot: indexer omitted %s", field)
		}
		var page []json.RawMessage
		if err := json.Unmarshal(raw, &page); err != nil {
			return nil, fmt.Errorf("legacy snapshot: decode %s page: %w", field, err)
		}
		all = append(all, page...)
		if len(page) < pageSize {
			return all, nil
		}
	}
}

func (c client) graphql(ctx context.Context, query string, variables map[string]any, dst any) error {
	body, err := json.Marshal(map[string]any{"query": query, "variables": variables})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.indexer, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("legacy snapshot: indexer request: %w", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("legacy snapshot: indexer status %d", resp.StatusCode)
	}
	var envelope struct {
		Data   json.RawMessage `json:"data"`
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return err
	}
	if len(envelope.Errors) != 0 {
		return fmt.Errorf("legacy snapshot: indexer: %s", envelope.Errors[0].Message)
	}
	return json.Unmarshal(envelope.Data, dst)
}

func (c client) blockHash(ctx context.Context, block int64) (string, error) {
	body, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "eth_getBlockByNumber",
		"params": []any{fmt.Sprintf("0x%x", block), false},
	})
	if err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.rpc, bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("legacy snapshot: rpc request: %w", err)
	}
	defer resp.Body.Close()
	var envelope struct {
		Result struct {
			Hash string `json:"hash"`
		} `json:"result"`
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&envelope); err != nil {
		return "", err
	}
	if envelope.Error != nil {
		return "", fmt.Errorf("legacy snapshot: rpc: %s", envelope.Error.Message)
	}
	if len(envelope.Result.Hash) != 66 {
		return "", errors.New("legacy snapshot: rpc returned an invalid block hash")
	}
	return envelope.Result.Hash, nil
}
