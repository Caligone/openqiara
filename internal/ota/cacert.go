package ota

import (
	"crypto/tls"
	"crypto/x509"
	_ "embed"
	"net/http"
	"time"
)

// Bundle CA Mozilla embarqué dans le binaire. La cam Qiara n'a pas de
// /etc/ssl/certs/ca-certificates.crt, donc crypto/x509 ne trouve pas
// de roots et l'appel HTTPS vers github.com échoue avec "x509:
// certificate signed by unknown authority".
//
// Source : https://curl.se/ca/cacert.pem (Mozilla CA bundle, mis à jour
// régulièrement). À rafraîchir 1× par an environ.
//
//go:embed cacert.pem
var caBundle []byte

// newHTTPClient construit un client HTTP qui utilise le bundle embarqué
// comme racines de confiance. Utilisé pour tous les appels OTA vers
// github.com / api.github.com.
func newHTTPClient(timeout time.Duration) *http.Client {
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(caBundle)
	return &http.Client{
		Timeout: timeout,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{
				RootCAs: pool,
			},
		},
	}
}
