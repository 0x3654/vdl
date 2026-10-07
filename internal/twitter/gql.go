// gql.go — GraphQL веб-API X под куками аккаунтов: TweetDetail для резолва
// и UserByScreenName-проба для health-check. Порт twitter_read.py:
// bearer, заголовки, queryId-ротация, 401=куки умерли, 429=не ждать.
// Отличия от CLI-оригинала: вместо сна на 429 отдаём мгновенный rate-limit,
// вместо смерти на 401 — ротация на следующий аккаунт.
package twitter

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"vdl/internal/media"
	"vdl/internal/store"
)

var (
	Base = "https://x.com"
	UA   = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 " +
		"(KHTML, like Gecko) Chrome/129.0.0.0 Safari/537.36"
	apiURL = "https://x.com/i/api/graphql"
	// Публичный веб-bearer (тот же, что шлёт браузер на x.com).
	bearer = "AAAAAAAAAAAAAAAAAAAAANRILgAAAAAAnNwIzUejRCOuH5E6I8xnZz4puTs%3D" +
		"1Zv7ttfk8LF81IUq16cHjhLTvJu4FA33AGWWjCpTnA"
	// Набор фичей @steipete/bird 0.8.0 (фиксили 400-ки из-за флагов).
	featuresTweet = `{
 "rweb_tipjar_consumption_enabled":true,
 "responsive_web_graphql_exclude_directive_enabled":true,
 "verified_phone_label_enabled":false,
 "creator_subscriptions_tweet_preview_api_enabled":true,
 "responsive_web_graphql_timeline_navigation_enabled":true,
 "responsive_web_graphql_skip_user_profile_image_extensions_enabled":false,
 "communities_web_enable_tweet_community_results_fetch":true,
 "c9s_tweet_anatomy_moderator_badge_enabled":true,
 "articles_preview_enabled":true,
 "responsive_web_edit_tweet_api_enabled":true,
 "graphql_is_translatable_tweet_is_translatable_enabled":true,
 "view_counts_everywhere_api_enabled":true,
 "longform_notetweets_consumption_enabled":true,
 "responsive_web_twitter_article_tweet_consumption_enabled":true,
 "tweet_awards_web_tipping_enabled":false,
 "creator_subscriptions_quote_tweet_preview_enabled":false,
 "freedom_of_speech_not_reach_fetch_enabled":true,
 "standardized_nudges_misinfo":true,
 "tweet_with_visibility_results_prefer_gql_limited_actions_policy_enabled":true,
 "rweb_video_timestamps_enabled":true,
 "longform_notetweets_rich_text_read_enabled":true,
 "longform_notetweets_inline_media_enabled":true,
 "responsive_web_enhance_cards_enabled":false,
 "rweb_video_screen_enabled":true,
 "profile_label_improvements_pcf_label_in_post_enabled":true,
 "responsive_web_profile_redirect_enabled":true,
 "premium_content_api_read_enabled":false,
 "responsive_web_grok_analyze_button_fetch_trends_enabled":false,
 "responsive_web_grok_analyze_post_followups_enabled":false,
 "responsive_web_grok_annotations_enabled":false,
 "responsive_web_jetfuel_frame":true,
 "post_ctas_fetch_enabled":true,
 "responsive_web_grok_share_attachment_enabled":true,
 "responsive_web_grok_show_grok_translated_post":false,
 "responsive_web_grok_analysis_button_from_backend":true,
 "responsive_web_grok_image_annotation_enabled":true,
 "responsive_web_grok_imagine_annotation_enabled":true,
 "responsive_web_grok_community_note_auto_translation_is_enabled":false,
 "articles_rest_api_enabled":true,
 "responsive_web_twitter_article_plain_text_enabled":true,
 "responsive_web_twitter_article_seed_tweet_detail_enabled":true
}`
	featuresUser = `{
 "hidden_profile_subscriptions_enabled":true,
 "rweb_tipjar_consumption_enabled":true,
 "responsive_web_graphql_exclude_directive_enabled":true,
 "verified_phone_label_enabled":false,
 "subscriptions_verification_info_is_identity_verified_enabled":true,
 "subscriptions_verification_info_verified_since_enabled":true,
 "highlights_tweets_tab_ui_enabled":true,
 "responsive_web_twitter_article_notes_tab_enabled":true,
 "subscriptions_feature_can_gift_premium":false,
 "creator_subscriptions_tweet_preview_api_enabled":true,
 "responsive_web_graphql_skip_user_profile_image_extensions_enabled":false,
 "responsive_web_graphql_timeline_navigation_enabled":true
}`
)

