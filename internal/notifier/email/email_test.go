package email

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"net/smtp"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/patrickdk77/endpoint-monitoring-operator/api/v1alpha1"
	"github.com/patrickdk77/endpoint-monitoring-operator/internal/notifier"
)

type fakeSMTP struct {
	addr     string
	accepted atomic.Int32
	mu       sync.Mutex
	rcpts    []string
	data     string
}

// startSMTP serves a minimal SMTP dialogue. It never answers when greet is
// false, stops answering at the command named by stallAt, and refuses
// recipients when rejectRcpt is set.
func startSMTP(t *testing.T, greet bool, stallAt string, rejectRcpt bool) *fakeSMTP {
	t.Helper()
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	s := &fakeSMTP{addr: ln.Addr().String()}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			s.accepted.Add(1)
			go s.serve(c, greet, stallAt, rejectRcpt)
		}
	}()
	return s
}

func (s *fakeSMTP) serve(c net.Conn, greet bool, stallAt string, rejectRcpt bool) {
	defer c.Close()
	if !greet {
		_, _ = io.Copy(io.Discard, c)
		return
	}
	r := bufio.NewReader(c)
	fmt.Fprintf(c, "220 test ESMTP\r\n")
	inData := false
	var body strings.Builder
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return
		}
		if inData {
			if line == ".\r\n" {
				inData = false
				s.mu.Lock()
				s.data = body.String()
				s.mu.Unlock()
				fmt.Fprintf(c, "250 queued\r\n")
				continue
			}
			body.WriteString(line)
			continue
		}
		cmd := strings.ToUpper(strings.TrimSpace(line))
		if stallAt != "" && strings.HasPrefix(cmd, stallAt) {
			_, _ = io.Copy(io.Discard, r)
			return
		}
		switch {
		case strings.HasPrefix(cmd, "EHLO"), strings.HasPrefix(cmd, "HELO"):
			fmt.Fprintf(c, "250 test\r\n")
		case strings.HasPrefix(cmd, "RCPT"):
			if rejectRcpt {
				fmt.Fprintf(c, "550 no such user\r\n")
				continue
			}
			s.mu.Lock()
			s.rcpts = append(s.rcpts, strings.TrimSpace(line[len("RCPT TO:"):]))
			s.mu.Unlock()
			fmt.Fprintf(c, "250 ok\r\n")
		case cmd == "DATA":
			inData = true
			fmt.Fprintf(c, "354 go ahead\r\n")
		case cmd == "QUIT":
			fmt.Fprintf(c, "221 bye\r\n")
			return
		default:
			fmt.Fprintf(c, "250 ok\r\n")
		}
	}
}

func sendWithin(t *testing.T, limit time.Duration, send func() error) error {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- send() }()
	select {
	case err := <-done:
		return err
	case <-time.After(limit):
		t.Fatalf("still sending after %v", limit)
		return nil
	}
}

