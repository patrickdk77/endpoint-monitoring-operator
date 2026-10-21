package driver

import (
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"net/netip"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	gocql "github.com/apache/cassandra-gocql-driver/v2"
	apierrors "k8s.io/apimachinery/pkg/api/errors"

	v1 "github.com/patrickdk77/endpoint-monitoring-operator/api/v1alpha1"
)

func cqlString(s string) []byte {
	return append(binary.BigEndian.AppendUint16(nil, uint16(len(s))), s...)
}

func cqlBytes(b []byte) []byte {
	if b == nil {
		return []byte{0xff, 0xff, 0xff, 0xff}
	}
	return append(binary.BigEndian.AppendUint32(nil, uint32(len(b))), b...)
}

func cqlError(code uint32, msg string) []byte {
	return append(binary.BigEndian.AppendUint32(nil, code), cqlString(msg)...)
}

type cqlColumn struct {
	name string
	kind uint16
}

// cqlRows encodes a Rows result from system.local with the given columns.
func cqlRows(cols []cqlColumn, cells ...[]byte) []byte {
	out := binary.BigEndian.AppendUint32(nil, 2)
	out = binary.BigEndian.AppendUint32(out, 1)
	out = binary.BigEndian.AppendUint32(out, uint32(len(cols)))
	out = append(out, cqlString("system")...)
	out = append(out, cqlString("local")...)
	for _, c := range cols {
		out = append(out, cqlString(c.name)...)
		out = binary.BigEndian.AppendUint16(out, c.kind)
	}
	out = binary.BigEndian.AppendUint32(out, 1)
	for _, cell := range cells {
		out = append(out, cqlBytes(cell)...)
	}
	return out
}

const (
	cqlVarchar = 0x000D
	cqlUUID    = 0x000C
)

// fakeCQL speaks enough of the CQL native protocol v4 for gocql: OPTIONS,
// STARTUP with a PasswordAuthenticator login when wantUser is set, and the
// two system.local queries, sent directly or prepared and executed, which
// report bootstrapped. Any other request fails. With answerQueries unset it
// never answers the driver's own query, which gocql prepares. A direct query, such as the control connection's, waits delay
// before its reply.
func fakeCQL(wantUser, wantPass, bootstrapped string, answerQueries bool, delay time.Duration) func(net.Conn) {
	local := []cqlColumn{{"key", cqlVarchar}, {"bootstrapped", cqlVarchar}, {"cluster_name", cqlVarchar}, {"data_center", cqlVarchar},
		{"host_id", cqlUUID}, {"rack", cqlVarchar}, {"release_version", cqlVarchar}}
	hostID := []byte{0x7b, 0x2e, 0x63, 0x8b, 0x1a, 0x52, 0x4d, 0x2c, 0x9a, 0x41, 0x5d, 0x07, 0x3e, 0x21, 0xc0, 0x11}
	columns := func(query string) []cqlColumn {
		switch {
		case strings.HasPrefix(query, "SELECT bootstrapped FROM system.local"):
			return []cqlColumn{{"bootstrapped", cqlVarchar}}
		case strings.HasPrefix(query, "SELECT * FROM system.local"):
			return local
		}
		return nil
	}
	answer := func(query string) (byte, []byte) {
		cols := columns(query)
		switch len(cols) {
		case 0:
			return 0x00, cqlError(0x2200, "unsupported query")
		case 1:
			return 0x08, cqlRows(cols, []byte(bootstrapped))
		}
		return 0x08, cqlRows(cols, []byte("local"), []byte(bootstrapped), []byte("test"), []byte("dc1"), hostID, []byte("rack1"), []byte("3.0.8"))
	}
	return func(c net.Conn) {
		prepared := map[string]string{}
		for {
			head := make([]byte, 9)
			if _, err := io.ReadFull(c, head); err != nil {
				return
			}
			body := make([]byte, binary.BigEndian.Uint32(head[5:]))
			if _, err := io.ReadFull(c, body); err != nil {
				return
			}
			op, out := byte(0x00), cqlError(0x000A, "unsupported request")
			switch {
			case head[0] != 0x04:
				out = cqlError(0x000A, "Invalid or unsupported protocol version")
			case head[4] == 0x05: // OPTIONS
				op = 0x06
				out = binary.BigEndian.AppendUint16(nil, 1)
				out = append(out, cqlString("CQL_VERSION")...)
				out = binary.BigEndian.AppendUint16(out, 1)
				out = append(out, cqlString("3.4.5")...)
			case head[4] == 0x01: // STARTUP
				op, out = 0x02, nil
				if wantUser != "" {
					op, out = 0x03, cqlString("org.apache.cassandra.auth.PasswordAuthenticator")
				}
			case head[4] == 0x0F: // AUTH_RESPONSE
				if len(body) >= 4 && string(body[4:]) == "\x00"+wantUser+"\x00"+wantPass {
					op, out = 0x10, cqlBytes(nil)
				} else {
					out = cqlError(0x0100, "Provided username and/or password are incorrect")
				}
			case head[4] == 0x0B: // REGISTER
				op, out = 0x02, nil
			case head[4] == 0x07: // QUERY
				op, out = answer(string(body[4 : 4+binary.BigEndian.Uint32(body)]))
			case head[4] == 0x09: // PREPARE
				query := string(body[4 : 4+binary.BigEndian.Uint32(body)])
				if !answerQueries && strings.HasPrefix(query, "SELECT bootstrapped") {
					_, _ = io.Copy(io.Discard, c)
					return
				}
				cols := columns(query)
				if cols == nil {
					out = cqlError(0x2200, "unsupported query")
					break
				}
				id := fmt.Sprintf("%016d", len(prepared))
				prepared[id] = query
				op = 0x08
				out = binary.BigEndian.AppendUint32(nil, 4)
				out = append(out, cqlString(id)...)
				out = append(out, make([]byte, 12)...) // no bound variables
				result := cqlRows(cols)
				out = append(out, result[4:len(result)-4]...)
			case head[4] == 0x0A: // EXECUTE
				id := string(body[2 : 2+binary.BigEndian.Uint16(body)])
				if query, ok := prepared[id]; ok {
					op, out = answer(query)
				} else {
					out = cqlError(0x2500, "unprepared statement")
				}
			}
			if head[4] == 0x07 {
				time.Sleep(delay)
			}
			reply := []byte{0x84, 0, head[2], head[3], op}
			reply = binary.BigEndian.AppendUint32(reply, uint32(len(out)))
			if _, err := c.Write(append(reply, out...)); err != nil {
				return
			}
		}
	}
}

