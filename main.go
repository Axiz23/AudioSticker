// WhatsApp sound sticker bot.
//
//	.sa  (reply to a video .mp4)           -> audio sticker, sound taken from the video
//	.sa  (reply to a visual, then to audio) -> audio sticker from two separate files
//
// Needs ffmpeg and ffprobe (with libwebp and libopus) in PATH.
package main

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/mdp/qrterminal/v3"
	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/store/sqlstore"
	waLog "go.mau.fi/whatsmeow/util/log"
	_ "modernc.org/sqlite" // pure Go SQLite driver: no CGO, so cross compiling just works
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "fatal:", err)
		os.Exit(1)
	}
}

func run() error {
	// Load .env first: it can set LOG_LEVEL and every other option.
	envFile := envStr("ENV_FILE", ".env")
	loaded, envErr := loadDotEnv(envFile)

	cfg := loadConfig()
	log := newLogger(cfg.LogLevel)
	if envErr != nil {
		log.Warn("could not read env file", "file", envFile, "err", envErr)
	} else if loaded > 0 {
		log.Info("env file loaded", "file", envFile, "vars", loaded)
	}

	for _, tool := range []string{"ffmpeg", "ffprobe"} {
		if _, err := exec.LookPath(tool); err != nil {
			return fmt.Errorf("%s not found in PATH", tool)
		}
	}
	selfCheck(log)
	cleanStaleTemp()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	dsn := fmt.Sprintf("file:%s?_pragma=foreign_keys(1)&_pragma=busy_timeout(10000)&_pragma=journal_mode(WAL)", cfg.DBPath)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	container := sqlstore.NewWithDB(db, "sqlite3", waLog.Stdout("DB", "WARN", false))
	defer container.Close()
	if err := container.Upgrade(ctx); err != nil {
		return fmt.Errorf("database upgrade: %w", err)
	}
	device, err := container.GetFirstDevice(ctx)
	if err != nil {
		return fmt.Errorf("device store: %w", err)
	}

	cli := whatsmeow.NewClient(device, waLog.Stdout("WA", "WARN", false))
	bot := newBot(ctx, cli, cfg, log)
	cli.AddEventHandler(bot.onEvent)
	go bot.Janitor(ctx)

	if err := connect(ctx, cli, cfg, log); err != nil {
		return err
	}
	log.Info("ready", "workers", cfg.Workers, "max_jobs", cfg.MaxJobs, "prefix", cfg.Prefix)

	<-ctx.Done()
	log.Info("shutting down")
	bot.Wait(30 * time.Second)
	cli.Disconnect()
	return nil
}

func connect(ctx context.Context, cli *whatsmeow.Client, cfg *Config, log *slog.Logger) error {
	if cli.Store.ID != nil {
		return cli.Connect() // already paired
	}

	if cfg.PairPhone != "" {
		if err := cli.Connect(); err != nil {
			return err
		}
		code, err := cli.PairPhone(ctx, cfg.PairPhone, true, whatsmeow.PairClientChrome, "Chrome (Linux)")
		if err != nil {
			return fmt.Errorf("pair code: %w", err)
		}
		fmt.Println("Pair code:", code)
		return nil
	}

	qrChan, err := cli.GetQRChannel(ctx)
	if err != nil {
		return err
	}
	if err := cli.Connect(); err != nil {
		return err
	}
	go func() {
		for item := range qrChan {
			switch item.Event {
			case whatsmeow.QRChannelEventCode:
				qrterminal.GenerateHalfBlock(item.Code, qrterminal.L, os.Stdout)
			case whatsmeow.QRChannelEventError:
				log.Error("pairing error", "err", item.Error)
			default:
				log.Info("pairing", "event", item.Event)
			}
		}
	}()
	return nil
}

func newLogger(level string) *slog.Logger {
	var l slog.Level
	switch level {
	case "debug":
		l = slog.LevelDebug
	case "warn":
		l = slog.LevelWarn
	case "error":
		l = slog.LevelError
	default:
		l = slog.LevelInfo
	}
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: l}))
}

// selfCheck verifies that the bundled whatsmeow classifies an audio sticker as
// type=media mediatype=sticker. Without that, the server silently drops the message.
func selfCheck(log *slog.Logger) {
	msg := &waE2E.Message{
		AudioStickerMessage: &waE2E.FutureProofMessage{
			Message: &waE2E.Message{StickerMessage: &waE2E.StickerMessage{}},
		},
	}
	typ, media := whatsmeow.StanzaTypesOf(msg)
	if typ == "media" && media == "sticker" {
		log.Info("self-check ok")
		return
	}
	log.Error("self-check failed: whatsmeow is not patched, audio stickers will be dropped",
		"type", typ, "mediatype", media)
}

// cleanStaleTemp removes work directories left behind by a crashed run.
func cleanStaleTemp() {
	dirs, _ := filepath.Glob(filepath.Join(os.TempDir(), "wa-sa-*"))
	for _, d := range dirs {
		if st, err := os.Stat(d); err == nil && time.Since(st.ModTime()) > time.Hour {
			_ = os.RemoveAll(d)
		}
	}
}
