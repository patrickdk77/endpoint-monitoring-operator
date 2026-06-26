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

	var notifiers []notifier.Notifier

	if config.Slack != nil && config.Slack.Enabled {
		slackNotifier, err := slack.New(config.Slack)
		if err != nil {
			return nil, fmt.Errorf("failed to create Slack notifier: %w", err)
		}
		notifiers = append(notifiers, slackNotifier)
	}

	if config.Email != nil && config.Email.Enabled {
		emailNotifier, err := email.New(config.Email)
		if err != nil {
			return nil, fmt.Errorf("failed to create Email notifier: %w", err)
		}
		notifiers = append(notifiers, emailNotifier)
	}

	if config.Discord != nil && config.Discord.Enabled {
		discordNotifier, err := discord.New(config.Discord)
		if err != nil {
			return nil, fmt.Errorf("failed to create Discord notifier: %w", err)
		}
		notifiers = append(notifiers, discordNotifier)
	}

	if config.Webhook != nil && config.Webhook.Enabled {
		webhookNotifier, err := webhook.New(config.Webhook)
		if err != nil {
			return nil, fmt.Errorf("failed to create Webhook notifier: %w", err)
		}
		notifiers = append(notifiers, webhookNotifier)
	}

	if config.Valkey != nil && config.Valkey.Enabled {
		valkeyNotifier, err := valkey.New(config.Valkey)
		if err != nil {
			return nil, fmt.Errorf("failed to create Valkey notifier: %w", err)
		}
		notifiers = append(notifiers, valkeyNotifier)
	}

	if len(notifiers) == 0 {
		return nil, fmt.Errorf("no notifiers enabled")
	}

	return &CompositeNotifier{notifiers: notifiers}, nil
}

// CompositeNotifier handles multiple notification channels
type CompositeNotifier struct {
	notifiers []notifier.Notifier
}

// SendAlert sends alerts to all configured notifiers
func (c *CompositeNotifier) SendAlert(status string, values *notifier.NoticeValues, client client.Client) error {
	var errs []error
	for _, n := range c.notifiers {
		if err := n.SendAlert(status, values, client); err != nil {
			errs = append(errs, err)
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("failed to send alerts: %v", errs)
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
	switch driverType {
	case "http":
		return driver.NewHTTPDriver(endpoint, monitor.Spec.TlsCheck, monitor.Spec.HttpCheck)
	case "http-json":
		return driver.NewHTTPJSONDriver(endpoint, monitor.Spec.HttpJsonCheck)
	case "smtp":
		return driver.NewSMTPDriver(endpoint, monitor.Spec.SmtpCheck, namespace, client)
	case "redis":
		return driver.NewREDISDriver(endpoint, monitor.Spec.RedisCheck, namespace, client)
	case "tls":
		return driver.NewTLSDriver(endpoint, monitor.Spec.TlsCheck)
	case "tcp":
		return driver.NewTCPDriver(endpoint)
	case "dns":
		return driver.NewDNSDriver(endpoint, monitor.Spec.DnsCheck)
	case "ping":
		return driver.NewPingDriver(endpoint)
	case "trino":
		return driver.NewTrinoDriver(endpoint)
	case "opensearch":
		return driver.NewOpenSearchDriver(endpoint)
	case "mysql":
		return driver.NewMYSQLDriver(endpoint, monitor.Spec.MysqlCheck, namespace, client)
	default:
		return nil, fmt.Errorf("unsupported driver type: %s", driverType)
	}
}
