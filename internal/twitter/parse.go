// parse.go — разбор ответа TweetDetail: модель данных X мигрирует
// (legacy↔core, entryType↔itemType, модули ответов) — поддерживаем оба
// варианта, ходим по map[string]any как python-оригинал.
package twitter

import (
	"encoding/json"
	"time"

	"vdl/internal/media"
)

func dict(v any) map[string]any {
	m, _ := v.(map[string]any)
	return m
}

func sl(v any) []any {
	s, _ := v.([]any)
	return s
}

func str(m map[string]any, keys ...string) string {
	for _, k := range keys {
		if v, ok := m[k].(string); ok {
			return v
		}
	}
	return ""
}

// userFields — имя/ник из user_results.result: core (новое) или legacy.
func userFields(u map[string]any) (name, screen string) {
	if u == nil {
		return "", ""
	}
	core := dict(u["core"])
	leg := dict(u["legacy"])
	name = str(core, "name")
	if name == "" {
		name = str(leg, "name")
	}
	screen = str(core, "screen_name")
	if screen == "" {
		screen = str(leg, "screen_name")
	}
	return name, screen
}

// extractMedia — legacy.extended_entities.media → список медиа.
// Видео: mp4-варианты, лучший по bitrate — как Best, остальные в Variants.
func extractMedia(legacy map[string]any) []media.Media {
	var out []media.Media
	for _, mv := range sl(dict(legacy["extended_entities"])["media"]) {
		m := dict(mv)
		if m == nil {
			continue
		}
		typ := str(m, "type")
		switch typ {
		case "photo":
			if u := str(m, "media_url_https"); u != "" {
				out = append(out, media.Media{Type: "photo", Best: u})
			}
		case "video", "animated_gif":
			vi := dict(m["video_info"])
			var variants []media.Variant
			for _, vv := range sl(vi["variants"]) {
				v := dict(vv)
				if v == nil || str(v, "content_type") != "video/mp4" {
					continue
				}
				url := str(v, "url")
				if url == "" {
					continue
				}
				br, _ := v["bitrate"].(float64)
				variants = append(variants, media.Variant{
					Bitrate: int(br), URL: url,
				})
			}
			if len(variants) == 0 {
				continue
			}
			best := variants[0]
			for _, v := range variants[1:] {
				if v.Bitrate > best.Bitrate {
					best = v
				}
			}
			out = append(out, media.Media{
				Type: typ, Best: best.URL, Poster: str(m, "media_url_https"),
				Variants: variants,
			})
		}
	}
	return out
}

// parseTweet — result твита → компактное представление; nil для мусора.
type parsedTweet struct {
	ID     string
	Screen string
	Name   string
	Text   string
	Medias []media.Media
}

func parseTweet(t map[string]any) *parsedTweet {
	if t == nil {
		return nil
	}
	legacy := dict(t["legacy"])
	if legacy == nil {
		legacy = dict(dict(t["tweet"])["legacy"])
	}
	if legacy == nil {
		return nil
	}
	_, screen := userFields(dict(dict(dict(t["core"])["user_results"])["result"]))
	text := str(legacy, "full_text")
	// длинные посты обрезаны в full_text — полный текст в note_tweet
	if note := str(dict(dict(dict(t["note_tweet"])["note_tweet_results"])["result"]), "text"); note != "" {
		text = note
	}
	pt := &parsedTweet{
		ID:     str(t, "rest_id", "id_str"),
		Screen: screen,
		Text:   text,
		Medias: extractMedia(legacy),
	}
	if pt.ID == "" {
		pt.ID = str(legacy, "id_str")
	}
	// RT: текст и медиа — оригинала
	if rt := dict(dict(legacy["retweeted_status_result"])["result"]); rt != nil {
		rtl := dict(rt["legacy"])
		_, rtScreen := userFields(dict(dict(dict(rt["core"])["user_results"])["result"]))
		if rtScreen != "" {
			pt.Screen = rtScreen
		}
		if t := str(rtl, "full_text"); t != "" {
			pt.Text = t
		}
		if len(pt.Medias) == 0 {
			pt.Medias = extractMedia(rtl)
		}
		// RT квот-поста: у квота бывают свои медиа
		if q := dict(dict(rtl["quoted_status_result"])["result"]); q != nil {
			pt.Medias = append(pt.Medias, quoteMedia(q, nil)...)
		}
	}
	// квот-цепочка: сам твит без медиа (или с ним — качаем всё) → медиа квотов
	// всех уровней; глубина ограничена на всякий случай
	if q := dict(dict(t["quoted_status_result"])["result"]); q != nil {
		pt.Medias = append(pt.Medias, quoteMedia(q, nil)...)
	}
	pt.Medias = dedupMedia(pt.Medias)
	return pt
}

