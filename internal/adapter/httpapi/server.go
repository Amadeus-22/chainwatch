// Package httpapi exposes chainwatch under /v1: Sign-In with Ethereum, the
// caller's organizations, and each organization's watch list, allowances and
// alerts; plus /healthz, /readyz, /metrics and, optionally, the built web app.
//
// Authentication is a session cookie (HTTP-only, SameSite=Strict) set by
// POST /v1/auth/verify. Authorization is app.Workspace's job; this package only
// resolves the caller and maps errors to status codes.
package httpapi

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/Amadeus-22/chainwatch/internal/app"
	"github.com/Amadeus-22/chainwatch/internal/domain"
	"github.com/Amadeus-22/chainwatch/internal/domain/ethsig"
	"github.com/Amadeus-22/chainwatch/internal/domain/risk"
	"github.com/Amadeus-22/chainwatch/internal/domain/siwe"
)

// SessionCookie is the name of the session cookie.
const SessionCookie = "chainwatch_session"

// Check is a named readiness probe of one dependency.
type Check struct {
	Name string
	Fn   func(context.Context) error
}

// Config wires the handler.
type Config struct {
	Auth      *app.Auth
	Workspace *app.Workspace
	Checks    []Check
	Gatherer  prometheus.Gatherer
	Log       *slog.Logger
	// Domain is the SIWE domain (host[:port] of the web app). Unsafe requests
	// whose Origin header names another host are refused (CSRF defence in depth
	// on top of SameSite=Strict).
	Domain       string
	CookieSecure bool
	WebDir       string // optional: serve the built web app from this directory
}

const (
	defaultAlertLimit = 100
	maxAlertLimit     = 1000
	maxBodyBytes      = 1 << 20
)

type api struct {
	cfg Config
	log *slog.Logger
}

// NewHandler builds the router.
func NewHandler(cfg Config) http.Handler {
	a := &api{cfg: cfg, log: cfg.Log}
	mux := http.NewServeMux()

	mux.HandleFunc("GET /v1/auth/nonce", a.nonce)
	mux.HandleFunc("POST /v1/auth/verify", a.verify)
	mux.HandleFunc("POST /v1/auth/logout", a.logout)
	mux.HandleFunc("GET /v1/chains", a.chains)
	mux.HandleFunc("GET /v1/me", a.authed(a.me))
	mux.HandleFunc("POST /v1/orgs", a.authed(a.createOrg))
	mux.HandleFunc("GET /v1/orgs/{org}/members", a.authed(a.members))
	mux.HandleFunc("POST /v1/orgs/{org}/members", a.authed(a.addMember))
	mux.HandleFunc("DELETE /v1/orgs/{org}/members/{address}", a.authed(a.removeMember))
	mux.HandleFunc("GET /v1/orgs/{org}/wallets", a.authed(a.listWallets))
	mux.HandleFunc("POST /v1/orgs/{org}/wallets", a.authed(a.addWallet))
	mux.HandleFunc("DELETE /v1/orgs/{org}/wallets/{chain}/{address}", a.authed(a.removeWallet))
	mux.HandleFunc("GET /v1/orgs/{org}/allowances", a.authed(a.allowances))
	mux.HandleFunc("GET /v1/orgs/{org}/alerts", a.authed(a.alerts))
	mux.HandleFunc("/v1/", func(w http.ResponseWriter, _ *http.Request) {
		writeError(w, http.StatusNotFound, "not_found", "no such endpoint")
	})

	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	mux.HandleFunc("GET /readyz", a.ready)
	mux.Handle("GET /metrics", promhttp.HandlerFor(cfg.Gatherer, promhttp.HandlerOpts{}))
	if cfg.WebDir != "" {
		mux.Handle("/", spa(cfg.WebDir))
	}
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

// --- authentication ---

func (a *api) nonce(w http.ResponseWriter, r *http.Request) {
	n, exp, err := a.cfg.Auth.NewNonce(r.Context())
	if err != nil {
		a.fail(w, r, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, map[string]any{"nonce": n, "expires_at": exp})
}

func (a *api) verify(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Message   string `json:"message"`
		Signature string `json:"signature"`
	}
	if !decode(w, r, &body, `{"message":"<EIP-4361 text>","signature":"0x<65 bytes>"}`) {
		return
	}
	sig, err := hex.DecodeString(strings.TrimPrefix(body.Signature, "0x"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_signature", "signature must be 0x-prefixed hex")
		return
	}
	token, s, err := a.cfg.Auth.SignIn(r.Context(), body.Message, sig)
	switch {
	case errors.Is(err, siwe.ErrInvalidMessage):
		writeError(w, http.StatusUnauthorized, "invalid_message", err.Error())
		return
	case errors.Is(err, ethsig.ErrInvalidSignature):
		writeError(w, http.StatusUnauthorized, "invalid_signature", err.Error())
		return
	case errors.Is(err, app.ErrBadNonce):
		writeError(w, http.StatusUnauthorized, "invalid_nonce", err.Error()+"; request a new one from /v1/auth/nonce")
		return
	case err != nil:
		a.fail(w, r, err)
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name: SessionCookie, Value: token, Path: "/", Expires: s.ExpiresAt,
		HttpOnly: true, Secure: a.cfg.CookieSecure, SameSite: http.SameSiteStrictMode,
	})
	writeJSON(w, http.StatusOK, map[string]any{"address": ethsig.Checksum(s.Address), "expires_at": s.ExpiresAt})
}

func (a *api) logout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(SessionCookie); err == nil {
		if err := a.cfg.Auth.SignOut(r.Context(), c.Value); err != nil {
			a.fail(w, r, err)
			return
		}
	}
	http.SetCookie(w, &http.Cookie{
		Name: SessionCookie, Value: "", Path: "/", MaxAge: -1,
		HttpOnly: true, Secure: a.cfg.CookieSecure, SameSite: http.SameSiteStrictMode,
	})
	w.WriteHeader(http.StatusNoContent)
}

