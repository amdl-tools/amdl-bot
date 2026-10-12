package bot

import (
	"amdl/internal/app"
	"context"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
)

const helpText = `Available commands:

/dl <url>      - Download ALAC (lossless)
/atmos <url>   - Download Dolby Atmos
/aac <url>     - Download AAC
/mv <url>      - Download music video
/search album|song|artist <query>
/status        - Show active downloads
/cancel        - Cancel your downloads
/quota         - Show your daily download limit
/stats         - Show statistics (admin only)
/resetquota <user id> - Reset a user's daily limit (admin only)

You can also paste an Apple Music link directly to download it as ALAC.

Supported link types: album, song, playlist, station, music-video`

func (b *Bot) cmdStart(msg *tgbotapi.Message) {
	name := msg.From.FirstName
	if name == "" {
		name = "there"
	}
	b.reply(msg, fmt.Sprintf("Hello, %s. Send an Apple Music link or use /help.", name))
}

func (b *Bot) cmdHelp(msg *tgbotapi.Message) {
	b.reply(msg, helpText)
}

// cmdDownload handles /dl, /atmos, /aac, /mv commands.
// The URL is expected as the command argument.
func (b *Bot) cmdDownload(msg *tgbotapi.Message, mode Mode) {
	rawURL := strings.TrimSpace(msg.CommandArguments())
	if rawURL == "" {
		b.reply(msg, fmt.Sprintf("Usage: /%s <apple music url>", msg.Command()))
		return
	}
	if DetectURLType(rawURL) == "" {
		b.reply(msg, "That does not look like a valid Apple Music link.")
		return
	}
	b.enqueueURL(msg, rawURL, mode)
}

// enqueueURL creates a job, sends an acknowledgment message (so we have a
// message ID to edit with progress), and adds the job to the queue.
func (b *Bot) enqueueURL(msg *tgbotapi.Message, rawURL string, mode Mode) {
	ackMsg, err := b.send(msg.Chat.ID, fmt.Sprintf("Queued [%s]: %s", mode, rawURL))
	if err != nil {
		return
	}

	job := newJob(msg.From.ID, msg.Chat.ID, ackMsg.MessageID, rawURL, mode, msg.From.UserName)
	if err := b.queue.Enqueue(job); err != nil {
		b.edit(msg.Chat.ID, ackMsg.MessageID, "Error: "+err.Error())
	}
}

// cmdSearch handles /search album|song|artist <query>.
func (b *Bot) cmdSearch(msg *tgbotapi.Message) {
	args := strings.Fields(msg.CommandArguments())
	if len(args) < 2 {
		b.reply(msg, "Usage: /search album|song|artist <query>")
		return
	}
	searchType := strings.ToLower(args[0])
	if searchType != "album" && searchType != "song" && searchType != "artist" {
		b.reply(msg, "Search type must be album, song, or artist.")
		return
	}
	query := strings.Join(args[1:], " ")

	b.reply(msg, fmt.Sprintf("Searching for %ss: %s ...", searchType, query))

	runner := app.NewRunner(b.appCfg)
	items, hasMore, err := runner.Search(searchType, query, "", 10, 0)
	if err != nil {
		b.reply(msg, "Search failed: "+err.Error())
		return
	}
	if len(items) == 0 {
		b.reply(msg, "No results found.")
		return
	}

	b.sessions.SetSearch(msg.From.ID, &SearchSession{
		SearchType: searchType,
		Query:      query,
		Items:      items,
		Offset:     0,
		HasMore:    hasMore,
	})

	b.sendSearchResults(msg.Chat.ID, msg.From.ID, items, hasMore, 0)
}

