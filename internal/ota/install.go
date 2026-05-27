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

	// BootTargetPath = chemin du script de boot (sur /data) à mettre à
	// jour en même temps que le binaire.
	BootTargetPath string
	// BootBackupPath = où on garde l'ancien boot.sh pour rollback (sur
	// /data : c'est petit, ~10 KB).
	BootBackupPath string
	// BootValidator est appelé sur le boot.sh téléchargé pour valider
	// qu'il est correct avant le swap. Retour non-nil = abort install.
	// Override en test pour shunter `sh -n` (qui peut manquer en CI).
	BootValidator func(path string) error
}

// DefaultInstallConfig pour la cam Qiara.
func DefaultInstallConfig() InstallConfig {
	return InstallConfig{
		StageDir:       "/media",
		TargetPath:     "/data/openqiarad",
		BackupPath:     "/media/openqiarad.old",
		BootTargetPath: "/data/boot.sh",
		BootBackupPath: "/data/boot.sh.old",
		BootValidator:  validateBootScript,
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
	binStagePath := filepath.Join(in.cfg.StageDir, "openqiarad.new")
	bootStagePath := filepath.Join(in.cfg.StageDir, "boot.sh.new")

	// 1. Download + verify binaire.
	if err := in.downloadAndVerify(ctx, tagName, AssetName, binStagePath); err != nil {
		_ = os.Remove(binStagePath)
		in.fail(fmt.Errorf("binary: %w", err))
		return
	}

	// 2. Download + verify boot.sh (optionnel : si pas dans la release on
	//    skip silencieusement pour rester compatible avec d'anciennes
	//    releases qui n'ont que le binaire). 404 sur l'asset = skip.
	bootInstalled := false
	if err := in.downloadAndVerify(ctx, tagName, BootScriptName, bootStagePath); err != nil {
		if !errors.Is(err, errAssetNotFound) {
			_ = os.Remove(binStagePath)
			_ = os.Remove(bootStagePath)
			in.fail(fmt.Errorf("boot script: %w", err))
			return
		}
		_ = os.Remove(bootStagePath)
	} else {
		// 3. Validate boot.sh syntaxe avant tout swap.
		if in.cfg.BootValidator != nil {
			if err := in.cfg.BootValidator(bootStagePath); err != nil {
				_ = os.Remove(binStagePath)
				_ = os.Remove(bootStagePath)
				in.fail(fmt.Errorf("boot script validation: %w", err))
				return
			}
		}
		bootInstalled = true
	}

	// 4. Backup ancien binaire vers /media (gros, donc hors /data).
	in.setStep(StepBackup, nil)
	if err := copyFile(in.cfg.TargetPath, in.cfg.BackupPath); err != nil {
		// Pas fatal — on continue, juste pas de filet rollback binaire.
		in.setStep(StepBackup, func(s *InstallStatus) {
			s.Error = "binary backup failed (continuing): " + err.Error()
		})
	}

	// 5. Swap boot.sh atomique (cp+rename). Si ça échoue après, on n'a
	//    pas encore touché au binaire — abort propre.
	if bootInstalled {
		if err := swapFile(bootStagePath, in.cfg.BootTargetPath, in.cfg.BootBackupPath); err != nil {
			_ = os.Remove(binStagePath)
			_ = os.Remove(bootStagePath)
			in.fail(fmt.Errorf("swap boot script: %w", err))
			return
		}
	}

	// 6. Schedule swap binaire via script détaché. Le binaire en cours
	//    d'exécution tient un FD ouvert sur /data/openqiarad : sur
	//    /data ~plein un cp direct échoue, on délègue à un script qui
	//    attend notre mort puis cp /media → /data et relance.
	//
	//    L'Installer s'arrête à "binaire ready sur /media" (StepDone) ;
	//    onComplete prend le relais (main.go injecte un callback qui
	//    lance le script puis SIGTERM).
	in.setStep(StepSwap, func(s *InstallStatus) {
		s.StagedAt = binStagePath
	})
	in.mu.Lock()
	in.status.Step = StepDone
	in.running = false
	in.mu.Unlock()

	// Pause pour laisser l'UI poller le status une dernière fois.
	time.Sleep(500 * time.Millisecond)
	in.onComplete()
}

// downloadAndVerify télécharge un asset depuis la release et vérifie
// son SHA256 contre SHA256SUMS. Retourne errAssetNotFound si l'asset
// n'existe pas dans cette release (404).
func (in *Installer) downloadAndVerify(ctx context.Context, tagName, assetName, stagePath string) error {
	url := fmt.Sprintf("https://github.com/%s/releases/download/%s/%s", in.client.repo, tagName, assetName)
	if err := in.download(ctx, url, stagePath); err != nil {
		return fmt.Errorf("download %s: %w", assetName, err)
	}
	in.setStep(StepVerify, nil)
	checksumsURL := fmt.Sprintf("https://github.com/%s/releases/download/%s/%s", in.client.repo, tagName, ChecksumsName)
	expected, err := in.fetchChecksum(ctx, checksumsURL, assetName)
	if err != nil {
		return fmt.Errorf("fetch checksum for %s: %w", assetName, err)
	}
	actual, err := sha256OfFile(stagePath)
	if err != nil {
		return fmt.Errorf("hash staged file: %w", err)
	}
	if !strings.EqualFold(actual, expected) {
		return fmt.Errorf("checksum mismatch for %s: got %s, expected %s", assetName, actual, expected)
	}
	return nil
}

// swapFile fait : backup target → backupPath, puis rename src → target.
// rename est atomique sur le même FS. On accepte que target n'existe
// pas encore (premier déploiement) — pas de backup dans ce cas.
func swapFile(src, target, backupPath string) error {
	if _, err := os.Stat(target); err == nil {
		// Backup via copyFile (pas rename : target et backup peuvent
		// être sur des FS différents).
		if err := copyFile(target, backupPath); err != nil {
			return fmt.Errorf("backup %s → %s: %w", target, backupPath, err)
		}
	}
	// Si src et target sont sur des FS différents (typique : /media → /data),
	// os.Rename retourne EXDEV → on tombe sur copyFile + remove.
	if err := os.Rename(src, target); err != nil {
		if err := copyFile(src, target); err != nil {
			return fmt.Errorf("copy %s → %s: %w", src, target, err)
		}
		_ = os.Remove(src)
	}
	return nil
}

// errAssetNotFound signale qu'un asset optionnel n'existe pas dans la
// release (skip silencieux côté caller).
var errAssetNotFound = errors.New("asset not found in release")

// download écrit l'URL dans dst et met à jour le status (progress %).
// Utilise downloadHTTP (timeout 10 min) plutôt que http (15s) car les
// binaires de release font ~10 MB et la cam download à ~100 KB/s.
func (in *Installer) download(ctx context.Context, url, dst string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := in.client.downloadHTTP.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusNotFound {
		return errAssetNotFound
	}
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

