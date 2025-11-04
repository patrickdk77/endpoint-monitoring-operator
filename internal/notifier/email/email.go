package email

import (
	"bytes"
	"fmt"
	"net"
	"net/smtp"
	"strings"
	"text/template"
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
		host, _, _ := net.SplitHostPort(cfg.Host)
		auth = smtp.PlainAuth("", cfg.Username, cfg.Password, host)
	}
	return &EmailNotifier{cfg: cfg, auth: auth}, nil
}

func (e *EmailNotifier) SendAlert(status string, values *notifier.NoticeValues) error {
	if !e.shouldAlert(status) {
		return nil // skip silently
	}

	var msg, subject string
	if e.cfg.Subject == "" {
		subject = values.AlertMessage
	} else {
		tmpl, err := template.New("subject").Parse(e.cfg.Subject)
		if err != nil {
			fmt.Printf("EMAIL SMTP: Failed to parse Subject Template: %s\n", err)
			return nil
		}
		var buf bytes.Buffer
		err = tmpl.Execute(&buf, values)
		if err != nil {
			fmt.Printf("EMAIL SMTP: Failed to solve Subject Template: %s\n", err)
			return nil
		}
		subject = buf.String()
	}
	if e.cfg.Body == "" {
		msg = values.AlertMessage
	} else {
		tmpl, err := template.New("body").Parse(e.cfg.Body)
		if err != nil {
			fmt.Printf("EMAIL SMTP: Failed to parse Body Template: %s\n", err)
			return nil
		}
		var buf bytes.Buffer
		err = tmpl.Execute(&buf, values)
		if err != nil {
			fmt.Printf("EMAIL SMTP: Failed to solve Body Template: %s\n", err)
			return nil
		}
		msg = buf.String()
	}
	if e.cfg.EmailProvider == "" || e.cfg.EmailProvider == "smtp" {
		if err := e.SendSMTP(subject, msg, values); err != nil {
			fmt.Printf("EMAIL SMTP: Send failed, Returned %s\n", err)
		}
	}
	//fmt.Printf("EMAIL ALERT: Status=%s, To=%v, From=%s, Message=%s\n",
	//	status, e.cfg.To, e.cfg.From, msg)

	return nil
}

func (e *EmailNotifier) shouldAlert(status string) bool {
	return notifier.ShouldAlert(e.cfg.AlertOn, status)
}

func (e *EmailNotifier) SendSMTP(subject string, msg string, values *notifier.NoticeValues) error {
	body := []byte(
		"From: " + e.cfg.From + "\r\n" +
			"To: " + e.cfg.From + "\r\n" +
			"Date: " + time.Now().Format(time.RFC1123Z) + "\r\n" +
			"Subject: " + subject + "\r\n\r\n" +
			strings.ReplaceAll(msg, "\n", "\r\n") + "\r\n")

	return smtp.SendMail(e.cfg.Host, e.auth, e.cfg.From, e.cfg.To, body)
}
