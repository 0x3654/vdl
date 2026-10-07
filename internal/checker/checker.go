// Package checker — периодический health-check кук: стаггер между
// аккаунтами 90 с, смерть → алерт (ровно один раз на переход).
package checker

import (
	"context"
	"log"
	"time"

	"vdl/internal/media"
	"vdl/internal/store"
)

type Checker struct {
	Store  *store.Store
	Probe  func(ctx context.Context, a *store.Account) error
	Notify func(text string)
	Every  time.Duration
}

// Run — блокирует до отмены ctx. Первый прогон через 2 минуты после старта
// (не мешать деплой-хелсчеку), дальше по тикеру.
func (c *Checker) Run(ctx context.Context) {
	select {
	case <-ctx.Done():
		return
	case <-time.After(2 * time.Minute):
	}
	for {
		c.pass(ctx)
		select {
		case <-ctx.Done():
			return
		case <-time.After(c.Every):
		}
	}
}

func (c *Checker) pass(ctx context.Context) {
	for _, a := range c.Store.List() {
		if ctx.Err() != nil {
			return
		}
		if a.Site != "twitter" || a.Status != "active" {
			continue // мёртвые не щупаем, нативные пробы только у twitter
		}
		acc := a
		cctx, cancel := context.WithTimeout(ctx, 20*time.Second)
		err := c.Probe(cctx, &acc)
		cancel()
		switch {
		case err == nil:
			c.Store.MarkOK(acc.Site, acc.Label)
		case isDead(err):
			if c.Store.MarkDead(acc.Site, acc.Label, err.Error()) && c.Notify != nil {
				c.Notify("🔴 <b>twitter/" + acc.Label + "</b> — куки протухли (" +
					err.Error() + ").\nПришли новые: x.com → F12 → Cookies → " +
					"auth_token и ct0 → сюда одной строкой (или /add twitter " + acc.Label + ").")
			}
		default:
			log.Printf("checker: twitter/%s: %v", acc.Label, err) // не убиваем за разовую ошибку
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(90 * time.Second): // стаггер: не дёргать X пачкой
		}
	}
}

func isDead(err error) bool {
	e := media.AsError(err)
	return e != nil && e.Kind == media.ErrCookiesDead
}
