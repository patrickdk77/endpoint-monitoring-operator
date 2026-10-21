//go:build docker

package docker

import (
	"testing"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/client"

	v1 "github.com/patrickdk77/endpoint-monitoring-operator/api/v1alpha1"
	"github.com/patrickdk77/endpoint-monitoring-operator/internal/driver"
)

func TestMySQLFamily(t *testing.T) {
	for _, tc := range []struct{ kind, image string }{
		{"mysql", "mysql:5.7"}, {"mysql", "percona/percona-server:5.7"}, {"mysql", "mysql:8.0"},
		{"mysql", "percona/percona-server:8.0"}, {"mysql", "mysql:8.4"}, {"mysql", "percona/percona-server:8.4"},
		{"mysql", "mysql:9.0"}, {"mysql", "mysql:9.5"},
		{"mariadb", "mariadb:10.3"}, {"mariadb", "mariadb:10.4"}, {"mariadb", "mariadb:10.6"}, {"mariadb", "mariadb:10.11"},
		{"mariadb", "mariadb:11.4"}, {"mariadb", "mariadb:11.8"}, {"mariadb", "mariadb:12"},
	} {
		t.Run(tc.image, func(t *testing.T) {
			t.Parallel()
			id, endpoint := start(t, server{image: tc.image, port: 3306, env: []string{"MYSQL_ROOT_PASSWORD=" + password}})
			build := driver.NewMYSQLDriver
			if tc.kind == "mariadb" {
				build = driver.NewMariaDBDriver
			}
			check := func(pass string) driver.Driver {
				d, err := build(endpoint, "", &v1.MysqlCheck{SecretRef: creds}, "default", secrets(t, login("root", pass)))
				if err != nil {
					t.Fatal(err)
				}
				return d
			}
			passes(t, id, check(password), 4*time.Minute)
			fails(t, check("wrong"), "Access denied", time.Minute)
		})
	}
}

func TestPostgres(t *testing.T) {
	for _, image := range []string{"postgres:11", "postgres:12", "postgres:13", "postgres:14", "postgres:15", "postgres:16", "postgres:17", "postgres:18"} {
		t.Run(image, func(t *testing.T) {
			t.Parallel()
			id, endpoint := start(t, server{image: image, port: 5432, env: []string{"POSTGRES_PASSWORD=" + password}})
			check := func(pass string) driver.Driver {
				d, err := driver.NewPostgresDriver(endpoint, "", &v1.PostgresCheck{SecretRef: creds}, "default", secrets(t, login("postgres", pass)))
				if err != nil {
					t.Fatal(err)
				}
				return d
			}
			passes(t, id, check(password), 3*time.Minute)
			fails(t, check("wrong"), "password authentication failed", time.Minute)
		})
	}
}

func TestMongoDB(t *testing.T) {
	for _, image := range []string{"mongo:4.4", "mongo:5.0", "mongo:6.0", "mongo:7.0", "mongo:8.0", "mongo:8.2"} {
		t.Run(image, func(t *testing.T) {
			t.Parallel()
			id, endpoint := start(t, server{image: image, port: 27017,
				env: []string{"MONGO_INITDB_ROOT_USERNAME=root", "MONGO_INITDB_ROOT_PASSWORD=" + password}})
			check := func(kube client.Client) driver.Driver {
				d, err := driver.NewMongoDBDriver(endpoint, "", &v1.MongodbCheck{SecretRef: creds}, "default", kube)
				if err != nil {
					t.Fatal(err)
				}
				return d
			}
			passes(t, id, check(secrets(t, login("root", password))), 3*time.Minute)
			fails(t, check(secrets(t, login("root", "wrong"))), "Authentication failed", time.Minute)
			passes(t, id, check(secrets(t, nil)), time.Minute)
		})
	}
}

func TestScyllaDB(t *testing.T) {
	// One at a time: each node claims a core and a gigabyte.
	for _, image := range []string{"scylladb/scylla:5.4", "scylladb/scylla:6.2", "scylladb/scylla:2025.3"} {
		t.Run(image, func(t *testing.T) {
			id, endpoint := start(t, server{image: image, port: 9042,
				args: []string{"--smp", "1", "--memory", "1G", "--overprovisioned", "1", "--developer-mode", "1", "--authenticator", "PasswordAuthenticator"}})
			check := func(pass string) driver.Driver {
				d, err := driver.NewScyllaDBDriver(endpoint, "", &v1.ScylladbCheck{SecretRef: creds}, "default", secrets(t, login("cassandra", pass)))
				if err != nil {
					t.Fatal(err)
				}
				return d
			}
			passes(t, id, check("cassandra"), 5*time.Minute)
			fails(t, check("wrong"), "are incorrect", time.Minute)
		})
	}
}

func TestRedis(t *testing.T) {
	for _, image := range []string{"redis:7.4", "redis:8.2", "valkey/valkey:8.1", "valkey/valkey:9.0"} {
		t.Run(image, func(t *testing.T) {
			t.Parallel()
			id, endpoint := start(t, server{image: image, port: 6379, args: []string{"--requirepass", password}})
			check := func(kube client.Client) driver.Driver {
				d, err := driver.NewREDISDriver(endpoint, "", &v1.RedisCheck{SecretRef: creds}, "default", kube)
				if err != nil {
					t.Fatal(err)
				}
				return d
			}
			passes(t, id, check(secrets(t, map[string]string{"password": password})), 2*time.Minute)
			fails(t, check(secrets(t, map[string]string{"password": "wrong"})), "WRONGPASS", time.Minute)
			fails(t, check(secrets(t, nil)), "NOAUTH", time.Minute)
		})
	}
}