// GQLClient — GraphQL-клиент с ротацией аккаунтов из store.
type GQLClient struct {
	HC     *http.Client
	Store  *store.Store
	IDs    *QueryIDCache
	Notify func(text string) // алерт боту, неблокирующий
}

func (g *GQLClient) Name() string            { return "graphql" }
func (g *GQLClient) MatchHost(h string) bool { return IsTwitterHost(h) }

func (g *GQLClient) apiHeaders(authToken, ct0 string) http.Header {
	// x-client-transaction-id: 16 случайных байт hex, как secrets.token_hex(16)
	raw := make([]byte, 16)
	_, _ = rand.Read(raw)
	h := http.Header{}
	h.Set("Authorization", "Bearer "+bearer)
	h.Set("x-csrf-token", ct0)
	h.Set("x-twitter-auth-type", "OAuth2Session")
	h.Set("x-twitter-active-user", "yes")
	h.Set("x-twitter-client-language", "en")
	h.Set("x-client-transaction-id", hex.EncodeToString(raw))
	h.Set("Cookie", "auth_token="+authToken+"; ct0="+ct0)
	h.Set("User-Agent", UA)
	h.Set("Origin", Base)
	h.Set("Referer", Base+"/")
	h.Set("Accept", "*/*")
	return h
}

// Resolve — ссылка на твит → медиа (кук не требует — требует аккаунтов).
func (g *GQLClient) Resolve(ctx context.Context, u *url.URL) (*media.ResolveResult, error) {
	_, id, err := NormalizeMaybeShort(ctx, g.HC, u)
	if err != nil {
		return nil, err
	}
	return g.TweetDetail(ctx, id)
}

// TweetDetail — focalTweetId с ротацией до 3 аккаунтов (LRU из store).
func (g *GQLClient) TweetDetail(ctx context.Context, tweetID string) (*media.ResolveResult, error) {
	accounts := g.Store.PickN("twitter", 3)
	if len(accounts) == 0 {
		return nil, &media.Error{Kind: media.ErrCookiesDead,
			Detail: "нет активных аккаунтов twitter — пришли куки ботом /add"}
	}
	var lastErr error
	for i := range accounts {
		acc := accounts[i]
		g.Store.MarkUsed("twitter", acc.Label)
		res, err := g.tweetDetailWith(ctx, &acc, tweetID)
		if err == nil {
			g.Store.MarkOK("twitter", acc.Label)
			return res, nil
		}
		lastErr = err
		me := media.AsError(err)
		if me != nil && me.Kind == media.ErrRateLimited {
			return nil, err // ждать некогда — отдаём 503 сразу
		}
		if me != nil && me.Kind == media.ErrCookiesDead {
			if g.Store.MarkDead("twitter", acc.Label, me.Detail) && g.Notify != nil {
				g.Notify("🔴 twitter/" + acc.Label + ": куки протухли (" + me.Detail +
					").\nПришли новые: x.com → F12 → Cookies → auth_token и ct0 → сюда одной строкой.")
			}
			continue // следующий аккаунт
		}
		// прочее (queryId/upstream) — пробуем следующий аккаунт
	}
	return nil, lastErr
}

