package driver

import (
	"bufio"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/miekg/dns"

	v1 "github.com/patrickdk77/endpoint-monitoring-operator/api/v1alpha1"
)

func TestFamilyNetwork(t *testing.T) {
	tests := []struct {
		network, family, want string
	}{
		{"tcp", "", "tcp"},
		{"tcp", FamilyIPv4, "tcp4"},
		{"tcp", FamilyIPv6, "tcp6"},
		{"udp", FamilyIPv4, "udp4"},
		{"udp", FamilyIPv6, "udp6"},
		{"tcp4", FamilyIPv6, "tcp6"},
		{"udp6", FamilyIPv4, "udp4"},
		{"tcp4", "", "tcp4"},
		{"tcp", "bogus", "tcp"},
	}
	for _, tc := range tests {
		if got := familyNetwork(tc.network, tc.family); got != tc.want {
			t.Errorf("familyNetwork(%q, %q) = %q, want %q", tc.network, tc.family, got, tc.want)
		}
	}
}

func TestPickFamily(t *testing.T) {
	both := []string{"192.0.2.1", "2001:db8::1", "192.0.2.2"}
	tests := []struct {
		name    string
		addrs   []string
		family  string
		want    string
		wantErr bool
	}{
		{"default takes first", both, "", "192.0.2.1", false},
		{"ipv4", both, FamilyIPv4, "192.0.2.1", false},
		{"ipv6", both, FamilyIPv6, "2001:db8::1", false},
		{"ipv6 first in list", []string{"2001:db8::1", "192.0.2.1"}, FamilyIPv4, "192.0.2.1", false},
		{"v4-mapped counts as ipv4", []string{"::ffff:192.0.2.1"}, FamilyIPv4, "::ffff:192.0.2.1", false},
		{"v4-mapped is not ipv6", []string{"::ffff:192.0.2.1"}, FamilyIPv6, "", true},
		{"zoned ipv6", []string{"fe80::1%lo"}, FamilyIPv6, "fe80::1%lo", false},
		{"zoned ipv6 is not ipv4", []string{"fe80::1%lo"}, FamilyIPv4, "", true},
		{"no ipv6", []string{"192.0.2.1"}, FamilyIPv6, "", true},
		{"no ipv4", []string{"2001:db8::1"}, FamilyIPv4, "", true},
		{"empty", nil, FamilyIPv4, "", true},
		{"empty default", nil, "", "", true},
		{"names are skipped", []string{"not-an-ip"}, FamilyIPv4, "", true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := pickFamily(tc.addrs, tc.family)
			if (err != nil) != tc.wantErr || got != tc.want {
				t.Fatalf("pickFamily(%v, %q) = %q, %v; want %q, err=%v", tc.addrs, tc.family, got, err, tc.want, tc.wantErr)
			}
		})
	}
}

func TestHTTPClientDefaultFamilyKeepsDefaultTransport(t *testing.T) {
	if c := httpClient("", time.Second); c.Transport != nil {
		t.Fatalf("default family should use http.DefaultTransport, got %T", c.Transport)
	}
	if c := httpClient(FamilyIPv4, time.Second); c.Transport == nil {
		t.Fatal("ipv4 needs its own transport")
	}
}

func TestHTTPClientPinnedFamilyTransport(t *testing.T) {
	for _, family := range []string{FamilyIPv4, FamilyIPv6} {
		transport, ok := httpClient(family, time.Second).Transport.(*http.Transport)
		if !ok {
			t.Fatalf("%s: transport %T", family, transport)
		}
		if transport.Proxy != nil {
			t.Errorf("%s: a pinned family must connect directly, not through an environment proxy", family)
		}
		if transport.DialContext == nil {
			t.Errorf("%s: no family dialer", family)
		}
		if !transport.DisableKeepAlives {
			t.Errorf("%s: keep-alives would leak one idle connection per check", family)
		}
	}
}

