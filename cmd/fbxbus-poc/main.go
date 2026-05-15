// PoC : se connecte au bus, s'abonne, log tout dans /tmp/fbxbus-poc-XXXX.log
// pendant 60s. Garde stdout/stderr propres pour visibilité.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/caligone/openqiara/internal/fbxbus"
)

type syncWriter struct{ f *os.File }

func (s *syncWriter) Write(p []byte) (int, error) {
	n, err := s.f.Write(p)
	_ = s.f.Sync()
	return n, err
}

func main() {
	logfile := fmt.Sprintf("/tmp/fbxbus-poc-%d.log", os.Getpid())
	f, err := os.Create(logfile)
	if err != nil {
		fmt.Fprintf(os.Stderr, "cant create log: %v\n", err)
		os.Exit(1)
	}
	defer func() { _ = f.Close() }()

	// Sanity check + Sync writer.
	_, _ = fmt.Fprintln(f, "PoC starting...")
	_ = f.Sync()
	logger := slog.New(slog.NewTextHandler(&syncWriter{f}, &slog.HandlerOptions{Level: slog.LevelDebug}))
	fmt.Printf("[poc %d] logging to %s\n", os.Getpid(), logfile)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	c, err := fbxbus.Dial(ctx, logger)
	if err != nil {
		fmt.Fprintf(os.Stderr, "dial: %v\n", err)
		os.Exit(1)
	}
	defer func() { _ = c.Close() }()

	fmt.Printf("[poc %d] connected: %s\n", os.Getpid(), c.Name())

	if err := c.Subscribe("fbxhome", "alarm_status_changed"); err != nil {
		fmt.Fprintf(os.Stderr, "subscribe alarm: %v\n", err)
	}
	if err := c.Subscribe("hl_event_collectd", "new_event"); err != nil {
		fmt.Fprintf(os.Stderr, "subscribe event: %v\n", err)
	}
	if err := c.Subscribe("hlconnman", "wifi_status_changed"); err != nil {
		fmt.Fprintf(os.Stderr, "subscribe wifi: %v\n", err)
	}

	go func() {
		if err := c.Listen(ctx); err != nil {
			fmt.Fprintf(os.Stderr, "[poc] listen ended: %v\n", err)
		}
	}()

	fmt.Printf("[poc %d] listening 60s...\n", os.Getpid())
	timeout := time.After(60 * time.Second)
	signalCount := 0
	for {
		select {
		case sig, ok := <-c.Signals():
			if !ok {
				fmt.Printf("[poc] signals channel closed (received %d)\n", signalCount)
				return
			}
			signalCount++
			fmt.Printf("[poc SIGNAL] path=%s member=%s sender=%s body_len=%d\n",
				sig.Path, sig.Member, sig.Sender, len(sig.Body))
		case <-timeout:
			fmt.Printf("[poc] 60s elapsed, received %d signals total\n", signalCount)
			return
		}
	}
}
