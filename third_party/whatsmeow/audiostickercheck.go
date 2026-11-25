package whatsmeow

import "go.mau.fi/whatsmeow/proto/waE2E"

// StanzaTypesOf reports the stanza "type" and "mediatype" attributes that SendMessage
// would use for msg. It exists so that applications can verify at startup that this
// copy of whatsmeow understands the audioStickerMessage wrapper (field 134).
// For a correct build, an audio sticker gives ("media", "sticker").
func StanzaTypesOf(msg *waE2E.Message) (stanzaType, mediaType string) {
	return getTypeFromMessage(msg), getMediaTypeFromMessage(msg)
}