type authedHandler func(http.ResponseWriter, *http.Request, domain.Principal)

// authed resolves the session cookie to a principal or answers 401.
func (a *api) authed(h authedHandler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		token := ""
		if c, err := r.Cookie(SessionCookie); err == nil {
			token = c.Value
		}
		p, err := a.cfg.Auth.Authenticate(r.Context(), token)
		if err != nil {
			a.fail(w, r, err)
			return
		}
		h(w, r, p)
	}
}

// --- account and organizations ---

type orgJSON struct {
	ID        domain.OrgID `json:"id"`
	Name      string       `json:"name"`
	Role      domain.Role  `json:"role,omitempty"`
	CreatedAt time.Time    `json:"created_at"`
}

func (a *api) me(w http.ResponseWriter, r *http.Request, p domain.Principal) {
	ms, err := a.cfg.Workspace.Memberships(r.Context(), p)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	orgs := make([]orgJSON, len(ms))
	for i, m := range ms {
		orgs[i] = orgJSON{ID: m.Org.ID, Name: m.Org.Name, Role: m.Role, CreatedAt: m.Org.CreatedAt}
	}
	writeJSON(w, http.StatusOK, map[string]any{"address": ethsig.Checksum(p.Address), "organizations": orgs})
}

func (a *api) createOrg(w http.ResponseWriter, r *http.Request, p domain.Principal) {
	var body struct {
		Name string `json:"name"`
	}
	if !decode(w, r, &body, `{"name":"..."}`) {
		return
	}
	o, err := a.cfg.Workspace.CreateOrganization(r.Context(), p, body.Name)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, orgJSON{ID: o.ID, Name: o.Name, Role: domain.RoleOwner, CreatedAt: o.CreatedAt})
}

