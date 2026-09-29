package httpapi_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/Amadeus-22/chainwatch/internal/adapter/httpapi"
	"github.com/Amadeus-22/chainwatch/internal/app"
	"github.com/Amadeus-22/chainwatch/internal/domain"
	"github.com/Amadeus-22/chainwatch/internal/platform/metrics"
)

const walletHex = "0x70997970c51812dc3a010c7d01b50e0d17dc79c8"

type fakeStore struct {
	wallets    map[domain.Address]domain.Wallet
	allowances []domain.Allowance
	alerts     []domain.Alert
	lastQuery  app.AlertQuery
	fail       error
}

func (s *fakeStore) AddWallet(_ context.Context, a domain.Address, label string) (domain.Wallet, bool, error) {
	if w, ok := s.wallets[a]; ok {
		return w, false, nil
	}
	w := domain.Wallet{Address: a, Label: label, CreatedAt: time.Unix(0, 0).UTC()}
	s.wallets[a] = w
	return w, true, nil
}

func (s *fakeStore) Wallet(_ context.Context, a domain.Address) (domain.Wallet, error) {
	if w, ok := s.wallets[a]; ok {
		return w, nil
	}
	return domain.Wallet{}, domain.ErrNotFound
}

func (s *fakeStore) RemoveWallet(_ context.Context, a domain.Address) error {
	if _, ok := s.wallets[a]; !ok {
		return domain.ErrNotFound
	}
	delete(s.wallets, a)
	return nil
}

func (s *fakeStore) Wallets(context.Context) ([]domain.Wallet, error) {
	if s.fail != nil {
		return nil, s.fail
	}
	var out []domain.Wallet
	for _, w := range s.wallets {
		out = append(out, w)
	}
	return out, nil
}

func (s *fakeStore) Allowances(context.Context, domain.Address) ([]domain.Allowance, error) {
	return s.allowances, nil
}

func (s *fakeStore) Alerts(_ context.Context, q app.AlertQuery) ([]domain.Alert, error) {
	s.lastQuery = q
	return s.alerts, nil
}

