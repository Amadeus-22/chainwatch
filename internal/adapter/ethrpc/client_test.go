package ethrpc_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/Amadeus-22/chainwatch/internal/adapter/ethrpc"
	"github.com/Amadeus-22/chainwatch/internal/app"
	"github.com/Amadeus-22/chainwatch/internal/domain"
	"github.com/Amadeus-22/chainwatch/internal/domain/erc20"
	"github.com/Amadeus-22/chainwatch/internal/platform/metrics"
)

type request struct {
	Method string            `json:"method"`
	Params []json.RawMessage `json:"params"`
}

// node is an httptest JSON-RPC server answering from a method -> raw reply table.
func node(t *testing.T, replies map[string]string, seen *[]request) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.Header.Get("Content-Type") != "application/json" {
			t.Errorf("bad request %s %s", r.Method, r.Header.Get("Content-Type"))
		}
		body, _ := io.ReadAll(r.Body)
		var req request
		if err := json.Unmarshal(body, &req); err != nil {
			t.Errorf("bad body %s", body)
		}
		if seen != nil {
			*seen = append(*seen, req)
		}
		reply, ok := replies[req.Method]
		if !ok {
			t.Errorf("unexpected method %s", req.Method)
		}
		_, _ = io.WriteString(w, `{"jsonrpc":"2.0","id":1,`+reply+`}`)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func newClient(url string) (*ethrpc.Client, *metrics.Metrics) {
	m := metrics.New(prometheus.NewRegistry())
	return ethrpc.New(url, 31337, time.Second, m), m
}

// A real anvil log from contracts/script/Drain.s.sol (the unlimited approval).
const anvilApprovalLog = `{
  "address": "0x5fbdb2315678afecb367f032d93f642f64180aa3",
  "topics": [
    "0x8c5be1e5ebec7d5bd14f71427d1e84f3dd0314c0f7b2291e5b200ac8c7c3b925",
    "0x00000000000000000000000070997970c51812dc3a010c7d01b50e0d17dc79c8",
    "0x0000000000000000000000003c44cdddb6a900fa2b585dd299e03d12fa4293bc"
  ],
  "data": "0xffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff",
  "blockHash": "0x79a5c73a991b89ab76230392cd29489037f79c1a27c3c8831a1324fa8912cad3",
  "blockNumber": "0x2",
  "blockTimestamp": "0x6abbd249",
  "transactionHash": "0x6b8676565d46df94b7c1ec925bec4dae61ee05661bb6e9f1041bfcc58420455a",
  "transactionIndex": "0x0",
  "logIndex": "0x0",
  "removed": false
}`

func TestLogsEncodesFilterAndDecodesLogs(t *testing.T) {
	var seen []request
	srv := node(t, map[string]string{"eth_getLogs": `"result":[` + anvilApprovalLog + `]`}, &seen)
	c, _ := newClient(srv.URL)
	wallet, _ := domain.ParseAddress("0x70997970c51812dc3a010c7d01b50e0d17dc79c8")

	logs, err := c.Logs(context.Background(), app.LogQuery{
		FromBlock: 1, ToBlock: 26,
		Topics: [][]domain.Hash{{erc20.TopicTransfer}, nil, {erc20.AddressTopic(wallet)}},
	})
	if err != nil {
		t.Fatal(err)
	}
	wantFilter := `{"fromBlock":"0x1","toBlock":"0x1a","topics":[["0xddf252ad1be2c89b69c2b068fc378daa952ba7f163c4a11628f55a4df523b3ef"],null,["0x00000000000000000000000070997970c51812dc3a010c7d01b50e0d17dc79c8"]]}`
	if got := string(seen[0].Params[0]); got != wantFilter {
		t.Errorf("filter\n got %s\nwant %s", got, wantFilter)
	}
	if len(logs) != 1 {
		t.Fatalf("got %d logs", len(logs))
	}
	ev, err := erc20.Decode(logs[0])
	if err != nil {
		t.Fatal(err)
	}
	if ev.Kind != domain.KindApproval || ev.From != wallet || ev.BlockNumber != 2 || ev.LogIndex != 0 ||
		ev.TxHash.String() != "0x6b8676565d46df94b7c1ec925bec4dae61ee05661bb6e9f1041bfcc58420455a" {
		t.Errorf("unexpected event %+v", ev)
	}
}

func TestLogsMapsRangeRefusals(t *testing.T) {
	for _, reply := range []string{
		`"error":{"code":-32005,"message":"query returned more than 10000 results"}`,
		`"error":{"code":-32602,"message":"Log response size exceeded. You can make eth_getLogs requests with up to a 2K block range"}`,
		`"error":{"code":-32000,"message":"exceed maximum block range: 5000"}`,
		`"error":{"code":-32600,"message":"block range too large"}`,
	} {
		srv := node(t, map[string]string{"eth_getLogs": reply}, nil)
		c, m := newClient(srv.URL)
		_, err := c.Logs(context.Background(), app.LogQuery{FromBlock: 0, ToBlock: 1})
		if !errors.Is(err, domain.ErrRangeTooLarge) {
			t.Errorf("reply %s: err = %v, want ErrRangeTooLarge", reply, err)
		}
		if got := testutil.ToFloat64(m.RPCErrors.WithLabelValues("31337", "eth_getLogs")); got != 1 {
			t.Errorf("rpc_errors_total = %v, want 1", got)
		}
	}
}

