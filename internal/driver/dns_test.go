package driver

import (
	"crypto/tls"
	"net"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/miekg/dns"

	v1 "github.com/patrickdk77/endpoint-monitoring-operator/api/v1alpha1"
)

// zoneHandler answers each question with the records of that name and
// type, plus any CNAME of that name.
func zoneHandler(t *testing.T, queries *atomic.Int32, zone ...string) dns.HandlerFunc {
	t.Helper()
	var rrs []dns.RR
	for _, z := range zone {
		rr, err := dns.NewRR(z)
		if err != nil {
			t.Fatal(err)
		}
		rrs = append(rrs, rr)
	}
	return func(w dns.ResponseWriter, r *dns.Msg) {
		queries.Add(1)
		m := new(dns.Msg)
		m.SetReply(r)
		for _, q := range r.Question {
			for _, rr := range rrs {
				h := rr.Header()
				if strings.EqualFold(h.Name, q.Name) && (h.Rrtype == q.Qtype || h.Rrtype == dns.TypeCNAME) {
					m.Answer = append(m.Answer, rr)
				}
			}
		}
		_ = w.WriteMsg(m)
	}
}

func serveDNS(t *testing.T, overTLS bool, h dns.Handler) string {
	t.Helper()
	srv := &dns.Server{Handler: h}
	var addr string
	if overTLS {
		ln := listen(t, "tcp4")
		srv.Listener = tls.NewListener(ln, &tls.Config{Certificates: []tls.Certificate{testCert}})
		addr = ln.Addr().String()
	} else {
		pc, err := net.ListenPacket("udp4", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		srv.PacketConn = pc
		addr = pc.LocalAddr().String()
	}
	started := make(chan struct{})
	srv.NotifyStartedFunc = func() { close(started) }
	go func() { _ = srv.ActivateAndServe() }()
	<-started
	t.Cleanup(func() { _ = srv.Shutdown() })
	return addr
}

func TestDNSDriverVerifyNames(t *testing.T) {
	var queries atomic.Int32
	server := serveDNS(t, false, zoneHandler(t, &queries,
		"alias.example.test. 60 IN CNAME Target.Example.Test.",
		"5.2.0.192.in-addr.arpa. 60 IN PTR Host.Example.Test.",
	))
	tests := []struct {
		name, endpoint, qtype, verify string
		wantOK                        bool
	}{
		{"CNAME as returned", "alias.example.test", "CNAME", "Target.Example.Test.", true},
		{"CNAME in another case", "alias.example.test", "CNAME", "target.example.test.", true},
		{"CNAME without the root dot", "alias.example.test", "CNAME", "Target.Example.Test", true},
		{"CNAME mismatch", "alias.example.test", "CNAME", "other.example.test", false},
		{"CNAME parent is not a match", "alias.example.test", "CNAME", "example.test", false},
		{"PTR as returned", "192.0.2.5", "PTR", "Host.Example.Test.", true},
		{"PTR in another case", "192.0.2.5", "PTR", "host.example.test.", true},
		{"PTR without the root dot", "192.0.2.5", "PTR", "Host.Example.Test", true},
		{"PTR mismatch", "192.0.2.5", "PTR", "other.example.test", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			d, _ := NewDNSDriver(tc.endpoint, "", &v1.DnsCheck{Server: server, Type: tc.qtype, Verify: tc.verify})
			res, _ := d.Check()
			if res.Success != tc.wantOK {
				t.Fatalf("success=%v %s", res.Success, res.Message)
			}
			if !tc.wantOK && !strings.Contains(res.Message, "returned dns result did not match Verify value") {
				t.Fatalf("unexpected failure: %s", res.Message)
			}
		})
	}
}

func TestDNSDriverOverTLS(t *testing.T) {
	withCheckTimeout(t, 2*time.Second)
	var queries atomic.Int32
	server := serveDNS(t, true, zoneHandler(t, &queries, "example.test. 60 IN A 192.0.2.1"))
	d, _ := NewDNSDriver("example.test", "", &v1.DnsCheck{Server: server, Tls: true, Type: "A", Verify: "192.0.2.1"})
	res := checkWithin(t, d, 5*time.Second)
	if !res.Success || queries.Load() == 0 {
		t.Fatalf("success=%v queries=%d %s", res.Success, queries.Load(), res.Message)
	}
}

func TestDNSDriverWithoutDnsCheck(t *testing.T) {
	d, err := NewDNSDriver("127.0.0.1", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if res, _ := d.Check(); !res.Success {
		t.Fatalf("a monitor without dnsCheck failed: %s", res.Message)
	}
}
