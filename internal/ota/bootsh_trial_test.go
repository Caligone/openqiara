package ota

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Le bloc OTA de camera_boot.sh (entre les marqueurs « ota begin/end »),
// joué dans des répertoires temporaires à la place de /data et /media.
// reboot est remplacé par une fonction qui le note.

func otaBlock(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile("../../scripts/camera_boot.sh")
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	i := strings.Index(s, "# --- ota begin")
	j := strings.Index(s, "# --- ota end")
	if i < 0 || j < i {
		t.Fatal("ota markers not found in camera_boot.sh")
	}
	return s[i:j]
}

type otaFS struct {
	t           *testing.T
	data, media string
	block       string
}

func newOtaFS(t *testing.T) *otaFS {
	f := &otaFS{t: t, data: t.TempDir(), media: t.TempDir(), block: otaBlock(t)}
	f.write(f.data, "openqiarad", "NEW-BINARY")
	f.write(f.media, "openqiarad.old", "OLD")
	f.write(f.data, "boot.sh", "new boot")
	f.write(f.data, "boot.sh.old", "old boot")
	return f
}

func (f *otaFS) write(dir, name, content string) {
	f.t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0644); err != nil {
		f.t.Fatal(err)
	}
}

func (f *otaFS) read(dir, name string) string {
	b, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		return "<absent>"
	}
	return string(b)
}

// run joue cmds après le bloc OTA.
func (f *otaFS) run(cmds string) {
	f.t.Helper()
	script := "reboot() { echo x >> \"$OTA_DATA/rebooted\"; }\nsleep() { :; }\n" + f.block + cmds
	cmd := exec.Command("/bin/sh", "-c", script)
	cmd.Env = append(os.Environ(), "OTA_DATA="+f.data, "OTA_MEDIA="+f.media)
	if out, err := cmd.CombinedOutput(); err != nil {
		f.t.Fatalf("sh: %v\n%s", err, out)
	}
}

func (f *otaFS) stage(binary string) {
	f.write(f.media, "openqiarad.new", binary)
	f.write(f.data, "ota_pending", filepath.Join(f.media, "openqiarad.new")+"\n")
	f.write(f.data, "ota_pending_tag", "v2\n")
}

func TestBootSwapStartsTrial(t *testing.T) {
	f := newOtaFS(t)
	f.write(f.data, "openqiarad", "OLD")
	f.stage("NEW-BINARY")
	f.run("ota_swap\n")
	if got := f.read(f.data, "openqiarad"); got != "NEW-BINARY" {
		t.Errorf("binary = %q", got)
	}
	if got := f.read(f.data, "ota_trial"); got != "v2\n" {
		t.Errorf("trial = %q", got)
	}
	for _, name := range []string{"ota_pending", "ota_pending_tag"} {
		if f.read(f.data, name) != "<absent>" {
			t.Errorf("%s left behind", name)
		}
	}
	if f.read(f.media, "openqiarad.new") != "<absent>" {
		t.Error("staged binary left behind")
	}
}

func TestBootFailedSwapRestoresBinaryAndBootScript(t *testing.T) {
	f := newOtaFS(t)
	f.write(f.data, "openqiarad", "OLD")
	f.stage("NEW-BINARY")
	// La copie du binaire stagé échoue (/data plein).
	f.run("cp() { case \"$1\" in *openqiarad.new) return 1 ;; esac; command cp \"$@\"; }\nota_swap\n")
	checks := []struct{ dir, name, want string }{
		{f.data, "openqiarad", "OLD"},
		{f.data, "boot.sh", "old boot"},
		{f.data, "ota_failed", "v2\n"},
		{f.data, "ota_trial", "<absent>"},
		{f.data, "ota_pending", "<absent>"},
	}
	for _, c := range checks {
		if got := f.read(c.dir, c.name); got != c.want {
			t.Errorf("%s = %q, want %q", c.name, got, c.want)
		}
	}
}

func TestBootOrphanTagRemoved(t *testing.T) {
	f := newOtaFS(t)
	f.write(f.data, "ota_pending_tag", "v1\n")
	f.run("ota_swap\n")
	if f.read(f.data, "ota_pending_tag") != "<absent>" {
		t.Error("orphan tag left behind")
	}
}

func TestBootTrialRollsBackAfterFourStarts(t *testing.T) {
	f := newOtaFS(t)
	f.write(f.data, "ota_trial", "v2\n")
	// 4 démarrages : toujours en essai.
	f.run("ota_launch\nota_launch\nota_launch\nota_launch\n")
	if got := f.read(f.data, "openqiarad"); got != "NEW-BINARY" {
		t.Fatalf("rolled back too early, binary = %q", got)
	}
	// 5e : retour arrière, ancien boot.sh, tag noté, reboot.
	f.run("ota_launch\n")
	checks := []struct{ dir, name, want string }{
		{f.data, "openqiarad", "OLD"},
		{f.data, "boot.sh", "old boot"},
		{f.data, "ota_failed", "v2\n"},
		{f.data, "ota_trial", "<absent>"},
		{f.data, "rebooted", "x\n"},
	}
	for _, c := range checks {
		if got := f.read(c.dir, c.name); got != c.want {
			t.Errorf("%s = %q, want %q", c.name, got, c.want)
		}
	}
}

func TestBootConfirmedTrialIsLeftAlone(t *testing.T) {
	f := newOtaFS(t)
	f.run("ota_launch\nota_launch\nota_launch\nota_launch\nota_launch\nota_launch\n")
	if got := f.read(f.data, "openqiarad"); got != "NEW-BINARY" {
		t.Errorf("binary = %q", got)
	}
	if f.read(f.data, "rebooted") != "<absent>" {
		t.Error("rebooted without a trial")
	}
}