func TestScyllaDBDriverChecksTheNode(t *testing.T) {
	tests := []struct {
		name               string
		secret             map[string]string
		wantUser, wantPass string
		bootstrapped       string
		wantInMsg          string
	}{
		{"bootstrapped without a login", nil, "", "", "COMPLETED", "SCYLLADB check successful"},
		{"bootstrapped with a login", map[string]string{"username": "monitor", "password": "p@ss:w/rd"}, "monitor", "p@ss:w/rd", "COMPLETED", "SCYLLADB check successful"},
		{"still joining", nil, "", "", "IN_PROGRESS", "the node is not bootstrapped: bootstrapped is IN_PROGRESS"},
		{"decommissioned", nil, "", "", "DECOMMISSIONED", "bootstrapped is DECOMMISSIONED"},
		{"wrong password", map[string]string{"username": "monitor", "password": "wrong password"}, "monitor", "right password", "COMPLETED", "are incorrect"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			endpoint, _ := tcpServer(fakeCQL(tc.wantUser, tc.wantPass, tc.bootstrapped, true, 0))(t, "tcp4")
			var check *v1.ScylladbCheck
			if tc.secret != nil {
				check = &v1.ScylladbCheck{SecretRef: v1.SecretRef{Name: "creds"}}
			}
			d, err := NewScyllaDBDriver(endpoint, "", check, "default", secretClient(t, tc.secret))
			if err != nil {
				t.Fatal(err)
			}
			res := checkWithin(t, d, 10*time.Second)
			if !strings.Contains(res.Message, tc.wantInMsg) || res.Success != strings.Contains(tc.wantInMsg, "successful") {
				t.Fatalf("success=%v %s", res.Success, res.Message)
			}
			if pass := tc.secret["password"]; pass != "" && strings.Contains(res.Message, pass) {
				t.Fatalf("the message carries the password: %s", res.Message)
			}
			if d.GetType() != "scylladb" || d.GetEndpoint() != endpoint {
				t.Fatalf("type %s endpoint %s", d.GetType(), d.GetEndpoint())
			}
		})
	}
}

