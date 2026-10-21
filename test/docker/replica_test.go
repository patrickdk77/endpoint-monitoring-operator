//go:build docker

package docker

import (
	"os/exec"
	"testing"
	"time"

	v1 "github.com/patrickdk77/endpoint-monitoring-operator/api/v1alpha1"
	"github.com/patrickdk77/endpoint-monitoring-operator/internal/driver"
)

// keepWriting runs script in the container every second until the test
// ends, so a replica always has something to apply.
func keepWriting(t *testing.T, id, script string) {
	t.Helper()
	done := make(chan struct{})
	t.Cleanup(func() { close(done) })
	go func() {
		for {
			select {
			case <-done:
				return
			case <-time.After(time.Second):
			}
			_ = exec.Command("docker", "exec", id, "sh", "-c", script).Run()
		}
	}()
}

const sqlClient = `MYSQL_PWD=` + password + ` "$(command -v mariadb || command -v mysql)" -uroot -h127.0.0.1 -e `

func TestMySQLReplica(t *testing.T) {
	for _, tc := range []struct {
		kind, image   string
		primary, args []string
		changeSource  string
		delay         string
	}{
		{"mysql", "mysql:9.5",
			[]string{"--server-id=1", "--log-bin=mysql-bin", "--gtid-mode=ON", "--enforce-gtid-consistency=ON"},
			[]string{"--server-id=2", "--gtid-mode=ON", "--enforce-gtid-consistency=ON", "--read-only=ON"},
			"CHANGE REPLICATION SOURCE TO SOURCE_HOST='primary', SOURCE_USER='repl', SOURCE_PASSWORD='" + password + "', SOURCE_AUTO_POSITION=1, GET_SOURCE_PUBLIC_KEY=1; START REPLICA;",
			"STOP REPLICA; CHANGE REPLICATION SOURCE TO SOURCE_DELAY=600; START REPLICA;"},
		{"mariadb", "mariadb:12",
			[]string{"--server-id=1", "--log-bin=mysql-bin", "--binlog-format=ROW"},
			[]string{"--server-id=2", "--read-only=1"},
			"CHANGE MASTER TO MASTER_HOST='primary', MASTER_USER='repl', MASTER_PASSWORD='" + password + "', MASTER_USE_GTID=slave_pos; START SLAVE;",
			"STOP SLAVE; CHANGE MASTER TO MASTER_DELAY=600; START SLAVE;"},
	} {
		t.Run(tc.image, func(t *testing.T) {
			t.Parallel()
			net := network(t)
			env := []string{"MYSQL_ROOT_PASSWORD=" + password}
			primary, primaryEndpoint := start(t, server{image: tc.image, port: 3306, env: env, network: net, name: "primary", args: tc.primary})
			replica, replicaEndpoint := start(t, server{image: tc.image, port: 3306, env: env, network: net, name: "replica", args: tc.args})
			run(t, primary, sqlClient+`"CREATE USER IF NOT EXISTS 'repl'@'%' IDENTIFIED BY '`+password+`'; GRANT REPLICATION SLAVE ON *.* TO 'repl'@'%'; CREATE DATABASE IF NOT EXISTS e2e; CREATE TABLE IF NOT EXISTS e2e.t (i INT)"`, 4*time.Minute)
			run(t, replica, sqlClient+`"`+tc.changeSource+`"`, 4*time.Minute)
			keepWriting(t, primary, sqlClient+`"INSERT INTO e2e.t VALUES (1)"`)

			build := driver.NewMYSQLDriver
			if tc.kind == "mariadb" {
				build = driver.NewMariaDBDriver
			}
			check := func(endpoint string, maxLag *int) driver.Driver {
				d, err := build(endpoint, "", &v1.MysqlCheck{SecretRef: creds, MaxSecondsBehindMaster: maxLag}, "default", secrets(t, login("root", password)))
				if err != nil {
					t.Fatal(err)
				}
				return d
			}
			minute, five := 60, 5
			passes(t, replica, check(replicaEndpoint, &minute), 2*time.Minute)
			passes(t, primary, check(primaryEndpoint, nil), time.Minute)
			fails(t, check(replicaEndpoint, nil), "expected to NOT be in readonly mode", time.Minute)
			fails(t, check(primaryEndpoint, &minute), "expected to be in readonly mode", time.Minute)

			run(t, replica, sqlClient+`"`+tc.delay+`"`, time.Minute)
			fails(t, check(replicaEndpoint, &five), "exceeds max allowed (5)", 2*time.Minute)
		})
	}
}

