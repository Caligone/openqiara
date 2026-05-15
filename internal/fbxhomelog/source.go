// Package fbxhomelog implémente une source d'événements en temps réel basée
// sur le tail de /var/log/fbxhome.log. Implémentation transitoire en
// attendant la voie push native de fbxhome (cf. session 2026-05-14).
//
// L'API expose des [Event] sur un channel. Quand la voie native sera connue,
// une autre source implémentant le même contrat la remplacera côté main.
package fbxhomelog

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

// Constantes de tail interne.
const (
	defaultChanBuffer  = 128
	readPollInterval   = 200 * time.Millisecond
	rotationCheckEvery = 10 * 200 * time.Millisecond // ~2s
	reopenBackoff      = 2 * time.Second
)

// Kind est le type d'évènement détecté.
type Kind string

const (
	KindDWSOpen  Kind = "dws_open"
	KindDWSClose Kind = "dws_close"
	KindPIRStart Kind = "pir_start" // motion start
	KindPIREnd   Kind = "pir_end"   // motion end
	KindKPD      Kind = "kpd"       // action keypad (DAY/NIGHT/OFF/EMERGENCY)
)

// Event représente un évènement parsé depuis le log.
//
// Action est renseigné pour les events KPD (ex: "KPD_DAY_ALARM"). NodeID est
// renseigné pour DWS/PIR ; pour KPD il reste à 0 car le node ID n'est pas
// présent sur la ligne — le caller doit résoudre via sa config (un seul KPD
// en pratique).
type Event struct {
	Kind   Kind
	NodeID int
	Action string
	Time   time.Time
}

// LogTail tail un fichier ligne par ligne et publie les Event sur un channel.
//
// Robuste à la rotation logrotate : détection via inode change, drain du
// reste de l'ancien descripteur, puis réouverture du nouveau fichier.
//
// Cycle de vie :
//
//	tail := NewLogTail(path, logger)
//	go func() { _ = tail.Run(ctx) }()
//	for evt := range tail.Events() { ... }
//
// Le channel Events() est fermé quand Run retourne (ctx annulé ou erreur
// irrécupérable). Run est bloquant ; le caller décide où le faire tourner.
type LogTail struct {
	path   string
	logger *slog.Logger
	out    chan Event

	// dropped compte les events perdus parce que le consommateur ne lit pas
	// assez vite (buffer plein). Exposé via Dropped() pour métriques.
	dropped atomic.Uint64

	// runStarted garantit qu'on n'appelle Run qu'une fois. Run retourne une
	// erreur si appelé deux fois.
	runStarted atomic.Bool
}

// NewLogTail crée un LogTail prêt à Run.
// Path typique sur la caméra : "/var/log/fbxhome.log".
func NewLogTail(path string, logger *slog.Logger) *LogTail {
	if logger == nil {
		logger = slog.Default()
	}
	return &LogTail{
		path:   path,
		logger: logger.With("component", "fbxhomelog"),
		out:    make(chan Event, defaultChanBuffer),
	}
}

// Events renvoie le channel d'events. Le consommateur doit lire en continu
// sinon des events sont droppés (avec compteur incrémenté).
//
// Le channel est fermé quand Run retourne.
func (t *LogTail) Events() <-chan Event { return t.out }

// Dropped retourne le nombre d'events perdus depuis le démarrage (consommateur
// trop lent). À sample périodiquement pour métriques.
func (t *LogTail) Dropped() uint64 { return t.dropped.Load() }

// Run lance la boucle de tail. Bloque jusqu'à annulation de ctx ou erreur
// irrécupérable. Ferme le channel Events() avant de retourner.
//
// Run doit être appelé exactement une fois ; appels suivants retournent
// errAlreadyStarted.
func (t *LogTail) Run(ctx context.Context) error {
	if !t.runStarted.CompareAndSwap(false, true) {
		return errAlreadyStarted
	}
	defer close(t.out)

	firstOpen := true
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		err := t.tailOnce(ctx, firstOpen)
		firstOpen = false
		if err != nil && !errors.Is(err, context.Canceled) {
			t.logger.Debug("tail iteration ended", "err", err)
		}
		if errors.Is(err, context.Canceled) {
			return ctx.Err()
		}
		// Backoff avant réouverture (rotation, fichier disparu…).
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(reopenBackoff):
		}
	}
}

var errAlreadyStarted = errors.New("fbxhomelog: Run already called")

