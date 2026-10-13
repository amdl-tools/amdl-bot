package bot

// largefiles.go handles tracks that are over Telegram's upload limit. Instead of
// dropping them, the bot packs all such tracks of a job into one ZIP archive,
// stored without compression (the audio is already compressed), uploads that to
// a file host, and sends the user the link.

import (
	"amdl/internal/app"
	"archive/zip"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"
)

// Uploader puts a file on an external host and returns a link to download it.
type Uploader interface {
	// Name is the host's display name, used in messages to the user.
	Name() string
	// Upload uploads the file at path under the given file name. progress, when
	// not nil, is called as bytes are sent (sent of total). It returns once the
	// upload is finished or ctx ends.
	Upload(ctx context.Context, path, name string, progress func(sent, total int64)) (url string, err error)
}

// ctxReader makes a long copy stop as soon as its context ends.
type ctxReader struct {
	ctx context.Context
	r   io.Reader
}

func (c ctxReader) Read(p []byte) (int, error) {
	if err := c.ctx.Err(); err != nil {
		return 0, err
	}
	return c.r.Read(p)
}

// buildStoredZip writes the given tracks into a temporary ZIP archive and
// returns its path and the file name to upload it as. Entries are stored, not
// compressed. The caller removes the archive. On any error nothing is left behind.
func buildStoredZip(ctx context.Context, tracks []app.AddedTrack) (zipPath, zipName string, err error) {
	if len(tracks) == 0 {
		return "", "", errors.New("no files to zip")
	}
	f, err := os.CreateTemp("", "amdl-*.zip")
	if err != nil {
		return "", "", err
	}
	tmp := f.Name()
	finished := false
	defer func() {
		if !finished {
			f.Close()
			os.Remove(tmp)
		}
	}()

	zw := zip.NewWriter(f)
	used := make(map[string]bool)
	for _, t := range tracks {
		if err := ctx.Err(); err != nil {
			return "", "", err
		}
		entry := uniqueEntry(entryName(t.Path), used)
		if err := addStored(ctx, zw, t.Path, entry); err != nil {
			return "", "", fmt.Errorf("zipping %s: %w", filepath.Base(t.Path), err)
		}
	}
	if err := zw.Close(); err != nil {
		return "", "", err
	}
	if err := f.Close(); err != nil {
		return "", "", err
	}
	finished = true
	return tmp, zipFileName(tracks), nil
}

// addStored copies one file into the archive without compressing it.
func addStored(ctx context.Context, zw *zip.Writer, src, entry string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	info, err := in.Stat()
	if err != nil {
		return err
	}
	hdr, err := zip.FileInfoHeader(info)
	if err != nil {
		return err
	}
	hdr.Name = entry
	hdr.Method = zip.Store

	w, err := zw.CreateHeader(hdr)
	if err != nil {
		return err
	}
	_, err = io.Copy(w, ctxReader{ctx: ctx, r: in})
	return err
}

// entryName is the name a file gets inside the archive: its folder (normally the
// album) and its own name. ZIP entries always use forward slashes.
func entryName(p string) string {
	base := filepath.Base(p)
	parent := filepath.Base(filepath.Dir(p))
	if parent == "." || parent == ".." || parent == "" || parent == string(filepath.Separator) {
		return base
	}
	return path.Join(parent, base)
}

// uniqueEntry makes name unique within the archive by adding " (2)", " (3)", ...
// before the extension, and records it in used.
func uniqueEntry(name string, used map[string]bool) string {
	candidate := name
	ext := path.Ext(name)
	stem := strings.TrimSuffix(name, ext)
	for i := 2; used[candidate]; i++ {
		candidate = fmt.Sprintf("%s (%d)%s", stem, i, ext)
	}
	used[candidate] = true
	return candidate
}

