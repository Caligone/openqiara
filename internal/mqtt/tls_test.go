package mqtt

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// writeSelfSigned creates a throwaway self-signed cert + key on disk and returns
// their paths, for exercising buildTLSConfig without a real broker.
func writeSelfSigned(t *testing.T) (certPath, keyPath string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	tmpl := x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "openqiara-test"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, &tmpl, &tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create certificate: %v", err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatalf("marshal key: %v", err)
	}
	dir := t.TempDir()
	certPath = filepath.Join(dir, "cert.pem")
	keyPath = filepath.Join(dir, "key.pem")
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	if err := os.WriteFile(certPath, certPEM, 0o600); err != nil {
		t.Fatalf("write cert: %v", err)
	}
	if err := os.WriteFile(keyPath, keyPEM, 0o600); err != nil {
		t.Fatalf("write key: %v", err)
	}
	return certPath, keyPath
}

func TestBuildTLSConfig_NoOptionsReturnsNil(t *testing.T) {
	tc, err := buildTLSConfig(Config{Broker: "tcp://broker:1883"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if tc != nil {
		t.Fatal("expected nil tls.Config when no TLS option is set")
	}
}

func TestBuildTLSConfig_CACertLoadsRootPool(t *testing.T) {
	cert, _ := writeSelfSigned(t)
	tc, err := buildTLSConfig(Config{Broker: "ssl://broker:8883", TLSCACert: cert})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if tc == nil || tc.RootCAs == nil {
		t.Fatal("expected RootCAs to be populated from the CA file")
	}
	if tc.MinVersion != tls.VersionTLS12 {
		t.Errorf("expected TLS 1.2 minimum, got %x", tc.MinVersion)
	}
}

func TestBuildTLSConfig_MissingCAFileErrors(t *testing.T) {
	if _, err := buildTLSConfig(Config{Broker: "ssl://broker:8883", TLSCACert: "/does/not/exist.pem"}); err == nil {
		t.Fatal("expected an error for a missing CA file")
	}
}

func TestBuildTLSConfig_InvalidCAContentErrors(t *testing.T) {
	p := filepath.Join(t.TempDir(), "bad.pem")
	if err := os.WriteFile(p, []byte("not a certificate"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := buildTLSConfig(Config{Broker: "ssl://broker:8883", TLSCACert: p}); err == nil {
		t.Fatal("expected an error for invalid CA content")
	}
}

func TestBuildTLSConfig_MutualTLSRequiresBoth(t *testing.T) {
	cert, _ := writeSelfSigned(t)
	if _, err := buildTLSConfig(Config{Broker: "ssl://broker:8883", TLSClientCert: cert}); err == nil {
		t.Fatal("expected an error when the client key is missing")
	}
}

func TestBuildTLSConfig_ClientCertLoads(t *testing.T) {
	cert, key := writeSelfSigned(t)
	tc, err := buildTLSConfig(Config{Broker: "ssl://broker:8883", TLSClientCert: cert, TLSClientKey: key})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if tc == nil || len(tc.Certificates) != 1 {
		t.Fatal("expected exactly one client certificate")
	}
}

func TestBuildTLSConfig_InsecureSetsSkipVerify(t *testing.T) {
	tc, err := buildTLSConfig(Config{Broker: "ssl://broker:8883", TLSInsecure: true})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if tc == nil || !tc.InsecureSkipVerify {
		t.Fatal("expected InsecureSkipVerify to be true")
	}
}

// TestBuildTLSConfig_TLSWithoutSSLSchemeErrors guards the silent-downgrade trap:
// any TLS option set against a non-ssl:// broker (paho would ignore the TLS
// config and connect in plaintext) must fail loudly instead.
func TestBuildTLSConfig_TLSWithoutSSLSchemeErrors(t *testing.T) {
	cert, key := writeSelfSigned(t)
	cases := map[string]Config{
		"ca cert":     {Broker: "tcp://broker:1883", TLSCACert: cert},
		"client pair": {Broker: "tcp://broker:1883", TLSClientCert: cert, TLSClientKey: key},
		"insecure":    {Broker: "tcp://broker:1883", TLSInsecure: true},
		"no scheme":   {Broker: "broker:1883", TLSCACert: cert},
	}
	for name, cfg := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := buildTLSConfig(cfg); err == nil {
				t.Fatal("expected an error when a TLS option is set but broker is not ssl://")
			}
		})
	}
}

// TestBuildTLSConfig_AcceptsAllTLSSchemes ensures the guard does not reject the
// other TLS schemes paho recognises (tls://, mqtts://, …).
func TestBuildTLSConfig_AcceptsAllTLSSchemes(t *testing.T) {
	cert, _ := writeSelfSigned(t)
	for _, broker := range []string{"ssl://b:8883", "tls://b:8883", "mqtts://b:8883", "mqtt+ssl://b:8883", "tcps://b:8883", "wss://b:443"} {
		t.Run(broker, func(t *testing.T) {
			if _, err := buildTLSConfig(Config{Broker: broker, TLSCACert: cert}); err != nil {
				t.Fatalf("unexpected error for TLS scheme %q: %v", broker, err)
			}
		})
	}
}
