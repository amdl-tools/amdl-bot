package app

import (
	ampapi "amdl/internal/amp-api"
	"fmt"
	"strings"
)

// fetchSearchPage calls the Apple Music search API and returns a flat list of
// SearchResultItem values suitable for building a Telegram inline keyboard.
// hasMore is true when there are more pages available (caller should increment offset by limit).
func (r *Runner) fetchSearchPage(searchType, query, token string, limit, offset int) ([]SearchResultItem, bool, error) {
	validTypes := map[string]bool{"album": true, "song": true, "artist": true}
	if !validTypes[searchType] {
		return nil, false, fmt.Errorf("invalid search type %q: must be album, song, or artist", searchType)
	}

	apiType := searchType + "s"
	resp, err := ampapi.Search(r.Config.General.Storefront, query, apiType, r.Config.General.Language, token, limit, offset)
	if err != nil {
		return nil, false, fmt.Errorf("search API error: %w", err)
	}

	var items []SearchResultItem
	hasMore := false

	switch searchType {
	case "album":
		if resp.Results.Albums != nil {
			for _, a := range resp.Results.Albums.Data {
				year := ""
				if len(a.Attributes.ReleaseDate) >= 4 {
					year = a.Attributes.ReleaseDate[:4]
				}
				detail := fmt.Sprintf("%s (%s, %d tracks)", a.Attributes.ArtistName, year, a.Attributes.TrackCount)
				items = append(items, SearchResultItem{
					Type:   "Album",
					Name:   a.Attributes.Name,
					Detail: detail,
					URL:    a.Attributes.URL,
					ID:     a.ID,
				})
			}
			hasMore = resp.Results.Albums.Next != ""
		}
	case "song":
		if resp.Results.Songs != nil {
			for _, s := range resp.Results.Songs.Data {
				detail := fmt.Sprintf("%s - %s", s.Attributes.ArtistName, s.Attributes.AlbumName)
				items = append(items, SearchResultItem{
					Type:   "Song",
					Name:   s.Attributes.Name,
					Detail: detail,
					URL:    s.Attributes.URL,
					ID:     s.ID,
				})
			}
			hasMore = resp.Results.Songs.Next != ""
		}
	case "artist":
		if resp.Results.Artists != nil {
			for _, a := range resp.Results.Artists.Data {
				detail := strings.Join(a.Attributes.GenreNames, ", ")
				items = append(items, SearchResultItem{
					Type:   "Artist",
					Name:   a.Attributes.Name,
					Detail: detail,
					URL:    a.Attributes.URL,
					ID:     a.ID,
				})
			}
			hasMore = resp.Results.Artists.Next != ""
		}
	}

	return items, hasMore, nil
}
