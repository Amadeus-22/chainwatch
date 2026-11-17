package httpapi_test

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"math/big"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/decred/dcrd/dcrec/secp256k1/v4"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/Amadeus-22/chainwatch/internal/adapter/httpapi"
	"github.com/Amadeus-22/chainwatch/internal/app"
	"github.com/Amadeus-22/chainwatch/internal/app/apptest"
	"github.com/Amadeus-22/chainwatch/internal/domain"
	"github.com/Amadeus-22/chainwatch/internal/domain/ethsig"
	"github.com/Amadeus-22/chainwatch/internal/domain/siwe"
	"github.com/Amadeus-22/chainwatch/internal/platform/metrics"
)

const (
	appDomain = "app.chainwatch.test"
	walletHex = "0x70997970c51812dc3a010c7d01b50e0d17dc79c8"
)

// failingStore breaks one query to check internal errors do not leak.
type failingStore struct {
	*apptest.Accounts
	fail error
}

func (f *failingStore) OrgWallets(ctx context.Context, org domain.OrgID) ([]domain.Wallet, error) {
	if f.fail != nil {
		return nil, f.fail
	}
	return f.Accounts.OrgWallets(ctx, org)
}

type env struct {
	srv   *httptest.Server
	store *failingStore
}

func setup(t *testing.T, mutate func(*httpapi.Config)) *env {
	t.Helper()
	store := &failingStore{Accounts: apptest.NewAccounts()}
	reg := prometheus.NewRegistry()
	metrics.New(reg)
	chains := []domain.Chain{{ID: 1, Name: "Ethereum", ExplorerURL: "https://etherscan.io"}, {ID: 8453, Name: "Base"}}
	cfg := httpapi.Config{
		Auth: app.NewAuth(store, store, app.AuthConfig{Domain: appDomain, ChainIDs: []domain.ChainID{1, 8453},
			NonceTTL: time.Minute, SessionTTL: time.Hour, ClockSkew: time.Minute}),
		Workspace: app.NewWorkspace(store, chains),
		Gatherer:  reg,
		Log:       slog.New(slog.NewTextHandler(io.Discard, nil)),
		Domain:    appDomain,
	}
	if mutate != nil {
		mutate(&cfg)
	}
	srv := httptest.NewServer(httpapi.NewHandler(cfg))
	t.Cleanup(srv.Close)
	return &env{srv: srv, store: store}
}

// client is a browser: it has a cookie jar and, once signed in, a session.
type client struct {
	t    *testing.T
	base string
	http *http.Client
	key  *secp256k1.PrivateKey
}

func (e *env) client(t *testing.T, seed byte) *client {
	jar, _ := cookiejar.New(nil)
	b := make([]byte, 32)
	b[0], b[31] = 0x22, seed
	return &client{t: t, base: e.srv.URL, http: &http.Client{Jar: jar}, key: secp256k1.PrivKeyFromBytes(b)}
}

func (c *client) address() domain.Address { return ethsig.PubkeyToAddress(c.key.PubKey()) }

func (c *client) do(method, path, body string) (int, map[string]any) {
	c.t.Helper()
	status, out, _ := c.raw(method, path, body, nil)
	return status, out
}

