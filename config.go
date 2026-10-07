package main

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// Config — всё через env (стиль nnm-rss): в образе нет файлов-конфигов,
// секреты живут в ansible vault и /data.
type Config struct {
	Port           string        // слушать (за nginx, 127.0.0.1)
	DataDir        string        // том с состоянием: accounts.json, query-ids.json
	Token          string        // доступ к /dl и /admin (constant-time compare)
	CobaltURL      string        // базовый URL cobalt-инстанса
	TGBotToken     string        // пусто → бот и чекер выключены
	TGAdminChats   []int64       // супергруппа управления куками + личка
	CheckInterval  time.Duration // период health-check; 0 → выключен
	CookiesOut     string        // куда материализовать cookies.json для cobalt
	ResolveTimeout time.Duration // бюджет резолва одного /dl
	RateRPM        int           // rate limit /dl, запросов в минуту на IP
	RateBurst      int
}

func Load() (Config, error) {
	cfg := Config{
		Port:           envStr("PORT", "8360"),
		DataDir:        envStr("DATA_DIR", "/data"),
		Token:          os.Getenv("TOKEN"),
		CobaltURL:      strings.TrimRight(envStr("COBALT_URL", "http://127.0.0.1:9000"), "/"),
		TGBotToken:     os.Getenv("TG_BOT_TOKEN"),
		CheckInterval:  envDur("CHECK_INTERVAL", 12*time.Hour),
		CookiesOut:     os.Getenv("COOKIES_OUT"),
		ResolveTimeout: envDur("RESOLVE_TIMEOUT", 30*time.Second),
		RateRPM:        envInt("RATE_RPM", 10),
		RateBurst:      envInt("RATE_BURST", 5),
	}
	for _, s := range strings.Split(os.Getenv("TG_ADMIN_CHATS"), ",") {
		s = strings.TrimSpace(s)
		if s == "" {
			continue
		}
		id, err := strconv.ParseInt(s, 10, 64)
		if err != nil {
			return cfg, fmt.Errorf("TG_ADMIN_CHATS: %q не число", s)
		}
		cfg.TGAdminChats = append(cfg.TGAdminChats, id)
	}
	if cfg.Token == "" {
		return cfg, fmt.Errorf("TOKEN не задан — доступ к /dl без токена не открываем")
	}
	if cfg.TGBotToken != "" && len(cfg.TGAdminChats) == 0 {
		return cfg, fmt.Errorf("TG_BOT_TOKEN задан, но нет TG_ADMIN_CHATS — бот некому отвечать")
	}
	return cfg, nil
}

func envStr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func envInt(k string, def int) int {
	if v := os.Getenv(k); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

func envDur(k string, def time.Duration) time.Duration {
	if v := os.Getenv(k); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
	}
	return def
}