func TestHTTPClientPinnedFamilyClosesConnections(t *testing.T) {
	ln := listen(t, "tcp4")
	var opened, closed atomic.Int32
	srv := &http.Server{
		Handler:           http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, "ok") }),
		ReadHeaderTimeout: 5 * time.Second,
		ConnState: func(c net.Conn, s http.ConnState) {
			switch s {
			case http.StateNew:
				opened.Add(1)
			case http.StateClosed, http.StateHijacked:
				closed.Add(1)
			}
		},
	}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { srv.Close() })

	for i := 0; i < 3; i++ {
		d, _ := NewHTTPDriver("http://"+ln.Addr().String(), FamilyIPv4, nil, nil)
		if res, _ := d.Check(); !res.Success {
			t.Fatalf("check %d: %s", i, res.Message)
		}
	}
	deadline := time.Now().Add(2 * time.Second)
	for closed.Load() < opened.Load() && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if opened.Load() != 3 || closed.Load() != 3 {
		t.Fatalf("opened=%d closed=%d; a pinned transport per check must not leave idle connections", opened.Load(), closed.Load())
	}
}

func TestFamilyDial(t *testing.T) {
	v4, _ := dnsServer(t, "udp4")
	v6, _ := dnsServer(t, "udp6")
	tests := []struct {
		family, network, addr string
		wantErr               bool
	}{
		{FamilyIPv4, "udp", v4, false},
		{FamilyIPv6, "udp", v6, false},
		{FamilyIPv6, "udp", v4, true},
		{FamilyIPv4, "udp", v6, true},
		{FamilyIPv6, "tcp", v4, true},
		{"", "udp", v4, false},
		{"", "udp", v6, false},
	}
	for _, tc := range tests {
		conn, err := familyDial(tc.family)(context.Background(), tc.network, tc.addr)
		if conn != nil {
			conn.Close()
		}
		if (err != nil) != tc.wantErr || (err != nil && !strings.Contains(err.Error(), "no suitable address")) {
			t.Errorf("familyDial(%q)(%s, %s): err=%v, wantErr=%v", tc.family, tc.network, tc.addr, err, tc.wantErr)
		}
	}
}

func resolvConfFamilies(t *testing.T) (v4, v6 int) {
	t.Helper()
	data, err := os.ReadFile("/etc/resolv.conf")
	if err != nil {
		t.Skipf("no /etc/resolv.conf: %v", err)
	}
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 || fields[0] != "nameserver" {
			continue
		}
		ip, err := netip.ParseAddr(fields[1])
		switch {
		case err != nil:
			t.Skipf("unparsable nameserver %q", fields[1])
		case ip.Unmap().Is4():
			v4++
		default:
			v6++
		}
	}
	return v4, v6
}

func TestDNSDefaultResolverHonorsFamily(t *testing.T) {
	v4, v6 := resolvConfFamilies(t)
	var missing string
	switch {
	case v4 > 0 && v6 == 0:
		missing = FamilyIPv6
	case v6 > 0 && v4 == 0:
		missing = FamilyIPv4
	default:
		t.Skipf("resolv.conf has %d IPv4 and %d IPv6 nameservers; need a single family", v4, v6)
	}
	d, _ := NewDNSDriver("example.test", missing, &v1.DnsCheck{Type: "A"})
	res, _ := d.Check()
	if res.Success || !strings.Contains(res.Message, "no suitable address") {
		t.Fatalf("default resolver over %s with no %s nameserver: success=%v %s", missing, missing, res.Success, res.Message)
	}
}

type familyServer func(t *testing.T, network string) (endpoint string, accepted *atomic.Int32)

func tcpServer(handle func(net.Conn)) familyServer {
	return func(t *testing.T, network string) (string, *atomic.Int32) {
		ln := listen(t, network)
		var n atomic.Int32
		serveConns(countingListener{ln, &n}, handle)
		return ln.Addr().String(), &n
	}
}

func tlsServer(handle func(net.Conn)) familyServer {
	return func(t *testing.T, network string) (string, *atomic.Int32) {
		ln := listen(t, network)
		var n atomic.Int32
		serveConns(tls.NewListener(countingListener{ln, &n}, &tls.Config{Certificates: []tls.Certificate{testCert}}), func(c net.Conn) {
			if c.(*tls.Conn).Handshake() == nil {
				handle(c)
			}
		})
		return ln.Addr().String(), &n
	}
}

func httpServer(handler http.HandlerFunc) familyServer {
	return func(t *testing.T, network string) (string, *atomic.Int32) {
		ln := listen(t, network)
		var n atomic.Int32
		srv := &http.Server{Handler: handler, ReadHeaderTimeout: 5 * time.Second}
		go func() { _ = srv.Serve(countingListener{ln, &n}) }()
		t.Cleanup(func() { srv.Close() })
		return "http://" + ln.Addr().String(), &n
	}
}

