package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	"google.golang.org/protobuf/proto"
)

const (
	maxMessageAge = 2 * time.Minute // ignore commands that were queued while offline
	usageText     = "Reply to a video (.mp4) with .sa. Or reply to a video/GIF/image/sticker and an audio with .sa"
)

// Bot is safe for concurrent use. Every incoming command runs in its own goroutine,
// bounded by three limits:
//
//	jobs    - how many commands are being handled at once (running + queued)
//	workers - how many ffmpeg/ffprobe processes run at once
//	guard   - one heavy job per user at a time, plus a per-user rate limit
type Bot struct {
	cli *whatsmeow.Client
	cfg *Config
	log *slog.Logger

	sess  *Sessions
	guard *Guard
	seen  *Dedupe

	jobs    chan struct{}
	workers chan struct{}
	busyMsg chan struct{} // caps the "busy" replies sent while overloaded

	wg  sync.WaitGroup
	ctx context.Context // cancelled on shutdown
}

func newBot(ctx context.Context, cli *whatsmeow.Client, cfg *Config, log *slog.Logger) *Bot {
	return &Bot{
		cli:     cli,
		cfg:     cfg,
		log:     log,
		sess:    newSessions(cfg.SessionTTL, cfg.MaxSessions),
		guard:   newGuard(cfg.UserPerMin),
		seen:    newDedupe(10 * time.Minute),
		jobs:    make(chan struct{}, cfg.MaxJobs),
		workers: make(chan struct{}, cfg.Workers),
		busyMsg: make(chan struct{}, 16),
		ctx:     ctx,
	}
}

// Janitor removes expired state until ctx is done.
func (b *Bot) Janitor(ctx context.Context) {
	t := time.NewTicker(30 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			b.sess.Sweep()
			b.guard.Sweep()
			b.seen.Sweep()
			b.log.Debug("janitor", "sessions", b.sess.Len(), "jobs", len(b.jobs))
		}
	}
}

// Wait blocks until running jobs finish or the timeout passes.
func (b *Bot) Wait(timeout time.Duration) {
	done := make(chan struct{})
	go func() { b.wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(timeout):
		b.log.Warn("shutdown timeout, some jobs were cut off")
	}
}

func (b *Bot) onEvent(raw any) {
	switch evt := raw.(type) {
	case *events.Message:
		b.onMessage(evt)
	case *events.Connected:
		b.log.Info("connected")
	case *events.Disconnected:
		b.log.Warn("disconnected, reconnecting")
	case *events.LoggedOut:
		b.log.Error("logged out: delete the database file and pair again")
	}
}

// parseCommand returns the argument of a ".sa [arg]" command.
func parseCommand(prefix, text string) (arg string, ok bool) {
	f := strings.Fields(text)
	if len(f) == 0 || !strings.EqualFold(f[0], prefix) {
		return "", false
	}
	if len(f) > 1 {
		arg = strings.ToLower(f[1])
	}
	return arg, true
}

func (b *Bot) allowed(src types.MessageSource) bool {
	if src.IsFromMe || len(b.cfg.Allowed) == 0 {
		return true
	}
	return b.cfg.Allowed[src.Sender.User] || (src.SenderAlt.User != "" && b.cfg.Allowed[src.SenderAlt.User])
}

// onMessage must return quickly: whatsmeow calls it from its event loop.
func (b *Bot) onMessage(evt *events.Message) {
	arg, ok := parseCommand(b.cfg.Prefix, messageText(evt.Message))
	if !ok || b.ctx.Err() != nil {
		return
	}
	if evt.Info.Chat.Server == types.BroadcastServer || evt.Info.Chat.Server == types.NewsletterServer {
		return
	}
	if time.Since(evt.Info.Timestamp) > maxMessageAge {
		return
	}
	if !b.allowed(evt.Info.MessageSource) {
		return
	}
	if !b.seen.First(evt.Info.Chat.String() + "/" + evt.Info.ID) {
		return
	}

	select {
	case b.jobs <- struct{}{}:
	default:
		b.log.Warn("overloaded, command rejected", "jobs", cap(b.jobs))
		b.replyBusy(evt)
		return
	}

	b.wg.Add(1)
	go func() {
		defer b.wg.Done()
		defer func() { <-b.jobs }()
		defer func() {
			if r := recover(); r != nil {
				b.log.Error("panic in job", "panic", r)
			}
		}()
		b.handle(evt, arg)
	}()
}

