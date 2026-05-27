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
	"time"
)

// fakeGitHub mock both /releases/latest (CheckLatest) et
// /releases/download/.../<asset> (Installer.run).
type fakeGitHub struct {
	srv      *httptest.Server
	binData  []byte
	binHash  string
	tag      string
}

func newFakeGitHub(t *testing.T, tag string) *fakeGitHub {
	t.Helper()
	bin := []byte("fake-arm-binary-content-for-test")
	h := sha256.Sum256(bin)
	hash := hex.EncodeToString(h[:])

	f := &fakeGitHub{binData: bin, binHash: hash, tag: tag}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/releases"):
			body := fmt.Sprintf(`[{"tag_name":%q,"name":"r","body":"","html_url":"","prerelease":false}]`, tag)
			_, _ = w.Write([]byte(body))
		case strings.HasSuffix(r.URL.Path, "/"+AssetName):
			w.Header().Set("Content-Length", fmt.Sprintf("%d", len(bin)))
			_, _ = w.Write(bin)
		case strings.HasSuffix(r.URL.Path, "/"+ChecksumsName):
			line := fmt.Sprintf("%s  %s\n%s  scripts/sd_setup.sh\n", hash, AssetName, "deadbeef")
			_, _ = w.Write([]byte(line))
		default:
			http.NotFound(w, r)
		}
	}))
	return f
}

func (f *fakeGitHub) close() { f.srv.Close() }

// client construit un Client OTA qui tape le mock au lieu de github.com.
func (f *fakeGitHub) client() *Client {
	c := NewClient("v0.0.1", nil)
	transport := rewriteTransport{
		base:   f.srv.URL,
		target: "https://github.com",
		inner: rewriteTransport{
			base:   f.srv.URL,
			target: "https://api.github.com",
			inner:  http.DefaultTransport,
		},
	}
	c.http = &http.Client{Transport: transport, Timeout: 5 * time.Second}
	c.downloadHTTP = &http.Client{Transport: transport, Timeout: 5 * time.Second}
	return c
}

// waitFor poll Status() jusqu'à matcher cond ou timeout.
func waitFor(t *testing.T, in *Installer, cond func(InstallStatus) bool, msg string) InstallStatus {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	var s InstallStatus
	for time.Now().Before(deadline) {
		s = in.Status()
		if cond(s) {
			return s
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timeout waiting for %s — last status: %+v", msg, s)
	return s
}

func TestInstaller_DownloadVerifySwap(t *testing.T) {
	gh := newFakeGitHub(t, "v0.0.2")
	defer gh.close()

	// Setup faux filesystem
	tmp := t.TempDir()
	target := filepath.Join(tmp, "data", "openqiarad")
	_ = os.MkdirAll(filepath.Dir(target), 0755)
	if err := os.WriteFile(target, []byte("old binary"), 0755); err != nil {
		t.Fatal(err)
	}
	stage := filepath.Join(tmp, "media")
	_ = os.MkdirAll(stage, 0755)

	cfg := InstallConfig{
		StageDir:   stage,
		TargetPath: target,
		BackupPath: filepath.Join(stage, "openqiarad.old"),
	}

	c := gh.client()
	// onComplete=nil → no-op : pas de kill du process de test.
	in := NewInstaller(c, cfg, nil)

	if err := in.Start(context.Background(), "v0.0.2"); err != nil {
		t.Fatalf("Start: %v", err)
	}

	final := waitFor(t, in, func(s InstallStatus) bool {
		return s.Step == StepDone || s.Step == StepFailed
	}, "install completion")

	if final.Step == StepFailed {
		t.Fatalf("install failed: %s", final.Error)
	}

	// L'Installer ne copie PAS vers target — c'est le rôle d'onComplete
	// en prod (script shell détaché qui attend la mort du parent puis cp).
	// Donc on vérifie juste que le binaire est staged sur /media.
	staged := filepath.Join(stage, "openqiarad.new")
	got, err := os.ReadFile(staged)
	if err != nil {
		t.Fatalf("read staged: %v", err)
	}
	if string(got) != string(gh.binData) {
		t.Errorf("staged content mismatch:\ngot:  %q\nwant: %q", got, gh.binData)
	}
	if final.StagedAt != staged {
		t.Errorf("StagedAt = %q, want %q", final.StagedAt, staged)
	}

	// Le binaire de backup doit exister.
	if _, err := os.Stat(cfg.BackupPath); err != nil {
		t.Errorf("backup not created: %v", err)
	}

	// Le target original doit être intact (Installer ne touche pas à
	// /data/openqiarad — c'est onComplete qui s'en occupe).
	gotTarget, _ := os.ReadFile(target)
	if string(gotTarget) != "old binary" {
		t.Errorf("target should be intact, got %q", gotTarget)
	}
}

func TestInstaller_ChecksumMismatch(t *testing.T) {
	gh := newFakeGitHub(t, "v0.0.2")
	defer gh.close()
	// Corrompre le SHA256SUMS pour forcer un mismatch.
	gh.srv.Close()
	gh.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/"+AssetName):
			_, _ = w.Write(gh.binData)
		case strings.HasSuffix(r.URL.Path, "/"+ChecksumsName):
			_, _ = w.Write([]byte("deadbeefcafe  " + AssetName + "\n"))
		default:
			http.NotFound(w, r)
		}
	}))

	tmp := t.TempDir()
	target := filepath.Join(tmp, "data", "openqiarad")
	_ = os.MkdirAll(filepath.Dir(target), 0755)
	_ = os.WriteFile(target, []byte("old"), 0755)
	stage := filepath.Join(tmp, "media")
	_ = os.MkdirAll(stage, 0755)
	cfg := InstallConfig{StageDir: stage, TargetPath: target, BackupPath: filepath.Join(stage, "openqiarad.old")}

	c := gh.client()
	in := NewInstaller(c, cfg, nil)
	if err := in.Start(context.Background(), "v0.0.2"); err != nil {
		t.Fatalf("Start: %v", err)
	}

	final := waitFor(t, in, func(s InstallStatus) bool { return s.Step == StepFailed }, "checksum mismatch failure")
	if !strings.Contains(final.Error, "checksum") {
		t.Errorf("expected checksum error, got %q", final.Error)
	}

	// Le target doit être intact.
	got, _ := os.ReadFile(target)
	if string(got) != "old" {
		t.Errorf("target should be untouched on checksum failure, got %q", got)
	}
}

func TestInstaller_AlreadyRunning(t *testing.T) {
	gh := newFakeGitHub(t, "v0.0.2")
	defer gh.close()
	tmp := t.TempDir()
	target := filepath.Join(tmp, "data", "openqiarad")
	_ = os.MkdirAll(filepath.Dir(target), 0755)
	_ = os.WriteFile(target, []byte("old"), 0755)
	stage := filepath.Join(tmp, "media")
	_ = os.MkdirAll(stage, 0755)
	cfg := InstallConfig{StageDir: stage, TargetPath: target, BackupPath: filepath.Join(stage, "openqiarad.old")}

	c := gh.client()
	in := NewInstaller(c, cfg, nil)
	if err := in.Start(context.Background(), "v0.0.2"); err != nil {
		t.Fatalf("first Start: %v", err)
	}
	// Second Start IMMÉDIAT doit refuser.
	err := in.Start(context.Background(), "v0.0.2")
	if err != ErrAlreadyRunning {
		t.Errorf("expected ErrAlreadyRunning, got %v", err)
	}
}
