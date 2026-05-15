package ota

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// InstallStep décrit où en est l'install pour l'UI.
type InstallStep string

const (
	StepIdle     InstallStep = "idle"
	StepDownload InstallStep = "downloading"
	StepVerify   InstallStep = "verifying"
	StepBackup   InstallStep = "backing_up"
	StepSwap     InstallStep = "swapping"
	StepDone     InstallStep = "done"
	StepFailed   InstallStep = "failed"
)

// InstallStatus est le snapshot de l'état courant exposé par
// /api/update/status. L'UI poll ce endpoint pendant l'install.
type InstallStatus struct {
	Step       InstallStep `json:"step"`
	Version    string      `json:"version,omitempty"`     // tag visé (v0.1.0...)
	Progress   int         `json:"progress,omitempty"`    // 0..100 (download)
	Bytes      int64       `json:"bytes,omitempty"`       // bytes téléchargés
	TotalBytes int64       `json:"total_bytes,omitempty"` // taille attendue
	Error      string      `json:"error,omitempty"`
	StartedAt  string      `json:"started_at,omitempty"`
	UpdatedAt  string      `json:"updated_at,omitempty"`
	// StagedAt est le chemin sur disque du binaire prêt à être swappé
	// (ex /media/openqiarad.new). Lu par onComplete pour orchestrer le
	// swap final.
	StagedAt string `json:"-"`
}

// InstallConfig détaille les chemins disque utilisés. Override en test
// via SetPaths().
type InstallConfig struct {
	// StageDir = répertoire qui reçoit le download avant swap. La cam
	// utilise /media (~2.7 GB libres) parce que /data est trop petit
	// (20 MB total dont 14 MB pris par le binaire courant).
	StageDir string
	// TargetPath = chemin du binaire courant (sur /data).
	TargetPath string
	// BackupPath = où on garde l'ancien binaire avant swap pour rollback
	// manuel SSH si le nouveau crash au boot.
	BackupPath string
}

// DefaultInstallConfig pour la cam Qiara.
func DefaultInstallConfig() InstallConfig {
	return InstallConfig{
		StageDir:   "/media",
		TargetPath: "/data/openqiarad",
		BackupPath: "/media/openqiarad.old",
	}
}

// Installer pilote le download + swap. Une seule install à la fois.
type Installer struct {
	client *Client
	cfg    InstallConfig

	// onComplete est appelé après StepDone. En prod main.go injecte un
	// callback qui SIGTERM le process pour laisser le watchdog relancer.
	// En test on injecte un no-op pour ne pas se tuer soi-même.
	onComplete func()

	mu     sync.Mutex
	status InstallStatus
	running bool
}

// NewInstaller construit un installer lié au client GitHub.
//
// onComplete est appelé une fois le swap réussi. Passer nil = no-op
// (utilisé par les tests). En prod injecter une fonction qui kill le
// process pour que le watchdog boot.sh relance le nouveau binaire.
func NewInstaller(client *Client, cfg InstallConfig, onComplete func()) *Installer {
	if onComplete == nil {
		onComplete = func() {}
	}
	return &Installer{
		client:     client,
		cfg:        cfg,
		onComplete: onComplete,
		status:     InstallStatus{Step: StepIdle},
	}
}

// Status retourne un snapshot thread-safe.
func (in *Installer) Status() InstallStatus {
	in.mu.Lock()
	defer in.mu.Unlock()
	return in.status
}

// Start lance une install asynchrone vers le tag donné. Retourne
// ErrAlreadyRunning si une install est déjà en cours.
//
// La goroutine d'install écrit dans `in.status` au fil des étapes ;
// l'appelant (le handler HTTP) sort immédiatement avec 202 Accepted
// et l'UI poll /api/update/status pour suivre.
func (in *Installer) Start(ctx context.Context, tagName string) error {
	in.mu.Lock()
	if in.running {
		in.mu.Unlock()
		return ErrAlreadyRunning
	}
	in.running = true
	in.status = InstallStatus{
		Step:      StepDownload,
		Version:   tagName,
		StartedAt: time.Now().UTC().Format(time.RFC3339),
		UpdatedAt: time.Now().UTC().Format(time.RFC3339),
	}
	in.mu.Unlock()

	// Goroutine isolée du contexte HTTP : on ne veut pas annuler
	// l'install si le client web ferme sa connexion.
	go in.run(context.Background(), tagName)
	return nil
}

// ErrAlreadyRunning est retournée par Start quand une install tourne.
var ErrAlreadyRunning = errors.New("install already running")

func (in *Installer) setStep(step InstallStep, mut func(*InstallStatus)) {
	in.mu.Lock()
	defer in.mu.Unlock()
	in.status.Step = step
	in.status.UpdatedAt = time.Now().UTC().Format(time.RFC3339)
	if mut != nil {
		mut(&in.status)
	}
}

func (in *Installer) fail(err error) {
	in.setStep(StepFailed, func(s *InstallStatus) {
		s.Error = err.Error()
	})
	in.mu.Lock()
	in.running = false
	in.mu.Unlock()
}