func newNotifier(t *testing.T, host, from string, to ...string) notifier.Notifier {
	t.Helper()
	n, err := New(&v1alpha1.EmailConfig{Enabled: true, Host: host, From: from, To: to})
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func TestSendSMTPDelivers(t *testing.T) {
	s := startSMTP(t, true, "", false)
	n := newNotifier(t, s.addr, "monitor@example.test", "oncall@example.test", "backup@example.test")
	if err := sendWithin(t, 5*time.Second, func() error {
		return n.SendAlert("failure", &notifier.NoticeValues{AlertMessage: "api is down"}, nil)
	}); err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if strings.Join(s.rcpts, ",") != "<oncall@example.test>,<backup@example.test>" {
		t.Fatalf("recipients %v", s.rcpts)
	}
	if !strings.Contains(s.data, "Subject: api is down\r\n") || !strings.Contains(s.data, "\r\n\r\napi is down\r\n") {
		t.Fatalf("message:\n%s", s.data)
	}
	if !strings.Contains(s.data, "To: oncall@example.test, backup@example.test\r\n") || !strings.Contains(s.data, "From: monitor@example.test\r\n") {
		t.Fatalf("headers:\n%s", s.data)
	}
}

func TestNewRejectsUnparsableTemplates(t *testing.T) {
	for _, cfg := range []v1alpha1.EmailConfig{{Subject: "{{.Status"}, {Body: "{{if .Status}}"}} {
		cfg.Enabled, cfg.Host, cfg.From, cfg.To = true, "smtp.example.test:25", "m@example.test", []string{"oncall@example.test"}
		if _, err := New(&cfg); err == nil || !strings.Contains(err.Error(), "failed to parse the") {
			t.Errorf("subject %q body %q: err = %v", cfg.Subject, cfg.Body, err)
		}
	}
}

func TestNewValidatesEmailProvider(t *testing.T) {
	tests := []struct {
		cfg     v1alpha1.EmailConfig
		wantErr string
	}{
		{v1alpha1.EmailConfig{EmailProvider: "sendgrid", Host: "smtp.example.test:25", From: "m@example.test"}, `unsupported emailProvider "sendgrid"`},
		{v1alpha1.EmailConfig{EmailProvider: "SES"}, "from field is required for ses"},
		{v1alpha1.EmailConfig{EmailProvider: "ses", From: "m@example.test"}, ""},
		{v1alpha1.EmailConfig{EmailProvider: "Smtp", Host: "smtp.example.test:25", From: "m@example.test"}, ""},
		{v1alpha1.EmailConfig{Host: "smtp.example.test:25", From: "m@example.test"}, ""},
	}
	for _, tc := range tests {
		cfg := tc.cfg
		cfg.Enabled, cfg.To = true, []string{"oncall@example.test"}
		_, err := New(&cfg)
		if tc.wantErr == "" && err != nil || tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)) {
			t.Errorf("provider %q: err = %v, want %q", tc.cfg.EmailProvider, err, tc.wantErr)
		}
	}
}

func TestSendSMTPGivesUpOnStalledServer(t *testing.T) {
	old := notifier.Timeout
	notifier.Timeout = 200 * time.Millisecond
	t.Cleanup(func() { notifier.Timeout = old })
	tests := []struct {
		name    string
		greet   bool
		stallAt string
	}{
		{"no greeting", false, ""},
		{"stalls at EHLO", true, "EHLO"},
		{"stalls at RCPT", true, "RCPT"},
		{"stalls at DATA", true, "DATA"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := startSMTP(t, tc.greet, tc.stallAt, false)
			n := newNotifier(t, s.addr, "monitor@example.test", "oncall@example.test")
			err := sendWithin(t, 5*time.Second, func() error {
				return n.SendAlert("failure", &notifier.NoticeValues{AlertMessage: "down"}, nil)
			})
			if err == nil || !strings.Contains(err.Error(), "i/o timeout") {
				t.Fatalf("err = %v, want an i/o timeout", err)
			}
		})
	}
}

func TestSendSMTPRefusesLineBreaksInAddresses(t *testing.T) {
	tests := []struct {
		name string
		from string
		to   string
	}{
		{"from", "monitor@example.test\r\nRCPT TO:<attacker@example.test>", "oncall@example.test"},
		{"to", "monitor@example.test", "oncall@example.test\nRCPT TO:<attacker@example.test>"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := startSMTP(t, true, "", false)
			n := newNotifier(t, s.addr, tc.from, tc.to)
			err := sendWithin(t, 5*time.Second, func() error {
				return n.SendAlert("failure", &notifier.NoticeValues{AlertMessage: "down"}, nil)
			})
			if err == nil || !strings.Contains(err.Error(), "must not contain CR or LF") {
				t.Fatalf("err = %v", err)
			}
			if s.accepted.Load() != 0 {
				t.Fatal("connected to the server before rejecting the address")
			}
		})
	}
}

