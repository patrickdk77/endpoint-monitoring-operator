package driver

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgproto3"
	apierrors "k8s.io/apimachinery/pkg/api/errors"

	v1 "github.com/patrickdk77/endpoint-monitoring-operator/api/v1alpha1"
)

type fakePGRow struct {
	vals []any
	err  error
}

func (r fakePGRow) Scan(dest ...any) error {
	if r.err != nil {
		return r.err
	}
	for i, d := range dest {
		switch d := d.(type) {
		case *bool:
			*d = r.vals[i].(bool)
		case *string:
			*d = r.vals[i].(string)
		case **string:
			*d = nil
			if s, ok := r.vals[i].(string); ok {
				*d = &s
			}
		case **float64:
			*d = nil
			if f, ok := r.vals[i].(float64); ok {
				*d = &f
			}
		}
	}
	return nil
}

// fakePG answers each query whose text starts with a known prefix.
type fakePG map[string]fakePGRow

func (f fakePG) QueryRow(_ context.Context, sql string, _ ...any) pgx.Row {
	for prefix, row := range f {
		if strings.HasPrefix(sql, prefix) {
			return row
		}
	}
	return fakePGRow{err: fmt.Errorf("unexpected query %q", sql)}
}

const (
	pgRecovery = "SELECT pg_is_in_recovery()"
	pgReadOnly = "SHOW transaction_read_only"
	pgReceiver = "SELECT status FROM pg_stat_wal_receiver"
	pgLag      = "SELECT CASE WHEN pg_last_wal_receive_lsn()"
)

func row(vals ...any) fakePGRow { return fakePGRow{vals: vals} }

func pgPrimary() fakePG {
	return fakePG{pgRecovery: row(false), pgReadOnly: row("off")}
}

func pgReplica(status any, lag any) fakePG {
	return fakePG{pgRecovery: row(true), pgReceiver: row(status), pgLag: row(lag)}
}

