package driver

import (
	"bufio"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"os"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/miekg/dns"

	v1 "github.com/patrickdk77/endpoint-monitoring-operator/api/v1alpha1"
)

func notAfter(t *testing.T, cert tls.Certificate) string {
	t.Helper()
	leaf, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	return leaf.NotAfter.UTC().Format(time.RFC3339)
}

func TestErrorText(t *testing.T) {
	local := net.TCPAddrFromAddrPort(netip.MustParseAddrPort("10.0.0.5:41234"))
	remote := net.TCPAddrFromAddrPort(netip.MustParseAddrPort("10.0.0.1:6379"))
	cert := &x509.Certificate{
		NotBefore: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		NotAfter:  time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC),
	}
	tests := []struct {
		name string
		err  error
		want string
	}{
		{"read timeout", fmt.Errorf("PING: %w", &net.OpError{Op: "read", Net: "tcp", Source: local, Addr: remote, Err: os.ErrDeadlineExceeded}),
			"PING: read tcp: i/o timeout"},
		{"reset inside a url.Error", &url.Error{Op: "Get", URL: "http://example.test/", Err: &net.OpError{Op: "read", Net: "tcp", Source: local, Addr: remote,
			Err: &os.SyscallError{Syscall: "read", Err: syscall.ECONNRESET}}},
			`Get "http://example.test/": read tcp: read: connection reset by peer`},
		{"dial error names no address", &net.OpError{Op: "dial", Net: "tcp", Addr: remote, Err: &os.SyscallError{Syscall: "connect", Err: syscall.ECONNREFUSED}},
			"dial tcp: connect: connection refused"},
		{"dial over IPv6", errors.New("dial tcp6 [2001:db8::1]:443: connect: network is unreachable"),
			"dial tcp6: connect: network is unreachable"},
		{"dial that never got an address", errors.New("dial tcp: lookup example.test: no such host"),
			"dial tcp: lookup example.test: no such host"},
		{"DNS read timeout", &net.DNSError{Err: "read udp 10.0.0.5:39812->10.96.0.10:53: i/o timeout", Name: "example.test", Server: "10.96.0.10:53", IsTimeout: true},
			"lookup example.test: read udp: i/o timeout"},
		{"DNS name not found from a rotating resolver", &net.DNSError{Err: "no such host", Name: "example.test", Server: "10.96.0.11:53", IsNotFound: true},
			"lookup example.test: no such host"},
		{"expired certificate", &tls.CertificateVerificationError{Err: x509.CertificateInvalidError{Cert: cert, Reason: x509.Expired,
			Detail: "current time 2026-10-19T21:15:57-04:00 is after 2026-04-01T00:00:00Z"}},
			"tls: failed to verify certificate: x509: certificate expired at 2026-04-01T00:00:00Z"},
		{"certificate not yet valid", &tls.CertificateVerificationError{Err: x509.CertificateInvalidError{Cert: cert, Reason: x509.Expired,
			Detail: "current time 2025-12-01T00:00:00Z is before 2026-01-01T00:00:00Z"}},
			"tls: failed to verify certificate: x509: certificate is not valid before 2026-01-01T00:00:00Z"},
		{"expired certificate flattened into a DNS error", &net.DNSError{Name: "example.test", Server: "127.0.0.1:853",
			Err: "tls: failed to verify certificate: x509: certificate has expired or is not yet valid: current time 2026-10-20T01:52:54-04:00 is after 2026-04-01T00:00:00Z"},
			"lookup example.test: tls: failed to verify certificate: x509: certificate expired at 2026-04-01T00:00:00Z"},
		{"HTTP/2 stream reset", errors.New(`Get "https://example.test/": stream error: stream ID 7; INTERNAL_ERROR; received from peer`),
			`Get "https://example.test/": stream error: INTERNAL_ERROR; received from peer`},
		{"HTTP/2 GOAWAY", errors.New(`Get "https://example.test/": http2: server sent GOAWAY and closed the connection; LastStreamID=7, ErrCode=NO_ERROR, debug=""`),
			`Get "https://example.test/": http2: server sent GOAWAY and closed the connection; ErrCode=NO_ERROR, debug=""`},
		{"redirect target with a nonce", errors.New(`Get "http://127.0.0.1:1/authorize?state=00001eef": dial tcp 127.0.0.1:1: connect: connection refused`),
			`Get "http://127.0.0.1:1/authorize": dial tcp: connect: connection refused`},
		{"https query", errors.New(`Get "https://example.test/health?token=t0ken": EOF`), `Get "https://example.test/health": EOF`},
		{"query holding an escaped quote", errors.New(`Get "http://example.test/cb?msg=\"hi\"&secret=abc123": EOF`), `Get "http://example.test/cb": EOF`},
		{"URL without a query", errors.New(`Get "http://example.test:8080/p#frag": EOF`), `Get "http://example.test:8080/p#frag": EOF`},
		{"quoted text that is not a URL", errors.New(`Something "not a url? really"`), `Something "not a url? really"`},
		{"ftp URL", errors.New(`Get "ftp://example.test/p?x=1": EOF`), `Get "ftp://example.test/p?x=1": EOF`},
		{"credentials in an unparsable URL", errors.New(`parse "http://user:s3cret@127.0.0.1:1x/p?token=t0ken": invalid port ":1x" after host`),
			`parse "http://user:***@127.0.0.1:1x/p": invalid port ":1x" after host`},
		{"credentials already masked", errors.New(`Get "http://user:***@example.test/p": EOF`), `Get "http://user:***@example.test/p": EOF`},
		{"credentials in an unquoted lookup name", errors.New(`PING: dial tcp: lookup redis://default:s3cret@127.0.0.1:1/0: no such host`),
			`PING: dial tcp: lookup redis://default:***@127.0.0.1:1/0: no such host`},
		{"user without a password", errors.New(`Get "http://monitor@example.test/p": EOF`), `Get "http://monitor@example.test/p": EOF`},
		{"unparsable endpoint", &url.Error{Op: "parse", URL: "http://monitor:pa/ss@127.0.0.1:1/health", Err: errors.New(`invalid port ":pa" after host`)},
			"the endpoint is not a valid URL"},
		{"other certificate error", x509.HostnameError{Certificate: &x509.Certificate{DNSNames: []string{"localhost"}}, Host: "other.test"},
			"x509: certificate is valid for localhost, not other.test"},
		{"mongodb connection counter", errors.New(`connection(db.example.test:27017[-16]) incomplete read of message header: EOF`),
			`connection(db.example.test:27017) incomplete read of message header: EOF`},
		{"plain error", errors.New(`unexpected reply "-ERR"`), `unexpected reply "-ERR"`},
	}
	for _, tc := range tests {
		if got := errorText(tc.err); got != tc.want {
			t.Errorf("%s:\n got %s\nwant %s", tc.name, got, tc.want)
		}
	}
}