// zipFileName names the archive after the content it contains:
//   - one track with no album (e.g. a music video): "Artist - Song"
//   - multiple tracks that all share one album:      "Artist - Album"
//   - anything else:                                  "amdl-download"
func zipFileName(tracks []app.AddedTrack) string {
	const generic = "amdl-download"

	first := tracks[0]

	// Single track with no album — typical for a music video.
	if len(tracks) == 1 && first.Album == "" {
		name := first.Song
		if first.Artist != "" && first.Song != "" {
			name = first.Artist + " - " + first.Song
		}
		if s := safeFileName(name); s != "" {
			return s + ".zip"
		}
		return generic + ".zip"
	}

	// Multiple tracks (or a single track that does have an album): use the album
	// when every track shares the same one.
	same := first.Album != ""
	for _, t := range tracks[1:] {
		if t.Album != first.Album || t.Artist != first.Artist {
			same = false
			break
		}
	}
	if !same {
		return generic + ".zip"
	}

	name := first.Album
	if first.Artist != "" {
		name = first.Artist + " - " + first.Album
	}
	if s := safeFileName(name); s != "" {
		return s + ".zip"
	}
	return generic + ".zip"
}

// safeFileName removes what file systems and download managers dislike.
func safeFileName(s string) string {
	s = strings.NewReplacer(
		`\`, "_", "/", "_", ":", "_", "*", "_", "?", "_", `"`, "_", "<", "_", ">", "_", "|", "_",
	).Replace(s)
	s = strings.Map(func(r rune) rune {
		if r < 32 {
			return -1
		}
		return r
	}, s)
	s = strings.Trim(s, " .")
	if r := []rune(s); len(r) > 100 {
		s = strings.TrimRight(string(r[:100]), " .")
	}
	return s
}

// progressEvery is how often the upload progress message is edited.
const progressEvery = 4 * time.Second

// statusMessage sends a message and returns a function that replaces its text.
func (b *Bot) statusMessage(chatID int64, text string) func(string) {
	msg, err := b.send(chatID, text)
	if err != nil {
		return func(string) {}
	}
	return func(t string) { b.edit(chatID, msg.MessageID, t) }
}

// deliverLarge zips the tracks that are over Telegram's limit, uploads the
// archive to the configured host and sends the user the link. The tracks are
// only treated as delivered, and so eligible for deletion, once the link exists;
// when anything fails they stay on the server.
func (b *Bot) deliverLarge(job *Job, tracks []app.AddedTrack) {
	var total int64
	for _, t := range tracks {
		if info, err := os.Stat(t.Path); err == nil {
			total += info.Size()
		}
	}
	host := b.uploader.Name()

	status := b.statusMessage(job.ChatID, fmt.Sprintf(
		"%d file(s), %s in total, are over the %d MB Telegram limit. Zipping them (stored, no compression) and uploading to %s...",
		len(tracks), humanSize(total), b.cfg.MaxFileSizeMB, host))

	zipPath, zipName, err := buildStoredZip(job.ctx, tracks)
	if err != nil {
		if job.ctx.Err() != nil {
			status("Cancelled.")
			return
		}
		status(fmt.Sprintf("Could not create the ZIP: %v\nThe files were kept on the server.", err))
		return
	}
	defer os.Remove(zipPath)

	var last time.Time
	link, err := b.uploader.Upload(job.ctx, zipPath, zipName, func(sent, all int64) {
		if all <= 0 || time.Since(last) < progressEvery {
			return
		}
		last = time.Now()
		status(fmt.Sprintf("Uploading %s to %s: %d%% of %s", zipName, host, sent*100/all, humanSize(all)))
	})
	if job.ctx.Err() != nil {
		status("Cancelled.")
		return
	}
	if err != nil {
		status(fmt.Sprintf("Upload to %s failed: %v\nThe files were kept on the server.", host, err))
		return
	}

	status(fmt.Sprintf(
		"%d file(s) over the Telegram limit were uploaded as one ZIP (%s, stored without compression):\n%s\nAnyone with this link can download it.",
		len(tracks), humanSize(total), link))
	for _, t := range tracks {
		b.cleaner.OnUploaded(t.Path)
	}
}
