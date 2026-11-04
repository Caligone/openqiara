package alarm

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// The state file is written whole or not at all: a temporary file renamed
// over the previous one, never a truncated file for Load to choke on after a
// power cut.
func TestSaveIsAtomic(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "alarm.json")

	// Leftover of a write interrupted by a power cut: ignored by Load,
	// replaced by the next save.
	if err := os.WriteFile(path+".tmp", []byte(`{"state":"arm`), 0644); err != nil {
		t.Fatal(err)
	}

	e := New(path, &fakeSiren{}, nil, nil, nil)
	t.Cleanup(e.Close)
	if err := e.Load(); err != nil {
		t.Fatalf("load with a stale temporary file: %v", err)
	}
	e.HandleCommand("arm_away", SourceRemote)
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var p persistedState
	if err := json.Unmarshal(b, &p); err != nil || p.State != StateArmedAway {
		t.Fatalf("state file = %s (%v)", b, err)
	}
	if _, err := os.Stat(path + ".tmp"); !os.IsNotExist(err) {
		t.Errorf("temporary file left behind: %v", err)
	}

	// Failed writes: the error is logged, the engine keeps its state in
	// memory and no temporary file is left. A missing directory fails at
	// the creation of the temporary file; a directory in place of the state
	// file fails at the rename, after the temporary file was written.
	blocked := filepath.Join(dir, "blocked.json")
	if err := os.Mkdir(blocked, 0755); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{filepath.Join(dir, "missing", "alarm.json"), blocked} {
		e2 := New(bad, &fakeSiren{}, nil, nil, nil)
		t.Cleanup(e2.Close)
		e2.HandleCommand("arm_night", SourceRemote)
		if s := e2.Snapshot().State; s != StateArmedNight {
			t.Errorf("%s: state after a failed write = %q, want %q", bad, s, StateArmedNight)
		}
		if _, err := os.Stat(bad + ".tmp"); !os.IsNotExist(err) {
			t.Errorf("%s: temporary file left behind after a failed write: %v", bad, err)
		}
	}

	// Read back by a new engine, as after a reboot.
	e3 := New(path, &fakeSiren{}, nil, nil, nil)
	t.Cleanup(e3.Close)
	if err := e3.Load(); err != nil || e3.Snapshot().State != StateArmedAway {
		t.Fatalf("reload: %v, state %q", err, e3.Snapshot().State)
	}
}