// sendSearchResults sends an inline keyboard with search results.
func (b *Bot) sendSearchResults(chatID, userID int64, items []app.SearchResultItem, hasMore bool, offset int) {
	var rows [][]tgbotapi.InlineKeyboardButton

	for i, item := range items {
		label := fmt.Sprintf("%d. %s - %s", i+1, item.Name, item.Detail)
		// Telegram callback data max is 64 bytes; store index, not full URL
		data := fmt.Sprintf("pick:%d", i)
		rows = append(rows, tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData(label, data),
		))
	}

	// Navigation row
	var navRow []tgbotapi.InlineKeyboardButton
	if offset > 0 {
		navRow = append(navRow, tgbotapi.NewInlineKeyboardButtonData("Previous", "page:prev"))
	}
	if hasMore {
		navRow = append(navRow, tgbotapi.NewInlineKeyboardButtonData("Next", "page:next"))
	}
	if len(navRow) > 0 {
		rows = append(rows, navRow)
	}

	keyboard := tgbotapi.NewInlineKeyboardMarkup(rows...)
	out := tgbotapi.NewMessage(chatID, "Select a result:")
	out.ReplyMarkup = keyboard
	_, _ = b.api.Send(out)
}

// handleCallback processes inline keyboard button presses.
func (b *Bot) handleCallback(cb *tgbotapi.CallbackQuery) {
	// Acknowledge immediately so the spinner clears in Telegram UI.
	ack := tgbotapi.NewCallback(cb.ID, "")
	_, _ = b.api.Request(ack)

	userID := cb.From.ID
	chatID := cb.Message.Chat.ID

	data := cb.Data

	switch {
	case strings.HasPrefix(data, "pick:"):
		b.handleSearchPick(userID, chatID, cb.Message.MessageID, data)

	case data == "page:next" || data == "page:prev":
		b.handleSearchPage(userID, chatID, data)

	case strings.HasPrefix(data, "mode:"):
		// Format: mode:<alac|atmos|aac>:<url>
		// The URL can be long - we stored it in the session
		b.handleModeSelect(userID, chatID, data, cb.From.UserName)

	default:
		_, _ = b.api.Send(tgbotapi.NewMessage(chatID, "Unknown action."))
	}
}

func (b *Bot) handleSearchPick(userID int64, chatID int64, msgID int, data string) {
	sess := b.sessions.GetSearch(userID)
	if sess == nil {
		_, _ = b.api.Send(tgbotapi.NewMessage(chatID, "Session expired. Run /search again."))
		return
	}

	var idx int
	fmt.Sscanf(strings.TrimPrefix(data, "pick:"), "%d", &idx)
	if idx < 0 || idx >= len(sess.Items) {
		_, _ = b.api.Send(tgbotapi.NewMessage(chatID, "Invalid selection."))
		return
	}
	item := sess.Items[idx]

	// Store the chosen URL in session so mode selection can retrieve it.
	sess.Query = "__selected__" + item.URL
	b.sessions.SetSearch(userID, sess)

	// Ask for quality/mode
	rows := [][]tgbotapi.InlineKeyboardButton{
		{
			tgbotapi.NewInlineKeyboardButtonData("ALAC (lossless)", "mode:alac"),
			tgbotapi.NewInlineKeyboardButtonData("Dolby Atmos", "mode:atmos"),
		},
		{
			tgbotapi.NewInlineKeyboardButtonData("AAC", "mode:aac"),
		},
	}
	if item.Type == "Album" || item.Type == "Song" {
		// MV not applicable here
	}
	keyboard := tgbotapi.NewInlineKeyboardMarkup(rows...)
	out := tgbotapi.NewMessage(chatID, fmt.Sprintf("Selected: %s\nChoose quality:", item.Name))
	out.ReplyMarkup = keyboard
	_, _ = b.api.Send(out)
}

