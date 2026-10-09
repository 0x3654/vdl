// Package twitter — нативный резолвер X/Twitter: нормализация ссылок,
// fxtwitter без кук + GraphQL под куками (порт twitter_read.py из скилла).
// Используется как fallback, когда cobalt не вытянул, и для health-check кук.
package twitter

import (
	"context"
	"net/http"
	"net/url"
	"regexp"
	"strings"

	"vdl/internal/cobalt"
	"vdl/internal/media"
)

var statusRe = regexp.MustCompile(`^/([A-Za-z0-9_]{1,20})/status(?:es)?/(\d{2,25})`)
var iStatusRe = regexp.MustCompile(`^/(?:i/)?status(?:es)?/(\d{2,25})`)

// CleanHost — хост в канонический вид: без порта/регистра/префиксов.
func CleanHost(host string) string {
	h := strings.ToLower(strings.TrimSuffix(host, "."))
	for _, p := range []string{"www.", "m.", "mobile."} {
		h = strings.TrimPrefix(h, p)
	}
	return h
}

// IsTwitterHost — x.com / twitter.com / t.co.
func IsTwitterHost(host string) bool {
	switch CleanHost(host) {
	case "x.com", "twitter.com", "t.co", "v.t.co":
		return true
	}
	return false
}

// Normalize — любой вид ссылки на твит → (screen_name, status_id).
func Normalize(u *url.URL) (string, string, error) {
	if !IsTwitterHost(u.Host) {
		return "", "", &media.Error{Kind: media.ErrBadURL, Detail: "не twitter-хост: " + CleanHost(u.Host)}
	}
	p := u.EscapedPath()
	if m := statusRe.FindStringSubmatch(p); m != nil {
		return m[1], m[2], nil
	}
	if m := iStatusRe.FindStringSubmatch(p); m != nil {
		return "i", m[1], nil
	}
	return "", "", &media.Error{Kind: media.ErrBadURL, Detail: "в ссылке нет /status/<id>"}
}

// ExpandShortLink — t.co разворачивается редиректом; забираем Location
// первого хопа и не качаем тело цели.
func ExpandShortLink(ctx context.Context, hc *http.Client, raw string) (string, error) {
	if hc == nil {
		hc = http.DefaultClient
	}
	noRedirect := *hc
	noRedirect.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		return http.ErrUseLastResponse
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, raw, nil)
	if err != nil {
		return "", err
	}
	resp, err := noRedirect.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	loc := resp.Header.Get("Location")
	if loc == "" {
		return "", &media.Error{Kind: media.ErrBadURL, Detail: "t.co не отдал Location"}
	}
	if abs, err := url.Parse(loc); err == nil && abs.IsAbs() {
		return loc, nil
	}
	return resp.Request.URL.JoinPath(loc).String(), nil
}

// NormalizeMaybeShort — поддерживает и t.co, и прямые ссылки; возвращает
// канонические (screen, id).
func NormalizeMaybeShort(ctx context.Context, hc *http.Client, u *url.URL) (string, string, error) {
	if CleanHost(u.Host) == "t.co" || CleanHost(u.Host) == "v.t.co" {
		expanded, err := ExpandShortLink(ctx, hc, u.String())
		if err != nil {
			return "", "", err
		}
		eu, err := url.Parse(expanded)
		if err != nil {
			return "", "", &media.Error{Kind: media.ErrBadURL, Detail: "t.co → кривой Location"}
		}
		return Normalize(eu)
	}
	return Normalize(u)
}

// cobaltTw — cobalt, ограниченный твиттер-хостами (адаптер для цепочки).
type cobaltTw struct{ *cobalt.Client }

func (c cobaltTw) Name() string            { return "cobalt" }
func (c cobaltTw) MatchHost(h string) bool { return IsTwitterHost(h) }
func (c cobaltTw) Resolve(ctx context.Context, u *url.URL) (*media.ResolveResult, error) {
	return c.Client.Resolve(ctx, u.String())
}

// Chain — собирает резолв-цепочку твиттера: cobalt → fxtwitter → GraphQL.
// Порядок: cobalt (гостевой syndication, куки не нужны) → fx (без кук,
// NSFW ок) → GraphQL (куки аккаунтов, egress через SOCKS при настройке).
func Chain(cb *cobalt.Client, fx *FXClient, gql *GQLClient) *media.Chain {
	twOnly := func(h string) bool { return IsTwitterHost(h) }
	var parts []media.Resolver
	if cb != nil {
		parts = append(parts, cobaltTw{cb})
	}
	if fx != nil {
		parts = append(parts, fx)
	}
	if gql != nil {
		parts = append(parts, gql)
	}
	return media.NewChain("twitter", twOnly, parts...)
}
