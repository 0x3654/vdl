// Package media — общие типы резолва: результат, сайт-резолверы и реестр.
// Пакет-лист: сайт-пакеты (twitter, …) зависят только от него, не друг от друга.
package media

import (
	"context"
	"fmt"
	"net"
	"net/url"
	"sync"
)

// Variant — одно качество одного медиа (для twitter: mp4 с разным bitrate).
type Variant struct {
	Bitrate int    `json:"bitrate"` // фото: 0
	Width   int    `json:"width"`
	URL     string `json:"url"`
}

// Media — одно медиа поста; Best — готовая прямая ссылка (лучший вариант).
type Media struct {
	Type     string    `json:"type"` // video | animated_gif | photo | tunnel
	Best     string    `json:"best"`
	Poster   string    `json:"poster,omitempty"`
	Width    int       `json:"width,omitempty"`
	Height   int       `json:"height,omitempty"`
	Duration float64   `json:"duration,omitempty"`
	Variants []Variant `json:"variants,omitempty"`
}

// ResolveResult — что выдал резолвер для одного поста.
type ResolveResult struct {
	Source string  `json:"source"` // cobalt | fxtwitter | graphql
	Site   string  `json:"site"`   // twitter | youtube | …
	Author string  `json:"author,omitempty"`
	Text   string  `json:"text,omitempty"`
	Medias []Media `json:"medias"`
	// Links — внешние ссылки из текста поста (fx разворачивает t.co):
	// сервер резолвит их тем же реестром — твит→твит→youtube рекурсивно
	Links []string `json:"links,omitempty"`
}

// First — первое медиа (фаза 1: ?index= не поддерживаем, берём лучшее).
func (r *ResolveResult) First() (*Media, error) {
	if r == nil || len(r.Medias) == 0 {
		return nil, &Error{Kind: ErrNoMedia, Detail: "в посте нет медиа"}
	}
	return &r.Medias[0], nil
}

// ErrKind — класс ошибки: определяет HTTP-код /dl и реакцию store/бота.
type ErrKind int

const (
	ErrBadURL      ErrKind = iota // 400 — не ссылка/не поддерживаемый сайт
	ErrNoMedia                    // 422 — пост без медиа
	ErrCookiesDead                // 503 + TG-алерт: все аккаунты умерли
	ErrRateLimited                // 503 + Retry-After
	ErrUpstream                   // 502
)

func (k ErrKind) String() string {
	switch k {
	case ErrBadURL:
		return "bad url"
	case ErrNoMedia:
		return "no media"
	case ErrCookiesDead:
		return "cookies dead"
	case ErrRateLimited:
		return "rate limited"
	default:
		return "upstream"
	}
}

// Error — ошибка резолва. Detail НИКОГДА не содержит кук.
type Error struct {
	Kind    ErrKind
	Account string // label аккаунта, если ошибка кук
	Detail  string
}

func (e *Error) Error() string {
	switch {
	case e == nil:
		return "<nil>"
	case e.Account != "":
		return fmt.Sprintf("%s (%s: %s)", e.Kind, e.Account, e.Detail)
	case e.Detail != "":
		return fmt.Sprintf("%s: %s", e.Kind, e.Detail)
	default:
		return e.Kind.String()
	}
}

// AsError — типизированная проверка ошибки.
func AsError(err error) *Error {
	if e, ok := err.(*Error); ok {
		return e
	}
	return nil
}

// Resolver — сайт-специфичный резолвер.
type Resolver interface {
	Name() string
	MatchHost(host string) bool
	Resolve(ctx context.Context, u *url.URL) (*ResolveResult, error)
}

// Registry — маршрутизация по хосту: первый зарегистрированный, кто сматчил.
// Порядок регистрации важен: twitter-цепочка до catch-all cobalt.
type Registry struct {
	mu sync.RWMutex
	rs []Resolver
}

func (r *Registry) Register(res Resolver) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.rs = append(r.rs, res)
}

func (r *Registry) ForHost(host string) Resolver {
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, res := range r.rs {
		if res.MatchHost(host) {
			return res
		}
	}
	return nil
}

// Chain — последовательный фолбэк нескольких резолверов одного сайта.
// ErrNoMedia и ErrRateLimited прерывают цепочку (дальше смысла нет),
// остальные ошибки — переходим к следующему.
type Chain struct {
	name  string
	match func(host string) bool
	parts []Resolver
}

func NewChain(name string, match func(host string) bool, parts ...Resolver) *Chain {
	return &Chain{name: name, match: match, parts: parts}
}

func (c *Chain) Name() string            { return c.name }
func (c *Chain) MatchHost(h string) bool { return c.match != nil && c.match(h) }

func (c *Chain) Resolve(ctx context.Context, u *url.URL) (*ResolveResult, error) {
	var lastErr error = &Error{Kind: ErrUpstream, Detail: "цепочка пуста"}
	for _, p := range c.parts {
		res, err := p.Resolve(ctx, u)
		if err == nil {
			if len(res.Medias) > 0 || len(res.Links) > 0 {
				return res, nil
			}
			lastErr = &Error{Kind: ErrNoMedia, Detail: p.Name() + ": пустой результат"}
			continue
		}
		lastErr = err
		if e := AsError(err); e != nil {
			if e.Kind == ErrNoMedia || e.Kind == ErrRateLimited || e.Kind == ErrBadURL {
				return nil, err
			}
		}
	}
	return nil, lastErr
}

// IsPublicHost — SSRF-защита перед выдачей 302: только публичные хосты,
// без userinfo, без IP-литералов приватных диапазонов.
func IsPublicHost(u *url.URL) bool {
	if u == nil || u.Scheme != "https" && u.Scheme != "http" {
		return false
	}
	if u.User != nil {
		return false
	}
	host := u.Hostname()
	if host == "" {
		return false
	}
	if ip := net.ParseIP(host); ip != nil {
		return ip.IsGlobalUnicast() && !ip.IsPrivate() && !ip.IsLoopback() &&
			!ip.IsLinkLocalUnicast() && !ip.IsMulticast() && !ip.IsUnspecified()
	}
	return true // домен: DNS резолвим только для проверки IP-литералов выше
}