func (b *Bot) handleModeSelect(userID int64, chatID int64, data string, username string) {
	sess := b.sessions.GetSearch(userID)
	if sess == nil {
		_, _ = b.api.Send(tgbotapi.NewMessage(chatID, "Session expired. Run /search again."))
		return
	}
	if !strings.HasPrefix(sess.Query, "__selected__") {
		_, _ = b.api.Send(tgbotapi.NewMessage(chatID, "No item selected. Run /search again."))
		return
	}
	selectedURL := strings.TrimPrefix(sess.Query, "__selected__")
	b.sessions.ClearSearch(userID)

	modeStr := strings.TrimPrefix(data, "mode:")
	var mode Mode
	switch modeStr {
	case "atmos":
		mode = ModeAtmos
	case "aac":
		mode = ModeAAC
	default:
		mode = ModeALAC
	}

	ackMsg, err := b.api.Send(tgbotapi.NewMessage(chatID,
		fmt.Sprintf("Queued [%s]: %s", mode, selectedURL)))
	if err != nil {
		return
	}

	// Build a fake tgbotapi.Message to reuse enqueue logic
	job := newJob(userID, chatID, ackMsg.MessageID, selectedURL, mode, username)
	if err := b.queue.Enqueue(job); err != nil {
		b.edit(chatID, ackMsg.MessageID, "Error: "+err.Error())
	}
}

func (b *Bot) handleSearchPage(userID int64, chatID int64, data string) {
	sess := b.sessions.GetSearch(userID)
	if sess == nil {
		_, _ = b.api.Send(tgbotapi.NewMessage(chatID, "Session expired. Run /search again."))
		return
	}

	pageSize := 10
	if data == "page:next" {
		sess.Offset += pageSize
	} else if sess.Offset >= pageSize {
		sess.Offset -= pageSize
	}

	runner := app.NewRunner(b.appCfg)
	items, hasMore, err := runner.Search(sess.SearchType, sess.Query, "", pageSize, sess.Offset)
	if err != nil {
		_, _ = b.api.Send(tgbotapi.NewMessage(chatID, "Search failed: "+err.Error()))
		return
	}
	sess.Items = items
	sess.HasMore = hasMore
	b.sessions.SetSearch(userID, sess)
	b.sendSearchResults(chatID, userID, items, hasMore, sess.Offset)
}

func (b *Bot) cmdStatus(msg *tgbotapi.Message) {
	jobs := b.queue.ActiveJobs(msg.From.ID)
	if len(jobs) == 0 {
		b.reply(msg, "No active downloads.")
		return
	}
	var lines []string
	for _, j := range jobs {
		lines = append(lines, fmt.Sprintf("[%s] %s - %s", j.Status, j.Mode, j.URL))
	}
	b.reply(msg, strings.Join(lines, "\n"))
}

func (b *Bot) isAdmin(userID int64) bool {
	for _, id := range b.cfg.AdminIDs {
		if id == userID {
			return true
		}
	}
	return false
}

func (b *Bot) cmdStats(msg *tgbotapi.Message) {
	if !b.isAdmin(msg.From.ID) {
		b.reply(msg, "This command is available to admins only.")
		return
	}
	report := b.stats.Summary()
	report += fmt.Sprintf("\nFile ID cache: %d entries", b.fileIDCache.Len())
	report += "\n" + b.monitor.StatusText()
	b.reply(msg, report)
}

// cmdQuota shows a user's daily allowance. Anyone can look at their own; an
// admin can add a user ID to look at someone else's.
func (b *Bot) cmdQuota(msg *tgbotapi.Message) {
	target := msg.From.ID
	arg := strings.TrimSpace(msg.CommandArguments())
	if arg != "" {
		if !b.isAdmin(msg.From.ID) {
			b.reply(msg, "Only admins can look at another user's quota.")
			return
		}
		id, err := strconv.ParseInt(arg, 10, 64)
		if err != nil {
			b.reply(msg, "Usage: /quota [user id]")
			return
		}
		target = id
	}

	report := b.quota.Report(target)
	if target != msg.From.ID {
		report = fmt.Sprintf("User %d\n%s", target, report)
	}
	b.reply(msg, report)
}