func (b *Bot) replyBusy(evt *events.Message) {
	select {
	case b.busyMsg <- struct{}{}:
	default:
		return // already sending enough "busy" replies
	}
	go func() {
		defer func() { <-b.busyMsg }()
		b.say(context.Background(), evt, "Busy, try again in a moment")
	}()
}

func sessionKey(evt *events.Message) string {
	return evt.Info.Chat.String() + "/" + evt.Info.Sender.ToNonAD().String()
}

func (b *Bot) handle(evt *events.Message, arg string) {
	ctx, cancel := context.WithTimeout(b.ctx, b.cfg.JobTimeout)
	defer cancel()
	key := sessionKey(evt)

	if arg == "reset" {
		b.sess.Reset(key)
		b.say(ctx, evt, "Session cleared")
		return
	}

	quoted := quotedMessage(evt.Message)
	if quoted == nil {
		b.say(ctx, evt, usageText)
		return
	}
	ref, ok := classify(quoted)
	if !ok {
		b.say(ctx, evt, usageText)
		return
	}
	if ref.size > uint64(b.cfg.MaxInput) {
		b.say(ctx, evt, fmt.Sprintf("File too large (max %d MB)", b.cfg.MaxInput>>20))
		return
	}

	// A normal video normally carries its own audio, so one reply is enough.
	// The exception is a session that already waits for the visual part.
	if ref.kind == kindVideo && !b.sess.HasAudio(key) {
		b.handleVideo(ctx, evt, key, ref)
		return
	}
	b.addToSession(ctx, evt, key, ref)
}

// handleVideo is the one-step .mp4 flow: the video supplies picture and sound.
func (b *Bot) handleVideo(ctx context.Context, evt *events.Message, key string, ref mediaRef) {
	release, deny := b.guard.Acquire(key)
	if deny != denyNone {
		b.sayDenied(ctx, evt, deny)
		return
	}
	defer release()

	b.typing(ctx, evt.Info.Chat, true)
	defer b.typing(context.WithoutCancel(ctx), evt.Info.Chat, false)

	dir, err := os.MkdirTemp("", "wa-sa-*")
	if err != nil {
		b.fail(ctx, evt, err)
		return
	}
	defer os.RemoveAll(dir)

	in := filepath.Join(dir, "video.in")
	if err := b.download(ctx, ref, in); err != nil {
		b.fail(ctx, evt, err)
		return
	}
	info, err := b.probe(ctx, in)
	if err != nil {
		b.fail(ctx, evt, userError("could not read that video"))
		return
	}

	if !info.HasAudio {
		// No sound in this video: fall back to the two-reply flow with it as the picture.
		release()
		b.addToSession(ctx, evt, key, ref)
		return
	}

	b.sess.Reset(key) // an old half-finished session is no longer relevant
	r, err := b.render(ctx, dir, visualSrc{path: in, kind: kindVideo}, in)
	if err != nil {
		b.fail(ctx, evt, err)
		return
	}
	if err := b.sendAudioSticker(ctx, evt.Info.Chat, r); err != nil {
		b.fail(ctx, evt, err)
	}
}

