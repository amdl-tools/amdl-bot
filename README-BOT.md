# Apple Music Downloader - Telegram Bot

A Telegram bot front-end for `apple-music-downloader`, written in Go.  
It uses the same download engine as the CLI tool directly (no subprocess),
so there is no extra overhead and all quality options are supported.

## Requirements

1. **wrapper-lite** running and reachable (same requirement as the CLI tool).
2. A Telegram Bot token from [@BotFather](https://t.me/BotFather).
3. Go 1.23+ if building from source, or a precompiled binary from the Releases page.

## Quick start (Docker, recommended)

```sh
# 1. Copy and edit the config
cp config-bot.yaml.example config-bot.yaml
# Set telegram.token, lite-server, and any paths you want

# 2. Start wrapper-lite and the bot together
docker compose -f docker-compose.bot.yml up -d
```

## Quick start (bare metal)

```sh
# Build
go build -o amdl-bot ./cmd/bot

# Copy and edit config
cp config-bot.yaml.example config-bot.yaml

# Run (wrapper-lite must already be running)
./amdl-bot --config config-bot.yaml
```

## Cross-platform builds

```sh
chmod +x build-bot.sh
./build-bot.sh 1.0.0
# Produces binaries in ./dist/ for Linux, macOS, Windows, Android
```

## Bot commands

| Command | Description |
|---|---|
| `/dl <url>` | Download ALAC (lossless) |
| `/atmos <url>` | Download Dolby Atmos |
| `/aac <url>` | Download AAC |
| `/mv <url>` | Download music video |
| `/search album\|song\|artist <query>` | Interactive search with inline keyboard |
| `/status` | Show your active downloads |
| `/cancel` | Cancel your active downloads |
| paste a link | Auto-downloads as ALAC |

## Configuration reference

See `config-bot.yaml.example`. The `telegram:` section is new; all other
sections (`general:`, `media:`, `paths:`, `metadata:`, `convert:`) are
identical to `config.yaml`.

### Key bot settings

| Key | Default | Description |
|---|---|---|
| `telegram.token` | (required) | Bot token from BotFather |
| `telegram.allowed-users` | `[]` (everyone) | Restrict access by Telegram user ID |
| `telegram.max-jobs-per-user` | `2` | Max simultaneous downloads per user |
| `telegram.workers` | `2` | Download worker goroutines |
| `telegram.max-file-size-mb` | `50` | Largest file sent via Telegram (50 MB limit for regular bots) |
| `telegram.send-as-document` | `true` | Send files to chat; set false to only save locally |
| `telegram.webhook-url` | `""` | Leave empty for long-polling (simpler); set to HTTPS URL for webhook |

## Architecture notes

The bot imports the `internal/app` package directly and calls the same
`Runner` used by the CLI. A thin public API (`internal/app/api.go`) exposes
`RunAlbum`, `RunPlaylist`, `RunStation`, `RunSong`, `RunMV`, and `Search`
without modifying any existing logic.

Because `Runner` currently writes to `os.Stdout`, each job temporarily
redirects stdout to a pipe to capture progress lines. A `sync.Mutex`
(`stdoutMu` in `internal/bot/runner.go`) prevents races when multiple
workers run concurrently. A future improvement is to add an `io.Writer`
field to `Runner` upstream and remove the pipe redirect.

Download results are read from `runner.State.AddedTracks`, which carries
the saved file paths, artist, album, and song names.

## Cache-first delivery

Every link is looked up in the Telegram file_id cache before anything is downloaded.

| Link | Lookup | Cost of a hit |
|---|---|---|
| song, music video, `album?i=` | track ID is read from the link | no Apple Music request at all |
| album, playlist | one metadata request lists the track IDs | only the missing tracks are downloaded |
| station | not looked up (track list is dynamic) | normal download |

Tracks found in the cache are sent first, so on a partly cached album they arrive
before the rest rather than in album order. The cache key is
`<track id>:<mode>`, so ALAC, Atmos and AAC are cached separately. A track is only
cached when it was uploaded with a valid thumbnail. Lookup is skipped when
`telegram.send-as-document` is false.

## Local Bot API server (uploads up to 2000 MB)

The public `api.telegram.org` accepts uploads up to 50 MB. A Bot API server you
run yourself with `--local` accepts up to 2000 MB, so a full lossless album no
longer has to be skipped. Set `telegram.api-endpoint` and the upload limit rises
to 2000 MB automatically (an explicit `max-file-size-mb` still wins).

1. Get an `api_id` and `api_hash` at https://my.telegram.org and put them in a
   `.env` file next to the compose file:
   ```
   TELEGRAM_API_ID=123456
   TELEGRAM_API_HASH=0123456789abcdef0123456789abcdef
   ```
2. Start the server (image `aiogram/telegram-bot-api`, run with `TELEGRAM_LOCAL=1`):
   ```sh
   docker compose -f docker-compose.bot.yml --profile local-api up -d telegram-bot-api
   ```
3. **Log the bot out of the public server first.** Telegram requires this when
   moving a bot to a local server, and a bot that was logged out cannot go back
   to the public server for a while, so only do it once you mean to switch:
   ```sh
   curl "https://api.telegram.org/bot<TOKEN>/logOut"
   ```
4. In `config-bot.yaml` set `telegram.api-endpoint: "http://telegram-bot-api:8081"`
   (or `http://127.0.0.1:8081` when the bot runs outside Docker) and restart the bot.

The bot refuses to start when `api-endpoint` is not an `http(s)://host[:port]` URL.
Cached `file_id`s were created on the public server. They are expected to keep
working on a local server, but if Telegram rejects one the bot evicts it and
uploads the file again, so nothing needs to be cleared by hand.

## Tests

```sh
go vet ./...
go test -race ./internal/bot/...
```

No test touches the real Telegram or Apple Music: a fake Telegram server
(`helpers_test.go`) records every request, so the tests can check what the bot
actually sends, for example that a cache hit sends only a `file_id` and uploads
nothing. Tests that need `ffmpeg` (thumbnails) are skipped when it is not
installed. `.github/workflows/ci.yml` runs vet and tests on Linux, Windows and
macOS, plus the race detector and `ffmpeg` on Linux.

## Automatic cleanup (`telegram.delete-mode`)

| Mode | Behaviour |
|---|---|
| `never` | keep every file (default) |
| `on_upload` | delete a file right after it was sent to Telegram |
| `disk_pressure` | when the disk is fuller than `disk-threshold-pct`, delete the oldest output files |

`disk_pressure` measures the **whole disk**, so if other data keeps the disk above
the threshold, deleting music can never bring it down. Two limits keep that from
wiping every download:

- `disk-min-age-min` (default 60): a file modified more recently than this is
  never removed, since it may still be downloading or waiting for its upload.
- `disk-max-delete-mb` (default 2048): the most one check may remove, counted
  across all output directories. Cleaning continues at the next check
  (`disk-check-interval-min`, default 5), so the worst case is bounded to roughly
  that amount per interval rather than everything at once.

When the disk is still above the threshold but nothing old enough remains, the
bot logs that once instead of at every check.

## Parallel downloads and /cancel

Every download runs in a process of its own: the bot starts itself again as
`amdl-bot -worker -config <file>`, sends the job on stdin, reads progress from
the output, and receives the result as a JSON file. This is needed because the
downloader prints from hundreds of places and keeps package-level state (the HTTP
client, for one), so two downloads cannot share a process, and because it has no
way to be interrupted from the inside.

What this gives you:

- `telegram.workers` is real parallelism. Mind the cost: each running download
  is a full process, and wrapper-lite sees that many concurrent requests.
- `/cancel` kills the download process together with everything it started
  (ffmpeg included): `SIGKILL` to the process group on Linux/macOS, `taskkill /T`
  on Windows. A job cancelled while still waiting never starts, and its message
  is updated to "Cancelled."
- Stopping the bot (Ctrl+C or SIGTERM) cancels every download first. A hard kill
  of the bot (`kill -9`, power loss) cannot do that and may leave a download
  process running until it finishes.
- Two jobs for the same album, playlist or song wait for each other, so they never
  write the same files. The second usually finds the tracks in the cache.

Limits worth knowing:

- A cancelled download can leave temporary files behind (`*.part`, `*.tmp-*`) in
  the output folders; a finished track is only written under its final name
  after it is complete. In `disk_pressure` mode such leftovers are removed once
  they are older than `disk-min-age-min`.
- A `/song/...` link and the `/album/...` link of the album it belongs to are not
  recognised as the same content, because the album is not known from the song
  link alone. Requesting both at the very same moment can make them write the
  same file.

## Files over the Telegram limit

A file larger than `telegram.max-file-size-mb` cannot be uploaded to Telegram.
Without a [local Bot API server](#local-bot-api-server-uploads-up-to-2000-mb) the
limit is 50 MB. With `telegram.large-file-host: "gofile"` the bot no longer drops
such files:

1. it collects every oversized track of the job (smaller tracks still go through
   Telegram as usual),
2. packs them into **one ZIP, stored without compression** (audio is already
   compressed, so compressing would only cost time),
3. uploads the ZIP to Gofile, with a progress message that updates every few seconds,
4. sends the user the download link.

The tracks are treated as delivered, and so deleted in `on_upload` mode, only after
the link exists. If zipping or uploading fails, or the job is cancelled, the files
stay on the server, the temporary ZIP is removed, and the user is told why.

Things to know:

- **Anyone with the link can download the ZIP.** Do not enable this if that is not
  acceptable. Gofile applies its own retention rules to uploaded files; check them
  on gofile.io.
- The ZIP is built in the system temporary directory, which needs free space equal
  to the size of the oversized files.
- **Guest uploads.** The bot uploads as a guest unless `telegram.gofile-token` is
  set. The official Gofile page says API requests need a token, while its API
  description as mirrored by third parties, and several client libraries, say
  uploads work without one. If Gofile rejects guest uploads the user gets an error
  that names the setting; put a token from your Gofile profile in
  `telegram.gofile-token` and it is used instead. A guest token is kept in
  `gofile-token-file` and reused; if Gofile later rejects it, the bot drops it and
  retries once as a new guest.
- **transfer.it is not supported.** It is a front-end to MEGA: each upload creates
  an anonymous MEGA account and sends the file encrypted in chunks over WebSockets,
  and there is no documented upload API, only unofficial reverse-engineered
  clients. Reimplementing that inside the bot would be fragile.
- Hosts sit behind the `Uploader` interface in `largefiles.go`; adding another one
  with a plain HTTP upload API means one new file next to `gofile.go`.

## State files (`fileid-cache-file`, `stats-file`, `gofile-token-file`)

These small JSON files are what the bot remembers between runs. The file_id cache
is the one that matters: losing it means every track is downloaded and uploaded
again.

- **Atomic writes.** A file is written to a temporary file next to it, flushed to
  disk, and moved over the real one with a rename. If the bot or the machine dies
  during a save, the previous file is still there, complete. Temporary files left
  by such a crash are removed at the next start.
- **Coalesced writes.** A change is saved about two seconds after it happens, and
  every change made in the meantime goes into the same write. Marking 200 deleted
  files, or caching a whole album, is one write, not hundreds. The price: a hard
  kill (`kill -9`, power loss) can lose the last couple of seconds of changes. A
  normal stop (Ctrl+C, SIGTERM) saves everything first.
- **An unreadable file is never overwritten.** If the cache or the statistics
  cannot be parsed, the bot renames the file to `<name>.corrupt-<date-time>`, logs
  where it went, and starts empty. Fix the JSON by hand and rename it back to
  recover its contents.
- Each save still rewrites the whole file. That is fine for thousands of entries.
  If the cache grows to hundreds of thousands of entries (each one is roughly a
  hundred bytes of JSON, so several MB per save), that is the point to move it to
  SQLite, which updates single rows.

## Daily limits per user

Set `telegram.daily-track-limit` and/or `telegram.daily-mb-limit` (0 = no limit,
the default) to cap what each user can download per day. This is what keeps a
public bot, one with an empty `allowed-users`, from filling the disk or keeping
every worker busy.

How it behaves:

- **Only real downloads count.** Tracks served from the file_id cache cost the
  server nothing, so they are free, and a user who has used up the day's
  allowance still receives them. Tracks counted are those a download delivered,
  including a file that already existed on the server.
- **Reserve, then settle.** Before a download starts the bot reserves the tracks
  it expects to fetch; when it ends the user is charged for what really came down,
  even if the job then failed. A cancelled job costs nothing. Because of the
  reservation, two jobs of one user running at once cannot both slip through when
  only one fits.
- **Whole requests, not partial ones.** If an album needs 14 tracks and 6 are
  left, the request is refused with that explanation rather than started and cut
  off. The refusal also says when the limit resets.
- **Admins** (`admin-ids`) are exempt.
- **Usage survives restarts** (`quota-file`), so restarting the bot does not give
  everyone a fresh allowance. Entries from earlier days are dropped.

Commands: `/quota` shows your own usage; an admin can add a user ID
(`/quota 123456`) to see someone else's, and `/resetquota 123456` clears a user's
usage for today.

Limits of the design:

- The **data limit is soft**. The size of a download is not known beforehand, so a
  job starts whenever some of the allowance is left and may go over it. The next job
  is then refused.
- The **number of tracks is known in advance only when the link can be looked up**
  (songs, albums, playlists, with `send-as-document: true`). For a station, or with
  that option off, one track is reserved and the real count is charged afterwards,
  so a single job can exceed the limit.
- The **day is the server's local calendar day.** In Docker that is UTC unless you
  set `TZ`.
- A refused job is refused when it reaches the front of the queue, not when it is
  sent, because a cache hit has to be allowed even from a user who is over the
  limit. This is also not a substitute for `max-jobs-per-user`, which limits how
  many jobs run at once.
- Nothing stops one person from using several Telegram accounts. Use
  `allowed-users` if that matters.

## The progress message

While a download runs, the message shows (plain text, no symbols beyond the bar):

```
Downloading [alac]
Linh Cao - Man Coi - Single
[########------------] 40%
Track 8 of 14: Song Name
Decrypting 63% at 3.2 MB/s
2m 05s elapsed, about 3m left
5 track(s) already sent from cache
```

- **The bar is overall progress**, counted in tracks that have to be downloaded.
  Tracks already sent from the cache are not part of it, so a mostly cached album
  starts near the end instead of at zero. The track in progress counts by its own
  percentage, so the bar moves during a track and not only between tracks.
- **The line under the track name** comes from the downloader's own progress bar
  (percentage and transfer rate). The title and track names come from the metadata
  lookup made for the cache, so they appear for albums and playlists, not for a
  single song link.
- **The estimate** appears only after at least 10 seconds and 3% of the work, and
  never once the work is finished; before that it would be guesswork.
- The message is redrawn about every three seconds and only when its text changed.
  Drawing happens on its own goroutine: if Telegram is slow, the download is not.
  The final message says how long the download took.

### Upload progress

When the download is done, the same message goes on to show the upload to
Telegram, and ends with the summary:

```
Uploading to Telegram [alac]
[#######-------------] 35%
File 3 of 9: 03. Song Name.m4a
12.4 MB of 27.3 MB at 2.1 MB/s
1m 10s elapsed, about 2m left
```

and finally `Download complete in 2m 31s, upload in 3m 20s:` followed by the list of
tracks. The user follows one message from the cache check to the end.

- **The bar counts bytes** of the files that really go to Telegram. Tracks sent from
  the cache (no upload) and files over the size limit (zipped and sent to a file
  host, which has its own message) are not part of it.
- **The count is of bytes handed to the network.** Telegram's client library has no
  progress callback, so each file is passed through a reader that counts what it
  hands over. That runs a little ahead of what Telegram has actually received, so
  when a file's last byte has been handed over the message says it is *waiting for
  Telegram* rather than claiming the file has arrived. The rate shown is that of
  the current file; the estimate of the time left uses the pace since the first
  byte went out, so it includes the pauses between files.
- **`/cancel` now stops an upload that is in progress.** The same reader ends the
  transfer as soon as the job is cancelled, instead of letting the current file
  finish first.
- Small thumbnails sent along with a track are not counted.

How the percentage is read: the downloader draws a progress bar for each track
(the `schollz/progressbar` library), redrawing one line with carriage returns.
The bot looks for a percentage and a rate such as `3.2 MB/s` in each redrawn
frame and ignores the rest, rather than relying on an exact layout. If a frame
shows neither, the bar still advances track by track.
