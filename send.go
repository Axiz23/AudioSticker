package main

import (
	"context"
	"crypto/rand"
	"fmt"
	"sync"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types"
	"google.golang.org/protobuf/proto"
)

// sendAudioSticker uploads the WebP and the Opus audio, then sends them as one
// audioStickerMessage (Message field 134 -> stickerMessage -> audioMessage).
func (b *Bot) sendAudioSticker(ctx context.Context, chat types.JID, r *rendered) error {
	var (
		wg         sync.WaitGroup
		stickerUp  whatsmeow.UploadResponse
		audioUp    whatsmeow.UploadResponse
		stickerErr error
		audioErr   error
	)
	wg.Add(2)
	go func() {
		defer wg.Done()
		stickerUp, stickerErr = b.cli.Upload(ctx, r.webp, whatsmeow.MediaImage) // stickers use the image keys
	}()
	go func() {
		defer wg.Done()
		audioUp, audioErr = b.cli.Upload(ctx, r.opus, whatsmeow.MediaAudio)
	}()
	wg.Wait()
	if stickerErr != nil {
		return fmt.Errorf("sticker upload: %w", stickerErr)
	}
	if audioErr != nil {
		return fmt.Errorf("audio upload: %w", audioErr)
	}

	now := time.Now()
	audio := &waE2E.AudioMessage{
		URL:               proto.String(audioUp.URL),
		DirectPath:        proto.String(audioUp.DirectPath),
		MediaKey:          audioUp.MediaKey,
		FileSHA256:        audioUp.FileSHA256,
		FileEncSHA256:     audioUp.FileEncSHA256,
		FileLength:        proto.Uint64(audioUp.FileLength),
		MediaKeyTimestamp: proto.Int64(now.Unix()),
		Mimetype:          proto.String("audio/ogg; codecs=opus"),
		Seconds:           proto.Uint32(r.seconds),
	}
	sticker := &waE2E.StickerMessage{
		URL:               proto.String(stickerUp.URL),
		DirectPath:        proto.String(stickerUp.DirectPath),
		MediaKey:          stickerUp.MediaKey,
		FileSHA256:        stickerUp.FileSHA256,
		FileEncSHA256:     stickerUp.FileEncSHA256,
		FileLength:        proto.Uint64(stickerUp.FileLength),
		MediaKeyTimestamp: proto.Int64(now.Unix()),
		Mimetype:          proto.String("image/webp"),
		Width:             proto.Uint32(512),
		Height:            proto.Uint32(512),
		IsAnimated:        proto.Bool(r.animated),
		StickerSentTS:     proto.Int64(now.UnixMilli()),
	}

	if b.cfg.Diag {
		// Diagnostic mode: also send the two parts as ordinary messages for comparison.
		for name, m := range map[string]*waE2E.Message{
			"plain sticker": {StickerMessage: sticker},
			"plain audio":   {AudioMessage: audio},
		} {
			if _, err := b.cli.SendMessage(ctx, chat, m); err != nil {
				b.log.Error("diag send failed", "part", name, "err", err)
			} else {
				b.log.Info("diag sent", "part", name)
			}
		}
	}

	sticker.Audio = &waE2E.StickerMessage_AudioMessage{AudioMessage: audio}

	secret := make([]byte, 32)
	if _, err := rand.Read(secret); err != nil {
		return fmt.Errorf("random: %w", err)
	}
	msg := &waE2E.Message{
		AudioStickerMessage: &waE2E.FutureProofMessage{
			Message: &waE2E.Message{StickerMessage: sticker},
		},
		MessageContextInfo: &waE2E.MessageContextInfo{MessageSecret: secret},
	}

	if _, err := b.cli.SendMessage(ctx, chat, msg); err != nil {
		return fmt.Errorf("send: %w", err)
	}
	b.log.Info("audio sticker sent",
		"webp_kb", len(r.webp)/1024, "opus_kb", len(r.opus)/1024, "secs", r.seconds)
	return nil
}
