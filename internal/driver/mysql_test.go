package driver

import (
	"bytes"
	"context"
	"crypto/sha1"
	"database/sql"
	"database/sql/driver"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"testing"

	"github.com/go-sql-driver/mysql"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1 "github.com/patrickdk77/endpoint-monitoring-operator/api/v1alpha1"
)

type fakeResult struct {
	cols []string
	rows [][]driver.Value
	err  error
}

// fakeSQL answers each query from a table; queries it does not know fail.
type fakeSQL struct {
	pingErr error
	results map[string]fakeResult
	queries []string
}

func (f *fakeSQL) Connect(context.Context) (driver.Conn, error) { return &fakeConn{f}, nil }
func (f *fakeSQL) Driver() driver.Driver                        { return nil }

type fakeConn struct{ f *fakeSQL }

func (c *fakeConn) Prepare(string) (driver.Stmt, error) { return nil, errors.New("not supported") }
func (c *fakeConn) Close() error                        { return nil }
func (c *fakeConn) Begin() (driver.Tx, error)           { return nil, errors.New("not supported") }
func (c *fakeConn) Ping(context.Context) error          { return c.f.pingErr }

func (c *fakeConn) QueryContext(_ context.Context, query string, _ []driver.NamedValue) (driver.Rows, error) {
	c.f.queries = append(c.f.queries, query)
	r, ok := c.f.results[query]
	if !ok {
		return nil, fmt.Errorf("unexpected query %q", query)
	}
	if r.err != nil {
		return nil, r.err
	}
	return &fakeRows{cols: r.cols, rows: r.rows}, nil
}

type fakeRows struct {
	cols []string
	rows [][]driver.Value
}

func (r *fakeRows) Columns() []string { return r.cols }
func (r *fakeRows) Close() error      { return nil }
func (r *fakeRows) Next(dest []driver.Value) error {
	if len(r.rows) == 0 {
		return io.EOF
	}
	copy(dest, r.rows[0])
	r.rows = r.rows[1:]
	return nil
}

func variable(name, value string) fakeResult {
	return fakeResult{cols: []string{"Variable_name", "Value"}, rows: [][]driver.Value{{name, value}}}
}

func replicaStatus(cols []string, values ...driver.Value) fakeResult {
	return fakeResult{cols: cols, rows: [][]driver.Value{values}}
}

var (
	replicaCols = []string{"Replica_IO_State", "Replica_IO_Running", "Replica_SQL_Running", "Seconds_Behind_Source"}
	slaveCols   = []string{"Slave_IO_State", "Slave_IO_Running", "Slave_SQL_Running", "Seconds_Behind_Master"}
)

func healthyServer(readOnly string) map[string]fakeResult {
	return map[string]fakeResult{
		"SHOW GLOBAL STATUS LIKE 'Threads_connected'":  variable("Threads_connected", "12"),
		"SHOW GLOBAL VARIABLES LIKE 'max_connections'": variable("max_connections", "151"),
		"SHOW GLOBAL VARIABLES LIKE 'read_only'":       variable("read_only", readOnly),
	}
}

func with(base map[string]fakeResult, query string, r fakeResult) map[string]fakeResult {
	out := map[string]fakeResult{}
	for k, v := range base {
		out[k] = v
	}
	out[query] = r
	return out
}

