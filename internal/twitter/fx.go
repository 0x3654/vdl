// fx.go — fxtwitter: публичное зеркало API, отдаёт твит (и NSFW) без кук,
// датацентровому IP не доверяет меньше, чем X собственному API.
package twitter

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"vdl/internal/media"
)

var fxBase = "https://api.fxtwitter.com"

// FXClient — резолвер через fxtwitter.
type FXClient struct {
	HC *http.Client
}

func NewFX(hc *http.Client) *FXClient { return &FXClient{HC: hc} }

func (f *FXClient) Name() string            { return "fxtwitter" }
func (f *FXClient) MatchHost(h string) bool { return IsTwitterHost(h) }

type fxFormat struct {
	URL         string  `json:"url"`
	Bitrate     float64 `json:"bitrate"`
	ContentType string  `json:"content_type"`
	Type        string  `json:"type"`
	Width       int     `json:"width"`
	Height      int     `json:"height"`
}

type fxMedia struct {
	Type         string     `json:"type"` // video | photo | gif
	URL          string     `json:"url"`
	Width        int        `json:"width"`
	Height       int        `json:"height"`
	Duration     float64    `json:"duration"`
	ThumbnailURL string     `json:"thumbnail_url"`
	Formats      []fxFormat `json:"formats"`
	Variants     []fxFormat `json:"variants"`
}

// fxTweet — рекурсивный: квот-пост вложен в quote (уровни — сколько X даст).
type fxTweet struct {
	Author *struct {
		Name       string `json:"name"`
		ScreenName string `json:"screen_name"`
	} `json:"author"`
	Text  string `json:"text"`
	Media *struct {
		All []fxMedia `json:"all"`
	} `json:"media"`
	Quote *fxTweet `json:"quote"`
}

type fxResp struct {
	Code  int      `json:"code"`
	Tweet *fxTweet `json:"tweet"`
}

// Resolve — ссылка на твит → медиа. t.co не разворачиваем: fx принимает
// только полный вид, коротыш пусть обрабатывает cobalt в цепочке.
func (f *FXClient) Resolve(ctx context.Context, u *url.URL) (*media.ResolveResult, error) {
	if CleanHost(u.Host) == "t.co" || CleanHost(u.Host) == "v.t.co" {
		return nil, &media.Error{Kind: media.ErrBadURL, Detail: "fxtwitter: t.co не поддерживает"}
	}
	screen, id, err := Normalize(u)
	if err != nil {
		return nil, err
	}
	return f.Tweet(ctx, screen, id)
}

// Tweet — GET /<screen>/status/<id>.
func (f *FXClient) Tweet(ctx context.Context, screen, id string) (*media.ResolveResult, error) {
	if f.HC == nil {
		f.HC = &http.Client{Timeout: 15 * time.Second}
	}
	u := fxBase + "/" + url.PathEscape(screen) + "/status/" + url.PathEscape(id)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, &media.Error{Kind: media.ErrUpstream, Detail: "fx: " + err.Error()}
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", UA)
	resp, err := f.HC.Do(req)
	if err != nil {
		return nil, &media.Error{Kind: media.ErrUpstream, Detail: "fx: " + err.Error()}
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, &media.Error{Kind: media.ErrUpstream, Detail: "fx: чтение: " + err.Error()}
	}
	var fr fxResp
	if err := json.Unmarshal(body, &fr); err != nil {
		return nil, &media.Error{Kind: media.ErrUpstream, Detail: "fx: битый JSON"}
	}
	if fr.Tweet == nil {
		kind := media.ErrUpstream
		if fr.Code == 404 {
			kind = media.ErrBadURL
		}
		return nil, &media.Error{Kind: kind,
			Detail: "fx: твит не получен (code " + strconv.Itoa(fr.Code) + ")"}
	}

	res := &media.ResolveResult{Source: "fxtwitter", Site: "twitter"}
	if fr.Tweet.Author != nil {
		res.Author = fr.Tweet.Author.ScreenName
	}
	res.Text = fr.Tweet.Text
	// цепочка: сам твит → его квот → квот квота… — собираем медиа всех уровней
	seenURL := map[string]bool{}
	for tw := fr.Tweet; tw != nil; tw = tw.Quote {
		if tw.Media == nil {
			continue
		}
		for _, m := range tw.Media.All {
			if m.URL == "" || seenURL[m.URL] {
				continue
			}
			seenURL[m.URL] = true
			mm := media.Media{
				Type:     m.Type,
				Best:     m.URL,
				Poster:   m.ThumbnailURL,
				Width:    m.Width,
				Height:   m.Height,
				Duration: m.Duration,
			}
			for _, f := range append(m.Variants, m.Formats...) {
				if f.URL == "" || (f.ContentType != "" && f.ContentType != "video/mp4") {
					continue
				}
				mm.Variants = append(mm.Variants, media.Variant{
					Bitrate: int(f.Bitrate), Width: f.Width, URL: f.URL,
				})
			}
			if mm.Type == "gif" {
				mm.Type = "animated_gif"
			}
			res.Medias = append(res.Medias, mm)
		}
	}
	if len(res.Medias) == 0 {
		// без медиа — но текст может содержать ссылки (fx разворачивает t.co):
		// это не пустой пост, а кандидат на рекурсию
		res.Links = linksFromText(res.Text)
		if len(res.Links) == 0 {
			return nil, &media.Error{Kind: media.ErrNoMedia, Detail: "fx: твит без медиа"}
		}
	} else if res.Text != "" {
		res.Links = linksFromText(res.Text)
	}
	return res, nil
}

var urlInText = regexp.MustCompile(`https?://[^\s]+`)
var skipLinkPrefix = []string{
	"https://pic.twitter.com", "https://t.co", "https://x.com/i/",
}

// linksFromText — внешние ссылки из текста твита (медиа-служебные вон).
func linksFromText(text string) []string {
	var out []string
	for _, u := range urlInText.FindAllString(text, 10) {
		u = strings.TrimRight(u, ".,);…»")
		if u == "" {
			continue
		}
		skip := false
		for _, p := range skipLinkPrefix {
			if strings.HasPrefix(u, p) {
				skip = true
				break
			}
		}
		if !skip {
			out = append(out, u)
		}
	}
	return out
}
