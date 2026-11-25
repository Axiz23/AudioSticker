use std::collections::HashMap;
use std::path::PathBuf;
use std::sync::atomic::{AtomicU64, Ordering};
use std::sync::{Arc, Mutex};
use std::time::{Duration, Instant, SystemTime, UNIX_EPOCH};

use anyhow::{Context, Result, anyhow, bail};
use log::{error, info, warn};
use tokio::process::Command;
use whatsapp_rust::UploadOptions;
use whatsapp_rust::download::{DownloadLimits, MediaType};
use whatsapp_rust::pair_code::PairCodeOptions;
use whatsapp_rust::prelude::*;

const MAX_ANIMATED_STICKER_BYTES: usize = 500 * 1024; // batas WhatsApp untuk animated sticker
const MAX_INPUT_BYTES: usize = 25 * 1024 * 1024;
const FFMPEG_TIMEOUT: Duration = Duration::from_secs(90);

const HELP: &str = "Cara pakai:\n\
1. Kirim video/GIF/WebM dan audio ke chat ini.\n\
2. Reply video dengan *.sa*\n\
3. Reply audio dengan *.sa*\n\
Setelah keduanya masuk, saya kirim sticker beraudio.\n\
*.sa reset* untuk membatalkan sesi.";

// ───────────────────────── konfigurasi & sesi ─────────────────────────

struct Config {
    session_ttl: Duration,
    sticker_max_secs: u32,
    audio_max_secs: u32,
}

impl Config {
    fn from_env() -> Self {
        let num = |k: &str, d: u32| std::env::var(k).ok().and_then(|v| v.parse().ok()).unwrap_or(d);
        Self {
            session_ttl: Duration::from_secs(u64::from(num("SESSION_SECS", 120))),
            sticker_max_secs: num("STICKER_MAX_SECS", 5),
            audio_max_secs: num("AUDIO_MAX_SECS", 5),
        }
    }
}

struct Visual {
    bytes: Vec<u8>,
    /// Sudah berupa WebP (dari sticker): dipakai langsung bila ukurannya muat.
    is_webp_sticker: bool,
}

struct Session {
    visual: Option<Visual>,
    audio: Option<Vec<u8>>,
    started: Instant,
}

type Sessions = Arc<Mutex<HashMap<String, Session>>>;

enum Slot {
    Visual(Visual),
    Audio(Vec<u8>),
}