// tweetDetailWith — один аккаунт: перебор queryId, при исчерпании — discovery.
func (g *GQLClient) tweetDetailWith(ctx context.Context, acc *store.Account, tweetID string) (*media.ResolveResult, error) {
	variables := map[string]any{
		"focalTweetId": tweetID, "with_rux_injections": false,
		"rankingMode": "Relevance", "includePromotedContent": true,
		"withCommunity": true, "withQuickPromoteEligibilityTweetFields": true,
		"withBirdwatchNotes": true, "withVoice": true,
	}
	fieldToggles := map[string]any{
		"withArticleRichContentState": true, "withArticlePlainText": true,
		"withPayments": false, "withAuxiliaryUserLabels": false,
	}

	var lastErr error = &media.Error{Kind: media.ErrUpstream, Detail: "нет queryId-кандидатов"}
	for attempt := 1; attempt <= 2; attempt++ {
		for _, qid := range g.IDs.Candidates("TweetDetail") {
			body, status, err := g.call(ctx, acc, "TweetDetail", qid,
				variables, featuresTweet, fieldToggles)
			if err != nil {
				return nil, &media.Error{Kind: media.ErrUpstream, Detail: "gql: " + err.Error()}
			}
			switch {
			case status == 200:
				res, perr := parseTweetDetailBody(body, tweetID)
				if perr == nil {
					return res, nil
				}
				lastErr = perr
				if me := media.AsError(perr); me != nil && me.Kind == media.ErrNoMedia {
					return nil, perr // пост без видео — ротация не поможет
				}
			case status == 401:
				return nil, &media.Error{Kind: media.ErrCookiesDead, Account: acc.Label,
					Detail: "HTTP 401"}
			case status == 429:
				return nil, &media.Error{Kind: media.ErrRateLimited, Account: acc.Label,
					Detail: "HTTP 429"}
			default:
				lastErr = &media.Error{Kind: media.ErrUpstream,
					Detail: "gql: HTTP " + strconv.Itoa(status)}
				// 400/403/404 → ротация queryId (следующий кандидат)
			}
		}
		if attempt == 1 {
			g.IDs.Discover(ctx, g.HC, acc.AuthToken())
		}
	}
	return nil, lastErr
}

// Probe — дешёвая проверка кук для health-check: UserByScreenName "x".
// nil = живые; ErrCookiesDead = протухли; прочее — «неизвестно».
func (g *GQLClient) Probe(ctx context.Context, acc *store.Account) error {
	variables := map[string]any{"screen_name": "x", "withSafetyModeUserFields": true}
	for _, qid := range g.IDs.Candidates("UserByScreenName") {
		body, status, err := g.call(ctx, acc, "UserByScreenName", qid,
			variables, featuresUser, nil)
		if err != nil {
			return &media.Error{Kind: media.ErrUpstream, Detail: err.Error()}
		}
		switch status {
		case 200:
			_ = body
			return nil
		case 401, 403:
			return &media.Error{Kind: media.ErrCookiesDead, Account: acc.Label,
				Detail: "HTTP " + strconv.Itoa(status)}
		default:
			// 404 и пр. — возможно queryId устарел, следующий кандидат
		}
	}
	return &media.Error{Kind: media.ErrUpstream, Detail: "проба не удалась (queryId?)"}
}

// call — GET к GraphQL (TweetDetail/UserByScreenName — GET-операции).
func (g *GQLClient) call(ctx context.Context, acc *store.Account, op, qid string,
	variables map[string]any, featuresJSON string, toggles map[string]any) ([]byte, int, error) {
	params := url.Values{}
	vj, _ := json.Marshal(variables)
	params.Set("variables", string(vj))
	if featuresJSON != "" {
		params.Set("features", compactJSON(featuresJSON))
	}
	if toggles != nil {
		tj, _ := json.Marshal(toggles)
		params.Set("fieldToggles", string(tj))
	}
	endpoint := apiURL + "/" + qid + "/" + op + "?" + params.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, 0, err
	}
	req.Header = g.apiHeaders(acc.AuthToken(), acc.CT0())
	resp, err := g.HC.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, resp.StatusCode, err
	}
	return body, resp.StatusCode, nil
}

// compactJSON — фичи лежат строками (байт-паритет с python), в запрос
// уходят компактно.
func compactJSON(s string) string {
	var buf bytes.Buffer
	if err := json.Compact(&buf, []byte(s)); err != nil {
		return s
	}
	return buf.String()
}

// NewGQL — клиент с таймаутами (без общего Timeout: резолв живёт в ctx;
// TLS-хендшейк и idle — короткие).
func NewGQL(hc *http.Client, st *store.Store, ids *QueryIDCache, notify func(string)) *GQLClient {
	if hc == nil {
		hc = &http.Client{Transport: &http.Transport{
			TLSHandshakeTimeout: 10 * time.Second,
			IdleConnTimeout:     60 * time.Second,
		}}
	}
	return &GQLClient{HC: hc, Store: st, IDs: ids, Notify: notify}
}
