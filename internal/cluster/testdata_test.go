package cluster

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
)

const dummySSHKey = `-----BEGIN OPENSSH PRIVATE KEY-----
b3BlbnNzaC1rZXktdjEAAAAABG5vbmUAAAAEbm9uZQAAAAAAAAABAAAAMwAAAAtzc2gtZW
QyNTUxOQAAACBGFnl0OaE6g1Ao9jr9UJX3gW7I+UCtPKvY+3t1kKsB9AAAAJhA6NOhQOjT
oQAAAAtzc2gtZWQyNTUxOQAAACBGFnl0OaE6g1Ao9jr9UJX3gW7I+UCtPKvY+3t1kKsB9
AAAAEBLOZWuEjkAA4Y5J5wpjjp2xh+PdMYZs1Q8ZG6T6Xj7kUeWefQ5oTqDU
-----END OPENSSH PRIVATE KEY-----
`

// dummyKey returns a freshly generated ed25519 key in PEM form for tests
// that need a parseable key file (the constant above is only for the
// "file present" path; here we actually need ssh.ParsePrivateKey to succeed).
func dummyKey(t interface{ Helper() }) string {
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		panic(err)
	}
	block, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		panic(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: block}))
}
