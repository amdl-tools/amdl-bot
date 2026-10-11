package bot

// precheck.go looks a link up in the file_id cache BEFORE anything is downloaded.
//
//   song / music video / album?i=<song>   the track ID is in the link itself, so
//                                         a hit needs no Apple Music request at all.
//   album / playlist                      one metadata request lists the track IDs;
//                                         every cached track is sent straight away and
//                                         only the missing ones are downloaded.
//   station                               never planned (the track list is dynamic).
//
// For albums and playlists the Runner already skips any position listed in
// State.OKDict[collectionID], so the plan only has to hand it the positions that
// were delivered from cache; the download logic itself is untouched.

import (
	ampapi "amdl/internal/amp-api"
	"fmt"
	"log"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
)

// cachePlan is the outcome of the cache lookup for one job.
type cachePlan struct {
	Token     string   // Apple Music token obtained during the lookup, reused by the download
	Skip      []int    // 1-based positions already delivered from cache
	Delivered int      // tracks sent from cache
	Total     int      // tracks the link contains (0 when the link cannot be planned)
	Names     []string // "Artist - Song" of every delivered track

	// What the progress message shows, when the link could be looked up.
	Title  string         // "Artist - Album" or the playlist name
	Titles map[int]string // 1-based position -> track name
}

// fullyServed reports whether every track was delivered, so no download is needed.
func (p cachePlan) fullyServed() bool {
	return p.Total > 0 && p.Delivered == p.Total
}

type plannedTrack struct {
	Index int // 1-based position, identical to the Runner's TaskNum
	ID    string
	Name  string // track name; empty when the link did not need a metadata lookup
}

// trackList is what a link contains, in order.
type trackList struct {
	Title  string // "Artist - Album" or the playlist name; empty when not looked up
	Tracks []plannedTrack
}

// listTracks resolves a link to its ordered track IDs without downloading anything.
// token is returned whenever one was fetched, even on failure, so the caller can reuse it.
// ok is false when the link cannot be planned.
func (b *Bot) listTracks(rawURL string) (list trackList, token string, ok bool) {
	kind := DetectURLType(rawURL)

	songID := ""
	if u, err := url.Parse(rawURL); err == nil {
		songID = u.Query().Get("i")
	}

	switch kind {
	case "song", "mv":
		_, id := detectURL(rawURL, kind)
		if id == "" {
			return trackList{}, "", false
		}
		return trackList{Tracks: []plannedTrack{{Index: 1, ID: id}}}, "", true

	case "album", "playlist":
		if kind == "album" && songID != "" {
			return trackList{Tracks: []plannedTrack{{Index: 1, ID: songID}}}, "", true
		}

		storefront, id := detectURL(rawURL, kind)
		if storefront == "" || id == "" {
			return trackList{}, "", false
		}
		tok, err := resolveToken(b.appCfg)
		if err != nil {
			log.Printf("precheck: no Apple Music token, skipping cache lookup: %v", err)
			return trackList{}, "", false
		}

		lang := b.appCfg.General.Language
		var title string
		var tracks []plannedTrack
		if kind == "album" {
			resp, err := ampapi.GetAlbumResp(storefront, id, lang, tok)
			if err != nil || resp == nil || len(resp.Data) == 0 {
				log.Printf("precheck: album lookup failed: %v", err)
				return trackList{}, tok, false
			}
			title = joinTitle(resp.Data[0].Attributes.ArtistName, resp.Data[0].Attributes.Name)
			for i, t := range resp.Data[0].Relationships.Tracks.Data {
				tracks = append(tracks, plannedTrack{Index: i + 1, ID: t.ID, Name: t.Attributes.Name})
			}
		} else {
			resp, err := ampapi.GetPlaylistResp(storefront, id, lang, tok)
			if err != nil || resp == nil || len(resp.Data) == 0 {
				log.Printf("precheck: playlist lookup failed: %v", err)
				return trackList{}, tok, false
			}
			title = resp.Data[0].Attributes.Name
			for i, t := range resp.Data[0].Relationships.Tracks.Data {
				tracks = append(tracks, plannedTrack{Index: i + 1, ID: t.ID, Name: t.Attributes.Name})
			}
		}
		return trackList{Title: title, Tracks: tracks}, tok, len(tracks) > 0
	}

	return trackList{}, "", false
}

