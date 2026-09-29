// Package config reads chainwatch's configuration from environment variables,
// once, at startup. Every variable is documented in the README table.
package config

import (
	"errors"
	"fmt"
	"log/slog"
	"math/big"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/Amadeus-22/chainwatch/internal/domain"
)

// Config is the validated process configuration.
type Config struct {
	RPCURL      string
	DatabaseURL string
	HTTPAddr    string
	LogLevel    slog.Level

	Confirmations uint64
	ReorgWindow   uint64
	MaxBlockRange uint64
	StartBlock    *uint64 // nil: start at the confirmed head on first run
	PollInterval  time.Duration
	RPCTimeout    time.Duration

	DrainWindow  uint64
	LargeOutflow map[domain.Address]*big.Int

	NotifyInterval   time.Duration
	WebhookURL       string
	TelegramBotToken string
	TelegramChatID   string

	ShutdownTimeout time.Duration
}

// Load parses and validates the configuration. getenv is os.Getenv in production
// and a map lookup in tests. All problems are reported at once.
func Load(getenv func(string) string) (Config, error) {
	p := parser{getenv: getenv}
	c := Config{
		RPCURL:      p.url("CHAINWATCH_RPC_URL", "", true),
		DatabaseURL: p.str("CHAINWATCH_DATABASE_URL", "", true),
		HTTPAddr:    p.str("CHAINWATCH_HTTP_ADDR", ":8080", false),
		LogLevel:    p.level("CHAINWATCH_LOG_LEVEL", slog.LevelInfo),

		Confirmations: p.uint("CHAINWATCH_CONFIRMATIONS", 12),
		ReorgWindow:   p.uint("CHAINWATCH_REORG_WINDOW", 64),
		MaxBlockRange: p.uint("CHAINWATCH_MAX_BLOCK_RANGE", 2000),
		StartBlock:    p.optionalUint("CHAINWATCH_START_BLOCK"),
		PollInterval:  p.duration("CHAINWATCH_POLL_INTERVAL", 6*time.Second),
		RPCTimeout:    p.duration("CHAINWATCH_RPC_TIMEOUT", 10*time.Second),

		DrainWindow:  p.uint("CHAINWATCH_DRAIN_WINDOW_BLOCKS", 7200),
		LargeOutflow: p.thresholds("CHAINWATCH_LARGE_OUTFLOW"),

		NotifyInterval:   p.duration("CHAINWATCH_NOTIFY_INTERVAL", 2*time.Second),
		WebhookURL:       p.url("CHAINWATCH_WEBHOOK_URL", "", false),
		TelegramBotToken: p.str("CHAINWATCH_TELEGRAM_BOT_TOKEN", "", false),
		TelegramChatID:   p.str("CHAINWATCH_TELEGRAM_CHAT_ID", "", false),

		ShutdownTimeout: p.duration("CHAINWATCH_SHUTDOWN_TIMEOUT", 15*time.Second),
	}
	if c.MaxBlockRange == 0 {
		p.fail("CHAINWATCH_MAX_BLOCK_RANGE", "must be at least 1")
	}
	if (c.TelegramBotToken == "") != (c.TelegramChatID == "") {
		p.fail("CHAINWATCH_TELEGRAM_CHAT_ID", "CHAINWATCH_TELEGRAM_BOT_TOKEN and CHAINWATCH_TELEGRAM_CHAT_ID must be set together")
	}
	if c.WebhookURL != "" && c.TelegramBotToken != "" {
		p.fail("CHAINWATCH_WEBHOOK_URL", "configure either a webhook or Telegram, not both")
	}
	if err := errors.Join(p.errs...); err != nil {
		return Config{}, fmt.Errorf("load config: %w", err)
	}
	return c, nil
}

type parser struct {
	getenv func(string) string
	errs   []error
}

func (p *parser) fail(key, format string, args ...any) {
	p.errs = append(p.errs, fmt.Errorf("%s: %s", key, fmt.Sprintf(format, args...)))
}

func (p *parser) str(key, def string, required bool) string {
	v := strings.TrimSpace(p.getenv(key))
	if v == "" {
		if required {
			p.fail(key, "is required")
		}
		return def
	}
	return v
}

func (p *parser) url(key, def string, required bool) string {
	v := p.str(key, def, required)
	if v == "" {
		return v
	}
	u, err := url.Parse(v)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		p.fail(key, "must be an http(s) URL, got %q", v)
	}
	return v
}

func (p *parser) uint(key string, def uint64) uint64 {
	if n := p.optionalUint(key); n != nil {
		return *n
	}
	return def
}

func (p *parser) optionalUint(key string) *uint64 {
	v := strings.TrimSpace(p.getenv(key))
	if v == "" {
		return nil
	}
	n, err := strconv.ParseUint(v, 10, 64)
	if err != nil {
		p.fail(key, "must be a non-negative integer, got %q", v)
		return nil
	}
	return &n
}

func (p *parser) duration(key string, def time.Duration) time.Duration {
	v := strings.TrimSpace(p.getenv(key))
	if v == "" {
		return def
	}
	d, err := time.ParseDuration(v)
	if err != nil || d <= 0 {
		p.fail(key, "must be a positive duration like 5s, got %q", v)
		return def
	}
	return d
}

func (p *parser) level(key string, def slog.Level) slog.Level {
	v := strings.TrimSpace(p.getenv(key))
	if v == "" {
		return def
	}
	var l slog.Level
	if err := l.UnmarshalText([]byte(v)); err != nil {
		p.fail(key, "must be debug, info, warn or error, got %q", v)
		return def
	}
	return l
}

// thresholds parses "0xToken=amount,0xToken=amount" with amounts in base units.
func (p *parser) thresholds(key string) map[domain.Address]*big.Int {
	out := map[domain.Address]*big.Int{}
	v := strings.TrimSpace(p.getenv(key))
	if v == "" {
		return out
	}
	for _, pair := range strings.Split(v, ",") {
		tokenStr, amountStr, ok := strings.Cut(strings.TrimSpace(pair), "=")
		if !ok {
			p.fail(key, "entry %q is not token=amount", pair)
			continue
		}
		token, err := domain.ParseAddress(strings.TrimSpace(tokenStr))
		if err != nil {
			p.fail(key, "entry %q: %v", pair, err)
			continue
		}
		amount, ok := new(big.Int).SetString(strings.TrimSpace(amountStr), 10)
		if !ok || amount.Sign() < 0 {
			p.fail(key, "entry %q: amount must be a non-negative integer in token base units", pair)
			continue
		}
		out[token] = amount
	}
	return out
}
