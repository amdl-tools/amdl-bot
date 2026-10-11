package bot

// runner.go runs one download with the existing app.Runner.
//
// The Runner and the packages below it print progress with fmt.Print* (hundreds
// of call sites) and keep state in package-level variables, so they cannot be
// run safely side by side inside one process. The bot therefore runs every
// download in its own process (see worker.go) and runDownload is what that
// process executes.

import (
	ampapi "amdl/internal/amp-api"
	"amdl/internal/app"
	"amdl/internal/config"
	"amdl/internal/download"
	"fmt"
	"net/url"
	"regexp"
	"strings"
)

// RunnerConfig is the subset of bot config needed to build an app.Runner.
type RunnerConfig struct {
	AppConfig config.ConfigSet

	// Token is an Apple Music token already obtained by the cache lookup.
	// When empty, runDownload fetches one itself.
	Token string

	// SkipTracks lists 1-based album/playlist positions that were already
	// delivered from the file_id cache and must not be downloaded again.
	SkipTracks []int
}

// resolveToken prepares the HTTP client (proxy) and returns an Apple Music API
// token. It tries the automatic fetch first and falls back to
// general.authorization-token when the network cannot reach music.apple.com.
func resolveToken(appCfg config.ConfigSet) (string, error) {
	// The proxy must be configured before GetToken makes its first request.
	if err := download.Init(appCfg.General.Proxy); err != nil {
		return "", fmt.Errorf("proxy config: %w", err)
	}

	token, err := ampapi.GetToken()
	if err == nil && token != "" {
		return token, nil
	}

	manual := strings.TrimPrefix(strings.TrimSpace(appCfg.General.AuthorizationToken), "Bearer ")
	if manual != "" && manual != "your-authorization-token" {
		return manual, nil
	}
	if err != nil {
		return "", fmt.Errorf("failed to get Apple Music token: %w", err)
	}
	return "", fmt.Errorf("failed to get Apple Music token: empty response (set general.authorization-token in config as fallback)")
}

// DownloadResult is what the bot receives after a job completes.
type DownloadResult struct {
	AddedTracks []app.AddedTrack
	Log         string // last lines the download printed
	Err         error
}

// runDownload performs one download in the current process. Progress is printed
// to stdout as the Runner normally does; the parent process reads it from the pipe.
func runDownload(cfg RunnerConfig, rawURL string, mode Mode) DownloadResult {
	var result DownloadResult

	// Proxy setup and Apple Music token (reused from the cache lookup when present).
	token := cfg.Token
	if token == "" {
		var err error
		token, err = resolveToken(cfg.AppConfig)
		if err != nil {
			result.Err = err
			return result
		}
	}

	// Build the Runner with the full config from the bot config file.
	runner := app.NewRunner(cfg.AppConfig)

	// Apply mode overrides.
	switch mode {
	case ModeAtmos:
		runner.Flags.Atmos = true
	case ModeAAC:
		runner.Flags.AAC = true
		runner.Config.Media.AacType = "aac-lc"
	}

	// Parse the URL and dispatch to the matching rip method.
	parsed, _ := url.Parse(rawURL)
	songID := ""
	if parsed != nil {
		songID = parsed.Query().Get("i")
	}

	mediaUserToken := runner.Config.General.MediaUserToken

	switch {
	case strings.Contains(rawURL, "/music-video/"):
		storefront, id := detectURL(rawURL, "mv")
		result.Err = runner.RunMV(id, runner.Config.Paths.MV, token, storefront)
	case strings.Contains(rawURL, "/song/"):
		storefront, id := detectURL(rawURL, "song")
		result.Err = runner.RunSong(id, token, storefront, mediaUserToken)
	case strings.Contains(rawURL, "/album/"):
		storefront, id := detectURL(rawURL, "album")
		runner.State.OKDict[id] = append(runner.State.OKDict[id], cfg.SkipTracks...)
		result.Err = runner.RunAlbum(id, token, storefront, mediaUserToken, songID)
	case strings.Contains(rawURL, "/playlist/"):
		storefront, id := detectURL(rawURL, "playlist")
		runner.State.OKDict[id] = append(runner.State.OKDict[id], cfg.SkipTracks...)
		result.Err = runner.RunPlaylist(id, token, storefront, mediaUserToken)
	case strings.Contains(rawURL, "/station/"):
		storefront, id := detectURL(rawURL, "station")
		result.Err = runner.RunStation(id, token, storefront, mediaUserToken)
	default:
		result.Err = fmt.Errorf("unrecognised Apple Music URL: %s", rawURL)
	}

	result.AddedTracks = runner.State.AddedTracks
	return result
}

// urlPatterns mirrors app/url.go so we can parse URLs without modifying the
// upstream package's unexported checkUrl function.
var urlPatterns = map[string]string{
	"album":    `^(?:https:\/\/(?:beta\.music|music|classical\.music)\.apple\.com\/(\w{2})(?:\/album|\/album\/.+))\/(?:id)?(\d[^\D]+)(?:$|\?)`,
	"mv":       `^(?:https:\/\/(?:beta\.music|music)\.apple\.com\/(\w{2})(?:\/music-video|\/music-video\/.+))\/(?:id)?(\d[^\D]+)(?:$|\?)`,
	"song":     `^(?:https:\/\/(?:beta\.music|music|classical\.music)\.apple\.com\/(\w{2})(?:\/song|\/song\/.+))\/(?:id)?(\d[^\D]+)(?:$|\?)`,
	"playlist": `^(?:https:\/\/(?:beta\.music|music|classical\.music)\.apple\.com\/(\w{2})(?:\/playlist|\/playlist\/.+))\/(?:id)?(pl\.[\w-]+)(?:$|\?)`,
	"station":  `^(?:https:\/\/(?:beta\.music|music)\.apple\.com\/(\w{2})(?:\/station|\/station\/.+))\/(?:id)?(ra\.[\w-]+)(?:$|\?)`,
}

func detectURL(rawURL, kind string) (storefront, id string) {
	pat, ok := urlPatterns[kind]
	if !ok {
		return "", ""
	}
	m := regexp.MustCompile(pat).FindStringSubmatch(rawURL)
	if len(m) < 3 {
		return "", ""
	}
	return m[1], m[2]
}

// DetectURLType returns the Apple Music content type ("album", "song", etc.)
// or an empty string if the URL is not recognised.
func DetectURLType(rawURL string) string {
	for kind := range urlPatterns {
		sf, id := detectURL(rawURL, kind)
		if sf != "" && id != "" {
			return kind
		}
	}
	return ""
}
