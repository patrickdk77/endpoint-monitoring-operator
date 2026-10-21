package driver

import (
	"bufio"
	"crypto/tls"
	"encoding/base64"
	"fmt"
	"net"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"

	v1 "github.com/patrickdk77/endpoint-monitoring-operator/api/v1alpha1"
)

type fakeSMTPOptions struct {
	implicitTLS       bool
	startTLS          bool
	mechanisms        string
	user, pass        string
	echoAuth          bool
	echoPassword      bool
	challengePassword bool
	ignoreQuit        bool
	rejectMail        bool
}

type fakeSMTPServer struct {
	addr     string
	mu       sync.Mutex
	commands []string
	closed   int
}

func (s *fakeSMTPServer) record(cmd string) {
	s.mu.Lock()
	s.commands = append(s.commands, cmd)
	s.mu.Unlock()
}

// receivedBeforeClose returns every command the client sent, once the
// connection has ended.
func (s *fakeSMTPServer) receivedBeforeClose(t *testing.T) []string {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		s.mu.Lock()
		closed, commands := s.closed, append([]string(nil), s.commands...)
		s.mu.Unlock()
		if closed > 0 {
			return commands
		}
		if time.Now().After(deadline) {
			t.Fatalf("the connection never ended; commands so far %v", commands)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func startFakeSMTP(t *testing.T, opts fakeSMTPOptions) *fakeSMTPServer {
	t.Helper()
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	s := &fakeSMTPServer{addr: ln.Addr().String()}
	tlsConfig := &tls.Config{Certificates: []tls.Certificate{testCert}}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			if opts.implicitTLS {
				c = tls.Server(c, tlsConfig)
			}
			go s.serve(c, opts, tlsConfig)
		}
	}()
	return s
}

func (s *fakeSMTPServer) serve(c net.Conn, opts fakeSMTPOptions, tlsConfig *tls.Config) {
	defer func() {
		s.mu.Lock()
		s.closed++
		s.mu.Unlock()
	}()
	defer c.Close()
	secure := opts.implicitTLS
	r := bufio.NewReader(c)
	readLine := func() (string, bool) {
		line, err := r.ReadString('\n')
		return strings.TrimRight(line, "\r\n"), err == nil
	}
	check := func(user, pass, sent string) {
		switch {
		case user == opts.user && pass == opts.pass:
			fmt.Fprintf(c, "235 authenticated\r\n")
		case opts.echoAuth:
			fmt.Fprintf(c, "535 rejected: %s\r\n", sent)
		case opts.echoPassword:
			fmt.Fprintf(c, "535 bad password %s\r\n", pass)
		default:
			fmt.Fprintf(c, "535 authentication failed\r\n")
		}
	}
	fmt.Fprintf(c, "220 test ESMTP\r\n")
	for {
		line, ok := readLine()
		if !ok {
			return
		}
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		verb := strings.ToUpper(fields[0])
		s.record(verb)
		switch verb {
		case "EHLO":
			fmt.Fprintf(c, "250-test\r\n")
			if opts.startTLS && !secure {
				fmt.Fprintf(c, "250-STARTTLS\r\n")
			}
			if opts.mechanisms != "" {
				fmt.Fprintf(c, "250-AUTH %s\r\n", opts.mechanisms)
			}
			fmt.Fprintf(c, "250 OK\r\n")
		case "STARTTLS":
			fmt.Fprintf(c, "220 ready\r\n")
			tc := tls.Server(c, tlsConfig)
			if tc.Handshake() != nil {
				return
			}
			c, r, secure = tc, bufio.NewReader(tc), true
		case "AUTH":
			if opts.mechanisms == "" {
				fmt.Fprintf(c, "502 not implemented\r\n")
				continue
			}
			if len(fields) < 2 {
				fmt.Fprintf(c, "501 syntax error\r\n")
				continue
			}
			switch strings.ToUpper(fields[1]) {
			case "PLAIN":
				if len(fields) < 3 {
					fmt.Fprintf(c, "501 syntax error\r\n")
					continue
				}
				raw, _ := base64.StdEncoding.DecodeString(fields[2])
				parts := strings.Split(string(raw), "\x00")
				if len(parts) != 3 {
					fmt.Fprintf(c, "501 malformed\r\n")
					continue
				}
				check(parts[1], parts[2], line)
			case "LOGIN":
				if opts.challengePassword {
					fmt.Fprintf(c, "334 %s\r\n", base64.StdEncoding.EncodeToString([]byte(opts.pass)))
					_, _ = readLine()
					fmt.Fprintf(c, "501 aborted\r\n")
					continue
				}
				fmt.Fprintf(c, "334 %s\r\n", base64.StdEncoding.EncodeToString([]byte("Username:")))
				u, _ := readLine()
				fmt.Fprintf(c, "334 %s\r\n", base64.StdEncoding.EncodeToString([]byte("Password:")))
				p, _ := readLine()
				user, _ := base64.StdEncoding.DecodeString(u)
				pass, _ := base64.StdEncoding.DecodeString(p)
				check(string(user), string(pass), p)
			}
		case "QUIT":
			if opts.ignoreQuit {
				continue
			}
			fmt.Fprintf(c, "221 bye\r\n")
			return
		case "MAIL":
			if opts.rejectMail {
				fmt.Fprintf(c, "550 sender rejected\r\n")
			} else {
				fmt.Fprintf(c, "250 ok\r\n")
			}
		default:
			fmt.Fprintf(c, "250 ok\r\n")
		}
	}
}

