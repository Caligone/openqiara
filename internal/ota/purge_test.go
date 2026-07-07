package ota

import (
	"os"
	"path/filepath"
	"sort"
	"testing"
)

func TestPurgeStaleBinaries(t *testing.T) {
	tmp := t.TempDir()
	stage := filepath.Join(tmp, "media")
	if err := os.MkdirAll(stage, 0755); err != nil {
		t.Fatal(err)
	}

	// The current binary lives on StageDir; TargetPath is a symlink to it
	// (mirrors the camera: /data/openqiarad → /media/openqiarad).
	current := filepath.Join(stage, "openqiarad")
	target := filepath.Join(tmp, "data", "openqiarad")
	_ = os.MkdirAll(filepath.Dir(target), 0755)

	files := []string{
		"openqiarad",           // current — keep (symlink target)
		"openqiarad.old",       // backup — keep
		"openqiarad.new",       // orphan staged — purge
		"openqiarad.bak-rtsp",  // manual leftover — purge
		"openqiarad.pre-rtsp",  // manual leftover — purge
		"openqiarad_new",       // manual leftover — purge
		"boot.sh",              // not an openqiarad* file — keep
		"chunk_aa",             // unrelated — keep
	}
	for _, f := range files {
		if err := os.WriteFile(filepath.Join(stage, f), []byte("x"), 0755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(current, target); err != nil {
		t.Fatal(err)
	}

	in := NewInstaller(nil, InstallConfig{
		StageDir:   stage,
		TargetPath: target,
		BackupPath: filepath.Join(stage, "openqiarad.old"),
	}, nil)
	in.purgeStaleBinaries()

	got := listNames(t, stage)
	want := []string{"boot.sh", "chunk_aa", "openqiarad", "openqiarad.old"}
	if !equalStrings(got, want) {
		t.Errorf("after purge:\n got:  %v\n want: %v", got, want)
	}
}

func listNames(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	sort.Strings(names)
	return names
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
