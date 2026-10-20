package driver

import (
	"crypto/tls"
	"net"
	"strings"
	"testing"

	v1 "github.com/patrickdk77/endpoint-monitoring-operator/api/v1alpha1"
)

func certServer(t *testing.T, cert tls.Certificate) (string, chan string) {
	t.Helper()
	ln := listen(t, "tcp4")
	sni := make(chan string, 1)
	cfg := &tls.Config{GetCertificate: func(hello *tls.ClientHelloInfo) (*tls.Certificate, error) {
		select {
		case sni <- hello.ServerName:
		default:
		}
		return &cert, nil
	}}
	serveConns(tls.NewListener(ln, cfg), func(c net.Conn) { _ = c.(*tls.Conn).Handshake() })
	return ln.Addr().String(), sni
}

func TestTLSDriver(t *testing.T) {
	tests := []struct {
		name      string
		cert      tls.Certificate
		check     *v1.TlsCheck
		wantOK    bool
		wantInMsg string
		wantSNI   string
	}{
		{"certificate for the endpoint without tlsCheck", testCert, nil, true, "TLS check successful", ""},
		{"certificate for tlsCheck.host", nameOnlyCert, &v1.TlsCheck{Host: "tls-host.test"}, true, "TLS check successful", "tls-host.test"},
		{"certificate for another name than the endpoint", nameOnlyCert, &v1.TlsCheck{}, false,
			"x509: cannot validate certificate for 127.0.0.1 because it doesn't contain any IP SANs", ""},
		{"tlsCheck.host not on the certificate", testCert, &v1.TlsCheck{Host: "other.test"}, false,
			"x509: certificate is valid for localhost, not other.test", "other.test"},
		{"expires within daysToExpire", testCert, &v1.TlsCheck{DaysToExpire: 1}, false,
			"TLS check failed: certificate expires at " + notAfter(t, testCert) + ", within the 1-day alert window", ""},
		{"expires after daysToExpire", nameOnlyCert, &v1.TlsCheck{Host: "tls-host.test", DaysToExpire: 89}, true, "TLS check successful", "tls-host.test"},
		{"expires at daysToExpire", nameOnlyCert, &v1.TlsCheck{Host: "tls-host.test", DaysToExpire: 90}, false,
			"TLS check failed: certificate expires at " + notAfter(t, nameOnlyCert) + ", within the 90-day alert window", "tls-host.test"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			endpoint, sni := certServer(t, tc.cert)
			d, err := NewTLSDriver(endpoint, "", tc.check)
			if err != nil {
				t.Fatal(err)
			}
			res, _ := d.Check()
			if res.Success != tc.wantOK || !strings.Contains(res.Message, tc.wantInMsg) {
				t.Fatalf("success=%v %s", res.Success, res.Message)
			}
			var got string
			select {
			case got = <-sni:
			default:
				t.Fatal("the server saw no ClientHello")
			}
			if got != tc.wantSNI {
				t.Fatalf("SNI %q, want %q", got, tc.wantSNI)
			}
		})
	}
}

func TestTLSTarget(t *testing.T) {
	tests := []struct {
		endpoint, host, wantAddr, wantName, wantPort string
	}{
		{"example.test", "", "example.test:443", "example.test", "443"},
		{"example.test:8443", "", "example.test:8443", "example.test", "8443"},
		{"::1", "", "[::1]:443", "::1", "443"},
		{"[::1]", "", "[::1]:443", "::1", "443"},
		{"[::1]:8443", "", "[::1]:8443", "::1", "8443"},
		{"192.0.2.1:8443", "cert.example.test", "192.0.2.1:8443", "cert.example.test", "8443"},
	}
	for _, tc := range tests {
		d, _ := NewTLSDriver(tc.endpoint, "", &v1.TlsCheck{Host: tc.host})
		addr, name, port := d.(*TLSDriver).target()
		if addr != tc.wantAddr || name != tc.wantName || port != tc.wantPort {
			t.Errorf("%s host=%q: addr=%s name=%s port=%s", tc.endpoint, tc.host, addr, name, port)
		}
	}
}
