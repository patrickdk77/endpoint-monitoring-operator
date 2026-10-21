package driver

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	apierrors "k8s.io/apimachinery/pkg/api/errors"

	v1 "github.com/patrickdk77/endpoint-monitoring-operator/api/v1alpha1"
)

// fakeAdmin answers each command, by name, with a document or an error.
type fakeAdmin map[string]any

func (f fakeAdmin) RunCommand(_ context.Context, cmd bson.D) (bson.Raw, error) {
	reply, ok := f[cmd[0].Key]
	if !ok {
		return nil, fmt.Errorf("unexpected command %s", cmd[0].Key)
	}
	if err, ok := reply.(error); ok {
		return nil, err
	}
	return bson.Marshal(reply)
}

var (
	mongoPrimary   = bson.M{"isWritablePrimary": true}
	mongoSecondary = bson.M{"isWritablePrimary": false, "secondary": true}
	mongoArbiter   = bson.M{"isWritablePrimary": false, "arbiterOnly": true}
)

func replStatus(lag time.Duration, members ...bson.M) bson.M {
	if members == nil {
		at := time.Date(2026, 10, 20, 10, 0, 0, 0, time.UTC)
		members = []bson.M{
			{"state": 1, "optimeDate": at},
			{"state": 2, "self": true, "optimeDate": at.Add(-lag)},
			{"state": 2, "optimeDate": at.Add(-time.Hour)},
		}
	}
	return bson.M{"ok": 1, "members": members}
}

func TestCheckMongoDB(t *testing.T) {
	maxLag := 10
	replica := &v1.MongodbCheck{MaxSecondsBehindMaster: &maxLag}
	tests := []struct {
		name    string
		admin   fakeAdmin
		check   *v1.MongodbCheck
		wantErr string
	}{
		{"writable primary", fakeAdmin{"hello": mongoPrimary}, nil, ""},
		{"primary with an empty check", fakeAdmin{"hello": mongoPrimary}, &v1.MongodbCheck{Tls: true}, ""},
		{"secondary where a primary is expected", fakeAdmin{"hello": mongoSecondary}, nil, "expected to be a writable primary, but it is a secondary"},
		{"arbiter where a primary is expected", fakeAdmin{"hello": mongoArbiter}, nil, "but it is an arbiter"},
		{"neither", fakeAdmin{"hello": bson.M{}}, nil, "but it is neither a primary nor a secondary"},
		{"hello fails", fakeAdmin{"hello": errors.New("boom")}, nil, "failed to run hello: boom"},
		{"hello unreadable", fakeAdmin{"hello": bson.M{"isWritablePrimary": "yes"}}, nil, "failed to read hello"},
		{"secondary within the lag", fakeAdmin{"hello": mongoSecondary, "replSetGetStatus": replStatus(3 * time.Second)}, replica, ""},
		{"secondary at the limit", fakeAdmin{"hello": mongoSecondary, "replSetGetStatus": replStatus(10 * time.Second)}, replica, ""},
		{"secondary past the limit", fakeAdmin{"hello": mongoSecondary, "replSetGetStatus": replStatus(11 * time.Second)}, replica, "the replication lag exceeds max allowed (10)"},
		{"primary where a secondary is expected", fakeAdmin{"hello": mongoPrimary}, replica, "expected to be a secondary, but it is a writable primary"},
		{"arbiter where a secondary is expected", fakeAdmin{"hello": mongoArbiter}, replica, "expected to be a secondary, but it is an arbiter"},
		{"status not allowed", fakeAdmin{"hello": mongoSecondary, "replSetGetStatus": errors.New("not authorized on admin")}, replica, "failed to run replSetGetStatus: not authorized on admin"},
		{"status unreadable", fakeAdmin{"hello": mongoSecondary, "replSetGetStatus": bson.M{"members": "none"}}, replica, "failed to read replSetGetStatus"},
		{"status without this server", fakeAdmin{"hello": mongoSecondary, "replSetGetStatus": replStatus(0, bson.M{"state": 1, "optimeDate": time.Now()})}, replica, "does not list this server"},
		{"no primary", fakeAdmin{"hello": mongoSecondary, "replSetGetStatus": replStatus(0, bson.M{"state": 2, "self": true, "optimeDate": time.Now()})}, replica, "the replica set has no primary"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := checkMongoDB(context.Background(), tc.admin, tc.check)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("error %v, want one containing %q", err, tc.wantErr)
			}
		})
	}
}

