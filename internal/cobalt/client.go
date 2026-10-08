// Package cobalt — тонкий клиент к локальному cobalt-инстансу (API v11):
// POST / с JSON, разбор redirect/tunnel/picker/error.
package cobalt

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"vdl/internal/media"
)

// Client — клиент одного инстанса. Сам инстанс у нас за nginx, режим
// tunnel отдаёт абсолютные ссылки под нашим доменом (/tunnel/…).
type Client struct {
	HC   *http.Client
	Base string // http://127.0.0.1:9000
}

func New(hc *http.Client, base string) *Client {
	return &Client{HC: hc, Base: strings.TrimRight(base, "/")}
}

type resp struct {
	Status   string `json:"status"`
	URL      string `json:"url,omitempty"`
	Filename string `json:"filename,omitempty"`
	Picker   []struct {
		Type string `json:"type"` // video | photo | gif
		URL  string `json:"url"`
	} `json:"picker,omitempty"`
	Error *struct {
		Code string `json:"code"`
	} `json:"error,omitempty"`
}

// Resolve — одна ссылка → результат. Качество максимальное, режим авто;
// cobalt сам решает redirect (IP-независимый CDN) vs tunnel (скачивает сам).
func (c *Client) Resolve(ctx context.Context, rawURL string) (*media.ResolveResult, error) {
	body, err := json.Marshal(map[string]any{
		"url":             rawURL,
		"videoQuality":    "max",
		"downloadMode":    "auto",
		"alwaysProxy":     false,
		"disableMetadata": false,
	})
	if err != nil {
		return nil, &media.Error{Kind: media.ErrUpstream, Detail: err.Error()}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.Base+"/",
		bytes.NewReader(body))
	if err != nil {
		return nil, &media.Error{Kind: media.ErrUpstream, Detail: err.Error()}
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")

	// отдельное имя: err уже объявлен как interface error выше, повторное
	// := заворачивает typed-nil *media.Error в ненулевой интерфейс (грабля,
	// ронявшая каждый успешный ответ cobalt как «upstream: <nil>»)
	respBytes, derr := c.do(req)
	if derr != nil {
		return nil, derr
	}
	var r resp
	if err := json.Unmarshal(respBytes, &r); err != nil {
		return nil, &media.Error{Kind: media.ErrUpstream,
			Detail: "cobalt: битый JSON: " + truncate(string(respBytes), 200)}
	}

	res := &media.ResolveResult{Source: "cobalt", Site: siteOf(rawURL)}
	switch r.Status {
	case "redirect", "tunnel":
		if r.URL == "" {
			return nil, &media.Error{Kind: media.ErrUpstream,
				Detail: "cobalt: " + r.Status + " без url"}
		}
		typ := "video"
		if r.Status == "tunnel" {
			typ = "tunnel"
		}
		res.Medias = []media.Media{{Type: typ, Best: r.URL}}
		return res, nil
	case "picker":
		// слайд-шоу/галерея: видео вперёд, порядок сохраняем
		for _, it := range r.Picker {
			if it.URL == "" {
				continue
			}
			res.Medias = append(res.Medias, media.Media{Type: it.Type, Best: it.URL})
		}
		if len(res.Medias) == 0 {
			return nil, &media.Error{Kind: media.ErrNoMedia, Detail: "cobalt: пустой picker"}
		}
		return res, nil
	case "error":
		return nil, mapError(r.Error)
	default:
		return nil, &media.Error{Kind: media.ErrUpstream,
			Detail: "cobalt: неизвестный статус " + truncate(r.Status, 40)}
	}
}

func (c *Client) do(req *http.Request) ([]byte, *media.Error) {
	if c.HC == nil {
		c.HC = &http.Client{Timeout: 25 * time.Second}
	}
	resp, err := c.HC.Do(req)
	if err != nil {
		return nil, &media.Error{Kind: media.ErrUpstream, Detail: "cobalt: " + err.Error()}
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, &media.Error{Kind: media.ErrUpstream, Detail: "cobalt: чтение ответа: " + err.Error()}
	}
	return b, nil
}

// mapError — коды cobalt → наши виды. Точных кодов в доках мало, матчуем
// по подстрокам, неизвестное — upstream с кодом в Detail.
func mapError(e *struct {
	Code string `json:"code"`
}) *media.Error {
	if e == nil {
		return &media.Error{Kind: media.ErrUpstream, Detail: "cobalt: error без кода"}
	}
	code := e.Code
	low := strings.ToLower(code)
	switch {
	case strings.Contains(low, "unsupported"), strings.Contains(low, "invalid"):
		return &media.Error{Kind: media.ErrBadURL, Detail: "cobalt: " + code}
	case strings.Contains(low, "rate"):
		return &media.Error{Kind: media.ErrRateLimited, Detail: "cobalt: " + code}
	case strings.Contains(low, "login"), strings.Contains(low, "auth"):
		return &media.Error{Kind: media.ErrCookiesDead, Detail: "cobalt: " + code}
	default:
		return &media.Error{Kind: media.ErrUpstream, Detail: "cobalt: " + code}
	}
}

// siteOf — грубое имя сайта из хоста (для логов/UI), не точная классификация.
func siteOf(rawURL string) string {
	if u, err := url.Parse(rawURL); err == nil {
		host := strings.TrimPrefix(u.Hostname(), "www.")
		if i := strings.IndexByte(host, '.'); i > 0 {
			host = host[:i]
		}
		return host
	}
	return "unknown"
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// CatchAll — cobalt как резолвер последней надежды: матчит любой хост,
// в реестре регистрируется последним (после сайт-специфичных цепочек).
type CatchAll struct{ *Client }

func (c CatchAll) Name() string          { return "cobalt" }
func (c CatchAll) MatchHost(string) bool { return true }
func (c CatchAll) Resolve(ctx context.Context, u *url.URL) (*media.ResolveResult, error) {
	return c.Client.Resolve(ctx, u.String())
}