func TestScyllaDBCluster(t *testing.T) {
	scyllaLookup = func(_ context.Context, network, host string) ([]netip.Addr, error) {
		if host != "db.example.test" {
			return nil, fmt.Errorf("lookup %s: no such host", host)
		}
		v4, v6 := netip.MustParseAddr("192.0.2.1"), netip.MustParseAddr("2001:db8::1")
		switch network {
		case "ip4":
			return []netip.Addr{v4}, nil
		case "ip6":
			return []netip.Addr{v6}, nil
		}
		return []netip.Addr{v4, v6}, nil
	}
	t.Cleanup(func() { scyllaLookup = nil })
	tests := []struct {
		endpoint, family, user, pass string
		wantHosts                    []string
	}{
		{"db.example.test", "", "", "", []string{"192.0.2.1:9042", "[2001:db8::1]:9042"}},
		{"db.example.test:9043", "", "monitor", "secret", []string{"192.0.2.1:9043", "[2001:db8::1]:9043"}},
		{"db.example.test", FamilyIPv4, "", "", []string{"192.0.2.1:9042"}},
		{"db.example.test", FamilyIPv6, "", "", []string{"[2001:db8::1]:9042"}},
		{"[::1]:9044", FamilyIPv6, "", "", []string{"[::1]:9044"}},
		{"127.0.0.1", "", "", "", []string{"127.0.0.1:9042"}},
	}
	for _, tc := range tests {
		s := &ScyllaDBDriver{endpoint: tc.endpoint, family: tc.family, username: tc.user, password: tc.pass}
		c, err := s.cluster(context.Background())
		if err != nil {
			t.Fatalf("%s: %v", tc.endpoint, err)
		}
		if fmt.Sprint(c.Hosts) != fmt.Sprint(tc.wantHosts) || c.ProtoVersion != 4 || c.NumConns != 1 || c.Consistency != gocql.One {
			t.Fatalf("%s %q: hosts %v proto %d conns %d consistency %v", tc.endpoint, tc.family, c.Hosts, c.ProtoVersion, c.NumConns, c.Consistency)
		}
		if !c.DisableInitialHostLookup || !c.Events.DisableNodeStatusEvents || !c.Events.DisableTopologyEvents || !c.Events.DisableSchemaEvents || c.Metadata.CacheMode != gocql.Disabled {
			t.Fatalf("%s: discovers the cluster or its schema", tc.endpoint)
		}
		if c.Timeout != 2*checkTimeout || c.ConnectTimeout != checkTimeout || c.ReconnectionPolicy.GetMaxRetries() != 1 {
			t.Fatalf("%s: timeout %v connect %v retries %d", tc.endpoint, c.Timeout, c.ConnectTimeout, c.ReconnectionPolicy.GetMaxRetries())
		}
		if tc.user == "" && c.Authenticator != nil {
			t.Fatalf("%s: logs in without a user", tc.endpoint)
		}
		if a, ok := c.Authenticator.(gocql.PasswordAuthenticator); tc.user != "" && (!ok || a.Username != tc.user || a.Password != tc.pass) {
			t.Fatalf("%s: authenticator %#v", tc.endpoint, c.Authenticator)
		}
		if (c.Dialer != nil) != (tc.family != "") {
			t.Fatalf("%s: dialer set %v with family %q", tc.endpoint, c.Dialer != nil, tc.family)
		}
	}
	if _, err := (&ScyllaDBDriver{endpoint: "no-such-host.invalid"}).cluster(context.Background()); err == nil {
		t.Fatal("a name that does not resolve was accepted")
	}
}

func TestScyllaDBFailureTextIgnoresTheHostOrder(t *testing.T) {
	ln, err := net.Listen("tcp4", "127.0.0.2:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			if _, err := io.ReadFull(c, make([]byte, 9)); err == nil {
				_ = c.(*net.TCPConn).SetLinger(0)
			}
			c.Close()
		}
	}()
	_, port, _ := net.SplitHostPort(ln.Addr().String())
	scyllaLookup = func(context.Context, string, string) ([]netip.Addr, error) {
		return []netip.Addr{netip.MustParseAddr("127.0.0.2"), netip.MustParseAddr("127.0.0.3")}, nil
	}
	t.Cleanup(func() { scyllaLookup = nil })
	d, _ := NewScyllaDBDriver("db.example.test:"+port, "", nil, "default", nil)
	first, _ := d.Check()
	if first.Success || strings.Contains(first.Message, "127.0.0.") || !strings.Contains(first.Message, "connection refused") || !strings.Contains(first.Message, "; ") {
		t.Fatalf("failure text %q; want both hosts' causes without their addresses", first.Message)
	}
	for range 8 {
		if res, _ := d.Check(); res.Message != first.Message {
			t.Fatalf("failure texts %q and %q differ between checks", first.Message, res.Message)
		}
	}
}