func smtpDialog(c net.Conn) {
	r := bufio.NewReader(c)
	fmt.Fprintf(c, "220 test ESMTP\r\n")
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return
		}
		switch cmd := strings.ToUpper(strings.TrimSpace(line)); {
		case strings.HasPrefix(cmd, "EHLO"), strings.HasPrefix(cmd, "HELO"):
			fmt.Fprintf(c, "250 test\r\n")
		case cmd == "QUIT":
			fmt.Fprintf(c, "221 bye\r\n")
			return
		default:
			fmt.Fprintf(c, "250 ok\r\n")
		}
	}
}

func redisDialog(c net.Conn) {
	r := bufio.NewReader(c)
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return
		}
		if strings.HasPrefix(strings.ToUpper(line), "PING") {
			fmt.Fprintf(c, "+PONG\r\n")
		}
	}
}

func TestDriversHonorFamily(t *testing.T) {
	cases := []struct {
		name string
		// connectOnly means the server cannot complete the protocol, so only
		// the connection is checked, not the result.
		connectOnly bool
		serve       familyServer
		build       func(endpoint, family string) (Driver, error)
	}{
		{"tcp", false, tcpServer(func(net.Conn) {}), func(e, f string) (Driver, error) {
			return NewTCPDriver(e, f)
		}},
		{"tls", false, tlsServer(func(net.Conn) {}), func(e, f string) (Driver, error) {
			return NewTLSDriver(e, f, &v1.TlsCheck{})
		}},
		{"smtp", false, tcpServer(smtpDialog), func(e, f string) (Driver, error) {
			return NewSMTPDriver(e, f, &v1.SmtpCheck{}, "default", nil)
		}},
		{"smtp-tls", false, tlsServer(smtpDialog), func(e, f string) (Driver, error) {
			return NewSMTPDriver(e, f, &v1.SmtpCheck{Tls: true}, "default", nil)
		}},
		{"redis", false, tcpServer(redisDialog), func(e, f string) (Driver, error) {
			return NewREDISDriver(e, f, &v1.RedisCheck{}, "default", nil)
		}},
		{"redis-tls", false, tlsServer(redisDialog), func(e, f string) (Driver, error) {
			return NewREDISDriver(e, f, &v1.RedisCheck{Tls: true}, "default", nil)
		}},
		{"mysql", true, tcpServer(func(net.Conn) {}), func(e, f string) (Driver, error) {
			return NewMYSQLDriver(e, f, nil, "default", nil)
		}},
		{"http", false, httpServer(func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprint(w, "ok")
		}), func(e, f string) (Driver, error) {
			return NewHTTPDriver(e, f, nil, nil)
		}},
		{"http-json", false, httpServer(func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprint(w, `{"status":"UP"}`)
		}), func(e, f string) (Driver, error) {
			return NewHTTPJSONDriver(e, f, &v1.HttpJsonCheck{JsonAssertions: map[string]string{"status": "UP"}})
		}},
		{"opensearch", false, httpServer(func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprint(w, `{"status":"green"}`)
		}), func(e, f string) (Driver, error) {
			return NewOpenSearchDriver(e, f)
		}},
		{"trino", false, httpServer(func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprint(w, `{"coordinator":true,"starting":false}`)
		}), func(e, f string) (Driver, error) {
			return NewTrinoDriver(e, f)
		}},
	}

	for _, tc := range cases {
		for _, listenNet := range []string{"tcp4", "tcp6"} {
			own, other := FamilyIPv4, FamilyIPv6
			if listenNet == "tcp6" {
				own, other = FamilyIPv6, FamilyIPv4
			}
			for _, family := range []string{"", own, other} {
				t.Run(fmt.Sprintf("%s/%s/%q", tc.name, listenNet, family), func(t *testing.T) {
					endpoint, accepted := tc.serve(t, listenNet)
					d, err := tc.build(endpoint, family)
					if err != nil {
						t.Fatal(err)
					}
					res, err := d.Check()
					if err != nil {
						t.Fatal(err)
					}
					if family == other {
						if res.Success || accepted.Load() != 0 || !strings.Contains(res.Message, "no suitable address") {
							t.Fatalf("family %s reached a %s listener: success=%v accepted=%d %s", family, listenNet, res.Success, accepted.Load(), res.Message)
						}
						return
					}
					if !waitAccepted(accepted) {
						t.Fatalf("no connection reached the %s listener: %s", listenNet, res.Message)
					}
					if !tc.connectOnly && !res.Success {
						t.Fatalf("check failed: %s", res.Message)
					}
				})
			}
		}
	}
}

