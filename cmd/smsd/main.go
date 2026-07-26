// Command smsd is a native Linux tray daemon that imports SMS from an Android
// phone over adb into a local SQLite store and provides a read-only viewer.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/jbinder/smsd/internal/adb"
	"github.com/jbinder/smsd/internal/app"
	"github.com/jbinder/smsd/internal/config"
	"github.com/jbinder/smsd/internal/database"
	"github.com/jbinder/smsd/internal/logging"
	"github.com/jbinder/smsd/internal/notify"
	"github.com/jbinder/smsd/internal/tray"
	"github.com/jbinder/smsd/internal/ui"
)

// version is overridden at build time via -ldflags "-X main.version=...".
var version = "dev"

func main() {
	showVersion := flag.Bool("version", false, "print version and exit")
	flag.Parse()
	if *showVersion {
		fmt.Println("smsd", version)
		return
	}

	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "smsd:", err)
		os.Exit(1)
	}
}

func run() error {
	paths, err := config.ResolvePaths()
	if err != nil {
		return fmt.Errorf("resolving paths: %w", err)
	}
	if err := paths.EnsureDirs(); err != nil {
		return err
	}

	cfg, err := config.Load(paths)
	if err != nil {
		return err
	}

	logger, err := logging.New(paths.LogFile, cfg.LogMaxBytes, os.Stderr)
	if err != nil {
		return err
	}
	defer logger.Close()
	logger.Infof("smsd %s starting (config=%s db=%s)", version, paths.ConfigFile, paths.DBFile)

	db, err := database.Open(paths.DBFile)
	if err != nil {
		return err
	}
	defer db.Close()

	client := adb.New(cfg.ADBPath)
	notifier := notify.New(cfg.NotifyEnabled)
	if cfg.NotifyEnabled && !notifier.Available() {
		logger.Warnf("notify-send not found on PATH; desktop notifications disabled")
	}
	viewer := ui.New(db, cfg.UIAddr, logger)
	defer viewer.Stop()

	application := app.New(cfg, logger, client, db, notifier, viewer)

	// Root context cancelled on quit or signal.
	ctx, cancel := context.WithCancel(context.Background())

	t := tray.New(tray.Callbacks{
		OnOpenHistory: application.OpenHistory,
		OnRefresh:     application.Refresh,
		OnMarkRead:    application.MarkRead,
		OnReconnect:   func() { application.Reconnect(ctx) },
		OnQuit: func() {
			logger.Infof("quit requested")
			cancel()
		},
	})
	application.SetTray(t)

	// Translate termination signals into a graceful shutdown of the tray, which
	// unblocks tray.Run below and fires OnQuit.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		select {
		case s := <-sigCh:
			logger.Infof("received signal %s", s)
			t.Quit()
		case <-ctx.Done():
		}
	}()

	// Device monitor + sync loops run in the background.
	monitorDone := make(chan struct{})
	go func() {
		application.Run(ctx)
		close(monitorDone)
	}()

	// tray.Run blocks on the main goroutine until Quit; OnQuit cancels ctx.
	t.Run()

	cancel()
	<-monitorDone
	logger.Infof("smsd stopped")
	return nil
}
