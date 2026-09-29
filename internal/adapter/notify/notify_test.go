package notify_test

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Amadeus-22/chainwatch/internal/adapter/notify"
	"github.com/Amadeus-22/chainwatch/internal/domain"
	"github.com/Amadeus-22/chainwatch/internal/platform/backoff"
)

var fast = notify.Options{Timeout: time.Second, Attempts: 3, Backoff: backoff.Policy{Base: time.Millisecond, Max: 2 * time.Millisecond}}

func testAlert() domain.Alert {
	return domain.Alert{
		ID: 7, Wallet: domain.Address{19: 1}, Rule: "drain_pattern", Severity: domain.SeverityCritical,
		Reason: "drained", Token: domain.Address{19: 2}, TxHash: domain.Hash{31: 3}, LogIndex: 4, BlockNumber: 5,
	}
}

// receiver answers with the given status codes in order, then 200, and records
// every request it sees.
type receiver struct {
	mu       sync.Mutex
	statuses []int
	bodies   [][]byte
	keys     []string
	paths    []string
}

func (r *receiver) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	r.mu.Lock()
	defer r.mu.Unlock()
	b, _ := io.ReadAll(req.Body)
	r.bodies = append(r.bodies, b)
	r.keys = append(r.keys, req.Header.Get("Idempotency-Key"))
	r.paths = append(r.paths, req.URL.Path)
	status := http.StatusOK
	if len(r.statuses) > 0 {
		status, r.statuses = r.statuses[0], r.statuses[1:]
	}
	w.WriteHeader(status)
}

func TestWebhookRetriesTransientFailuresWithSameKey(t *testing.T) {
	rcv := &receiver{statuses: []int{503, 429}}
	srv := httptest.NewServer(rcv)
	defer srv.Close()

	if err := notify.NewWebhook(srv.URL, fast).Notify(context.Background(), testAlert()); err != nil {
		t.Fatal(err)
	}
	if len(rcv.bodies) != 3 {
		t.Fatalf("receiver saw %d requests, want 3", len(rcv.bodies))
	}
	want := testAlert().DedupKey()
	for i, k := range rcv.keys {
		if k != want {
			t.Errorf("attempt %d Idempotency-Key = %q, want %q", i, k, want)
		}
	}
	var p notify.WebhookPayload
	if err := json.Unmarshal(rcv.bodies[2], &p); err != nil {
		t.Fatal(err)
	}
	if p.Rule != "drain_pattern" || p.DedupKey != want || p.AlertID != 7 || p.TxHash != (domain.Hash{31: 3}) {
		t.Errorf("payload = %+v", p)
	}
}

func TestWebhookGivesUpAfterAttempts(t *testing.T) {
	rcv := &receiver{statuses: []int{500, 500, 500, 500}}
	srv := httptest.NewServer(rcv)
	defer srv.Close()
	err := notify.NewWebhook(srv.URL, fast).Notify(context.Background(), testAlert())
	if err == nil || !strings.Contains(err.Error(), "500") {
		t.Fatalf("err = %v", err)
	}
	if len(rcv.bodies) != 3 {
		t.Fatalf("receiver saw %d requests, want 3 attempts", len(rcv.bodies))
	}
}

func TestWebhookDoesNotRetryClientErrors(t *testing.T) {
	rcv := &receiver{statuses: []int{400}}
	srv := httptest.NewServer(rcv)
	defer srv.Close()
	if err := notify.NewWebhook(srv.URL, fast).Notify(context.Background(), testAlert()); err == nil {
		t.Fatal("want error")
	}
	if len(rcv.bodies) != 1 {
		t.Fatalf("receiver saw %d requests, want 1 (400 is permanent)", len(rcv.bodies))
	}
}

func TestWebhookTimesOut(t *testing.T) {
	var calls atomic.Int32
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		calls.Add(1)
		<-release
	}))
	defer srv.Close()
	defer close(release)
	opts := fast
	opts.Timeout = 20 * time.Millisecond
	start := time.Now()
	if err := notify.NewWebhook(srv.URL, opts).Notify(context.Background(), testAlert()); err == nil {
		t.Fatal("want timeout error")
	}
	if calls.Load() != 3 || time.Since(start) > time.Second {
		t.Fatalf("calls = %d in %s", calls.Load(), time.Since(start))
	}
}

func TestTelegramSendsMessageAndHidesToken(t *testing.T) {
	rcv := &receiver{}
	srv := httptest.NewServer(rcv)
	defer srv.Close()
	tg := notify.NewTelegram(srv.URL, "SECRET", "42", fast)
	if err := tg.Notify(context.Background(), testAlert()); err != nil {
		t.Fatal(err)
	}
	if rcv.paths[0] != "/botSECRET/sendMessage" {
		t.Errorf("path = %s", rcv.paths[0])
	}
	var msg struct {
		ChatID string `json:"chat_id"`
		Text   string `json:"text"`
	}
	_ = json.Unmarshal(rcv.bodies[0], &msg)
	if msg.ChatID != "42" || !strings.Contains(msg.Text, "[CRITICAL] drain_pattern") {
		t.Errorf("message = %+v", msg)
	}

	down := &receiver{statuses: []int{401}}
	srv2 := httptest.NewServer(down)
	defer srv2.Close()
	err := notify.NewTelegram(srv2.URL, "SECRET", "42", fast).Notify(context.Background(), testAlert())
	if err == nil || strings.Contains(err.Error(), "SECRET") {
		t.Fatalf("err = %v; want an error without the bot token", err)
	}
}

func TestLogNotifier(t *testing.T) {
	var buf strings.Builder
	n := notify.NewLog(slog.New(slog.NewJSONHandler(&buf, nil)))
	if err := n.Notify(context.Background(), testAlert()); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), `"rule":"drain_pattern"`) {
		t.Fatalf("log = %s", buf.String())
	}
}