func TestPostgresReplica(t *testing.T) {
	t.Parallel()
	net := network(t)
	psql := `PGPASSWORD=` + password + ` psql -h 127.0.0.1 -U postgres -c `
	primary, primaryEndpoint := start(t, server{image: "postgres:18", port: 5432, env: []string{"POSTGRES_PASSWORD=" + password}, network: net, name: "primary"})
	run(t, primary, psql+`'SELECT 1'`, 3*time.Minute)
	run(t, primary, `echo "host replication all all scram-sha-256" >> "$PGDATA/pg_hba.conf" && `+psql+`'SELECT pg_reload_conf()' -c 'CREATE TABLE IF NOT EXISTS e2e (i int)'`, time.Minute)
	replica, replicaEndpoint := start(t, server{image: "postgres:18", port: 5432, env: []string{"PGPASSWORD=" + password}, network: net, name: "replica",
		entrypoint: "bash", args: []string{"-c", `set -e; mkdir -p "$PGDATA"
until pg_basebackup -h primary -U postgres -D "$PGDATA" -R -X stream; do rm -rf "$PGDATA"/*; sleep 1; done
chown -R postgres:postgres "$PGDATA"; chmod 700 "$PGDATA"
exec gosu postgres postgres`}})
	keepWriting(t, primary, psql+`'INSERT INTO e2e VALUES (1)'`)

	check := func(endpoint string, maxLag *int) driver.Driver {
		d, err := driver.NewPostgresDriver(endpoint, "", &v1.PostgresCheck{SecretRef: creds, MaxSecondsBehindMaster: maxLag}, "default", secrets(t, login("postgres", password)))
		if err != nil {
			t.Fatal(err)
		}
		return d
	}
	minute, five := 60, 5
	passes(t, replica, check(replicaEndpoint, &minute), 3*time.Minute)
	passes(t, primary, check(primaryEndpoint, nil), time.Minute)
	fails(t, check(replicaEndpoint, nil), "expected to be a primary, but it is in recovery", time.Minute)
	fails(t, check(primaryEndpoint, &minute), "expected to be a replica in recovery, but it is a primary", time.Minute)

	run(t, replica, `echo "recovery_min_apply_delay = '10min'" >> "$PGDATA/postgresql.auto.conf" && `+psql+`'SELECT pg_reload_conf()'`, time.Minute)
	fails(t, check(replicaEndpoint, &five), "the replay lag exceeds max allowed (5)", 2*time.Minute)
}

func TestMongoDBReplicaSet(t *testing.T) {
	t.Parallel()
	net := network(t)
	member := func(name string) (string, string) {
		return start(t, server{image: "mongo:8.2", port: 27017, network: net, name: name, args: []string{"--replSet", "rs0", "--bind_ip_all"}})
	}
	m1, primaryEndpoint := member("m1")
	_, secondaryEndpoint := member("m2")
	_, delayedEndpoint := member("m3")
	run(t, m1, `mongosh --quiet --eval 'rs.initiate({_id: "rs0", members: [
		{_id: 0, host: "m1:27017", priority: 2},
		{_id: 1, host: "m2:27017", priority: 1},
		{_id: 2, host: "m3:27017", priority: 0, hidden: true, secondaryDelaySecs: 600}]})'`, 3*time.Minute)
	keepWriting(t, m1, `mongosh --quiet --eval 'db.getSiblingDB("e2e").t.insertOne({i: 1})'`)

	check := func(endpoint string, maxLag *int) driver.Driver {
		d, err := driver.NewMongoDBDriver(endpoint, "", &v1.MongodbCheck{MaxSecondsBehindMaster: maxLag}, "default", nil)
		if err != nil {
			t.Fatal(err)
		}
		return d
	}
	minute, five := 60, 5
	passes(t, m1, check(primaryEndpoint, nil), 3*time.Minute)
	passes(t, m1, check(secondaryEndpoint, &minute), 3*time.Minute)
	fails(t, check(secondaryEndpoint, nil), "expected to be a writable primary, but it is a secondary", time.Minute)
	fails(t, check(primaryEndpoint, &minute), "expected to be a secondary, but it is a writable primary", time.Minute)
	fails(t, check(delayedEndpoint, &five), "the replication lag exceeds max allowed (5)", 3*time.Minute)
}
