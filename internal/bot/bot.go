package bot

import (
	"amdl/internal/config"
	"fmt"
	"log"
	"net/http"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
)

// Bot is the top-level struct that wires together the Telegram API client,
// the download queue, and per-user session state.
type Bot struct {
	api      *tgbotapi.BotAPI
	cfg      BotConfig
	appCfg   config.ConfigSet
	queue    *Queue
	sessions *SessionStore
	cleaner     *Cleaner
	stats       *Stats
	monitor     *Monitor
	fileIDCache *FileIDCache
	quota       *Quota     // daily download limits per user; nil when none are set
	uploader    Uploader   // file host for tracks over the Telegram limit; nil when disabled
	locks       *keyedLock // serialises jobs that write the same album
	cfgPath     string     // config file download processes load
	token       string     // cached Apple Music API token (refreshed on error)
}

// New creates and returns a fully initialised Bot.
func New(fullCfg *FullConfig) (*Bot, error) {
	endpoint, err := apiEndpointFormat(fullCfg.Telegram.APIEndpoint)
	if err != nil {
		return nil, err
	}
	api, err := tgbotapi.NewBotAPIWithAPIEndpoint(fullCfg.Telegram.Token, endpoint)
	if err != nil {
		return nil, fmt.Errorf("failed to connect to Telegram (%s): %w", endpoint, err)
	}

	cleaner := NewCleaner(fullCfg.Telegram)
	cleaner.SetWatchDirs([]string{
		fullCfg.Bot.Paths.Alac,
		fullCfg.Bot.Paths.Atmos,
		fullCfg.Bot.Paths.Aac,
		fullCfg.Bot.Paths.MV,
	})
	stats := NewStats(fullCfg.Telegram.StatsFile)
	cleaner.SetStats(stats)
	cleaner.Start()

	fileIDCache := NewFileIDCache(fullCfg.Telegram.FileIDCacheFile)

	b := &Bot{
		api:         api,
		cfg:         fullCfg.Telegram,
		appCfg:      fullCfg.Bot,
		queue:       NewQueue(fullCfg.Telegram.Workers, fullCfg.Telegram.MaxJobsPerUser),
		sessions:    NewSessionStore(10 * time.Minute),
		cleaner:     cleaner,
		stats:       stats,
		fileIDCache: fileIDCache,
		locks:       newKeyedLock(),
		cfgPath:     fullCfg.Path,
		uploader:    newUploader(fullCfg.Telegram),
		quota: NewQuota(QuotaConfig{
			DailyTracks: fullCfg.Telegram.DailyTrackLimit,
			DailyBytes:  fullCfg.Telegram.DailyMBLimit * 1024 * 1024,
		}, fullCfg.Telegram.QuotaFile, fullCfg.Telegram.AdminIDs),
	}
	if b.quota != nil {
		log.Printf("quota: %d track(s) and %d MB per user per day (0 = no limit); admins are exempt",
			fullCfg.Telegram.DailyTrackLimit, fullCfg.Telegram.DailyMBLimit)
	}

	monitor := NewMonitor(
		fullCfg.Bot.General.LiteServer,
		time.Duration(fullCfg.Telegram.MonitorIntervalMin)*time.Minute,
		time.Duration(fullCfg.Telegram.MonitorTimeoutSec)*time.Second,
		fullCfg.Telegram.AdminIDs,
		func(adminID int64, text string) {
			_, _ = b.api.Send(tgbotapi.NewMessage(adminID, text))
		},
	)
	monitor.Start()
	b.monitor = monitor

	// A job cancelled while it was still waiting never reaches processJob, so
	// its progress message is updated here.
	b.queue.SetOnCancelled(func(job *Job) {
		b.edit(job.ChatID, job.MessageID, "Cancelled.")
	})
	b.queue.Start(b.processJob)
	log.Printf("Bot started as @%s (delete-mode: %s)", api.Self.UserName, fullCfg.Telegram.DeleteMode)
	return b, nil
}

