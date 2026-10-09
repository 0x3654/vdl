// dl.go — главный эндпоинт шортката: GET /dl?token=…&url=<ссылка>.
// Резолв → 302 на CDN или на /tunnel (cobalt качает сам). SSRF-фильтр,
// кэш резолвов (глотает ретраи шортката), rate-limit на IP.
package server

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"vdl/internal/media"
)

// UA — как браузер: CDN твиттера привередничает к клиенту.
const UA = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 " +
	"(KHTML, like Gecko) Chrome/129.0.0.0 Safari/537.36"

func (s *Server) dl(w http.ResponseWriter, r *http.Request) {
	ip := clientIP(r)
	if !s.Limit.allow(ip) {
		writeJSON(w, 429, map[string]string{"error": "slow down"})
		return
	}
	if !s.checkToken(r) {
		writeJSON(w, 401, map[string]string{"error": "bad token"})
		return
	}
	rawURL := strings.TrimSpace(extractURLParam(r))
	if rawURL == "" {
		writeJSON(w, 400, map[string]string{"error": "нет параметра url"})
		return
	}
	u, err := url.Parse(rawURL)
	if err != nil || u.Host == "" || !u.IsAbs() {
		writeJSON(w, 400, map[string]string{"error": "не ссылка"})
		return
	}
	host := strings.ToLower(u.Hostname())
	resolver := s.Reg.ForHost(host)
	if resolver == nil {
		writeJSON(w, 400, map[string]string{"error": "сайт не поддерживается: " + host})
		return
	}

	// кэш: ретраи шортката и повторные запросы не дёргают апстрим
	medias, ok := s.Cache.get(rawURL)
	if !ok {
		var rerr error
		medias, rerr = s.resolveDeep(r.Context(), u)
		if rerr != nil {
			s.handleResolveError(w, rerr)
			return
		}
		if len(medias) == 0 {
			writeJSON(w, 422, map[string]string{"error": "в посте нет медиа"})
			return
		}
		s.Cache.put(rawURL, medias)
	}
	if len(medias) == 0 {
		writeJSON(w, 422, map[string]string{"error": "в посте нет медиа"})
		return
	}

	// list=1: шорткат качает всё (квоты, галереи) — JSON со ссылками на наш
	// прокси /f: iOS Get Contents of URL не следует 302, отдаём байты сами
	if strings.EqualFold(r.URL.Query().Get("list"), "1") {
		proxied := make([]string, len(medias))
		for i, m := range medias {
			if isTunnelURL(r, m) {
				proxied[i] = m // tunnel cobalt уже наш домен (ютуб IP-bound)
			} else {
				proxied[i] = proxyURL(r, m)
			}
		}
		writeJSON(w, 200, map[string]any{"ok": true, "count": len(proxied), "medias": proxied})
		return
	}
	idx := 0
	if v := r.URL.Query().Get("index"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 && n < len(medias) {
			idx = n
		}
	}
	s.redirect(w, r, medias[idx])
}

// isTunnelURL — ссылка на /tunnel одного из наших cobalt (свой домен).
func isTunnelURL(r *http.Request, mediaURL string) bool {
	_ = r
	// tunnel-ссылки генерируют только наши cobalt (vdl./ae2. домены);
	// резолверы чужих /tunnel не отдают
	u, err := url.Parse(mediaURL)
	return err == nil && u.Host != "" && strings.Contains(u.Path, "/tunnel")
}

// proxyURL — ссылка на наш стрим-прокси: тот же хост/порт/схема, что у входящего
// запроса (за nginx это https публичного домена), путь /f?url=….
func proxyURL(r *http.Request, mediaURL string) string {
	scheme := "http"
	if r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https" {
		scheme = "https"
	}
	host := r.Host
	if host == "" {
		host = "127.0.0.1:" + port()
	}
	u := url.URL{Scheme: scheme, Host: host, Path: "/f",
		RawQuery: "url=" + url.QueryEscape(mediaURL)}
	return u.String()
}

func port() string {
	if p := os.Getenv("PORT"); p != "" {
		return p
	}
	return "8360"
}

// handleFile — GET /f?url=<медиа-CDN>: скачать и отдать байтами (стрим).
// iOS Shortcuts не следует 302 — это основной путь файла в шорткат.
func (s *Server) handleFile(w http.ResponseWriter, r *http.Request) {
	raw := strings.TrimSpace(r.URL.Query().Get("url"))
	u, err := url.Parse(raw)
	if err != nil || !isMediaHost(u) {
		writeJSON(w, 400, map[string]string{"error": "не медиа-ссылка"})
		return
	}
	req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, raw, nil)
	if err != nil {
		writeJSON(w, 502, map[string]string{"error": err.Error()})
		return
	}
	req.Header.Set("User-Agent", UA)
	req.Header.Set("Referer", "https://x.com/")
	if inm := r.Header.Get("If-None-Match"); inm != "" {
		req.Header.Set("If-None-Match", inm)
	}
	resp, err := fileClient.Do(req)
	if err != nil {
		writeJSON(w, 502, map[string]string{"error": "CDN: " + err.Error()})
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		writeJSON(w, 502, map[string]string{"error": "CDN " + strconv.Itoa(resp.StatusCode)})
		return
	}
	h := w.Header()
	for _, k := range []string{"Content-Type", "Content-Length", "ETag", "Last-Modified"} {
		if v := resp.Header.Get(k); v != "" {
			h.Set(k, v)
		}
	}
	h.Set("Cache-Control", "no-store")
	w.WriteHeader(200)
	_, _ = io.Copy(w, resp.Body)
}

