package ota

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeGitHubWithBoot étend fakeGitHub pour servir aussi boot.sh.
// On garde la même approche : un httptest qui répond aux 3 URLs
// (binary, boot script, checksums).
type fakeGitHubWithBoot struct {
	srv         *httptest.Server
	binData     []byte
	binHash     string
	bootData    []byte
	bootHash    string
	tag         string
	bootStatus  int // override HTTP status for boot.sh (0 = OK)
	bootCorrupt bool
}

func newFakeGHWithBoot(t *testing.T, tag string, bootData []byte) *fakeGitHubWithBoot {
	t.Helper()
	bin := []byte("fake-arm-binary-content-for-test")
	binH := sha256.Sum256(bin)
	bootH := sha256.Sum256(bootData)

	f := &fakeGitHubWithBoot{
		binData:  bin,
		binHash:  hex.EncodeToString(binH[:]),
		bootData: bootData,
		bootHash: hex.EncodeToString(bootH[:]),
		tag:      tag,
	}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/releases"):
			body := fmt.Sprintf(`[{"tag_name":%q,"name":"r","body":"","html_url":"","prerelease":false}]`, tag)
			_, _ = w.Write([]byte(body))
		case strings.HasSuffix(r.URL.Path, "/"+AssetName):
			w.Header().Set("Content-Length", fmt.Sprintf("%d", len(bin)))
			_, _ = w.Write(bin)
		case strings.HasSuffix(r.URL.Path, "/"+BootScriptName):
			if f.bootStatus != 0 {
				w.WriteHeader(f.bootStatus)
				return
			}
			w.Header().Set("Content-Length", fmt.Sprintf("%d", len(bootData)))
			_, _ = w.Write(bootData)
		case strings.HasSuffix(r.URL.Path, "/"+ChecksumsName):
			checksum := f.bootHash
			if f.bootCorrupt {
				checksum = "deadbeefdeadbeef" // intentionnellement faux
			}
			line := fmt.Sprintf("%s  %s\n%s  %s\n",
				f.binHash, AssetName,
				checksum, BootScriptName)
			_, _ = w.Write([]byte(line))
		default:
			http.NotFound(w, r)
		}
	}))
	return f
}

func (f *fakeGitHubWithBoot) close() { f.srv.Close() }

func (f *fakeGitHubWithBoot) client() *Client {
	return (&fakeGitHub{srv: f.srv}).client()
}

func setupInstallCfg(t *testing.T) (cfg InstallConfig, target, bootTarget string) {
	t.Helper()
	tmp := t.TempDir()
	target = filepath.Join(tmp, "data", "openqiarad")
	_ = os.MkdirAll(filepath.Dir(target), 0755)
	_ = os.WriteFile(target, []byte("old binary"), 0755)
	bootTarget = filepath.Join(tmp, "data", "boot.sh")
	_ = os.WriteFile(bootTarget, []byte("#!/bin/sh\necho old\n"), 0755)
	stage := filepath.Join(tmp, "media")
	_ = os.MkdirAll(stage, 0755)
	cfg = InstallConfig{
		StageDir:       stage,
		TargetPath:     target,
		BackupPath:     filepath.Join(stage, "openqiarad.old"),
		BootTargetPath: bootTarget,
		BootBackupPath: bootTarget + ".old",
		BootValidator:  nil, // skip sh -n par défaut, override par cas
	}
	return cfg, target, bootTarget
}

func TestInstaller_BootScriptDeployed(t *testing.T) {
	bootContent := []byte("#!/bin/sh\necho new\n")
	gh := newFakeGHWithBoot(t, "v0.0.3", bootContent)
	defer gh.close()

	cfg, _, bootTarget := setupInstallCfg(t)

	in := NewInstaller(gh.client(), cfg, nil)
	if err := in.Start(context.Background(), "v0.0.3"); err != nil {
		t.Fatalf("Start: %v", err)
	}
	final := waitFor(t, in, func(s InstallStatus) bool {
		return s.Step == StepDone || s.Step == StepFailed
	}, "install completion")
	if final.Step == StepFailed {
		t.Fatalf("install failed: %s", final.Error)
	}

	got, err := os.ReadFile(bootTarget)
	if err != nil {
		t.Fatalf("read boot.sh: %v", err)
	}
	if string(got) != string(bootContent) {
		t.Errorf("boot.sh content mismatch:\ngot:  %q\nwant: %q", got, bootContent)
	}
	// Backup doit exister avec l'ancien contenu.
	bak, err := os.ReadFile(cfg.BootBackupPath)
	if err != nil {
		t.Fatalf("read boot.sh.old: %v", err)
	}
	if !strings.Contains(string(bak), "echo old") {
		t.Errorf("backup should contain old content, got %q", bak)
	}
}

