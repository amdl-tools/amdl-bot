package bot

import (
	"fmt"
	"net/url"
	"strings"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
)

// Upload limits of the Bot API. The public server accepts 50 MB per file; a
// server started with --local accepts up to 2000 MB.
const (
	cloudMaxFileSizeMB = 50
	localMaxFileSizeMB = 2000
)

// defaultMaxFileSizeMB picks the upload limit to assume when
// telegram.max-file-size-mb is not set.
func defaultMaxFileSizeMB(apiEndpoint string) int64 {
	if strings.TrimSpace(apiEndpoint) != "" {
		return localMaxFileSizeMB
	}
	return cloudMaxFileSizeMB
}

// apiEndpointFormat turns telegram.api-endpoint (a base URL such as
// "http://telegram-bot-api:8081") into the format string the Telegram client
// library expects: ".../bot%s/%s", filled in with the token and the method.
// An empty base URL selects the public api.telegram.org.
func apiEndpointFormat(base string) (string, error) {
	base = strings.TrimSpace(base)
	if base == "" {
		return tgbotapi.APIEndpoint, nil
	}
	u, err := url.Parse(base)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return "", fmt.Errorf("telegram.api-endpoint must look like http://host:8081, got %q", base)
	}
	return strings.TrimRight(base, "/") + "/bot%s/%s", nil
}
