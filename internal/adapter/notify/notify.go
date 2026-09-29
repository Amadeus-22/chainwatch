// Package notify delivers alerts: a generic JSON webhook, Telegram, and a log-only
// notifier for when neither is configured. Each call retries transient failures a
// few times with jittered backoff; longer outages are handled by app.Alerter,
// which keeps the alert pending and tries again later.
package notify

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/Amadeus-22/chainwatch/internal/app"
	"github.com/Amadeus-22/chainwatch/internal/domain"
	"github.com/Amadeus-22/chainwatch/internal/platform/backoff"
)

var (
	_ app.Notifier = (*Webhook)(nil)
	_ app.Notifier = (*Telegram)(nil)
	_ app.Notifier = (*Log)(nil)
)

// Options tune the in-call retry. Zero values get defaults.
type Options struct {
	Timeout  time.Duration  // per HTTP attempt; default 10s
	Attempts int            // default 3
	Backoff  backoff.Policy // default 200ms..2s
}

func (o Options) withDefaults() Options {
	if o.Timeout == 0 {
		o.Timeout = 10 * time.Second
	}
	if o.Attempts == 0 {
		o.Attempts = 3
	}
	if o.Backoff == (backoff.Policy{}) {
		o.Backoff = backoff.Policy{Base: 200 * time.Millisecond, Max: 2 * time.Second}
	}
	return o
}

// permanentError marks a failure that retrying cannot fix (e.g. HTTP 400).
type permanentError struct{ err error }

func (e permanentError) Error() string { return e.err.Error() }
func (e permanentError) Unwrap() error { return e.err }

// post sends body to url, retrying network errors, 429 and 5xx responses.
func post(ctx context.Context, client *http.Client, o Options, url string, body []byte, header http.Header) error {
	var err error
	for attempt := 1; attempt <= o.Attempts; attempt++ {
		if err = postOnce(ctx, client, o.Timeout, url, body, header); err == nil {
			return nil
		}
		var perm permanentError
		if errors.As(err, &perm) || attempt == o.Attempts {
			break
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(o.Backoff.Delay(attempt)):
		}
	}
	return err
}

func postOnce(ctx context.Context, client *http.Client, timeout time.Duration, url string, body []byte, header http.Header) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return permanentError{fmt.Errorf("build request: %w", err)}
	}
	for k, v := range header {
		req.Header[k] = v
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("send: %w", err)
	}
	defer resp.Body.Close()
	snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
	switch {
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		return nil
	case resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500:
		return fmt.Errorf("receiver returned %d: %s", resp.StatusCode, snippet)
	default:
		return permanentError{fmt.Errorf("receiver returned %d: %s", resp.StatusCode, snippet)}
	}
}

// Log "delivers" alerts to the process log. It is the default when no external
// channel is configured, so alerts are still visible and do not pile up pending.
type Log struct{ log *slog.Logger }

// NewLog returns a log-only notifier.
func NewLog(log *slog.Logger) *Log { return &Log{log: log} }

// Notify logs the alert at warn level.
func (l *Log) Notify(_ context.Context, a domain.Alert) error {
	l.log.Warn("ALERT", "rule", a.Rule, "severity", string(a.Severity), "wallet", a.Wallet.String(),
		"token", a.Token.String(), "tx", a.TxHash.String(), "block", a.BlockNumber, "reason", a.Reason)
	return nil
}
