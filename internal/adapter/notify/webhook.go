package notify

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/Amadeus-22/chainwatch/internal/domain"
)

// Webhook POSTs each alert as JSON to a fixed URL. The Idempotency-Key header
// (and dedup_key field) is stable across retries and restarts.
type Webhook struct {
	url    string
	client *http.Client
	opts   Options
}

// NewWebhook returns a webhook notifier.
func NewWebhook(url string, opts Options) *Webhook {
	return &Webhook{url: url, client: &http.Client{}, opts: opts.withDefaults()}
}

// WebhookPayload is the JSON body sent to the webhook.
type WebhookPayload struct {
	DedupKey    string          `json:"dedup_key"`
	AlertID     int64           `json:"alert_id"`
	Wallet      domain.Address  `json:"wallet"`
	Rule        string          `json:"rule"`
	Severity    domain.Severity `json:"severity"`
	Reason      string          `json:"reason"`
	Token       domain.Address  `json:"token"`
	TxHash      domain.Hash     `json:"tx_hash"`
	LogIndex    uint            `json:"log_index"`
	BlockNumber uint64          `json:"block_number"`
	CreatedAt   time.Time       `json:"created_at"`
}

// Notify sends the alert.
func (w *Webhook) Notify(ctx context.Context, a domain.Alert) error {
	body, err := json.Marshal(WebhookPayload{
		DedupKey: a.DedupKey(), AlertID: a.ID, Wallet: a.Wallet, Rule: a.Rule, Severity: a.Severity,
		Reason: a.Reason, Token: a.Token, TxHash: a.TxHash, LogIndex: a.LogIndex,
		BlockNumber: a.BlockNumber, CreatedAt: a.CreatedAt,
	})
	if err != nil {
		return fmt.Errorf("encode webhook payload: %w", err)
	}
	header := http.Header{"Idempotency-Key": {a.DedupKey()}}
	if err := post(ctx, w.client, w.opts, w.url, body, header); err != nil {
		return fmt.Errorf("deliver alert %d to webhook: %w", a.ID, err)
	}
	return nil
}