// tailOnce ouvre et lit le fichier jusqu'à rotation ou ctx annulé.
// Retourne nil sur rotation détectée (pour rebouclage propre), ou une erreur
// sur problème d'I/O. ctx.Err() si annulé.
//
// firstOpen=true (premier démarrage) : seek to end pour ne pas relire
// l'historique. firstOpen=false (après rotation) : lit depuis le début du
// nouveau fichier — il est fresh, vide ou avec uniquement les lignes écrites
// depuis la rotation.
func (t *LogTail) tailOnce(ctx context.Context, firstOpen bool) error {
	f, err := os.Open(t.path)
	if err != nil {
		return fmt.Errorf("open %s: %w", t.path, err)
	}
	defer func() { _ = f.Close() }()

	startStat, err := f.Stat()
	if err != nil {
		return fmt.Errorf("stat %s: %w", t.path, err)
	}
	if firstOpen {
		if _, err := f.Seek(0, io.SeekEnd); err != nil {
			return fmt.Errorf("seek end %s: %w", t.path, err)
		}
	}

	r := bufio.NewReader(f)
	var idle int
	for {
		if err := ctx.Err(); err != nil {
			return err
		}

		line, err := r.ReadString('\n')
		if err == nil {
			idle = 0
			t.dispatch(line)
			continue
		}
		if err != io.EOF {
			return fmt.Errorf("read %s: %w", t.path, err)
		}

		// EOF : pas de nouvelle ligne. On dort en restant cancellable.
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(readPollInterval):
		}
		idle++

		// Périodiquement, on vérifie une rotation (inode change).
		if idle >= int(rotationCheckEvery/readPollInterval) {
			idle = 0
			st, statErr := os.Stat(t.path)
			if statErr == nil && !os.SameFile(startStat, st) {
				// Rotation détectée. Drain le reste de l'ancien fd avant de
				// retourner pour réouverture, sinon on rate les lignes écrites
				// juste avant la rotation.
				t.drainRemaining(r)
				t.logger.Info("log rotation detected, reopening", "path", t.path)
				return nil
			}
		}
	}
}

// dispatch parse la ligne et publie l'Event si applicable. Drop avec compteur
// si le channel est plein.
func (t *LogTail) dispatch(line string) {
	evt, ok := parse(strings.TrimRight(line, "\r\n"))
	if !ok {
		return
	}
	select {
	case t.out <- evt:
	default:
		t.dropped.Add(1)
		t.logger.Warn("event channel full, dropping",
			"kind", evt.Kind, "node", evt.NodeID, "total_dropped", t.dropped.Load())
	}
}

// drainRemaining lit les lignes restantes du reader courant après détection
// d'une rotation, pour ne pas perdre les events écrits dans le fd juste avant
// que logrotate ne renomme le fichier.
func (t *LogTail) drainRemaining(r *bufio.Reader) {
	for {
		line, err := r.ReadString('\n')
		if line != "" {
			t.dispatch(line)
		}
		if err != nil {
			return
		}
	}
}

// --- Parsing ---

var (
	reDWSOpen  = regexp.MustCompile(`Dws:\s+(\d+)\s+open\b`)
	reDWSClose = regexp.MustCompile(`Dws:\s+(\d+)\s+close\b`)
	rePIRStart = regexp.MustCompile(`Pir:\s+(\d+)\s+mvt start\b`)
	rePIREnd   = regexp.MustCompile(`Pir:\s+(\d+)\s+mvt end\b`)
	reKPD      = regexp.MustCompile(`HlKpd:\s+(KPD_\w+)`)
)

// parse extrait un Event de la ligne. Retourne ok=false si rien à matcher.
//
// Format observé (exemples) :
//
//	"2026-05-14 07:50:01 [info] Dws:  14 open, room:  area: "
//	"2026-05-14 07:50:43 [info] Pir: 20 mvt start, room:  area: "
//	"2026-05-13 19:09:36 [debug] HlKpd: KPD_ALARM_OFF"
//
// Le timestamp est extrait quand présent (préfixe "YYYY-MM-DD HH:MM:SS").
func parse(line string) (Event, bool) {
	ts := parseTimestamp(line)

	if m := reDWSOpen.FindStringSubmatch(line); m != nil {
		id, _ := strconv.Atoi(m[1])
		return Event{Kind: KindDWSOpen, NodeID: id, Time: ts}, true
	}
	if m := reDWSClose.FindStringSubmatch(line); m != nil {
		id, _ := strconv.Atoi(m[1])
		return Event{Kind: KindDWSClose, NodeID: id, Time: ts}, true
	}
	if m := rePIRStart.FindStringSubmatch(line); m != nil {
		id, _ := strconv.Atoi(m[1])
		return Event{Kind: KindPIRStart, NodeID: id, Time: ts}, true
	}
	if m := rePIREnd.FindStringSubmatch(line); m != nil {
		id, _ := strconv.Atoi(m[1])
		return Event{Kind: KindPIREnd, NodeID: id, Time: ts}, true
	}
	if m := reKPD.FindStringSubmatch(line); m != nil {
		return Event{Kind: KindKPD, Action: m[1], Time: ts}, true
	}
	return Event{}, false
}

// parseTimestamp extrait "2006-01-02 15:04:05" en préfixe.
func parseTimestamp(line string) time.Time {
	if len(line) < 19 {
		return time.Time{}
	}
	t, err := time.ParseInLocation("2006-01-02 15:04:05", line[:19], time.Local)
	if err != nil {
		return time.Time{}
	}
	return t
}
