package main

import (
	"io"
	"os"

	"gopkg.in/natefinch/lumberjack.v2"
)

// newLogWriter retourne stdout si path est vide, sinon un lumberjack
// configuré pour faire de la rotation par taille.
//
// Sur la cam Qiara /data fait 20 MB ; sans cap, openqiarad.log finit
// par saturer la partition (vu 2026-05-27). Avec lumberjack et les
// défauts (1 MB, 3 backups) on plafonne à ~4 MB, soit 20% de /data.
func newLogWriter(path string, maxMB, maxBackups int) io.Writer {
	if path == "" {
		return os.Stdout
	}
	return &lumberjack.Logger{
		Filename:   path,
		MaxSize:    maxMB,
		MaxBackups: maxBackups,
		// Compress=false : la cam a peu de CPU et les backups restent
		// dans le budget /data même non-compressés.
		// LocalTime=true pour des noms de fichiers lisibles.
		LocalTime: true,
	}
}
