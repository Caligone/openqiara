package camera

import (
	"bufio"
	"log/slog"
	"os"
	"strings"
	"sync"
	"time"
)

// LogWatcher tails the fbxhome log file and emits SensorEvents for KPD actions.
//
// L'ID du KPD est résolu à chaque event via kpdIDFn — ainsi un re-pair en
// runtime (qui change l'ID) ne nécessite pas de restart. Si la fonction
// retourne 0, l'event est ignoré (pas de KPD configuré).
type LogWatcher struct {
	path     string
	logger   *slog.Logger
	events   LogWatcherEventSink
	kpdIDFn  func() int
	done     chan struct{}
	once     sync.Once
	wg       sync.WaitGroup
}

// LogWatcherEventSink is a channel that accepts sensor events.
type LogWatcherEventSink chan<- SensorEvent

// NewLogWatcher creates a watcher on the given log file. kpdIDFn est appelée
// à chaque event KPD détecté pour obtenir l'ID actuel du KPD — permet de
// survivre à un re-pair sans restart.
func NewLogWatcher(logPath string, kpdIDFn func() int, events LogWatcherEventSink, logger *slog.Logger) *LogWatcher {
	if kpdIDFn == nil {
		kpdIDFn = func() int { return 0 }
	}
	return &LogWatcher{
		path:    logPath,
		kpdIDFn: kpdIDFn,
		events:  events,
		done:    make(chan struct{}),
		logger:  logger,
	}
}

// Start begins tailing the log file in a goroutine.
func (w *LogWatcher) Start() {
	w.wg.Add(1)
	go w.tailLoop()
}

// Stop stops the watcher.
func (w *LogWatcher) Stop() {
	w.once.Do(func() { close(w.done) })
	w.wg.Wait()
}

func (w *LogWatcher) tailLoop() {
	defer w.wg.Done()

	for {
		select {
		case <-w.done:
			return
		default:
		}

		w.tailFile()

		// If file disappeared or error, wait and retry
		select {
		case <-w.done:
			return
		case <-time.After(2 * time.Second):
		}
	}
}

func (w *LogWatcher) tailFile() {
	f, err := os.Open(w.path)
	if err != nil {
		return
	}
	defer func() { _ = f.Close() }()

	// Seek to end
	_, _ = f.Seek(0, 2)

	// Stat the open file to detect rotation later (inode changes when logrotate
	// renames the file and a new one takes its place).
	startStat, err := f.Stat()
	if err != nil {
		return
	}

	reader := bufio.NewReader(f)
	idle := 0
	for {
		select {
		case <-w.done:
			return
		default:
		}

		line, err := reader.ReadString('\n')
		if err != nil {
			time.Sleep(200 * time.Millisecond)
			idle++
			// Check for rotation every ~2s (10 idle ticks).
			if idle >= 10 {
				idle = 0
				if pathStat, statErr := os.Stat(w.path); statErr == nil && !os.SameFile(startStat, pathStat) {
					return // reopen in caller
				}
			}
			continue
		}
		idle = 0
		w.processLine(strings.TrimRight(line, "\n\r"))
	}
}

func (w *LogWatcher) processLine(line string) {
	if !strings.Contains(line, "HlKpd:") {
		return
	}

	var action string
	if strings.Contains(line, "KPD_ALARM_OFF") {
		action = "disarmed"
	} else if strings.Contains(line, "KPD_DAY_ALARM") {
		action = "armed_away"
	} else if strings.Contains(line, "KPD_NIGHT_ALARM") {
		action = "armed_night"
	} else if strings.Contains(line, "KPD_EMERGENCY") {
		action = "triggered"
	} else {
		return
	}

	kpdID := w.kpdIDFn()
	if kpdID == 0 {
		// Pas de KPD configuré (encore) — on log juste pour debug.
		w.logger.Debug("KPD event ignored (no KPD paired yet)", "action", action)
		return
	}

	w.logger.Info("KPD event detected", "action", action, "node_id", kpdID)

	evt := SensorEvent{
		SensorID: kpdID,
		Sensor: Sensor{
			ID:   kpdID,
			Type: "KPD",
			Open: action == "arm_away" || action == "arm_night" || action == "triggered",
		},
	}
	// Store action in the Label field (hack for now)
	evt.Sensor.Label = action

	select {
	case w.events <- evt:
	default:
		w.logger.Warn("event channel full, dropping KPD event")
	}
}