// quoteMedia — медиа квот-поста и его собственного RT/квота (рекурсивно,
// максимум 4 уровня — защита от циклов в кривых ответах).
func quoteMedia(qr map[string]any, seen map[string]bool) []media.Media {
	if qr == nil {
		return nil
	}
	if seen == nil {
		seen = map[string]bool{}
	}
	if id := str(qr, "rest_id"); id != "" {
		if seen[id] {
			return nil
		}
		seen[id] = true
	}
	var out []media.Media
	leg := dict(qr["legacy"])
	out = append(out, extractMedia(leg)...)
	rt := dict(dict(leg["retweeted_status_result"])["result"])
	if rt != nil {
		out = append(out, extractMedia(dict(rt["legacy"]))...)
	}
	// квот следующего уровня: у самого квота или внутри его RT
	next := dict(dict(qr["quoted_status_result"])["result"])
	if next == nil && rt != nil {
		next = dict(dict(dict(rt["legacy"])["quoted_status_result"])["result"])
	}
	if next != nil && len(seen) < 4 {
		out = append(out, quoteMedia(next, seen)...)
	}
	return out
}

// dedupMedia — убрать дубли по Best (RT/квоты могут повторяться).
func dedupMedia(in []media.Media) []media.Media {
	seen := map[string]bool{}
	out := in[:0]
	for _, m := range in {
		if m.Best == "" || seen[m.Best] {
			continue
		}
		seen[m.Best] = true
		out = append(out, m)
	}
	return out
}

// parseISO — "Wed Oct 01 12:00:00 +0000 2026" → RFC3339.
func parseISO(s string) string {
	if ts, err := time.Parse(time.RFC1123Z, s); err == nil {
		return ts.UTC().Format(time.RFC3339)
	}
	return s
}

// tweetFromItem — itemContent: свежие ревизии itemType, старые entryType.
func tweetFromItem(item map[string]any) *parsedTweet {
	if item == nil || item["tweet_results"] == nil {
		return nil
	}
	typ := str(item, "itemType")
	if typ == "" {
		typ = str(item, "entryType")
	}
	if typ != "" && typ != "TimelineTweet" {
		return nil
	}
	return parseTweet(dict(dict(item["tweet_results"])["result"]))
}

// tweetsFromInstructions — обход instructions → все твиты (без курсоров:
// для TweetDetail пагинация не нужна).
func tweetsFromInstructions(instructions []any) []*parsedTweet {
	var tweets []*parsedTweet
	seen := map[string]bool{}
	add := func(t *parsedTweet) {
		if t != nil && t.ID != "" && !seen[t.ID] {
			seen[t.ID] = true
			tweets = append(tweets, t)
		}
	}
	for _, iv := range instructions {
		inst := dict(iv)
		if inst == nil {
			continue
		}
		switch str(inst, "type") {
		case "TimelineAddEntries", "TimelineAddToModule":
		default:
			continue
		}
		for _, ev := range sl(inst["entries"]) {
			content := dict(dict(ev)["content"])
			if content == nil {
				continue
			}
			switch str(content, "entryType") {
			case "TimelineTimelineCursor":
				continue
			case "TimelineTimelineModule":
				// ветки ответов свёрнуты в модули: content.items[].item.itemContent
				for _, itv := range sl(content["items"]) {
					add(tweetFromItem(dict(dict(dict(itv)["item"])["itemContent"])))
				}
			default:
				add(tweetFromItem(dict(content["itemContent"])))
			}
		}
	}
	return tweets
}

// parseTweetDetailBody — тело GraphQL-ответа → результат для focal-твита.
func parseTweetDetailBody(body []byte, focalID string) (*media.ResolveResult, error) {
	var root map[string]any
	if err := json.Unmarshal(body, &root); err != nil {
		return nil, &media.Error{Kind: media.ErrUpstream, Detail: "gql: битый JSON"}
	}
	data := dict(root["data"])
	if data == nil {
		if errs := sl(root["errors"]); len(errs) > 0 {
			msg := str(dict(errs[0]), "message")
			return nil, &media.Error{Kind: media.ErrUpstream, Detail: "gql: " + msg}
		}
		return nil, &media.Error{Kind: media.ErrUpstream, Detail: "gql: нет data"}
	}
	instructions := sl(dict(data["threaded_conversation_with_injections_v2"])["instructions"])
	tweets := tweetsFromInstructions(instructions)
	main := (*parsedTweet)(nil)
	for _, t := range tweets {
		if t.ID == focalID {
			main = t
			break
		}
	}
	if main == nil && len(tweets) > 0 {
		main = tweets[0] // фолбэк: первый entry
	}
	if main == nil {
		return nil, &media.Error{Kind: media.ErrUpstream, Detail: "gql: твит не найден в ответе"}
	}
	res := &media.ResolveResult{
		Source: "graphql", Site: "twitter",
		Author: main.Screen, Text: main.Text,
		Medias: main.Medias,
	}
	if len(res.Medias) == 0 {
		return nil, &media.Error{Kind: media.ErrNoMedia, Detail: "gql: твит без медиа"}
	}
	return res, nil
}
