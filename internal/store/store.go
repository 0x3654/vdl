// Package store — аккаунты-куки (site,label) → произвольный набор кук.
// JSON в DATA_DIR, права 0600, атомарная запись tmp+rename (паттерн nnm-rss).
// Twitter хранит auth_token+ct0; будущие сайты — свои ключи в той же карте.
package store

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Account — один аккаунт одного сайта.
type Account struct {
	Site     string            `json:"site"`  // twitter | instagram | youtube | …
	Label    string            `json:"label"` // уникален внутри site
	Cookies  map[string]string `json:"cookies"`
	Status   string            `json:"status"` // active | dead
	LastOK   time.Time         `json:"last_ok,omitempty"`
	LastUsed time.Time         `json:"last_used,omitempty"`
	AddedAt  time.Time         `json:"added_at"`
	FailMsg  string            `json:"fail_msg,omitempty"` // HTTP 401 (…) — для /accounts
}

// AuthToken / CT0 — удобные геттеры для twitter-кук.
func (a *Account) AuthToken() string { return a.Cookies["auth_token"] }
func (a *Account) CT0() string       { return a.Cookies["ct0"] }

// CookieHeader — куки строкой «k=v; k=v» (формат cookies.json у cobalt).
func (a *Account) CookieHeader() string {
	s := ""
	for k, v := range a.Cookies {
		if s != "" {
			s += "; "
		}
		s += k + "=" + v
	}
	return s
}

type fileFormat struct {
	Accounts []Account `json:"accounts"`
}

// Store — потокобезопасное хранилище. Мутации сохраняются сразу же.
type Store struct {
	mu   sync.Mutex
	path string
	accs []*Account
}

func Open(path string) (*Store, error) {
	s := &Store{path: path}
	raw, err := os.ReadFile(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return s, nil // пустой стор — нормальное состояние (fx/cobalt без кук)
	case err != nil:
		return nil, err
	}
	var f fileFormat
	if err := json.Unmarshal(raw, &f); err != nil {
		// битый файл с куками молча терять нельзя — падаем громко
		return nil, fmt.Errorf("store: %s бит: %w", path, err)
	}
	for i := range f.Accounts {
		a := f.Accounts[i]
		if a.Status == "" {
			a.Status = "active"
		}
		s.accs = append(s.accs, &a)
	}
	return s, nil
}

func (s *Store) saveLocked() error {
	f := fileFormat{Accounts: make([]Account, len(s.accs))}
	for i, a := range s.accs {
		f.Accounts[i] = *a
	}
	raw, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}

// List — копии аккаунтов (по значению: секреты не раздаём по ссылке).
func (s *Store) List() []Account {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Account, len(s.accs))
	for i, a := range s.accs {
		out[i] = *a
	}
	return out
}

// PickN — до n активных аккаунтов сайта, дольше всех не использовавшиеся
// (LRU: размазываем rate-limit по аккаунтам). MarkUsed отмечает факт.
func (s *Store) PickN(site string, n int) []Account {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []*Account
	for _, a := range s.accs {
		if a.Site == site && a.Status == "active" {
			out = append(out, a)
		}
	}
	// сортировка вставкой по LastUsed (n маленькое, аккаунтов единицы)
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j].LastUsed.Before(out[j-1].LastUsed); j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	if len(out) > n {
		out = out[:n]
	}
	res := make([]Account, len(out))
	for i, a := range out {
		res[i] = *a
	}
	return res
}

// MarkUsed — фиксируем использование (внутри Pick-цикла резолва).
func (s *Store) MarkUsed(site, label string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if a := s.find(site, label); a != nil {
		a.LastUsed = time.Now().UTC()
		_ = s.saveLocked()
	}
}

// MarkOK — проба прошла.
func (s *Store) MarkOK(site, label string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if a := s.find(site, label); a != nil {
		a.Status = "active"
		a.LastOK = time.Now().UTC()
		a.FailMsg = ""
		_ = s.saveLocked()
	}
}

// MarkDead — куки умерли. Возвращает true, если статус сменился
// (для алерта «умер» ровно один раз).
func (s *Store) MarkDead(site, label, reason string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	a := s.find(site, label)
	if a == nil || a.Status == "dead" {
		return false
	}
	a.Status = "dead"
	a.FailMsg = reason
	_ = s.saveLocked()
	return true
}

// Upsert — вставка/обновление по (site,label). Новые куки на тот же label
// реактивируют аккаунт (Status=active, FailMsg="").
func (s *Store) Upsert(a Account) error {
	if a.Site == "" || a.Label == "" || len(a.Cookies) == 0 {
		return fmt.Errorf("store: Upsert требует site, label и куки")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if cur := s.find(a.Site, a.Label); cur != nil {
		cur.Cookies = a.Cookies
		cur.Status = "active"
		cur.FailMsg = ""
		cur.LastOK = time.Time{}
	} else {
		if a.AddedAt.IsZero() {
			a.AddedAt = time.Now().UTC()
		}
		if a.Status == "" {
			a.Status = "active"
		}
		cp := a
		s.accs = append(s.accs, &cp)
	}
	return s.saveLocked()
}

// Delete — удалить аккаунт; true если был.
func (s *Store) Delete(site, label string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, a := range s.accs {
		if a.Site == site && a.Label == label {
			s.accs = append(s.accs[:i], s.accs[i+1:]...)
			_ = s.saveLocked()
			return true
		}
	}
	return false
}

// Counts — сколько активных/мёртвых (для /healthz).
func (s *Store) Counts() (active, dead int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, a := range s.accs {
		if a.Status == "active" {
			active++
		} else {
			dead++
		}
	}
	return active, dead
}

func (s *Store) find(site, label string) *Account {
	for _, a := range s.accs {
		if a.Site == site && a.Label == label {
			return a
		}
	}
	return nil
}