func TestCheckMySQL(t *testing.T) {
	maxBehind := 60
	replica := &v1.MysqlCheck{MaxSecondsBehindMaster: &maxBehind}
	syntaxErr := &mysql.MySQLError{Number: 1064, Message: "You have an error in your SQL syntax"}
	tests := []struct {
		name    string
		pingErr error
		results map[string]fakeResult
		check   *v1.MysqlCheck
		wantErr string
	}{
		{"primary", nil, healthyServer("OFF"), nil, ""},
		{"primary with an empty mysqlCheck", nil, healthyServer("OFF"), &v1.MysqlCheck{}, ""},
		{"ping fails", errors.New("connection refused"), healthyServer("OFF"), nil, "failed to ping: connection refused"},
		{"connections near the limit", nil, with(healthyServer("OFF"), "SHOW GLOBAL STATUS LIKE 'Threads_connected'", variable("Threads_connected", "140")), nil,
			"threads_connected exceeds 90% of max_connections (151)"},
		{"connections at 90 percent", nil, with(with(healthyServer("OFF"), "SHOW GLOBAL STATUS LIKE 'Threads_connected'", variable("Threads_connected", "90")), "SHOW GLOBAL VARIABLES LIKE 'max_connections'", variable("max_connections", "100")), nil, ""},
		{"max_connections unknown", nil, with(healthyServer("OFF"), "SHOW GLOBAL VARIABLES LIKE 'max_connections'", variable("max_connections", "0")), nil, ""},
		{"threads_connected not a number", nil, with(healthyServer("OFF"), "SHOW GLOBAL STATUS LIKE 'Threads_connected'", variable("Threads_connected", "many")), nil, "failed to get threads_connected"},
		{"threads_connected query fails", nil, with(healthyServer("OFF"), "SHOW GLOBAL STATUS LIKE 'Threads_connected'", fakeResult{err: errors.New("denied")}), nil, "failed to get threads_connected: denied"},
		{"primary is read-only", nil, healthyServer("ON"), nil, "expected to NOT be in readonly mode"},
		{"replica is writable", nil, healthyServer("OFF"), replica, "expected to be in readonly mode"},
		{"replica in sync", nil, with(healthyServer("ON"), "SHOW REPLICA STATUS", replicaStatus(replicaCols, "Waiting", "Yes", "Yes", "3")), replica, ""},
		{"replica at the lag limit", nil, with(healthyServer("1"), "SHOW REPLICA STATUS", replicaStatus(replicaCols, "Waiting", "Yes", "Yes", "60")), replica, ""},
		{"replica lagging", nil, with(healthyServer("ON"), "SHOW REPLICA STATUS", replicaStatus(replicaCols, "Waiting", "Yes", "Yes", "99999")), replica,
			"Seconds_Behind_Source exceeds max allowed (60)"},
		{"replica IO stopped", nil, with(healthyServer("ON"), "SHOW REPLICA STATUS", replicaStatus(replicaCols, "", "No", "Yes", nil)), replica, "Replica_IO_Running is No, expected Yes"},
		{"replica SQL stopped", nil, with(healthyServer("ON"), "SHOW REPLICA STATUS", replicaStatus(replicaCols, "Waiting", "Yes", "No", nil)), replica, "Replica_SQL_Running is No, expected Yes"},
		{"replica IO connecting", nil, with(healthyServer("ON"), "SHOW REPLICA STATUS", replicaStatus(replicaCols, "Connecting", "Connecting", "Yes", nil)), replica, "Replica_IO_Running is Connecting, expected Yes"},
		{"lag unknown", nil, with(healthyServer("ON"), "SHOW REPLICA STATUS", replicaStatus(replicaCols, "Waiting", "Yes", "Yes", nil)), replica, "Seconds_Behind_Source is NULL"},
		{"replication not configured", nil, with(healthyServer("ON"), "SHOW REPLICA STATUS", fakeResult{cols: replicaCols}), replica, "replication status is empty"},
		{"replication status denied", nil, with(healthyServer("ON"), "SHOW REPLICA STATUS", fakeResult{err: errors.New("access denied")}), replica, "failed to query replication status: access denied"},
		{"server error other than 1064 does not fall back", nil, with(healthyServer("ON"), "SHOW REPLICA STATUS",
			fakeResult{err: &mysql.MySQLError{Number: 1227, Message: "Access denied; you need the REPLICATION CLIENT privilege"}}), replica,
			"failed to query replication status: Error 1227: Access denied"},
		{"old server falls back to SHOW SLAVE STATUS", nil, with(with(healthyServer("ON"), "SHOW REPLICA STATUS", fakeResult{err: syntaxErr}), "SHOW SLAVE STATUS", replicaStatus(slaveCols, "Waiting", "Yes", "Yes", "5")), replica, ""},
		{"old server lagging", nil, with(with(healthyServer("ON"), "SHOW REPLICA STATUS", fakeResult{err: syntaxErr}), "SHOW SLAVE STATUS", replicaStatus(slaveCols, "Waiting", "Yes", "Yes", "61")), replica,
			"Seconds_Behind_Master exceeds max allowed (60)"},
		{"old server IO stopped", nil, with(with(healthyServer("ON"), "SHOW REPLICA STATUS", fakeResult{err: syntaxErr}), "SHOW SLAVE STATUS", replicaStatus(slaveCols, "", "No", "Yes", nil)), replica, "Slave_IO_Running is No, expected Yes"},
		{"MariaDB names under SHOW REPLICA STATUS", nil, with(healthyServer("ON"), "SHOW REPLICA STATUS", replicaStatus(slaveCols, "Waiting", "Yes", "Yes", "0")), replica, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := &fakeSQL{pingErr: tc.pingErr, results: tc.results}
			db := sql.OpenDB(f)
			defer db.Close()
			err := checkMySQL(context.Background(), db, tc.check)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v (queries %q)", err, f.queries)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("err = %v, want %q (queries %q)", err, tc.wantErr, f.queries)
			}
		})
	}
}

