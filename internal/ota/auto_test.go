package ota

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

func TestShouldAutoInstall(t *testing.T) {
	cases := []struct {
		name   string
		newer  bool
		latest string
		failed string
		want   bool
	}{
		{"newer release", true, "v0.1.0-rc.22", "", true},
		{"up to date or unknown", false, "v0.1.0-rc.22", "", false},
		{"no release", true, "", "", false},
		{"release rolled back", true, "v0.1.0-rc.22", "v0.1.0-rc.22", false},
		{"older failure", true, "v0.1.0-rc.23", "v0.1.0-rc.22", true},
		{"failure with a lost tag", true, "v0.1.0-rc.23", "unknown", false},
	}
	for _, c := range cases {
		if got := shouldAutoInstall(c.newer, c.latest, c.failed); got != c.want {
			t.Errorf("%s: got %v, want %v", c.name, got, c.want)
		}
	}
}

func TestListedAfterLatest(t *testing.T) {
	list := []Release{{TagName: "v0.1.0-rc.22"}, {TagName: "v0.1.0-rc.21"}, {TagName: "v0.1.0-rc.20"}}
	cases := map[string]bool{
		"v0.1.0-rc.21 (abc1234, 2026-10-20)": true,
		"v0.1.0-rc.20":                       true,
		"v0.1.0-rc.22":                       false, // à jour
		"v0.1.0-rc.23":                       false, // dernière release supprimée : pas de downgrade
		"dev":                                false,
	}
	for cur, want := range cases {
		if got := listedAfterLatest(cur, list); got != want {
			t.Errorf("%s: got %v, want %v", cur, got, want)
		}
	}
}

func TestReadMarker(t *testing.T) {
	dir := t.TempDir()
	if got := readMarker(filepath.Join(dir, "absent")); got != "" {
		t.Errorf("absent: got %q", got)
	}
	p := filepath.Join(dir, "trial")
	if err := os.WriteFile(p, []byte("v0.1.0-rc.22\nx\nx\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if got := readMarker(p); got != "v0.1.0-rc.22" {
		t.Errorf("trial: got %q", got)
	}
}

func TestConfirmTrialAfterPeriodOnFirstHealthyCheck(t *testing.T) {
	p := filepath.Join(t.TempDir(), "ota_trial")
	if err := os.WriteFile(p, []byte("v1\n"), 0644); err != nil {
		t.Fatal(err)
	}
	// Santé KO aux 3 premiers contrôles, après la période : on attend le 1er OK.
	var calls atomic.Int32
	health := func() error {
		if calls.Add(1) <= 3 {
			return errors.New("radio silent")
		}
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	start := time.Now()
	confirmTrial(ctx, p, health, 50*time.Millisecond, 10*time.Millisecond, slog.Default())
	if _, err := os.Stat(p); !os.IsNotExist(err) {
		t.Fatalf("trial marker still there: %v", err)
	}
	if d := time.Since(start); d < 70*time.Millisecond {
		t.Errorf("confirmed after %v: before the period plus the failed checks", d)
	}
	if n := calls.Load(); n != 4 {
		t.Errorf("health checked %d times, want 4", n)
	}
}

func TestConfirmTrialNeverWhileUnhealthy(t *testing.T) {
	p := filepath.Join(t.TempDir(), "ota_trial")
	if err := os.WriteFile(p, []byte("v1\n"), 0644); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	confirmTrial(ctx, p, func() error { return errors.New("stuck") }, 10*time.Millisecond, 5*time.Millisecond, slog.Default())
	if _, err := os.Stat(p); err != nil {
		t.Fatalf("trial confirmed while unhealthy: %v", err)
	}
}

func TestStartRefusedWhilePendingOrTrial(t *testing.T) {
	for _, name := range []string{"ota_pending", "ota_trial"} {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, name), []byte("x\n"), 0644); err != nil {
			t.Fatal(err)
		}
		in := NewInstaller(nil, InstallConfig{
			StageDir:    dir,
			PendingPath: filepath.Join(dir, "ota_pending"),
			TrialPath:   filepath.Join(dir, "ota_trial"),
		}, nil)
		if err := in.Start(context.Background(), "v1"); !errors.Is(err, ErrBusy) {
			t.Errorf("%s: got %v, want ErrBusy", name, err)
		}
	}
}
