package driver

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	v1 "github.com/patrickdk77/endpoint-monitoring-operator/api/v1alpha1"
	"github.com/patrickdk77/endpoint-monitoring-operator/internal/notifier"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type PostgresDriver struct {
	endpoint string
	family   string
	username string
	password string
	check    *v1.PostgresCheck
}

func NewPostgresDriver(endpoint, family string, check *v1.PostgresCheck, namespace string, client client.Client) (Driver, error) {
	if endpoint == "" {
		return nil, fmt.Errorf("endpoint cannot be empty")
	}
	if err := checkHostEndpoint(endpoint); err != nil {
		return nil, err
	}

	var username, password string
	if check != nil && check.SecretRef.Name != "" {
		secret, err := notifier.GetSecret(check.SecretRef.Name, namespace, client)
		if err != nil {
			return nil, fmt.Errorf("unable to read secret: %w", err)
		}
		username = string(secret.Data["username"])
		password = string(secret.Data["password"])
	}

	return &PostgresDriver{
		endpoint: endpoint,
		family:   family,
		username: username,
		password: password,
		check:    check,
	}, nil
}

func (p *PostgresDriver) Check() (*CheckResult, error) {
	start := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), checkTimeout)
	defer cancel()

	err := p.run(ctx)
	duration := time.Since(start)
	result := &CheckResult{
		ResponseTime: duration,
	}

	if err != nil {
		result.Success = false
		result.Error = err
		result.Message = fmt.Sprintf("POSTGRES check failed: %s", errorText(err))
		return result, nil
	}

	result.Success = true
	result.Message = fmt.Sprintf("POSTGRES check successful (response time: %v)", duration)

	return result, nil
}

func (p *PostgresDriver) run(ctx context.Context) error {
	cfg, err := p.config()
	if err != nil {
		return fmt.Errorf("failed to initialize: %w", err)
	}
	conn, err := pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		return fmt.Errorf("failed to connect: %w", &textError{text: pgConnectText(err), err: err})
	}
	defer func() {
		closeCtx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = conn.Close(closeCtx)
	}()
	return checkPostgres(ctx, conn, p.check)
}

// config logs in as postgres to the postgres database unless the secret and
// check name others, with the secret's password or none. The connection
// string pins every setting pgconn would otherwise take from PG* variables
// or files in the home directory, and the startup parameters are reset to
// the application name alone.
func (p *PostgresDriver) config() (*pgx.ConnConfig, error) {
	host, port := hostPort(p.endpoint, "5432")
	user := p.username
	if user == "" {
		user = "postgres"
	}
	database := "postgres"
	if p.check != nil && p.check.Database != "" {
		database = p.check.Database
	}
	u := url.URL{
		Scheme: "postgres",
		User:   url.User(user),
		Host:   net.JoinHostPort(host, port),
		Path:   "/" + database,
		RawQuery: "sslmode=prefer&sslcert=&sslkey=&sslrootcert=&sslpassword=&sslsni=1&sslnegotiation=postgres" +
			"&passfile=&connect_timeout=0&target_session_attrs=any&channel_binding=prefer&require_auth=&min_protocol_version=3.0&max_protocol_version=3.0",
	}
	cfg, err := pgx.ParseConfig(u.String())
	if err != nil {
		return nil, err
	}
	cfg.Password = p.password
	cfg.RuntimeParams = map[string]string{"application_name": "endpoint-monitoring-operator"}
	cfg.ConnectTimeout = 2 * checkTimeout
	cfg.DefaultQueryExecMode = pgx.QueryExecModeSimpleProtocol
	if p.family != "" {
		cfg.DialFunc = familyDial(p.family)
		lookup := cfg.LookupFunc
		if pgLookup != nil {
			lookup = pgLookup
		}
		cfg.LookupFunc = familyLookup(lookup, p.family)
	}
	return cfg, nil
}

// pgLookup, when set, replaces pgconn's resolver.
var pgLookup pgconn.LookupFunc