func TestMongoDBOptions(t *testing.T) {
	tests := []struct {
		endpoint, family, user, pass string
		tls                          bool
		wantHost                     string
	}{
		{"db.example.test", "", "", "", false, "db.example.test:27017"},
		{"db.example.test:27018", FamilyIPv4, "monitor", "secret", true, "db.example.test:27018"},
		{"[::1]:27019", FamilyIPv6, "mon:itor@ops", "p@ss:w/rd?x=1&y", false, "[::1]:27019"},
		{"::1", "", "monitor", "", false, "[::1]:27017"},
	}
	for _, tc := range tests {
		m := &MongoDBDriver{endpoint: tc.endpoint, family: tc.family, username: tc.user, password: tc.pass, check: &v1.MongodbCheck{Tls: tc.tls}}
		opts := m.options()
		if len(opts.Hosts) != 1 || opts.Hosts[0] != tc.wantHost || opts.Direct == nil || !*opts.Direct {
			t.Fatalf("%s: hosts %v direct %v", tc.endpoint, opts.Hosts, opts.Direct)
		}
		if tc.user == "" {
			if opts.Auth != nil {
				t.Fatalf("%s: authenticates without a user: %+v", tc.endpoint, opts.Auth)
			}
		} else if a := opts.Auth; a == nil || a.AuthSource != "admin" || a.Username != tc.user || a.Password != tc.pass || a.PasswordSet != (tc.pass != "") {
			t.Fatalf("%s: credential %+v", tc.endpoint, a)
		}
		if (opts.TLSConfig != nil) != tc.tls {
			t.Fatalf("%s: TLS %v, want %v", tc.endpoint, opts.TLSConfig != nil, tc.tls)
		}
		if tc.tls && opts.TLSConfig.ServerName != "db.example.test" {
			t.Fatalf("%s: TLS server name %q", tc.endpoint, opts.TLSConfig.ServerName)
		}
		if (opts.Dialer != nil) != (tc.family != "") {
			t.Fatalf("%s: dialer set %v with family %q", tc.endpoint, opts.Dialer != nil, tc.family)
		}
		if *opts.ConnectTimeout != 2*checkTimeout || *opts.ServerSelectionTimeout != 2*checkTimeout || opts.RetryReads == nil || *opts.RetryReads {
			t.Fatalf("%s: timeouts %v %v retry %v", tc.endpoint, *opts.ConnectTimeout, *opts.ServerSelectionTimeout, opts.RetryReads)
		}
	}
}

