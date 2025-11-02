package email

import (
	"fmt"

	"github.com/LiciousTech/endpoint-monitoring-operator/api/v1alpha1"
	"github.com/LiciousTech/endpoint-monitoring-operator/internal/notifier"
)

type EmailNotifier struct {
	cfg *v1alpha1.EmailConfig
}

func New(cfg *v1alpha1.EmailConfig) (notifier.Notifier, error) {
	if cfg == nil || !cfg.Enabled {
		return nil, fmt.Errorf("email config is nil or disabled")
	}

	// Basic validation
	if cfg.From == "" || len(cfg.To) == 0 {
		return nil, fmt.Errorf("invalid email configuration: from and to fields are required")
	}

	return &EmailNotifier{cfg: cfg}, nil
}

func (e *EmailNotifier) SendAlert(status string, msg string) error {
	if !e.shouldAlert(status) {
		return nil // skip silently
	}

	// TODO: Actual SES or SMTP integration can go here
	// For now, log output
	fmt.Printf("EMAIL ALERT: Status=%s, To=%v, From=%s, Message=%s\n",
		status, e.cfg.To, e.cfg.From, msg)

	return nil
}

func (e *EmailNotifier) shouldAlert(status string) bool {
	return notifier.ShouldAlert(e.cfg.AlertOn, status)
}
