package etcd

import (
	"crypto/tls"
	"net/http"
)

// insecureTransport returns an http.Transport that skips TLS certificate
// verification. Used only when the user opts into Insecure — never default.
func insecureTransport() http.RoundTripper {
	return &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
	}
}
