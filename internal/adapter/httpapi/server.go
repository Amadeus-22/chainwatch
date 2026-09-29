// Package httpapi exposes the watch list, allowances and alerts under /v1, plus
// /healthz, /readyz and /metrics.
package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/Amadeus-22/chainwatch/internal/app"
	"github.com/Amadeus-22/chainwatch/internal/domain"
	"github.com/Amadeus-22/chainwatch/internal/domain/risk"
)

// Store is what the API reads and writes. The Postgres adapter implements it.
type Store interface {
	AddWallet(ctx context.Context, addr domain.Address, label string) (domain.Wallet, bool, error)
	Wallet(ctx context.Context, addr domain.Address) (domain.Wallet, error)
	RemoveWallet(ctx context.Context, addr domain.Address) error
	Wallets(ctx context.Context) ([]domain.Wallet, error)
	Allowances(ctx context.Context, owner domain.Address) ([]domain.Allowance, error)
	Alerts(ctx context.Context, q app.AlertQuery) ([]domain.Alert, error)
}

// Check is a named readiness probe of one dependency.
type Check struct {
	Name string
	Fn   func(context.Context) error
}

const (
	defaultAlertLimit = 100
	maxAlertLimit     = 1000
	maxBodyBytes      = 1 << 20
)

type api struct {
	store  Store
	checks []Check
	log    *slog.Logger
}

// NewHandler builds the router.
func NewHandler(store Store, checks []Check, gatherer prometheus.Gatherer, log *slog.Logger) http.Handler {
	a := &api{store: store, checks: checks, log: log}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/wallets", a.addWallet)
	mux.HandleFunc("GET /v1/wallets", a.listWallets)
	mux.HandleFunc("DELETE /v1/wallets/{address}", a.removeWallet)
	mux.HandleFunc("GET /v1/wallets/{address}/allowances", a.allowances)
	mux.HandleFunc("GET /v1/alerts", a.alerts)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	mux.HandleFunc("GET /readyz", a.ready)
	mux.Handle("GET /metrics", promhttp.HandlerFor(gatherer, promhttp.HandlerOpts{}))
	return a.middleware(mux)
}

// NewServer returns an http.Server with explicit timeouts.
func NewServer(addr string, h http.Handler) *http.Server {
	return &http.Server{
		Addr:              addr,
		Handler:           h,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      15 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
}

// --- handlers ---

type walletJSON struct {
	Address   domain.Address `json:"address"`
	Label     string         `json:"label"`
	CreatedAt time.Time      `json:"created_at"`
}

func (a *api) addWallet(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Address string `json:"address"`
		Label   string `json:"label"`
	}
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_body", "body must be JSON like {\"address\":\"0x...\",\"label\":\"...\"}: "+err.Error())
		return
	}
	addr, err := domain.ParseAddress(body.Address)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_address", err.Error())
		return
	}
	if len(body.Label) > 200 {
		writeError(w, http.StatusBadRequest, "invalid_body", "label must be at most 200 bytes")
		return
	}
	wallet, created, err := a.store.AddWallet(r.Context(), addr, body.Label)
	if err != nil {
		a.internal(w, r, err)
		return
	}
	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	writeJSON(w, status, walletJSON(wallet))
}

func (a *api) listWallets(w http.ResponseWriter, r *http.Request) {
	wallets, err := a.store.Wallets(r.Context())
	if err != nil {
		a.internal(w, r, err)
		return
	}
	out := make([]walletJSON, len(wallets))
	for i, wl := range wallets {
		out[i] = walletJSON(wl)
	}
	writeJSON(w, http.StatusOK, map[string]any{"wallets": out})
}

func (a *api) removeWallet(w http.ResponseWriter, r *http.Request) {
	addr, ok := pathAddress(w, r)
	if !ok {
		return
	}
	if err := a.store.RemoveWallet(r.Context(), addr); err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			writeError(w, http.StatusNotFound, "not_found", "wallet "+addr.String()+" is not watched")
			return
		}
		a.internal(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

type allowanceJSON struct {
	Token        domain.Address `json:"token"`
	Spender      domain.Address `json:"spender"`
	Amount       string         `json:"amount"` // base units, decimal string (uint256 does not fit JSON numbers)
	Unlimited    bool           `json:"unlimited"`
	UpdatedBlock uint64         `json:"updated_block"`
}

func (a *api) allowances(w http.ResponseWriter, r *http.Request) {
	addr, ok := pathAddress(w, r)
	if !ok {
		return
	}
	if _, err := a.store.Wallet(r.Context(), addr); err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			writeError(w, http.StatusNotFound, "not_found", "wallet "+addr.String()+" is not watched")
			return
		}
		a.internal(w, r, err)
		return
	}
	list, err := a.store.Allowances(r.Context(), addr)
	if err != nil {
		a.internal(w, r, err)
		return
	}
	out := make([]allowanceJSON, len(list))
	for i, al := range list {
		out[i] = allowanceJSON{
			Token: al.Token, Spender: al.Spender, Amount: al.Amount.String(),
			Unlimited: al.Amount.Cmp(risk.UnlimitedThreshold) >= 0, UpdatedBlock: al.UpdatedBlock,
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"wallet": addr, "allowances": out})
}

