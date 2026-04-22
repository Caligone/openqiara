package main

import (
	"flag"
	"fmt"
	"os"
)

func main() {
	device := flag.String("device", "", "SD card device (e.g. /dev/disk4)")
	wifiSSID := flag.String("wifi-ssid", "", "WiFi network name")
	wifiPass := flag.String("wifi-pass", "", "WiFi password")
	sshKey := flag.String("ssh-key", "", "path to SSH public key file")
	password := flag.String("password", "", "admin password for web UI")
	resetNVM := flag.Bool("reset-nvm", false, "install NVM-reset MCU firmware (for cameras with corrupted NVM)")
	daemonBin := flag.String("daemon", "", "path to openqiarad ARM binary (auto-detected if not set)")
	flag.Parse()

	if *device == "" {
		fmt.Fprintln(os.Stderr, "Usage: openqiara-flash --device /dev/disk4 [options]")
		fmt.Fprintln(os.Stderr, "")
		fmt.Fprintln(os.Stderr, "Transforms a stock Qiara SD card into an OpenQiara system.")
		fmt.Fprintln(os.Stderr, "The SD card must contain the original Qiara rootfs.")
		fmt.Fprintln(os.Stderr, "")
		flag.PrintDefaults()
		os.Exit(1)
	}

	fmt.Println("OpenQiara Flash")
	fmt.Println("===============")
	fmt.Printf("  Device:    %s\n", *device)
	fmt.Printf("  WiFi:      %s\n", *wifiSSID)
	fmt.Printf("  SSH key:   %s\n", *sshKey)
	fmt.Printf("  Password:  %s\n", maskPassword(*password))
	fmt.Printf("  Reset NVM: %v\n", *resetNVM)
	fmt.Printf("  Daemon:    %s\n", *daemonBin)
	fmt.Println()

	_ = wifiPass
	_ = daemonBin

	// Non implémenté. La méthode d'install canonique est scripts/sd_setup.sh.
	// Ce binaire est conservé comme placeholder pour un futur outil unifié,
	// mais il ne fait rien aujourd'hui.
	fmt.Fprintln(os.Stderr, "openqiara-flash: not implemented yet — use scripts/sd_setup.sh instead.")
	os.Exit(2)
}

func maskPassword(p string) string {
	if p == "" {
		return "(none)"
	}
	return "****"
}
