package config

import (
	"errors"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Amadeus-22/chainwatch/internal/domain"
)

func env(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func noFile(string) ([]byte, error) { return nil, errors.New("no file in this test") }

func files(m map[string]string) func(string) ([]byte, error) {
	return func(name string) ([]byte, error) {
		v, ok := m[name]
		if !ok {
			return nil, os.ErrNotExist
		}
		return []byte(v), nil
	}
}

// base is the minimum valid single-chain environment.
func base(extra map[string]string) map[string]string {
	m := map[string]string{
		"CHAINWATCH_RPC_URL":      "http://localhost:8545",
		"CHAINWATCH_CHAIN_ID":     "31337",
		"CHAINWATCH_DATABASE_URL": "postgres://u:p@localhost/db",
		"CHAINWATCH_SIWE_DOMAIN":  "localhost:5173",
	}
	for k, v := range extra {
		m[k] = v
	}
	return m
}

func TestLoadDefaults(t *testing.T) {
	c, err := Load(env(base(nil)), noFile)
	if err != nil {
		t.Fatal(err)
	}
	if c.HTTPAddr != ":8080" || c.LogLevel != slog.LevelInfo || !c.CookieSecure || c.SessionTTL != 24*time.Hour ||
		c.DrainWindow != 7200 || len(c.Chains) != 1 {
		t.Fatalf("unexpected defaults: %+v", c)
	}
	ch := c.Chains[0]
	if ch.ID != 31337 || ch.Name != "Chain 31337" || ch.Confirmations != 12 || ch.ReorgWindow != 64 ||
		ch.MaxBlockRange != 2000 || ch.PollInterval != 6*time.Second || ch.StartBlock != nil || len(ch.LargeOutflow) != 0 {
		t.Fatalf("unexpected chain defaults: %+v", ch)
	}
}

func TestLoadOverrides(t *testing.T) {
	c, err := Load(env(base(map[string]string{
		"CHAINWATCH_RPC_URL":         "https://rpc.example",
		"CHAINWATCH_CHAIN_NAME":      "anvil",
		"CHAINWATCH_START_BLOCK":     "0",
		"CHAINWATCH_CONFIRMATIONS":   "2",
		"CHAINWATCH_LOG_LEVEL":       "debug",
		"CHAINWATCH_LARGE_OUTFLOW":   "0x5FbDB2315678afecb367f032d93F642f64180aa3=100000000000000000000, 0x0000000000000000000000000000000000000001=5",
		"CHAINWATCH_WEBHOOK_URL":     "http://hooks.local/x",
		"CHAINWATCH_POLL_INTERVAL":   "500ms",
		"CHAINWATCH_REORG_WINDOW":    "10",
		"CHAINWATCH_HTTP_ADDR":       "127.0.0.1:9000",
		"CHAINWATCH_RPC_TIMEOUT":     "3s",
		"CHAINWATCH_NOTIFY_INTERVAL": "1s",
		"CHAINWATCH_COOKIE_SECURE":   "false",
		"CHAINWATCH_SESSION_TTL":     "1h",
	})), noFile)
	if err != nil {
		t.Fatal(err)
	}
	token, _ := domain.ParseAddress("0x5fbdb2315678afecb367f032d93f642f64180aa3")
	ch := c.Chains[0]
	if ch.StartBlock == nil || *ch.StartBlock != 0 || ch.Confirmations != 2 || c.LogLevel != slog.LevelDebug ||
		ch.LargeOutflow[token].String() != "100000000000000000000" || len(ch.LargeOutflow) != 2 ||
		ch.PollInterval != 500*time.Millisecond || c.WebhookURL == "" || ch.Name != "anvil" ||
		c.CookieSecure || c.SessionTTL != time.Hour {
		t.Fatalf("overrides not applied: %+v", c)
	}
}

const twoChains = `
chains:
  - id: 1
    name: Ethereum
    rpc_url: https://eth.example/v2/${TEST_API_KEY}
    explorer_url: https://etherscan.io
    confirmations: 12
    large_outflow:
      "0xA0b86991c6218b36c1d19D4a2e9Eb0cE3606eB48": "100000000000"
  - id: 8453
    name: Base
    rpc_url: https://base.example
    confirmations: 30
    poll_interval: 2s
    max_block_range: 500
`

func TestLoadChainsFile(t *testing.T) {
	m := base(map[string]string{"CHAINWATCH_CHAINS_FILE": "chains.yaml", "TEST_API_KEY": "sekret",
		"CHAINWATCH_REORG_WINDOW": "32"})
	delete(m, "CHAINWATCH_RPC_URL")
	delete(m, "CHAINWATCH_CHAIN_ID")
	c, err := Load(env(m), files(map[string]string{"chains.yaml": twoChains}))
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Chains) != 2 {
		t.Fatalf("chains = %+v", c.Chains)
	}
	eth, base := c.Chains[0], c.Chains[1]
	if eth.ID != 1 || eth.RPCURL != "https://eth.example/v2/sekret" || eth.Confirmations != 12 ||
		eth.ExplorerURL != "https://etherscan.io" || len(eth.LargeOutflow) != 1 || eth.ReorgWindow != 32 {
		t.Fatalf("ethereum = %+v", eth)
	}
	if base.ID != 8453 || base.Confirmations != 30 || base.PollInterval != 2*time.Second ||
		base.MaxBlockRange != 500 || base.ReorgWindow != 32 || len(base.LargeOutflow) != 0 {
		t.Fatalf("base = %+v", base)
	}
}