type memberJSON struct {
	Address   domain.Address `json:"address"`
	Role      domain.Role    `json:"role"`
	CreatedAt time.Time      `json:"created_at"`
}

func (a *api) members(w http.ResponseWriter, r *http.Request, p domain.Principal) {
	org, ok := pathOrg(w, r)
	if !ok {
		return
	}
	ms, err := a.cfg.Workspace.Members(r.Context(), p, org)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	out := make([]memberJSON, len(ms))
	for i, m := range ms {
		out[i] = memberJSON(m)
	}
	writeJSON(w, http.StatusOK, map[string]any{"members": out})
}

func (a *api) addMember(w http.ResponseWriter, r *http.Request, p domain.Principal) {
	org, ok := pathOrg(w, r)
	if !ok {
		return
	}
	var body struct {
		Address string `json:"address"`
		Role    string `json:"role"`
	}
	if !decode(w, r, &body, `{"address":"0x...","role":"member|owner"}`) {
		return
	}
	addr, err := domain.ParseAddress(body.Address)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	if body.Role == "" {
		body.Role = string(domain.RoleMember)
	}
	role, err := domain.ParseRole(body.Role)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	m, created, err := a.cfg.Workspace.AddMember(r.Context(), p, org, addr, role)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	writeJSON(w, createdStatus(created), memberJSON(m))
}

