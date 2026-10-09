// queryid.go — ротация queryId GraphQL: X хэши эндпоинтов ротирует раз в
// недели. Известные кандидаты → кэш (TTL 7 дней) → discovery из JS-бандлов.
// Порт twitter_read.py.
package twitter

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

// Известные queryId (fallback). Источники: dumbtests (2026-04, рабочие),
// @steipete/bird 0.8.0 (2026-01). При 404 — авторефреш из бандлов x.com.
var fallbackIDs = map[string][]string{
	"UserByScreenName":     {"KybxDj9RrADIITXlGG8kpw", "G3KGOASz96M-Qu0nwmGXNg"},
	"UserTweets":           {"OeFjWKHutsuyWXZGmLr02A", "E3opETHurmVJflFsUBVuUQ"},
	"UserTweetsAndReplies": {"-4Ujf5pYzDdr_qY8qxgF9A"},
	"Bookmarks":            {"RV1g3b8n_SGOHwkqKYSCFw", "tmd4ifV8RHltzn8ymGg1aw"},
	"Likes":                {"o000A_Cp4JPOihhbeEgi0g", "ETJflBunfqNa1uE1mBPCaw"},
	"SearchTimeline":       {"KPSo2_UWdOMpPJhfT1Qg", "6AAys3t42mosm_yTI_QENg"},
	"TweetDetail":          {"FyR-GrebyjdkRoW1z6uCgQ", "_NvJCnIjOW__EP5-RF197A"},
}

// Дополнительные кандидаты из старых ревизий (пробуются после основных).
var extraIDs = map[string][]string{
	"SearchTimeline":   {"gkjsKepM6gl_HmFWoWKfgg", "lZ0tMo51JmojPnr5CymHyw"},
	"UserByScreenName": {"7mjxD3-C6BLvAM_ZoSrsEQ", "oUZZZ8M3uCdMHcBEV02AYA"},
	"UserTweets":       {"V7H0Ap3_Hh2FyS75OCDO3Q", "kOGiwBOSOCjMmntbL_ApXQ"},
}

var bundleRe = regexp.MustCompile(`https://abs\.twimg\.com/responsive-web/client-web(?:-legacy)?/[A-Za-z0-9.\-]+\.js`)

// Четыре порядка key:value в минифицированных бандлах (как в python;
// RE2 ограничивает повтор 1000 — в минифицированном коде их хватает,
// python-оригинал допускал 4000).
var opPatterns = []*regexp.Regexp{
	regexp.MustCompile(`e\.exports=\{queryId\s*:\s*["']([^"']+)["']\s*,\s*operationName\s*:\s*["']([^"']+)["']`),
	regexp.MustCompile(`e\.exports=\{operationName\s*:\s*["']([^"']+)["']\s*,\s*queryId\s*:\s*["']([^"']+)["']`),
	regexp.MustCompile(`(?s)operationName\s*[:=]\s*["']([^"']+)["'](.{0,1000}?)queryId\s*[:=]\s*["']([^"']+)["']`),
	regexp.MustCompile(`(?s)queryId\s*[:=]\s*["']([^"']+)["'](.{0,1000}?)operationName\s*[:=]\s*["']([^"']+)["']`),
}

const idsTTL = 7 * 24 * time.Hour

// QueryIDCache — файл-кэш + single-flight discovery (параллельные 404-шторма
// не должны качать бандлы одновременно).
type QueryIDCache struct {
	mu        sync.Mutex
	path      string
	fetched   time.Time
	ids       map[string]string
	discovery bool // discovery уже идёт
}

func OpenQueryIDs(path string) *QueryIDCache {
	c := &QueryIDCache{path: path, ids: map[string]string{}}
	raw, err := os.ReadFile(path)
	if err != nil {
		return c
	}
	var f struct {
		FetchedAt float64           `json:"fetchedAt"`
		IDs       map[string]string `json:"ids"`
	}
	if json.Unmarshal(raw, &f) == nil && len(f.IDs) > 0 {
		c.fetched = time.Unix(int64(f.FetchedAt), 0)
		c.ids = f.IDs
	}
	return c
}

