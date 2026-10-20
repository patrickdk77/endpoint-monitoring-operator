package driver

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
	"time"

	"github.com/go-sql-driver/mysql"
	v1 "github.com/patrickdk77/endpoint-monitoring-operator/api/v1alpha1"
	"github.com/patrickdk77/endpoint-monitoring-operator/internal/notifier"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type MYSQLDriver struct {
	kind     string
	endpoint string
	family   string
	username string
	password string
	check    *v1.MysqlCheck
}

func NewMYSQLDriver(endpoint, family string, check *v1.MysqlCheck, namespace string, client client.Client) (Driver, error) {
	return newMySQLFamilyDriver("mysql", endpoint, family, check, namespace, client)
}

func NewMariaDBDriver(endpoint, family string, check *v1.MysqlCheck, namespace string, client client.Client) (Driver, error) {
	return newMySQLFamilyDriver("mariadb", endpoint, family, check, namespace, client)
}

func newMySQLFamilyDriver(kind, endpoint, family string, check *v1.MysqlCheck, namespace string, client client.Client) (Driver, error) {
	if endpoint == "" {
		return nil, fmt.Errorf("endpoint cannot be empty")
	}

	var username, password string
	if check != nil && check.SecretRef.Name != "" {
		secret, err := notifier.GetSecret(check.SecretRef.Name, namespace, client)
		if err != nil {
			return nil, fmt.Errorf("unable to read secret: %w", err)
		}

		if usernameSecret, ok := secret.Data["username"]; ok {
			username = string(usernameSecret)
		}
		if passwordSecret, ok := secret.Data["password"]; ok {
			password = string(passwordSecret)
		}
	}

	return &MYSQLDriver{
		kind:     kind,
		endpoint: endpoint,
		family:   family,
		username: username,
		password: password,
		check:    check,
	}, nil
}

func (m *MYSQLDriver) Check() (*CheckResult, error) {
	start := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), checkTimeout)
	defer cancel()

	err := m.run(ctx)
	duration := time.Since(start)
	result := &CheckResult{
		ResponseTime: duration,
	}

	if err != nil {
		result.Success = false
		result.Error = err
		result.Message = fmt.Sprintf("%s check failed: %s", strings.ToUpper(m.kind), errorText(err))
		return result, nil
	}

	result.Success = true
	result.Message = fmt.Sprintf("%s check successful (response time: %v)", strings.ToUpper(m.kind), duration)

	return result, nil
}

func (m *MYSQLDriver) run(ctx context.Context) error {
	connector, err := mysql.NewConnector(m.config())
	if err != nil {
		return fmt.Errorf("failed to initialize: %w", err)
	}
	db := sql.OpenDB(connector)
	defer db.Close()
	db.SetConnMaxLifetime(5 * time.Second)
	db.SetMaxOpenConns(1)
	return checkMySQL(ctx, db, m.check)
}

func (m *MYSQLDriver) config() *mysql.Config {
	hostPart, port := hostPort(m.endpoint, "3306")
	cfg := mysql.NewConfig()
	cfg.User = m.username
	cfg.Passwd = m.password
	cfg.Net = familyNetwork("tcp", m.family)
	cfg.Addr = net.JoinHostPort(hostPart, port)
	cfg.Timeout = 2 * checkTimeout
	cfg.ReadTimeout = 2 * checkTimeout
	cfg.WriteTimeout = 2 * checkTimeout
	return cfg
}