func (in *Installer) run(ctx context.Context, tagName string) {
	stagePath := filepath.Join(in.cfg.StageDir, "openqiarad.new")

	// 1. Download binaire ARM.
	binURL := fmt.Sprintf("https://github.com/%s/releases/download/%s/%s", in.client.repo, tagName, AssetName)
	if err := in.download(ctx, binURL, stagePath); err != nil {
		in.fail(fmt.Errorf("download: %w", err))
		return
	}

	// 2. Verify SHA256.
	in.setStep(StepVerify, nil)
	checksumsURL := fmt.Sprintf("https://github.com/%s/releases/download/%s/%s", in.client.repo, tagName, ChecksumsName)
	expected, err := in.fetchChecksum(ctx, checksumsURL, AssetName)
	if err != nil {
		_ = os.Remove(stagePath)
		in.fail(fmt.Errorf("fetch checksum: %w", err))
		return
	}
	actual, err := sha256OfFile(stagePath)
	if err != nil {
		_ = os.Remove(stagePath)
		in.fail(fmt.Errorf("hash staged file: %w", err))
		return
	}
	if !strings.EqualFold(actual, expected) {
		_ = os.Remove(stagePath)
		in.fail(fmt.Errorf("checksum mismatch: got %s, expected %s", actual, expected))
		return
	}

	// 3. Backup. Copie de l'ancien binaire vers /media pour permettre
	//    un rollback manuel SSH si le nouveau crash.
	in.setStep(StepBackup, nil)
	if err := copyFile(in.cfg.TargetPath, in.cfg.BackupPath); err != nil {
		// Pas fatal — on continue, juste pas de filet rollback.
		// Mais on log via le status pour traçabilité.
		in.setStep(StepBackup, func(s *InstallStatus) {
			s.Error = "backup failed (continuing): " + err.Error()
		})
	}

	// 4. Swap. Le binaire en cours d'exécution tient un FD ouvert sur
	//    /data/openqiarad (Linux référence par inode) — tant qu'on est
	//    vivant, un `rm` ne libère pas l'espace disque. Sur la cam où
	//    /data est ~plein, copier le nouveau binaire dans /data avant
	//    de mourir échoue avec "no space left on device".
	//
	//    Solution : on délègue le swap à un script détaché qui attend
	//    notre mort, puis fait le cp /media → /data et relance.
	//
	//    L'Installer s'arrête à "binaire ready sur /media" (StepReady).
	//    onComplete prend le relais : main.go injecte un callback qui
	//    lance le script puis SIGTERM. Découplage utile pour les tests
	//    (onComplete=nil = on ne touche pas le système).
	in.setStep(StepSwap, func(s *InstallStatus) {
		s.StagedAt = stagePath
	})
	in.mu.Lock()
	in.status.Step = StepDone
	in.running = false
	in.mu.Unlock()

	// Pause pour laisser l'UI poller le status une dernière fois.
	time.Sleep(500 * time.Millisecond)
	in.onComplete()
}

// download écrit l'URL dans dst et met à jour le status (progress %).
func (in *Installer) download(ctx context.Context, url, dst string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := in.client.http.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("status %d", resp.StatusCode)
	}

	total := resp.ContentLength
	in.setStep(StepDownload, func(s *InstallStatus) {
		s.TotalBytes = total
	})

	f, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()

	buf := make([]byte, 64*1024)
	var written int64
	for {
		n, rerr := resp.Body.Read(buf)
		if n > 0 {
			if _, werr := f.Write(buf[:n]); werr != nil {
				return werr
			}
			written += int64(n)
			progress := 0
			if total > 0 {
				progress = int(written * 100 / total)
			}
			in.setStep(StepDownload, func(s *InstallStatus) {
				s.Bytes = written
				s.Progress = progress
			})
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			return rerr
		}
	}
	return f.Sync()
}

// fetchChecksum télécharge SHA256SUMS et retourne la ligne pour
// `filename`. Format attendu (sha256sum standard) :
//
//	<hex>  <filename>
func (in *Installer) fetchChecksum(ctx context.Context, url, filename string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	resp, err := in.client.http.Do(req)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("status %d", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	for _, line := range strings.Split(string(body), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		// Le format sha256sum utilise "  " (2 espaces) ou "*" entre hash et fichier.
		// Normaliser le nom de fichier en prenant la base.
		if filepath.Base(fields[1]) == filename {
			return fields[0], nil
		}
	}
	return "", fmt.Errorf("checksum line for %q not found", filename)
}

// sha256OfFile calcule le hash hex d'un fichier sur disque.
func sha256OfFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// copyFile crée dst à partir de src. Garde un comportement simple :
// le buffer de 64KB suffit pour un binaire de 10MB, et on synchronise
// après pour s'assurer que le contenu est sur disque avant de relancer
// le process.
func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()

	// Si dst existe et fait partie d'un FS différent on prend le
	// chemin "copy" plutôt que "rename" (cross-device error sinon).
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0755)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return err
	}
	if err := out.Sync(); err != nil {
		_ = out.Close()
		return err
	}
	return out.Close()
}

