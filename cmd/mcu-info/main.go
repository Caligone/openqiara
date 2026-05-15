package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/caligone/openqiara/internal/charmux"
)

func main() {
	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	client := charmux.New(charmux.WithLogger(logger))

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := client.Connect(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "Connect: %v\n", err)
		os.Exit(1)
	}
	defer func() { _ = client.Close() }()

	info, err := client.GetInfo(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "GetInfo: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("NetworkID=%d (0x%04x), Addr=%d, State=0x%02x, Flags=%02x%02x%02x\n",
		info.NetworkID, info.NetworkID, info.Address, info.State,
		info.Flags[0], info.Flags[1], info.Flags[2])

	nodes, err := client.GetNodes(ctx)
	if err != nil {
		fmt.Printf("GetNodes: %v\n", err)
	} else {
		fmt.Printf("Nodes: %d bytes: %x\n", len(nodes), nodes)
	}
}
