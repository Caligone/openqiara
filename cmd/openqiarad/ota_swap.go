package main

import (
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
)

// launchSwapScript écrit /data/ota_swap.sh et le lance détaché. Le script
// attend la mort du parent (PID donné), copie le binaire staged vers
// /data/openqiarad, et relance le daemon avec les args originaux.
//
// Pourquoi un script externe : le binaire courant tient un FD sur
// /data/openqiarad (fichier en cours d'exécution sous Linux). Le rm/cp
// ne libère pas l'espace tant qu'on est vivant — sur /data quasi-plein
// la copie échoue avec "no space left on device". En passant par un
// script enfant qui survit à notre mort, l'espace se libère bien avant
// la copie.
func launchSwapScript(stagedPath, targetPath string, args []string, logger *slog.Logger) error {
	const scriptPath = "/data/ota_swap.sh"

	// Sérialise les args sous une forme parseable par sh. On quote
	// chaque arg avec des simples-quotes en échappant les ' internes
	// (forme "'foo'\\''bar'" pour foo'bar). Ça reste robuste pour les
	// args usuels (-web :80, -mode fbxhome).
	var quoted strings.Builder
	for _, a := range args {
		if quoted.Len() > 0 {
			quoted.WriteByte(' ')
		}
		quoted.WriteByte('\'')
		quoted.WriteString(strings.ReplaceAll(a, "'", "'\\''"))
		quoted.WriteByte('\'')
	}

	// Le script :
	// 1. attendre la mort du parent (kill -0 = test existence)
	// 2. cp le staged → target (espace est libéré maintenant)
	// 3. chmod +x
	// 4. relancer avec les args originaux, détaché, logs en append
	// 5. self-rm pour ne pas laisser de trace
	script := fmt.Sprintf(`#!/bin/sh
PARENT_PID=%d
STAGED=%q
TARGET=%q
LOG=/data/openqiarad.log

# Attendre que le parent soit mort (max 30s).
for i in $(seq 1 60); do
    kill -0 $PARENT_PID 2>/dev/null || break
    sleep 0.5
done

# Recopier le binaire staged vers /data. /data devrait avoir libéré
# l'espace de l'ancien binaire dès la mort du parent.
rm -f "$TARGET"
cp "$STAGED" "$TARGET" || {
    echo "[ota_swap] cp failed" >> "$LOG"
    exit 1
}
chmod 755 "$TARGET"
rm -f "$STAGED"
echo "[ota_swap] binary swapped at $(date -Iseconds)" >> "$LOG"

# Relancer avec les args originaux, détaché.
nohup %s >> "$LOG" 2>&1 &

# Self-cleanup.
rm -f %q
`, os.Getpid(), stagedPath, targetPath, quoted.String(), scriptPath)

	if err := os.WriteFile(scriptPath, []byte(script), 0755); err != nil {
		return fmt.Errorf("write swap script: %w", err)
	}

	// Lance le script détaché (nouveau process group via Setsid).
	cmd := exec.Command("/bin/sh", scriptPath)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	// Stdin/Stdout/Stderr détachés.
	cmd.Stdin = nil
	cmd.Stdout = nil
	cmd.Stderr = nil
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start swap script: %w", err)
	}
	// Wait dans une goroutine isolée pour ne pas zombifier (mais notre
	// process va mourir avant donc init prendra le relais).
	go func() { _ = cmd.Wait() }()

	logger.Info("OTA swap script launched",
		"script", scriptPath,
		"staged", stagedPath,
		"pid", cmd.Process.Pid,
		"parent_pid_var", strconv.Itoa(os.Getpid()))
	return nil
}
