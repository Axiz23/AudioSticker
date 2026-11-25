package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const maxStickerBytes = 500 * 1024

// userError is an error whose text is safe and useful to show in the chat.
type userError string

func (e userError) Error() string { return string(e) }

type probeInfo struct {
	HasAudio bool
	HasVideo bool
	Duration float64 // seconds, 0 when unknown
}

// acquireWorker waits for a free ffmpeg slot. This is what keeps the CPU from being
// swamped when many users send media at the same time: jobs queue here.
func (b *Bot) acquireWorker(ctx context.Context) error {
	select {
	case b.workers <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (b *Bot) releaseWorker() { <-b.workers }

func (b *Bot) runTool(ctx context.Context, prog string, args ...string) ([]byte, error) {
	if err := b.acquireWorker(ctx); err != nil {
		return nil, err
	}
	defer b.releaseWorker()

	cctx, cancel := context.WithTimeout(ctx, b.cfg.FFTimeout)
	defer cancel()

	var stderr bytes.Buffer
	cmd := exec.CommandContext(cctx, prog, args...)
	cmd.Stderr = &stderr
	cmd.WaitDelay = 3 * time.Second

	out, err := cmd.Output()
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err() // whole job timed out or the bot is shutting down
		}
		if errors.Is(cctx.Err(), context.DeadlineExceeded) {
			b.log.Error("tool timed out", "tool", prog, "limit", b.cfg.FFTimeout)
			return nil, userError("conversion took too long, try a shorter video")
		}
		tail := strings.TrimSpace(stderr.String())
		if r := []rune(tail); len(r) > 300 {
			tail = string(r[len(r)-300:])
		}
		return nil, fmt.Errorf("%s failed: %v: %s", prog, err, tail)
	}
	return out, nil
}

func (b *Bot) probe(ctx context.Context, path string) (probeInfo, error) {
	out, err := b.runTool(ctx, "ffprobe", "-v", "error",
		"-show_entries", "format=duration:stream=codec_type",
		"-of", "json", path)
	if err != nil {
		return probeInfo{}, err
	}
	var parsed struct {
		Streams []struct {
			CodecType string `json:"codec_type"`
		} `json:"streams"`
		Format struct {
			Duration string `json:"duration"`
		} `json:"format"`
	}
	if err := json.Unmarshal(out, &parsed); err != nil {
		return probeInfo{}, fmt.Errorf("ffprobe output: %w", err)
	}
	var info probeInfo
	for _, s := range parsed.Streams {
		switch s.CodecType {
		case "audio":
			info.HasAudio = true
		case "video":
			info.HasVideo = true
		}
	}
	if d, err := strconv.ParseFloat(parsed.Format.Duration, 64); err == nil && d > 0 && !math.IsInf(d, 0) {
		info.Duration = d
	}
	return info, nil
}

func secs(v float64) string { return strconv.FormatFloat(v, 'f', 2, 64) }

// visualSrc describes the file that becomes the sticker picture.
type visualSrc struct {
	path     string
	kind     mediaKind
	animated bool // sticker input only
	size     int64
}

// toWebp makes a 512x512 animated WebP of at most maxStickerBytes. A sticker that is
// already small enough is used as is.
func (b *Bot) toWebp(ctx context.Context, dir string, v visualSrc, length float64) ([]byte, bool, error) {
	if v.kind == kindSticker && v.size <= maxStickerBytes {
		data, err := os.ReadFile(v.path)
		return data, v.animated, err
	}

	out := filepath.Join(dir, "sticker.webp")
	tiers := []struct{ fps, quality int }{{15, 60}, {12, 45}, {10, 35}, {8, 25}, {8, 15}}
	for _, t := range tiers {
		vf := fmt.Sprintf("fps=%d,scale=512:512:force_original_aspect_ratio=decrease:flags=bicubic,"+
			"format=rgba,pad=512:512:(ow-iw)/2:(oh-ih)/2:color=0x00000000", t.fps)

		args := []string{"-hide_banner", "-loglevel", "error", "-y", "-threads", "2"}
		if v.kind == kindImage {
			// Turn a still image into a clip that lasts as long as the audio.
			args = append(args, "-loop", "1", "-framerate", strconv.Itoa(t.fps))
		}
		args = append(args,
			"-t", secs(length), "-i", v.path,
			"-an", "-vf", vf,
			"-c:v", "libwebp", "-lossless", "0", "-q:v", strconv.Itoa(t.quality),
			"-compression_level", "4", "-loop", "0",
			out)

		if _, err := b.runTool(ctx, "ffmpeg", args...); err != nil {
			return nil, false, err
		}
		data, err := os.ReadFile(out)
		if err != nil {
			return nil, false, err
		}
		if len(data) <= maxStickerBytes {
			return data, true, nil
		}
	}
	return nil, false, userError("sticker is over 500 KB, use a shorter video")
}

// toOpus extracts the first audio stream of in (an audio file or a video) as Ogg Opus
// mono 32 kbps, cut to length seconds. It also returns the real duration in seconds.
func (b *Bot) toOpus(ctx context.Context, dir, in string, length float64) ([]byte, uint32, error) {
	out := filepath.Join(dir, "audio.ogg")
	_, err := b.runTool(ctx, "ffmpeg",
		"-hide_banner", "-loglevel", "error", "-y", "-threads", "1",
		"-t", secs(length), "-i", in,
		"-map", "0:a:0", "-vn",
		"-c:a", "libopus", "-b:a", "32k", "-ar", "48000", "-ac", "1", "-f", "ogg",
		out)
	if err != nil {
		return nil, 0, err
	}
	data, err := os.ReadFile(out)
	if err != nil {
		return nil, 0, err
	}

	seconds := uint32(1)
	if info, err := b.probe(ctx, out); err == nil && info.Duration > 0 {
		seconds = uint32(math.Max(1, math.Ceil(info.Duration)))
	}
	return data, seconds, nil
}

// rendered is everything needed to send an audio sticker.
type rendered struct {
	webp     []byte
	animated bool
	opus     []byte
	seconds  uint32
}

// render builds the WebP and the Opus audio. audioPath may be a plain audio file or
// the same video file as the picture (the .mp4 case).
func (b *Bot) render(ctx context.Context, dir string, v visualSrc, audioPath string) (*rendered, error) {
	info, err := b.probe(ctx, audioPath)
	if err != nil {
		return nil, userError("could not read that media")
	}
	if !info.HasAudio {
		return nil, userError("no audio track found")
	}

	length := b.cfg.AudioSecs
	if info.Duration > 0 && info.Duration < length {
		length = info.Duration
	}

	t0 := time.Now()
	opus, seconds, err := b.toOpus(ctx, dir, audioPath, length)
	if err != nil {
		return nil, err
	}
	t1 := time.Now()
	webp, animated, err := b.toWebp(ctx, dir, v, math.Min(length, b.cfg.StickerSecs))
	if err != nil {
		return nil, err
	}
	b.log.Info("converted",
		"audio_ms", t1.Sub(t0).Milliseconds(), "webp_ms", time.Since(t1).Milliseconds(),
		"webp_kb", len(webp)/1024, "secs", seconds)
	return &rendered{webp: webp, animated: animated, opus: opus, seconds: seconds}, nil
}