// joinTitle writes "Artist - Album", or just the album when there is no artist.
func joinTitle(artist, name string) string {
	if artist == "" {
		return name
	}
	return artist + " - " + name
}

// serveFromCache sends every cached track of the job's link to the user and
// reports what is left to download. It never downloads anything itself, and any
// failure simply leaves the track to the normal download path.
func (b *Bot) serveFromCache(job *Job, status func(string)) cachePlan {
	var plan cachePlan

	list, token, ok := b.listTracks(job.URL)
	tracks := list.Tracks
	plan.Token = token
	if !ok || len(tracks) == 0 {
		return plan
	}
	plan.Total = len(tracks)
	plan.Title = list.Title
	for _, t := range tracks {
		if t.Name != "" {
			if plan.Titles == nil {
				plan.Titles = make(map[int]string)
			}
			plan.Titles[t.Index] = t.Name
		}
	}

	// Cheap pass: leave quietly when nothing is cached.
	hits := 0
	for _, t := range tracks {
		if _, found := b.fileIDCache.Get(t.ID, job.Mode); found {
			hits++
		}
	}
	if hits == 0 {
		return plan
	}

	for _, t := range tracks {
		if job.ctx.Err() != nil {
			break
		}
		entry, found := b.fileIDCache.Get(t.ID, job.Mode)
		if !found {
			continue
		}

		if plan.Delivered%5 == 0 { // keep message edits well under Telegram's limits
			status(fmt.Sprintf("Sending from cache [%s]: %d/%d", job.Mode, plan.Delivered+1, hits))
		}

		if err := b.sendCached(job.ChatID, entry); err != nil {
			log.Printf("precheck: cached send failed for %s: %v", t.ID, err)
			if isStaleFileID(err) {
				b.fileIDCache.Delete(t.ID, job.Mode)
			}
			continue // not delivered: it will be downloaded like any other missing track
		}

		plan.Skip = append(plan.Skip, t.Index)
		plan.Delivered++
		plan.Names = append(plan.Names, entry.Artist+" - "+entry.Song)
		pause(250 * time.Millisecond)
	}
	return plan
}

// sendCached dispatches a cached entry as audio or document.
func (b *Bot) sendCached(chatID int64, entry FileIDEntry) error {
	if entry.Kind == "audio" {
		return b.sendAudioCached(chatID, entry)
	}
	return b.sendDocumentCached(chatID, entry)
}

// isStaleFileID reports whether Telegram rejected the stored file_id itself, as
// opposed to a transient failure (network, rate limit) that should not evict a
// perfectly good cache entry.
func isStaleFileID(err error) bool {
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "wrong file") ||
		strings.Contains(msg, "file identifier") ||
		strings.Contains(msg, "file_id")
}

// pause is time.Sleep, replaced by tests so they do not wait for real.
var pause = time.Sleep

var reRetryAfter = regexp.MustCompile(`retry after (\d+)`)

// sendRetry sends a message and, on Telegram's flood limit ("Too Many Requests:
// retry after N"), waits and tries once more. Sending a whole album from cache
// takes seconds instead of the minutes an upload used to, so this limit is now
// easy to hit.
func (b *Bot) sendRetry(c tgbotapi.Chattable) (tgbotapi.Message, error) {
	msg, err := b.api.Send(c)
	if err == nil {
		return msg, nil
	}
	m := reRetryAfter.FindStringSubmatch(err.Error())
	if len(m) < 2 {
		return msg, err
	}
	wait, convErr := strconv.Atoi(m[1])
	if convErr != nil || wait <= 0 || wait > 60 {
		return msg, err
	}
	pause(time.Duration(wait+1) * time.Second)
	return b.api.Send(c)
}