// cmdResetQuota clears a user's usage for today (admin only).
func (b *Bot) cmdResetQuota(msg *tgbotapi.Message) {
	if !b.isAdmin(msg.From.ID) {
		b.reply(msg, "This command is available to admins only.")
		return
	}
	id, err := strconv.ParseInt(strings.TrimSpace(msg.CommandArguments()), 10, 64)
	if err != nil {
		b.reply(msg, "Usage: /resetquota <user id>")
		return
	}
	if b.quota == nil {
		b.reply(msg, "No download limits are configured.")
		return
	}
	b.quota.Reset(id)
	b.reply(msg, fmt.Sprintf("Quota of user %d has been reset for today.", id))
}

func (b *Bot) cmdCancel(msg *tgbotapi.Message) {
	n := b.queue.Cancel(msg.From.ID)
	if n == 0 {
		b.reply(msg, "No active downloads to cancel.")
	} else {
		b.reply(msg, fmt.Sprintf("Cancelled %d download(s).", n))
	}
}

// processJob is the worker function called by the Queue for each job.
//
// Order of work:
//  1. look the link up in the file_id cache and send every cached track
//  2. download only what is still missing
//  3. upload the downloaded tracks (and cache their file_ids)
func (b *Bot) processJob(job *Job) {
	prog := NewProgress(job.Mode, func(text string) {
		b.edit(job.ChatID, job.MessageID, text)
	})

	// Downloads run in parallel, so two jobs for the same album must not write it
	// at once. The second waits here and then usually finds the tracks cached.
	unlock, err := b.locks.Lock(job.ctx, jobKey(job.URL), func() {
		b.edit(job.ChatID, job.MessageID, "Waiting for another download of the same content...")
	})
	if err != nil {
		prog.Done("Cancelled.")
		return
	}
	defer unlock()

	// Step 1: cache lookup. Only meaningful when files are actually sent to
	// Telegram; with send-as-document disabled the user wants the files on disk.
	var plan cachePlan
	if b.cfg.SendAsDocument {
		b.edit(job.ChatID, job.MessageID, fmt.Sprintf("Checking cache [%s]...", job.Mode))
		plan = b.serveFromCache(job, func(text string) {
			b.edit(job.ChatID, job.MessageID, text)
		})
		b.stats.RecordCacheHits(job.UserID, job.username, plan.Delivered)
	}

	if job.ctx.Err() != nil {
		prog.Done("Cancelled.")
		return
	}

	if plan.fullyServed() {
		job.Status = JobDone
		prog.Done(fmt.Sprintf("Sent %d track(s) from cache, nothing was downloaded:\n\n%s",
			plan.Delivered, listNames(plan.Names)))
		return
	}

	// Daily quota: reserve what this download is expected to fetch. Tracks that
	// came from the cache were free and are already sent. When the link could not
	// be looked up in advance (a station, or the cache is off) one track is
	// reserved and the real count is charged afterwards.
	expected := 1
	if plan.Total > 0 {
		expected = plan.Total - plan.Delivered
	}
	reservation, quotaErr := b.quota.Reserve(job.UserID, expected)
	if quotaErr != nil {
		job.Status = JobFailed
		job.ErrMsg = quotaErr.Error()
		msg := quotaErr.Error()
		if plan.Delivered > 0 {
			msg = fmt.Sprintf("%d track(s) were sent from cache. The other %d need a download. %s",
				plan.Delivered, plan.Total-plan.Delivered, msg)
		}
		prog.Done(msg)
		return
	}
	defer reservation.Release() // a no-op once the reservation has been settled

	// Step 2: download whatever the cache could not provide. The progress message
	// takes over from here: it knows how many tracks there are, which ones came
	// from the cache, and redraws itself while the download runs.
	prog.SetPlan(plan)
	prog.Start()

	// The download runs in a process of its own: cancelling the job kills it.
	result := runDownloadProcess(job.ctx, b.cfgPath, workerRequest{
		URL:        job.URL,
		Mode:       job.Mode,
		Token:      plan.Token,
		SkipTracks: plan.Skip,
	}, prog.Feed)

	if job.ctx.Err() != nil {
		prog.Done("Cancelled.")
		return
	}

	// What this download fetched, whether or not it finished: the quota is charged
	// for the tracks that really came down.
	var downloadedBytes int64
	for _, t := range result.AddedTracks {
		if info, err := os.Stat(t.Path); err == nil {
			downloadedBytes += info.Size()
		}
	}
	reservation.Commit(len(result.AddedTracks), downloadedBytes)

	if result.Err != nil {
		job.Status = JobFailed
		job.ErrMsg = result.Err.Error()
		b.stats.RecordFailure(job.UserID, job.username)
		prefix := ""
		if plan.Delivered > 0 {
			prefix = fmt.Sprintf("%d track(s) were sent from cache.\n", plan.Delivered)
		}
		prog.Done(fmt.Sprintf("%sDownload failed: %s\n\n%s", prefix, result.Err.Error(), lastLines(result.Log, 5)))
		return
	}

	if len(result.AddedTracks) == 0 {
		msg := "Download completed but no tracks were saved."
		if plan.Delivered > 0 {
			msg = fmt.Sprintf("%d track(s) sent from cache. The remaining tracks could not be downloaded.", plan.Delivered)
		}
		prog.Done(msg)
		return
	}

	b.stats.RecordSuccess(job.UserID, job.username, len(result.AddedTracks), downloadedBytes)

	job.Status = JobDone
	summary := buildSummary(result.AddedTracks)
	if plan.Delivered > 0 {
		summary = fmt.Sprintf("(%d more track(s) were sent from cache)\n", plan.Delivered) + summary
	}
	downloadTime := prog.Elapsed()

	// With send-as-document off the files stay on the server: the job ends here.
	if !b.cfg.SendAsDocument {
		prog.Done(fmt.Sprintf("Download complete in %s:\n\n%s", formatElapsed(downloadTime), summary))
		return
	}

	// Step 3: upload the freshly downloaded tracks. The job's message goes on to
	// show the upload and ends with the same summary the download would have ended
	// with, so the user follows one message from start to finish.
	prog.Stop()
	up := NewUploadProgress(job.Mode, func(text string) {
		b.edit(job.ChatID, job.MessageID, text)
	})
	up.SetFinal(summary, downloadTime)
	b.sendTracks(job, result.AddedTracks, up)
}

