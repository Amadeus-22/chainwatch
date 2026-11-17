// Package config reads chainwatch's configuration once, at startup: process
// settings from environment variables and, for several chains, the network list
// from a YAML file decoded strictly. Every variable is documented in the README.
package config

import (
	"bytes"
	"errors"
	"fmt"
	"log/slog"
	"math/big"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/Amadeus-22/chainwatch/internal/domain"
)

// Config is the validated process configuration.
type Config struct {
	Chains      []Chain
	DatabaseURL string
	HTTPAddr    string
	LogLevel    slog.Level
	RPCTimeout  time.Duration
	DrainWindow uint64

	// SIWEDomain is the host[:port] of the web app; sign-in messages must name it.
	SIWEDomain   string
	SessionTTL   time.Duration
	CookieSecure bool
	WebDir       string // optional: serve the built web app (web/dist) from here

	NotifyInterval   time.Duration
	WebhookURL       string
	TelegramBotToken string
	TelegramChatID   string

	ShutdownTimeout time.Duration
}

// Chain is one EVM network to index.
type Chain struct {
	ID            domain.ChainID
	Name          string
	RPCURL        string
	ExplorerURL   string
	Confirmations uint64
	ReorgWindow   uint64
	MaxBlockRange uint64
	StartBlock    *uint64 // nil: start at the confirmed head on first run
	PollInterval  time.Duration
	LargeOutflow  map[domain.Address]*big.Int
}

// Load parses and validates the configuration. getenv is os.Getenv in production
// and a map lookup in tests; readFile reads CHAINWATCH_CHAINS_FILE. All problems
// are reported at once.
func Load(getenv func(string) string, readFile func(string) ([]byte, error)) (Config, error) {
	p := parser{getenv: getenv}
	c := Config{
		DatabaseURL: p.str("CHAINWATCH_DATABASE_URL", "", true),
		HTTPAddr:    p.str("CHAINWATCH_HTTP_ADDR", ":8080", false),
		LogLevel:    p.level("CHAINWATCH_LOG_LEVEL", slog.LevelInfo),
		RPCTimeout:  p.duration("CHAINWATCH_RPC_TIMEOUT", 10*time.Second),
		DrainWindow: p.uint("CHAINWATCH_DRAIN_WINDOW_BLOCKS", 7200),

		SIWEDomain:   p.str("CHAINWATCH_SIWE_DOMAIN", "", true),
		SessionTTL:   p.duration("CHAINWATCH_SESSION_TTL", 24*time.Hour),
		CookieSecure: p.bool("CHAINWATCH_COOKIE_SECURE", true),
		WebDir:       p.str("CHAINWATCH_WEB_DIR", "", false),

		NotifyInterval:   p.duration("CHAINWATCH_NOTIFY_INTERVAL", 2*time.Second),
		WebhookURL:       p.url("CHAINWATCH_WEBHOOK_URL", "", false),
		TelegramBotToken: p.str("CHAINWATCH_TELEGRAM_BOT_TOKEN", "", false),
		TelegramChatID:   p.str("CHAINWATCH_TELEGRAM_CHAT_ID", "", false),

		ShutdownTimeout: p.duration("CHAINWATCH_SHUTDOWN_TIMEOUT", 15*time.Second),
	}
	if strings.Contains(c.SIWEDomain, "/") {
		p.fail("CHAINWATCH_SIWE_DOMAIN", "must be host[:port] without a scheme, got %q", c.SIWEDomain)
	}
	if (c.TelegramBotToken == "") != (c.TelegramChatID == "") {
		p.fail("CHAINWATCH_TELEGRAM_CHAT_ID", "CHAINWATCH_TELEGRAM_BOT_TOKEN and CHAINWATCH_TELEGRAM_CHAT_ID must be set together")
	}
	if c.WebhookURL != "" && c.TelegramBotToken != "" {
		p.fail("CHAINWATCH_WEBHOOK_URL", "configure either a webhook or Telegram, not both")
	}
	c.Chains = p.chains(readFile)
	if err := errors.Join(p.errs...); err != nil {
		return Config{}, fmt.Errorf("load config: %w", err)
	}
	return c, nil
}

// defaults are the per-chain settings given by environment variables; a chain in
// the YAML file overrides each one it sets.
func (p *parser) defaults() Chain {
	d := Chain{
		Confirmations: p.uint("CHAINWATCH_CONFIRMATIONS", 12),
		ReorgWindow:   p.uint("CHAINWATCH_REORG_WINDOW", 64),
		MaxBlockRange: p.uint("CHAINWATCH_MAX_BLOCK_RANGE", 2000),
		StartBlock:    p.optionalUint("CHAINWATCH_START_BLOCK"),
		PollInterval:  p.duration("CHAINWATCH_POLL_INTERVAL", 6*time.Second),
		LargeOutflow:  p.thresholds("CHAINWATCH_LARGE_OUTFLOW", p.getenv("CHAINWATCH_LARGE_OUTFLOW")),
	}
	if d.MaxBlockRange == 0 {
		p.fail("CHAINWATCH_MAX_BLOCK_RANGE", "must be at least 1")
	}
	return d
}

