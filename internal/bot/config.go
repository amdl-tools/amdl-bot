package bot

import (
	"amdl/internal/config"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/knadh/koanf/parsers/yaml"
	"github.com/knadh/koanf/providers/file"
	"github.com/knadh/koanf/v2"
)

// BotConfig holds all bot-specific settings.
type BotConfig struct {
	Token          string  `koanf:"token"`
	AllowedUsers   []int64 `koanf:"allowed-users"`
	AdminIDs       []int64 `koanf:"admin-ids"`
	MaxJobsPerUser int     `koanf:"max-jobs-per-user"`
	Workers        int     `koanf:"workers"`
	MaxFileSizeMB  int64   `koanf:"max-file-size-mb"`
	SendAsDocument bool    `koanf:"send-as-document"`
	// Daily download limits per user (0 = no limit); admins are exempt. Only what a
	// download actually fetches counts: tracks served from the cache are free.
	DailyTrackLimit int64  `koanf:"daily-track-limit"`
	DailyMBLimit    int64  `koanf:"daily-mb-limit"`
	QuotaFile       string `koanf:"quota-file"` // where the day's usage is kept between restarts

	// What to do with a file over max-file-size-mb: "none" only tells the user it
	// is too large; "gofile" zips all such files of a job (stored, no compression),
	// uploads the ZIP to Gofile and sends the link.
	LargeFileHost string `koanf:"large-file-host"`
	// Optional Gofile account token. Without one, uploads use a guest account
	// whose token is kept in GofileTokenFile.
	GofileToken     string `koanf:"gofile-token"`
	GofileTokenFile string `koanf:"gofile-token-file"`

	// Base URL of a self-hosted Telegram Bot API server, for example
	// "http://telegram-bot-api:8081". Empty means the public api.telegram.org.
	// A local server in --local mode raises the upload limit from 50 MB to 2000 MB.
	APIEndpoint string `koanf:"api-endpoint"`

	// Webhook mode (optional). If empty, long-polling is used.
	WebhookURL  string `koanf:"webhook-url"`
	WebhookPort int    `koanf:"webhook-port"`

	// File cleanup after upload.
	// Values: "never" | "on_upload" | "disk_pressure"
	DeleteMode           string  `koanf:"delete-mode"`
	DiskThresholdPct     float64 `koanf:"disk-threshold-pct"`
	DiskCheckIntervalMin int     `koanf:"disk-check-interval-min"`

	// Safety limits of delete-mode "disk_pressure".
	DiskMinAgeMin   int   `koanf:"disk-min-age-min"`   // never remove files modified more recently than this
	DiskMaxDeleteMB int64 `koanf:"disk-max-delete-mb"` // most a single check may remove

	// Loopback port used to detect a second bot instance on the same machine.
	InstanceLockPort int `koanf:"instance-lock-port"`

	// Stats persistence: path to the JSON file that stores counters.
	StatsFile string `koanf:"stats-file"`

	// Telegram file_id cache: path to the JSON file that maps track IDs to
	// Telegram file_ids so previously uploaded files are sent without re-uploading.
	FileIDCacheFile string `koanf:"fileid-cache-file"`

	// wrapper-lite health monitor
	MonitorIntervalMin int `koanf:"monitor-interval-min"`
	MonitorTimeoutSec  int `koanf:"monitor-timeout-sec"`
}

// FullConfig is the combined config loaded from config-bot.yaml.
type FullConfig struct {
	Bot      config.ConfigSet // re-uses the existing app config for all amdl settings
	Telegram BotConfig
	Path     string // absolute path the config was loaded from; download processes load the same file
}

// LoadBotConfig loads the bot config file (defaults to config-bot.yaml).
// The file must contain a top-level "telegram" section and may contain any
// section from the standard config.yaml.
func LoadBotConfig(path string) (*FullConfig, error) {
	if path == "" {
		path = "config-bot.yaml"
	}
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return nil, fmt.Errorf("config file not found: %s (run with --config to specify a path)", path)
	}

	k := koanf.New(".")
	if err := k.Load(file.Provider(path), yaml.Parser()); err != nil {
		return nil, fmt.Errorf("failed to load config: %w", err)
	}

	var appCfg config.ConfigSet
	if err := k.Unmarshal("", &appCfg); err != nil {
		return nil, fmt.Errorf("failed to parse app config: %w", err)
	}

	var botCfg BotConfig
	if err := k.Unmarshal("telegram", &botCfg); err != nil {
		return nil, fmt.Errorf("failed to parse telegram config: %w", err)
	}

	// Apply defaults
	if botCfg.Workers <= 0 {
		botCfg.Workers = 2
	}
	if botCfg.MaxJobsPerUser <= 0 {
		botCfg.MaxJobsPerUser = 2
	}
	if botCfg.MaxFileSizeMB <= 0 {
		botCfg.MaxFileSizeMB = defaultMaxFileSizeMB(botCfg.APIEndpoint)
	}
	if botCfg.DailyTrackLimit < 0 || botCfg.DailyMBLimit < 0 {
		return nil, fmt.Errorf("telegram.daily-track-limit and telegram.daily-mb-limit must not be negative")
	}
	if botCfg.QuotaFile == "" {
		botCfg.QuotaFile = "amdl-quota.json"
	}
	botCfg.LargeFileHost = strings.ToLower(strings.TrimSpace(botCfg.LargeFileHost))
	switch botCfg.LargeFileHost {
	case "":
		botCfg.LargeFileHost = "none"
	case "none", "gofile":
	default:
		return nil, fmt.Errorf("telegram.large-file-host must be \"none\" or \"gofile\", got %q", botCfg.LargeFileHost)
	}
	if botCfg.GofileTokenFile == "" {
		botCfg.GofileTokenFile = "amdl-gofile-token.txt"
	}
	if botCfg.DeleteMode == "" {
		botCfg.DeleteMode = "never"
	}
	if botCfg.DiskThresholdPct <= 0 {
		botCfg.DiskThresholdPct = 80
	}
	if botCfg.DiskCheckIntervalMin <= 0 {
		botCfg.DiskCheckIntervalMin = 5
	}
	if botCfg.DiskMinAgeMin <= 0 {
		botCfg.DiskMinAgeMin = defaultDiskMinAgeMin
	}
	if botCfg.DiskMaxDeleteMB <= 0 {
		botCfg.DiskMaxDeleteMB = defaultDiskMaxDeleteMB
	}
	if botCfg.StatsFile == "" {
		botCfg.StatsFile = "amdl-stats.json"
	}
	if botCfg.FileIDCacheFile == "" {
		botCfg.FileIDCacheFile = "amdl-fileid-cache.json"
	}
	if botCfg.MonitorIntervalMin <= 0 {
		botCfg.MonitorIntervalMin = 2
	}
	if botCfg.MonitorTimeoutSec <= 0 {
		botCfg.MonitorTimeoutSec = 5
	}

	abs, err := filepath.Abs(path)
	if err != nil {
		abs = path
	}
	return &FullConfig{Bot: appCfg, Telegram: botCfg, Path: abs}, nil
}