// listNames formats up to 10 track names for a status message.
func listNames(names []string) string {
	var lines []string
	for i, n := range names {
		if i >= 10 {
			lines = append(lines, fmt.Sprintf("... and %d more", len(names)-10))
			break
		}
		lines = append(lines, fmt.Sprintf("  %d. %s", i+1, n))
	}
	return strings.Join(lines, "\n")
}

// sendFiles sends each downloaded track to the user without showing progress.
func (b *Bot) sendFiles(job *Job, tracks []app.AddedTrack) {
	b.sendTracks(job, tracks, nil)
}

// sendTracks sends each downloaded track to the user.
// It checks the file_id cache first; if a cache hit is found the file is sent
// directly from Telegram's servers (no upload, no disk read).
// On a cache miss the file is uploaded and the returned file_id is stored for
// future requests.
// Files over MaxFileSizeMB are skipped with a notice, or zipped and uploaded to a
// file host when one is configured.
//
// up, when not nil, shows the upload in the job's message and ends that message
// with the final summary, on every path out of this function. Passing nil sends
// the files without any progress display.
func (b *Bot) sendTracks(job *Job, tracks []app.AddedTrack, up *UploadProgress) {
	defer up.Complete() // idempotent: the message always ends with its final text

	limitBytes := b.cfg.MaxFileSizeMB * 1024 * 1024

	// Work out what will really be uploaded to Telegram, for the progress bar.
	// Tracks that are cached, missing or over the limit are not part of it.
	planned := make([]bool, len(tracks))
	var planFiles int
	var planBytes int64
	for i, t := range tracks {
		if size, ok := b.uploadSize(t, job.Mode, limitBytes); ok {
			planned[i] = true
			planFiles++
			planBytes += size
		}
	}
	up.Plan(planFiles, planBytes)

	// Tracks over the limit are collected here and delivered as one ZIP link
	// after the others have been sent, when a file host is configured.
	var oversized []app.AddedTrack

	for i, track := range tracks {
		if job.ctx.Err() != nil {
			up.Cancel()
			return // cancelled: send nothing more
		}
		if track.Path == "" {
			continue
		}

		ext := strings.ToLower(filepath.Ext(track.Path))
		isAudio := ext == ".m4a" || ext == ".flac" || ext == ".mp3" || ext == ".aac"

		// A track can be cached here when the lookup before the download could
		// not plan the link (for example a station) or failed. The file was
		// already downloaded, so it is still cleaned up like any uploaded file.
		if entry, ok := b.fileIDCache.Get(track.TrackID, job.Mode); ok {
			sendErr := b.sendCached(job.ChatID, entry)
			if sendErr == nil {
				b.cleaner.OnUploaded(track.Path)
				continue
			}
			log.Printf("fileid_cache: cached send failed for %s: %v", track.TrackID, sendErr)
			if isStaleFileID(sendErr) {
				b.fileIDCache.Delete(track.TrackID, job.Mode)
			}
		}

		// Cache miss: need the file on disk.
		info, err := os.Stat(track.Path)
		if err != nil {
			_, _ = b.api.Send(tgbotapi.NewMessage(job.ChatID,
				fmt.Sprintf("File not found on disk: %s", filepath.Base(track.Path))))
			continue
		}
		if info.Size() > limitBytes {
			if b.uploader != nil {
				oversized = append(oversized, track)
				continue
			}
			_, _ = b.api.Send(tgbotapi.NewMessage(job.ChatID,
				fmt.Sprintf("File too large to send (%s): %s",
					humanSize(info.Size()), filepath.Base(track.Path))))
			continue
		}

		if !planned[i] {
			up.AddUnplanned(info.Size()) // its cached copy was unusable: it is uploaded after all
		}
		up.Begin(filepath.Base(track.Path), info.Size())

		// Called from the goroutine that streams the file; up may be nil.
		onSent := func(sent, total int64) { up.Sent(sent) }

		var sendErr error
		if isAudio {
			sendErr = b.sendAudio(job.ctx, job.ChatID, job.Mode, track, onSent)
		} else {
			sendErr = b.sendDocument(job.ctx, job.ChatID, job.Mode, track, onSent)
		}
		up.Finish()

		if sendErr != nil {
			if job.ctx.Err() != nil {
				up.Cancel() // /cancel stopped the transfer: that is not an error to report
				return
			}
			_, _ = b.api.Send(tgbotapi.NewMessage(job.ChatID,
				fmt.Sprintf("Could not send %s: %v", filepath.Base(track.Path), sendErr)))
			continue
		}
		b.cleaner.OnUploaded(track.Path)
	}

	// The job's own message is finished before the slower ZIP upload, which has
	// its own messages.
	up.Complete()

	if len(oversized) > 0 && job.ctx.Err() == nil {
		b.deliverLarge(job, oversized)
	}
}