func TestCheckPostgres(t *testing.T) {
	maxLag := 10
	replica := &v1.PostgresCheck{MaxSecondsBehindMaster: &maxLag}
	tests := []struct {
		name    string
		db      fakePG
		check   *v1.PostgresCheck
		wantErr string
	}{
		{"writable primary", pgPrimary(), nil, ""},
		{"primary with an empty check", pgPrimary(), &v1.PostgresCheck{}, ""},
		{"primary in recovery", fakePG{pgRecovery: row(true), pgReadOnly: row("on")}, nil, "expected to be a primary, but it is in recovery"},
		{"read-only primary", fakePG{pgRecovery: row(false), pgReadOnly: row("on")}, nil, "transaction_read_only is on"},
		{"recovery unreadable", fakePG{pgRecovery: {err: errors.New("boom")}}, nil, "failed to get pg_is_in_recovery: boom"},
		{"read_only unreadable", fakePG{pgRecovery: row(false), pgReadOnly: {err: errors.New("boom")}}, nil, "failed to get transaction_read_only: boom"},
		{"streaming replica", pgReplica("streaming", 3.0), replica, ""},
		{"replica caught up", pgReplica("streaming", 0.0), replica, ""},
		{"replica at the limit", pgReplica("streaming", 10.0), replica, ""},
		{"replica past the limit", pgReplica("streaming", 10.5), replica, "the replay lag exceeds max allowed (10)"},
		{"replica that is a primary", fakePG{pgRecovery: row(false)}, replica, "expected to be a replica in recovery, but it is a primary"},
		{"no WAL receiver", fakePG{pgRecovery: row(true), pgReceiver: {err: pgx.ErrNoRows}}, replica, "no WAL receiver is running"},
		{"receiver hidden", pgReplica(nil, 0.0), replica, "grant it pg_monitor"},
		{"receiver waiting", pgReplica("waiting", 0.0), replica, "the WAL receiver is waiting, expected streaming"},
		{"receiver unreadable", fakePG{pgRecovery: row(true), pgReceiver: {err: errors.New("boom")}}, replica, "failed to get the WAL receiver status: boom"},
		{"nothing replayed yet", pgReplica("streaming", nil), replica, "the replay lag is unknown"},
		{"lag unreadable", fakePG{pgRecovery: row(true), pgReceiver: row("streaming"), pgLag: {err: errors.New("boom")}}, replica, "failed to get the replay lag: boom"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := checkPostgres(context.Background(), tc.db, tc.check)
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

func TestPostgresConfig(t *testing.T) {
	for _, env := range []string{"PGUSER", "PGPASSWORD", "PGDATABASE", "PGSSLMODE", "PGHOST", "PGPORT"} {
		t.Setenv(env, "from-the-environment")
	}
	t.Setenv("PGPORT", "1")
	tests := []struct {
		endpoint, user, pass, database string
		wantHost                       string
		wantPort                       uint16
		wantUser, wantDatabase         string
	}{
		{"db.example.test", "", "", "", "db.example.test", 5432, "postgres", "postgres"},
		{"db.example.test:6432", "monitor", "secret", "app", "db.example.test", 6432, "monitor", "app"},
		{"[::1]:5433", "mon:itor@ops", "p@ss:w/rd?x=1&y", "my db", "::1", 5433, "mon:itor@ops", "my db"},
		{"::1", "", "", "", "::1", 5432, "postgres", "postgres"},
		{"192.0.2.1", "", "", "", "192.0.2.1", 5432, "postgres", "postgres"},
	}
	for _, tc := range tests {
		p := &PostgresDriver{endpoint: tc.endpoint, username: tc.user, password: tc.pass, check: &v1.PostgresCheck{Database: tc.database}}
		cfg, err := p.config()
		if err != nil {
			t.Fatalf("%s: %v", tc.endpoint, err)
		}
		if cfg.Host != tc.wantHost || cfg.Port != tc.wantPort || cfg.User != tc.wantUser || cfg.Database != tc.wantDatabase || cfg.Password != tc.pass {
			t.Fatalf("%s: host=%s port=%d user=%s database=%s password=%q", tc.endpoint, cfg.Host, cfg.Port, cfg.User, cfg.Database, cfg.Password)
		}
		if cfg.TLSConfig == nil || len(cfg.Fallbacks) != 1 || cfg.Fallbacks[0].TLSConfig != nil {
			t.Fatalf("%s: want TLS first with a plain fallback, as sslmode=prefer", tc.endpoint)
		}
		if cfg.ConnectTimeout != 2*checkTimeout || cfg.DefaultQueryExecMode != pgx.QueryExecModeSimpleProtocol || cfg.DialFunc == nil {
			t.Fatalf("%s: timeout %v, exec mode %v", tc.endpoint, cfg.ConnectTimeout, cfg.DefaultQueryExecMode)
		}
	}
	if _, err := (&PostgresDriver{endpoint: "db.example.test:notaport"}).config(); err == nil {
		t.Fatal("an endpoint with a bad port was accepted")
	}
}

type pgAnswer struct {
	oid   uint32
	value []byte
}

var pgPrimaryAnswers = map[string]pgAnswer{
	pgRecovery: {16, []byte("f")},
	pgReadOnly: {25, []byte("off")},
}

// pgDialog logs a client in as wantUser to wantDB, asking for a cleartext
// password when wantPass is set, refuses TLS, and answers the queries it
// knows; any other query fails. With no answers it never answers a query.
// It closes loggedOut, when given, once a logged-in client goes away.
func pgDialog(wantUser, wantDB, wantPass string, answers map[string]pgAnswer, loggedOut ...chan struct{}) func(net.Conn) {
	return func(c net.Conn) {
		be := pgproto3.NewBackend(c, c)
		msg, err := be.ReceiveStartupMessage()
		if err != nil {
			return
		}
		if _, ok := msg.(*pgproto3.SSLRequest); ok {
			if _, err := c.Write([]byte("N")); err != nil {
				return
			}
			if msg, err = be.ReceiveStartupMessage(); err != nil {
				return
			}
		}
		startup, ok := msg.(*pgproto3.StartupMessage)
		if !ok {
			return
		}
		fail := func(code, text string) {
			be.Send(&pgproto3.ErrorResponse{Severity: "FATAL", Code: code, Message: text})
			_ = be.Flush()
		}
		user, db := startup.Parameters["user"], startup.Parameters["database"]
		if user != wantUser || db != wantDB {
			fail("28000", fmt.Sprintf("no entry for user %q, database %q", user, db))
			return
		}
		if wantPass != "" {
			be.Send(&pgproto3.AuthenticationCleartextPassword{})
			if be.Flush() != nil || be.SetAuthType(pgproto3.AuthTypeCleartextPassword) != nil {
				return
			}
			m, err := be.Receive()
			if pw, ok := m.(*pgproto3.PasswordMessage); err != nil || !ok || pw.Password != wantPass {
				fail("28P01", fmt.Sprintf("password authentication failed for user %q", user))
				return
			}
		}
		be.Send(&pgproto3.AuthenticationOk{})
		be.Send(&pgproto3.ParameterStatus{Name: "client_encoding", Value: "UTF8"})
		be.Send(&pgproto3.ParameterStatus{Name: "standard_conforming_strings", Value: "on"})
		be.Send(&pgproto3.BackendKeyData{ProcessID: 1, SecretKey: []byte{0, 0, 0, 1}})
		be.Send(&pgproto3.ReadyForQuery{TxStatus: 'I'})
		if be.Flush() != nil {
			return
		}
		for _, ch := range loggedOut {
			defer close(ch)
		}
		if answers == nil {
			_, _ = io.Copy(io.Discard, c)
			return
		}
		for {
			m, err := be.Receive()
			if err != nil {
				return
			}
			q, ok := m.(*pgproto3.Query)
			if !ok {
				return
			}
			var a pgAnswer
			known := false
			for prefix, answer := range answers {
				if strings.HasPrefix(q.String, prefix) {
					a, known = answer, true
				}
			}
			if known {
				be.Send(&pgproto3.RowDescription{Fields: []pgproto3.FieldDescription{{Name: []byte("v"), DataTypeOID: a.oid, DataTypeSize: -1, TypeModifier: -1}}})
				be.Send(&pgproto3.DataRow{Values: [][]byte{a.value}})
				be.Send(&pgproto3.CommandComplete{CommandTag: []byte("SELECT 1")})
			} else {
				be.Send(&pgproto3.ErrorResponse{Severity: "ERROR", Code: "42601", Message: "queries are not answered"})
			}
			be.Send(&pgproto3.ReadyForQuery{TxStatus: 'I'})
			if be.Flush() != nil {
				return
			}
		}
	}
}

func pgReplicaAnswers(lagOID uint32, lag []byte) map[string]pgAnswer {
	return map[string]pgAnswer{
		pgRecovery: {16, []byte("t")},
		pgReceiver: {25, []byte("streaming")},
		pgLag:      {lagOID, lag},
	}
}

func TestPostgresDriverLogsIn(t *testing.T) {
	maxLag := 5
	tests := []struct {
		name                   string
		secret                 map[string]string
		check                  *v1.PostgresCheck
		wantUser, wantDB, pass string
		answers                map[string]pgAnswer
		wantInMsg              string
	}{
		{"defaults without a secret", nil, nil, "postgres", "postgres", "", pgPrimaryAnswers, "POSTGRES check successful"},
		{"user, password and database", map[string]string{"username": "monitor", "password": "p@ss:w/rd?x=1&y"},
			&v1.PostgresCheck{Database: "app"}, "monitor", "app", "p@ss:w/rd?x=1&y", pgPrimaryAnswers, "POSTGRES check successful"},
		{"wrong password", map[string]string{"username": "monitor", "password": "wrong password"},
			nil, "monitor", "postgres", "right password", pgPrimaryAnswers, `password authentication failed for user "monitor"`},
		{"a replica where a primary is expected", nil, nil, "postgres", "postgres", "",
			map[string]pgAnswer{pgRecovery: {16, []byte("t")}}, "expected to be a primary, but it is in recovery"},
		{"a replica with a numeric lag", nil, &v1.PostgresCheck{MaxSecondsBehindMaster: &maxLag}, "postgres", "postgres", "",
			pgReplicaAnswers(1700, []byte("3.25")), "POSTGRES check successful"},
		{"a replica with a float8 lag", nil, &v1.PostgresCheck{MaxSecondsBehindMaster: &maxLag}, "postgres", "postgres", "",
			pgReplicaAnswers(701, []byte("2")), "POSTGRES check successful"},
		{"a replica past the limit", nil, &v1.PostgresCheck{MaxSecondsBehindMaster: &maxLag}, "postgres", "postgres", "",
			pgReplicaAnswers(1700, []byte("9.5")), "the replay lag exceeds max allowed (5)"},
		{"a replica that has replayed nothing", nil, &v1.PostgresCheck{MaxSecondsBehindMaster: &maxLag}, "postgres", "postgres", "",
			pgReplicaAnswers(1700, nil), "the replay lag is unknown"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			endpoint, _ := tcpServer(pgDialog(tc.wantUser, tc.wantDB, tc.pass, tc.answers))(t, "tcp4")
			check := tc.check
			kube := secretClient(t, tc.secret)
			if tc.secret != nil {
				if check == nil {
					check = &v1.PostgresCheck{}
				}
				check.SecretRef = v1.SecretRef{Name: "creds"}
			}
			d, err := NewPostgresDriver(endpoint, "", check, "default", kube)
			if err != nil {
				t.Fatal(err)
			}
			res, _ := d.Check()
			if !strings.Contains(res.Message, tc.wantInMsg) || res.Success != strings.Contains(tc.wantInMsg, "successful") {
				t.Fatalf("success=%v %s", res.Success, res.Message)
			}
			if pass := tc.secret["password"]; pass != "" && strings.Contains(res.Message, pass) {
				t.Fatalf("the message carries the password: %s", res.Message)
			}
			if strings.Contains(res.Message, "refused TLS") || strings.Contains(res.Message, "127.0.0.1") {
				t.Fatalf("the message names the refused TLS attempt or the address: %s", res.Message)
			}
			if d.GetType() != "postgres" || d.GetEndpoint() != endpoint {
				t.Fatalf("type %s endpoint %s", d.GetType(), d.GetEndpoint())
			}
		})
	}
}

func TestPostgresDriverMissingSecret(t *testing.T) {
	_, err := NewPostgresDriver("127.0.0.1:5432", "", &v1.PostgresCheck{SecretRef: v1.SecretRef{Name: "absent"}}, "default", secretClient(t, nil))
	if err == nil || !apierrors.IsNotFound(err) {
		t.Fatalf("err = %v, want a wrapped NotFound", err)
	}
}

func TestPostgresDriverRejectsAURL(t *testing.T) {
	if _, err := NewPostgresDriver("postgres://db.example.test", "", nil, "default", nil); !errors.Is(err, errNotHostPort) {
		t.Fatalf("err = %v, want errNotHostPort", err)
	}
	if _, err := NewPostgresDriver("", "", nil, "default", nil); err == nil {
		t.Fatal("an empty endpoint was accepted")
	}
}

func TestPostgresConfigIgnoresTheEnvironment(t *testing.T) {
	dir := t.TempDir()
	serviceFile := filepath.Join(dir, "pg_service.conf")
	if err := os.WriteFile(serviceFile, []byte("[mon]\noptions=-c default_transaction_read_only=on\ntarget_session_attrs=read-write\nsslmode=require\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	passFile := filepath.Join(dir, "pgpass")
	if err := os.WriteFile(passFile, []byte("*:*:*:*:from-the-passfile\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for env, value := range map[string]string{
		"PGHOST": "elsewhere.test", "PGPORT": "1", "PGDATABASE": "other", "PGUSER": "other", "PGPASSWORD": "from-the-environment",
		"PGPASSFILE": passFile, "PGAPPNAME": "other", "PGSSLMODE": "verify-full", "PGSSLKEY": "/missing.key", "PGSSLCERT": "/missing.crt",
		"PGSSLSNI": "0", "PGSSLROOTCERT": "/missing-root.crt", "PGSSLPASSWORD": "x", "PGSSLNEGOTIATION": "direct",
		"PGTARGETSESSIONATTRS": "read-write", "PGSERVICE": "mon", "PGSERVICEFILE": serviceFile, "PGTZ": "Asia/Tokyo",
		"PGOPTIONS": "-c default_transaction_read_only=on", "PGMINPROTOCOLVERSION": "3.2", "PGMAXPROTOCOLVERSION": "3.2",
		"PGCHANNELBINDING": "require", "PGREQUIREAUTH": "password", "PGCONNECT_TIMEOUT": "abc",
	} {
		t.Setenv(env, value)
	}
	cfg, err := (&PostgresDriver{endpoint: "db.example.test"}).config()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Host != "db.example.test" || cfg.Port != 5432 || cfg.User != "postgres" || cfg.Database != "postgres" || cfg.Password != "" {
		t.Fatalf("host=%s port=%d user=%s database=%s password=%q", cfg.Host, cfg.Port, cfg.User, cfg.Database, cfg.Password)
	}
	if len(cfg.RuntimeParams) != 1 || cfg.RuntimeParams["application_name"] != "endpoint-monitoring-operator" {
		t.Fatalf("startup parameters %v", cfg.RuntimeParams)
	}
	if cfg.ValidateConnect != nil || cfg.ChannelBinding != "prefer" || cfg.RequireAuth != "" || cfg.SSLNegotiation != "postgres" ||
		cfg.MinProtocolVersion != "3.0" || cfg.MaxProtocolVersion != "3.0" {
		t.Fatalf("validate %v, channel binding %s, require auth %q, negotiation %s, protocol %s-%s",
			cfg.ValidateConnect != nil, cfg.ChannelBinding, cfg.RequireAuth, cfg.SSLNegotiation, cfg.MinProtocolVersion, cfg.MaxProtocolVersion)
	}
	if cfg.TLSConfig == nil || !cfg.TLSConfig.InsecureSkipVerify || cfg.TLSConfig.RootCAs != nil || len(cfg.TLSConfig.Certificates) != 0 ||
		cfg.TLSConfig.ServerName != "db.example.test" || len(cfg.Fallbacks) != 1 || cfg.Fallbacks[0].TLSConfig != nil {
		t.Fatalf("TLS %+v, %d fallbacks; want sslmode=prefer without client or root certificates", cfg.TLSConfig, len(cfg.Fallbacks))
	}
}

func TestPostgresConnectFailureTextIgnoresTheAddresses(t *testing.T) {
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
			// Read the whole first message so closing sends a FIN, not a reset.
			pgSSLRequest(c)
			c.Close()
		}
	}()
	_, port, _ := net.SplitHostPort(ln.Addr().String())
	var texts []string
	for _, order := range [][]string{{"127.0.0.2", "127.0.0.3"}, {"127.0.0.3", "127.0.0.2"}} {
		cfg, err := (&PostgresDriver{endpoint: "db.example.test:" + port}).config()
		if err != nil {
			t.Fatal(err)
		}
		cfg.LookupFunc = func(context.Context, string) ([]string, error) { return order, nil }
		_, err = pgx.ConnectConfig(context.Background(), cfg)
		if err == nil {
			t.Fatal("connected to a server that hangs up")
		}
		texts = append(texts, pgConnectText(err))
	}
	if texts[0] != texts[1] || strings.Contains(texts[0], "127.0.0.") || strings.Count(texts[0], "connection refused") != 1 || !strings.Contains(texts[0], "; ") {
		t.Fatalf("failure texts %q and %q; want one text listing both causes without addresses", texts[0], texts[1])
	}
}

func TestPostgresDriverClosesItsConnection(t *testing.T) {
	done := make(chan struct{})
	endpoint, _ := tcpServer(pgDialog("postgres", "postgres", "", pgPrimaryAnswers, done))(t, "tcp4")
	d, _ := NewPostgresDriver(endpoint, "", nil, "default", nil)
	if res, _ := d.Check(); !res.Success {
		t.Fatalf("check failed: %s", res.Message)
	}
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("the check left its connection open")
	}
}

func TestFamilyLookupDropsTheOtherFamily(t *testing.T) {
	both := func(context.Context, string) ([]string, error) { return []string{"::1", "127.0.0.1", "::2"}, nil }
	for _, tc := range []struct {
		family string
		want   []string
	}{
		{FamilyIPv4, []string{"127.0.0.1"}},
		{FamilyIPv6, []string{"::1", "::2"}},
	} {
		got, err := familyLookup(both, tc.family)(context.Background(), "db.example.test")
		if err != nil || !slices.Equal(got, tc.want) {
			t.Fatalf("%s: %v, %v; want %v", tc.family, got, err, tc.want)
		}
	}
	v6 := func(context.Context, string) ([]string, error) { return []string{"::1"}, nil }
	if got, _ := familyLookup(v6, FamilyIPv4)(context.Background(), "::1"); !slices.Equal(got, []string{"::1"}) {
		t.Fatalf("a name without IPv4 addresses gave %v; want its addresses, so the dial reports the family", got)
	}
	failing := func(context.Context, string) ([]string, error) { return nil, errors.New("no such host") }
	if _, err := familyLookup(failing, FamilyIPv4)(context.Background(), "db.example.test"); err == nil {
		t.Fatal("a failed lookup was not reported")
	}
	pgLookup = both
	t.Cleanup(func() { pgLookup = nil })
	cfg, err := (&PostgresDriver{endpoint: "db.example.test", family: FamilyIPv4}).config()
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := cfg.LookupFunc(context.Background(), "db.example.test"); !slices.Equal(got, []string{"127.0.0.1"}) {
		t.Fatalf("config with ipv4 looks the name up as %v", got)
	}
}
