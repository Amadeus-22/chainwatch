// Package ethrpc is a minimal Ethereum JSON-RPC client over HTTP, implementing
// app.Chain with the five methods chainwatch needs. It works against any node or
// proxy (anvil, geth, a hosted provider, rpcgate).
package ethrpc

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/Amadeus-22/chainwatch/internal/app"
	"github.com/Amadeus-22/chainwatch/internal/domain"
	"github.com/Amadeus-22/chainwatch/internal/platform/metrics"
)

// Client calls a JSON-RPC endpoint. Retrying is the caller's job: the indexer
// retries a whole sync with backoff, which keeps this client simple and avoids
// retry amplification when both layers retry.
type Client struct {
	url     string
	http    *http.Client
	timeout time.Duration
	m       *metrics.Metrics
	id      atomic.Uint64
	chainID domain.ChainID
	label   string // chain id as a metric label value
}

var _ app.Chain = (*Client)(nil)

// New returns a client for the node at url, which must serve chainID. timeout
// bounds every call.
func New(url string, chainID domain.ChainID, timeout time.Duration, m *metrics.Metrics) *Client {
	return &Client{url: url, http: &http.Client{}, timeout: timeout, m: m,
		chainID: chainID, label: strconv.FormatUint(uint64(chainID), 10)}
}

// RPCError is an error object returned by the node.
type RPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (e *RPCError) Error() string { return fmt.Sprintf("rpc error %d: %s", e.Code, e.Message) }

// BlockNumber calls eth_blockNumber.
func (c *Client) BlockNumber(ctx context.Context) (uint64, error) {
	var q quantity
	if err := c.call(ctx, "eth_blockNumber", nil, &q); err != nil {
		return 0, err
	}
	return uint64(q), nil
}

// BlockHash calls eth_getBlockByNumber without transaction bodies.
func (c *Client) BlockHash(ctx context.Context, n uint64) (domain.Hash, error) {
	var block *struct {
		Hash domain.Hash `json:"hash"`
	}
	if err := c.call(ctx, "eth_getBlockByNumber", []any{encodeQuantity(n), false}, &block); err != nil {
		return domain.Hash{}, err
	}
	if block == nil {
		return domain.Hash{}, fmt.Errorf("block %d: %w", n, domain.ErrNotFound)
	}
	return block.Hash, nil
}

// Logs calls eth_getLogs. Node refusals of the range are mapped to
// domain.ErrRangeTooLarge.
func (c *Client) Logs(ctx context.Context, q app.LogQuery) ([]domain.Log, error) {
	topics := make([]any, len(q.Topics))
	for i, t := range q.Topics {
		if t != nil {
			topics[i] = t
		}
	}
	filter := map[string]any{
		"fromBlock": encodeQuantity(q.FromBlock),
		"toBlock":   encodeQuantity(q.ToBlock),
		"topics":    topics,
	}
	var raw []rpcLog
	if err := c.call(ctx, "eth_getLogs", []any{filter}, &raw); err != nil {
		var rpcErr *RPCError
		if errors.As(err, &rpcErr) && isRangeTooLarge(rpcErr) {
			return nil, fmt.Errorf("%w: %v", domain.ErrRangeTooLarge, rpcErr)
		}
		return nil, err
	}
	logs := make([]domain.Log, len(raw))
	for i, r := range raw {
		logs[i] = domain.Log{
			Address:     r.Address,
			Topics:      r.Topics,
			Data:        r.Data,
			BlockNumber: uint64(r.BlockNumber),
			BlockHash:   r.BlockHash,
			TxHash:      r.TxHash,
			LogIndex:    uint(r.LogIndex),
			Removed:     r.Removed,
		}
	}
	return logs, nil
}

// HasCode calls eth_getCode at the latest block.
func (c *Client) HasCode(ctx context.Context, a domain.Address) (bool, error) {
	var code hexBytes
	if err := c.call(ctx, "eth_getCode", []any{a, "latest"}, &code); err != nil {
		return false, err
	}
	return len(code) > 0, nil
}