// uploadSize reports whether track will be uploaded to Telegram, which is when it
// is not in the file_id cache, exists, and is within the size limit, and its size.
func (b *Bot) uploadSize(track app.AddedTrack, mode Mode, limitBytes int64) (int64, bool) {
	if track.Path == "" {
		return 0, false
	}
	if _, cached := b.fileIDCache.Get(track.TrackID, mode); cached {
		return 0, false
	}
	info, err := os.Stat(track.Path)
	if err != nil || info.Size() > limitBytes {
		return 0, false
	}
	return info.Size(), true
}

// uploadSource opens a file for upload. The file is handed to the Telegram
// client through a reader that reports every read to onSent (which may be nil)
// and ends the transfer as soon as ctx does, so a cancelled job stops uploading
// at once. The returned function closes the file; call it when the upload is over.
func uploadSource(ctx context.Context, path string, onSent func(sent, total int64)) (tgbotapi.RequestFileData, func(), error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, func() {}, err
	}
	info, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, func() {}, err
	}
	reader := &uploadReader{ctx: ctx, r: f, total: info.Size(), fn: onSent}
	return tgbotapi.FileReader{Name: filepath.Base(path), Reader: reader}, func() { f.Close() }, nil
}

// sendAudioCached sends an audio track using a previously stored Telegram file_id.
// No thumbnail is set here: Telegram does not accept a file_id as a thumbnail,
// and the stored file already carries the thumbnail it was uploaded with.
func (b *Bot) sendAudioCached(chatID int64, entry FileIDEntry) error {
	audio := tgbotapi.NewAudio(chatID, tgbotapi.FileID(entry.FileID))
	audio.Performer = entry.Artist
	audio.Title = entry.Song
	_, err := b.sendRetry(audio)
	return err
}

