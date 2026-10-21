package email

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"net/smtp"
	"strings"
	"text/template"
	"time"

	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/sesv2"
	"github.com/aws/aws-sdk-go-v2/service/sesv2/types"
	"github.com/patrickdk77/endpoint-monitoring-operator/api/v1alpha1"
	"github.com/patrickdk77/endpoint-monitoring-operator/internal/common/smtpAuth"
	"github.com/patrickdk77/endpoint-monitoring-operator/internal/notifier"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type EmailNotifier struct {
	cfg     *v1alpha1.EmailConfig
	subject *template.Template
	body    *template.Template
}

func New(cfg *v1alpha1.EmailConfig) (notifier.Notifier, error) {
	if cfg == nil || !cfg.Enabled {
		return nil, fmt.Errorf("email config is nil or disabled")
	}

	provider := strings.ToUpper(cfg.EmailProvider)
	switch provider {
	case "", "SMTP":
	case "SES":
		if cfg.From == "" {
			return nil, fmt.Errorf("invalid email configuration: from field is required for ses")
		}
	default:
		return nil, fmt.Errorf("unsupported emailProvider %q, must be smtp or ses", cfg.EmailProvider)
	}
	if cfg.EmailSecretRef.Name == "" && (provider == "" || provider == "SMTP") {
		if provider == "" && cfg.Host == "" {
			return nil, fmt.Errorf("email config is nil or missing Host or emailProvider")
		}
		if provider == "SMTP" && cfg.Host == "" {
			return nil, fmt.Errorf("emailProvider is smtp and missing Host")
		}
		if cfg.From == "" {
			return nil, fmt.Errorf("invalid email configuration: from field is required")
		}
	}

	if len(cfg.To) == 0 {
		return nil, fmt.Errorf("invalid email configuration: to array is required")
	}

	e := &EmailNotifier{cfg: cfg}
	if cfg.Subject != "" {
		tmpl, err := template.New("subject").Parse(cfg.Subject)
		if err != nil {
			return nil, fmt.Errorf("failed to parse the subject template: %w", err)
		}
		e.subject = tmpl
	}
	if cfg.Body != "" {
		tmpl, err := template.New("body").Parse(cfg.Body)
		if err != nil {
			return nil, fmt.Errorf("failed to parse the body template: %w", err)
		}
		e.body = tmpl
	}
	return e, nil
}

func (e *EmailNotifier) SendAlert(status string, values *notifier.NoticeValues, client client.Client) error {
	if !e.shouldAlert(status) {
		return nil // skip silently
	}

	var msg, subject string
	if e.subject == nil {
		subject = values.AlertMessage
	} else {
		var buf bytes.Buffer
		err := e.subject.Execute(&buf, values)
		if err != nil {
			return fmt.Errorf("failed to execute the subject template: %w", err)
		}
		subject = buf.String()
	}
	if e.body == nil {
		msg = values.AlertMessage
	} else {
		var buf bytes.Buffer
		err := e.body.Execute(&buf, values)
		if err != nil {
			return fmt.Errorf("failed to execute the body template: %w", err)
		}
		msg = buf.String()
	}
	provider := strings.ToUpper(e.cfg.EmailProvider)
	if provider == "" || provider == "SMTP" {
		if err := e.SendSMTP(subject, msg, values, client); err != nil {
			return fmt.Errorf("smtp send failed: %w", err)
		}
	}
	if provider == "SES" {
		if err := e.SendSES(subject, msg, values); err != nil {
			return fmt.Errorf("ses send failed: %w", err)
		}
	}
	//fmt.Printf("EMAIL ALERT: Status=%s, To=%v, From=%s, Message=%s\n",
	//	status, e.cfg.To, e.cfg.From, msg)

	return nil
}

func (e *EmailNotifier) shouldAlert(status string) bool {
	return notifier.ShouldAlert(e.cfg.AlertOn, status)
}

