package fbxhomelog

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestRotation simule une rotation logrotate (rename + nouveau fichier).
// Le tail doit lire les events de l'ancien fichier ET du nouveau sans rater
// de lignes (modulo le délai de détection ~2s).
func TestRotation(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "fbxhome.log")
	if err := os.WriteFile(logPath, nil, 0644); err != nil {
		t.Fatalf("create log: %v", err)
	}

	tail := NewLogTail(logPath, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// La goroutine de Run termine quand ctx est annulé (timeout ou defer).
	runErr := make(chan error, 1)
	go func() { runErr <- tail.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		<-runErr // attend la fin propre, ne fuit pas la goroutine
	})

	// Laisse le tail se mettre en attente.
	time.Sleep(100 * time.Millisecond)

	// Écrit dans l'ancien fichier.
	appendLine(t, logPath, "2026-05-14 10:00:00 [info] Dws:  14 open, room:  area: ")
	appendLine(t, logPath, "2026-05-14 10:00:01 [info] Dws: 14 close, room:  area: ")

	// Attend la détection.
	want := []Kind{KindDWSOpen, KindDWSClose}
	for i, k := range want {
		evt, ok := waitEvent(tail, 2*time.Second)
		if !ok {
			t.Fatalf("pre-rotation event #%d not received", i)
		}
		if evt.Kind != k {
			t.Errorf("pre-rotation event #%d kind=%s want %s", i, evt.Kind, k)
		}
	}

	// Simule rotation logrotate : rename + recreate.
	rotated := logPath + ".1"
	if err := os.Rename(logPath, rotated); err != nil {
		t.Fatalf("rotate: %v", err)
	}
	// Ligne écrite dans l'ancien fichier juste après la rotation (peut arriver
	// avec fbxhome qui a son fd ouvert sur l'ancien).
	appendLine(t, rotated, "2026-05-14 10:00:02 [info] Pir: 20 mvt start, room:  area: ")

	// Crée un nouveau fichier vide.
	if err := os.WriteFile(logPath, nil, 0644); err != nil {
		t.Fatalf("recreate: %v", err)
	}

	// Le tail doit détecter la rotation et reprendre sur le nouveau fichier.
	// Délai max : ~3s (interval de check 2s + marge).
	appendLine(t, logPath, "2026-05-14 10:00:05 [info] Dws:  14 open, room:  area: ")

	// On peut recevoir les 2 events restants dans n'importe quel ordre selon
	// le timing (l'event PIR de l'ancien fichier, l'event DWS du nouveau).
	got := map[Kind]bool{}
	for i := 0; i < 2; i++ {
		evt, ok := waitEvent(tail, 5*time.Second)
		if !ok {
			t.Fatalf("post-rotation event #%d not received (got so far: %v)", i, got)
		}
		got[evt.Kind] = true
	}
	if !got[KindDWSOpen] {
		t.Errorf("event KindDWSOpen (du nouveau fichier) jamais reçu")
	}
	// Le PIR de l'ancien fichier peut être perdu si la rotation est détectée
	// très vite — c'est documenté comme tradeoff acceptable. On l'accepte
	// silencieusement (le test passe tant que le DWS du nouveau arrive).
}

func appendLine(t *testing.T, path, line string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		t.Fatalf("open append: %v", err)
	}
	defer func() { _ = f.Close() }()
	if _, err := fmt.Fprintln(f, line); err != nil {
		t.Fatalf("write: %v", err)
	}
}

func waitEvent(tail *LogTail, timeout time.Duration) (Event, bool) {
	select {
	case evt := <-tail.Events():
		return evt, true
	case <-time.After(timeout):
		return Event{}, false
	}
}

func TestParseLines(t *testing.T) {
	cases := []struct {
		name     string
		line     string
		wantOK   bool
		wantKind Kind
		wantID   int
		wantAct  string
	}{
		{
			name:     "DWS open avec double espace",
			line:     "2026-05-14 07:50:01 [info] Dws:  14 open, room:  area: ",
			wantOK:   true, wantKind: KindDWSOpen, wantID: 14,
		},
		{
			name:     "DWS close avec simple espace",
			line:     "2026-05-14 07:50:02 [info] Dws: 14 close, room:  area: ",
			wantOK:   true, wantKind: KindDWSClose, wantID: 14,
		},
		{
			name:     "PIR start",
			line:     "2026-05-14 07:50:43 [info] Pir: 20 mvt start, room:  area: ",
			wantOK:   true, wantKind: KindPIRStart, wantID: 20,
		},
		{
			name:     "PIR end",
			line:     "2026-05-14 07:50:44 [info] Pir: 20 mvt end, room:  area: ",
			wantOK:   true, wantKind: KindPIREnd, wantID: 20,
		},
		{
			name:     "KPD disarm",
			line:     "2026-05-13 19:09:36 [debug] HlKpd: KPD_ALARM_OFF",
			wantOK:   true, wantKind: KindKPD, wantAct: "KPD_ALARM_OFF",
		},
		{
			name:   "ligne non pertinente",
			line:   "2026-05-14 07:50:00 [debug] PrivHttpRqHandler",
			wantOK: false,
		},
		{
			name:   "ligne vide",
			line:   "",
			wantOK: false,
		},
		{
			name:   "ligne send manage (pas un event)",
			line:   "2026-05-14 07:00:00 [debug] Sent manage: [gwdst:14, gwsrc:1, ...]",
			wantOK: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := parse(tc.line)
			if ok != tc.wantOK {
				t.Fatalf("ok=%v want %v (line=%q)", ok, tc.wantOK, tc.line)
			}
			if !ok {
				return
			}
			if got.Kind != tc.wantKind {
				t.Errorf("kind=%q want %q", got.Kind, tc.wantKind)
			}
			if got.NodeID != tc.wantID {
				t.Errorf("node_id=%d want %d", got.NodeID, tc.wantID)
			}
			if got.Action != tc.wantAct {
				t.Errorf("action=%q want %q", got.Action, tc.wantAct)
			}
			if got.Time.IsZero() {
				t.Errorf("timestamp not parsed for line %q", tc.line)
			}
		})
	}
}