func TestOtherRPCErrorsAreNotRangeErrors(t *testing.T) {
	srv := node(t, map[string]string{"eth_getLogs": `"error":{"code":-32000,"message":"header not found"}`}, nil)
	c, _ := newClient(srv.URL)
	_, err := c.Logs(context.Background(), app.LogQuery{})
	var rpcErr *ethrpc.RPCError
	if errors.Is(err, domain.ErrRangeTooLarge) || !errors.As(err, &rpcErr) || rpcErr.Code != -32000 {
		t.Fatalf("err = %v", err)
	}
	if !strings.Contains(err.Error(), "eth_getLogs") {
		t.Errorf("error does not name the method: %v", err)
	}
}

func TestBlockMethods(t *testing.T) {
	var seen []request
	srv := node(t, map[string]string{
		"eth_blockNumber":          `"result":"0x10"`,
		"eth_getBlockByNumber":     `"result":{"hash":"0x94d8c47ec131f3d55978aa14e46152b9e9673380c3dbf7e8f5f1e39805a86984","number":"0x3"}`,
		"eth_getCode":              `"result":"0x6080"`,
		"eth_getTransactionByHash": `"result":{"from":"0x3c44cdddb6a900fa2b585dd299e03d12fa4293bc"}`,
	}, &seen)
	c, _ := newClient(srv.URL)
	ctx := context.Background()

	if n, err := c.BlockNumber(ctx); err != nil || n != 16 {
		t.Errorf("BlockNumber = %d, %v", n, err)
	}
	if h, err := c.BlockHash(ctx, 3); err != nil || h.String() != "0x94d8c47ec131f3d55978aa14e46152b9e9673380c3dbf7e8f5f1e39805a86984" {
		t.Errorf("BlockHash = %s, %v", h, err)
	}
	if got := string(seen[1].Params[0]) + string(seen[1].Params[1]); got != `"0x3"false` {
		t.Errorf("eth_getBlockByNumber params = %s", got)
	}
	if ok, err := c.HasCode(ctx, domain.Address{}); err != nil || !ok {
		t.Errorf("HasCode = %v, %v", ok, err)
	}
	if s, err := c.TxSender(ctx, domain.Hash{}); err != nil || s.String() != "0x3c44cdddb6a900fa2b585dd299e03d12fa4293bc" {
		t.Errorf("TxSender = %s, %v", s, err)
	}
}

func TestEmptyCodeAndMissingBlock(t *testing.T) {
	srv := node(t, map[string]string{
		"eth_getCode":              `"result":"0x"`,
		"eth_getBlockByNumber":     `"result":null`,
		"eth_getTransactionByHash": `"result":null`,
	}, nil)
	c, _ := newClient(srv.URL)
	ctx := context.Background()
	if ok, err := c.HasCode(ctx, domain.Address{}); err != nil || ok {
		t.Errorf("HasCode on EOA = %v, %v", ok, err)
	}
	if _, err := c.BlockHash(ctx, 99); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("BlockHash on missing block: %v", err)
	}
	if _, err := c.TxSender(ctx, domain.Hash{}); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("TxSender on missing tx: %v", err)
	}
}

func TestHTTPFailureAndTimeout(t *testing.T) {
	down := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "bad gateway", http.StatusBadGateway)
	}))
	defer down.Close()
	c, m := newClient(down.URL)
	if _, err := c.BlockNumber(context.Background()); err == nil || !strings.Contains(err.Error(), "502") {
		t.Errorf("502: err = %v", err)
	}
	if got := testutil.ToFloat64(m.RPCErrors.WithLabelValues("31337", "eth_blockNumber")); got != 1 {
		t.Errorf("rpc_errors_total = %v", got)
	}

	release := make(chan struct{})
	slow := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { <-release }))
	defer slow.Close()
	defer close(release)
	c = ethrpc.New(slow.URL, 31337, 50*time.Millisecond, metrics.New(prometheus.NewRegistry()))
	start := time.Now()
	if _, err := c.BlockNumber(context.Background()); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("slow node: err = %v, want deadline exceeded", err)
	}
	if time.Since(start) > time.Second {
		t.Error("timeout not enforced")
	}
}

func TestPingChecksChainID(t *testing.T) {
	anvil := node(t, map[string]string{"eth_chainId": `"result":"0x7a69"`}, nil)
	c, _ := newClient(anvil.URL)
	if err := c.Ping(context.Background()); err != nil {
		t.Fatalf("Ping on the configured chain: %v", err)
	}
	mainnet := node(t, map[string]string{"eth_chainId": `"result":"0x1"`}, nil)
	c, _ = newClient(mainnet.URL)
	if err := c.Ping(context.Background()); err == nil || !strings.Contains(err.Error(), "chain id 1, configured 31337") {
		t.Fatalf("Ping on the wrong chain: err = %v", err)
	}
}
