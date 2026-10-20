package driver

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	v1 "github.com/patrickdk77/endpoint-monitoring-operator/api/v1alpha1"
)

func withCheckTimeout(t *testing.T, d time.Duration) {
	t.Helper()
	old := checkTimeout
	checkTimeout = d
	t.Cleanup(func() { checkTimeout = old })
}

// checkWithin fails the test if the check is still running after limit.
func checkWithin(t *testing.T, d Driver, limit time.Duration) *CheckResult {
	t.Helper()
	done := make(chan *CheckResult, 1)
	go func() {
		res, err := d.Check()
		if err != nil {
			res = &CheckResult{Message: err.Error()}
		}
		done <- res
	}()
	select {
	case res := <-done:
		return res
	case <-time.After(limit):
		t.Fatalf("check still running after %v", limit)
		return nil
	}
}

// stalledSMTP stops answering before the greeting, or at the command named
// by stallAt.
func stalledSMTP(greeting bool, stallAt string) func(net.Conn) {
	return func(c net.Conn) {
		if !greeting {
			_, _ = io.Copy(io.Discard, c)
			return
		}
		fmt.Fprintf(c, "220 test ESMTP\r\n")
		r := bufio.NewReader(c)
		for {
			line, err := r.ReadString('\n')
			if err != nil {
				return
			}
			cmd := strings.ToUpper(strings.TrimSpace(line))
			if stallAt != "" && strings.HasPrefix(cmd, stallAt) {
				_, _ = io.Copy(io.Discard, r)
				return
			}
			switch {
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
}

func TestSMTPDriverGivesUpOnStalledServer(t *testing.T) {
	withCheckTimeout(t, 200*time.Millisecond)
	tests := []struct {
		name  string
		serve familyServer
		check v1.SmtpCheck
	}{
		{"no greeting", tcpServer(stalledSMTP(false, "")), v1.SmtpCheck{}},
		{"stalls at EHLO", tcpServer(stalledSMTP(true, "EHLO")), v1.SmtpCheck{Helo: "test"}},
		{"stalls at VRFY", tcpServer(stalledSMTP(true, "VRFY")), v1.SmtpCheck{Helo: "test", VerifyAssertion: "postmaster"}},
		{"stalls at MAIL", tcpServer(stalledSMTP(true, "MAIL")), v1.SmtpCheck{FromAssertion: "monitor@example.test"}},
		{"no greeting over TLS", tlsServer(stalledSMTP(false, "")), v1.SmtpCheck{Tls: true}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			endpoint, _ := tc.serve(t, "tcp4")
			check := tc.check
			d, err := NewSMTPDriver(endpoint, "", &check, "default", nil)
			if err != nil {
				t.Fatal(err)
			}
			res := checkWithin(t, d, 5*time.Second)
			if res.Success || !strings.Contains(res.Message, "i/o timeout") {
				t.Fatalf("success=%v %s", res.Success, res.Message)
			}
		})
	}
}

func TestSMTPDriverStillPassesWithinTimeout(t *testing.T) {
	withCheckTimeout(t, 2*time.Second)
	endpoint, _ := tcpServer(stalledSMTP(true, ""))(t, "tcp4")
	d, _ := NewSMTPDriver(endpoint, "", &v1.SmtpCheck{Helo: "test", VerifyAssertion: "postmaster", FromAssertion: "m@example.test", ToAssertion: "r@example.test"}, "default", nil)
	if res := checkWithin(t, d, 5*time.Second); !res.Success {
		t.Fatalf("a responsive server failed: %s", res.Message)
	}
}

func TestMySQLDriverGivesUpOnStalledServer(t *testing.T) {
	withCheckTimeout(t, 200*time.Millisecond)
	for _, greeting := range []bool{false, true} {
		t.Run(fmt.Sprintf("handshake sent=%v", greeting), func(t *testing.T) {
			endpoint, accepted := tcpServer(func(c net.Conn) {
				if greeting {
					// A packet header promising more bytes than are ever sent.
					_, _ = c.Write([]byte{0xff, 0x00, 0x00, 0x00, 0x0a})
				}
				_, _ = io.Copy(io.Discard, c)
			})(t, "tcp4")
			d, _ := NewMYSQLDriver(endpoint, "", nil, "default", nil)
			res := checkWithin(t, d, 10*time.Second)
			if res.Success || !waitAccepted(accepted) {
				t.Fatalf("success=%v accepted=%d %s", res.Success, accepted.Load(), res.Message)
			}
			time.Sleep(100 * time.Millisecond)
			if accepted.Load() != 1 {
				t.Fatalf("%d connections; once the check's deadline passes it must not dial again", accepted.Load())
			}
		})
	}
}

func TestMySQLDriverGivesUpOnTricklingServer(t *testing.T) {
	withCheckTimeout(t, 200*time.Millisecond)
	endpoint, _ := tcpServer(func(c net.Conn) {
		// A packet header promising 255 bytes, then one byte every 50ms, so
		// no single read waits long enough to hit a read timeout.
		if _, err := c.Write([]byte{0xff, 0x00, 0x00, 0x00}); err != nil {
			return
		}
		for range 255 {
			time.Sleep(50 * time.Millisecond)
			if _, err := c.Write([]byte{0x0a}); err != nil {
				return
			}
		}
	})(t, "tcp4")
	d, _ := NewMYSQLDriver(endpoint, "", nil, "default", nil)
	start := time.Now()
	res := checkWithin(t, d, 5*time.Second)
	if res.Success {
		t.Fatalf("a trickling server passed: %s", res.Message)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("took %v with a 200ms timeout", elapsed)
	}
}

func TestDNSDriverGivesUpOnSilentServer(t *testing.T) {
	withCheckTimeout(t, 200*time.Millisecond)
	pc, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { pc.Close() })
	d, _ := NewDNSDriver("example.test", "", &v1.DnsCheck{Server: pc.LocalAddr().String(), Type: "A"})
	start := time.Now()
	res := checkWithin(t, d, 3*time.Second)
	if res.Success || !strings.Contains(res.Message, "i/o timeout") {
		t.Fatalf("success=%v %s", res.Success, res.Message)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("took %v with a 200ms timeout", elapsed)
	}
}

func TestDNSDriverStillAnswersWithinTimeout(t *testing.T) {
	withCheckTimeout(t, 2*time.Second)
	server, _ := dnsServer(t, "udp4")
	d, _ := NewDNSDriver("example.test", "", &v1.DnsCheck{Server: server, Type: "A", Verify: "192.0.2.1"})
	if res := checkWithin(t, d, 5*time.Second); !res.Success {
		t.Fatalf("a responsive DNS server failed: %s", res.Message)
	}
}
