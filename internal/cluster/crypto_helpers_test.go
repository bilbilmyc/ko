package cluster

import (
	"crypto/x509"
	"encoding/pem"
)

func pemDecode(b []byte) *pem.Block {
	block, _ := pem.Decode(b)
	return block
}

func x509Parse(der []byte) (*x509.Certificate, error) {
	return x509.ParseCertificate(der)
}
