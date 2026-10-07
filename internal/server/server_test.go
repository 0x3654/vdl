package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"vdl/internal/media"
	"vdl/internal/store"
)

// fakeRes — резолвер с фиксированным ответом.
type fakeRes struct {
	host string
	best string
	err  error
}

func (f fakeRes) Name() string            { return "fake" }
func (f fakeRes) MatchHost(h string) bool { return h == f.host || f.host == "*" }
func (f fakeRes) Resolve(ctx context.Context, u *url.URL) (*media.ResolveResult, error) {
	if f.err != nil {
		return nil, f.err
	}
	return &media.ResolveResult{Site: "test", Medias: []media.Media{{Type: "video", Best: f.best}}}, nil
}

func newTestServer(t *testing.T, res media.Resolver) *Server {
	t.Helper()
	st, err := store.Open(t.TempDir() + "/a.json")
	if err != nil {
		t.Fatal(err)
	}
	reg := &media.Registry{}
	reg.Register(res)
	return New("secrettoken", reg, st, nil, 1000, 1000, 5*time.Second, t.TempDir())
}

func get(t *testing.T, s *Server, path string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	return rec
}

func TestTokenRequired(t *testing.T) {
	s := newTestServer(t, fakeRes{host: "x.com", best: "https://video.twimg.com/a.mp4"})
	if code := get(t, s, "/dl?url=https://x.com/a/status/1").Code; code != 401 {
		t.Errorf("без токена: %d, ждали 401", code)
	}
	if code := get(t, s, "/dl?token=wrong&url=https://x.com/a/status/1").Code; code != 401 {
		t.Errorf("чужой токен: %d, ждали 401", code)
	}
}

func TestRedirect(t *testing.T) {
	s := newTestServer(t, fakeRes{host: "x.com", best: "https://video.twimg.com/a.mp4"})
	rec := get(t, s, "/dl?token=secrettoken&url=https://x.com/a/status/123")
	if rec.Code != 302 {
		t.Fatalf("код %d: %s", rec.Code, rec.Body.String())
	}
	if loc := rec.Header().Get("Location"); loc != "https://video.twimg.com/a.mp4" {
		t.Errorf("location: %s", loc)
	}
}

func TestRedirectCacheHit(t *testing.T) {
	calls := 0
	res := fakeRes{host: "x.com", best: "https://video.twimg.com/a.mp4"}
	s := newTestServer(t, &countingRes{res, &calls})
	get(t, s, "/dl?token=secrettoken&url=https://x.com/a/status/123")
	get(t, s, "/dl?token=secrettoken&url=https://x.com/a/status/123")
	if calls != 1 {
		t.Errorf("резолвер вызван %d раз, кэш не сработал", calls)
	}
}

type countingRes struct {
	fakeRes
	calls *int
}

func (c *countingRes) Resolve(ctx context.Context, u *url.URL) (*media.ResolveResult, error) {
	*c.calls++
	return c.fakeRes.Resolve(ctx, u)
}

func TestSSRFPrivateBlocked(t *testing.T) {
	s := newTestServer(t, fakeRes{host: "x.com", best: "http://127.0.0.1:8080/evil"})
	if code := get(t, s, "/dl?token=secrettoken&url=https://x.com/a/status/1").Code; code != 502 {
		t.Errorf("приватный редирект: %d, ждали 502", code)
	}
	s2 := newTestServer(t, fakeRes{host: "x.com", best: "http://192.168.1.1/x"})
	if code := get(t, s2, "/dl?token=secrettoken&url=https://x.com/a/status/1").Code; code != 502 {
		t.Errorf("приватный редирект 2: %d, ждали 502", code)
	}
}

func TestUnsupportedHost(t *testing.T) {
	s := newTestServer(t, fakeRes{host: "x.com", best: "x"})
	if code := get(t, s, "/dl?token=secrettoken&url=https://example.com/video").Code; code != 400 {
		t.Errorf("чужой хост: %d, ждали 400", code)
	}
	if code := get(t, s, "/dl?token=secrettoken").Code; code != 400 {
		t.Errorf("без url: %d, ждали 400", code)
	}
}