func TestPingHonorsFamily(t *testing.T) {
	d, _ := NewPingDriver("127.0.0.1", FamilyIPv6)
	res, _ := d.Check()
	if res.Success || !strings.Contains(res.Message, "no suitable address") {
		t.Fatalf("ipv6 ping of an IPv4 address: success=%v %s", res.Success, res.Message)
	}
	d, _ = NewPingDriver("127.0.0.1", FamilyIPv4)
	res, _ = d.Check()
	if strings.Contains(res.Message, "no suitable address") {
		t.Fatalf("ipv4 ping of an IPv4 address was refused by family: %s", res.Message)
	}
}

func dnsServer(t *testing.T, network string) (string, *atomic.Int32) {
	t.Helper()
	pc, err := net.ListenPacket(network, loopback(network))
	if err != nil {
		if network == "udp6" {
			t.Skipf("no IPv6 loopback: %v", err)
		}
		t.Fatal(err)
	}
	var n atomic.Int32
	srv := &dns.Server{PacketConn: pc, Handler: dns.HandlerFunc(func(w dns.ResponseWriter, r *dns.Msg) {
		n.Add(1)
		m := new(dns.Msg)
		m.SetReply(r)
		for _, q := range r.Question {
			if q.Qtype == dns.TypeA {
				rr, _ := dns.NewRR(q.Name + " 60 IN A 192.0.2.1")
				m.Answer = append(m.Answer, rr)
			}
		}
		_ = w.WriteMsg(m)
	})}
	started := make(chan struct{})
	srv.NotifyStartedFunc = func() { close(started) }
	go func() { _ = srv.ActivateAndServe() }()
	<-started
	t.Cleanup(func() { _ = srv.Shutdown() })
	return pc.LocalAddr().String(), &n
}

func TestDNSHonorsFamily(t *testing.T) {
	for _, listenNet := range []string{"udp4", "udp6"} {
		own, other := FamilyIPv4, FamilyIPv6
		if listenNet == "udp6" {
			own, other = FamilyIPv6, FamilyIPv4
		}
		for _, family := range []string{"", own, other} {
			t.Run(fmt.Sprintf("%s/%q", listenNet, family), func(t *testing.T) {
				server, queries := dnsServer(t, listenNet)
				d, _ := NewDNSDriver("example.test", family, &v1.DnsCheck{Server: server, Type: "A", Verify: "192.0.2.1"})
				res, err := d.Check()
				if err != nil {
					t.Fatal(err)
				}
				if family == other {
					if res.Success || queries.Load() != 0 || !strings.Contains(res.Message, "no "+family+" address") {
						t.Fatalf("family %s queried a %s server: success=%v queries=%d %s", family, listenNet, res.Success, queries.Load(), res.Message)
					}
					return
				}
				if !res.Success || queries.Load() == 0 {
					t.Fatalf("success=%v queries=%d %s", res.Success, queries.Load(), res.Message)
				}
			})
		}
	}
}

type fakeDriver struct {
	result *CheckResult
	err    error
	calls  int
}

func (f *fakeDriver) Check() (*CheckResult, error) {
	f.calls++
	return f.result, f.err
}
func (f *fakeDriver) GetEndpoint() string { return "fake.test" }
func (f *fakeDriver) GetType() string     { return "fake" }

