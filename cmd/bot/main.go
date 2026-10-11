package main

import (
	"amdl/internal/bot"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"
)

func main() {
	configFile := flag.String("config", "config-bot.yaml", "Path to bot config file")
	worker := flag.Bool("worker", false, "internal: run a single download read from stdin, then exit")
	flag.Parse()

	// A download process started by the bot. It must not take the instance lock:
	// the bot that started it already holds it.
	if *worker {
		os.Exit(bot.RunWorker(*configFile, os.Stdin))
	}

	cfg, err := bot.LoadBotConfig(*configFile)
	if err != nil {
		fmt.Fprintln(os.Stderr, "Config error:", err)
		os.Exit(1)
	}

	if cfg.Telegram.Token == "" {
		fmt.Fprintln(os.Stderr, "telegram.token is not set in", *configFile)
		os.Exit(1)
	}

	lock, err := bot.AcquireInstanceLock(cfg.Telegram.InstanceLockPort)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	defer lock.Close()

	b, err := bot.New(cfg)
	if err != nil {
		log.Fatal("Failed to start bot:", err)
	}

	// Download processes live in their own process groups, so Ctrl+C does not
	// reach them. Stop them explicitly, or they would keep running after the
	// bot has exited.
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)

	done := make(chan struct{})
	go func() {
		b.Run()
		close(done)
	}()

	select {
	case s := <-stop:
		log.Printf("received %v: cancelling downloads and shutting down", s)
		b.Shutdown()
	case <-done:
	}
}