func TestScyllaDBFailureTextNamesASharedCauseOnce(t *testing.T) {
	ln, err := net.Listen("tcp4", "127.0.0.2:0")
	if err != nil {
		t.Fatal(err)
	}
	_, port, _ := net.SplitHostPort(ln.Addr().String())
	ln.Close()
	scyllaLookup = func(context.Context, string, string) ([]netip.Addr, error) {
		return []netip.Addr{netip.MustParseAddr("127.0.0.2"), netip.MustParseAddr("127.0.0.3")}, nil
	}
	t.Cleanup(func() { scyllaLookup = nil })
	d, _ := NewScyllaDBDriver("db.example.test:"+port, "", nil, "default", nil)
	res, _ := d.Check()
	if want := "unable to connect to initial hosts: dial tcp: connect: connection refused"; res.Success || !strings.HasSuffix(res.Message, want) {
		t.Fatalf("failure text %q; want it to end with %q", res.Message, want)
	}
}

func TestScyllaDBDriverClosesASessionThatOpensTooLate(t *testing.T) {
	withCheckTimeout(t, 200*time.Millisecond)
	var open atomic.Int32
	serve := fakeCQL("", "", "COMPLETED", true, 300*time.Millisecond)
	endpoint, accepted := tcpServer(func(c net.Conn) {
		open.Add(1)
		defer open.Add(-1)
		serve(c)
	})(t, "tcp4")
	d, _ := NewScyllaDBDriver(endpoint, "", nil, "default", nil)
	if res := checkWithin(t, d, 5*time.Second); res.Success || !strings.Contains(res.Message, "deadline exceeded") {
		t.Fatalf("success=%v %s; want the check to give up before the session opens", res.Success, res.Message)
	}
	deadline := time.Now().Add(5 * time.Second)
	for (open.Load() != 0 || accepted.Load() < 2) && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if n := open.Load(); n != 0 || accepted.Load() < 2 {
		t.Fatalf("%d of %d connections still open; the late session was not closed", n, accepted.Load())
	}
}

func TestScyllaDBHonorsFamily(t *testing.T) {
	withCheckTimeout(t, 2*time.Second)
	for _, listenNet := range []string{"tcp4", "tcp6"} {
		own, other := FamilyIPv4, FamilyIPv6
		if listenNet == "tcp6" {
			own, other = FamilyIPv6, FamilyIPv4
		}
		for _, family := range []string{"", own, other} {
			t.Run(fmt.Sprintf("%s/%q", listenNet, family), func(t *testing.T) {
				endpoint, accepted := tcpServer(fakeCQL("", "", "COMPLETED", true, 0))(t, listenNet)
				d, err := NewScyllaDBDriver(endpoint, family, nil, "default", nil)
				if err != nil {
					t.Fatal(err)
				}
				res := checkWithin(t, d, 10*time.Second)
				if family == other {
					if res.Success || accepted.Load() != 0 || !strings.Contains(res.Message, "no suitable address") {
						t.Fatalf("family %s reached a %s listener: success=%v accepted=%d %s", family, listenNet, res.Success, accepted.Load(), res.Message)
					}
					return
				}
				if !res.Success {
					t.Fatalf("check failed: %s", res.Message)
				}
			})
		}
	}
}

func TestScyllaDBDriverClosesItsConnections(t *testing.T) {
	var open atomic.Int32
	serve := fakeCQL("", "", "COMPLETED", true, 0)
	endpoint, accepted := tcpServer(func(c net.Conn) {
		open.Add(1)
		defer open.Add(-1)
		serve(c)
	})(t, "tcp4")
	d, _ := NewScyllaDBDriver(endpoint, "", nil, "default", nil)
	if res, _ := d.Check(); !res.Success {
		t.Fatalf("check failed: %s", res.Message)
	}
	deadline := time.Now().Add(2 * time.Second)
	for open.Load() != 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if n := open.Load(); n != 0 || accepted.Load() == 0 {
		t.Fatalf("%d of %d connections still open after the check", n, accepted.Load())
	}
}

func TestScyllaDBDriverSecret(t *testing.T) {
	d, err := NewScyllaDBDriver("127.0.0.1:9042", "", &v1.ScylladbCheck{SecretRef: v1.SecretRef{Name: "creds"}}, "default",
		secretClient(t, map[string]string{"username": "monitor", "password": "secret"}))
	if err != nil {
		t.Fatal(err)
	}
	if s := d.(*ScyllaDBDriver); s.username != "monitor" || s.password != "secret" {
		t.Fatalf("credentials %q %q", s.username, s.password)
	}
	_, err = NewScyllaDBDriver("127.0.0.1:9042", "", &v1.ScylladbCheck{SecretRef: v1.SecretRef{Name: "absent"}}, "default", secretClient(t, nil))
	if err == nil || !apierrors.IsNotFound(err) {
		t.Fatalf("err = %v, want a wrapped NotFound", err)
	}
	if _, err := NewScyllaDBDriver("", "", nil, "default", nil); err == nil {
		t.Fatal("an empty endpoint was accepted")
	}
}
