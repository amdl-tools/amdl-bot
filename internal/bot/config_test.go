package bot

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeConfig(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config-bot.yaml")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoadBotConfigDefaults(t *testing.T) {
	cfg, err := LoadBotConfig(writeConfig(t, "telegram:\n  token: \"abc\"\n"))
	if err != nil {
		t.Fatal(err)
	}
	tg := cfg.Telegram
	if tg.Token != "abc" {
		t.Errorf("Token = %q", tg.Token)
	}
	if tg.Workers != 2 || tg.MaxJobsPerUser != 2 {
		t.Errorf("Workers=%d MaxJobsPerUser=%d, want 2 and 2", tg.Workers, tg.MaxJobsPerUser)
	}
	if tg.MaxFileSizeMB != 50 {
		t.Errorf("MaxFileSizeMB = %d, want 50 for the public Bot API", tg.MaxFileSizeMB)
	}
	if tg.DiskMinAgeMin != 60 || tg.DiskMaxDeleteMB != 2048 {
		t.Errorf("disk safety limits = %d min / %d MB, want 60 and 2048", tg.DiskMinAgeMin, tg.DiskMaxDeleteMB)
	}
	if tg.DeleteMode != "never" || tg.DiskThresholdPct != 80 {
		t.Errorf("DeleteMode=%q DiskThresholdPct=%v", tg.DeleteMode, tg.DiskThresholdPct)
	}
	if tg.StatsFile != "amdl-stats.json" || tg.FileIDCacheFile != "amdl-fileid-cache.json" {
		t.Errorf("StatsFile=%q FileIDCacheFile=%q", tg.StatsFile, tg.FileIDCacheFile)
	}
	if tg.MonitorIntervalMin != 2 || tg.MonitorTimeoutSec != 5 {
		t.Errorf("monitor defaults = %d min / %d s", tg.MonitorIntervalMin, tg.MonitorTimeoutSec)
	}
}

func TestLoadBotConfigLocalServerRaisesUploadLimit(t *testing.T) {
	cfg, err := LoadBotConfig(writeConfig(t,
		"telegram:\n  token: \"abc\"\n  api-endpoint: \"http://telegram-bot-api:8081\"\n"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Telegram.APIEndpoint != "http://telegram-bot-api:8081" {
		t.Errorf("APIEndpoint = %q", cfg.Telegram.APIEndpoint)
	}
	if cfg.Telegram.MaxFileSizeMB != 2000 {
		t.Errorf("MaxFileSizeMB = %d, want 2000 with a local Bot API server", cfg.Telegram.MaxFileSizeMB)
	}
}

func TestLoadBotConfigExplicitLimitWins(t *testing.T) {
	cfg, err := LoadBotConfig(writeConfig(t,
		"telegram:\n  token: \"abc\"\n  api-endpoint: \"http://localhost:8081\"\n  max-file-size-mb: 100\n"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Telegram.MaxFileSizeMB != 100 {
		t.Errorf("MaxFileSizeMB = %d, want the explicit 100", cfg.Telegram.MaxFileSizeMB)
	}
}

func TestLoadBotConfigMissingFile(t *testing.T) {
	if _, err := LoadBotConfig(filepath.Join(t.TempDir(), "nope.yaml")); err == nil {
		t.Fatal("a missing config file must be an error")
	}
}

func TestLoadBotConfigLargeFileHost(t *testing.T) {
	cfg, err := LoadBotConfig(writeConfig(t, "telegram:\n  token: \"abc\"\n"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Telegram.LargeFileHost != "none" || cfg.Telegram.GofileTokenFile != "amdl-gofile-token.txt" {
		t.Errorf("defaults: host %q, token file %q", cfg.Telegram.LargeFileHost, cfg.Telegram.GofileTokenFile)
	}

	cfg, err = LoadBotConfig(writeConfig(t, "telegram:\n  token: \"abc\"\n  large-file-host: \" GoFile \"\n"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Telegram.LargeFileHost != "gofile" {
		t.Errorf("host should be normalised to lower case, got %q", cfg.Telegram.LargeFileHost)
	}

	_, err = LoadBotConfig(writeConfig(t, "telegram:\n  token: \"abc\"\n  large-file-host: \"mega\"\n"))
	if err == nil || !strings.Contains(err.Error(), "large-file-host") {
		t.Fatalf("an unknown host must be rejected with a pointer to the setting, got %v", err)
	}
}

func TestLoadBotConfigQuota(t *testing.T) {
	cfg, err := LoadBotConfig(writeConfig(t, "telegram:\n  token: \"abc\"\n"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Telegram.DailyTrackLimit != 0 || cfg.Telegram.DailyMBLimit != 0 || cfg.Telegram.QuotaFile != "amdl-quota.json" {
		t.Errorf("defaults: %d tracks, %d MB, file %q (limits are off by default)",
			cfg.Telegram.DailyTrackLimit, cfg.Telegram.DailyMBLimit, cfg.Telegram.QuotaFile)
	}

	cfg, err = LoadBotConfig(writeConfig(t, "telegram:\n  token: \"abc\"\n  daily-track-limit: 20\n  daily-mb-limit: 2048\n"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Telegram.DailyTrackLimit != 20 || cfg.Telegram.DailyMBLimit != 2048 {
		t.Errorf("limits = %d tracks, %d MB", cfg.Telegram.DailyTrackLimit, cfg.Telegram.DailyMBLimit)
	}

	_, err = LoadBotConfig(writeConfig(t, "telegram:\n  token: \"abc\"\n  daily-track-limit: -1\n"))
	if err == nil || !strings.Contains(err.Error(), "must not be negative") {
		t.Fatalf("a negative limit must be rejected, got %v", err)
	}
}
