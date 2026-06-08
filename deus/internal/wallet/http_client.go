package wallet

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/paxlabs-inc/deus/internal/chain"
)

// HTTPClient calls the embedded wallet REST API (docs/10-integration.md §10.3).
type HTTPClient struct {
	BaseURL string
	ChainID int64
	HTTP    *http.Client
}

func (c *HTTPClient) httpClient() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return &http.Client{Timeout: 60 * time.Second}
}

func (c *HTTPClient) base() string {
	return strings.TrimSuffix(strings.TrimSpace(c.BaseURL), "/")
}

type httpStatusError struct {
	code int
	body string
}

func (e *httpStatusError) Error() string {
	if e.body != "" {
		return fmt.Sprintf("wallet: http %d: %s", e.code, e.body)
	}
	return fmt.Sprintf("wallet: http %d", e.code)
}

func (c *HTTPClient) do(ctx context.Context, method, path string, body, out any, bearer string) error {
	if c.base() == "" {
		return fmt.Errorf("wallet: MATRIX_WALLET_API_URL not configured")
	}
	var reader *bytes.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(b)
	} else {
		reader = bytes.NewReader(nil)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base()+path, reader)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	resp, err := c.httpClient().Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusForbidden {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return &PolicyDenied{Message: strings.TrimSpace(string(b))}
	}
	if resp.StatusCode >= 400 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return &httpStatusError{code: resp.StatusCode, body: strings.TrimSpace(string(b))}
	}
	if out == nil {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// AuthorizeSpend verifies the caller bearer; spend policy is enforced on send.
func (c *HTTPClient) AuthorizeSpend(ctx context.Context, bearer, amountWei, serviceID string) error {
	_ = amountWei
	_ = serviceID
	var me struct {
		Wallet struct {
			Address string `json:"address"`
		} `json:"wallet"`
	}
	if err := c.do(ctx, http.MethodGet, "/v1/agent/me", nil, &me, bearer); err != nil {
		return err
	}
	if me.Wallet.Address == "" {
		return fmt.Errorf("wallet: agent wallet not provisioned")
	}
	return nil
}

type sendResponse struct {
	TxHash  string `json:"tx_hash"`
	Address string `json:"address"`
}

func (c *HTTPClient) sendTx(ctx context.Context, bearer string, tx map[string]any) (string, error) {
	if c.ChainID > 0 {
		tx["chainId"] = c.ChainID
	}
	var out sendResponse
	if err := c.do(ctx, http.MethodPost, "/v1/agent/send", map[string]any{"tx": tx}, &out, bearer); err != nil {
		return "", err
	}
	if out.TxHash == "" {
		return "", fmt.Errorf("wallet: send returned no tx_hash")
	}
	return out.TxHash, nil
}

// Send executes POST /v1/agent/send on the direct rail.
func (c *HTTPClient) Send(ctx context.Context, bearer, toAddress, amountWei string) (string, error) {
	return c.sendTx(ctx, bearer, map[string]any{
		"to":    toAddress,
		"value": amountWei,
	})
}

// FundEscrow funds a PaymentChannel via fund() with native PAX.
func (c *HTTPClient) FundEscrow(ctx context.Context, bearer, escrowAddr, capWei string) (string, error) {
	enc, err := chain.NewPaymentChannel(nil)
	if err != nil {
		return "", err
	}
	data, err := enc.EncodeFund()
	if err != nil {
		return "", err
	}
	return c.sendTx(ctx, bearer, map[string]any{
		"to":    escrowAddr,
		"value": capWei,
		"data":  "0x" + hex.EncodeToString(data),
	})
}

// OpenStream opens a PaymentStreams 0x0906 session via agent/send.
func (c *HTTPClient) OpenStream(ctx context.Context, bearer string, in StreamOpenInput) (OpenStreamResult, error) {
	streams, err := chain.NewPaymentStreams(nil)
	if err != nil {
		return OpenStreamResult{}, err
	}
	token := common.Address{}
	if in.Token != "" {
		token = common.HexToAddress(in.Token)
	}
	rate, ok := new(big.Int).SetString(in.RatePerSecondWei, 10)
	if !ok {
		return OpenStreamResult{}, fmt.Errorf("wallet: invalid stream rate")
	}
	capWei, ok := new(big.Int).SetString(in.CapWei, 10)
	if !ok {
		return OpenStreamResult{}, fmt.Errorf("wallet: invalid stream cap")
	}
	data, err := streams.EncodeOpen(common.HexToAddress(in.Payee), token, rate, capWei, 0, in.StopTime)
	if err != nil {
		return OpenStreamResult{}, err
	}
	txHash, err := c.sendTx(ctx, bearer, map[string]any{
		"to":    chain.PaymentStreamsAddr.Hex(),
		"value": in.CapWei,
		"data":  "0x" + hex.EncodeToString(data),
	})
	if err != nil {
		return OpenStreamResult{}, err
	}
	return OpenStreamResult{ChainStreamID: "", TxHash: txHash}, nil
}

// StreamSettle proxies streams.settle via agent/send.
func (c *HTTPClient) StreamSettle(ctx context.Context, bearer, chainStreamID string) (string, error) {
	return c.streamTx(ctx, bearer, chainStreamID, "settle")
}

// StreamClose proxies streams.close via agent/send.
func (c *HTTPClient) StreamClose(ctx context.Context, bearer, chainStreamID string) (string, error) {
	return c.streamTx(ctx, bearer, chainStreamID, "close")
}

func (c *HTTPClient) streamTx(ctx context.Context, bearer, chainStreamID, method string) (string, error) {
	streams, err := chain.NewPaymentStreams(nil)
	if err != nil {
		return "", err
	}
	id, ok := new(big.Int).SetString(chainStreamID, 10)
	if !ok {
		return "", fmt.Errorf("wallet: invalid stream id")
	}
	var data []byte
	switch method {
	case "settle":
		data, err = streams.EncodeSettle(id)
	case "close":
		data, err = streams.EncodeClose(id)
	default:
		return "", fmt.Errorf("wallet: unknown stream method")
	}
	if err != nil {
		return "", err
	}
	return c.sendTx(ctx, bearer, map[string]any{
		"to":   chain.PaymentStreamsAddr.Hex(),
		"data": "0x" + hex.EncodeToString(data),
	})
}
