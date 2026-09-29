package config

import (
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/Amadeus-22/chainwatch/internal/domain"
)

func env(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func TestLoadDefaults(t *testing.T) {
	c, err := Load(env(map[string]string{
		"CHAINWATCH_RPC_URL":      "http://localhost:8545",
		"CHAINWATCH_DATABASE_URL": "postgres://u:p@localhost/db",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if c.HTTPAddr != ":8080" || c.Confirmations != 12 || c.ReorgWindow != 64 || c.MaxBlockRange != 2000 ||
		c.PollInterval != 6*time.Second || c.StartBlock != nil || c.LogLevel != slog.LevelInfo || len(c.LargeOutflow) != 0 {
		t.Fatalf("unexpected defaults: %+v", c)
	}
}

func TestLoadOverrides(t *testing.T) {
	c, err := Load(env(map[string]string{
		"CHAINWATCH_RPC_URL":         "https://rpc.example",
		"CHAINWATCH_DATABASE_URL":    "postgres://x",
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
	}))
	if err != nil {
		t.Fatal(err)
	}
	token, _ := domain.ParseAddress("0x5fbdb2315678afecb367f032d93f642f64180aa3")
	if c.StartBlock == nil || *c.StartBlock != 0 || c.Confirmations != 2 || c.LogLevel != slog.LevelDebug ||
		c.LargeOutflow[token].String() != "100000000000000000000" || len(c.LargeOutflow) != 2 ||
		c.PollInterval != 500*time.Millisecond || c.WebhookURL == "" {
		t.Fatalf("overrides not applied: %+v", c)
	}
}

func TestLoadReportsEveryProblem(t *testing.T) {
	_, err := Load(env(map[string]string{
		"CHAINWATCH_RPC_URL":            "localhost:8545",
		"CHAINWATCH_CONFIRMATIONS":      "-1",
		"CHAINWATCH_POLL_INTERVAL":      "soon",
		"CHAINWATCH_MAX_BLOCK_RANGE":    "0",
		"CHAINWATCH_LARGE_OUTFLOW":      "0x12=5,nonsense",
		"CHAINWATCH_TELEGRAM_BOT_TOKEN": "t",
		"CHAINWATCH_LOG_LEVEL":          "loud",
	}))
	if err == nil {
		t.Fatal("want error")
	}
	for _, key := range []string{
		"CHAINWATCH_RPC_URL", "CHAINWATCH_DATABASE_URL", "CHAINWATCH_CONFIRMATIONS", "CHAINWATCH_POLL_INTERVAL",
		"CHAINWATCH_MAX_BLOCK_RANGE", "CHAINWATCH_LARGE_OUTFLOW", "CHAINWATCH_TELEGRAM_CHAT_ID", "CHAINWATCH_LOG_LEVEL",
	} {
		if !strings.Contains(err.Error(), key) {
			t.Errorf("error does not mention %s: %v", key, err)
		}
	}
}

func TestLoadRejectsTwoNotifiers(t *testing.T) {
	_, err := Load(env(map[string]string{
		"CHAINWATCH_RPC_URL":            "http://a",
		"CHAINWATCH_DATABASE_URL":       "postgres://x",
		"CHAINWATCH_WEBHOOK_URL":        "http://b",
		"CHAINWATCH_TELEGRAM_BOT_TOKEN": "t",
		"CHAINWATCH_TELEGRAM_CHAT_ID":   "1",
	}))
	if err == nil || !strings.Contains(err.Error(), "not both") {
		t.Fatalf("err = %v", err)
	}
}