func TestBothFamilies(t *testing.T) {
	errV4 := errors.New("v4 down")
	errV6 := errors.New("v6 down")
	ok := func(ms int, msg string) *CheckResult {
		return &CheckResult{Success: true, ResponseTime: time.Duration(ms) * time.Millisecond, Message: msg}
	}
	bad := func(ms int, msg string, err error) *CheckResult {
		return &CheckResult{ResponseTime: time.Duration(ms) * time.Millisecond, Message: msg, Error: err}
	}
	tests := []struct {
		name     string
		v4, v6   *CheckResult
		wantOK   bool
		wantIs   []error
		wantErr  string
		wantTime time.Duration
		wantMsg  string
	}{
		{"both up", ok(10, "up4"), ok(30, "up6"), true, nil, "", 30 * time.Millisecond, "ipv4: up4; ipv6: up6"},
		{"ipv4 down", bad(5, "down4", errV4), ok(30, "up6"), false, []error{errV4}, "ipv4: v4 down", 30 * time.Millisecond, "ipv4: down4; ipv6: up6"},
		{"ipv6 down", ok(10, "up4"), bad(50, "down6", errV6), false, []error{errV6}, "ipv6: v6 down", 50 * time.Millisecond, "ipv4: up4; ipv6: down6"},
		{"both down", bad(5, "down4", errV4), bad(7, "down6", errV6), false, []error{errV4, errV6}, "ipv4: v4 down\nipv6: v6 down", 7 * time.Millisecond, "ipv4: down4; ipv6: down6"},
		{"failure without an error", bad(5, "status 500", nil), ok(1, "up6"), false, nil, "ipv4: status 500", 5 * time.Millisecond, "ipv4: status 500; ipv6: up6"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			d := NewBothFamiliesDriver(&fakeDriver{result: tc.v4}, &fakeDriver{result: tc.v6})
			res, err := d.Check()
			if err != nil {
				t.Fatal(err)
			}
			if res.Success != tc.wantOK || res.ResponseTime != tc.wantTime || res.Message != tc.wantMsg {
				t.Fatalf("got success=%v time=%v msg=%q", res.Success, res.ResponseTime, res.Message)
			}
			if tc.wantErr == "" {
				if res.Error != nil {
					t.Fatalf("unexpected error %v", res.Error)
				}
				return
			}
			if res.Error == nil || res.Error.Error() != tc.wantErr {
				t.Fatalf("error %q, want %q", res.Error, tc.wantErr)
			}
			for _, want := range tc.wantIs {
				if !errors.Is(res.Error, want) {
					t.Fatalf("error %q does not wrap %v", res.Error, want)
				}
			}
		})
	}

	t.Run("ipv4 check error stops before ipv6", func(t *testing.T) {
		v6 := &fakeDriver{result: ok(1, "up6")}
		_, err := NewBothFamiliesDriver(&fakeDriver{err: errV4}, v6).Check()
		if !errors.Is(err, errV4) || !strings.HasPrefix(err.Error(), "ipv4: ") || v6.calls != 0 {
			t.Fatalf("err=%v ipv6 calls=%d", err, v6.calls)
		}
	})
	t.Run("ipv6 check error", func(t *testing.T) {
		_, err := NewBothFamiliesDriver(&fakeDriver{result: ok(1, "up4")}, &fakeDriver{err: errV6}).Check()
		if !errors.Is(err, errV6) || !strings.HasPrefix(err.Error(), "ipv6: ") {
			t.Fatalf("err=%v", err)
		}
	})
	t.Run("endpoint and type come from the ipv4 driver", func(t *testing.T) {
		d := NewBothFamiliesDriver(&fakeDriver{}, &fakeDriver{})
		if d.GetEndpoint() != "fake.test" || d.GetType() != "fake" {
			t.Fatalf("%s %s", d.GetEndpoint(), d.GetType())
		}
	})
}

func TestBothFamiliesAgainstSingleStackEndpoint(t *testing.T) {
	ln := listen(t, "tcp4")
	var n atomic.Int32
	serveConns(countingListener{ln, &n}, func(net.Conn) {})
	v4, _ := NewTCPDriver(ln.Addr().String(), FamilyIPv4)
	v6, _ := NewTCPDriver(ln.Addr().String(), FamilyIPv6)
	res, err := NewBothFamiliesDriver(v4, v6).Check()
	if err != nil {
		t.Fatal(err)
	}
	if res.Success || !waitAccepted(&n) || n.Load() != 1 || !strings.Contains(res.Message, "ipv4: TCP check successful") || !strings.Contains(res.Message, "ipv6: TCP check failed") {
		t.Fatalf("success=%v accepted=%d %s", res.Success, n.Load(), res.Message)
	}
}
