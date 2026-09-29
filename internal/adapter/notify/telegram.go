package notify

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/Amadeus-22/chainwatch/internal/domain"
)

// Telegram sends alerts as bot messages to one chat. The Bot API has no
// idempotency key, so the rare crash-between-send-and-ack case can repeat a message.
type Telegram struct {
	apiURL string // https://api.telegram.org/bot<token>/sendMessage
	chatID string
	client *http.Client
	opts   Options
}

// NewTelegram returns a Telegram notifier. baseURL is https://api.telegram.org
// in production and an httptest server in tests.
func NewTelegram(baseURL, botToken, chatID string, opts Options) *Telegram {
	return &Telegram{
		apiURL: strings.TrimRight(baseURL, "/") + "/bot" + botToken + "/sendMessage",
		chatID: chatID,
		client: &http.Client{},
		opts:   opts.withDefaults(),
	}
}

// Notify sends the alert as a plain-text message.
func (t *Telegram) Notify(ctx context.Context, a domain.Alert) error {
	text := fmt.Sprintf("[%s] %s\n%s\nwallet %s\ntx %s (block %d)",
		strings.ToUpper(string(a.Severity)), a.Rule, a.Reason, a.Wallet, a.TxHash, a.BlockNumber)
	body, err := json.Marshal(map[string]any{"chat_id": t.chatID, "text": text, "disable_web_page_preview": true})
	if err != nil {
		return fmt.Errorf("encode telegram message: %w", err)
	}
	if err := post(ctx, t.client, t.opts, t.apiURL, body, nil); err != nil {
		// The URL embeds the bot token: never let it reach the logs.
		return fmt.Errorf("deliver alert %d to telegram: %s", a.ID, strings.ReplaceAll(err.Error(), t.apiURL, "<telegram-api>"))
	}
	return nil
}