func TestSMTPDriverAuth(t *testing.T) {
	const user, pass = "monitor@example.test", "s3cret pass"
	tests := []struct {
		name      string
		opts      fakeSMTPOptions
		check     v1.SmtpCheck
		secret    map[string]string
		wantOK    bool
		wantInMsg string
		wantAuth  bool
	}{
		{"STARTTLS with PLAIN", fakeSMTPOptions{startTLS: true, mechanisms: "PLAIN LOGIN", user: user, pass: pass},
			v1.SmtpCheck{StartTls: true}, map[string]string{"username": user, "password": pass}, true, "SMTP check successful", true},
		{"implicit TLS with PLAIN", fakeSMTPOptions{implicitTLS: true, mechanisms: "PLAIN", user: user, pass: pass},
			v1.SmtpCheck{Tls: true}, map[string]string{"username": user, "password": pass}, true, "SMTP check successful", true},
		{"STARTTLS with LOGIN only", fakeSMTPOptions{startTLS: true, mechanisms: "LOGIN", user: user, pass: pass},
			v1.SmtpCheck{StartTls: true}, map[string]string{"username": user, "password": pass}, true, "SMTP check successful", true},
		{"no credentials", fakeSMTPOptions{startTLS: true, mechanisms: "PLAIN"},
			v1.SmtpCheck{StartTls: true}, nil, true, "SMTP check successful", false},
		{"wrong password", fakeSMTPOptions{startTLS: true, mechanisms: "PLAIN", user: user, pass: pass},
			v1.SmtpCheck{StartTls: true}, map[string]string{"username": user, "password": "wrong"}, false, `535 "authentication failed"`, true},
		{"credentials without TLS", fakeSMTPOptions{mechanisms: "PLAIN", user: user, pass: pass},
			v1.SmtpCheck{}, map[string]string{"username": user, "password": pass}, false, "insecure connection", false},
		{"server without AUTH", fakeSMTPOptions{startTLS: true},
			v1.SmtpCheck{StartTls: true}, map[string]string{"username": user, "password": pass}, false, `502 "not implemented"`, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := startFakeSMTP(t, tc.opts)
			check := tc.check
			var d Driver
			var err error
			if tc.secret != nil {
				check.EmailSecretRef.Name = "creds"
				d, err = NewSMTPDriver(s.addr, "", &check, "default", secretClient(t, tc.secret))
			} else {
				d, err = NewSMTPDriver(s.addr, "", &check, "default", nil)
			}
			if err != nil {
				t.Fatal(err)
			}
			res, _ := d.Check()
			if res.Success != tc.wantOK || !strings.Contains(res.Message, tc.wantInMsg) {
				t.Fatalf("success=%v %s", res.Success, res.Message)
			}
			if strings.Contains(res.Message, pass) {
				t.Fatalf("the message leaks the password: %s", res.Message)
			}
			got := s.receivedBeforeClose(t)
			if len(got) == 0 || got[len(got)-1] != "QUIT" {
				t.Fatalf("the session did not end with QUIT: %v", got)
			}
			sentAuth := strings.Contains(strings.Join(got, " "), "AUTH")
			if sentAuth != tc.wantAuth {
				t.Fatalf("AUTH sent=%v, want %v: %v", sentAuth, tc.wantAuth, got)
			}
		})
	}
}

func TestSMTPDriverDoesNotWaitForQuitReply(t *testing.T) {
	withCheckTimeout(t, 5*time.Second)
	s := startFakeSMTP(t, fakeSMTPOptions{ignoreQuit: true})
	d, _ := NewSMTPDriver(s.addr, "", &v1.SmtpCheck{FromAssertion: "monitor@example.test"}, "default", nil)
	start := time.Now()
	res, _ := d.Check()
	if elapsed := time.Since(start); !res.Success || elapsed > time.Second {
		t.Fatalf("success=%v after %v: %s", res.Success, elapsed, res.Message)
	}
	if got := s.receivedBeforeClose(t); got[len(got)-1] != "QUIT" {
		t.Fatalf("the session did not end with QUIT: %v", got)
	}
}

func TestSMTPDriverQuitsAfterARejectedCommand(t *testing.T) {
	s := startFakeSMTP(t, fakeSMTPOptions{rejectMail: true})
	d, _ := NewSMTPDriver(s.addr, "", &v1.SmtpCheck{FromAssertion: "monitor@example.test"}, "default", nil)
	res, _ := d.Check()
	if res.Success || !strings.Contains(res.Message, `550 "sender rejected"`) {
		t.Fatalf("success=%v %s", res.Success, res.Message)
	}
	if got := s.receivedBeforeClose(t); strings.Join(got, " ") != "EHLO MAIL QUIT" {
		t.Fatalf("commands %v, want EHLO MAIL QUIT", got)
	}
}

