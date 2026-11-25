package main

import (
	"os"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// Config holds every tunable value. All of them come from environment variables.
type Config struct {
	DBPath    string
	PairPhone string
	LogLevel  string
	Prefix    string
	Diag      bool

	SessionTTL  time.Duration // how long a half-finished .sa session lives
	StickerSecs float64       // max length of the animated sticker
	AudioSecs   float64       // max length of the audio
	MaxInput    int64         // max size of a downloaded input file, in bytes
	MaxStickerBytes int       // max size of the output WebP; the encoder lowers fps/quality to fit

	Workers     int           // ffmpeg/ffprobe processes running at the same time
	MaxJobs     int           // jobs in flight (running + waiting for a worker)
	UserPerMin  int           // heavy jobs one user may start per minute
	FFTimeout   time.Duration // timeout of a single ffmpeg run
	JobTimeout  time.Duration // timeout of a whole job (download + convert + upload + send)
	MaxSessions int           // global cap of pending .sa sessions

	Allowed map[string]bool // empty = everyone may use the bot
}

func loadConfig() *Config {
	workers := clamp(envInt("WORKERS", runtime.NumCPU()), 1, 16)
	c := &Config{
		DBPath:    envStr("WA_DB", "whatsapp.db"),
		PairPhone: strings.TrimSpace(os.Getenv("PAIR_PHONE")),
		LogLevel:  strings.ToLower(envStr("LOG_LEVEL", "info")),
		Prefix:    envStr("PREFIX", ".sa"),
		Diag:      os.Getenv("DIAG") == "1",

		SessionTTL:  time.Duration(envInt("SESSION_SECS", 120)) * time.Second,
		StickerSecs: float64(envInt("STICKER_MAX_SECS", 5)),
		AudioSecs:   float64(envInt("AUDIO_MAX_SECS", 5)),
		MaxInput:    int64(envInt("MAX_INPUT_MB", 16)) << 20,
		MaxStickerBytes: envInt("STICKER_MAX_KB", 500) * 1024,

		Workers:     workers,
		MaxJobs:     envInt("MAX_JOBS", workers*8),
		UserPerMin:  envInt("USER_PER_MIN", 6),
		FFTimeout:   time.Duration(envInt("FFMPEG_TIMEOUT_SECS", 90)) * time.Second,
		JobTimeout:  time.Duration(envInt("JOB_TIMEOUT_SECS", 240)) * time.Second,
		MaxSessions: envInt("MAX_SESSIONS", 2000),

		Allowed: map[string]bool{},
	}
	for _, n := range strings.Split(os.Getenv("ALLOWED_NUMBERS"), ",") {
		n = digitsOnly(n)
		if n != "" {
			c.Allowed[n] = true
		}
	}
	return c
}

func envStr(key, def string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return def
}

func envInt(key string, def int) int {
	if v, err := strconv.Atoi(strings.TrimSpace(os.Getenv(key))); err == nil && v > 0 {
		return v
	}
	return def
}

func clamp(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

func digitsOnly(s string) string {
	var b strings.Builder
	for _, r := range s {
		if r >= '0' && r <= '9' {
			b.WriteRune(r)
		}
	}
	return b.String()
}