enum Outcome {
    Ready(Visual, Vec<u8>),
    Waiting(&'static str),
}

// ───────────────────────── handler pesan ─────────────────────────

/// `.sa` -> Some(""), `.sa reset` -> Some("reset"), selain itu None.
fn parse_command(text: &str) -> Option<&str> {
    let rest = text.trim().strip_prefix(".sa")?;
    if rest.is_empty() || rest.starts_with(char::is_whitespace) {
        Some(rest.trim())
    } else {
        None
    }
}

async fn say(ctx: &MessageContext, text: &str) {
    if let Err(e) = ctx.reply_quoting(text).await {
        warn!("gagal membalas: {e}");
    }
}

async fn handle(ctx: MessageContext, sessions: Sessions, cfg: Arc<Config>) {
    let Some(text) = ctx.message.text_content() else {
        return;
    };
    let Some(arg) = parse_command(text) else {
        return;
    };
    let key = format!("{}/{}", ctx.info.source.chat, ctx.info.source.sender);

    match arg {
        "" => {}
        "reset" => {
            sessions.lock().unwrap_or_else(|p| p.into_inner()).remove(&key);
            say(&ctx, "Sesi dihapus.").await;
            return;
        }
        _ => {
            say(&ctx, HELP).await;
            return;
        }
    }

    let Some(quoted) = ctx
        .message
        .context_info()
        .and_then(|c| c.quoted_message.as_option())
    else {
        say(&ctx, HELP).await;
        return;
    };

    let slot = match fetch_media(&ctx.client, quoted).await {
        Ok(Some(slot)) => slot,
        Ok(None) => {
            say(&ctx, "Pesan yang di-reply harus berupa video/GIF/WebM/sticker atau audio.").await;
            return;
        }
        Err(e) => {
            error!("ambil media gagal: {e:#}");
            say(&ctx, "Gagal mengunduh media itu. Coba kirim ulang filenya.").await;
            return;
        }
    };

    let got = match &slot {
        Slot::Visual(_) => "visual",
        Slot::Audio(_) => "audio",
    };

    let outcome = {
        let mut map = sessions.lock().unwrap_or_else(|p| p.into_inner());
        if map.get(&key).is_some_and(|s| s.started.elapsed() > cfg.session_ttl) {
            map.remove(&key);
        }
        let s = map.entry(key.clone()).or_insert_with(|| Session {
            visual: None,
            audio: None,
            started: Instant::now(),
        });
        match slot {
            Slot::Visual(v) => s.visual = Some(v),
            Slot::Audio(a) => s.audio = Some(a),
        }
        match (s.visual.is_some(), s.audio.is_some()) {
            (true, true) => match map.remove(&key) {
                Some(Session { visual: Some(v), audio: Some(a), .. }) => Outcome::Ready(v, a),
                _ => Outcome::Waiting("visual/audio"),
            },
            (true, false) => Outcome::Waiting("audio"),
            _ => Outcome::Waiting("video/GIF/WebM"),
        }
    };

    match outcome {
        Outcome::Waiting(missing) => {
            let secs = cfg.session_ttl.as_secs();
            say(
                &ctx,
                &format!("{got} diterima. Sekarang reply {missing} dengan .sa (maks {secs} detik)."),
            )
            .await;
        }
        Outcome::Ready(visual, audio) => {
            say(&ctx, "Memproses sticker beraudio…").await;
            if let Err(e) = merge_and_send(&ctx, visual, audio, &cfg).await {
                error!("merge/kirim gagal: {e:#}");
                say(&ctx, &format!("Gagal membuat sticker: {e}")).await;
            }
        }
    }
}

// ───────────────────────── unduh media ─────────────────────────

async fn fetch_media(client: &Client, quoted: &wa::Message) -> Result<Option<Slot>> {
    let base = quoted.get_base_message();
    let limits = DownloadLimits::default();

    let (slot_is_visual, is_webp_sticker, bytes) = if let Some(v) = base.video_message.as_option() {
        (true, false, client.download(v, limits).await)
    } else if let Some(i) = base.image_message.as_option() {
        (true, false, client.download(i, limits).await)
    } else if let Some(s) = base.sticker_message.as_option() {
        (true, true, client.download(s, limits).await)
    } else if let Some(a) = base.audio_message.as_option() {
        (false, false, client.download(a, limits).await)
    } else if let Some(d) = base.document_message.as_option() {
        // WebM/GIF/audio sering terkirim sebagai dokumen.
        let mime = d.mimetype.as_deref().unwrap_or("");
        let visual = mime.starts_with("video/") || mime.starts_with("image/");
        if !visual && !mime.starts_with("audio/") {
            return Ok(None);
        }
        (visual, false, client.download(d, limits).await)
    } else {
        return Ok(None);
    };

    let bytes = bytes.map_err(|e| anyhow!("download: {e:?}"))?;
    if bytes.len() > MAX_INPUT_BYTES {
        bail!("file terlalu besar ({} MB)", bytes.len() / 1024 / 1024);
    }
    Ok(Some(if slot_is_visual {
        Slot::Visual(Visual { bytes, is_webp_sticker })
    } else {
        Slot::Audio(bytes)
    }))
}

// ───────────────────────── konversi ffmpeg ─────────────────────────

static COUNTER: AtomicU64 = AtomicU64::new(0);

struct TempDir(PathBuf);

impl TempDir {
    fn new() -> Result<Self> {
        let p = std::env::temp_dir().join(format!(
            "wa-sa-{}-{}",
            std::process::id(),
            COUNTER.fetch_add(1, Ordering::Relaxed)
        ));
        std::fs::create_dir_all(&p)?;
        Ok(Self(p))
    }
    fn join(&self, name: &str) -> PathBuf {
        self.0.join(name)
    }
}

impl Drop for TempDir {
    fn drop(&mut self) {
        let _ = std::fs::remove_dir_all(&self.0);
    }
}

fn tail(b: &[u8]) -> String {
    let s = String::from_utf8_lossy(b);
    let n = s.chars().count();
    s.chars().skip(n.saturating_sub(400)).collect()
}

async fn run(program: &str, args: &[String]) -> Result<std::process::Output> {
    let fut = Command::new(program).args(args).kill_on_drop(true).output();
    let out = tokio::time::timeout(FFMPEG_TIMEOUT, fut)
        .await
        .map_err(|_| anyhow!("{program} timeout"))?
        .with_context(|| format!("tidak bisa menjalankan {program} (sudah terpasang di PATH?)"))?;
    if !out.status.success() {
        bail!("{program} gagal: {}", tail(&out.stderr));
    }
    Ok(out)
}

fn s(x: impl ToString) -> String {
    x.to_string()
}

/// Visual apa pun -> animated WebP 512x512 <= 500 KB (turunkan fps/kualitas bertahap).
async fn to_animated_webp(dir: &TempDir, visual: &Visual, max_secs: u32) -> Result<Vec<u8>> {
    if visual.is_webp_sticker && visual.bytes.len() <= MAX_ANIMATED_STICKER_BYTES {
        return Ok(visual.bytes.clone());
    }
    let input = dir.join("visual.in");
    let output = dir.join("sticker.webp");
    tokio::fs::write(&input, &visual.bytes).await?;

    for (fps, quality) in [(15u32, 60u32), (12, 45), (10, 35), (8, 25), (8, 15)] {
        let vf = format!(
            "fps={fps},scale=512:512:force_original_aspect_ratio=decrease:flags=lanczos,\
             format=rgba,pad=512:512:(ow-iw)/2:(oh-ih)/2:color=0x00000000"
        );
        let args = [
            "-hide_banner", "-loglevel", "error", "-y",
            "-t", &max_secs.to_string(),
            "-i", input.to_str().unwrap_or_default(),
            "-an", "-vf", &vf,
            "-c:v", "libwebp", "-lossless", "0",
            "-q:v", &quality.to_string(),
            "-compression_level", "6", "-loop", "0",
            output.to_str().unwrap_or_default(),
        ]
        .map(s);
        run("ffmpeg", &args).await?;
        let webp = tokio::fs::read(&output).await?;
        if webp.len() <= MAX_ANIMATED_STICKER_BYTES {
            return Ok(webp);
        }
        info!("webp {} KB > 500 KB, turunkan fps/kualitas", webp.len() / 1024);
    }
    bail!("tidak bisa mengecilkan sticker di bawah 500 KB; pakai video yang lebih pendek");
}

/// Audio apa pun -> Ogg Opus mono (format yang dipakai sticker beraudio asli).
async fn to_opus(dir: &TempDir, audio: &[u8], max_secs: u32) -> Result<(Vec<u8>, u32)> {
    let input = dir.join("audio.in");
    let output = dir.join("audio.ogg");
    tokio::fs::write(&input, audio).await?;
    let args = [
        "-hide_banner", "-loglevel", "error", "-y",
        "-t", &max_secs.to_string(),
        "-i", input.to_str().unwrap_or_default(),
        "-vn", "-c:a", "libopus", "-b:a", "32k", "-ar", "48000", "-ac", "1",
        "-f", "ogg",
        output.to_str().unwrap_or_default(),
    ]
    .map(s);
    run("ffmpeg", &args).await?;

    let probe = [
        "-v", "error", "-show_entries", "format=duration", "-of", "csv=p=0",
        output.to_str().unwrap_or_default(),
    ]
    .map(s);
    let secs = run("ffprobe", &probe)
        .await
        .ok()
        .and_then(|o| String::from_utf8_lossy(&o.stdout).trim().parse::<f64>().ok())
        .map_or(1, |d| d.ceil().max(1.0) as u32);

    Ok((tokio::fs::read(&output).await?, secs))
}

// ───────────────────────── upload & kirim ─────────────────────────

async fn merge_and_send(
    ctx: &MessageContext,
    visual: Visual,
    audio: Vec<u8>,
    cfg: &Config,
) -> Result<()> {
    let dir = TempDir::new()?;
    let webp = to_animated_webp(&dir, &visual, cfg.sticker_max_secs).await?;
    let (opus, seconds) = to_opus(&dir, &audio, cfg.audio_max_secs).await?;
    info!("sticker {} KB, audio {} KB ({}s)", webp.len() / 1024, opus.len() / 1024, seconds);

    let sticker_up = ctx
        .client
        .upload(webp, MediaType::Sticker, UploadOptions::default())
        .await
        .context("upload sticker")?;
    let audio_up = ctx
        .client
        .upload(opus, MediaType::Audio, UploadOptions::default())
        .await
        .context("upload audio")?;

    // AudioMessage di dalam StickerMessage. Sample asli tidak punya ptt/waveform/sidecar.
    let mut audio_msg = wa::message::AudioMessage::default();
    audio_msg.url = Some(audio_up.url);
    audio_msg.direct_path = Some(audio_up.direct_path);
    audio_msg.media_key = Some(audio_up.media_key.to_vec());
    audio_msg.file_sha256 = Some(audio_up.file_sha256.to_vec());
    audio_msg.file_enc_sha256 = Some(audio_up.file_enc_sha256.to_vec());
    audio_msg.file_length = Some(audio_up.file_length);
    audio_msg.media_key_timestamp = Some(audio_up.media_key_timestamp);
    audio_msg.mimetype = Some("audio/ogg; codecs=opus".to_string());
    audio_msg.seconds = Some(seconds);

    let now_ms = SystemTime::now()
        .duration_since(UNIX_EPOCH)
        .map_or(0, |d| d.as_millis() as i64);

    let mut sticker = wa::message::StickerMessage::default();
    sticker.url = Some(sticker_up.url);
    sticker.direct_path = Some(sticker_up.direct_path);
    sticker.media_key = Some(sticker_up.media_key.to_vec());
    sticker.file_sha256 = Some(sticker_up.file_sha256.to_vec());
    sticker.file_enc_sha256 = Some(sticker_up.file_enc_sha256.to_vec());
    sticker.file_length = Some(sticker_up.file_length);
    sticker.media_key_timestamp = Some(sticker_up.media_key_timestamp);
    sticker.mimetype = Some("image/webp".to_string());
    sticker.width = Some(512);
    sticker.height = Some(512);
    sticker.is_animated = Some(true);
    sticker.sticker_sent_ts = Some(now_ms);
    sticker.audio = Some(wa::message::sticker_message::Audio::AudioMessage(Box::new(audio_msg)));

    let mut inner = wa::Message::default();
    inner.sticker_message = MessageField::some(sticker);
    let mut wrapper = wa::message::FutureProofMessage::default();
    wrapper.message = MessageField::some(inner);
    let mut msg = wa::Message::default();
    msg.audio_sticker_message = MessageField::some(wrapper);

    let sent = ctx.send_message(msg).await.context("kirim audioStickerMessage")?;
    info!("sticker beraudio terkirim: {}", sent.message_id);
    Ok(())
}

// ───────────────────────── main ─────────────────────────

#[tokio::main]
async fn main() -> Result<(), Box<dyn std::error::Error>> {
    env_logger::Builder::from_env(env_logger::Env::default().default_filter_or("info")).init();

    let db = std::env::var("WA_DB").unwrap_or_else(|_| "whatsapp.db".into());
    let sessions: Sessions = Arc::default();
    let cfg = Arc::new(Config::from_env());

    let mut builder = Bot::builder()
        .with_backend(SqliteStore::open(&db).await?)
        .on_qr_code(|code, timeout| async move {
            println!("Scan QR (berlaku {} detik):\n{code}", timeout.as_secs());
        })
        .on_pair_code(|code, _| async move {
            println!("Pair code: {code}  (WhatsApp > Perangkat tertaut > Tautkan dengan nomor telepon)");
        })
        .on_connected(|_| async { info!("Terhubung.") })
        .on_message({
            let sessions = sessions.clone();
            let cfg = cfg.clone();
            move |ctx| {
                let sessions = sessions.clone();
                let cfg = cfg.clone();
                async move { handle(ctx, sessions, cfg).await }
            }
        });

    if let Ok(phone) = std::env::var("PAIR_PHONE") {
        builder = builder.with_pair_code(PairCodeOptions {
            phone_number: phone,
            custom_code: None,
            ..Default::default()
        });
    }

    builder.build().await?.run().await;
    Ok(())
}
