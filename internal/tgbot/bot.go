// bot.go — цикл long-poll и команды управления куками.
// Обработка апдейтов последовательная (админ-бот, гонки не нужны),
// NotifyAdmins — неблокирующая, через буферизованный канал.
package tgbot

import (
	"context"
	"fmt"
	"html"
	"log"
	"strconv"
	"strings"
	"sync"
	"time"

	"vdl/internal/media"
	"vdl/internal/store"
)

// Bot — бот управления аккаунтами.
type Bot struct {
	API       *Client
	Admins    map[int64]bool
	Store     *store.Store
	Probe     func(ctx context.Context, a *store.Account) error // twitter-проба кук
	OnSave    func()                                            // материализовать cookies.json для cobalt
	OnCookies func(ctx context.Context) error                   // рестарт контейнера cobalt (docker-сокет)

	pendingMu sync.Mutex
	pending   map[int64]pendingAdd // chat → ждём куки
	notifyCh  chan string
}

type pendingAdd struct {
	site, label string
	deadline    time.Time
}

// knownSites — сервисы, которым cobalt (и наш стор) понимают куки;
// ключ должен совпадать с cookies.json cobalt, иначе молча игнорируется.
var knownSites = map[string]bool{
	"twitter": true, "instagram": true, "youtube": true,
	"reddit": true, "vimeo_bearer": true,
}

var siteList = []string{"twitter", "instagram", "youtube", "reddit", "vimeo_bearer"}

const pendingTTL = 10 * time.Minute

func New(api *Client, admins []int64, st *store.Store,
	probe func(context.Context, *store.Account) error, onSave func(),
	onCookies func(ctx context.Context) error) *Bot {
	b := &Bot{
		API: api, Admins: map[int64]bool{}, Store: st, Probe: probe,
		OnSave: onSave, OnCookies: onCookies,
		pending: map[int64]pendingAdd{},
		notifyCh: make(chan string, 32),
	}
	for _, id := range admins {
		b.Admins[id] = true
	}
	return b
}

// Run — главный цикл; блокирует до отмены ctx.
func (b *Bot) Run(ctx context.Context) {
	go b.sender(ctx)
	offset := 0
	for {
		updates, err := b.API.GetUpdates(ctx, offset)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			log.Printf("tg: getUpdates: %v", err)
			select {
			case <-ctx.Done():
				return
			case <-time.After(3 * time.Second):
			}
			continue
		}
		for _, u := range updates {
			if u.UpdateID >= offset {
				offset = u.UpdateID + 1
			}
			b.handle(ctx, u)
		}
	}
}

// NotifyAdmins — алерт во все админ-чаты; неблокирует вызывающего.
func (b *Bot) NotifyAdmins(text string) {
	select {
	case b.notifyCh <- text:
	default: // переполнен — капнем, персональный бот
	}
}

func (b *Bot) sender(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case text := <-b.notifyCh:
			for chat := range b.Admins {
				cctx, cancel := context.WithTimeout(ctx, 15*time.Second)
				err := b.API.SendMessage(cctx, chat, text)
				cancel()
				if err != nil {
					log.Printf("tg: notify: %v", err)
				}
			}
		}
	}
}

func (b *Bot) handle(ctx context.Context, u Update) {
	msg := u.Message
	if msg == nil || msg.Text == "" {
		return
	}
	if !b.Admins[msg.Chat.ID] {
		return // чужой чат: молча (назначение бота не светим)
	}
	text := strings.TrimSpace(msg.Text)
	chat := msg.Chat.ID

	switch {
	case strings.HasPrefix(text, "/start"), strings.HasPrefix(text, "/help"):
		b.reply(ctx, chat, helpText())
	case strings.HasPrefix(text, "/accounts"), strings.HasPrefix(text, "/list"):
		b.reply(ctx, chat, b.accountsText())
	case strings.HasPrefix(text, "/add"):
		b.cmdAdd(ctx, chat, text)
	case strings.HasPrefix(text, "/del"), strings.HasPrefix(text, "/rm"):
		b.cmdDel(ctx, chat, text)
	case strings.HasPrefix(text, "/check"):
		b.cmdCheck(ctx, chat, text)
	case strings.HasPrefix(text, "/"):
		b.reply(ctx, chat, "Не знаю такую команду. /help")
	default:
		b.handlePaste(ctx, chat, text)
	}
}