// TxSender calls eth_getTransactionByHash and returns its sender.
func (c *Client) TxSender(ctx context.Context, tx domain.Hash) (domain.Address, error) {
	var t *struct {
		From domain.Address `json:"from"`
	}
	if err := c.call(ctx, "eth_getTransactionByHash", []any{tx}, &t); err != nil {
		return domain.Address{}, err
	}
	if t == nil {
		return domain.Address{}, fmt.Errorf("transaction %s: %w", tx, domain.ErrNotFound)
	}
	return t.From, nil
}

// Ping checks the endpoint is reachable and serves the configured chain (used by
// /readyz): an RPC URL pointing at the wrong network would otherwise index the
// wrong chain silently.
func (c *Client) Ping(ctx context.Context) error {
	var q quantity
	if err := c.call(ctx, "eth_chainId", nil, &q); err != nil {
		return err
	}
	if domain.ChainID(q) != c.chainID {
		return fmt.Errorf("node serves chain id %d, configured %d", uint64(q), c.chainID)
	}
	return nil
}

func (c *Client) call(ctx context.Context, method string, params []any, result any) (err error) {
	defer func() {
		if err != nil {
			c.m.RPCErrors.WithLabelValues(c.label, method).Inc()
			err = fmt.Errorf("%s: %w", method, err)
		}
	}()
	if params == nil {
		params = []any{}
	}
	body, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": c.id.Add(1), "method": method, "params": params})
	if err != nil {
		return fmt.Errorf("encode request: %w", err)
	}
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.url, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("send request: %w", err)
	}
	defer resp.Body.Close()
	payload, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return fmt.Errorf("read response: %w", err)
	}
	var out struct {
		Result json.RawMessage `json:"result"`
		Error  *RPCError       `json:"error"`
	}
	if err := json.Unmarshal(payload, &out); err != nil {
		if resp.StatusCode != http.StatusOK {
			return fmt.Errorf("http status %d: %s", resp.StatusCode, truncate(payload))
		}
		return fmt.Errorf("decode response: %w", err)
	}
	if out.Error != nil {
		return out.Error
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("http status %d: %s", resp.StatusCode, truncate(payload))
	}
	if err := json.Unmarshal(out.Result, result); err != nil {
		return fmt.Errorf("decode result: %w", err)
	}
	return nil
}

// isRangeTooLarge recognises the ways common nodes and providers refuse an
// eth_getLogs range. There is no standard error code, so messages are matched.
func isRangeTooLarge(e *RPCError) bool {
	if e.Code == -32005 { // Infura & others: "query returned more than 10000 results"
		return true
	}
	msg := strings.ToLower(e.Message)
	for _, s := range []string{
		"query returned more than",
		"response size exceeded",
		"block range",
		"range too large",
		"too many results",
		"exceed maximum block range",
		"query timeout exceeded",
	} {
		if strings.Contains(msg, s) {
			return true
		}
	}
	return false
}

func truncate(b []byte) string {
	if len(b) > 200 {
		return string(b[:200]) + "..."
	}
	return string(b)
}

func encodeQuantity(n uint64) string { return "0x" + strconv.FormatUint(n, 16) }

// quantity is a JSON-RPC hex-encoded unsigned integer ("0x1a").
type quantity uint64

func (q *quantity) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return err
	}
	if !strings.HasPrefix(s, "0x") {
		return fmt.Errorf("quantity %q: missing 0x prefix", s)
	}
	n, err := strconv.ParseUint(s[2:], 16, 64)
	if err != nil {
		return fmt.Errorf("quantity %q: %w", s, err)
	}
	*q = quantity(n)
	return nil
}

// hexBytes is JSON-RPC hex-encoded data ("0x", "0xdeadbeef").
type hexBytes []byte

func (h *hexBytes) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return err
	}
	if !strings.HasPrefix(s, "0x") {
		return fmt.Errorf("data %q: missing 0x prefix", s)
	}
	d, err := hex.DecodeString(s[2:])
	if err != nil {
		return fmt.Errorf("data: %w", err)
	}
	*h = d
	return nil
}

type rpcLog struct {
	Address     domain.Address `json:"address"`
	Topics      []domain.Hash  `json:"topics"`
	Data        hexBytes       `json:"data"`
	BlockNumber quantity       `json:"blockNumber"`
	BlockHash   domain.Hash    `json:"blockHash"`
	TxHash      domain.Hash    `json:"transactionHash"`
	LogIndex    quantity       `json:"logIndex"`
	Removed     bool           `json:"removed"`
}
