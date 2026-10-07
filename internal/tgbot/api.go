// Package tgbot — управление куками через Telegram (long-poll Bot API).
// Только админ-чаты из whitelist; куки не логируются.
package tgbot

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// Client — минимальная обёртка Bot API: getUpdates + sendMessage.
type Client struct {
	HC    *http.Client
	Token string
}

func NewClient(token string) *Client {
	return &Client{
		Token: token,
		HC:    &http.Client{Timeout: 60 * time.Second}, // long-poll 50с + запас
	}
}

type Update struct {
	UpdateID int      `json:"update_id"`
	Message  *Message `json:"message"`
}

type Message struct {
	Chat Chat   `json:"chat"`
	Text string `json:"text"`
	From *struct {
		ID        int64  `json:"id"`
		FirstName string `json:"first_name"`
	} `json:"from"`
}

type Chat struct {
	ID   int64  `json:"id"`
	Type string `json:"type"` // private | group | supergroup
}

// GetUpdates — long-poll (timeout 50с).
func (c *Client) GetUpdates(ctx context.Context, offset int) ([]Update, error) {
	var out struct {
		OK          bool     `json:"ok"`
		Result      []Update `json:"result"`
		Description string   `json:"description"`
	}
	err := c.post(ctx, "getUpdates", map[string]any{
		"offset":          offset,
		"timeout":         50,
		"allowed_updates": []string{"message"},
	}, &out)
	if err != nil {
		return nil, err
	}
	if !out.OK {
		return nil, fmt.Errorf("tg: getUpdates: %s", out.Description)
	}
	return out.Result, nil
}

// SendMessage — HTML, без превью ссылок.
func (c *Client) SendMessage(ctx context.Context, chat int64, text string) error {
	var out struct {
		OK          bool   `json:"ok"`
		Description string `json:"description"`
	}
	err := c.post(ctx, "sendMessage", map[string]any{
		"chat_id":      chat,
		"text":         text,
		"parse_mode":   "HTML",
		"link_preview_options": map[string]any{"is_disabled": true},
	}, &out)
	if err != nil {
		return err
	}
	if !out.OK {
		return fmt.Errorf("tg: sendMessage: %s", out.Description)
	}
	return nil
}

func (c *Client) post(ctx context.Context, method string, payload any, out any) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		"https://api.telegram.org/bot"+c.Token+"/"+method, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.HC.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return err
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("tg: %s: битый ответ", method)
	}
	return nil
}
