package twitter

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"vdl/internal/media"
)

func TestNormalize(t *testing.T) {
	cases := []struct {
		raw, screen, id string
		wantErr         bool
	}{
		{"https://x.com/durov/status/1234567890123456789", "durov", "1234567890123456789", false},
		{"https://twitter.com/durov/status/1234567890123456789?s=20&t=abc#m", "durov", "1234567890123456789", false},
		{"https://www.x.com/durov/statuses/99", "durov", "99", false},
		{"https://mobile.twitter.com/abc123_/status/555", "abc123_", "555", false},
		{"https://x.com/i/status/777", "i", "777", false},
		{"https://x.com/durov", "", "", true},
		{"https://youtube.com/watch?v=1", "", "", true},
		{"https://x.com/durov/status/abc", "", "", true},
	}
	for _, c := range cases {
		u, err := url.Parse(c.raw)
		if err != nil {
			t.Fatalf("parse %s: %v", c.raw, err)
		}
		screen, id, err := Normalize(u)
		if c.wantErr {
			if err == nil {
				t.Errorf("%s: ждали ошибку, получили %s/%s", c.raw, screen, id)
			}
			continue
		}
		if err != nil {
			t.Errorf("%s: %v", c.raw, err)
			continue
		}
		if screen != c.screen || id != c.id {
			t.Errorf("%s: получили %s/%s, ждали %s/%s", c.raw, screen, id, c.screen, c.id)
		}
	}
}

func TestIsTwitterHost(t *testing.T) {
	for _, h := range []string{"x.com", "X.com", "www.twitter.com", "mobile.x.com", "t.co", "v.t.co"} {
		if !IsTwitterHost(h) {
			t.Errorf("%s должен быть twitter-хостом", h)
		}
	}
	for _, h := range []string{"fxtwitter.com", "api.fxtwitter.com", "youtube.com", "x.com.evil.io"} {
		if IsTwitterHost(h) {
			t.Errorf("%s не twitter-хост", h)
		}
	}
}

func TestExpandShortLink(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Location", "https://x.com/someone/status/42")
		w.WriteHeader(301)
	}))
	defer srv.Close()
	expanded, err := ExpandShortLink(t.Context(), srv.Client(), srv.URL+"/abc")
	if err != nil {
		t.Fatalf("expand: %v", err)
	}
	if expanded != "https://x.com/someone/status/42" {
		t.Fatalf("получили %s", expanded)
	}
}

// Строим резолвер-хелпер для полной цепочки тестов ниже.
func testResult(site string) *media.ResolveResult {
	return &media.ResolveResult{Site: site, Medias: []media.Media{{Type: "video", Best: "https://video.twimg.com/x.mp4"}}}
}