func helpText() string {
	return "🎛 <b>vdl — куки-менеджер</b>\n" +
		"\n<b>Добавить:</b> <code>/add [сайт] [метка]</code> и следующим сообщением куки " +
		"(или просто вставь куки — сайт twitter, метка автоматом).\n" +
		"Форматы: хедером <code>auth_token=…; ct0=…</code>, JSON, построчно k=v.\n" +
		"\n<b>Прочее:</b>\n" +
		"<code>/accounts</code> — список с состоянием\n" +
		"<code>/check [сайт] [метка]</code> — проверить прямо сейчас\n" +
		"<code>/del [сайт] [метка]</code> — удалить\n" +
		"\nПротухшие куки отмечу 🔴 и попрошу новые."
}

func (b *Bot) accountsText() string {
	accs := b.Store.List()
	if len(accs) == 0 {
		return "Аккаунтов нет. Добавь: <code>/add twitter main</code> → куки."
	}
	var sb strings.Builder
	sb.WriteString("<b>Аккаунты</b>\n")
	for _, a := range accs {
		emoji := "🟢"
		switch a.Status {
		case "dead":
			emoji = "🔴"
		}
		last := "не проверялся"
		if !a.LastOK.IsZero() {
			last = humanSince(a.LastOK)
		}
		fmt.Fprintf(&sb, "%s <b>%s/%s</b> — ok: %s", emoji, html.EscapeString(a.Site),
			html.EscapeString(a.Label), last)
		if a.FailMsg != "" {
			fmt.Fprintf(&sb, " · %s", html.EscapeString(a.FailMsg))
		}
		sb.WriteString("\n")
	}
	return sb.String()
}

func humanSince(t time.Time) string {
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return "только что"
	case d < time.Hour:
		return strconv.Itoa(int(d.Minutes())) + " мин назад"
	case d < 24*time.Hour:
		return strconv.Itoa(int(d.Hours())) + " ч назад"
	default:
		return strconv.Itoa(int(d.Hours()/24)) + " дн назад"
	}
}

// cmdAdd — "/add [site] [label]"; куки можно прислать в этом же сообщении
// следующей строкой.
func (b *Bot) cmdAdd(ctx context.Context, chat int64, text string) {
	rest := ""
	body := strings.TrimPrefix(text, "/add")
	if i := strings.Index(body, "\n"); i >= 0 {
		rest = strings.TrimSpace(body[i+1:])
		body = body[:i]
	}
	args := strings.Fields(body)
	var site, label string
	switch len(args) {
	case 0:
		site = "twitter"
	case 1:
		site = strings.ToLower(args[0])
	case 2:
		site, label = strings.ToLower(args[0]), args[1]
	default:
		b.reply(ctx, chat, "Формат: <code>/add [сайт] [метка]</code> — например /add twitter main")
		return
	}
	if !knownSites[site] {
		b.reply(ctx, chat, "🚫 сайт <code>"+html.EscapeString(site)+"</code> не поддерживается. "+
			"Доступно: "+strings.Join(siteList, ", "))
		return
	}
	if label == "" {
		label = b.autoLabel(site)
	}
	if rest != "" { // куки пришли сразу — сохраняем без ожидания
		b.savePaste(ctx, chat, site, label, rest)
		return
	}
	b.pendingMu.Lock()
	b.pending[chat] = pendingAdd{site: site, label: label, deadline: time.Now().Add(pendingTTL)}
	b.pendingMu.Unlock()
	b.reply(ctx, chat, "Вставь куки для <b>"+html.EscapeString(site+"/"+label)+
		"</b> — например строкой из F12: <code>auth_token=…; ct0=…</code>")
}

// handlePaste — сообщение с куками (или мусор → подсказка).
func (b *Bot) handlePaste(ctx context.Context, chat int64, text string) {
	site, label := "twitter", b.autoLabel("twitter")
	b.pendingMu.Lock()
	if p, ok := b.pending[chat]; ok && time.Now().Before(p.deadline) {
		site, label = p.site, p.label
	}
	delete(b.pending, chat) // попытка одна, дальше снова явный /add
	b.pendingMu.Unlock()
	b.savePaste(ctx, chat, site, label, text)
}