// sendDocumentCached sends a document using a previously stored Telegram file_id.
func (b *Bot) sendDocumentCached(chatID int64, entry FileIDEntry) error {
	doc := tgbotapi.NewDocument(chatID, tgbotapi.FileID(entry.FileID))
	_, err := b.sendRetry(doc)
	return err
}

// maxThumbBytes is kept safely below Telegram's 200 kB thumbnail limit.
const maxThumbBytes = 190 * 1024

// buildThumbnail produces a thumbnail that satisfies Telegram's rules
// (JPEG, at most 320x320, under 200 kB). Telegram silently ignores a thumbnail
// that breaks any of these rules, so the raw album cover (often several MB when
// artwork.size is large) must never be passed through as-is.
//
// Sources are tried in order:
//  1. the album/playlist cover saved by the downloader (CoverPath)
//  2. the artwork embedded in the audio file itself
//
// It returns an empty path (and a no-op cleanup) when no thumbnail could be
// produced; the reason is logged so the failure is visible.
func (b *Bot) buildThumbnail(track app.AddedTrack) (string, func()) {
	noop := func() {}

	var sources []string
	if track.CoverPath != "" {
		if _, err := os.Stat(track.CoverPath); err == nil {
			sources = append(sources, track.CoverPath)
		}
	}
	sources = append(sources, track.Path)

	ffmpeg := b.appCfg.Convert.FFmpegPath
	if ffmpeg == "" {
		ffmpeg = "ffmpeg"
	}
	const scale = "scale='min(320,iw)':'min(320,ih)':force_original_aspect_ratio=decrease"

	for _, src := range sources {
		// Lower -q:v number = higher quality = larger file. Step down until it fits.
		for _, q := range []string{"4", "12", "24"} {
			tmp, err := os.CreateTemp("", "amdl-thumb-*.jpg")
			if err != nil {
				log.Printf("thumbnail: cannot create temp file: %v", err)
				return "", noop
			}
			tmp.Close()
			out := tmp.Name()

			cmd := exec.Command(ffmpeg, "-y", "-loglevel", "error",
				"-i", src, "-map", "0:v:0", "-an",
				"-vf", scale, "-frames:v", "1", "-update", "1",
				"-q:v", q, out)
			if output, err := cmd.CombinedOutput(); err != nil {
				os.Remove(out)
				log.Printf("thumbnail: ffmpeg failed for %s: %v: %s",
					filepath.Base(src), err, lastLines(string(output), 2))
				break // same source will fail at every quality, try the next source
			}

			info, err := os.Stat(out)
			if err == nil && info.Size() > 0 && info.Size() <= maxThumbBytes {
				return out, func() { os.Remove(out) }
			}
			os.Remove(out)
		}
	}

	log.Printf("thumbnail: none produced for %q (is ffmpeg installed and on PATH?)", track.Song)
	return "", noop
}

