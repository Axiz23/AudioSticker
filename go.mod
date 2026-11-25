module audio-sticker-bot

go 1.26.0

// whatsmeow is bundled in third_party/ because upstream does not know the
// audioStickerMessage wrapper yet (see patches/whatsmeow-audio-sticker.patch).
require go.mau.fi/whatsmeow v0.0.0-00010101000000-000000000000

replace go.mau.fi/whatsmeow => ./third_party/whatsmeow