// isMediaHost — только CDN медиа: наш /f не открытый прокси.
func isMediaHost(u *url.URL) bool {
	if u == nil || (u.Scheme != "https" && u.Scheme != "http") {
		return false
	}
	host := strings.ToLower(u.Hostname())
	for _, suffix := range []string{
		".twimg.com", ".cdninstagram.com", ".fbcdn.net", ".googlevideo.com",
		".akamaized.net", ".cloudfront.net", ".video.pscp.tv",
	} {
		if strings.HasSuffix(host, suffix) {
			return true
		}
	}
	return false
}

// fileClient — без общего таймаута (стрим может быть долгим), idle короткие.
var fileClient = &http.Client{
	Transport: &http.Transport{
		TLSHandshakeTimeout:   10 * time.Second,
		IdleConnTimeout:       90 * time.Second,
		ResponseHeaderTimeout: 30 * time.Second,
	},
}

// redirect — финальный 302 после SSRF-фильтра (media.IsPublicHost:
// https/http, без userinfo, без приватных IP-литералов; тоннель cobalt
// живёт под нашим же доменом — тоже легитимная цель).
func (s *Server) redirect(w http.ResponseWriter, r *http.Request, best string) {
	u, err := url.Parse(best)
	if err != nil || !media.IsPublicHost(u) {
		writeJSON(w, 502, map[string]string{"error": "резолвер вернул плохую ссылку"})
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	http.Redirect(w, r, best, http.StatusFound)
}

// clientIP — за nginx берём X-Real-IP, иначе RemoteAddr.
func clientIP(r *http.Request) string {
	if ip := r.Header.Get("X-Real-IP"); ip != "" {
		return ip
	}
	if ip := r.Header.Get("X-Forwarded-For"); ip != "" {
		return strings.TrimSpace(strings.Split(ip, ",")[0])
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// extractURLParam — url должен быть ПОСЛЕДНИМ параметром запроса
// (token=…&url=<ссылка>). iOS Shortcuts подставляет переменную в поле URL
// без percent-кодирования: ?s=46 и & внутри ссылки ломают обычный парсер
// query. Поэтому берём всё после «url=» как есть и декодируем мягко.
func extractURLParam(r *http.Request) string {
	q := r.URL.RawQuery
	switch {
	case strings.HasPrefix(q, "url="):
		q = q[len("url="):]
	default:
		i := strings.Index(q, "&url=")
		if i < 0 {
			return r.URL.Query().Get("url") // обычный закодированный случай
		}
		q = q[i+len("&url="):]
	}
	if dec, err := url.QueryUnescape(q); err == nil && dec != "" {
		return dec
	}
	return q
}

// resolveDeep — резолв + рекурсия по Links: пост может быть текстом со
// ссылками на другие посты/сервисы (твит со ссылкой на твит или youtube).
// Вглубь до 2 уровней, всего не больше 6 вложенных резолвов.
func (s *Server) resolveDeep(parent context.Context, u *url.URL) ([]string, error) {
	type item struct {
		u     *url.URL
		depth int
	}
	var medias []string
	seenURL := map[string]bool{}
	queue := []item{{u, 0}}
	resolves := 0
	for len(queue) > 0 {
		it := queue[0]
		queue = queue[1:]
		ctx, cancel := context.WithTimeout(parent, s.ResolveTimeout)
		res, err := s.Reg.ForHost(strings.ToLower(it.u.Hostname())).Resolve(ctx, it.u)
		cancel()
		if err != nil {
			if it.depth == 0 {
				return nil, err
			}
			continue // вложенный линк не разрешался — молча пропускаем
		}
		resolves++
		for _, m := range res.Medias {
			if m.Best != "" && !seenURL[m.Best] {
				seenURL[m.Best] = true
				medias = append(medias, m.Best)
			}
		}
		if it.depth >= 2 || resolves >= 6 {
			continue
		}
		for _, l := range res.Links {
			lu, err := url.Parse(l)
			if err != nil || seenURL[l] {
				continue
			}
			seenURL[l] = true
			if s.Reg.ForHost(strings.ToLower(lu.Hostname())) != nil {
				queue = append(queue, item{lu, it.depth + 1})
			}
		}
	}
	return medias, nil
}
