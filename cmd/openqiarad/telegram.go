package main

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	// Embedded CA roots, used only when the system has none (the camera
	// firmware ships without a CA bundle).
	_ "golang.org/x/crypto/x509roots/fallback"

	"github.com/caligone/openqiara/internal/alarm"
	"github.com/caligone/openqiara/internal/camera"
	"github.com/caligone/openqiara/internal/clip"
	"github.com/caligone/openqiara/internal/config"
	"github.com/caligone/openqiara/internal/telegram"
)

const (
	telegramConfigPath = "/data/openqiara_telegram.json"
	clipLength         = 15 * time.Second // alert clips and the 🎥 button
	clipBudget         = 30 * time.Second // shutter, stream start and recording
)

// sensorName returns the sensor label if set, otherwise a generic name
// derived from its type.
func sensorName(sensors []config.SensorEntry, id int) string {
	for _, s := range sensors {
		if s.ID != id {
			continue
		}
		if s.Label != "" {
			return s.Label
		}
		switch s.Type {
		case "DWS":
			return fmt.Sprintf("Ouverture %d", id)
		case "PIR":
			return fmt.Sprintf("Mouvement %d", id)
		}
		break
	}
	return fmt.Sprintf("Capteur %d", id)
}

// startTelegram wires the Telegram notifier on the alarm engine when
// /data/openqiara_telegram.json exists. A bad config is logged and never
// stops the daemon.
func startTelegram(ctx context.Context, eng *alarm.Engine, cam camera.Client, store *config.Store, logger *slog.Logger) {
	cfg, ok, err := telegram.LoadConfig(telegramConfigPath)
	if err != nil {
		logger.Error("telegram: invalid config, notifications disabled", "path", telegramConfigPath, "error", err)
		return
	}
	if !ok {
		return
	}
	if eng == nil {
		logger.Warn("telegram ignoré en mode alarmo")
		return
	}
	// openShutter opens the privacy shutter if it is closed: photos and
	// clips are taken anyway.
	openShutter := func(ctx context.Context) {
		if store.Get().ShutterClosed {
			if err := cam.SetShutter(ctx, true); err != nil {
				logger.Warn("telegram: open shutter failed", "error", err)
			}
		}
	}
	snapshot := func(ctx context.Context) ([]byte, error) {
		openShutter(ctx)
		return camera.MulticastSnapshot(ctx, camera.MulticastJPEG, logger)
	}
	record := func(ctx context.Context) (clip.Clip, error) {
		ctx, cancel := context.WithTimeout(ctx, clipBudget)
		defer cancel() // also stops the multicast reader
		openShutter(ctx)
		src, err := camera.MulticastVideo(ctx, camera.MulticastVideoMain, logger)
		if err != nil {
			return clip.Clip{}, err
		}
		return clip.Record(ctx, src, clipLength)
	}
	n := telegram.NewNotifier(telegram.NewBot(cfg.BotToken, "", nil), telegram.Options{
		ChatIDs:  cfg.ChatIDs,
		Alarm:    eng,
		Snapshot: snapshot,
		Clip:     record,
		Name:     func(id int) string { return sensorName(store.Get().Sensors, id) },
		Now:      time.Now,
		Logger:   logger,
	})
	eng.SetAlertHook(n.Alert)
	go n.Run(ctx)
	logger.Info("telegram notifier started", "chats", len(cfg.ChatIDs))
}
