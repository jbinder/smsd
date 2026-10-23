// Command smsd is a native Linux tray daemon that imports SMS from an Android
// phone over adb into a local SQLite store and provides a viewer that can also
// send SMS.
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
	"github.com/jbinder/smsd/internal/logging"
	"github.com/jbinder/smsd/internal/notify"
	"github.com/jbinder/smsd/internal/profile"
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
	logger.Infof("smsd %s starting (config=%s profiles=%s)", version, paths.ConfigFile, paths.ProfilesFile)

	profiles, err := profile.Load(paths.ProfilesFile)
	if err != nil {
		return err
	}
	// Older versions kept every phone in one database. Split it once, one
	// profile per phone, so their histories stop mixing.
	migrated, err := profile.MigrateShared(profiles, paths.DBFile, paths.ProfilesDir)
	if err != nil {
		return fmt.Errorf("splitting %s into profiles: %w", paths.DBFile, err)
	}
	for _, p := range migrated {
		logger.Infof("moved phone %s into profile %s (%s); %s is kept as a backup",
			p.Devices[0], p.Name, profile.DBPath(paths.ProfilesDir, p), paths.DBFile)
	}

	client := adb.New(cfg.ADBPath)
	notifier := notify.New(cfg.NotifyEnabled, cfg.NotifyTimeoutSeconds)
	if cfg.NotifyEnabled && !notifier.Available() {
		logger.Warnf("notify-send not found on PATH; desktop notifications disabled")
	}
	viewer := ui.New(nil, cfg.UIAddr, logger)
	defer viewer.Stop()

	application, err := app.New(cfg, logger, client, profiles, paths.ProfilesDir, notifier, viewer)
	if err != nil {
		return err
	}
	defer application.Close()
	viewer.SetProfiles(application)
	viewer.SetSender(application)
	viewer.SetOnRead(application.ReadChanged)

	// Root context cancelled on quit or signal.
	ctx, cancel := context.WithCancel(context.Background())

	t := tray.New(tray.Callbacks{
		OnOpenHistory: application.OpenHistory,
		OnRefresh:     application.Refresh,
		OnMarkRead:    application.MarkRead,
		OnReconnect:   func() { application.Reconnect(ctx) },
		OnNewProfile:  application.NewProfile,
		OnAttach:      application.Attach,
		OnIgnore:      application.Ignore,
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
