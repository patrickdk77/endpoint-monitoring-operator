package driver

import (
	"database/sql"
	"fmt"
	"net"
	"strconv"
	"time"

	_ "github.com/go-sql-driver/mysql"
	v1 "github.com/patrickdk77/endpoint-monitoring-operator/api/v1alpha1"
	"github.com/patrickdk77/endpoint-monitoring-operator/internal/notifier"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type MYSQLDriver struct {
	endpoint string
	username string
	password string
	check    *v1.MysqlCheck
}

func NewMYSQLDriver(endpoint string, check *v1.MysqlCheck, namespace string, client client.Client) (Driver, error) {
	if endpoint == "" {
		return nil, fmt.Errorf("endpoint cannot be empty")
	}

	var username, password string
	if check != nil && check.SecretRef.Name != "" {
		secret, err := notifier.GetSecret(check.SecretRef.Name, namespace, client)
		if err != nil {
			return nil, fmt.Errorf("unable to read secret: %s", err)
		}

		if usernameSecret, ok := secret.Data["username"]; ok {
			username = string(usernameSecret)
		}
		if passwordSecret, ok := secret.Data["password"]; ok {
			password = string(passwordSecret)
		}
	}

	return &MYSQLDriver{
		endpoint: endpoint,
		username: username,
		password: password,
		check:    check,
	}, nil
}

func (m *MYSQLDriver) Check() (*CheckResult, error) {
	start := time.Now()

	hostPart, port, errSplit := net.SplitHostPort(m.endpoint)
	if errSplit != nil {
		hostPart = m.endpoint
		port = "3306"
	}
	host := net.JoinHostPort(hostPart, port)

	dsn := fmt.Sprintf("%s:%s@tcp(%s)/", m.username, m.password, host)
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		return &CheckResult{
			Success:      false,
			ResponseTime: time.Since(start),
			Error:        err,
			Message:      fmt.Sprintf("MYSQL check failed to initialize: %v", err),
		}, nil
	} else {
		defer db.Close()
		db.SetConnMaxLifetime(time.Second * 5)
		db.SetMaxOpenConns(1)
	}

	// Test connection
	if err := db.Ping(); err != nil {
		err = fmt.Errorf("failed to ping: %v", err)
	}

	// Read Max_used_connections and max_connections
	var maxUsedConnections, maxConnections int

	var varName, varValue string
	err = db.QueryRow("SHOW GLOBAL STATUS LIKE 'Max_used_connections'").Scan(&varName, &varValue)
	if err != nil {
		err = fmt.Errorf("failed to get max_used_connections: %v", err)
	} else {
		maxUsedConnections, _ = strconv.Atoi(varValue)
	}

	if err == nil {
		err = db.QueryRow("SHOW GLOBAL VARIABLES LIKE 'max_connections'").Scan(&varName, &varValue)
		if err != nil {
			err = fmt.Errorf("failed to get max_connections: %v", err)
		} else {
			maxConnections, _ = strconv.Atoi(varValue)
		}
	}

	if err == nil && maxConnections > 0 {
		ratio := float64(maxUsedConnections) / float64(maxConnections)
		if ratio > 0.90 {
			err = fmt.Errorf("max_used_connections (%d) exceeds 90%% of max_connections (%d)", maxUsedConnections, maxConnections)
		}
	}

	if err == nil {
		var readOnlyValue string
		err = db.QueryRow("SHOW GLOBAL VARIABLES LIKE 'read_only'").Scan(&varName, &readOnlyValue)
		if err != nil {
			err = fmt.Errorf("failed to get read_only variable: %v", err)
		} else {
			isSlave := m.check != nil && m.check.MaxSecondsBehindMaster != nil
			isReadOnly := readOnlyValue == "ON" || readOnlyValue == "1"

			if isSlave && !isReadOnly {
				err = fmt.Errorf("mysql is expected to be in readonly mode (it is a slave), but read_only is %s", readOnlyValue)
			} else if !isSlave && isReadOnly {
				err = fmt.Errorf("mysql is expected to NOT be in readonly mode (it is a master), but read_only is %s", readOnlyValue)
			}
		}
	}

	// Check Slave Status if MaxSecondsBehindMaster is defined
	if err == nil && m.check != nil && m.check.MaxSecondsBehindMaster != nil {
		rows, err := db.Query("SHOW SLAVE STATUS")
		if err != nil {
			err = fmt.Errorf("failed to query slave status: %v", err)
		} else {
			defer rows.Close()

			if !rows.Next() {
				err = fmt.Errorf("SHOW SLAVE STATUS returned empty, but MaxSecondsBehindMaster is defined")
			}

			cols, err := rows.Columns()
			if err != nil {
				err = fmt.Errorf("failed to get slave status columns: %v", err)
			} else {

				// MySQL returns many columns for SHOW SLAVE STATUS, so we need to map them dynamically
				values := make([]sql.RawBytes, len(cols))
				scanArgs := make([]interface{}, len(values))
				for i := range values {
					scanArgs[i] = &values[i]
				}

				if err := rows.Scan(scanArgs...); err != nil {
					err = fmt.Errorf("failed to scan slave status: %v", err)
				} else {

					slaveIORunning := ""
					slaveSQLRunning := ""
					var secondsBehindMaster *int

					for i, col := range cols {
						val := string(values[i])
						switch col {
						case "Slave_IO_Running":
							slaveIORunning = val
						case "Slave_SQL_Running":
							slaveSQLRunning = val
						case "Seconds_Behind_Master":
							if val != "NULL" && val != "" {
								parsed, err := strconv.Atoi(val)
								if err == nil {
									secondsBehindMaster = &parsed
								}
							}
						}
					}

					if slaveIORunning != "Yes" {
						err = fmt.Errorf("Slave_IO_Running is %s, expected Yes", slaveIORunning)
					} else if slaveSQLRunning != "Yes" {
						err = fmt.Errorf("Slave_SQL_Running is %s, expected Yes", slaveSQLRunning)
					} else if secondsBehindMaster == nil {
						err = fmt.Errorf("Seconds_Behind_Master is NULL")
					} else if *secondsBehindMaster > *m.check.MaxSecondsBehindMaster {
						err = fmt.Errorf("Seconds_Behind_Master (%d) exceeds max allowed (%d)", *secondsBehindMaster, *m.check.MaxSecondsBehindMaster)
					}
				}
			}
		}
	}

	duration := time.Since(start)
	result := &CheckResult{
		ResponseTime: duration,
	}

	if err != nil {
		result.Success = false
		result.Error = err
		result.Message = fmt.Sprintf("MYSQL check failed: %v", err)
		return result, nil
	}

	result.Success = true
	result.Message = fmt.Sprintf("MYSQL check successful (response time: %v)", duration)

	return result, nil
}

func (m *MYSQLDriver) GetEndpoint() string {
	return m.endpoint
}

func (m *MYSQLDriver) GetType() string {
	return "mysql"
}