type alertJSON struct {
	ID          int64              `json:"id"`
	Wallet      domain.Address     `json:"wallet"`
	Rule        string             `json:"rule"`
	Severity    domain.Severity    `json:"severity"`
	Reason      string             `json:"reason"`
	Token       domain.Address     `json:"token"`
	TxHash      domain.Hash        `json:"tx_hash"`
	LogIndex    uint               `json:"log_index"`
	BlockNumber uint64             `json:"block_number"`
	Status      domain.AlertStatus `json:"status"`
	Attempts    int                `json:"attempts"`
	CreatedAt   time.Time          `json:"created_at"`
	DeliveredAt *time.Time         `json:"delivered_at"`
}

func (a *api) alerts(w http.ResponseWriter, r *http.Request) {
	q := app.AlertQuery{Limit: defaultAlertLimit}
	params := r.URL.Query()
	if v := params.Get("wallet"); v != "" {
		addr, err := domain.ParseAddress(v)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid_address", err.Error())
			return
		}
		q.Wallet = &addr
	}
	if v := params.Get("since"); v != "" {
		t, err := time.Parse(time.RFC3339, v)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid_query", "since must be an RFC 3339 timestamp like 2026-01-02T15:04:05Z")
			return
		}
		q.Since = t
	}
	if v := params.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > maxAlertLimit {
			writeError(w, http.StatusBadRequest, "invalid_query", "limit must be an integer between 1 and 1000")
			return
		}
		q.Limit = n
	}
	list, err := a.store.Alerts(r.Context(), q)
	if err != nil {
		a.internal(w, r, err)
		return
	}
	out := make([]alertJSON, len(list))
	for i, al := range list {
		out[i] = alertJSON{
			ID: al.ID, Wallet: al.Wallet, Rule: al.Rule, Severity: al.Severity, Reason: al.Reason, Token: al.Token,
			TxHash: al.TxHash, LogIndex: al.LogIndex, BlockNumber: al.BlockNumber, Status: al.Status,
			Attempts: al.Attempts, CreatedAt: al.CreatedAt, DeliveredAt: al.DeliveredAt,
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"alerts": out})
}

func (a *api) ready(w http.ResponseWriter, r *http.Request) {
	results := map[string]string{}
	status := http.StatusOK
	for _, c := range a.checks {
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		err := c.Fn(ctx)
		cancel()
		if err != nil {
			results[c.Name] = err.Error()
			status = http.StatusServiceUnavailable
			continue
		}
		results[c.Name] = "ok"
	}
	writeJSON(w, status, map[string]any{"checks": results})
}

// --- helpers ---

func pathAddress(w http.ResponseWriter, r *http.Request) (domain.Address, bool) {
	addr, err := domain.ParseAddress(r.PathValue("address"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_address", err.Error())
		return domain.Address{}, false
	}
	return addr, true
}

func (a *api) internal(w http.ResponseWriter, r *http.Request, err error) {
	a.log.Error("request failed", "method", r.Method, "path", r.URL.Path, "err", err)
	writeError(w, http.StatusInternalServerError, "internal", "internal error")
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, map[string]any{"error": map[string]string{"code": code, "message": msg}})
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (s *statusRecorder) WriteHeader(code int) {
	s.status = code
	s.ResponseWriter.WriteHeader(code)
}

// middleware logs each request and turns panics into 500s.
func (a *api) middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		defer func() {
			if p := recover(); p != nil {
				a.log.Error("panic in handler", "method", r.Method, "path", r.URL.Path, "panic", p)
				writeError(rec, http.StatusInternalServerError, "internal", "internal error")
			}
			level := slog.LevelDebug
			if rec.status >= 500 {
				level = slog.LevelWarn
			}
			a.log.Log(r.Context(), level, "http request", "method", r.Method, "path", r.URL.Path,
				"status", rec.status, "duration_ms", time.Since(start).Milliseconds())
		}()
		next.ServeHTTP(rec, r)
	})
}
