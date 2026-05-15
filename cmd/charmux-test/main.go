package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/caligone/openqiara/internal/charmux"
	"github.com/caligone/openqiara/internal/domus"
)

func main() {
	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelDebug}))
	client := charmux.New(charmux.WithLogger(logger), charmux.WithReadTimeout(5*time.Second))

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	fmt.Println("Connecting...")
	if err := client.Connect(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "Connect failed: %v\n", err)
		os.Exit(1)
	}
	defer func() { _ = client.Close() }()

	info, _ := client.GetInfo(ctx)
	if info != nil {
		fmt.Printf("MCU: NetworkID=%d, Addr=%d, State=0x%02x\n", info.NetworkID, info.Address, info.State)
	}

	keys, err := domus.LoadVendorKeys("/etc/hl/vendors.keys")
	if err != nil {
		fmt.Printf("Keys error: %v\n", err)
		os.Exit(1)
	}

	// Find cofidur1 key
	var cofidur domus.VendorKey
	for _, k := range keys {
		if k.Name == "cofidur1" {
			cofidur = k
			break
		}
	}
	fmt.Printf("Using vendor key: %s\n", cofidur.Name)

	// Step 1: Write vendor key to config zone 1
	fmt.Println("\n=== Step 1: Write vendor key via 0x01 ===")
	if err := client.WriteConfigZone1(ctx, cofidur.Key); err != nil {
		fmt.Printf("WriteConfigZone1 failed: %v\n", err)
	} else {
		fmt.Println("Vendor key written OK")
	}

	// Step 2: Try internal pairing (0x13)
	fmt.Println("\n=== Step 2: Internal pairing (0x13) ===")
	fmt.Println("PUT SENSOR IN PAIRING MODE NOW!")
	resp, err := client.StartPairingInternal(ctx)
	if err != nil {
		fmt.Printf("StartPairingInternal failed: %v\n", err)
	} else {
		fmt.Printf("0x13 response: %x\n", resp)
	}

	// Step 3: Listen for events
	fmt.Println("\n=== Listening 60s for pairing events ===")
	timer := time.After(60 * time.Second)
	for {
		select {
		case evt, ok := <-client.Events():
			if !ok {
				return
			}
			fmt.Printf("EVENT ch=%d len=%d: %x\n", evt.Channel, len(evt.Data), evt.Data)
		case <-timer:
			fmt.Println("Timeout.")

			// Check nodes
			fmt.Println("\n=== GetNodes after pairing ===")
			nodes, err := client.GetNodes(ctx)
			if err != nil {
				fmt.Printf("GetNodes: %v\n", err)
			} else {
				fmt.Printf("Nodes: %d bytes\n", len(nodes))
				for i := 0; i < len(nodes); i += 9 {
					end := i + 9
					if end > len(nodes) { end = len(nodes) }
					fmt.Printf("  [%2d]: ", i)
					for _, b := range nodes[i:end] { fmt.Printf("%02x ", b) }
					fmt.Println()
				}
			}
			return
		}
	}
}