func TestExampleChainsFileIsValid(t *testing.T) {
	m := base(map[string]string{"CHAINWATCH_CHAINS_FILE": "../../../deploy/chains.example.yaml", "ALCHEMY_API_KEY": "k"})
	delete(m, "CHAINWATCH_RPC_URL")
	delete(m, "CHAINWATCH_CHAIN_ID")
	c, err := Load(env(m), os.ReadFile)
	if err != nil {
		t.Fatal(err)
	}
	var ids []domain.ChainID
	for _, ch := range c.Chains {
		ids = append(ids, ch.ID)
	}
	if len(ids) != 4 || ids[0] != 1 || ids[1] != 8453 || ids[2] != 42161 || ids[3] != 137 {
		t.Fatalf("example chains = %v, want Ethereum, Base, Arbitrum One, Polygon", ids)
	}
}

func TestLoadChainsFileErrors(t *testing.T) {
	for name, tt := range map[string]struct {
		yaml string
		want string
	}{
		"unknown key":  {"chains:\n  - id: 1\n    name: E\n    rpc_url: http://a\n    confirmation: 3\n", "field confirmation not found"},
		"duplicate id": {"chains:\n  - {id: 1, name: A, rpc_url: http://a}\n  - {id: 1, name: B, rpc_url: http://b}\n", "listed twice"},
		"missing name": {"chains:\n  - {id: 1, rpc_url: http://a}\n", "name is required"},
		"bad rpc":      {"chains:\n  - {id: 1, name: A, rpc_url: ws://a}\n", "rpc_url must be"},
		"zero id":      {"chains:\n  - {name: A, rpc_url: http://a}\n", "positive EIP-155"},
		"empty":        {"chains: []\n", "lists no chains"},
		"bad outflow":  {"chains:\n  - {id: 1, name: A, rpc_url: http://a, large_outflow: {\"0x12\": \"5\"}}\n", "large_outflow"},
	} {
		t.Run(name, func(t *testing.T) {
			m := base(map[string]string{"CHAINWATCH_CHAINS_FILE": "c.yaml"})
			delete(m, "CHAINWATCH_RPC_URL")
			_, err := Load(env(m), files(map[string]string{"c.yaml": tt.yaml}))
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("err = %v, want it to mention %q", err, tt.want)
			}
		})
	}
}

func TestLoadNeedsExactlyOneChainSource(t *testing.T) {
	m := base(map[string]string{"CHAINWATCH_CHAINS_FILE": "c.yaml"})
	if _, err := Load(env(m), noFile); err == nil || !strings.Contains(err.Error(), "not both") {
		t.Fatalf("both: err = %v", err)
	}
	delete(m, "CHAINWATCH_CHAINS_FILE")
	delete(m, "CHAINWATCH_RPC_URL")
	if _, err := Load(env(m), noFile); err == nil || !strings.Contains(err.Error(), "CHAINWATCH_CHAINS_FILE: is required") {
		t.Fatalf("neither: err = %v", err)
	}
	m = base(nil)
	delete(m, "CHAINWATCH_CHAIN_ID")
	if _, err := Load(env(m), noFile); err == nil || !strings.Contains(err.Error(), "CHAINWATCH_CHAIN_ID") {
		t.Fatalf("rpc without chain id: err = %v", err)
	}
}

func TestLoadReportsEveryProblem(t *testing.T) {
	_, err := Load(env(map[string]string{
		"CHAINWATCH_RPC_URL":            "localhost:8545",
		"CHAINWATCH_CHAIN_ID":           "1",
		"CHAINWATCH_CONFIRMATIONS":      "-1",
		"CHAINWATCH_POLL_INTERVAL":      "soon",
		"CHAINWATCH_MAX_BLOCK_RANGE":    "0",
		"CHAINWATCH_LARGE_OUTFLOW":      "0x12=5,nonsense",
		"CHAINWATCH_TELEGRAM_BOT_TOKEN": "t",
		"CHAINWATCH_LOG_LEVEL":          "loud",
		"CHAINWATCH_COOKIE_SECURE":      "maybe",
	}), noFile)
	if err == nil {
		t.Fatal("want error")
	}
	for _, key := range []string{
		"CHAINWATCH_RPC_URL", "CHAINWATCH_DATABASE_URL", "CHAINWATCH_CONFIRMATIONS", "CHAINWATCH_POLL_INTERVAL",
		"CHAINWATCH_MAX_BLOCK_RANGE", "CHAINWATCH_LARGE_OUTFLOW", "CHAINWATCH_TELEGRAM_CHAT_ID", "CHAINWATCH_LOG_LEVEL",
		"CHAINWATCH_SIWE_DOMAIN", "CHAINWATCH_COOKIE_SECURE",
	} {
		if !strings.Contains(err.Error(), key) {
			t.Errorf("error does not mention %s: %v", key, err)
		}
	}
}

func TestLoadRejectsTwoNotifiers(t *testing.T) {
	_, err := Load(env(base(map[string]string{
		"CHAINWATCH_WEBHOOK_URL":        "http://b",
		"CHAINWATCH_TELEGRAM_BOT_TOKEN": "t",
		"CHAINWATCH_TELEGRAM_CHAT_ID":   "1",
	})), noFile)
	if err == nil || !strings.Contains(err.Error(), "not both") {
		t.Fatalf("err = %v", err)
	}
}