func TestSendSMTPReportsRefusedRecipient(t *testing.T) {
	s := startSMTP(t, true, "", true)
	n := newNotifier(t, s.addr, "monitor@example.test", "nobody@example.test")
	err := sendWithin(t, 5*time.Second, func() error {
		return n.SendAlert("failure", &notifier.NoticeValues{AlertMessage: "down"}, nil)
	})
	if err == nil || !strings.Contains(err.Error(), "550") {
		t.Fatalf("err = %v, want the 550 refusal", err)
	}
}

func TestSendMailNeedsAuthExtensionForCredentials(t *testing.T) {
	s := startSMTP(t, true, "", false)
	err := sendWithin(t, 5*time.Second, func() error {
		return sendMail(s.addr, smtp.PlainAuth("", "user", "pass", "127.0.0.1"), "monitor@example.test", []string{"oncall@example.test"}, []byte("hi\r\n"))
	})
	if err == nil || !strings.Contains(err.Error(), "doesn't support AUTH") {
		t.Fatalf("err = %v", err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.rcpts) != 0 {
		t.Fatalf("mail was sent without the configured credentials: %v", s.rcpts)
	}
}

func TestSendMailNeedsAuthFromHELOOnlyServer(t *testing.T) {
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	var mailFrom atomic.Int32
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		r := bufio.NewReader(c)
		fmt.Fprintf(c, "220 old ESMTP-less server\r\n")
		for {
			line, err := r.ReadString('\n')
			if err != nil {
				return
			}
			switch cmd := strings.ToUpper(strings.TrimSpace(line)); {
			case strings.HasPrefix(cmd, "EHLO"):
				fmt.Fprintf(c, "502 command not implemented\r\n")
			case strings.HasPrefix(cmd, "MAIL"):
				mailFrom.Add(1)
				fmt.Fprintf(c, "250 ok\r\n")
			case cmd == "QUIT":
				fmt.Fprintf(c, "221 bye\r\n")
				return
			default:
				fmt.Fprintf(c, "250 ok\r\n")
			}
		}
	}()
	err = sendWithin(t, 5*time.Second, func() error {
		return sendMail(ln.Addr().String(), smtp.PlainAuth("", "user", "pass", "127.0.0.1"), "monitor@example.test", []string{"oncall@example.test"}, []byte("hi\r\n"))
	})
	if err == nil || !strings.Contains(err.Error(), "doesn't support AUTH") {
		t.Fatalf("err = %v", err)
	}
	if mailFrom.Load() != 0 {
		t.Fatal("mail was sent without the configured credentials")
	}
}

func TestSendSESGivesUpOnStalledEndpoint(t *testing.T) {
	old := notifier.Timeout
	notifier.Timeout = 300 * time.Millisecond
	t.Cleanup(func() { notifier.Timeout = old })

	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				_, _ = io.Copy(io.Discard, c)
			}()
		}
	}()
	t.Setenv("AWS_ENDPOINT_URL", "http://"+ln.Addr().String())
	t.Setenv("AWS_REGION", "us-east-1")
	t.Setenv("AWS_ACCESS_KEY_ID", "test")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "test")
	t.Setenv("AWS_EC2_METADATA_DISABLED", "true")
	t.Setenv("AWS_CONFIG_FILE", os.DevNull)
	t.Setenv("AWS_SHARED_CREDENTIALS_FILE", os.DevNull)

	n, err := New(&v1alpha1.EmailConfig{Enabled: true, EmailProvider: "ses", From: "monitor@example.test", To: []string{"oncall@example.test"}})
	if err != nil {
		t.Fatal(err)
	}
	err = sendWithin(t, 5*time.Second, func() error {
		return n.SendAlert("failure", &notifier.NoticeValues{AlertMessage: "down"}, nil)
	})
	if err == nil || !strings.Contains(err.Error(), "deadline exceeded") {
		t.Fatalf("err = %v, want the context deadline", err)
	}
}