func resetConn(c net.Conn) {
	_ = c.(*net.TCPConn).SetLinger(0)
	_ = c.Close()
}

func TestFailureTextIsStableBetweenChecks(t *testing.T) {
	withCheckTimeout(t, 200*time.Millisecond)
	resetAfterPing, _ := tcpServer(func(c net.Conn) {
		if _, err := readRESP(bufio.NewReader(c)); err == nil {
			resetConn(c)
		}
	})(t, "tcp4")
	resetAfterEhlo, _ := tcpServer(func(c net.Conn) {
		fmt.Fprint(c, "220 test ESMTP\r\n")
		if _, err := bufio.NewReader(c).ReadString('\n'); err == nil {
			resetConn(c)
		}
	})(t, "tcp4")
	resetAfterRequest, _ := tcpServer(func(c net.Conn) {
		if _, err := bufio.NewReader(c).ReadString('\n'); err == nil {
			resetConn(c)
		}
	})(t, "tcp4")
	resetMidBody, _ := tcpServer(func(c net.Conn) {
		if _, err := bufio.NewReader(c).ReadString('\n'); err == nil {
			fmt.Fprint(c, "HTTP/1.1 200 OK\r\nContent-Length: 1000\r\n\r\n{\"status\":")
			time.Sleep(20 * time.Millisecond)
			resetConn(c)
		}
	})(t, "tcp4")
	h2 := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		panic(http.ErrAbortHandler)
	}))
	h2.EnableHTTP2 = true
	h2.TLS = &tls.Config{Certificates: []tls.Certificate{testCert}}
	h2.StartTLS()
	t.Cleanup(h2.Close)
	silent, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { silent.Close() })
	closedPort, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	refusing := closedPort.LocalAddr().String()
	closedPort.Close()
	downLn, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	down := downLn.Addr().String()
	downLn.Close()
	var nonce atomic.Int64
	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, fmt.Sprintf("http://%s/authorize?state=%08x", down, nonce.Add(7919)), http.StatusFound)
	}))
	t.Cleanup(redirect.Close)

	drivers := map[string]Driver{}
	drivers["redis reset"], _ = NewREDISDriver(resetAfterPing, "", nil, "default", nil)
	drivers["smtp reset"], _ = NewSMTPDriver(resetAfterEhlo, "", &v1.SmtpCheck{Helo: "monitor.test"}, "default", nil)
	drivers["http reset"], _ = NewHTTPDriver("http://"+resetAfterRequest, "", nil, nil)
	drivers["http-json reset"], _ = NewHTTPJSONDriver("http://"+resetAfterRequest, "", &v1.HttpJsonCheck{JsonAssertions: map[string]string{"status": "UP"}})
	drivers["opensearch reset"], _ = NewOpenSearchDriver("http://"+resetAfterRequest, "")
	drivers["trino reset"], _ = NewTrinoDriver("http://"+resetAfterRequest, "")
	drivers["http body reset"], _ = NewHTTPDriver("http://"+resetMidBody, "", nil, &v1.HttpCheck{BodyContains: "UP"})
	drivers["http-json body reset"], _ = NewHTTPJSONDriver("http://"+resetMidBody, "", &v1.HttpJsonCheck{JsonAssertions: map[string]string{"status": "UP"}})
	drivers["opensearch body reset"], _ = NewOpenSearchDriver("http://"+resetMidBody, "")
	drivers["trino body reset"], _ = NewTrinoDriver("http://"+resetMidBody, "")
	drivers["http2 stream reset"], _ = NewHTTPDriver(h2.URL, "", nil, nil)
	drivers["http redirect with a nonce"], _ = NewHTTPDriver(redirect.URL, "", nil, nil)
	drivers["dns timeout"], _ = NewDNSDriver("example.test", "", &v1.DnsCheck{Server: silent.LocalAddr().String(), Type: "A"})
	mongoHangUp, _ := tcpServer(func(c net.Conn) {
		if _, err := io.ReadFull(c, make([]byte, 16)); err == nil {
			resetConn(c)
		}
	})(t, "tcp4")
	drivers["mongodb reset"], _ = NewMongoDBDriver(mongoHangUp, "", nil, "default", nil)
	drivers["mongodb refused"], _ = NewMongoDBDriver(down, "", nil, "default", nil)
	for name, d := range drivers {
		t.Run(name, func(t *testing.T) {
			first, _ := d.Check()
			second, _ := d.Check()
			if first.Success || second.Success {
				t.Fatalf("expected failures: %q / %q", first.Message, second.Message)
			}
			if first.Message != second.Message || strings.Contains(first.Message, "->") || strings.Contains(first.Message, "stream ID") || strings.Contains(first.Message, "[-") {
				t.Fatalf("the failure text names a local address or changed:\n%s\n%s", first.Message, second.Message)
			}
		})
	}

	t.Run("dns refused", func(t *testing.T) {
		d, _ := NewDNSDriver("example.test", "", &v1.DnsCheck{Server: refusing, Type: "A"})
		// Rarely a sample times out instead of being refused; only refused samples are compared.
		var refused []string
		for i := 0; i < 6 && len(refused) < 2; i++ {
			if res, _ := d.Check(); strings.Contains(res.Message, "connection refused") {
				refused = append(refused, res.Message)
			}
		}
		if len(refused) < 2 || refused[0] != refused[1] || strings.Contains(refused[0], "->") {
			t.Fatalf("refused samples named a local address or changed: %q", refused)
		}
	})
}

