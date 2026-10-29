package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"math/big"
	"net"
	"strings"
	"testing"
	"time"
)

// selfSigned returns a server cert for 127.0.0.1 and a pool that trusts it.
func selfSigned(t *testing.T) (tls.Certificate, *x509.CertPool) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "fake pop"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IPAddresses:           []net.IP{net.ParseIP("127.0.0.1")},
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(cert)
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}, pool
}

// Same poison scenario, but over POP3S like the real Yahoo server.
func TestPoisonOverTLS(t *testing.T) {
	cert, pool := selfSigned(t)
	pop := newFakePOP(t, msg("a"), poison("bad"), msg("c"))
	sm := newFakeSMTP(t)
	cfg := testConfig(t, pop, sm)

	// Put a TLS listener in front of the fake server.
	tln, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{Certificates: []tls.Certificate{cert}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { tln.Close() })
	go func() {
		for {
			c, err := tln.Accept()
			if err != nil {
				return
			}
			go pop.serve(c)
		}
	}()
	yes := true
	cfg.TLS = &yes
	cfg.Server = tln.Addr().String()
	cfg.tlsRoots = pool

	if _, err := runOnce(t, cfg); err != nil {
		t.Fatal(err)
	}
	eq(t, "delivered over TLS", uidsOf(sm.received()), []string{"a", "c"})
	eq(t, "poison left", pop.uids(), []string{"bad"})

	// And an untrusted cert must be refused, not silently accepted.
	cfg.tlsRoots = x509.NewCertPool()
	_, err = runOnce(t, cfg)
	if err == nil || !strings.Contains(err.Error(), "TLS handshake") {
		t.Fatalf("expected a TLS verification failure, got %v", err)
	}
}
