package bot

import (
	"strings"
	"testing"
)

func TestDetectURLType(t *testing.T) {
	cases := []struct{ url, want string }{
		{"https://music.apple.com/gb/album/fantasy/169962059?i=169962189", "album"},
		{"https://music.apple.com/us/album/some-album/1234567890", "album"},
		{"https://music.apple.com/us/album/1234567890", "album"},
		{songURL, "song"},
		{"https://music.apple.com/us/playlist/todays-hits/pl.f4d106fed2bd41149aaacabb233eb5eb", "playlist"},
		{"https://music.apple.com/us/station/some-station/ra.985484166", "station"},
		{"https://music.apple.com/us/music-video/some-video/1234567890", "mv"},
		{"https://example.com/us/album/some-album/1234567890", ""},
		{"not a url", ""},
		{"", ""},
	}
	for _, tc := range cases {
		if got := DetectURLType(tc.url); got != tc.want {
			t.Errorf("DetectURLType(%q) = %q, want %q", tc.url, got, tc.want)
		}
	}
}

func TestDetectURLExtractsStorefrontAndID(t *testing.T) {
	sf, id := detectURL("https://music.apple.com/gb/album/fantasy/169962059?i=169962189", "album")
	if sf != "gb" || id != "169962059" {
		t.Fatalf("got storefront %q id %q", sf, id)
	}
	sf, id = detectURL("https://music.apple.com/us/playlist/x/pl.abc123", "playlist")
	if sf != "us" || id != "pl.abc123" {
		t.Fatalf("got storefront %q id %q", sf, id)
	}
	if sf, id = detectURL("https://example.com", "album"); sf != "" || id != "" {
		t.Fatalf("a non Apple Music link must not match, got %q %q", sf, id)
	}
	if sf, id = detectURL(songURL, "no-such-kind"); sf != "" || id != "" {
		t.Fatalf("an unknown kind must not match, got %q %q", sf, id)
	}
}

// Links whose track ID is in the URL are resolved without any network access.
func TestListTracksWithoutNetwork(t *testing.T) {
	b := &Bot{}

	cases := []struct {
		name   string
		url    string
		wantID string
		wantOK bool
	}{
		{"song", songURL, "1234567890", true},
		{"music video", "https://music.apple.com/us/music-video/some-video/1234567890", "1234567890", true},
		{"album with ?i=", "https://music.apple.com/gb/album/fantasy/169962059?i=169962189", "169962189", true},
		{"station is never planned", "https://music.apple.com/us/station/some-station/ra.985484166", "", false},
		{"not an Apple Music link", "https://example.com/x", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			list, token, ok := b.listTracks(tc.url)
			tracks := list.Tracks
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tc.wantOK)
			}
			if token != "" {
				t.Errorf("no token should be fetched for this link, got %q", token)
			}
			if !tc.wantOK {
				return
			}
			if len(tracks) != 1 || tracks[0].Index != 1 || tracks[0].ID != tc.wantID {
				t.Fatalf("tracks = %+v, want one track %q at position 1", tracks, tc.wantID)
			}
		})
	}
}

func TestSmallFormatters(t *testing.T) {
	if got := humanSize(1536); got != "1.5 KB" {
		t.Errorf("humanSize(1536) = %q", got)
	}
	if got := lastLines("a\nb\nc\nd", 2); got != "c\nd" {
		t.Errorf("lastLines = %q", got)
	}
	if got := lastLines("a\nb", 5); got != "a\nb" {
		t.Errorf("lastLines with fewer lines = %q", got)
	}
	if got := listNames([]string{"A - 1", "B - 2"}); got != "  1. A - 1\n  2. B - 2" {
		t.Errorf("listNames = %q", got)
	}
	if got := listNames(make([]string, 12)); got == "" || !strings.Contains(got, "... and 2 more") {
		t.Errorf("listNames should cap at 10 entries, got %q", got)
	}
}
