// vdl — универсальный видео-загрузчик: cobalt-ядро + нативный твиттер-fallback
// и куки-менеджер в Telegram. См. internal/* за деталями.
//
// GET /dl?token=…&url=…  — 302 на CDN/tunnel (для шортката)
// GET /healthz           — живость + счётчик аккаунтов
// TG-бот                 — /add /accounts /del /check кук twitter
package main

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"vdl/internal/checker"
	"vdl/internal/cobalt"
	"vdl/internal/media"
	"vdl/internal/server"
	"vdl/internal/store"
	"vdl/internal/tgbot"
	"vdl/internal/twitter"
)

var rev = "unknown" // -ldflags -X main.rev=…

func main() {
	cfg, err := Load()
	if err != nil {
		log.Fatalf("config: %v", err)
	}
	if err := os.MkdirAll(cfg.DataDir, 0o700); err != nil {
		log.Fatalf("data dir: %v", err)
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	// хранилище кук + кэш queryId
	st, err := store.Open(filepath.Join(cfg.DataDir, "accounts.json"))
	if err != nil {
		log.Fatalf("store: %v", err)
	}
	ids := twitter.OpenQueryIDs(filepath.Join(cfg.DataDir, "query-ids.json"))

	// notify появляется вместе с ботом; до его старта — только лог
	var notifyFn atomic.Value // func(string)
	notify := func(text string) {
		log.Printf("notify: %s", text)
		if f, ok := notifyFn.Load().(func(string)); ok {
			f(text)
		}
	}

	cob := cobalt.New(&http.Client{Timeout: 25 * time.Second}, cfg.CobaltURL)
	fx := twitter.NewFX(&http.Client{Timeout: 15 * time.Second})
	gql := twitter.NewGQL(gqlClient(), st, ids, notify)

	reg := &media.Registry{}
	reg.Register(twitter.Chain(cob, fx, gql))
	// youtube: cobalt-main (датацентр) → cobalt-age (домашний egress,
	// только для возрастных) — регистрируется ДО catch-all
	if cfg.CobaltAgeURL != "" {
		reg.Register(media.NewChain("youtube-age", ytHost,
			cobaltTwWrap{cob}, cobaltAge{cobalt.New(&http.Client{Timeout: 40 * time.Second}, cfg.CobaltAgeURL)}))
	}
	reg.Register(cobalt.CatchAll{Client: cob})

	// материализация кук для cobalt (контейнер рестартует cron'ом по mtime);
	// при старте — сразу: seed-аккаунты из DATA_DIR попадают в cobalt
	// без единого сообщения боту
	onChange := func() {
		if cfg.CookiesOut == "" {
			return
		}
		if err := materializeCookies(cfg.CookiesOut, st); err != nil {
			log.Printf("cookies.json: %v", err)
		}
	}
	onChange()

	// TG-бот управления куками + чекер; новые куки → рестарт cobalt (docker-сокет)
	if cfg.TGBotToken != "" {
		bot := tgbot.New(tgbot.NewClient(cfg.TGBotToken), cfg.TGAdminChats, st, gql.Probe,
			onChange, cobalt.RestartFromEnv)
		notifyFn.Store(bot.NotifyAdmins)
		go bot.Run(ctx)
		if cfg.CheckInterval > 0 {
			ch := &checker.Checker{Store: st, Probe: gql.Probe,
				Notify: bot.NotifyAdmins, Every: cfg.CheckInterval}
			go ch.Run(ctx)
		}
	}

	srv := server.New(cfg.Token, reg, st, notify, cfg.RateRPM, cfg.RateBurst, cfg.ResolveTimeout, cfg.DataDir)
	server.SetRev(rev)
	httpSrv := &http.Server{
		// в контейнере слушаем все интерфейсы — наружу порт пробрасывается
		// публикой 127.0.0.1:8360:8360 (nginx на хосте), плюс токен на /dl
		Addr:              ":" + cfg.Port,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
	go func() {
		log.Printf("vdl %s на :%s (cobalt %s)", rev, cfg.Port, cfg.CobaltURL)
		if err := httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("http: %v", err)
		}
	}()

	<-ctx.Done()
	shCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	_ = httpSrv.Shutdown(shCtx)
	log.Printf("остановлен")
}

// gqlClient — HTTP-клиент GraphQL: без общего таймаута (бюджет — в ctx).
func gqlClient() *http.Client {
	return &http.Client{Transport: &http.Transport{
		TLSHandshakeTimeout: 10 * time.Second,
		IdleConnTimeout:     60 * time.Second,
	}}
}

// materializeCookies — активные аккаунты → cookies.json в формате cobalt
// ({twitter: ["auth_token=…; ct0=…"], instagram: […]}, массивы = ротация).
// cobalt сам обновляет куки в файле (refresh ct0): строки с тем же
// auth_token берём из его версии, чтобы не откатывать свежие токены.
func materializeCookies(path string, st *store.Store) error {
	existing := map[string][]string{}
	if raw, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(raw, &existing) // битый/пустой — просто перепишем
	}
	fresher := map[string]string{} // auth_token → строка кук из cobalt
	for _, arr := range existing {
		for _, hdr := range arr {
			if m := parseCookieHeader(hdr); m != nil {
				if at := m["auth_token"]; at != "" {
					fresher[at] = hdr
				}
			}
		}
	}

	bySite := map[string][]string{}
	for _, a := range st.List() {
		if a.Status != "active" {
			continue
		}
		hdr := a.CookieHeader()
		if hdr == "" {
			continue
		}
		if at := a.Cookies["auth_token"]; at != "" {
			if f, ok := fresher[at]; ok {
				hdr = f // cobalt обновил эту куку — не затираем
			}
		}
		bySite[a.Site] = append(bySite[a.Site], hdr)
	}
	raw, err := json.MarshalIndent(bySite, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	// 0666: cobalt в контейнере живёт под uid 1000 (node) и должен читать
	// и обновлять файл (refresh ct0); каталог 0700/65534 закрывает от остальных
	if err := os.WriteFile(tmp, raw, 0o666); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// parseCookieHeader — "k=v; k2=v2" → map (без валидации, для слияния кук).
func parseCookieHeader(s string) map[string]string {
	out := map[string]string{}
	for _, pair := range strings.Split(s, ";") {
		pair = strings.TrimSpace(pair)
		if k, v, ok := strings.Cut(pair, "="); ok {
			out[strings.TrimSpace(k)] = strings.TrimSpace(v)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// ytHost — youtube-хосты для цепочки age-fallback.
func ytHost(h string) bool {
	l := strings.ToLower(h)
	return strings.HasSuffix(l, "youtube.com") || strings.HasSuffix(l, "youtu.be") ||
		strings.HasSuffix(l, "youtube-nocookie.com")
}

// cobaltAge — второй cobalt (прокси через домашний IP) в цепочке youtube.
type cobaltAge struct{ *cobalt.Client }

func (c cobaltAge) Name() string            { return "cobalt-age" }
func (c cobaltAge) MatchHost(h string) bool { return ytHost(h) }
func (c cobaltAge) Resolve(ctx context.Context, u *url.URL) (*media.ResolveResult, error) {
	return c.Client.Resolve(ctx, u.String())
}

// cobaltTwWrap — cobalt, ограниченный youtube-хостами (звено цепочки).
type cobaltTwWrap struct{ *cobalt.Client }

func (c cobaltTwWrap) Name() string            { return "cobalt" }
func (c cobaltTwWrap) MatchHost(h string) bool { return ytHost(h) }
func (c cobaltTwWrap) Resolve(ctx context.Context, u *url.URL) (*media.ResolveResult, error) {
	return c.Client.Resolve(ctx, u.String())
}