func TestSMTPDriverWithholdsEchoedCredentials(t *testing.T) {
	const user = "monitor@example.test"
	tests := []struct {
		name, mechanisms, pass string
		opts                   fakeSMTPOptions
	}{
		{"AUTH PLAIN line", "PLAIN", "s3cret pass", fakeSMTPOptions{echoAuth: true}},
		{"AUTH LOGIN password line", "LOGIN", "s3cret pass", fakeSMTPOptions{echoAuth: true}},
		{"decoded password", "PLAIN", "s3cret pass", fakeSMTPOptions{echoPassword: true}},
		{"decoded password with a quote", "PLAIN", `pa"ss`, fakeSMTPOptions{echoPassword: true}},
		{"decoded password with a backslash", "PLAIN", `pa\ss`, fakeSMTPOptions{echoPassword: true}},
		{"decoded password with a tab", "PLAIN", "pa\tss", fakeSMTPOptions{echoPassword: true}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			opts := tc.opts
			opts.startTLS, opts.mechanisms, opts.user, opts.pass = true, tc.mechanisms, user, "other"
			s := startFakeSMTP(t, opts)
			check := &v1.SmtpCheck{StartTls: true, EmailSecretRef: v1.SecretRef{Name: "creds"}}
			d, err := NewSMTPDriver(s.addr, "", check, "default", secretClient(t, map[string]string{"username": user, "password": tc.pass}))
			if err != nil {
				t.Fatal(err)
			}
			res, _ := d.Check()
			if res.Success || res.Message != "SMTP check failed: AUTH rejected with 535; the reply is withheld because it repeats the credentials" {
				t.Fatalf("success=%v %s", res.Success, res.Message)
			}
			quoted := strconv.Quote(tc.pass)
			for _, leak := range []string{
				tc.pass,
				quoted[1 : len(quoted)-1],
				base64.StdEncoding.EncodeToString([]byte(tc.pass)),
				base64.StdEncoding.EncodeToString([]byte("\x00" + user + "\x00" + tc.pass)),
			} {
				if strings.Contains(res.Message, leak) || strings.Contains(res.Error.Error(), leak) {
					t.Fatalf("the result carries %q: %s / %v", leak, res.Message, res.Error)
				}
			}
		})
	}
}

func TestSMTPDriverWithholdsCredentialsInAChallenge(t *testing.T) {
	const user, pass = "monitor@example.test", `pa"ss`
	s := startFakeSMTP(t, fakeSMTPOptions{startTLS: true, mechanisms: "LOGIN", user: user, pass: pass, challengePassword: true})
	check := &v1.SmtpCheck{StartTls: true, EmailSecretRef: v1.SecretRef{Name: "creds"}}
	d, _ := NewSMTPDriver(s.addr, "", check, "default", secretClient(t, map[string]string{"username": user, "password": pass}))
	res, _ := d.Check()
	if res.Success || res.Message != "SMTP check failed: AUTH failed; the error is withheld because it repeats the credentials" {
		t.Fatalf("success=%v %s", res.Success, res.Message)
	}
}

func TestSMTPDriverKeepsTextOfFailuresAfterAuth(t *testing.T) {
	const user, pass = "monitor@example.test", "sender"
	s := startFakeSMTP(t, fakeSMTPOptions{startTLS: true, mechanisms: "PLAIN", user: user, pass: pass, rejectMail: true})
	check := &v1.SmtpCheck{StartTls: true, FromAssertion: "monitor@example.test", EmailSecretRef: v1.SecretRef{Name: "creds"}}
	d, _ := NewSMTPDriver(s.addr, "", check, "default", secretClient(t, map[string]string{"username": user, "password": pass}))
	res, _ := d.Check()
	if res.Success || res.Message != `SMTP check failed: 550 "sender rejected"` {
		t.Fatalf("success=%v %s", res.Success, res.Message)
	}
}

func TestSMTPDriverWithoutSmtpCheck(t *testing.T) {
	s := startFakeSMTP(t, fakeSMTPOptions{})
	d, err := NewSMTPDriver(s.addr, "", nil, "default", nil)
	if err != nil {
		t.Fatal(err)
	}
	if res, _ := d.Check(); !res.Success {
		t.Fatalf("a monitor without smtpCheck failed: %s", res.Message)
	}
}

func TestSMTPDriverMissingSecret(t *testing.T) {
	_, err := NewSMTPDriver("127.0.0.1:25", "", &v1.SmtpCheck{EmailSecretRef: v1.SecretRef{Name: "absent"}}, "default", secretClient(t, nil))
	if err == nil || !apierrors.IsNotFound(err) {
		t.Fatalf("err = %v, want a wrapped NotFound", err)
	}
}