func TestMySQLConfig(t *testing.T) {
	tests := []struct {
		endpoint, family, wantNet, wantAddr string
	}{
		{"db.example.test", "", "tcp", "db.example.test:3306"},
		{"db.example.test:3307", "", "tcp", "db.example.test:3307"},
		{"192.0.2.1", FamilyIPv4, "tcp4", "192.0.2.1:3306"},
		{"::1", FamilyIPv6, "tcp6", "[::1]:3306"},
		{"[::1]:3307", "", "tcp", "[::1]:3307"},
		{"[::1]", "", "tcp", "[::1]:3306"},
	}
	for _, tc := range tests {
		m := &MYSQLDriver{endpoint: tc.endpoint, family: tc.family, username: "monitor@ops", password: "p@ss:w/rd?x=1&y"}
		cfg := m.config()
		if cfg.Net != tc.wantNet || cfg.Addr != tc.wantAddr || cfg.User != "monitor@ops" || cfg.Passwd != "p@ss:w/rd?x=1&y" {
			t.Fatalf("%s: net=%s addr=%s user=%s passwd=%s", tc.endpoint, cfg.Net, cfg.Addr, cfg.User, cfg.Passwd)
		}
		if cfg.Timeout != 2*checkTimeout || cfg.ReadTimeout != 2*checkTimeout || cfg.WriteTimeout != 2*checkTimeout {
			t.Fatalf("%s: timeouts %v %v %v", tc.endpoint, cfg.Timeout, cfg.ReadTimeout, cfg.WriteTimeout)
		}
	}
}

func TestMySQLDriverReportsPingFailure(t *testing.T) {
	for _, tc := range []struct {
		build     func(string, string, *v1.MysqlCheck, string, client.Client) (Driver, error)
		kind      string
		wantInMsg string
	}{
		{NewMYSQLDriver, "mysql", "MYSQL check failed: failed to ping"},
		{NewMariaDBDriver, "mariadb", "MARIADB check failed: failed to ping"},
	} {
		endpoint, _ := tcpServer(func(c net.Conn) {})(t, "tcp4")
		d, _ := tc.build(endpoint, "", nil, "default", nil)
		res, _ := d.Check()
		if res.Success || !strings.Contains(res.Message, tc.wantInMsg) || d.GetType() != tc.kind {
			t.Fatalf("type=%s success=%v %s", d.GetType(), res.Success, res.Message)
		}
	}
}

func TestMySQLDriverMissingSecret(t *testing.T) {
	_, err := NewMYSQLDriver("127.0.0.1:3306", "", &v1.MysqlCheck{SecretRef: v1.SecretRef{Name: "absent"}}, "default", secretClient(t, nil))
	if err == nil || !apierrors.IsNotFound(err) {
		t.Fatalf("err = %v, want a wrapped NotFound", err)
	}
}

