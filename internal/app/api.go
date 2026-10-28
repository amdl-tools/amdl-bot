package app

// api.go exposes public methods on Runner that the bot (and any future
// programmatic caller) can use without going through Main() / os.Args.
//
// These are thin wrappers around the existing unexported methods so that
// the upstream logic stays unchanged.

import "fmt"

// RunAlbum downloads an album (or a single track within an album when urlArgI
// is non-empty, matching the ?i= query parameter).
func (r *Runner) RunAlbum(albumID, token, storefront, mediaUserToken, urlArgI string) error {
	return r.ripAlbum(albumID, token, storefront, mediaUserToken, urlArgI)
}

// RunPlaylist downloads a playlist.
func (r *Runner) RunPlaylist(playlistID, token, storefront, mediaUserToken string) error {
	return r.ripPlaylist(playlistID, token, storefront, mediaUserToken)
}

// RunStation downloads a station.
func (r *Runner) RunStation(stationID, token, storefront, mediaUserToken string) error {
	return r.ripStation(stationID, token, storefront, mediaUserToken)
}

// RunSong downloads a single song.
func (r *Runner) RunSong(songID, token, storefront, mediaUserToken string) error {
	return r.ripSong(songID, token, storefront, mediaUserToken)
}

// RunMV downloads a music video.
func (r *Runner) RunMV(mvID, saveDir, token, storefront string) error {
	fmt.Println("Track 1 of 1: mv")
	return r.mvDownloader(mvID, saveDir, token, storefront, nil)
}

// Search returns search result items for the given type and query.
// searchType is one of "album", "song", "artist".
// It returns raw SearchResultItem values so callers can build their own UI
// (e.g. Telegram inline keyboards) instead of using the terminal survey prompt.
func (r *Runner) Search(searchType, query string, token string, limit, offset int) ([]SearchResultItem, bool, error) {
	return r.fetchSearchPage(searchType, query, token, limit, offset)
}