func TestInstaller_BootScriptOptionalWhenMissing(t *testing.T) {
	bootContent := []byte("ignored")
	gh := newFakeGHWithBoot(t, "v0.0.3", bootContent)
	gh.bootStatus = http.StatusNotFound
	defer gh.close()

	cfg, _, bootTarget := setupInstallCfg(t)
	originalBoot, _ := os.ReadFile(bootTarget)

	in := NewInstaller(gh.client(), cfg, nil)
	if err := in.Start(context.Background(), "v0.0.3"); err != nil {
		t.Fatalf("Start: %v", err)
	}
	final := waitFor(t, in, func(s InstallStatus) bool {
		return s.Step == StepDone || s.Step == StepFailed
	}, "install completion")
	if final.Step != StepDone {
		t.Fatalf("expected StepDone (boot.sh missing should be tolerated), got %s err=%s",
			final.Step, final.Error)
	}

	// boot.sh ne doit PAS avoir changé.
	got, _ := os.ReadFile(bootTarget)
	if string(got) != string(originalBoot) {
		t.Errorf("boot.sh should be untouched when asset absent")
	}
}

func TestInstaller_BootScriptChecksumMismatchAbortsAll(t *testing.T) {
	bootContent := []byte("#!/bin/sh\necho new\n")
	gh := newFakeGHWithBoot(t, "v0.0.3", bootContent)
	gh.bootCorrupt = true
	defer gh.close()

	cfg, target, bootTarget := setupInstallCfg(t)
	originalBin, _ := os.ReadFile(target)
	originalBoot, _ := os.ReadFile(bootTarget)

	in := NewInstaller(gh.client(), cfg, nil)
	if err := in.Start(context.Background(), "v0.0.3"); err != nil {
		t.Fatalf("Start: %v", err)
	}
	final := waitFor(t, in, func(s InstallStatus) bool {
		return s.Step == StepFailed
	}, "checksum mismatch failure")

	if !strings.Contains(final.Error, "checksum") {
		t.Errorf("expected checksum error, got %q", final.Error)
	}
	// Tout doit être intact : ni binaire ni boot.sh touchés.
	gotBin, _ := os.ReadFile(target)
	gotBoot, _ := os.ReadFile(bootTarget)
	if string(gotBin) != string(originalBin) {
		t.Errorf("binary should be untouched on boot checksum fail")
	}
	if string(gotBoot) != string(originalBoot) {
		t.Errorf("boot.sh should be untouched on boot checksum fail")
	}
}

func TestInstaller_BootScriptValidationFailureAbortsAll(t *testing.T) {
	bootContent := []byte("#!/bin/sh\necho new\n")
	gh := newFakeGHWithBoot(t, "v0.0.3", bootContent)
	defer gh.close()

	cfg, target, bootTarget := setupInstallCfg(t)
	originalBin, _ := os.ReadFile(target)
	originalBoot, _ := os.ReadFile(bootTarget)

	// Validator qui refuse toujours.
	cfg.BootValidator = func(string) error {
		return fmt.Errorf("simulated syntax error")
	}

	in := NewInstaller(gh.client(), cfg, nil)
	if err := in.Start(context.Background(), "v0.0.3"); err != nil {
		t.Fatalf("Start: %v", err)
	}
	final := waitFor(t, in, func(s InstallStatus) bool {
		return s.Step == StepFailed
	}, "validation failure")

	if !strings.Contains(final.Error, "boot script validation") {
		t.Errorf("expected validation error, got %q", final.Error)
	}
	gotBin, _ := os.ReadFile(target)
	gotBoot, _ := os.ReadFile(bootTarget)
	if string(gotBin) != string(originalBin) {
		t.Errorf("binary should be untouched on validation fail")
	}
	if string(gotBoot) != string(originalBoot) {
		t.Errorf("boot.sh should be untouched on validation fail")
	}
}