// fakeMySQLLogin accepts a mysql_native_password login only for the given
// user and password, answers COM_PING and fails every query.
func fakeMySQLLogin(t *testing.T, wantUser, wantPass string) string {
	t.Helper()
	endpoint, _ := tcpServer(func(c net.Conn) {
		salt := []byte("abcdefghijklmnopqrst")
		write := func(seq byte, payload []byte) {
			h := []byte{byte(len(payload)), byte(len(payload) >> 8), byte(len(payload) >> 16), seq}
			_, _ = c.Write(append(h, payload...))
		}
		read := func() ([]byte, error) {
			h := make([]byte, 4)
			if _, err := io.ReadFull(c, h); err != nil {
				return nil, err
			}
			p := make([]byte, int(h[0])|int(h[1])<<8|int(h[2])<<16)
			_, err := io.ReadFull(c, p)
			return p, err
		}
		fail := func(seq byte, code uint16, msg string) {
			write(seq, append([]byte{0xff, byte(code), byte(code >> 8), '#', '2', '8', '0', '0', '0'}, msg...))
		}
		ok := []byte{0, 0, 0, 2, 0, 0, 0}

		caps := uint32(0x1 | 0x4 | 0x200 | 0x2000 | 0x8000 | 0x20000 | 0x80000)
		hs := append([]byte{0x0a}, "8.0.36\x00"...)
		hs = append(hs, 1, 0, 0, 0)
		hs = append(hs, salt[:8]...)
		hs = append(hs, 0)
		hs = binary.LittleEndian.AppendUint16(hs, uint16(caps))
		hs = append(hs, 0x21, 0x02, 0x00)
		hs = binary.LittleEndian.AppendUint16(hs, uint16(caps>>16))
		hs = append(hs, 21)
		hs = append(hs, make([]byte, 10)...)
		hs = append(hs, salt[8:]...)
		hs = append(hs, 0)
		hs = append(hs, "mysql_native_password\x00"...)
		write(0, hs)

		resp, err := read()
		if err != nil || len(resp) < 33 {
			return
		}
		rest := resp[32:]
		end := bytes.IndexByte(rest, 0)
		if end < 0 || len(rest) < end+2 || len(rest) < end+2+int(rest[end+1]) {
			return
		}
		user, token := string(rest[:end]), rest[end+2:end+2+int(rest[end+1])]
		s1 := sha1.Sum([]byte(wantPass))
		s2 := sha1.Sum(s1[:])
		want := sha1.Sum(append(append([]byte{}, salt...), s2[:]...))
		for i := range want {
			want[i] ^= s1[i]
		}
		if user != wantUser || !bytes.Equal(token, want[:]) {
			fail(2, 1045, "Access denied for user '"+user+"'@'localhost' (using password: YES)")
			return
		}
		write(2, ok)
		for {
			cmd, err := read()
			if err != nil || len(cmd) == 0 || cmd[0] == 0x01 {
				return
			}
			if cmd[0] == 0x0e {
				write(1, ok)
			} else {
				fail(1, 1146, "queries are not answered")
			}
		}
	})(t, "tcp4")
	return endpoint
}

func TestMySQLDriverLogsInWithAnyCredentials(t *testing.T) {
	tests := []struct {
		user, pass, sent string
		wantInMsg        string
	}{
		{"monitor", "plain", "plain", "failed to get threads_connected: Error 1146"},
		{"monitor@ops", "p@ss:w/rd?x=1&y", "p@ss:w/rd?x=1&y", "failed to get threads_connected: Error 1146"},
		{"mon:itor", "plain", "plain", "failed to get threads_connected: Error 1146"},
		{"mon:itor", "p@ss:w/rd?x=1&y", "p@ss:w/rd?x=1&y", "failed to get threads_connected: Error 1146"},
		{"monitor", "right password", "wrong password", "Access denied for user 'monitor'"},
	}
	for _, tc := range tests {
		t.Run(tc.user+"/"+tc.sent, func(t *testing.T) {
			endpoint := fakeMySQLLogin(t, tc.user, tc.pass)
			d := &MYSQLDriver{endpoint: endpoint, username: tc.user, password: tc.sent}
			res, _ := d.Check()
			if res.Success || !strings.Contains(res.Message, tc.wantInMsg) {
				t.Fatalf("success=%v %s", res.Success, res.Message)
			}
			if strings.Contains(res.Message, tc.sent) {
				t.Fatalf("the message carries the password: %s", res.Message)
			}
		})
	}
}