func (c *client) raw(method, path, body string, header http.Header) (int, map[string]any, *http.Response) {
	c.t.Helper()
	req, _ := http.NewRequest(method, c.base+path, strings.NewReader(body))
	for k, v := range header {
		req.Header[k] = v
	}
	resp, err := c.http.Do(req)
	if err != nil {
		c.t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var out map[string]any
	if len(raw) > 0 && strings.HasPrefix(resp.Header.Get("Content-Type"), "application/json") {
		if err := json.Unmarshal(raw, &out); err != nil {
			c.t.Fatalf("bad JSON %s", raw)
		}
	}
	return resp.StatusCode, out, resp
}

// signedMessage asks for a nonce and signs a message the way the web app does.
func (c *client) signedMessage(edit func(*siwe.Message)) string {
	c.t.Helper()
	status, body := c.do("GET", "/v1/auth/nonce", "")
	if status != 200 {
		c.t.Fatalf("nonce: %d %v", status, body)
	}
	m := siwe.Message{Domain: appDomain, Address: c.address(), Statement: "Sign in to chainwatch.",
		URI: "https://" + appDomain, Version: "1", ChainID: 1, Nonce: body["nonce"].(string), IssuedAt: time.Now()}
	if edit != nil {
		edit(&m)
	}
	text := m.String()
	sig := ethsig.SignPersonal(c.key, []byte(text))
	payload, _ := json.Marshal(map[string]string{"message": text, "signature": "0x" + hex.EncodeToString(sig)})
	return string(payload)
}

func (c *client) signIn() domain.OrgID {
	c.t.Helper()
	status, body := c.do("POST", "/v1/auth/verify", c.signedMessage(nil))
	if status != 200 {
		c.t.Fatalf("verify: %d %v", status, body)
	}
	_, me := c.do("GET", "/v1/me", "")
	orgs := me["organizations"].([]any)
	return domain.OrgID(orgs[0].(map[string]any)["id"].(float64))
}

func errCode(body map[string]any) string {
	e, _ := body["error"].(map[string]any)
	s, _ := e["code"].(string)
	return s
}

func orgPath(org domain.OrgID, rest string) string {
	return "/v1/orgs/" + big.NewInt(int64(org)).String() + rest
}

func TestSignInSetsHTTPOnlyCookieAndLogoutClearsIt(t *testing.T) {
	e := setup(t, nil)
	c := e.client(t, 1)
	status, body, resp := c.raw("POST", "/v1/auth/verify", c.signedMessage(nil), nil)
	if status != 200 || body["address"] != ethsig.Checksum(c.address()) {
		t.Fatalf("verify: %d %v", status, body)
	}
	var cookie *http.Cookie
	for _, ck := range resp.Cookies() {
		if ck.Name == httpapi.SessionCookie {
			cookie = ck
		}
	}
	if cookie == nil || !cookie.HttpOnly || cookie.SameSite != http.SameSiteStrictMode || cookie.Path != "/" || cookie.Value == "" {
		t.Fatalf("session cookie = %+v", cookie)
	}
	if strings.Contains(fmtBody(body), cookie.Value) {
		t.Fatal("token leaked into the response body")
	}

	status, me := c.do("GET", "/v1/me", "")
	if status != 200 || me["address"] != ethsig.Checksum(c.address()) {
		t.Fatalf("me: %d %v", status, me)
	}
	orgs := me["organizations"].([]any)
	if len(orgs) != 1 || orgs[0].(map[string]any)["role"] != "owner" {
		t.Fatalf("first sign-in should create a personal organization: %v", orgs)
	}

	if status, _ := c.do("POST", "/v1/auth/logout", ""); status != 204 {
		t.Fatalf("logout: %d", status)
	}
	if status, body := c.do("GET", "/v1/me", ""); status != 401 || errCode(body) != "unauthenticated" {
		t.Fatalf("me after logout: %d %v", status, body)
	}
}

func fmtBody(b map[string]any) string {
	raw, _ := json.Marshal(b)
	return string(raw)
}

func TestSecureCookieFlag(t *testing.T) {
	e := setup(t, func(c *httpapi.Config) { c.CookieSecure = true })
	c := e.client(t, 1)
	_, _, resp := c.raw("POST", "/v1/auth/verify", c.signedMessage(nil), nil)
	for _, ck := range resp.Cookies() {
		if ck.Name == httpapi.SessionCookie && !ck.Secure {
			t.Fatal("cookie not Secure")
		}
	}
}

func TestVerifyRejections(t *testing.T) {
	e := setup(t, nil)
	c := e.client(t, 1)

	replayed := c.signedMessage(nil)
	if status, _ := c.do("POST", "/v1/auth/verify", replayed); status != 200 {
		t.Fatal("first use failed")
	}
	other := e.client(t, 2)
	forged := c.signedMessage(nil)
	var p map[string]string
	_ = json.Unmarshal([]byte(forged), &p)
	p["signature"] = "0x" + hex.EncodeToString(ethsig.SignPersonal(other.key, []byte(p["message"])))
	forgedJSON, _ := json.Marshal(p)

	for name, tt := range map[string]struct {
		body   string
		status int
		code   string
	}{
		"replayed nonce":  {replayed, 401, "invalid_nonce"},
		"wrong signer":    {string(forgedJSON), 401, "invalid_signature"},
		"phishing domain": {c.signedMessage(func(m *siwe.Message) { m.Domain = "evil.test" }), 401, "invalid_message"},
		"unknown chain":   {c.signedMessage(func(m *siwe.Message) { m.ChainID = 10 }), 401, "invalid_message"},
		"not hex":         {`{"message":"x","signature":"zz"}`, 400, "invalid_signature"},
		"garbage message": {`{"message":"hello","signature":"0x00"}`, 401, "invalid_message"},
		"unknown field":   {`{"message":"x","signature":"0x","extra":1}`, 400, "invalid_body"},
		"unissued nonce":  {c.signedMessage(func(m *siwe.Message) { m.Nonce = "0123456789abcdef" }), 401, "invalid_nonce"},
	} {
		t.Run(name, func(t *testing.T) {
			status, body := c.do("POST", "/v1/auth/verify", tt.body)
			if status != tt.status || errCode(body) != tt.code {
				t.Fatalf("%d %v, want %d %s", status, body, tt.status, tt.code)
			}
		})
	}
}

func TestEndpointsNeedASession(t *testing.T) {
	e := setup(t, nil)
	c := e.client(t, 1)
	for _, r := range [][2]string{
		{"GET", "/v1/me"}, {"POST", "/v1/orgs"}, {"GET", "/v1/orgs/1/wallets"}, {"POST", "/v1/orgs/1/wallets"},
		{"DELETE", "/v1/orgs/1/wallets/1/" + walletHex}, {"GET", "/v1/orgs/1/allowances"}, {"GET", "/v1/orgs/1/alerts"},
		{"GET", "/v1/orgs/1/members"}, {"POST", "/v1/orgs/1/members"}, {"DELETE", "/v1/orgs/1/members/" + walletHex},
	} {
		if status, body := c.do(r[0], r[1], "{}"); status != 401 || errCode(body) != "unauthenticated" {
			t.Errorf("%s %s without session: %d %v", r[0], r[1], status, body)
		}
	}
	// A forged cookie is not a session.
	req, _ := http.NewRequest("GET", e.srv.URL+"/v1/me", nil)
	req.AddCookie(&http.Cookie{Name: httpapi.SessionCookie, Value: "forged"})
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 401 {
		t.Fatalf("forged cookie: %d", resp.StatusCode)
	}
}

func TestOrganizationsCannotSeeEachOther(t *testing.T) {
	e := setup(t, nil)
	alice, bob := e.client(t, 1), e.client(t, 2)
	orgA, orgB := alice.signIn(), bob.signIn()
	if status, _ := alice.do("POST", orgPath(orgA, "/wallets"), `{"chain_id":1,"address":"`+walletHex+`","label":"treasury"}`); status != 201 {
		t.Fatalf("add wallet: %d", status)
	}
	owner, _ := domain.ParseAddress(walletHex)
	e.store.SeedAllowances = []domain.Allowance{{ChainID: 1, Owner: owner, Token: domain.Address{1}, Spender: domain.Address{2}, Amount: big.NewInt(9)}}
	e.store.SeedAlerts = []domain.Alert{{ID: 1, OrgID: orgA, ChainID: 1, Wallet: owner, Rule: "approval_to_eoa"}}

	for _, r := range [][3]string{
		{"GET", "/wallets", ""},
		{"POST", "/wallets", `{"chain_id":1,"address":"` + walletHex + `"}`},
		{"DELETE", "/wallets/1/" + walletHex, ""},
		{"GET", "/allowances", ""},
		{"GET", "/allowances?chain_id=1&wallet=" + walletHex, ""},
		{"GET", "/alerts", ""},
		{"GET", "/members", ""},
		{"POST", "/members", `{"address":"` + walletHex + `"}`},
		{"DELETE", "/members/" + alice.address().String(), ""},
	} {
		if status, body := bob.do(r[0], orgPath(orgA, r[1]), r[2]); status != 403 || errCode(body) != "forbidden" {
			t.Errorf("bob %s org A %s: %d %v, want 403", r[0], r[1], status, body)
		}
	}

	// Bob's own organization shows none of org A's data.
	for _, path := range []string{"/wallets", "/allowances", "/alerts"} {
		_, body := bob.do("GET", orgPath(orgB, path), "")
		for _, list := range body {
			if n := len(list.([]any)); n != 0 {
				t.Errorf("org B %s has %d rows of org A's data", path, n)
			}
		}
	}
	if status, _ := bob.do("GET", orgPath(orgB, "/allowances?chain_id=1&wallet="+walletHex), ""); status != 404 {
		t.Errorf("org B filtering by org A's wallet: %d, want 404", status)
	}

	// Alice sees her rows.
	_, body := alice.do("GET", orgPath(orgA, "/allowances?chain_id=1&wallet="+walletHex), "")
	list := body["allowances"].([]any)
	if len(list) != 1 || list[0].(map[string]any)["risk"] != "medium" || list[0].(map[string]any)["chain_id"] != float64(1) {
		t.Fatalf("org A allowances = %v", body)
	}
	if _, body := alice.do("GET", orgPath(orgA, "/alerts"), ""); len(body["alerts"].([]any)) != 1 {
		t.Fatalf("org A alerts = %v", body)
	}
}

func TestWalletLifecycleAndMembers(t *testing.T) {
	e := setup(t, nil)
	alice, carol := e.client(t, 1), e.client(t, 3)
	org := alice.signIn()
	carol.signIn()

	wallets := orgPath(org, "/wallets")
	if status, _ := alice.do("POST", wallets, `{"chain_id":8453,"address":"`+walletHex+`","label":"ops"}`); status != 201 {
		t.Fatalf("add: %d", status)
	}
	if status, body := alice.do("POST", wallets, `{"chain_id":8453,"address":"`+walletHex+`","label":"x"}`); status != 200 || body["label"] != "ops" {
		t.Fatalf("add again: %d %v", status, body)
	}
	if status, body := alice.do("POST", wallets, `{"chain_id":10,"address":"`+walletHex+`"}`); status != 400 || errCode(body) != "invalid_input" {
		t.Fatalf("unconfigured chain: %d %v", status, body)
	}
	if status, body := alice.do("POST", wallets, `{"chain_id":1,"address":"0x12"}`); status != 400 || errCode(body) != "invalid_address" {
		t.Fatalf("bad address: %d %v", status, body)
	}
	_, body := alice.do("GET", wallets, "")
	if list := body["wallets"].([]any); len(list) != 1 || list[0].(map[string]any)["chain_id"] != float64(8453) {
		t.Fatalf("list: %v", body)
	}

	// Add carol as a member: she can read, but not manage members.
	if status, _ := alice.do("POST", orgPath(org, "/members"), `{"address":"`+carol.address().String()+`"}`); status != 201 {
		t.Fatalf("add member: %d", status)
	}
	if status, _ := carol.do("GET", wallets, ""); status != 200 {
		t.Fatalf("member read: %d", status)
	}
	if status, _ := carol.do("POST", orgPath(org, "/members"), `{"address":"`+walletHex+`"}`); status != 403 {
		t.Fatalf("member adding members: %d", status)
	}
	if status, _ := alice.do("POST", orgPath(org, "/members"), `{"address":"`+walletHex+`","role":"admin"}`); status != 400 {
		t.Fatalf("bad role: %d", status)
	}
	if status, body := alice.do("DELETE", orgPath(org, "/members/"+alice.address().String()), ""); status != 409 {
		t.Fatalf("last owner leaving: %d %v", status, body)
	}
	_, body = carol.do("GET", orgPath(org, "/members"), "")
	if len(body["members"].([]any)) != 2 {
		t.Fatalf("members: %v", body)
	}

	if status, _ := alice.do("DELETE", orgPath(org, "/wallets/8453/"+walletHex), ""); status != 204 {
		t.Fatalf("delete: %d", status)
	}
	if status, _ := alice.do("DELETE", orgPath(org, "/wallets/8453/"+walletHex), ""); status != 404 {
		t.Fatalf("delete again: %d", status)
	}
	if status, _ := alice.do("GET", "/v1/orgs/abc/wallets", ""); status != 400 {
		t.Fatalf("bad org id: %d", status)
	}

	status, created := alice.do("POST", "/v1/orgs", `{"name":"Treasury Ops"}`)
	if status != 201 || created["name"] != "Treasury Ops" || created["role"] != "owner" {
		t.Fatalf("create org: %d %v", status, created)
	}
	if status, _ := alice.do("POST", "/v1/orgs", `{"name":""}`); status != 400 {
		t.Fatalf("empty org name: %d", status)
	}
}

func TestAlertsQueryValidation(t *testing.T) {
	e := setup(t, nil)
	c := e.client(t, 1)
	org := c.signIn()
	for _, q := range []string{"since=yesterday", "limit=0", "limit=5000", "wallet=0x12", "chain_id=base"} {
		if status, _ := c.do("GET", orgPath(org, "/alerts?"+q), ""); status != 400 {
			t.Errorf("?%s: %d, want 400", q, status)
		}
	}
	if status, _ := c.do("GET", orgPath(org, "/alerts?chain_id=1&wallet="+walletHex+"&since=2026-01-02T15:04:05Z&limit=5"), ""); status != 200 {
		t.Fatalf("valid query: %d", status)
	}
}

func TestChainsArePublicWithoutRPCURLs(t *testing.T) {
	e := setup(t, nil)
	status, body := e.client(t, 1).do("GET", "/v1/chains", "")
	if status != 200 || len(body["chains"].([]any)) != 2 || strings.Contains(fmtBody(body), "rpc") {
		t.Fatalf("%d %v", status, body)
	}
	first := body["chains"].([]any)[0].(map[string]any)
	if first["id"] != float64(1) || first["name"] != "Ethereum" || first["explorer_url"] != "https://etherscan.io" {
		t.Fatalf("chain = %v", first)
	}
}

func TestCrossOriginUnsafeRequestsAreRefused(t *testing.T) {
	e := setup(t, nil)
	c := e.client(t, 1)
	org := c.signIn()
	body := `{"chain_id":1,"address":"` + walletHex + `"}`
	status, resp, _ := c.raw("POST", orgPath(org, "/wallets"), body, http.Header{"Origin": {"https://evil.test"}})
	if status != 403 || errCode(resp) != "cross_origin" {
		t.Fatalf("cross-origin POST: %d %v", status, resp)
	}
	if status, _, _ := c.raw("POST", orgPath(org, "/wallets"), body, http.Header{"Origin": {"https://" + appDomain}}); status != 201 {
		t.Fatalf("same-origin POST: %d", status)
	}
	if status, _, _ := c.raw("GET", orgPath(org, "/wallets"), "", http.Header{"Origin": {"https://evil.test"}}); status != 200 {
		t.Fatalf("cross-origin GET is harmless and allowed: %d", status)
	}
}

func TestStoreFailureIs500WithoutDetails(t *testing.T) {
	e := setup(t, nil)
	c := e.client(t, 1)
	org := c.signIn()
	e.store.fail = errors.New("connection refused to 10.0.0.5")
	status, body := c.do("GET", orgPath(org, "/wallets"), "")
	if status != 500 || errCode(body) != "internal" || strings.Contains(fmtBody(body), "10.0.0.5") {
		t.Fatalf("%d %v", status, body)
	}
}

func TestServesWebApp(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "index.html"), []byte("<html>app</html>"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "app.js"), []byte("console.log(1)"), 0o644); err != nil {
		t.Fatal(err)
	}
	e := setup(t, func(c *httpapi.Config) { c.WebDir = dir })
	for path, want := range map[string]string{"/": "<html>app</html>", "/app.js": "console.log(1)", "/dashboard/deep/link": "<html>app</html>"} {
		resp, err := http.Get(e.srv.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		raw, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != 200 || string(raw) != want {
			t.Errorf("GET %s = %d %q, want %q", path, resp.StatusCode, raw, want)
		}
	}
	if status, body := e.client(t, 1).do("GET", "/v1/nope", ""); status != 404 || errCode(body) != "not_found" {
		t.Errorf("unknown API path falls through to the app: %d %v", status, body)
	}
}

