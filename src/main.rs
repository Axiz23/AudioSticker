//! WhatsApp sound sticker bot: reply to a video/GIF and an audio with `.sa`.
//! Needs `ffmpeg` and `ffprobe` in PATH.

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

const MAX_STICKER_BYTES: usize = 500 * 1024;
const MAX_INPUT_BYTES: usize = 25 * 1024 * 1024;
const FFMPEG_TIMEOUT: Duration = Duration::from_secs(90);
const USAGE: &str = "Reply to a video/GIF/WebM/sticker and an audio with .sa";

struct Config {
    session_ttl: Duration,
    sticker_secs: u32,
    audio_secs: u32,
    diag: bool,
}

impl Config {
    fn from_env() -> Self {
        let num = |k: &str, d: u32| std::env::var(k).ok().and_then(|v| v.parse().ok()).unwrap_or(d);
        Self {
            session_ttl: Duration::from_secs(u64::from(num("SESSION_SECS", 120))),
            sticker_secs: num("STICKER_MAX_SECS", 5),
            audio_secs: num("AUDIO_MAX_SECS", 5),
            diag: std::env::var("DIAG").is_ok_and(|v| v == "1"),
        }
    }
}

struct Visual {
    bytes: Vec<u8>,
    is_webp: bool,
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

fn parse_command(text: &str) -> Option<&str> {
    let rest = text.trim().strip_prefix(".sa")?;
    (rest.is_empty() || rest.starts_with(char::is_whitespace)).then(|| rest.trim())
}

async fn say(ctx: &MessageContext, text: &str) {
    if let Err(e) = ctx.reply_quoting(text).await {
        warn!("reply failed: {e}");
    }
}

async fn handle(ctx: MessageContext, sessions: Sessions, cfg: Arc<Config>) {
    let Some(arg) = ctx.message.text_content().and_then(parse_command) else {
        return;
    };
    let key = format!("{}/{}", ctx.info.source.chat, ctx.info.source.sender);

    if arg == "reset" {
        sessions.lock().unwrap_or_else(|p| p.into_inner()).remove(&key);
        say(&ctx, "Session cleared").await;
        return;
    }

    let Some(quoted) = ctx.message.context_info().and_then(|c| c.quoted_message.as_option()) else {
        say(&ctx, USAGE).await;
        return;
    };

    let slot = match fetch_media(&ctx.client, quoted).await {
        Ok(Some(slot)) => slot,
        Ok(None) => return say(&ctx, USAGE).await,
        Err(e) => {
            error!("download failed: {e:#}");
            return say(&ctx, "Could not download that media").await;
        }
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
                _ => return,
            },
            (true, false) => Outcome::Waiting("audio"),
            _ => Outcome::Waiting("video/GIF"),
        }
    };

    match outcome {
        Outcome::Waiting(missing) => say(&ctx, &format!("Now reply the {missing} with .sa")).await,
        Outcome::Ready(visual, audio) => {
            if let Err(e) = merge_and_send(&ctx, visual, audio, &cfg).await {
                error!("send failed: {e:#}");
                say(&ctx, &format!("Failed: {e}")).await;
            }
        }
    }
}

async fn fetch_media(client: &Client, quoted: &wa::Message) -> Result<Option<Slot>> {
    let base = quoted.get_base_message();
    let limits = DownloadLimits::default();

    let (is_visual, is_webp, bytes) = if let Some(v) = base.video_message.as_option() {
        (true, false, client.download(v, limits).await)
    } else if let Some(i) = base.image_message.as_option() {
        (true, false, client.download(i, limits).await)
    } else if let Some(s) = base.sticker_message.as_option() {
        (true, true, client.download(s, limits).await)
    } else if let Some(a) = base.audio_message.as_option() {
        (false, false, client.download(a, limits).await)
    } else if let Some(d) = base.document_message.as_option() {
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
        bail!("file too large");
    }
    Ok(Some(if is_visual {
        Slot::Visual(Visual { bytes, is_webp })
    } else {
        Slot::Audio(bytes)
    }))
}

static COUNTER: AtomicU64 = AtomicU64::new(0);

struct TempDir(PathBuf);