func httpFamily(endpoint string) []Driver {
	httpDriver, _ := NewHTTPDriver(endpoint, "", nil, nil)
	httpJSON, _ := NewHTTPJSONDriver(endpoint, "", &v1.HttpJsonCheck{JsonAssertions: map[string]string{"status": "UP"}})
	opensearch, _ := NewOpenSearchDriver(endpoint, "")
	trino, _ := NewTrinoDriver(endpoint, "")
	return []Driver{httpDriver, httpJSON, opensearch, trino}
}

func TestFailureTextHidesURLCredentials(t *testing.T) {
	for _, endpoint := range []string{
		"http://monitor:s3cret@127.0.0.1:1x/health?token=t0ken",
		"http://monitor:pa/ss@127.0.0.1:1/health",
		`http://monitor:pa"ss@127.0.0.1:1x/health`,
		"http://monitor:pa?ss@127.0.0.1:1/health",
		"http://monitor:pa#ss@127.0.0.1:1/health",
		"http://monitor:p@ss@127.0.0.1:1x/health",
	} {
		for _, d := range httpFamily(endpoint) {
			res, _ := d.Check()
			if res.Success || !strings.HasSuffix(res.Message, ": the endpoint is not a valid URL") {
				t.Errorf("%s %s: success=%v %s", d.GetType(), endpoint, res.Success, res.Message)
			}
		}
	}

	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	closed := ln.Addr().String()
	ln.Close()
	for _, d := range httpFamily("http://monitor:s3cret@" + closed + "/health?token=t0ken") {
		res, _ := d.Check()
		if res.Success || strings.Contains(res.Message, "s3cret") || strings.Contains(res.Message, "t0ken") || !strings.Contains(res.Message, "monitor:***@") {
			t.Errorf("%s: success=%v %s", d.GetType(), res.Success, res.Message)
		}
	}
}