func (e *EmailNotifier) SendSMTP(subject string, msg string, values *notifier.NoticeValues, client client.Client) error {
	host := e.cfg.Host
	from := e.cfg.From

	var auth smtp.Auth = nil
	if e.cfg.EmailSecretRef.Name != "" {
		secret, err := notifier.GetSecret(e.cfg.EmailSecretRef.Name, values.Namespace, client)
		if err != nil {
			return fmt.Errorf("unable to read secret: %w", err)
		}

		var identity, username, password string
		identity = ""
		if identitySecret, ok := secret.Data["identity"]; ok {
			identity = string(identitySecret)
		}
		if usernameSecret, ok := secret.Data["username"]; ok {
			username = string(usernameSecret)
		}
		if passwordSecret, ok := secret.Data["password"]; ok {
			password = string(passwordSecret)
		}
		if hostSecret, ok := secret.Data["host"]; ok {
			host = string(hostSecret)
		}
		if fromSecret, ok := secret.Data["from"]; ok {
			from = string(fromSecret)
		}

		hostPart, _, _ := net.SplitHostPort(host)
		if username != "" && password != "" {
			auth = smtpAuth.CommonAuth(identity, username, password, hostPart)
		}
	}

	if host == "" {
		return fmt.Errorf("emailProvider is smtp and missing host")
	}
	if from == "" {
		return fmt.Errorf("invalid email configuration: from field is required")
	}

	body := []byte(
		"From: " + from + "\r\n" +
			"To: " + strings.Join(e.cfg.To, ", ") + "\r\n" +
			"Date: " + time.Now().Format(time.RFC1123Z) + "\r\n" +
			"Subject: " + escapeUTF8(subject) + "\r\n\r\n" +
			strings.ReplaceAll(msg, "\n", "\r\n") + "\r\n")

	return sendMail(host, auth, from, e.cfg.To, body)
}

func (e *EmailNotifier) SendSES(subject string, msg string, values *notifier.NoticeValues) error {
	ctx, cancel := context.WithTimeout(context.Background(), notifier.Timeout)
	defer cancel()
	sdkcfg, err := config.LoadDefaultConfig(ctx)
	if err != nil {
		return err
	}

	sesClient := sesv2.NewFromConfig(sdkcfg)

	body := strings.ReplaceAll(msg, "\n", "\r\n") + "\r\n"

	charset := "UTF-8"
	params := sesv2.SendEmailInput{
		Content: &types.EmailContent{
			Simple: &types.Message{
				Subject: &types.Content{
					Data:    &subject,
					Charset: &charset,
				},
				Body: &types.Body{
					Text: &types.Content{
						Data:    &body,
						Charset: &charset,
					},
				},
			},
		},
		Destination: &types.Destination{
			ToAddresses: e.cfg.To,
		},
		FromEmailAddress: &e.cfg.From,
	}

	_, err = sesClient.SendEmail(ctx, &params)
	if err != nil {
		return err
	}

	return nil
}

func escapeUTF8(s string) string {
	for _, r := range s {
		if r > 127 || r < 32 {
			return "=?utf-8?B?" + base64.StdEncoding.EncodeToString([]byte(s)) + "?="
		}
	}
	return s
}

// sendMail is smtp.SendMail with one deadline for the dial and the whole
// conversation. Unlike smtp.SendMail, it refuses to send when credentials
// are set and the server offers no AUTH.
func sendMail(addr string, a smtp.Auth, from string, to []string, msg []byte) error {
	if err := validateLine(from); err != nil {
		return err
	}
	for _, recp := range to {
		if err := validateLine(recp); err != nil {
			return err
		}
	}
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return err
	}
	deadline := time.Now().Add(notifier.Timeout)
	dialer := net.Dialer{Deadline: deadline}
	conn, err := dialer.Dial("tcp", addr)
	if err != nil {
		return err
	}
	defer conn.Close()
	if err := conn.SetDeadline(deadline); err != nil {
		return err
	}
	c, err := smtp.NewClient(conn, host)
	if err != nil {
		return err
	}
	defer c.Close()
	if err = c.Hello("localhost"); err != nil {
		return err
	}
	if ok, _ := c.Extension("STARTTLS"); ok {
		if err = c.StartTLS(&tls.Config{ServerName: host}); err != nil {
			return err
		}
	}
	if a != nil {
		if ok, _ := c.Extension("AUTH"); !ok {
			return errors.New("smtp: server doesn't support AUTH")
		}
		if err = c.Auth(a); err != nil {
			return err
		}
	}
	if err = c.Mail(from); err != nil {
		return err
	}
	for _, addr := range to {
		if err = c.Rcpt(addr); err != nil {
			return err
		}
	}
	w, err := c.Data()
	if err != nil {
		return err
	}
	if _, err = w.Write(msg); err != nil {
		return err
	}
	if err = w.Close(); err != nil {
		return err
	}
	return c.Quit()
}

func validateLine(line string) error {
	if strings.ContainsAny(line, "\n\r") {
		return errors.New("smtp: A line must not contain CR or LF")
	}
	return nil
}
