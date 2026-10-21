package driver

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

var testCert, nameOnlyCert tls.Certificate

func selfSigned(name string, lifetime time.Duration, dnsNames []string, ips []net.IP) tls.Certificate {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		panic(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: name},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(lifetime),
		DNSNames:              dnsNames,
		IPAddresses:           ips,
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		panic(err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}

func TestMain(m *testing.M) {
	testCert = selfSigned("driver-test", 24*time.Hour, []string{"localhost"}, []net.IP{net.ParseIP("127.0.0.1"), net.ParseIP("::1")})
	nameOnlyCert = selfSigned("tls-host.test", 90*24*time.Hour, []string{"tls-host.test"}, nil)

	dir, err := os.MkdirTemp("", "driver-test-roots")
	if err != nil {
		panic(err)
	}
	roots := filepath.Join(dir, "roots.pem")
	var pemRoots []byte
	for _, c := range []tls.Certificate{testCert, nameOnlyCert} {
		pemRoots = append(pemRoots, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: c.Certificate[0]})...)
	}
	if err := os.WriteFile(roots, pemRoots, 0o600); err != nil {
		panic(err)
	}
	os.Setenv("SSL_CERT_FILE", roots)

	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

func loopback(network string) string {
	if network == "tcp6" || network == "udp6" {
		return "[::1]:0"
	}
	return "127.0.0.1:0"
}

func listen(t *testing.T, network string) net.Listener {
	t.Helper()
	ln, err := net.Listen(network, loopback(network))
	if err != nil {
		if network == "tcp6" {
			t.Skipf("no IPv6 loopback: %v", err)
		}
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	return ln
}

type countingListener struct {
	net.Listener
	accepted *atomic.Int32
}

func (l countingListener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err == nil {
		l.accepted.Add(1)
	}
	return c, err
}

func waitAccepted(n *atomic.Int32) bool {
	deadline := time.Now().Add(2 * time.Second)
	for n.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	return n.Load() > 0
}

func serveConns(ln net.Listener, handle func(net.Conn)) {
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				handle(c)
			}()
		}
	}()
}
