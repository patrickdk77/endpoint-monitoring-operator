package driver

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"sync/atomic"
	"time"

	v1 "github.com/patrickdk77/endpoint-monitoring-operator/api/v1alpha1"
	"github.com/patrickdk77/endpoint-monitoring-operator/internal/notifier"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/event"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type MongoDBDriver struct {
	endpoint string
	family   string
	username string
	password string
	check    *v1.MongodbCheck
}

func NewMongoDBDriver(endpoint, family string, check *v1.MongodbCheck, namespace string, client client.Client) (Driver, error) {
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

	return &MongoDBDriver{
		endpoint: endpoint,
		family:   family,
		username: username,
		password: password,
		check:    check,
	}, nil
}

func (m *MongoDBDriver) Check() (*CheckResult, error) {
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
		result.Message = fmt.Sprintf("MONGODB check failed: %s", errorText(err))
		return result, nil
	}

	result.Success = true
	result.Message = fmt.Sprintf("MONGODB check successful (response time: %v)", duration)

	return result, nil
}

// run ends the check at a failed heartbeat that comes before any successful
// one, with its cause; the Go driver would otherwise keep selecting a server
// until the check times out.
func (m *MongoDBDriver) run(ctx context.Context) error {
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	var answered atomic.Bool
	opts := m.options().SetServerMonitor(&event.ServerMonitor{
		ServerHeartbeatSucceeded: func(*event.ServerHeartbeatSucceededEvent) { answered.Store(true) },
		ServerHeartbeatFailed: func(e *event.ServerHeartbeatFailedEvent) {
			if !answered.Load() {
				cancel(e.Failure)
			}
		},
	})
	c, err := mongo.Connect(opts)
	if err != nil {
		return fmt.Errorf("failed to initialize: %w", err)
	}
	defer func() {
		closeCtx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = c.Disconnect(closeCtx)
	}()
	err = checkMongoDB(ctx, mongoAdmin{c.Database("admin")}, m.check)
	if cause := context.Cause(ctx); err != nil && cause != nil && !errors.Is(cause, context.DeadlineExceeded) {
		return fmt.Errorf("failed to connect: %w", cause)
	}
	return err
}

// options connects to the endpoint itself rather than to the replica set it
// belongs to, and authenticates against admin only when the secret names a
// user.
func (m *MongoDBDriver) options() *options.ClientOptions {
	host, port := hostPort(m.endpoint, "27017")
	opts := options.Client().
		SetHosts([]string{net.JoinHostPort(host, port)}).
		SetDirect(true).
		SetConnectTimeout(2 * checkTimeout).
		SetServerSelectionTimeout(2 * checkTimeout).
		SetRetryReads(false)
	if m.username != "" {
		opts.SetAuth(options.Credential{
			AuthSource:  "admin",
			Username:    m.username,
			Password:    m.password,
			PasswordSet: m.password != "",
		})
	}
	if m.check != nil && m.check.Tls {
		opts.SetTLSConfig(&tls.Config{ServerName: host})
	}
	if m.family != "" {
		opts.SetDialer(dialFunc(familyDial(m.family)))
	}
	return opts
}

type dialFunc func(ctx context.Context, network, address string) (net.Conn, error)

func (f dialFunc) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	return f(ctx, network, address)
}

type mongoRunner interface {
	RunCommand(ctx context.Context, cmd bson.D) (bson.Raw, error)
}

type mongoAdmin struct{ db *mongo.Database }

func (a mongoAdmin) RunCommand(ctx context.Context, cmd bson.D) (bson.Raw, error) {
	return a.db.RunCommand(ctx, cmd).Raw()
}

type replMember struct {
	State      int       `bson:"state"`
	Self       bool      `bson:"self"`
	OptimeDate time.Time `bson:"optimeDate"`
}

// checkMongoDB expects a writable primary, or with MaxSecondsBehindMaster set,
// a secondary whose last applied operation is no further behind the
// primary's than that.
func checkMongoDB(ctx context.Context, admin mongoRunner, check *v1.MongodbCheck) error {
	raw, err := admin.RunCommand(ctx, bson.D{{Key: "hello", Value: 1}})
	if err != nil {
		return fmt.Errorf("failed to run hello: %w", err)
	}
	var hello struct {
		IsWritablePrimary bool `bson:"isWritablePrimary"`
		Secondary         bool `bson:"secondary"`
		ArbiterOnly       bool `bson:"arbiterOnly"`
	}
	if err := bson.Unmarshal(raw, &hello); err != nil {
		return fmt.Errorf("failed to read hello: %w", err)
	}
	role := "neither a primary nor a secondary"
	switch {
	case hello.IsWritablePrimary:
		role = "a writable primary"
	case hello.Secondary:
		role = "a secondary"
	case hello.ArbiterOnly:
		role = "an arbiter"
	}
	if check == nil || check.MaxSecondsBehindMaster == nil {
		if !hello.IsWritablePrimary {
			return fmt.Errorf("the server is expected to be a writable primary, but it is %s", role)
		}
		return nil
	}
	if !hello.Secondary {
		return fmt.Errorf("the server is expected to be a secondary, but it is %s", role)
	}

	raw, err = admin.RunCommand(ctx, bson.D{{Key: "replSetGetStatus", Value: 1}})
	if err != nil {
		return fmt.Errorf("failed to run replSetGetStatus: %w", err)
	}
	var status struct {
		Members []replMember `bson:"members"`
	}
	if err := bson.Unmarshal(raw, &status); err != nil {
		return fmt.Errorf("failed to read replSetGetStatus: %w", err)
	}
	var self, primary *replMember
	for i := range status.Members {
		switch member := &status.Members[i]; {
		case member.Self:
			self = member
		case member.State == 1:
			primary = member
		}
	}
	if self == nil {
		return errors.New("replSetGetStatus does not list this server")
	}
	if primary == nil {
		return errors.New("the replica set has no primary, so the replication lag is unknown")
	}
	if primary.OptimeDate.Sub(self.OptimeDate) > time.Duration(*check.MaxSecondsBehindMaster)*time.Second {
		return fmt.Errorf("the replication lag exceeds max allowed (%d)", *check.MaxSecondsBehindMaster)
	}
	return nil
}

func (m *MongoDBDriver) GetEndpoint() string {
	return m.endpoint
}

func (m *MongoDBDriver) GetType() string {
	return "mongodb"
}
