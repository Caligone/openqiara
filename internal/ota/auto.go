package ota

import (
	"context"
	"log/slog"
	"os"
	"strings"
	"time"
)

// Mise à jour automatique et essai du nouveau binaire.
//
// Cycle d'une release installée :
//
//  1. Installer garde l'ancien binaire en BackupPath et stage le nouveau ;
//     onComplete écrit PendingMarkerPath + PendingTagPath, attend l'alarme
//     désarmée et reboote.
//  2. boot.sh fait la bascule et pose TrialMarkerPath (le tag).
//  3. Avant chaque lancement d'openqiarad pendant l'essai, boot.sh ajoute
//     une ligne au marqueur. Au-delà de 4 lancements sans confirmation, il
//     remet BackupPath et l'ancien boot.sh, écrit le tag dans
//     FailedMarkerPath et reboote. Compter les lancements sur disque, et
//     pas les sorties en RAM, couvre aussi le binaire qui ne plante pas
//     mais fait rebooter la cam : sa santé échoue, le watchdog MCU n'est
//     plus nourri, la cam redémarre en 5 à 6 min.
//  4. ConfirmTrial efface le marqueur passé TrialPeriod, dès que la santé
//     est OK : la release est adoptée.

// TrialMarkerPath existe tant que le binaire installé par l'OTA n'a pas été
// confirmé. 1re ligne : le tag ; puis une ligne par lancement (boot.sh).
const TrialMarkerPath = "/data/ota_trial"

// FailedMarkerPath contient le tag de la dernière release revenue en
// arrière, ou dont la bascule a échoué. Écrit par boot.sh.
const FailedMarkerPath = "/data/ota_failed"

// TrialPeriod est la durée d'essai minimale. La santé ne voit une radio
// muette qu'après 30 min sans trame de la sirène (RadioHealth), puis le
// watchdog MCU reboote 5 à 6 min plus tard : en deçà, une release à la
// radio cassée serait adoptée avant de faire rebooter la cam en boucle.
const TrialPeriod = 40 * time.Minute

// ConfirmTrial adopte le binaire courant s'il est en essai : passé
// TrialPeriod, au premier contrôle de santé OK. Pas de santé continue
// exigée : un essai qui ne se confirmerait jamais bloquerait toutes les
// installs (ErrBusy), et ce qu'on veut écarter, c'est le binaire qui fait
// rebooter la cam ; celui qui a tenu 40 min ne l'a pas fait. Bloque
// jusque-là, à lancer en goroutine.
func ConfirmTrial(ctx context.Context, health func() error, logger *slog.Logger) {
	confirmTrial(ctx, TrialMarkerPath, health, TrialPeriod, 30*time.Second, logger)
}

func confirmTrial(ctx context.Context, path string, health func() error, period, every time.Duration, logger *slog.Logger) {
	if _, err := os.Stat(path); err != nil {
		return
	}
	select {
	case <-ctx.Done():
		return
	case <-time.After(period):
	}
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		if health() == nil {
			if err := os.Remove(path); err != nil {
				logger.Error("OTA: confirm trial failed", "error", err)
				return
			}
			logger.Info("OTA: new binary confirmed")
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// AutoConfig branche l'auto-update sur le reste du daemon.
type AutoConfig struct {
	Enabled  func() bool // option update.auto
	Disarmed func() bool // alarme désarmée, état connu
	Every    time.Duration
	First    time.Duration // premier check après le démarrage
}

// RunAuto vérifie périodiquement la dernière release et l'installe quand
// les conditions sont réunies. Bloque jusqu'à l'annulation de ctx.
func RunAuto(ctx context.Context, client *Client, installer *Installer, cfg AutoConfig, logger *slog.Logger) {
	wait := cfg.First
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
		wait = cfg.Every

		if !cfg.Enabled() || !cfg.Disarmed() {
			continue
		}
		res, err := client.CheckLatest(ctx, true)
		if err != nil {
			logger.Warn("OTA auto: check failed", "error", err)
			continue
		}
		tag := res.Latest.TagName
		if !shouldAutoInstall(res.Newer, tag, readMarker(FailedMarkerPath)) {
			continue
		}
		logger.Info("OTA auto: installing", "from", res.Current, "to", tag)
		// Install en attente de reboot ou binaire en essai : ErrBusy.
		if err := installer.Start(ctx, tag); err != nil {
			logger.Info("OTA auto: install not started", "error", err)
		}
	}
}

// unknownTag est ce que boot.sh écrit quand le tag de la release a été
// perdu (marqueur vide après une coupure, install par un ancien binaire).
const unknownTag = "unknown"

// shouldAutoInstall : une release plus récente que la courante, qui n'a
// pas déjà échoué. Un échec au tag inconnu arrête l'auto-update : sinon il
// réinstallerait peut-être la release cassée. Supprimer /data/ota_failed,
// ou installer à la main, pour reprendre.
func shouldAutoInstall(newer bool, latest, failed string) bool {
	return newer && latest != "" && latest != failed && failed != unknownTag
}

// readMarker renvoie la première ligne d'un marqueur, "" s'il n'existe pas.
func readMarker(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	line, _, _ := strings.Cut(string(b), "\n")
	return strings.TrimSpace(line)
}

// AwaitIdle bloque tant que busy() est vrai, en revérifiant chaque minute.
// Sert à ne jamais rebooter pour une mise à jour pendant que l'alarme est
// armée : le téléchargement dure, l'alarme a pu être armée entre-temps.
func AwaitIdle(busy func() bool, logger *slog.Logger) {
	logged := false
	for busy() {
		if !logged {
			logger.Info("OTA: alarm not disarmed, reboot postponed")
			logged = true
		}
		time.Sleep(time.Minute)
	}
}