// fakeMongo answers the driver's handshake as hello describes the server,
// and each later command from replies, by name; other commands fail. With
// no replies it never answers a command after the handshake.
func fakeMongo(hello bson.M, replies map[string]bson.M) func(net.Conn) {
	return func(c net.Conn) {
		handshake := bson.M{
			"ok": 1, "ismaster": hello["isWritablePrimary"], "maxWireVersion": int32(21), "minWireVersion": int32(0),
			"maxBsonObjectSize": int32(16777216), "maxMessageSizeBytes": int32(48000000), "maxWriteBatchSize": int32(100000),
			"localTime": time.Now(), "logicalSessionTimeoutMinutes": int32(30), "connectionId": int32(1),
		}
		for k, v := range hello {
			handshake[k] = v
		}
		for {
			head := make([]byte, 16)
			if _, err := io.ReadFull(c, head); err != nil {
				return
			}
			body := make([]byte, binary.LittleEndian.Uint32(head)-16)
			if _, err := io.ReadFull(c, body); err != nil {
				return
			}
			requestID, opCode := binary.LittleEndian.Uint32(head[4:]), binary.LittleEndian.Uint32(head[12:])
			var cmd bson.Raw
			switch opCode {
			case 2004: // OP_QUERY, the first handshake
				rest := body[4:]
				rest = rest[bytes.IndexByte(rest, 0)+9:]
				cmd = bson.Raw(rest[:binary.LittleEndian.Uint32(rest)])
			case 2013: // OP_MSG
				cmd = bson.Raw(body[5 : 5+binary.LittleEndian.Uint32(body[5:])])
			default:
				return
			}
			name := cmd.Index(0).Key()
			reply := bson.M{"ok": 0, "errmsg": "no such command: " + name, "code": int32(59)}
			switch {
			case name == "isMaster" || name == "ismaster" || name == "hello" && opCode == 2004:
				reply = handshake
			case replies == nil:
				_, _ = io.Copy(io.Discard, c)
				return
			case name == "hello":
				reply = handshake
			case replies[name] != nil:
				reply = replies[name]
			case name == "endSessions":
				reply = bson.M{"ok": 1}
			}
			doc, err := bson.Marshal(reply)
			if err != nil {
				return
			}
			var out []byte
			if opCode == 2004 {
				out = binary.LittleEndian.AppendUint32(nil, uint32(16+20+len(doc)))
				out = binary.LittleEndian.AppendUint32(out, requestID+1)
				out = binary.LittleEndian.AppendUint32(out, requestID)
				out = binary.LittleEndian.AppendUint32(out, 1) // OP_REPLY
				out = append(out, make([]byte, 16)...)
				out = binary.LittleEndian.AppendUint32(out, 1)
			} else {
				out = binary.LittleEndian.AppendUint32(nil, uint32(16+5+len(doc)))
				out = binary.LittleEndian.AppendUint32(out, requestID+1)
				out = binary.LittleEndian.AppendUint32(out, requestID)
				out = binary.LittleEndian.AppendUint32(out, 2013)
				out = append(out, 0, 0, 0, 0, 0)
			}
			if _, err := c.Write(append(out, doc...)); err != nil {
				return
			}
		}
	}
}

func TestMongoDBDriverChecksTheServer(t *testing.T) {
	maxLag := 10
	tests := []struct {
		name      string
		hello     bson.M
		replies   map[string]bson.M
		check     *v1.MongodbCheck
		wantInMsg string
	}{
		{"primary", mongoPrimary, map[string]bson.M{}, nil, "MONGODB check successful"},
		{"secondary within the lag", mongoSecondary, map[string]bson.M{"replSetGetStatus": replStatus(3 * time.Second)},
			&v1.MongodbCheck{MaxSecondsBehindMaster: &maxLag}, "MONGODB check successful"},
		{"secondary where a primary is expected", mongoSecondary, map[string]bson.M{}, nil, "expected to be a writable primary, but it is a secondary"},
		{"status refused", mongoSecondary, map[string]bson.M{"replSetGetStatus": {"ok": 0, "errmsg": "not authorized on admin to execute command", "code": int32(13), "codeName": "Unauthorized"}},
			&v1.MongodbCheck{MaxSecondsBehindMaster: &maxLag}, "failed to run replSetGetStatus"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			endpoint, _ := tcpServer(fakeMongo(tc.hello, tc.replies))(t, "tcp4")
			d, err := NewMongoDBDriver(endpoint, "", tc.check, "default", nil)
			if err != nil {
				t.Fatal(err)
			}
			res := checkWithin(t, d, 10*time.Second)
			if !strings.Contains(res.Message, tc.wantInMsg) || res.Success != strings.Contains(tc.wantInMsg, "successful") {
				t.Fatalf("success=%v %s", res.Success, res.Message)
			}
			if d.GetType() != "mongodb" || d.GetEndpoint() != endpoint {
				t.Fatalf("type %s endpoint %s", d.GetType(), d.GetEndpoint())
			}
		})
	}
}

