package twitter

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
)

func TestFXTweetFixture(t *testing.T) {
	raw, err := os.ReadFile("../../testdata/fx_video.json")
	if err != nil {
		t.Skipf("фикстура недоступна: %v", err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(raw)
	}))
	defer srv.Close()
	old := fxBase
	fxBase = srv.URL
	defer func() { fxBase = old }()

	fx := NewFX(srv.Client())
	res, err := fx.Tweet(t.Context(), "ifillwicks", "2106743734079545531")
	if err != nil {
		t.Fatalf("tweet: %v", err)
	}
	if res.Source != "fxtwitter" || len(res.Medias) != 1 {
		t.Fatalf("source=%s medias=%d", res.Source, len(res.Medias))
	}
	m := res.Medias[0]
	if m.Type != "video" || m.Best == "" {
		t.Errorf("тип=%s best=%q", m.Type, m.Best)
	}
	if m.Poster == "" || m.Duration <= 0 || m.Width == 0 {
		t.Errorf("метаданные: poster=%q dur=%f w=%d", m.Poster, m.Duration, m.Width)
	}
	if !strings.Contains(m.Best, ".mp4") {
		t.Errorf("best не mp4: %s", m.Best)
	}
}

func TestFXQuoteChain(t *testing.T) {
	raw, err := os.ReadFile("../../testdata/fx_quote.json")
	if err != nil {
		t.Skipf("фикстура недоступна: %v", err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(raw)
	}))
	defer srv.Close()
	old := fxBase
	fxBase = srv.URL
	defer func() { fxBase = old }()

	res, err := NewFX(srv.Client()).Tweet(t.Context(), "tumbalperaya", "2107124790339477720")
	if err != nil {
		t.Fatalf("tweet: %v", err)
	}
	if len(res.Medias) == 0 {
		t.Fatal("медиа из quote не извлечены")
	}
	if !strings.Contains(res.Medias[0].Best, ".mp4") {
		t.Errorf("best не mp4: %s", res.Medias[0].Best)
	}
}

func TestFXNotFound(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(404)
		_, _ = w.Write([]byte(`{"code":404,"message":"твит не найден"}`))
	}))
	defer srv.Close()
	old := fxBase
	fxBase = srv.URL
	defer func() { fxBase = old }()

	_, err := NewFX(srv.Client()).Tweet(t.Context(), "x", "1")
	if err == nil {
		t.Fatal("ждали ошибку")
	}
}

func TestFXResolve(t *testing.T) {
	fx := NewFX(nil)
	u, _ := url.Parse("https://x.com/a/status/123")
	// fxBase ведёт на настоящий api.fxtwitter.com — тест без сети не гоняем,
	// проверяем только маршрутизацию/нормализацию через ошибку ниже.
	if _, err := fx.Resolve(t.Context(), u); err == nil {
		t.Fatal("без сети должно упасть")
	}
}