// familyLookup keeps only the addresses of the family, so pgconn does not
// try the others, unless the name has none of that family; dialing those
// then fails with the usual error.
func familyLookup(lookup pgconn.LookupFunc, family string) pgconn.LookupFunc {
	return func(ctx context.Context, host string) ([]string, error) {
		addrs, err := lookup(ctx, host)
		if err != nil {
			return nil, err
		}
		var kept []string
		for _, a := range addrs {
			if _, err := pickFamily([]string{a}, family); err == nil {
				kept = append(kept, a)
			}
		}
		if len(kept) == 0 {
			return addrs, nil
		}
		return kept, nil
	}
}

var pgAttemptPrefix = regexp.MustCompile(`^\S+ \([^()]*\): `)

// pgConnectText lists the distinct reasons pgconn's connection attempts
// failed, without the address each attempt starts with and in sorted order,
// so the text does not depend on the addresses a name resolves to or their
// order. A refused TLS request is left out when an attempt failed for
// another reason.
func pgConnectText(err error) string {
	var ce *pgconn.ConnectError
	if !errors.As(err, &ce) || ce.Unwrap() == nil {
		return errorText(err)
	}
	var causes []string
	for _, line := range strings.Split(ce.Unwrap().Error(), "\n") {
		cause := errorText(errors.New(pgAttemptPrefix.ReplaceAllString(strings.TrimSpace(line), "")))
		if cause != "" && !slices.Contains(causes, cause) {
			causes = append(causes, cause)
		}
	}
	if len(causes) > 1 {
		causes = slices.DeleteFunc(causes, func(c string) bool { return strings.HasSuffix(c, "server refused TLS connection") })
	}
	slices.Sort(causes)
	return strings.Join(causes, "; ")
}

type pgQuerier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// checkPostgres expects a writable primary, or with MaxSecondsBehindMaster
// set, a replica streaming from its primary and within that lag. The lag is
// zero while the replica has replayed all the WAL it received, and otherwise
// the time since the last transaction it replayed.
func checkPostgres(ctx context.Context, q pgQuerier, check *v1.PostgresCheck) error {
	var inRecovery bool
	if err := q.QueryRow(ctx, "SELECT pg_is_in_recovery()").Scan(&inRecovery); err != nil {
		return fmt.Errorf("failed to get pg_is_in_recovery: %w", err)
	}
	if check == nil || check.MaxSecondsBehindMaster == nil {
		if inRecovery {
			return errors.New("the server is expected to be a primary, but it is in recovery (a replica)")
		}
		var readOnly string
		if err := q.QueryRow(ctx, "SHOW transaction_read_only").Scan(&readOnly); err != nil {
			return fmt.Errorf("failed to get transaction_read_only: %w", err)
		}
		if readOnly != "off" {
			return fmt.Errorf("the server is expected to be a writable primary, but transaction_read_only is %s", readOnly)
		}
		return nil
	}

	if !inRecovery {
		return errors.New("the server is expected to be a replica in recovery, but it is a primary")
	}
	var status *string
	err := q.QueryRow(ctx, "SELECT status FROM pg_stat_wal_receiver").Scan(&status)
	if errors.Is(err, pgx.ErrNoRows) {
		return errors.New("no WAL receiver is running, so the replica is not streaming")
	}
	if err != nil {
		return fmt.Errorf("failed to get the WAL receiver status: %w", err)
	}
	if status == nil {
		return errors.New("the WAL receiver status is hidden from this login; grant it pg_monitor")
	}
	if *status != "streaming" {
		return fmt.Errorf("the WAL receiver is %s, expected streaming", *status)
	}
	var lag *float64
	if err := q.QueryRow(ctx, `SELECT CASE WHEN pg_last_wal_receive_lsn() = pg_last_wal_replay_lsn() THEN 0
		ELSE EXTRACT(EPOCH FROM now() - pg_last_xact_replay_timestamp()) END`).Scan(&lag); err != nil {
		return fmt.Errorf("failed to get the replay lag: %w", err)
	}
	if lag == nil {
		return errors.New("the replay lag is unknown: no transaction has been replayed yet")
	}
	if *lag > float64(*check.MaxSecondsBehindMaster) {
		return fmt.Errorf("the replay lag exceeds max allowed (%d)", *check.MaxSecondsBehindMaster)
	}
	return nil
}

func (p *PostgresDriver) GetEndpoint() string {
	return p.endpoint
}

func (p *PostgresDriver) GetType() string {
	return "postgres"
}