// addToSession is the two-reply flow: visual and audio arrive in separate replies.
func (b *Bot) addToSession(ctx context.Context, evt *events.Message, key string, ref mediaRef) {
	res := b.sess.Put(key, ref)
	if !res.ready {
		b.say(ctx, evt, "Now reply the "+res.missing+" with .sa")
		return
	}

	release, deny := b.guard.Acquire(key)
	if deny != denyNone {
		b.sayDenied(ctx, evt, deny)
		return
	}
	defer release()

	b.typing(ctx, evt.Info.Chat, true)
	defer b.typing(context.WithoutCancel(ctx), evt.Info.Chat, false)

	dir, err := os.MkdirTemp("", "wa-sa-*")
	if err != nil {
		b.fail(ctx, evt, err)
		return
	}
	defer os.RemoveAll(dir)

	visPath := filepath.Join(dir, "visual.in")
	audPath := filepath.Join(dir, "audio.in")
	if err := b.downloadBoth(ctx, res.visual, visPath, res.audio, audPath); err != nil {
		b.fail(ctx, evt, err)
		return
	}
	st, err := os.Stat(visPath)
	if err != nil {
		b.fail(ctx, evt, err)
		return
	}

	vis := visualSrc{path: visPath, kind: res.visual.kind, animated: res.visual.animated, size: st.Size()}
	r, err := b.render(ctx, dir, vis, audPath)
	if err != nil {
		b.fail(ctx, evt, err)
		return
	}
	if err := b.sendAudioSticker(ctx, evt.Info.Chat, r); err != nil {
		b.fail(ctx, evt, err)
	}
}

// ---------------------------------------------------------------------------
// download
// ---------------------------------------------------------------------------

func (b *Bot) download(ctx context.Context, ref mediaRef, path string) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()

	if err := b.cli.DownloadToFile(ctx, ref.dl, f); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		b.log.Error("download failed", "err", err)
		return userError("could not download that media")
	}
	if st, err := f.Stat(); err == nil && st.Size() > b.cfg.MaxInput {
		return userError("file too large")
	}
	return nil
}

func (b *Bot) downloadBoth(ctx context.Context, a mediaRef, aPath string, c mediaRef, cPath string) error {
	var wg sync.WaitGroup
	var errA, errC error
	wg.Add(2)
	go func() { defer wg.Done(); errA = b.download(ctx, a, aPath) }()
	go func() { defer wg.Done(); errC = b.download(ctx, c, cPath) }()
	wg.Wait()
	return errors.Join(errA, errC)
}

// ---------------------------------------------------------------------------
// chat replies
// ---------------------------------------------------------------------------

// say replies to the command message, quoting it. It uses its own timeout so that a
// reply still goes out after the job context was cancelled or timed out.
func (b *Bot) say(ctx context.Context, evt *events.Message, text string) {
	rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 20*time.Second)
	defer cancel()

	ci := &waE2E.ContextInfo{
		StanzaID:      proto.String(evt.Info.ID),
		QuotedMessage: &waE2E.Message{Conversation: proto.String(messageText(evt.Message))},
	}
	if evt.Info.IsGroup {
		ci.Participant = proto.String(evt.Info.Sender.ToNonAD().String())
	}
	msg := &waE2E.Message{ExtendedTextMessage: &waE2E.ExtendedTextMessage{
		Text:        proto.String(text),
		ContextInfo: ci,
	}}
	if _, err := b.cli.SendMessage(rctx, evt.Info.Chat, msg); err != nil {
		b.log.Warn("reply failed", "err", err)
	}
}

func (b *Bot) sayDenied(ctx context.Context, evt *events.Message, deny string) {
	if deny == denyBusy {
		b.say(ctx, evt, "Still working on your previous request")
		return
	}
	b.say(ctx, evt, "Too many requests, try again in a minute")
}

func (b *Bot) fail(ctx context.Context, evt *events.Message, err error) {
	var ue userError
	if errors.As(err, &ue) {
		b.say(ctx, evt, "Failed: "+string(ue))
		return
	}
	if errors.Is(err, context.DeadlineExceeded) {
		b.log.Error("job timed out")
		b.say(ctx, evt, "Failed: took too long")
		return
	}
	if errors.Is(err, context.Canceled) {
		return // shutting down
	}
	b.log.Error("job failed", "err", err)
	b.say(ctx, evt, "Failed, please try again")
}

func (b *Bot) typing(ctx context.Context, chat types.JID, on bool) {
	state := types.ChatPresencePaused
	if on {
		state = types.ChatPresenceComposing
	}
	_ = b.cli.SendChatPresence(ctx, chat, state, types.ChatPresenceMediaText)
}
