// cookies.go — робастный парсер вставленных кук: админ копирует из F12 как
// удобно, бот понимает все разумные форматы. Ошибки не эхают вставленный текст.
package tgbot

import (
	"encoding/json"
	"regexp"
	"strings"
)

var cookieNameRe = regexp.MustCompile(`^[A-Za-z0-9_.\-]+$`)

var pairRe = regexp.MustCompile(`^\s*([A-Za-z0-9_.\-]+)\s*[=:]\s*(.+?)\s*[;,]?\s*$`)

// ParseCookiePaste — строка из чата → карта кук. Понимает:
//  1. cookie-хедер: "auth_token=abc; ct0=def" (разделители ; , \n)
//  2. JSON: {"auth_token":"...","ct0":"..."}
//  3. k=v на отдельных строках (DevTools-таблица)
//  4. две голых строки (без имён)
//
// Голые значения попадают под ключи "#bare1"/"#bare2" — классификация
// по паттернам в TwitterAuth ниже.
func ParseCookiePaste(s string) map[string]string {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}
	if strings.HasPrefix(s, "{") {
		var m map[string]string
		if err := json.Unmarshal([]byte(s), &m); err == nil && len(m) > 0 {
			return lowerKeys(m)
		}
	}
	out := map[string]string{}
	bare := []string{}
	for _, seg := range strings.FieldsFunc(s, func(r rune) bool {
		return r == ';' || r == ',' || r == '\n' || r == '\r'
	}) {
		// таблица DevTools Application→Cookies: name<TAB>value<TAB>домен…
		if f := strings.SplitN(seg, "\t", 3); len(f) >= 2 && cookieNameRe.MatchString(strings.TrimSpace(f[0])) {
			v := strings.Trim(strings.TrimSpace(f[1]), "=;, \t") // 'name<TAB>= value,'
			if v != "" && !strings.Contains(v, "\t") {
				out[strings.ToLower(strings.TrimSpace(f[0]))] = strings.Trim(v, `"`)
			}
			continue
		}
		if m := pairRe.FindStringSubmatch(seg); m != nil {
			out[strings.ToLower(m[1])] = cleanValue(m[2])
			continue
		}
		seg = strings.TrimSpace(seg)
		if seg != "" {
			bare = append(bare, seg)
		}
	}
	for i, v := range bare {
		if i > 2 {
			break
		}
		out["#bare"+string(rune('1'+i))] = v
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// TwitterAuth — достаёт пару twitter из карты: именованные ключи или
// классификация двух голых значений (auth_token = ровно 40 hex,
// ct0 = всё остальное длиннее 20). ok=false → имён нет и классифицировать
// нельзя, просим вставить с именами.
func TwitterAuth(m map[string]string) (authToken, ct0 string, ok bool) {
	if m == nil {
		return "", "", false
	}
	authToken, ct0 = m["auth_token"], m["ct0"]
	if authToken != "" && ct0 != "" {
		return authToken, ct0, true
	}
	b1, b2 := m["#bare1"], m["#bare2"]
	if b1 == "" || b2 == "" {
		return "", "", false
	}
	hex40 := regexp.MustCompile(`^[0-9a-fA-F]{40}$`)
	switch {
	case hex40.MatchString(b1) && !hex40.MatchString(b2):
		return b1, b2, true
	case hex40.MatchString(b2) && !hex40.MatchString(b1):
		return b2, b1, true
	default:
		return "", "", false
	}
}

func lowerKeys(m map[string]string) map[string]string {
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[strings.ToLower(k)] = v
	}
	return out
}

func cleanValue(v string) string {
	v = strings.Trim(v, `"' `)
	return strings.TrimSuffix(v, ";")
}