func TestMongoDBHonorsFamily(t *testing.T) {
	withCheckTimeout(t, 2*time.Second)
	for _, listenNet := range []string{"tcp4", "tcp6"} {
		own, other := FamilyIPv4, FamilyIPv6
		if listenNet == "tcp6" {
			own, other = FamilyIPv6, FamilyIPv4
		}
		for _, family := range []string{"", own, other} {
			t.Run(fmt.Sprintf("%s/%q", listenNet, family), func(t *testing.T) {
				endpoint, accepted := tcpServer(fakeMongo(mongoPrimary, map[string]bson.M{}))(t, listenNet)
				d, err := NewMongoDBDriver(endpoint, family, nil, "default", nil)
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

func TestMongoDBDriverSecret(t *testing.T) {
	d, err := NewMongoDBDriver("127.0.0.1:27017", "", &v1.MongodbCheck{SecretRef: v1.SecretRef{Name: "creds"}}, "default",
		secretClient(t, map[string]string{"username": "monitor", "password": "secret"}))
	if err != nil {
		t.Fatal(err)
	}
	if m := d.(*MongoDBDriver); m.username != "monitor" || m.password != "secret" {
		t.Fatalf("credentials %q %q", m.username, m.password)
	}
	_, err = NewMongoDBDriver("127.0.0.1:27017", "", &v1.MongodbCheck{SecretRef: v1.SecretRef{Name: "absent"}}, "default", secretClient(t, nil))
	if err == nil || !apierrors.IsNotFound(err) {
		t.Fatalf("err = %v, want a wrapped NotFound", err)
	}
	if _, err := NewMongoDBDriver("", "", nil, "default", nil); err == nil {
		t.Fatal("an empty endpoint was accepted")
	}
}

func TestMongoDBDriverClosesItsConnections(t *testing.T) {
	var open atomic.Int32
	serve := fakeMongo(mongoPrimary, map[string]bson.M{})
	endpoint, accepted := tcpServer(func(c net.Conn) {
		open.Add(1)
		defer open.Add(-1)
		serve(c)
	})(t, "tcp4")
	d, _ := NewMongoDBDriver(endpoint, "", nil, "default", nil)
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

func TestMongoDBDriverFailsAtTheFirstFailedHeartbeat(t *testing.T) {
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	down := ln.Addr().String()
	ln.Close()
	d, _ := NewMongoDBDriver(down, "", nil, "default", nil)
	start := time.Now()
	res := checkWithin(t, d, 10*time.Second)
	if res.Success || !strings.Contains(res.Message, "failed to connect") || !strings.Contains(res.Message, "connection refused") {
		t.Fatalf("success=%v %s", res.Success, res.Message)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("took %v to report a refused connection with a %v check timeout", elapsed, checkTimeout)
	}
}

type slowConn struct {
	net.Conn
	delay time.Duration
}

func (c slowConn) Write(b []byte) (int, error) {
	time.Sleep(c.delay)
	return c.Conn.Write(b)
}

func TestMongoDBDriverIgnoresHeartbeatFailuresOnceTheServerAnswered(t *testing.T) {
	var conns atomic.Int32
	serve := fakeMongo(mongoPrimary, map[string]bson.M{})
	endpoint, _ := tcpServer(func(c net.Conn) {
		if conns.Add(1) == 1 {
			// The monitor connects first: let it answer its handshake, then drop it.
			time.AfterFunc(100*time.Millisecond, func() { c.Close() })
			serve(c)
			return
		}
		serve(slowConn{c, 300 * time.Millisecond})
	})(t, "tcp4")
	d, _ := NewMongoDBDriver(endpoint, "", nil, "default", nil)
	if res := checkWithin(t, d, 10*time.Second); !res.Success {
		t.Fatalf("a dropped monitor connection failed a server that had answered: %s", res.Message)
	}
}