// Candidates — свежий кэш → fallback → extra, без дублей.
func (c *QueryIDCache) Candidates(op string) []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []string
	seen := map[string]bool{}
	add := func(qid string) {
		if qid != "" && !seen[qid] {
			seen[qid] = true
			out = append(out, qid)
		}
	}
	add(c.ids[op])
	for _, q := range fallbackIDs[op] {
		add(q)
	}
	for _, q := range extraIDs[op] {
		add(q)
	}
	return out
}

// Fresh — живой ли кэш (для решения «обновить заранее»).
func (c *QueryIDCache) Fresh() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return time.Since(c.fetched) < idsTTL && len(c.ids) > 0
}

// Discover — собрать свежие queryId из JS-бандлов x.com. authToken нужен
// любой живой (страницы отдаются залогиненным), берём первый попавшийся.
func (c *QueryIDCache) Discover(ctx context.Context, hc *http.Client, authToken string) map[string]string {
	c.mu.Lock()
	if c.discovery { // уже кто-то качает — отдаём что есть
		defer c.mu.Unlock()
		return c.ids
	}
	c.discovery = true
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		c.discovery = false
		c.mu.Unlock()
	}()

	ids := c.discover(ctx, hc, authToken)
	if len(ids) > 0 {
		c.mu.Lock()
		c.ids = ids
		c.fetched = time.Now()
		c.mu.Unlock()
		raw, _ := json.Marshal(map[string]any{
			"fetchedAt": time.Now().Unix(), "ids": ids,
		})
		_ = os.WriteFile(c.path, raw, 0o600)
	}
	return ids
}

func (c *QueryIDCache) discover(ctx context.Context, hc *http.Client, authToken string) map[string]string {
	headers := map[string]string{
		"User-Agent": UA,
		"Accept":     "text/html,application/json;q=0.9,*/*;q=0.8",
	}
	if authToken != "" {
		headers["Cookie"] = "auth_token=" + authToken
	}
	var urls []string
	for _, page := range []string{Base + "/?lang=en", Base + "/explore"} {
		body := fetch(ctx, hc, page, headers)
		urls = append(urls, bundleRe.FindAllString(string(body), -1)...)
	}
	// уникальные, api-бандлы (все эндпоинты разом) — первыми
	uniq := map[string]bool{}
	var list []string
	for _, u := range urls {
		if !uniq[u] {
			uniq[u] = true
			list = append(list, u)
		}
	}
	sort.SliceStable(list, func(i, j int) bool {
		ai, aj := strings.Contains(list[i], "/api"), strings.Contains(list[j], "/api")
		if ai != aj {
			return ai
		}
		return list[i] < list[j]
	})

	ids := map[string]string{}
	for _, u := range list[:min(len(list), 12)] {
		body := fetch(ctx, hc, u, headers)
		if len(body) == 0 {
			continue
		}
		if len(body) > 8_000_000 {
			body = body[:8_000_000]
		}
		for i, pat := range opPatterns {
			for _, m := range pat.FindAllStringSubmatch(string(body), -1) {
				var qid, op string
				switch i {
				case 0:
					qid, op = m[1], m[2]
				case 1:
					op, qid = m[1], m[2]
				case 2:
					op, qid = m[1], m[3]
				case 3:
					qid, op = m[1], m[3]
				}
				if op != "" && qid != "" {
					if _, dup := ids[op]; !dup {
						ids[op] = qid
					}
				}
			}
		}
	}
	return ids
}

func fetch(ctx context.Context, hc *http.Client, url string, headers map[string]string) []byte {
	if hc == nil {
		return nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := hc.Do(req)
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
		return nil
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, 12_000_000))
	if err != nil {
		return nil
	}
	return b
}
