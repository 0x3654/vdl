// Package server — HTTP: /dl (шорткат), /healthz, /admin/accounts, / (UI).
// Логирование без токенов (они в query) и без кук; токен — constant-time.
package server

import (
	"crypto/subtle"
	_ "embed"
	"encoding/json"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"vdl/internal/media"
	"vdl/internal/store"
)

//go:embed ui.html
var uiHTML []byte

var rev = "unknown" // -ldflags -X (проставляет сборка); main его обновляет

// SetRev — прокинуть версию сборки из main.
func SetRev(r string) { rev = r }

type Server struct {
	Token          string
	Reg            *media.Registry
	Store          *store.Store
	Notify         func(string)
	Cache          *resolveCache
	Limit          *rateLimiter
	ResolveTimeout time.Duration
	DataDir        string // /shortcut артефакт: DATA_DIR/shortcut.shortcut
}

func New(token string, reg *media.Registry, st *store.Store, notify func(string),
	rpm, burst int, resolveTimeout time.Duration, dataDir string) *Server {
	return &Server{
		Token: token, Reg: reg, Store: st, Notify: notify,
		Cache:          newResolveCache(512, 10*time.Minute),
		Limit:          newRateLimiter(rpm, burst),
		ResolveTimeout: resolveTimeout,
		DataDir:        dataDir,
	}
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", s.healthz)
	mux.HandleFunc("/dl", s.dl)
	// /f — стрим-прокси медиа: iOS Shortcuts не следует 302, файл нужен байтами
	mux.HandleFunc("/f", s.handleFile)
	// /list — то же, что /dl?list=1, но без зависимости от параметра:
	// шорткат всегда получает JSON со всеми медиа (ссылки — на наш /f)
	mux.HandleFunc("/list", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if q.Get("list") == "" {
			q.Set("list", "1")
			r.URL.RawQuery = q.Encode()
		}
		s.dl(w, r)
	})
	mux.HandleFunc("/admin/accounts", s.adminAccounts)
	// /shortcut — готовый подписанный iOS-шорткат (деплой кладёт его в DATA_DIR,
	// собирает scripts/build-shortcut.py с прод-токеном); гейт — тот же токен
	mux.HandleFunc("/shortcut", s.shortcutFile)
	mux.HandleFunc("/", s.index)
	return s.logMW(mux)
}

// shortcutFile — отдача подписанного артефакта из DATA_DIR/shortcut.shortcut.
// Имя и тип обязательны: без расширения iOS открывает файл как текст.
func (s *Server) shortcutFile(w http.ResponseWriter, r *http.Request) {
	if !s.checkToken(r) {
		writeJSON(w, 401, map[string]string{"error": "bad token"})
		return
	}
	if s.DataDir == "" {
		writeJSON(w, 404, map[string]string{"error": "файл не настроен"})
		return
	}
	raw, err := os.ReadFile(filepath.Join(s.DataDir, "shortcut.shortcut"))
	if err != nil {
		writeJSON(w, 404, map[string]string{"error": "файл не положен (см. scripts/build-shortcut.py)"})
		return
	}
	w.Header().Set("Content-Type", "application/x-shortcut")
	w.Header().Set("Content-Disposition", `attachment; filename="vdl.shortcut"`)
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(200)
	_, _ = w.Write(raw)
}

func (s *Server) index(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(uiHTML)
}

func (s *Server) healthz(w http.ResponseWriter, r *http.Request) {
	active, dead := 0, 0
	if s.Store != nil {
		active, dead = s.Store.Counts()
	}
	writeJSON(w, 200, map[string]any{
		"ok": true, "rev": rev,
		"accounts": map[string]int{"active": active, "dead": dead},
	})
}

func (s *Server) adminAccounts(w http.ResponseWriter, r *http.Request) {
	if !s.checkToken(r) {
		writeJSON(w, 401, map[string]string{"error": "bad token"})
		return
	}
	if s.Store == nil {
		writeJSON(w, 200, map[string]any{"accounts": []any{}})
		return
	}
	type accOut struct {
		Site, Label, Status string
		LastOK, LastUsed    string
		FailMsg             string
		Cookies             []string // имена ключей, не значения
	}
	out := []accOut{}
	for _, a := range s.Store.List() {
		keys := make([]string, 0, len(a.Cookies))
		for k := range a.Cookies {
			keys = append(keys, k)
		}
		out = append(out, accOut{
			a.Site, a.Label, a.Status,
			a.LastOK.Format(time.RFC3339), a.LastUsed.Format(time.RFC3339),
			a.FailMsg, keys,
		})
	}
	writeJSON(w, 200, map[string]any{"accounts": out})
}

// checkToken — ?token= или Authorization: Bearer, constant-time.
func (s *Server) checkToken(r *http.Request) bool {
	tok := r.URL.Query().Get("token")
	if tok == "" {
		h := r.Header.Get("Authorization")
		tok = strings.TrimPrefix(h, "Bearer ")
	}
	if tok == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(tok), []byte(s.Token)) == 1
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

// logMW — метод, путь (без query), IP, статус, длительность.
func (s *Server) logMW(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		sw := &statusWriter{ResponseWriter: w, code: 200}
		next.ServeHTTP(sw, r)
		ip, _, _ := net.SplitHostPort(r.RemoteAddr)
		log.Printf("%s %s ip=%s -> %d (%s)", r.Method, r.URL.Path, ip,
			sw.code, time.Since(start).Round(time.Millisecond))
	})
}

type statusWriter struct {
	http.ResponseWriter
	code int
}

func (w *statusWriter) WriteHeader(code int) {
	w.code = code
	w.ResponseWriter.WriteHeader(code)
}

// handleResolveError — единый маппинг ошибок резолва в HTTP.
func (s *Server) handleResolveError(w http.ResponseWriter, err error) {
	e := media.AsError(err)
	if e == nil {
		writeJSON(w, 502, map[string]string{"error": "upstream: " + err.Error()})
		return
	}
	switch e.Kind {
	case media.ErrBadURL:
		writeJSON(w, 400, map[string]string{"error": e.Detail})
	case media.ErrNoMedia:
		writeJSON(w, 422, map[string]string{"error": e.Detail})
	case media.ErrCookiesDead:
		if s.Notify != nil {
			s.Notify("⚠️ /dl: " + e.Error() + " — нужно обновить куки")
		}
		writeJSON(w, 503, map[string]string{"error": e.Detail})
	case media.ErrRateLimited:
		w.Header().Set("Retry-After", "60")
		writeJSON(w, 503, map[string]string{"error": e.Detail})
	default:
		writeJSON(w, 502, map[string]string{"error": e.Detail})
	}
}
