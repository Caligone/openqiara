package ota

import (
	"fmt"
	"os/exec"
)

// validateBootScript vérifie qu'un script shell se parse sans erreur de
// syntaxe via `sh -n` (mode no-exec). C'est un filet de sécurité contre
// un boot.sh corrompu qui rendrait la cam unbootable — un sh -n vert
// ne garantit pas que le script fait ce qu'on attend, juste qu'il
// parse. Suffisant pour éviter les erreurs grossières (typo de syntaxe,
// quote non fermée, etc.).
func validateBootScript(path string) error {
	cmd := exec.Command("/bin/sh", "-n", path)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("sh -n failed: %w (%s)", err, string(out))
	}
	return nil
}