func setup(t *testing.T, checks ...httpapi.Check) (*httptest.Server, *fakeStore) {
	t.Helper()
	store := &fakeStore{wallets: map[domain.Address]domain.Wallet{}}
	reg := prometheus.NewRegistry()
	metrics.New(reg)
	h := httpapi.NewHandler(store, checks, reg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return srv, store
}

func do(t *testing.T, method, url, body string) (int, map[string]any) {
	t.Helper()
	req, _ := http.NewRequest(method, url, strings.NewReader(body))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var out map[string]any
	if len(raw) > 0 && strings.HasPrefix(resp.Header.Get("Content-Type"), "application/json") {
		if err := json.Unmarshal(raw, &out); err != nil {
			t.Fatalf("bad JSON %s", raw)
		}
	}
	return resp.StatusCode, out
}

func errCode(body map[string]any) string {
	e, _ := body["error"].(map[string]any)
	s, _ := e["code"].(string)
	return s
}

func TestWalletLifecycle(t *testing.T) {
	srv, _ := setup(t)
	checksummed := "0x70997970C51812dc3A010C7d01b50e0d17dc79C8"

	status, body := do(t, "POST", srv.URL+"/v1/wallets", `{"address":"`+checksummed+`","label":"victim"}`)
	if status != http.StatusCreated || body["address"] != walletHex || body["label"] != "victim" {
		t.Fatalf("create: %d %v", status, body)
	}
	if status, _ := do(t, "POST", srv.URL+"/v1/wallets", `{"address":"`+walletHex+`"}`); status != http.StatusOK {
		t.Fatalf("re-create: %d, want 200", status)
	}
	status, body = do(t, "GET", srv.URL+"/v1/wallets", "")
	if list, _ := body["wallets"].([]any); status != http.StatusOK || len(list) != 1 {
		t.Fatalf("list: %d %v", status, body)
	}
	if status, _ := do(t, "DELETE", srv.URL+"/v1/wallets/"+walletHex, ""); status != http.StatusNoContent {
		t.Fatalf("delete: %d", status)
	}
	if status, body := do(t, "DELETE", srv.URL+"/v1/wallets/"+walletHex, ""); status != http.StatusNotFound || errCode(body) != "not_found" {
		t.Fatalf("second delete: %d %v", status, body)
	}
}

func TestBadRequests(t *testing.T) {
	srv, _ := setup(t)
	tests := []struct {
		method, path, body string
		status             int
		code               string
	}{
		{"POST", "/v1/wallets", `{"address":"0x12"}`, 400, "invalid_address"},
		{"POST", "/v1/wallets", `not json`, 400, "invalid_body"},
		{"POST", "/v1/wallets", `{"address":"` + walletHex + `","extra":1}`, 400, "invalid_body"},
		{"POST", "/v1/wallets", `{"address":"` + walletHex + `","label":"` + strings.Repeat("x", 201) + `"}`, 400, "invalid_body"},
		{"DELETE", "/v1/wallets/nope", "", 400, "invalid_address"},
		{"GET", "/v1/wallets/nope/allowances", "", 400, "invalid_address"},
		{"GET", "/v1/wallets/" + walletHex + "/allowances", "", 404, "not_found"},
		{"GET", "/v1/alerts?wallet=0x1", "", 400, "invalid_address"},
		{"GET", "/v1/alerts?since=yesterday", "", 400, "invalid_query"},
		{"GET", "/v1/alerts?limit=0", "", 400, "invalid_query"},
		{"GET", "/v1/alerts?limit=5000", "", 400, "invalid_query"},
	}
	for _, tt := range tests {
		t.Run(tt.method+" "+tt.path, func(t *testing.T) {
			status, body := do(t, tt.method, srv.URL+tt.path, tt.body)
			if status != tt.status || errCode(body) != tt.code {
				t.Fatalf("got %d %v, want %d %s", status, body, tt.status, tt.code)
			}
		})
	}
}

func TestAllowances(t *testing.T) {
	srv, store := setup(t)
	owner, _ := domain.ParseAddress(walletHex)
	store.wallets[owner] = domain.Wallet{Address: owner}
	maxU256 := new(big.Int).Sub(new(big.Int).Lsh(big.NewInt(1), 256), big.NewInt(1))
	store.allowances = []domain.Allowance{
		{Owner: owner, Token: domain.Address{19: 1}, Spender: domain.Address{19: 2}, Amount: maxU256, UpdatedBlock: 9},
		{Owner: owner, Token: domain.Address{19: 1}, Spender: domain.Address{19: 3}, Amount: big.NewInt(5), UpdatedBlock: 4},
	}
	status, body := do(t, "GET", srv.URL+"/v1/wallets/"+walletHex+"/allowances", "")
	list, _ := body["allowances"].([]any)
	if status != http.StatusOK || len(list) != 2 {
		t.Fatalf("%d %v", status, body)
	}
	first := list[0].(map[string]any)
	if first["amount"] != maxU256.String() || first["unlimited"] != true || first["updated_block"] != float64(9) {
		t.Errorf("unlimited allowance = %v", first)
	}
	if second := list[1].(map[string]any); second["unlimited"] != false || second["amount"] != "5" {
		t.Errorf("finite allowance = %v", second)
	}
}

func TestAlertsQuery(t *testing.T) {
	srv, store := setup(t)
	store.alerts = []domain.Alert{{ID: 1, Rule: "drain_pattern", Severity: domain.SeverityCritical, Status: domain.AlertPending, TxHash: domain.Hash{1}}}
	status, body := do(t, "GET", srv.URL+"/v1/alerts?wallet="+walletHex+"&since=2026-01-02T15:04:05Z&limit=5", "")
	list, _ := body["alerts"].([]any)
	if status != http.StatusOK || len(list) != 1 || list[0].(map[string]any)["rule"] != "drain_pattern" {
		t.Fatalf("%d %v", status, body)
	}
	q := store.lastQuery
	if q.Wallet == nil || q.Wallet.String() != walletHex || q.Limit != 5 || !q.Since.Equal(time.Date(2026, 1, 2, 15, 4, 5, 0, time.UTC)) {
		t.Fatalf("query = %+v", q)
	}
	do(t, "GET", srv.URL+"/v1/alerts", "")
	if store.lastQuery.Limit != 100 || store.lastQuery.Wallet != nil {
		t.Fatalf("default query = %+v", store.lastQuery)
	}
}

func TestStoreFailureIs500WithoutDetails(t *testing.T) {
	srv, store := setup(t)
	store.fail = errors.New("connection refused to 10.0.0.5")
	status, body := do(t, "GET", srv.URL+"/v1/wallets", "")
	if status != 500 || errCode(body) != "internal" || strings.Contains(body["error"].(map[string]any)["message"].(string), "10.0.0.5") {
		t.Fatalf("%d %v", status, body)
	}
}

func TestHealthReadyMetrics(t *testing.T) {
	healthy := httpapi.Check{Name: "postgres", Fn: func(context.Context) error { return nil }}
	broken := httpapi.Check{Name: "rpc", Fn: func(context.Context) error { return errors.New("dial tcp: refused") }}

	srv, _ := setup(t, healthy)
	if status, _ := do(t, "GET", srv.URL+"/healthz", ""); status != 200 {
		t.Fatalf("healthz %d", status)
	}
	if status, body := do(t, "GET", srv.URL+"/readyz", ""); status != 200 || body["checks"].(map[string]any)["postgres"] != "ok" {
		t.Fatalf("readyz %d %v", status, body)
	}

	srv2, _ := setup(t, healthy, broken)
	status, body := do(t, "GET", srv2.URL+"/readyz", "")
	if status != http.StatusServiceUnavailable || body["checks"].(map[string]any)["rpc"] != "dial tcp: refused" {
		t.Fatalf("readyz with broken rpc %d %v", status, body)
	}

	resp, err := http.Get(srv.URL + "/metrics")
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(raw), "chainwatch_reorgs_total 0") {
		t.Fatalf("metrics output misses chainwatch_reorgs_total:\n%s", raw)
	}
}

func TestServerHasTimeouts(t *testing.T) {
	s := httpapi.NewServer(":0", http.NotFoundHandler())
	if s.ReadHeaderTimeout == 0 || s.ReadTimeout == 0 || s.WriteTimeout == 0 || s.IdleTimeout == 0 {
		t.Fatalf("missing timeouts: %+v", s)
	}
}