func TestDialFailuresNameNoAddress(t *testing.T) {
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	closed := ln.Addr().String()
	ln.Close()
	tcp, _ := NewTCPDriver(closed, "")
	ping, _ := NewPingDriver(closed, "")
	mysqlDriver, _ := NewMYSQLDriver(closed, "", nil, "default", nil)
	for _, d := range []Driver{tcp, ping, mysqlDriver} {
		res, _ := d.Check()
		if res.Success || !strings.Contains(res.Message, "dial tcp: connect: connection refused") || strings.Contains(res.Message, "127.0.0.1") {
			t.Errorf("%s: success=%v %s", d.GetType(), res.Success, res.Message)
		}
	}
}

func TestExpiredCertificateText(t *testing.T) {
	expired := selfSigned("expired", -30*time.Minute, nil, []net.IP{net.ParseIP("127.0.0.1")})
	want := "x509: certificate expired at " + notAfter(t, expired)

	tlsEndpoint, _ := certServer(t, expired)
	tlsDriver, _ := NewTLSDriver(tlsEndpoint, "", nil)

	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	srv.TLS = &tls.Config{Certificates: []tls.Certificate{expired}}
	srv.StartTLS()
	t.Cleanup(srv.Close)
	httpDriver, _ := NewHTTPDriver(srv.URL, "", nil, nil)

	ln := listen(t, "tcp4")
	dot := &dns.Server{Listener: tls.NewListener(ln, &tls.Config{Certificates: []tls.Certificate{expired}}), Handler: dns.HandlerFunc(func(w dns.ResponseWriter, r *dns.Msg) {})}
	started := make(chan struct{})
	dot.NotifyStartedFunc = func() { close(started) }
	go func() { _ = dot.ActivateAndServe() }()
	<-started
	t.Cleanup(func() { _ = dot.Shutdown() })
	dnsDriver, _ := NewDNSDriver("example.test", "", &v1.DnsCheck{Server: ln.Addr().String(), Tls: true, Type: "A"})

	for _, d := range []Driver{tlsDriver, httpDriver, dnsDriver} {
		res, _ := d.Check()
		if res.Success || !strings.Contains(res.Message, want) || strings.Contains(res.Message, "current time") {
			t.Errorf("%s: success=%v %s", d.GetType(), res.Success, res.Message)
		}
	}
}

func TestHTTPDriverDaysToExpire(t *testing.T) {
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	srv.TLS = &tls.Config{Certificates: []tls.Certificate{testCert}}
	srv.StartTLS()
	t.Cleanup(srv.Close)
	d, _ := NewHTTPDriver(srv.URL, "", &v1.TlsCheck{DaysToExpire: 1}, nil)
	res, _ := d.Check()
	want := "HTTP check failed: certificate expires at " + notAfter(t, testCert) + ", within the 1-day alert window"
	if res.Success || res.Message != want {
		t.Fatalf("success=%v %s", res.Success, res.Message)
	}
}
