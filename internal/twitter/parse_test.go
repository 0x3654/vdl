package twitter

import (
	"os"
	"strings"
	"testing"

	"vdl/internal/media"
)

// fixture — живой снимок TweetDetail (видео-твит), снят 2026-10-05.
const fixturePath = "../../testdata/tweetdetail_video.json"

// fixture — квот-пост без своих медиа, видео внутри квота.
const quoteFixturePath = "../../testdata/tweetdetail_quote.json"

func TestParseQuoteChain(t *testing.T) {
	raw, err := os.ReadFile(quoteFixturePath)
	if err != nil {
		t.Skipf("фикстура недоступна: %v", err)
	}
	// focal — сам квот-пост (своих медиа нет), видео должно прийти из квота
	res, err := parseTweetDetailBody(raw, "2107124790339477720")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(res.Medias) == 0 {
		t.Fatal("медиа из квота не извлечены")
	}
	if !strings.Contains(res.Medias[0].Best, "video.twimg.com") ||
		!strings.Contains(res.Medias[0].Best, ".mp4") {
		t.Errorf("best не mp4: %s", res.Medias[0].Best)
	}
	t.Logf("медиа: %d, первое: %s", len(res.Medias), res.Medias[0].Best)
}

func TestParseTweetDetailFixture(t *testing.T) {
	raw, err := os.ReadFile(fixturePath)
	if err != nil {
		t.Skipf("фикстура недоступна: %v", err)
	}
	res, err := parseTweetDetailBody(raw, "2106743734079545531")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if res.Source != "graphql" || res.Site != "twitter" {
		t.Errorf("источник: %s/%s", res.Source, res.Site)
	}
	if len(res.Medias) == 0 {
		t.Fatal("медиа не найдены")
	}
	m := res.Medias[0]
	if m.Type != "video" {
		t.Errorf("тип: %s", m.Type)
	}
	if m.Best == "" || len(m.Variants) == 0 {
		t.Errorf("best=%q variants=%d", m.Best, len(m.Variants))
	}
	// лучший вариант = максимальный bitrate
	max := 0
	for _, v := range m.Variants {
		if v.Bitrate > max {
			max = v.Bitrate
		}
	}
	bestBr := 0
	for _, v := range m.Variants {
		if v.URL == m.Best {
			bestBr = v.Bitrate
		}
	}
	if bestBr != max {
		t.Errorf("best bitrate %d != max %d", bestBr, max)
	}
	t.Logf("author=%s medias=%d best=%s variants=%d", res.Author, len(res.Medias), m.Best, len(m.Variants))
}

func TestParseTweetDetailNoMedia(t *testing.T) {
	// минимальный корректный ответ без видео
	body := []byte(`{"data":{"threaded_conversation_with_injections_v2":{"instructions":[{"type":"TimelineAddEntries","entries":[{"content":{"entryType":"TimelineTimelineItem","itemContent":{"itemType":"TimelineTweet","tweet_results":{"result":{"rest_id":"111","legacy":{"full_text":"just text"}}}}}}]}]}}}`)
	_, err := parseTweetDetailBody(body, "111")
	if err == nil {
		t.Fatal("ждали ErrNoMedia")
	}
	if e := media.AsError(err); e == nil || e.Kind != media.ErrNoMedia {
		t.Fatalf("ожидали ErrNoMedia, получили %v", err)
	}
}

func TestParseCoreModel(t *testing.T) {
	// новая модель: core вместо legacy у пользователя
	body := []byte(`{"data":{"threaded_conversation_with_injections_v2":{"instructions":[{"type":"TimelineAddEntries","entries":[{"content":{"entryType":"TimelineTimelineItem","itemContent":{"entryType":"TimelineTweet","tweet_results":{"result":{"rest_id":"222","core":{"user_results":{"result":{"core":{"name":"N","screen_name":"scr"}}}},"legacy":{"full_text":"text","extended_entities":{"media":[{"type":"animated_gif","media_url_https":"https://pbs.twimg.com/t.jpg","video_info":{"variants":[{"content_type":"video/mp4","bitrate":100,"url":"https://video.twimg.com/a.mp4"},{"content_type":"video/mp4","bitrate":320,"url":"https://video.twimg.com/b.mp4"},{"content_type":"application/x-mpegURL","url":"https://video.twimg.com/x.m3u8"}]}}]}}}}}}}]}]}}}`)
	res, err := parseTweetDetailBody(body, "222")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if res.Author != "scr" {
		t.Errorf("author: %s", res.Author)
	}
	m := res.Medias[0]
	if m.Type != "animated_gif" {
		t.Errorf("тип: %s", m.Type)
	}
	if m.Best != "https://video.twimg.com/b.mp4" {
		t.Errorf("best: %s (m3u8 не должен участвовать)", m.Best)
	}
	if len(m.Variants) != 2 {
		t.Errorf("варианты: %d (m3u8 отфильтровать)", len(m.Variants))
	}
}