func (a *api) removeMember(w http.ResponseWriter, r *http.Request, p domain.Principal) {
	org, ok := pathOrg(w, r)
	if !ok {
		return
	}
	addr, err := domain.ParseAddress(r.PathValue("address"))
	if err != nil {
		a.fail(w, r, err)
		return
	}
	if err := a.cfg.Workspace.RemoveMember(r.Context(), p, org, addr); err != nil {
		a.fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// --- chains and wallets ---

type chainJSON struct {
	ID          domain.ChainID `json:"id"`
	Name        string         `json:"name"`
	ExplorerURL string         `json:"explorer_url,omitempty"`
}

func (a *api) chains(w http.ResponseWriter, _ *http.Request) {
	cs := a.cfg.Workspace.Chains()
	out := make([]chainJSON, len(cs))
	for i, c := range cs {
		out[i] = chainJSON(c)
	}
	writeJSON(w, http.StatusOK, map[string]any{"chains": out})
}

type walletJSON struct {
	ChainID   domain.ChainID `json:"chain_id"`
	Address   domain.Address `json:"address"`
	Label     string         `json:"label"`
	CreatedAt time.Time      `json:"created_at"`
}

func toWalletJSON(w domain.Wallet) walletJSON {
	return walletJSON{ChainID: w.ChainID, Address: w.Address, Label: w.Label, CreatedAt: w.CreatedAt}
}

func (a *api) addWallet(w http.ResponseWriter, r *http.Request, p domain.Principal) {
	org, ok := pathOrg(w, r)
	if !ok {
		return
	}
	var body struct {
		ChainID uint64 `json:"chain_id"`
		Address string `json:"address"`
		Label   string `json:"label"`
	}
	if !decode(w, r, &body, `{"chain_id":1,"address":"0x...","label":"..."}`) {
		return
	}
	addr, err := domain.ParseAddress(body.Address)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	wallet, created, err := a.cfg.Workspace.AddWallet(r.Context(), p, org, domain.ChainID(body.ChainID), addr, body.Label)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	writeJSON(w, createdStatus(created), toWalletJSON(wallet))
}

func (a *api) listWallets(w http.ResponseWriter, r *http.Request, p domain.Principal) {
	org, ok := pathOrg(w, r)
	if !ok {
		return
	}
	wallets, err := a.cfg.Workspace.Wallets(r.Context(), p, org)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	out := make([]walletJSON, len(wallets))
	for i, wl := range wallets {
		out[i] = toWalletJSON(wl)
	}
	writeJSON(w, http.StatusOK, map[string]any{"wallets": out})
}

func (a *api) removeWallet(w http.ResponseWriter, r *http.Request, p domain.Principal) {
	org, ok := pathOrg(w, r)
	if !ok {
		return
	}
	chain, err := strconv.ParseUint(r.PathValue("chain"), 10, 64)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_input", "chain must be a chain id like 1 or 8453")
		return
	}
	addr, err := domain.ParseAddress(r.PathValue("address"))
	if err != nil {
		a.fail(w, r, err)
		return
	}
	if err := a.cfg.Workspace.RemoveWallet(r.Context(), p, org, domain.ChainID(chain), addr); err != nil {
		a.fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// --- allowances and alerts ---

type allowanceJSON struct {
	ChainID      domain.ChainID  `json:"chain_id"`
	Owner        domain.Address  `json:"owner"`
	Token        domain.Address  `json:"token"`
	Spender      domain.Address  `json:"spender"`
	Amount       string          `json:"amount"` // base units, decimal string (uint256 does not fit JSON numbers)
	Unlimited    bool            `json:"unlimited"`
	Risk         domain.Severity `json:"risk"`
	UpdatedBlock uint64          `json:"updated_block"`
}

func (a *api) allowances(w http.ResponseWriter, r *http.Request, p domain.Principal) {
	org, ok := pathOrg(w, r)
	if !ok {
		return
	}
	q := app.AllowanceQuery{Org: org}
	var err error
	if q.ChainID, q.Owner, err = chainAndWallet(r.URL.Query()); err != nil {
		a.fail(w, r, err)
		return
	}
	list, err := a.cfg.Workspace.Allowances(r.Context(), p, q)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	out := make([]allowanceJSON, len(list))
	for i, al := range list {
		out[i] = allowanceJSON{
			ChainID: al.ChainID, Owner: al.Owner, Token: al.Token, Spender: al.Spender, Amount: al.Amount.String(),
			Unlimited: al.Amount.Cmp(risk.UnlimitedThreshold) >= 0, Risk: risk.AllowanceRisk(al.Amount),
			UpdatedBlock: al.UpdatedBlock,
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"allowances": out})
}

type alertJSON struct {
	ID          int64              `json:"id"`
	ChainID     domain.ChainID     `json:"chain_id"`
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

func (a *api) alerts(w http.ResponseWriter, r *http.Request, p domain.Principal) {
	org, ok := pathOrg(w, r)
	if !ok {
		return
	}
	q := app.AlertQuery{Org: org, Limit: defaultAlertLimit}
	params := r.URL.Query()
	var err error
	if q.ChainID, q.Wallet, err = chainAndWallet(params); err != nil {
		a.fail(w, r, err)
		return
	}
	if v := params.Get("since"); v != "" {
		t, err := time.Parse(time.RFC3339, v)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid_input", "since must be an RFC 3339 timestamp like 2026-01-02T15:04:05Z")
			return
		}
		q.Since = t
	}
	if v := params.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > maxAlertLimit {
			writeError(w, http.StatusBadRequest, "invalid_input", "limit must be an integer between 1 and 1000")
			return
		}
		q.Limit = n
	}
	list, err := a.cfg.Workspace.Alerts(r.Context(), p, q)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	out := make([]alertJSON, len(list))
	for i, al := range list {
		out[i] = alertJSON{
			ID: al.ID, ChainID: al.ChainID, Wallet: al.Wallet, Rule: al.Rule, Severity: al.Severity, Reason: al.Reason,
			Token: al.Token, TxHash: al.TxHash, LogIndex: al.LogIndex, BlockNumber: al.BlockNumber, Status: al.Status,
			Attempts: al.Attempts, CreatedAt: al.CreatedAt, DeliveredAt: al.DeliveredAt,
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"alerts": out})
}

func (a *api) ready(w http.ResponseWriter, r *http.Request) {
	results := map[string]string{}
	status := http.StatusOK
	for _, c := range a.cfg.Checks {
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

func pathOrg(w http.ResponseWriter, r *http.Request) (domain.OrgID, bool) {
	id, err := strconv.ParseInt(r.PathValue("org"), 10, 64)
	if err != nil || id <= 0 {
		writeError(w, http.StatusBadRequest, "invalid_input", "organization id must be a positive integer")
		return 0, false
	}
	return domain.OrgID(id), true
}

// chainAndWallet reads the optional chain_id and wallet query filters.
func chainAndWallet(q url.Values) (*domain.ChainID, *domain.Address, error) {
	var chain *domain.ChainID
	var wallet *domain.Address
	if v := q.Get("chain_id"); v != "" {
		n, err := strconv.ParseUint(v, 10, 64)
		if err != nil {
			return nil, nil, fmt.Errorf("%w: chain_id must be a chain id like 1 or 8453", domain.ErrInvalidInput)
		}
		c := domain.ChainID(n)
		chain = &c
	}
	if v := q.Get("wallet"); v != "" {
		a, err := domain.ParseAddress(v)
		if err != nil {
			return nil, nil, err
		}
		wallet = &a
	}
	return chain, wallet, nil
}

func decode(w http.ResponseWriter, r *http.Request, v any, shape string) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_body", "body must be JSON like "+shape+": "+err.Error())
		return false
	}
	return true
}

func createdStatus(created bool) int {
	if created {
		return http.StatusCreated
	}
	return http.StatusOK
}

// fail maps domain errors to status codes; anything else is a logged 500.
func (a *api) fail(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, domain.ErrUnauthenticated):
		writeError(w, http.StatusUnauthorized, "unauthenticated", "sign in with Ethereum first (POST /v1/auth/verify)")
	case errors.Is(err, domain.ErrForbidden):
		writeError(w, http.StatusForbidden, "forbidden", err.Error())
	case errors.Is(err, domain.ErrNotFound):
		writeError(w, http.StatusNotFound, "not_found", err.Error())
	case errors.Is(err, domain.ErrInvalidAddress):
		writeError(w, http.StatusBadRequest, "invalid_address", err.Error())
	case errors.Is(err, domain.ErrInvalidInput):
		writeError(w, http.StatusBadRequest, "invalid_input", err.Error())
	case errors.Is(err, domain.ErrConflict):
		writeError(w, http.StatusConflict, "conflict", err.Error())
	default:
		a.log.Error("request failed", "method", r.Method, "path", r.URL.Path, "err", err)
		writeError(w, http.StatusInternalServerError, "internal", "internal error")
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, map[string]any{"error": map[string]string{"code": code, "message": msg}})
}