impl TempDir {
    fn new() -> Result<Self> {
        let n = COUNTER.fetch_add(1, Ordering::Relaxed);
        let p = std::env::temp_dir().join(format!("wa-sa-{}-{n}", std::process::id()));
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

fn s(x: impl ToString) -> String {
    x.to_string()
}

async fn run(program: &str, args: &[String]) -> Result<std::process::Output> {
    let fut = Command::new(program).args(args).kill_on_drop(true).output();
    let out = tokio::time::timeout(FFMPEG_TIMEOUT, fut)
        .await
        .map_err(|_| anyhow!("{program} timed out"))?
        .with_context(|| format!("cannot run {program}"))?;
    if !out.status.success() {
        let err = String::from_utf8_lossy(&out.stderr);
        let n = err.chars().count();
        bail!("{program} failed: {}", err.chars().skip(n.saturating_sub(300)).collect::<String>());
    }
    Ok(out)
}

async fn to_animated_webp(dir: &TempDir, visual: &Visual, max_secs: u32) -> Result<Vec<u8>> {
    if visual.is_webp && visual.bytes.len() <= MAX_STICKER_BYTES {
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
            "-c:v", "libwebp", "-lossless", "0", "-q:v", &quality.to_string(),
            "-compression_level", "6", "-loop", "0",
            output.to_str().unwrap_or_default(),
        ]
        .map(s);
        run("ffmpeg", &args).await?;
        let webp = tokio::fs::read(&output).await?;
        if webp.len() <= MAX_STICKER_BYTES {
            return Ok(webp);
        }
    }
    bail!("sticker is over 500 KB, use a shorter video")
}

async fn to_opus(dir: &TempDir, audio: &[u8], max_secs: u32) -> Result<(Vec<u8>, u32)> {
    let input = dir.join("audio.in");
    let output = dir.join("audio.ogg");
    tokio::fs::write(&input, audio).await?;
    let args = [
        "-hide_banner", "-loglevel", "error", "-y",
        "-t", &max_secs.to_string(),
        "-i", input.to_str().unwrap_or_default(),
        "-vn", "-c:a", "libopus", "-b:a", "32k", "-ar", "48000", "-ac", "1", "-f", "ogg",
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

async fn merge_and_send(ctx: &MessageContext, visual: Visual, audio: Vec<u8>, cfg: &Config) -> Result<()> {
    let dir = TempDir::new()?;
    let webp = to_animated_webp(&dir, &visual, cfg.sticker_secs).await?;
    let (opus, seconds) = to_opus(&dir, &audio, cfg.audio_secs).await?;
    let sizes = (webp.len() / 1024, opus.len() / 1024);

    let sticker_up = ctx
        .client
        .upload(webp, MediaType::Sticker, UploadOptions::default())
        .await
        .context("sticker upload")?;
    let audio_up = ctx
        .client
        .upload(opus, MediaType::Audio, UploadOptions::default())
        .await
        .context("audio upload")?;

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

    if cfg.diag {
        let mut plain_sticker = wa::Message::default();
        plain_sticker.sticker_message = MessageField::some(sticker.clone());
        let mut plain_audio = wa::Message::default();
        plain_audio.audio_message = MessageField::some(audio_msg.clone());
        for (name, msg) in [("plain sticker", plain_sticker), ("plain audio", plain_audio)] {
            match ctx.send_message(msg).await {
                Ok(_) => info!("diag: {name} sent"),
                Err(e) => error!("diag: {name} failed: {e}"),
            }
        }
    }

    sticker.audio = Some(wa::message::sticker_message::Audio::AudioMessage(Box::new(audio_msg)));

    let mut inner = wa::Message::default();
    inner.sticker_message = MessageField::some(sticker);
    let mut wrapper = wa::message::FutureProofMessage::default();
    wrapper.message = MessageField::some(inner);
    let mut msg = wa::Message::default();
    msg.audio_sticker_message = MessageField::some(wrapper);

    ctx.send_message(msg).await.context("send")?;
    info!("audio sticker sent (webp {} KB, opus {} KB, {seconds}s)", sizes.0, sizes.1);
    Ok(())
}

fn self_check() {
    let mut inner = wa::Message::default();
    inner.sticker_message = MessageField::some(Default::default());
    let mut wrapper = wa::message::FutureProofMessage::default();
    wrapper.message = MessageField::some(inner);
    let mut msg = wa::Message::default();
    msg.audio_sticker_message = MessageField::some(wrapper);

    let media = whatsapp_rust::wacore::send::media_type_from_message(&msg);
    let kind = whatsapp_rust::wacore::send::stanza_type_from_message(&msg);
    if media == Some("sticker") && kind == "media" {
        info!("self-check ok");
    } else {
        error!("self-check failed (type={kind}, mediatype={media:?}): outdated whatsapp-rust, run `cargo update -p whatsapp-rust`");
    }
}

#[tokio::main]
async fn main() -> Result<(), Box<dyn std::error::Error>> {
    env_logger::Builder::from_env(env_logger::Env::default().default_filter_or("info")).init();
    self_check();

    let db = std::env::var("WA_DB").unwrap_or_else(|_| "whatsapp.db".into());
    let sessions: Sessions = Arc::default();
    let cfg = Arc::new(Config::from_env());

    let mut builder = Bot::builder()
        .with_backend(SqliteStore::open(&db).await?)
        .on_qr_code(|code, _| async move { println!("{code}") })
        .on_pair_code(|code, _| async move { println!("Pair code: {code}") })
        .on_connected(|_| async { info!("connected") })
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