// Shutdown stops taking new updates and cancels every download, which kills
// the download processes, then waits a few seconds for them to finish stopping
// and saves the cache and statistics. Without it a download process would keep
// running after the bot exits.
func (b *Bot) Shutdown() {
	b.api.StopReceivingUpdates()
	b.queue.CancelAll()

	deadline := time.Now().Add(10 * time.Second)
	for b.queue.ActiveCount() > 0 && time.Now().Before(deadline) {
		time.Sleep(100 * time.Millisecond)
	}

	// Changes to the cache and the statistics are saved a moment after they happen;
	// write whatever is still waiting before the process exits.
	b.fileIDCache.Flush()
	b.stats.Flush()
	b.quota.Flush()
}

// Run starts receiving updates (long-polling or webhook).
func (b *Bot) Run() {
	if b.cfg.WebhookURL != "" {
		b.runWebhook()
	} else {
		b.runPolling()
	}
}

func (b *Bot) runPolling() {
	u := tgbotapi.NewUpdate(0)
	u.Timeout = 60
	updates := b.api.GetUpdatesChan(u)
	for update := range updates {
		b.dispatch(update)
	}
}

func (b *Bot) runWebhook() {
	port := b.cfg.WebhookPort
	if port == 0 {
		port = 8443
	}
	wh, err := tgbotapi.NewWebhook(b.cfg.WebhookURL)
	if err != nil {
		log.Fatalf("invalid webhook URL: %v", err)
	}
	if _, err = b.api.Request(wh); err != nil {
		log.Fatalf("failed to set webhook: %v", err)
	}
	updates := b.api.ListenForWebhook("/")
	go func() {
		addr := fmt.Sprintf(":%d", port)
		log.Printf("Webhook active on %s", addr)
		if err := http.ListenAndServe(addr, nil); err != nil {
			log.Fatalf("webhook server error: %v", err)
		}
	}()
	for update := range updates {
		b.dispatch(update)
	}
}

// dispatch routes an incoming update to the correct handler.
func (b *Bot) dispatch(update tgbotapi.Update) {
	if update.CallbackQuery != nil {
		b.handleCallback(update.CallbackQuery)
		return
	}
	if update.Message == nil {
		return
	}
	msg := update.Message

	// Access control
	if !b.isAllowed(msg.From.ID) {
		b.reply(msg, "Access denied.")
		return
	}

	if msg.IsCommand() {
		switch msg.Command() {
		case "start":
			b.cmdStart(msg)
		case "help":
			b.cmdHelp(msg)
		case "dl":
			b.cmdDownload(msg, ModeALAC)
		case "atmos":
			b.cmdDownload(msg, ModeAtmos)
		case "aac":
			b.cmdDownload(msg, ModeAAC)
		case "mv":
			b.cmdDownload(msg, ModeMV)
		case "search":
			b.cmdSearch(msg)
		case "status":
			b.cmdStatus(msg)
		case "cancel":
			b.cmdCancel(msg)
		case "stats":
			b.cmdStats(msg)
		case "quota":
			b.cmdQuota(msg)
		case "resetquota":
			b.cmdResetQuota(msg)
		default:
			b.reply(msg, "Unknown command. Send /help for the list of commands.")
		}
		return
	}

	// Plain text that looks like an Apple Music link
	if DetectURLType(msg.Text) != "" {
		b.enqueueURL(msg, msg.Text, ModeALAC)
		return
	}

	b.reply(msg, "Send an Apple Music link or use /help.")
}

// isAllowed checks if a user is permitted to use the bot.
// An empty AllowedUsers list means everyone is allowed.
func (b *Bot) isAllowed(userID int64) bool {
	if len(b.cfg.AllowedUsers) == 0 {
		return true
	}
	for _, id := range b.cfg.AllowedUsers {
		if id == userID {
			return true
		}
	}
	return false
}

// reply sends a plain-text reply to a message.
func (b *Bot) reply(msg *tgbotapi.Message, text string) {
	out := tgbotapi.NewMessage(msg.Chat.ID, text)
	out.ReplyToMessageID = msg.MessageID
	_, _ = b.api.Send(out)
}

// send sends a plain message to a chat.
func (b *Bot) send(chatID int64, text string) (tgbotapi.Message, error) {
	out := tgbotapi.NewMessage(chatID, text)
	return b.api.Send(out)
}

// edit updates an existing message.
func (b *Bot) edit(chatID int64, msgID int, text string) {
	edit := tgbotapi.NewEditMessageText(chatID, msgID, text)
	_, _ = b.api.Send(edit)
}
