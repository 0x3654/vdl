package tgbot

import (
	"strings"
	"testing"
)

func TestParseCookieHeader(t *testing.T) {
	m := ParseCookiePaste("auth_token=abc123; ct0=def456")
	if m["auth_token"] != "abc123" || m["ct0"] != "def456" {
		t.Fatalf("%+v", m)
	}
}

func TestParseMultilineKV(t *testing.T) {
	m := ParseCookiePaste("auth_token\t= abc123,\nct0 = def456;")
	if m["auth_token"] != "abc123" || m["ct0"] != "def456" {
		t.Fatalf("%+v", m)
	}
}

func TestParseJSON(t *testing.T) {
	m := ParseCookiePaste(`{"auth_token":"aaa","ct0":"bbb"}`)
	if m["auth_token"] != "aaa" || m["ct0"] != "bbb" {
		t.Fatalf("%+v", m)
	}
}

func TestParseBareTwoLines(t *testing.T) {
	m := ParseCookiePaste(strings.Repeat("f", 40) + "\n" + strings.Repeat("c", 160))
	auth, ct0, ok := TwitterAuth(m)
	if !ok {
		t.Fatalf("классификация не сработала: %+v", m)
	}
	if auth != strings.Repeat("f", 40) || ct0 != strings.Repeat("c", 160) {
		t.Errorf("auth=%s… ct0=%s…", auth[:4], ct0[:4])
	}
}

func TestParseGarbage(t *testing.T) {
	if m := ParseCookiePaste("привет, как дела?"); m == nil {
		t.Fatal("мусор должен дать bare-элементы, не nil — и потом отвергнуться TwitterAuth")
	} else if _, _, ok := TwitterAuth(m); ok {
		t.Fatal("мусор не должен проходить как twitter-пара")
	}
	if ParseCookiePaste("") != nil {
		t.Error("пустая строка → nil")
	}
}

func TestTwitterAuthAmbiguousBare(t *testing.T) {
	// оба значения 40-hex → неоднозначно, просим имена
	m := ParseCookiePaste(strings.Repeat("a", 40) + "\n" + strings.Repeat("b", 40))
	if _, _, ok := TwitterAuth(m); ok {
		t.Fatal("неоднозначная пара должна требовать имена")
	}
}

func TestTwitterAuthNamed(t *testing.T) {
	m := map[string]string{"auth_token": "x", "ct0": "y"}
	auth, ct0, ok := TwitterAuth(m)
	if !ok || auth != "x" || ct0 != "y" {
		t.Fatalf("%q %q %v", auth, ct0, ok)
	}
}

// Куки не должны эхаться в ошибки (тест формата сообщений).
func TestNoSecretEcho(t *testing.T) {
	secret := strings.Repeat("s", 40)
	m := ParseCookiePaste(secret + "\n" + secret)
	if _, _, ok := TwitterAuth(m); ok {
		t.Fatal("не должно проходить")
	}
	// сам парсер молчит — секрет не попадает ни в какие строки ошибок здесь
}
