package driver

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"slices"
	"strings"
	"sync"
	"time"

	gocql "github.com/apache/cassandra-gocql-driver/v2"
	v1 "github.com/patrickdk77/endpoint-monitoring-operator/api/v1alpha1"
	"github.com/patrickdk77/endpoint-monitoring-operator/internal/notifier"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type ScyllaDBDriver struct {
	endpoint string
	family   string
	username string
	password string
}

func NewScyllaDBDriver(endpoint, family string, check *v1.ScylladbCheck, namespace string, client client.Client) (Driver, error) {
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

	return &ScyllaDBDriver{
		endpoint: endpoint,
		family:   family,
		username: username,
		password: password,
	}, nil
}

func (s *ScyllaDBDriver) Check() (*CheckResult, error) {
	start := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), checkTimeout)
	defer cancel()

	err := s.run(ctx)
	duration := time.Since(start)
	result := &CheckResult{
		ResponseTime: duration,
	}

	if err != nil {
		result.Success = false
		result.Error = err
		result.Message = fmt.Sprintf("SCYLLADB check failed: %s", errorText(err))
		return result, nil
	}

	result.Success = true
	result.Message = fmt.Sprintf("SCYLLADB check successful (response time: %v)", duration)

	return result, nil
}

// run opens a session in the background, since gocql takes no context for
// it, and closes a session that opens after the check has given up.
func (s *ScyllaDBDriver) run(ctx context.Context) error {
	cluster, err := s.cluster(ctx)
	if err != nil {
		return fmt.Errorf("failed to resolve: %w", err)
	}
	failures := &hostFailures{}
	cluster.Logger = failures
	type opened struct {
		session *gocql.Session
		err     error
	}
	done := make(chan opened, 1)
	go func() {
		session, err := cluster.CreateSession()
		done <- opened{session, err}
	}()
	var session *gocql.Session
	select {
	case o := <-done:
		if text := failures.text(); o.err != nil && text != "" && strings.Contains(o.err.Error(), "unable to connect to initial hosts") {
			return fmt.Errorf("failed to connect: %w", &textError{text: "gocql: unable to create session: unable to connect to initial hosts: " + text, err: o.err})
		}
		if o.err != nil {
			return fmt.Errorf("failed to connect: %w", o.err)
		}
		session = o.session
	case <-ctx.Done():
		go func() {
			if o := <-done; o.session != nil {
				o.session.Close()
			}
		}()
		return fmt.Errorf("failed to connect: %w", ctx.Err())
	}
	defer session.Close()

	var bootstrapped string
	if err := session.Query("SELECT bootstrapped FROM system.local WHERE key='local'").WithContext(ctx).Scan(&bootstrapped); err != nil {
		return fmt.Errorf("failed to read system.local: %w", err)
	}
	if bootstrapped != "COMPLETED" {
		return fmt.Errorf("the node is not bootstrapped: bootstrapped is %s", bootstrapped)
	}
	return nil
}

// cluster connects to the endpoint alone, without discovering the other
// nodes or reading the schema, and logs in only when the secret names a
// user. A name is resolved here, to the addresses of the ipFamily when it is
// set, so gocql never tries the others.
func (s *ScyllaDBDriver) cluster(ctx context.Context) (*gocql.ClusterConfig, error) {
	host, port := hostPort(s.endpoint, "9042")
	hosts := []string{net.JoinHostPort(host, port)}
	if net.ParseIP(host) == nil {
		network := "ip"
		switch s.family {
		case FamilyIPv4:
			network = "ip4"
		case FamilyIPv6:
			network = "ip6"
		}
		lookup := net.DefaultResolver.LookupNetIP
		if scyllaLookup != nil {
			lookup = scyllaLookup
		}
		ips, err := lookup(ctx, network, host)
		if err != nil {
			return nil, err
		}
		hosts = hosts[:0]
		for _, ip := range ips {
			hosts = append(hosts, net.JoinHostPort(ip.Unmap().String(), port))
		}
	}
	c := gocql.NewCluster(hosts...)
	c.ProtoVersion = 4
	c.Consistency = gocql.One
	c.NumConns = 1
	c.Timeout = 2 * checkTimeout
	c.ConnectTimeout = checkTimeout
	c.ReconnectionPolicy = &gocql.ConstantReconnectionPolicy{MaxRetries: 1}
	c.DisableInitialHostLookup = true
	c.Events.DisableNodeStatusEvents = true
	c.Events.DisableTopologyEvents = true
	c.Events.DisableSchemaEvents = true
	c.Metadata.CacheMode = gocql.Disabled
	if s.username != "" {
		c.Authenticator = gocql.PasswordAuthenticator{Username: s.username, Password: s.password}
	}
	if s.family != "" {
		c.Dialer = dialFunc(familyDial(s.family))
	}
	return c, nil
}

// hostFailures collects why gocql's control connection failed with each
// host; gocql's own error names only the last host it tried.
type hostFailures struct {
	mu     sync.Mutex
	causes []string
}

func (h *hostFailures) Error(string, ...gocql.LogField)   {}
func (h *hostFailures) Warning(string, ...gocql.LogField) {}
func (h *hostFailures) Debug(string, ...gocql.LogField)   {}

func (h *hostFailures) Info(msg string, fields ...gocql.LogField) {
	if !strings.HasPrefix(msg, "Control connection") {
		return
	}
	for _, f := range fields {
		if f.Name != "err" {
			continue
		}
		cause := errorText(errors.New(f.Value.String()))
		h.mu.Lock()
		if !slices.Contains(h.causes, cause) {
			h.causes = append(h.causes, cause)
		}
		h.mu.Unlock()
	}
}

// text lists the distinct causes in sorted order, so it does not depend on
// the order gocql shuffled the hosts into.
func (h *hostFailures) text() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	causes := slices.Clone(h.causes)
	slices.Sort(causes)
	return strings.Join(causes, "; ")
}

// scyllaLookup, when set, replaces the resolver cluster uses.
var scyllaLookup func(ctx context.Context, network, host string) ([]netip.Addr, error)

func (s *ScyllaDBDriver) GetEndpoint() string {
	return s.endpoint
}

func (s *ScyllaDBDriver) GetType() string {
	return "scylladb"
}