// sendAudio uploads an audio file with a Telegram-compliant thumbnail and
// stores the returned file_id in the cache.
func (b *Bot) sendAudio(ctx context.Context, chatID int64, mode Mode, track app.AddedTrack, onSent func(sent, total int64)) error {
	src, closeSrc, err := uploadSource(ctx, track.Path, onSent)
	if err != nil {
		return err
	}
	defer closeSrc() // before the caller deletes the file: an open file cannot be removed on Windows

	audio := tgbotapi.NewAudio(chatID, src)
	audio.Performer = track.Artist
	audio.Title = track.Song

	thumbPath, cleanThumb := b.buildThumbnail(track)
	defer cleanThumb()
	if thumbPath != "" {
		audio.Thumb = tgbotapi.FilePath(thumbPath)
	}

	msg, err := b.api.Send(audio)
	if err != nil {
		return err
	}

	// Only cache uploads that carry a proper thumbnail. Caching a thumbnail-less
	// upload would make every later request for this track reuse that result;
	// leaving it uncached lets the next request try again.
	if msg.Audio != nil && track.TrackID != "" && thumbPath != "" {
		b.fileIDCache.Set(track.TrackID, mode, FileIDEntry{
			FileID: msg.Audio.FileID,
			Kind:   "audio",
			Artist: track.Artist,
			Song:   track.Song,
		})
	}
	return nil
}

// sendDocument uploads a non-audio file with a thumbnail and caches the file_id.
func (b *Bot) sendDocument(ctx context.Context, chatID int64, mode Mode, track app.AddedTrack, onSent func(sent, total int64)) error {
	src, closeSrc, err := uploadSource(ctx, track.Path, onSent)
	if err != nil {
		return err
	}
	defer closeSrc()

	doc := tgbotapi.NewDocument(chatID, src)

	thumbPath, cleanThumb := b.buildThumbnail(track)
	defer cleanThumb()
	if thumbPath != "" {
		doc.Thumb = tgbotapi.FilePath(thumbPath)
	}

	msg, err := b.api.Send(doc)
	if err != nil {
		return err
	}

	if msg.Document != nil && track.TrackID != "" && thumbPath != "" {
		b.fileIDCache.Set(track.TrackID, mode, FileIDEntry{
			FileID: msg.Document.FileID,
			Kind:   "document",
			Artist: track.Artist,
			Song:   track.Song,
		})
	}
	return nil
}

// buildSummary formats downloaded tracks into a readable list.
func buildSummary(tracks []app.AddedTrack) string {
	if len(tracks) == 0 {
		return "(no tracks)"
	}
	var lines []string
	// Group by album
	album := tracks[0].Album
	artist := tracks[0].Artist
	lines = append(lines, fmt.Sprintf("%s - %s (%d track(s))", artist, album, len(tracks)))
	for i, t := range tracks {
		if i >= 10 {
			lines = append(lines, fmt.Sprintf("... and %d more", len(tracks)-10))
			break
		}
		lines = append(lines, fmt.Sprintf("  %d. %s", i+1, t.Song))
	}
	return strings.Join(lines, "\n")
}

// lastLines returns the last n non-empty lines of a log string.
func lastLines(log string, n int) string {
	lines := strings.Split(strings.TrimSpace(log), "\n")
	if len(lines) <= n {
		return strings.Join(lines, "\n")
	}
	return strings.Join(lines[len(lines)-n:], "\n")
}

// humanSize formats bytes as a human-readable string.
func humanSize(b int64) string {
	const unit = 1024
	if b < unit {
		return fmt.Sprintf("%d B", b)
	}
	div, exp := int64(unit), 0
	for n := b / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(b)/float64(div), "KMGTPE"[exp])
}