// savePaste — распарсить и сохранить куки, ответить, проверить.
func (b *Bot) savePaste(ctx context.Context, chat int64, site, label, paste string) {
	m := ParseCookiePaste(paste)
	if len(m) == 0 {
		b.reply(ctx, chat, "Не похоже на куки. /help — форматы.")
		return
	}
	acc := store.Account{Site: site, Label: label, Cookies: m}
	if err := b.Store.Upsert(acc); err != nil {
		b.reply(ctx, chat, "⚠️ не сохранил: "+html.EscapeString(err.Error()))
		return
	}
	if b.OnSave != nil {
		b.OnSave()
	}
	// cobalt читает cookies.json только при старте — рестартим сразу,
	// чтобы новые куки подхватились без ожидания/ручных действий
	restartMsg := ""
	if b.OnCookies != nil {
		rctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
		if err := b.OnCookies(rctx); err != nil {
			cancel()
			restartMsg = "\n⚠️ cobalt не перезапущен (" + html.EscapeString(err.Error()) +
				") — вручную: docker restart vdl-cobalt"
		} else {
			cancel()
			restartMsg = "\n🔄 cobalt перезапущен — куки в деле"
		}
	}
	b.reply(ctx, chat, "💾 <b>"+html.EscapeString(site+"/"+label)+"</b> — сохранено"+restartMsg)
	b.probeAndReply(ctx, chat, site, label)
}

func (b *Bot) cmdDel(ctx context.Context, chat int64, text string) {
	args := strings.Fields(strings.TrimPrefix(text, "/del"))
	if len(args) == 1 {
		if b.Store.Delete("twitter", args[0]) {
			if b.OnSave != nil {
				b.OnSave()
			}
			b.reply(ctx, chat, "🗑 twitter/"+html.EscapeString(args[0])+" удалён")
			return
		}
		b.reply(ctx, chat, "Не нашёл. /accounts — что есть.")
		return
	}
	if len(args) == 2 && b.Store.Delete(args[0], args[1]) {
		if b.OnSave != nil {
			b.OnSave()
		}
		b.reply(ctx, chat, "🗑 "+html.EscapeString(args[0]+"/"+args[1])+" удалён")
		return
	}
	b.reply(ctx, chat, "Формат: <code>/del [сайт] [метка]</code>")
}

func (b *Bot) cmdCheck(ctx context.Context, chat int64, text string) {
	args := strings.Fields(strings.TrimPrefix(text, "/check"))
	site, label := "twitter", ""
	switch len(args) {
	case 0:
	case 1:
		label = args[0]
	case 2:
		site, label = args[0], args[1]
	}
	if label == "" { // все аккаунты сайта
		any := false
		for _, a := range b.Store.List() {
			if a.Site == site {
				any = true
				b.probeAndReply(ctx, chat, site, a.Label)
			}
		}
		if !any {
			b.reply(ctx, chat, "Аккаунтов сайта нет.")
		}
		return
	}
	b.probeAndReply(ctx, chat, site, label)
}

// probeAndReply — проба кук с ответом и реакцией store.
func (b *Bot) probeAndReply(ctx context.Context, chat int64, site, label string) {
	var acc *store.Account
	for _, a := range b.Store.List() {
		if a.Site == site && a.Label == label {
			cp := a
			acc = &cp
			break
		}
	}
	if acc == nil {
		b.reply(ctx, chat, "Нет такого аккаунта. /accounts")
		return
	}
	if site != "twitter" || b.Probe == nil {
		b.reply(ctx, chat, "🤷 для "+html.EscapeString(site)+" живость не проверяю, но сохранил.")
		return
	}
	cctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	err := b.Probe(cctx, acc)
	switch {
	case err == nil:
		b.Store.MarkOK(site, label)
		b.reply(ctx, chat, "✅ <b>"+html.EscapeString(site+"/"+label)+"</b> — куки живые")
	case isDead(err):
		if b.Store.MarkDead(site, label, err.Error()) {
			b.NotifyAdmins("🔴 <b>"+html.EscapeString(site+"/"+label)+"</b> — куки протухли: "+
				html.EscapeString(err.Error())+"\nПришли новые (x.com → F12 → Cookies → "+
				"auth_token и ct0 → сюда одной строкой).")
		}
		b.reply(ctx, chat, "🔴 <b>"+html.EscapeString(site+"/"+label)+"</b> — протухли, жду новые")
	default:
		b.reply(ctx, chat, "⚠️ "+html.EscapeString(site+"/"+label)+": проверить не вышло — "+
			html.EscapeString(err.Error()))
	}
}

func isDead(err error) bool {
	e := media.AsError(err)
	return e != nil && e.Kind == media.ErrCookiesDead
}

func (b *Bot) reply(ctx context.Context, chat int64, text string) {
	cctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	if err := b.API.SendMessage(cctx, chat, text); err != nil {
		log.Printf("tg: reply: %v", err)
	}
}

// autoLabel — main для первого аккаунта сайта, дальше site2, site3…
func (b *Bot) autoLabel(site string) string {
	n := 0
	for _, a := range b.Store.List() {
		if a.Site == site {
			n++
		}
	}
	if n == 0 {
		return "main"
	}
	return site + strconv.Itoa(n+1)
}
