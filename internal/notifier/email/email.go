package email

import (
	"fmt"
	"net/smtp"
	"time"

	"github.com/LiciousTech/endpoint-monitoring-operator/api/v1alpha1"
	"github.com/LiciousTech/endpoint-monitoring-operator/internal/notifier"
)

type EmailNotifier struct {
	cfg  *v1alpha1.EmailConfig
	auth smtp.Auth
}

func New(cfg *v1alpha1.EmailConfig) (notifier.Notifier, error) {
	if cfg == nil || !cfg.Enabled {
		return nil, fmt.Errorf("email config is nil or disabled")
	}

	if cfg.EmailProvider == "" && cfg.Host == "" {
		return nil, fmt.Errorf("email config is nil or missing Host or emailProvider")
	}

	if cfg.EmailProvider == "smtp" && cfg.Host == "" {
		return nil, fmt.Errorf("emailProvider is smtp and missing Host")
	}

	// Basic validation
	if cfg.From == "" || len(cfg.To) == 0 {
		return nil, fmt.Errorf("invalid email configuration: from and to fields are required")
	}

	var auth smtp.Auth = nil
	if cfg.Username != "" && cfg.Password != "" && cfg.Host != "" {
		auth = smtp.PlainAuth("", cfg.Username, cfg.Password, cfg.Host)
	}
	return &EmailNotifier{cfg: cfg, auth: auth}, nil
}

func (e *EmailNotifier) SendAlert(status string, msg string) error {
	if !e.shouldAlert(status) {
		return nil // skip silently
	}

	if e.cfg.EmailProvider == "" || e.cfg.EmailProvider == "smtp" {
		e.SendSMTP(msg)
	}
	//fmt.Printf("EMAIL ALERT: Status=%s, To=%v, From=%s, Message=%s\n",
	//	status, e.cfg.To, e.cfg.From, msg)

	return nil
}

func (e *EmailNotifier) shouldAlert(status string) bool {
	return notifier.ShouldAlert(e.cfg.AlertOn, status)
}

func (e *EmailNotifier) SendSMTP(msg string) error {
	subject := e.cfg.Subject
	if subject == "" {
		subject = msg
	}
	body := []byte(
		"From: " + e.cfg.From + "\r\n" +
			"To: " + e.cfg.From + "\r\n" +
			"Date: " + time.Now().Format(time.RFC1123Z) + "\r\n" +
			"Subject: " + subject + "\r\n\r\n" +
			msg + "\r\n")

	return smtp.SendMail(e.cfg.Host, e.auth, e.cfg.From, e.cfg.To, body)
}
