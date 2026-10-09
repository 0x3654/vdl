package twitter

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"vdl/internal/media"
	"vdl/internal/store"
)

// gqlTestEnv — фейковый x.com: и API, и страницы discovery, и бандлы.
type gqlTestEnv struct {
	srv      *httptest.Server
	store    *store.Store
	ids      *QueryIDCache
	gql      *GQLClient
	notified []string
	mu       sync.Mutex
}

func newGQLTestEnv(t *testing.T, handler http.HandlerFunc, accounts ...store.Account) *gqlTestEnv {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	st, err := store.Open(filepath.Join(t.TempDir(), "accounts.json"))
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	for i := range accounts {
		if err := st.Upsert(accounts[i]); err != nil {
			t.Fatalf("upsert: %v", err)
		}
	}
	ids := OpenQueryIDs(filepath.Join(t.TempDir(), "ids.json"))
	env := &gqlTestEnv{srv: srv, store: st, ids: ids}
	env.gql = NewGQL(srv.Client(), st, ids, func(text string) {
		env.mu.Lock()
		env.notified = append(env.notified, text)
		env.mu.Unlock()
	})
	oldBase, oldAPI := Base, apiURL
	Base, apiURL = srv.URL, srv.URL+"/i/api/graphql"
	t.Cleanup(func() { Base, apiURL = oldBase, oldAPI })
	return env
}

func TestGQLDeadCookieRotation(t *testing.T) {
	env := newGQLTestEnv(t, func(w http.ResponseWriter, r *http.Request) {
		// все запросы → 401 (куки мертвы)
		w.WriteHeader(401)
	}, twitterAcc("main"), twitterAcc("backup"))
	_, err := env.gql.TweetDetail(t.Context(), "123")
	if err == nil {
		t.Fatal("ждали ошибку")
	}
	e := media.AsError(err)
	if e == nil || e.Kind != media.ErrCookiesDead {
		t.Fatalf("ожидали ErrCookiesDead, получили %v", err)
	}
	// оба аккаунта помечены dead
	accs := env.store.List()
	for _, a := range accs {
		if a.Status != "dead" {
			t.Errorf("%s: статус %s, ждали dead", a.Label, a.Status)
		}
	}
	// алерт улетел (по одному на аккаунт)
	env.mu.Lock()
	n := len(env.notified)
	env.mu.Unlock()
	if n != 2 {
		t.Errorf("алертов %d, ждали 2", n)
	}
}

func TestGQLRateLimitFast(t *testing.T) {
	env := newGQLTestEnv(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(429)
	}, twitterAcc("main"))
	start := time.Now()
	_, err := env.gql.TweetDetail(t.Context(), "123")
	if time.Since(start) > time.Second {
		t.Fatalf("429 должен отдаваться мгновенно, заняло %s", time.Since(start))
	}
	e := media.AsError(err)
	if e == nil || e.Kind != media.ErrRateLimited {
		t.Fatalf("ожидали ErrRateLimited, получили %v", err)
	}
}

func TestGQLSuccessAndHeaders(t *testing.T) {
	fixture, err := os.ReadFile("../../testdata/tweetdetail_video.json")
	if err != nil {
		t.Skip("фикстура недоступна")
	}
	var gotCookie, gotCSRF, gotAuth, gotTxn string
	env := newGQLTestEnv(t, func(w http.ResponseWriter, r *http.Request) {
		gotCookie = r.Header.Get("Cookie")
		gotCSRF = r.Header.Get("x-csrf-token")
		gotAuth = r.Header.Get("Authorization")
		gotTxn = r.Header.Get("x-client-transaction-id")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(fixture)
	}, twitterAcc("main"))

	res, err := env.gql.TweetDetail(t.Context(), "2106743734079545531")
	if err != nil {
		t.Fatalf("tweetdetail: %v", err)
	}
	if len(res.Medias) == 0 {
		t.Fatal("медиа нет")
	}
	if !strings.HasPrefix(gotAuth, "Bearer ") {
		t.Errorf("authorization: %q", gotAuth)
	}
	if !strings.Contains(gotCookie, "auth_token=") || !strings.Contains(gotCookie, "ct0=") {
		t.Errorf("cookie: %q", gotCookie)
	}
	if gotCSRF == "" || gotTxn == "" {
		t.Errorf("csrf=%q txn=%q", gotCSRF, gotTxn)
	}
}

func TestGQLQueryIDDiscovery(t *testing.T) {
	// первый кандидат — 404, discovery отдаёт бандл с новым queryId,
	// повторный запрос через него — 200
	fixture, err := os.ReadFile("../../testdata/tweetdetail_video.json")
	if err != nil {
		t.Skip("фикстура недоступна")
	}
	const freshQID = "freshQID123"
	var env *gqlTestEnv
	var paths []string
	env = newGQLTestEnv(t, func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.Method+" "+r.URL.Path)
		if strings.Contains(r.URL.Path, "/i/api/graphql/") {
			if strings.Contains(r.URL.Path, freshQID) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write(fixture)
				return
			}
			w.WriteHeader(404) // известные кандидаты устарели
			return
		}
		// страницы discovery → бандл с операцией (плоский путь, как на abs.twimg.com)
		if strings.HasSuffix(r.URL.Path, ".js") {
			_, _ = w.Write([]byte(`e.exports={queryId:"` + freshQID + `",operationName:"TweetDetail"}`))
			return
		}
		// / и /explore отдают HTML со ссылкой на бандл
		_, _ = w.Write([]byte(`<script src="` + env.srv.URL + `/responsive-web/client-web/main.abc123.js"></script>`))
	}, twitterAcc("main"))
	// бандлы в тесте лежат на локальном сервере, а не на abs.twimg.com
	oldBundleRe := bundleRe
	bundleRe = regexp.MustCompile(regexp.QuoteMeta(env.srv.URL) +
		`/responsive-web/client-web/[A-Za-z0-9.\-]+\.js`)
	t.Cleanup(func() { bundleRe = oldBundleRe })

	res, err := env.gql.TweetDetail(t.Context(), "2106743734079545531")
	if err != nil {
		for _, p := range paths {
			t.Logf("hit: %s", p)
		}
		t.Fatalf("tweetdetail через discovery: %v", err)
	}
	if len(res.Medias) == 0 {
		t.Fatal("медиа нет")
	}
	// кэш сохранил свежий qid
	if got := env.ids.Candidates("TweetDetail")[0]; got != freshQID {
		t.Errorf("кэш qid: %q, ждали %q", got, freshQID)
	}
}

func twitterAcc(label string) store.Account {
	return store.Account{
		Site: "twitter", Label: label,
		Cookies: map[string]string{
			"auth_token": strings.Repeat("a", 40),
			"ct0":        strings.Repeat("b", 160),
		},
	}
}
