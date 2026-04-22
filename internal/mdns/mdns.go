// Package mdns announces the openqiara web UI via mDNS so the device is
// discoverable as openqiara.local on the LAN.
//
// We use hashicorp/mdns here (and not brutella/dnssd, even though that's
// what HomeKit uses internally) because brutella/dnssd cannot cohabit
// with itself in the same process — both instances try to bind UDP/5353
// without SO_REUSEADDR. hashicorp/mdns sets the right socket options so
// it can coexist with the brutella/dnssd responder that brutella/hap
// runs from internal/publisher/homekit.go.
package mdns

import (
	"context"
	"fmt"
	"log/slog"
	"net"

	"github.com/hashicorp/mdns"
)

// Announce starts an mDNS responder advertising the openqiarad web UI on
// the local network. The service is published as `_http._tcp` with
// hostname "openqiara" so it resolves as `openqiara.local` on Bonjour /
// Avahi.
//
// Runs until ctx is cancelled. Safe to call in a goroutine. Co-exists
// with the HomeKit publisher's own mDNS announcer in the same process.
func Announce(ctx context.Context, port int, logger *slog.Logger) error {
	if logger == nil {
		logger = slog.Default()
	}

	ips := collectIPs()
	if len(ips) == 0 {
		return fmt.Errorf("mdns: no usable IPv4 address")
	}

	// Force "openqiara" as the hostname rather than os.Hostname() so the
	// device is always reachable as openqiara.local regardless of the
	// system hostname (the Qiara camera ships as "hlcam02").
	const hostname = "openqiara"

	svc, err := mdns.NewMDNSService(
		"OpenQiara",        // instance name
		"_http._tcp",       // service type
		"",                 // domain (default ".local")
		hostname+".local.", // host name (FQDN form, with trailing dot)
		port,
		ips,
		[]string{"path=/", "version=1"}, // TXT records
	)
	if err != nil {
		return fmt.Errorf("mdns: web ui service: %w", err)
	}

	var iface *net.Interface
	if i, err := net.InterfaceByName("ssv0"); err == nil {
		iface = i
	}

	srv, err := mdns.NewServer(&mdns.Config{Zone: svc, Iface: iface})
	if err != nil {
		return fmt.Errorf("mdns: start server: %w", err)
	}

	logger.Info("mDNS: announcing service",
		"host", hostname+".local",
		"port", port,
		"type", "_http._tcp",
		"ips", ips)

	<-ctx.Done()
	srv.Shutdown()
	return nil
}

// collectIPs returns the IPv4 addresses to advertise. Prefers ssv0 (the
// camera's WiFi interface) and falls back to all non-loopback IPv4
// addresses on the host.
func collectIPs() []net.IP {
	if iface, err := net.InterfaceByName("ssv0"); err == nil {
		if ips := ipv4Of(iface); len(ips) > 0 {
			return ips
		}
	}
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return nil
	}
	var ips []net.IP
	for _, a := range addrs {
		ipnet, ok := a.(*net.IPNet)
		if !ok || ipnet.IP.To4() == nil || ipnet.IP.IsLoopback() {
			continue
		}
		ips = append(ips, ipnet.IP)
	}
	return ips
}

func ipv4Of(iface *net.Interface) []net.IP {
	addrs, err := iface.Addrs()
	if err != nil {
		return nil
	}
	var ips []net.IP
	for _, a := range addrs {
		ipnet, ok := a.(*net.IPNet)
		if !ok || ipnet.IP.To4() == nil {
			continue
		}
		ips = append(ips, ipnet.IP)
	}
	return ips
}