// chainsFile is the YAML schema of CHAINWATCH_CHAINS_FILE.
type chainsFile struct {
	Chains []struct {
		ID            uint64            `yaml:"id"`
		Name          string            `yaml:"name"`
		RPCURL        string            `yaml:"rpc_url"`
		ExplorerURL   string            `yaml:"explorer_url"`
		Confirmations *uint64           `yaml:"confirmations"`
		ReorgWindow   *uint64           `yaml:"reorg_window"`
		MaxBlockRange *uint64           `yaml:"max_block_range"`
		StartBlock    *uint64           `yaml:"start_block"`
		PollInterval  string            `yaml:"poll_interval"`
		LargeOutflow  map[string]string `yaml:"large_outflow"`
	} `yaml:"chains"`
}

// chains reads the network list: from CHAINWATCH_CHAINS_FILE, or a single chain
// from CHAINWATCH_RPC_URL and CHAINWATCH_CHAIN_ID.
func (p *parser) chains(readFile func(string) ([]byte, error)) []Chain {
	def := p.defaults()
	file := p.str("CHAINWATCH_CHAINS_FILE", "", false)
	rpc := p.str("CHAINWATCH_RPC_URL", "", false)
	switch {
	case file != "" && rpc != "":
		p.fail("CHAINWATCH_CHAINS_FILE", "set either CHAINWATCH_CHAINS_FILE or CHAINWATCH_RPC_URL, not both")
		return nil
	case file == "" && rpc == "":
		p.fail("CHAINWATCH_CHAINS_FILE", "is required (or CHAINWATCH_RPC_URL with CHAINWATCH_CHAIN_ID for a single chain)")
		return nil
	case rpc != "":
		c := def
		c.RPCURL = p.url("CHAINWATCH_RPC_URL", "", true)
		id := p.optionalUint("CHAINWATCH_CHAIN_ID")
		if id == nil || *id == 0 {
			p.fail("CHAINWATCH_CHAIN_ID", "is required with CHAINWATCH_RPC_URL and must be a positive integer")
			return nil
		}
		c.ID = domain.ChainID(*id)
		c.Name = p.str("CHAINWATCH_CHAIN_NAME", "Chain "+strconv.FormatUint(*id, 10), false)
		c.ExplorerURL = p.url("CHAINWATCH_EXPLORER_URL", "", false)
		return []Chain{c}
	}

	const key = "CHAINWATCH_CHAINS_FILE"
	raw, err := readFile(file)
	if err != nil {
		p.fail(key, "read %s: %v", file, err)
		return nil
	}
	var f chainsFile
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	dec.KnownFields(true)
	if err := dec.Decode(&f); err != nil {
		p.fail(key, "decode %s: %v", file, err)
		return nil
	}
	if len(f.Chains) == 0 {
		p.fail(key, "%s lists no chains", file)
		return nil
	}
	seen := map[uint64]bool{}
	out := make([]Chain, 0, len(f.Chains))
	for i, fc := range f.Chains {
		where := fmt.Sprintf("%s: chains[%d]", file, i)
		c := def
		c.ID, c.Name, c.ExplorerURL = domain.ChainID(fc.ID), strings.TrimSpace(fc.Name), fc.ExplorerURL
		if fc.ID == 0 {
			p.fail(key, "%s: id must be a positive EIP-155 chain id", where)
		} else if seen[fc.ID] {
			p.fail(key, "%s: chain id %d is listed twice", where, fc.ID)
		}
		seen[fc.ID] = true
		if c.Name == "" {
			p.fail(key, "%s: name is required", where)
		}
		// ${VAR} in rpc_url is expanded so API keys can stay in the environment.
		c.RPCURL = os.Expand(fc.RPCURL, p.getenv)
		if !isHTTPURL(c.RPCURL) {
			p.fail(key, "%s: rpc_url must be an http(s) URL", where)
		}
		if c.ExplorerURL != "" && !isHTTPURL(c.ExplorerURL) {
			p.fail(key, "%s: explorer_url must be an http(s) URL", where)
		}
		if fc.Confirmations != nil {
			c.Confirmations = *fc.Confirmations
		}
		if fc.ReorgWindow != nil {
			c.ReorgWindow = *fc.ReorgWindow
		}
		if fc.MaxBlockRange != nil {
			c.MaxBlockRange = *fc.MaxBlockRange
			if c.MaxBlockRange == 0 {
				p.fail(key, "%s: max_block_range must be at least 1", where)
			}
		}
		if fc.StartBlock != nil {
			c.StartBlock = fc.StartBlock
		}
		if fc.PollInterval != "" {
			d, err := time.ParseDuration(fc.PollInterval)
			if err != nil || d <= 0 {
				p.fail(key, "%s: poll_interval must be a positive duration like 12s", where)
			}
			c.PollInterval = d
		}
		if fc.LargeOutflow != nil {
			pairs := make([]string, 0, len(fc.LargeOutflow))
			for token, amount := range fc.LargeOutflow {
				pairs = append(pairs, token+"="+amount)
			}
			c.LargeOutflow = p.thresholds(key+": "+where+": large_outflow", strings.Join(pairs, ","))
		}
		out = append(out, c)
	}
	return out
}

func isHTTPURL(v string) bool {
	u, err := url.Parse(v)
	return err == nil && (u.Scheme == "http" || u.Scheme == "https") && u.Host != ""
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
	if !isHTTPURL(v) {
		p.fail(key, "must be an http(s) URL, got %q", v)
	}
	return v
}

func (p *parser) bool(key string, def bool) bool {
	v := strings.TrimSpace(p.getenv(key))
	if v == "" {
		return def
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		p.fail(key, "must be true or false, got %q", v)
		return def
	}
	return b
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
func (p *parser) thresholds(key, v string) map[domain.Address]*big.Int {
	out := map[domain.Address]*big.Int{}
	v = strings.TrimSpace(v)
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