// spa serves the built single-page app: files that exist, index.html otherwise.
func spa(dir string) http.Handler {
	files := http.FileServer(http.Dir(dir))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "the web app only serves GET")
			return
		}
		clean := filepath.Clean("/" + r.URL.Path)
		if st, err := os.Stat(filepath.Join(dir, filepath.FromSlash(clean))); err != nil || st.IsDir() {
			http.ServeFile(w, r, filepath.Join(dir, "index.html"))
			return
		}
		files.ServeHTTP(w, r)
	})
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (s *statusRecorder) WriteHeader(code int) {
	s.status = code
	s.ResponseWriter.WriteHeader(code)
}

func unsafeMethod(m string) bool {
	return m != http.MethodGet && m != http.MethodHead && m != http.MethodOptions
}

// middleware logs each request, refuses cross-origin unsafe requests and turns
// panics into 500s.
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
		rec.Header().Set("X-Content-Type-Options", "nosniff")
		if origin := r.Header.Get("Origin"); origin != "" && unsafeMethod(r.Method) {
			if u, err := url.Parse(origin); err != nil || u.Host != a.cfg.Domain {
				writeError(rec, http.StatusForbidden, "cross_origin", "requests from origin "+origin+" are not allowed")
				return
			}
		}
		next.ServeHTTP(rec, r)
	})
}
