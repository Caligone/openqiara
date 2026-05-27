package main

import (
	"os"
	"testing"

	"gopkg.in/natefinch/lumberjack.v2"
)

func TestNewLogWriterEmptyPathReturnsStdout(t *testing.T) {
	w := newLogWriter("", 1, 3)
	if w != os.Stdout {
		t.Errorf("empty path should return os.Stdout, got %T", w)
	}
}

func TestNewLogWriterFileReturnsLumberjack(t *testing.T) {
	w := newLogWriter("/tmp/openqiarad-test.log", 2, 5)
	lj, ok := w.(*lumberjack.Logger)
	if !ok {
		t.Fatalf("expected *lumberjack.Logger, got %T", w)
	}
	if lj.Filename != "/tmp/openqiarad-test.log" {
		t.Errorf("Filename mismatch: %q", lj.Filename)
	}
	if lj.MaxSize != 2 {
		t.Errorf("MaxSize = %d, want 2", lj.MaxSize)
	}
	if lj.MaxBackups != 5 {
		t.Errorf("MaxBackups = %d, want 5", lj.MaxBackups)
	}
}
