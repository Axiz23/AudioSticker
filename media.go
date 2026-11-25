package main

import (
	"strings"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waE2E"
)

type mediaKind int

const (
	kindVideo   mediaKind = iota + 1 // normal video (.mp4), usually has its own audio
	kindGIF                          // video sent as GIF, never has audio
	kindImage                        // still image
	kindSticker                      // WebP sticker
	kindAudio                        // audio / voice note
)

func (k mediaKind) isVisual() bool { return k != kindAudio }

// mediaRef is a light reference to a WhatsApp media message. The file itself is only
// downloaded when a job actually needs it, so pending sessions cost almost no memory.
type mediaRef struct {
	kind     mediaKind
	dl       whatsmeow.DownloadableMessage
	size     uint64 // declared file length, 0 if unknown
	animated bool   // only meaningful for stickers
}

// unwrap peels the "future proof" wrappers (ephemeral, view once, ...) off a message.
func unwrap(m *waE2E.Message) *waE2E.Message {
	for i := 0; i < 6 && m != nil; i++ {
		var next *waE2E.Message
		switch {
		case m.GetEphemeralMessage().GetMessage() != nil:
			next = m.GetEphemeralMessage().GetMessage()
		case m.GetViewOnceMessage().GetMessage() != nil:
			next = m.GetViewOnceMessage().GetMessage()
		case m.GetViewOnceMessageV2().GetMessage() != nil:
			next = m.GetViewOnceMessageV2().GetMessage()
		case m.GetViewOnceMessageV2Extension().GetMessage() != nil:
			next = m.GetViewOnceMessageV2Extension().GetMessage()
		case m.GetDocumentWithCaptionMessage().GetMessage() != nil:
			next = m.GetDocumentWithCaptionMessage().GetMessage()
		}
		if next == nil {
			return m
		}
		m = next
	}
	return m
}

// classify turns a quoted message into a mediaRef. ok is false when the message holds
// nothing the bot can use.
func classify(quoted *waE2E.Message) (ref mediaRef, ok bool) {
	m := unwrap(quoted)
	if m == nil {
		return ref, false
	}
	switch {
	case m.GetVideoMessage() != nil:
		v := m.GetVideoMessage()
		kind := kindVideo
		if v.GetGifPlayback() {
			kind = kindGIF
		}
		return mediaRef{kind: kind, dl: v, size: v.GetFileLength()}, true
	case m.GetImageMessage() != nil:
		i := m.GetImageMessage()
		return mediaRef{kind: kindImage, dl: i, size: i.GetFileLength()}, true
	case m.GetStickerMessage() != nil:
		s := m.GetStickerMessage()
		return mediaRef{kind: kindSticker, dl: s, size: s.GetFileLength(), animated: s.GetIsAnimated()}, true
	case m.GetAudioMessage() != nil:
		a := m.GetAudioMessage()
		return mediaRef{kind: kindAudio, dl: a, size: a.GetFileLength()}, true
	case m.GetDocumentMessage() != nil:
		d := m.GetDocumentMessage()
		mime := strings.ToLower(d.GetMimetype())
		switch {
		case strings.HasPrefix(mime, "video/"):
			return mediaRef{kind: kindVideo, dl: d, size: d.GetFileLength()}, true
		case strings.HasPrefix(mime, "image/"):
			return mediaRef{kind: kindImage, dl: d, size: d.GetFileLength()}, true
		case strings.HasPrefix(mime, "audio/"):
			return mediaRef{kind: kindAudio, dl: d, size: d.GetFileLength()}, true
		}
	}
	return ref, false
}

func messageText(m *waE2E.Message) string {
	if m == nil {
		return ""
	}
	if c := m.GetConversation(); c != "" {
		return c
	}
	return m.GetExtendedTextMessage().GetText()
}

func quotedMessage(m *waE2E.Message) *waE2E.Message {
	return m.GetExtendedTextMessage().GetContextInfo().GetQuotedMessage()
}
