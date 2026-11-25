# WhatsApp Audio Sticker Bot (Go + whatsmeow)

Bot yang membuat **audio sticker** WhatsApp (sticker animasi + suara).

## Cara pakai

| Aksi | Hasil |
|---|---|
| Reply video **.mp4** dengan `.sa` | Langsung jadi audio sticker, suara diambil dari video itu |
| Reply video tanpa suara / GIF / gambar / sticker dengan `.sa`, lalu reply audio dengan `.sa` | Digabung jadi audio sticker (urutan bebas) |
| `.sa reset` | Hapus sesi yang sedang menunggu |

Aturan kecil:
- Kalau sudah ada audio yang menunggu di sesi, lalu kamu reply sebuah mp4, mp4 itu dipakai sebagai **gambar** dan audio dari sesi yang dipakai.
- Sesi per **chat + pengirim**, kedaluwarsa setelah `SESSION_SECS`.
- Durasi mengikuti audio, maksimal `AUDIO_MAX_SECS` (animasi maksimal `STICKER_MAX_SECS`).

## Batas video (.mp4)

- Tidak perlu resize ke 512x512 dulu. Bot memotong video ke `AUDIO_MAX_SECS` (5 detik) dan mengecilkannya sendiri,
  jadi video 1080x1920 berdurasi 15 detik diproses sama seperti yang kecil.
- Batas ukuran file input `MAX_INPUT_MB` (default 16 MB, sama dengan batas video biasa di WhatsApp). Ukuran file hanya
  memengaruhi waktu unduh, bukan waktu konversi.
- Kalau masih timeout di server yang lambat: naikkan `FFMPEG_TIMEOUT_SECS` / `JOB_TIMEOUT_SECS`, atau turunkan
  `STICKER_MAX_SECS`. Jalankan dengan `LOG_LEVEL=info` dan lihat baris `converted ... webp_ms=...` untuk waktu tiap tahap.

## Kebutuhan

- `ffmpeg` dan `ffprobe` (dengan libwebp dan libopus) di `PATH`.
- Tidak perlu CGO: SQLite memakai driver pure Go (`modernc.org/sqlite`).

## Build

```bash
go mod tidy        # sekali saja, membuat go.sum
make build         # binary untuk mesin ini
make dist          # cross compile: linux amd64/arm64/armv7, windows, macOS arm64
```

GitHub Actions (`.github/workflows/build.yml`) menjalankan `go mod tidy`, `go vet`, `go test`, lalu `make dist`
dan mengunggah semua binary sebagai artifact.

## Menjalankan

```bash
./audio-sticker-bot                 # login dengan QR di terminal
PAIR_PHONE=628123456789 ./audio-sticker-bot   # atau login dengan pairing code
```

Sesi login disimpan di `whatsapp.db`. Jangan dibagikan.

## Environment variable

Bisa diatur lewat file `.env` (salin dari `.env.example`) di folder tempat bot dijalankan, atau lewat shell.
Variabel dari shell menang atas `.env`. File lain bisa dipakai dengan `ENV_FILE=/path/ke/file.env`.
Bot membaca file ini sekali saat start, jadi restart setelah mengubahnya.


| Nama | Default | Arti |
|---|---|---|
| `WA_DB` | `whatsapp.db` | File database SQLite |
| `PAIR_PHONE` | - | Nomor untuk pairing code (tanpa `+`) |
| `PREFIX` | `.sa` | Kata perintah |
| `SESSION_SECS` | `120` | Umur sesi dua-reply |
| `STICKER_MAX_SECS` | `5` | Maks durasi animasi |
| `AUDIO_MAX_SECS` | `5` | Maks durasi audio |
| `STICKER_MAX_KB` | `500` | Batas ukuran WebP hasil. Lihat catatan di bawah sebelum menaikkan |
| `MAX_INPUT_MB` | `16` | Maks ukuran file input. Resolusi tidak dibatasi |
| `WORKERS` | jumlah CPU (1-16) | Proses ffmpeg yang jalan bersamaan |
| `MAX_JOBS` | `WORKERS*8` | Perintah yang diproses bersamaan (jalan + antre) |
| `USER_PER_MIN` | `6` | Maks proses berat per user per menit |
| `MAX_SESSIONS` | `2000` | Maks sesi menunggu secara global |
| `FFMPEG_TIMEOUT_SECS` | `90` | Timeout satu proses ffmpeg |
| `JOB_TIMEOUT_SECS` | `240` | Timeout satu job penuh |
| `ALLOWED_NUMBERS` | kosong | Daftar nomor dipisah koma; kosong = semua boleh. Akun bot sendiri selalu boleh |
| `LOG_LEVEL` | `info` | `debug`, `info`, `warn`, `error` |
| `DIAG` | - | `1` = kirim juga sticker biasa dan audio biasa sebagai pembanding |

## Batas 500 KB sticker

500 KB adalah aturan WhatsApp untuk sticker animasi, bukan batasan bot. Bot menurunkan fps dan kualitas bertahap
sampai hasilnya muat. Batas ini bisa dinaikkan dengan `STICKER_MAX_KB=2000`, tapi WhatsApp bisa menolak atau tidak
menampilkan sticker yang lebih besar di HP penerima. Uji dulu ke akun lain sebelum dipakai.

## Penanganan banyak user

- Setiap perintah jalan di goroutine sendiri, handler event whatsmeow tidak pernah diblokir.
- **Tiga batas**: `MAX_JOBS` (total perintah), `WORKERS` (proses ffmpeg), dan satu proses berat per user + `USER_PER_MIN`.
  Saat penuh, bot membalas "Busy" atau "Too many requests", bukan crash atau menumpuk.
- Sesi hanya menyimpan referensi media (kecil). File baru diunduh saat digabung, ke folder temp per-job yang selalu dihapus.
- Duplikat pesan diabaikan, pesan lama (> 2 menit, misalnya saat bot offline) diabaikan.
- Shutdown (`Ctrl+C`/SIGTERM) menunggu job yang berjalan sampai 30 detik.

## Patch whatsmeow (penting)

whatsmeow upstream belum mengenali wrapper `audioStickerMessage` (field 134), sehingga stanza terkirim tanpa
`mediatype` dan server membuangnya diam-diam. Salinan whatsmeow di `third_party/whatsmeow` sudah ditambal
(`patches/whatsmeow-audio-sticker.patch`), dan `go.mod` memakainya lewat `replace`.
Saat start, bot menjalankan **self-check**; log `self-check ok` berarti patch aktif.

Untuk update whatsmeow: ganti isi `third_party/whatsmeow` dengan versi baru, lalu terapkan ulang patch.
