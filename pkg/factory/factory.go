package factory

import (
	"fmt"

	"github.com/patrickdk77/endpoint-monitoring-operator/api/v1alpha1"
	"github.com/patrickdk77/endpoint-monitoring-operator/internal/driver"
	"github.com/patrickdk77/endpoint-monitoring-operator/internal/notifier"
	"github.com/patrickdk77/endpoint-monitoring-operator/internal/notifier/discord"
	"github.com/patrickdk77/endpoint-monitoring-operator/internal/notifier/email"
	"github.com/patrickdk77/endpoint-monitoring-operator/internal/notifier/slack"
	"github.com/patrickdk77/endpoint-monitoring-operator/internal/notifier/valkey"
	"github.com/patrickdk77/endpoint-monitoring-operator/internal/notifier/webhook"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// NotifierFactory creates notifiers based on configuration
type NotifierFactory struct{}

// NewNotifier creates a notifier instance based on the configuration
func NewNotifier(config *v1alpha1.NotifyConfig) (notifier.Notifier, error) {
	factory := &NotifierFactory{}
	return factory.CreateNotifier(config)
}

// CreateNotifier implements the factory pattern for notifiers
func (f *NotifierFactory) CreateNotifier(config *v1alpha1.NotifyConfig) (notifier.Notifier, error) {
	if config == nil {
		return nil, fmt.Errorf("notify config is nil")
	}

	var notifiers []namedNotifier

	if config.Slack != nil && config.Slack.Enabled {
		slackNotifier, err := slack.New(config.Slack)
		if err != nil {
			return nil, fmt.Errorf("failed to create Slack notifier: %w", err)
		}
		notifiers = append(notifiers, namedNotifier{"slack", slackNotifier})
	}

	if config.Email != nil && config.Email.Enabled {
		emailNotifier, err := email.New(config.Email)
		if err != nil {
			return nil, fmt.Errorf("failed to create Email notifier: %w", err)
		}
		notifiers = append(notifiers, namedNotifier{"email", emailNotifier})
	}

	if config.Discord != nil && config.Discord.Enabled {
		discordNotifier, err := discord.New(config.Discord)
		if err != nil {
			return nil, fmt.Errorf("failed to create Discord notifier: %w", err)
		}
		notifiers = append(notifiers, namedNotifier{"discord", discordNotifier})
	}

	if config.Webhook != nil && config.Webhook.Enabled {
		webhookNotifier, err := webhook.New(config.Webhook)
		if err != nil {
			return nil, fmt.Errorf("failed to create Webhook notifier: %w", err)
		}
		notifiers = append(notifiers, namedNotifier{"webhook", webhookNotifier})
	}

	if config.Valkey != nil && config.Valkey.Enabled {
		valkeyNotifier, err := valkey.New(config.Valkey)
		if err != nil {
			return nil, fmt.Errorf("failed to create Valkey notifier: %w", err)
		}
		notifiers = append(notifiers, namedNotifier{"valkey", valkeyNotifier})
	}

	if len(notifiers) == 0 {
		return nil, fmt.Errorf("no notifiers enabled")
	}

	return &CompositeNotifier{notifiers: notifiers}, nil
}

type namedNotifier struct {
	name string
	notifier.Notifier
}

// CompositeNotifier handles multiple notification channels
type CompositeNotifier struct {
	notifiers []namedNotifier
}

// SendError lists the notifiers that failed to send an alert.
type SendError struct {
	Notifiers []string
	Errs      []error
	Failed    []notifier.Notifier
}

func (e *SendError) Error() string {
	return fmt.Sprintf("failed to send alerts: %v", e.Errs)
}

func (e *SendError) Unwrap() []error {
	return e.Errs
}

// SendAlert sends alerts to all configured notifiers
func (c *CompositeNotifier) SendAlert(status string, values *notifier.NoticeValues, client client.Client) error {
	var failed SendError
	for _, n := range c.notifiers {
		if err := n.SendAlert(status, values, client); err != nil {
			failed.Notifiers = append(failed.Notifiers, n.name)
			failed.Errs = append(failed.Errs, err)
			failed.Failed = append(failed.Failed, n.Notifier)
		}
	}
	if len(failed.Errs) > 0 {
		return &failed
	}
	return nil
}

// DriverFactory creates monitoring drivers based on configuration
type DriverFactory struct{}

// NewDriver creates a driver instance based on the driver type
func NewDriver(driverType string, endpoint string, monitor *v1alpha1.EndpointMonitor, namespace string, client client.Client) (driver.Driver, error) {
	factory := &DriverFactory{}
	return factory.CreateDriver(driverType, endpoint, monitor, namespace, client)
}

// CreateDriver implements the factory pattern for drivers
func (f *DriverFactory) CreateDriver(driverType string, endpoint string, monitor *v1alpha1.EndpointMonitor, namespace string, client client.Client) (driver.Driver, error) {
	var ipv4, ipv6 bool
	for _, family := range monitor.Spec.IpFamily {
		switch family {
		case driver.FamilyIPv4:
			ipv4 = true
		case driver.FamilyIPv6:
			ipv6 = true
		default:
			return nil, fmt.Errorf("unsupported ipFamily: %s", family)
		}
	}
	switch {
	case ipv4 && ipv6:
		v4, err := f.createFamilyDriver(driverType, endpoint, driver.FamilyIPv4, monitor, namespace, client)
		if err != nil {
			return nil, err
		}
		v6, err := f.createFamilyDriver(driverType, endpoint, driver.FamilyIPv6, monitor, namespace, client)
		if err != nil {
			return nil, err
		}
		return driver.NewBothFamiliesDriver(v4, v6), nil
	case ipv4:
		return f.createFamilyDriver(driverType, endpoint, driver.FamilyIPv4, monitor, namespace, client)
	case ipv6:
		return f.createFamilyDriver(driverType, endpoint, driver.FamilyIPv6, monitor, namespace, client)
	default:
		return f.createFamilyDriver(driverType, endpoint, "", monitor, namespace, client)
	}
}

func (f *DriverFactory) createFamilyDriver(driverType, endpoint, family string, monitor *v1alpha1.EndpointMonitor, namespace string, client client.Client) (driver.Driver, error) {
	switch driverType {
	case "http":
		return driver.NewHTTPDriver(endpoint, family, monitor.Spec.TlsCheck, monitor.Spec.HttpCheck)
	case "http-json":
		return driver.NewHTTPJSONDriver(endpoint, family, monitor.Spec.HttpJsonCheck)
	case "smtp":
		return driver.NewSMTPDriver(endpoint, family, monitor.Spec.SmtpCheck, namespace, client)
	case "redis":
		return driver.NewREDISDriver(endpoint, family, monitor.Spec.RedisCheck, namespace, client)
	case "tls":
		return driver.NewTLSDriver(endpoint, family, monitor.Spec.TlsCheck)
	case "tcp":
		return driver.NewTCPDriver(endpoint, family)
	case "dns":
		return driver.NewDNSDriver(endpoint, family, monitor.Spec.DnsCheck)
	case "ping":
		return driver.NewPingDriver(endpoint, family)
	case "trino":
		return driver.NewTrinoDriver(endpoint, family)
	case "opensearch":
		return driver.NewOpenSearchDriver(endpoint, family)
	case "mysql":
		return driver.NewMYSQLDriver(endpoint, family, monitor.Spec.MysqlCheck, namespace, client)
	case "mariadb":
		return driver.NewMariaDBDriver(endpoint, family, monitor.Spec.MysqlCheck, namespace, client)
	case "postgres":
		return driver.NewPostgresDriver(endpoint, family, monitor.Spec.PostgresCheck, namespace, client)
	case "mongodb":
		return driver.NewMongoDBDriver(endpoint, family, monitor.Spec.MongodbCheck, namespace, client)
	case "scylladb":
		return driver.NewScyllaDBDriver(endpoint, family, monitor.Spec.ScylladbCheck, namespace, client)
	default:
		return nil, fmt.Errorf("unsupported driver type: %s", driverType)
	}
}