func checkMySQL(ctx context.Context, db *sql.DB, check *v1.MysqlCheck) error {
	if err := db.PingContext(ctx); err != nil {
		return fmt.Errorf("failed to ping: %w", err)
	}

	threads, err := globalInt(ctx, db, "SHOW GLOBAL STATUS LIKE 'Threads_connected'")
	if err != nil {
		return fmt.Errorf("failed to get threads_connected: %w", err)
	}
	maxConnections, err := globalInt(ctx, db, "SHOW GLOBAL VARIABLES LIKE 'max_connections'")
	if err != nil {
		return fmt.Errorf("failed to get max_connections: %w", err)
	}
	if maxConnections > 0 && float64(threads)/float64(maxConnections) > 0.90 {
		return fmt.Errorf("threads_connected exceeds 90%% of max_connections (%d)", maxConnections)
	}

	var varName, readOnlyValue string
	if err := db.QueryRowContext(ctx, "SHOW GLOBAL VARIABLES LIKE 'read_only'").Scan(&varName, &readOnlyValue); err != nil {
		return fmt.Errorf("failed to get read_only variable: %w", err)
	}
	isReplica := check != nil && check.MaxSecondsBehindMaster != nil
	isReadOnly := readOnlyValue == "ON" || readOnlyValue == "1"
	if isReplica && !isReadOnly {
		return fmt.Errorf("the server is expected to be in readonly mode (it is a replica), but read_only is %s", readOnlyValue)
	}
	if !isReplica && isReadOnly {
		return fmt.Errorf("the server is expected to NOT be in readonly mode (it is a primary), but read_only is %s", readOnlyValue)
	}
	if !isReplica {
		return nil
	}
	return checkReplication(ctx, db, *check.MaxSecondsBehindMaster)
}

func globalInt(ctx context.Context, db *sql.DB, query string) (int, error) {
	var name, value string
	if err := db.QueryRowContext(ctx, query).Scan(&name, &value); err != nil {
		return 0, err
	}
	return strconv.Atoi(value)
}

// checkReplication reads SHOW REPLICA STATUS, or SHOW SLAVE STATUS on
// servers older than MySQL 8.0.22, and accepts either set of column names.
func checkReplication(ctx context.Context, db *sql.DB, maxBehind int) error {
	rows, err := db.QueryContext(ctx, "SHOW REPLICA STATUS")
	var mysqlErr *mysql.MySQLError
	if errors.As(err, &mysqlErr) && mysqlErr.Number == 1064 {
		rows, err = db.QueryContext(ctx, "SHOW SLAVE STATUS")
	}
	if err != nil {
		return fmt.Errorf("failed to query replication status: %w", err)
	}
	defer rows.Close()

	cols, err := rows.Columns()
	if err != nil {
		return fmt.Errorf("failed to get replication status columns: %w", err)
	}
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return fmt.Errorf("failed to read replication status: %w", err)
		}
		return errors.New("replication status is empty, but maxSecondsBehindMaster is set")
	}
	values := make([]sql.NullString, len(cols))
	scanArgs := make([]any, len(values))
	for i := range values {
		scanArgs[i] = &values[i]
	}
	if err := rows.Scan(scanArgs...); err != nil {
		return fmt.Errorf("failed to scan replication status: %w", err)
	}
	status := make(map[string]sql.NullString, len(cols))
	for i, col := range cols {
		status[col] = values[i]
	}
	column := func(names ...string) (string, sql.NullString) {
		for _, name := range names {
			if v, ok := status[name]; ok {
				return name, v
			}
		}
		return names[0], sql.NullString{}
	}

	for _, names := range [][]string{{"Replica_IO_Running", "Slave_IO_Running"}, {"Replica_SQL_Running", "Slave_SQL_Running"}} {
		name, v := column(names...)
		if v.String != "Yes" {
			return fmt.Errorf("%s is %s, expected Yes", name, nullText(v))
		}
	}
	name, behind := column("Seconds_Behind_Source", "Seconds_Behind_Master")
	if !behind.Valid || behind.String == "" {
		return fmt.Errorf("%s is NULL", name)
	}
	seconds, err := strconv.Atoi(behind.String)
	if err != nil {
		return fmt.Errorf("%s is %q: %w", name, behind.String, err)
	}
	if seconds > maxBehind {
		return fmt.Errorf("%s exceeds max allowed (%d)", name, maxBehind)
	}
	return nil
}

func nullText(v sql.NullString) string {
	if !v.Valid {
		return "NULL"
	}
	return v.String
}

func (m *MYSQLDriver) GetEndpoint() string {
	return m.endpoint
}

func (m *MYSQLDriver) GetType() string {
	return m.kind
}