func TestHealthReadyMetrics(t *testing.T) {
	healthy := httpapi.Check{Name: "postgres", Fn: func(context.Context) error { return nil }}
	broken := httpapi.Check{Name: "rpc_base", Fn: func(context.Context) error { return errors.New("dial tcp: refused") }}

	e := setup(t, func(c *httpapi.Config) { c.Checks = []httpapi.Check{healthy} })
	c := e.client(t, 1)
	if status, _ := c.do("GET", "/healthz", ""); status != 200 {
		t.Fatalf("healthz %d", status)
	}
	if status, body := c.do("GET", "/readyz", ""); status != 200 || body["checks"].(map[string]any)["postgres"] != "ok" {
		t.Fatalf("readyz %d %v", status, body)
	}

	e2 := setup(t, func(c *httpapi.Config) { c.Checks = []httpapi.Check{healthy, broken} })
	status, body := e2.client(t, 1).do("GET", "/readyz", "")
	if status != http.StatusServiceUnavailable || body["checks"].(map[string]any)["rpc_base"] != "dial tcp: refused" {
		t.Fatalf("readyz with broken rpc %d %v", status, body)
	}

	resp, err := http.Get(e.srv.URL + "/metrics")
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(raw), "chainwatch_alerts_pending 0") {
		t.Fatalf("metrics output misses chainwatch_alerts_pending:\n%s", raw)
	}
}

func TestServerHasTimeouts(t *testing.T) {
	s := httpapi.NewServer(":0", http.NotFoundHandler())
	if s.ReadHeaderTimeout == 0 || s.ReadTimeout == 0 || s.WriteTimeout == 0 || s.IdleTimeout == 0 {
		t.Fatalf("missing timeouts: %+v", s)
	}
}
