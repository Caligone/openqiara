package mqtt

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"os"
	"strings"
)

// buildTLSConfig returns a *tls.Config for the broker connection, or nil when no
// TLS option is set (paho then applies its own default once the broker scheme is
// ssl://). Invalid or unreadable certificate material is a hard error rather than
// a silent fallback to plaintext: a misconfigured alarm must fail loudly.
func buildTLSConfig(cfg Config) (*tls.Config, error) {
	if cfg.TLSCACert == "" && cfg.TLSClientCert == "" && cfg.TLSClientKey == "" && !cfg.TLSInsecure {
		return nil, nil
	}

	// paho only applies a TLS config when the broker scheme is a TLS one. A TLS
	// option set against a plaintext broker (tcp://, or no scheme) would be
	// silently ignored and the alarm would connect in clear — refuse to start
	// instead of downgrading.
	if !IsTLSScheme(cfg.Broker) {
		return nil, fmt.Errorf("mqtt: TLS options set but broker %q has no TLS scheme (ssl://, tls://, mqtts://) — refusing to connect in plaintext", cfg.Broker)
	}

	tc := &tls.Config{
		MinVersion:         tls.VersionTLS12,
		InsecureSkipVerify: cfg.TLSInsecure, //nolint:gosec // opt-in flag, documented as test-only
	}

	if cfg.TLSCACert != "" {
		pem, err := os.ReadFile(cfg.TLSCACert)
		if err != nil {
			return nil, fmt.Errorf("read mqtt tls_ca_cert: %w", err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("mqtt tls_ca_cert %q: no valid certificate found", cfg.TLSCACert)
		}
		tc.RootCAs = pool
	}

	if cfg.TLSClientCert != "" || cfg.TLSClientKey != "" {
		if cfg.TLSClientCert == "" || cfg.TLSClientKey == "" {
			return nil, fmt.Errorf("mqtt mutual TLS needs both tls_client_cert and tls_client_key")
		}
		pair, err := tls.LoadX509KeyPair(cfg.TLSClientCert, cfg.TLSClientKey)
		if err != nil {
			return nil, fmt.Errorf("load mqtt client cert/key: %w", err)
		}
		tc.Certificates = []tls.Certificate{pair}
	}

	return tc, nil
}

// IsTLSScheme reports whether the broker URL uses a scheme for which paho
// establishes a TLS connection (and thus honours the tls.Config). Mirrors the
// schemes paho recognises so a valid TLS broker is never rejected here.
func IsTLSScheme(broker string) bool {
	switch {
	case strings.HasPrefix(broker, "ssl://"),
		strings.HasPrefix(broker, "tls://"),
		strings.HasPrefix(broker, "mqtts://"),
		strings.HasPrefix(broker, "mqtt+ssl://"),
		strings.HasPrefix(broker, "tcps://"),
		strings.HasPrefix(broker, "wss://"):
		return true
	default:
		return false
	}
}