func TestRateLimit(t *testing.T) {
	st, _ := store.Open(t.TempDir() + "/a.json")
	reg := &media.Registry{}
	reg.Register(fakeRes{host: "x.com", best: "https://video.twimg.com/a.mp4"})
	s := New("t", reg, st, nil, 60, 2, 5*time.Second, t.TempDir()) // burst 2
	req := httptest.NewRequest(http.MethodGet, "/dl?token=t&url=https://x.com/a/status/1", nil)
	req.RemoteAddr = "10.0.0.1:1234"
	for i := 0; i < 2; i++ {
		rec := httptest.NewRecorder()
		s.Handler().ServeHTTP(rec, req)
		if rec.Code != 302 {
			t.Fatalf("запрос %d: %d", i, rec.Code)
		}
	}
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != 429 {
		t.Errorf("третий подряд: %d, ждали 429", rec.Code)
	}
}

func TestLenientURLParam(t *testing.T) {
	// iOS Shortcuts подставляет ссылку без кодирования — ? и & внутри
	s := newTestServer(t, fakeRes{host: "x.com", best: "https://video.twimg.com/a.mp4"})
	// незакодированный url с ?s=46 внутри (url — последний параметр)
	rec := get(t, s, "/dl?token=secrettoken&url=https://x.com/a/status/123?s=46")
	if rec.Code != 302 {
		t.Fatalf("сырой url: %d %s", rec.Code, rec.Body.String())
	}
	// закодированный (обычные клиенты) тоже работает
	rec = get(t, s, "/dl?token=secrettoken&url=https%3A%2F%2Fx.com%2Fa%2Fstatus%2F123")
	if rec.Code != 302 {
		t.Fatalf("кодированный url: %d", rec.Code)
	}
}

func TestListAndIndex(t *testing.T) {
	s := newTestServer(t, multiRes{host: "x.com", bests: []string{
		"https://video.twimg.com/a.mp4", "https://pbs.twimg.com/b.jpg"}})
	// list=1: JSON со всеми медиа
	rec := get(t, s, "/dl?token=secrettoken&list=1&url=https://x.com/a/status/1")
	if rec.Code != 200 || !stringContains(rec.Body.String(), "a.mp4") ||
		!stringContains(rec.Body.String(), "b.jpg") {
		t.Fatalf("list: %d %s", rec.Code, rec.Body.String())
	}
	// index=1: вторая (картинка)
	rec = get(t, s, "/dl?token=secrettoken&index=1&url=https://x.com/a/status/1")
	if loc := rec.Header().Get("Location"); loc != "https://pbs.twimg.com/b.jpg" {
		t.Errorf("index=1 location: %s", loc)
	}
	// дефолт — первое
	rec = get(t, s, "/dl?token=secrettoken&url=https://x.com/a/status/1")
	if loc := rec.Header().Get("Location"); loc != "https://video.twimg.com/a.mp4" {
		t.Errorf("default location: %s", loc)
	}
}

type multiRes struct {
	host  string
	bests []string
}

func (m multiRes) Name() string            { return "multi" }
func (m multiRes) MatchHost(h string) bool { return h == m.host }
func (m multiRes) Resolve(ctx context.Context, u *url.URL) (*media.ResolveResult, error) {
	medias := make([]media.Media, len(m.bests))
	for i, b := range m.bests {
		medias[i] = media.Media{Type: "video", Best: b}
	}
	return &media.ResolveResult{Site: "twitter", Medias: medias}, nil
}

func TestHealthz(t *testing.T) {
	s := newTestServer(t, fakeRes{host: "x.com", best: "x"})
	rec := get(t, s, "/healthz")
	if rec.Code != 200 || !jsonOK(rec.Body.String()) {
		t.Errorf("healthz: %d %s", rec.Code, rec.Body.String())
	}
}

func jsonOK(body string) bool {
	return len(body) > 0 && body[0] == '{'
}

func TestAdminAccountsMasks(t *testing.T) {
	st, _ := store.Open(t.TempDir() + "/a.json")
	_ = st.Upsert(store.Account{Site: "twitter", Label: "m",
		Cookies: map[string]string{"auth_token": "SECRETVALUE"}})
	reg := &media.Registry{}
	s := New("t", reg, st, nil, 60, 10, 5*time.Second, t.TempDir())
	rec := get(t, s, "/admin/accounts?token=t")
	if rec.Code != 200 {
		t.Fatalf("%d", rec.Code)
	}
	if body := rec.Body.String(); len(body) > 0 && stringContains(body, "SECRETVALUE") {
		t.Errorf("куки утекли в admin endpoint")
	}
}

func stringContains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
