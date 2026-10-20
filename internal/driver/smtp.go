package driver

import (
	"crypto/tls"
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"net/smtp"
	"net/textproto"
	"strconv"
	"strings"
	"time"

	v1 "github.com/patrickdk77/endpoint-monitoring-operator/api/v1alpha1"
	"github.com/patrickdk77/endpoint-monitoring-operator/internal/common"
	"github.com/patrickdk77/endpoint-monitoring-operator/internal/common/smtpAuth"
	"github.com/patrickdk77/endpoint-monitoring-operator/internal/notifier"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type SMTPDriver struct {
	endpoint string
	family   string
	auth     smtp.Auth
	secrets  []string
	check    *v1.SmtpCheck
}

func NewSMTPDriver(endpoint, family string, check *v1.SmtpCheck, namespace string, client client.Client) (Driver, error) {
	if endpoint == "" {
		return nil, fmt.Errorf("endpoint cannot be empty")
	}
	if err := checkHostEndpoint(endpoint); err != nil {
		return nil, err
	}
	if check == nil {
		check = &v1.SmtpCheck{}
	}

	var auth smtp.Auth = nil
	var secrets []string
	if check.EmailSecretRef.Name != "" {
		secret, err := notifier.GetSecret(check.EmailSecretRef.Name, namespace, client)
		if err != nil {
			return nil, fmt.Errorf("unable to read secret: %w", err)
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
		hostPart, _ := hostPort(endpoint, "25")
		if username != "" && password != "" {
			auth = smtpAuth.CommonAuth(identity, username, password, hostPart)
			quoted := strconv.Quote(password)
			secrets = []string{
				base64.StdEncoding.EncodeToString([]byte(identity + "\x00" + username + "\x00" + password)),
				base64.StdEncoding.EncodeToString([]byte(password)),
				password,
				quoted[1 : len(quoted)-1],
			}
		}
	}
	return &SMTPDriver{
		endpoint: endpoint,
		family:   family,
		auth:     auth,
		secrets:  secrets,
		check:    check,
	}, nil
}

func (t *SMTPDriver) Check() (*CheckResult, error) {
	start := time.Now()

	var conn net.Conn
	var sconn *smtp.Client
	var err error
	hostPart, port := hostPort(t.endpoint, "25")
	host := net.JoinHostPort(hostPart, port)

	if t.check.Tls {
		tlsConfig := &tls.Config{ServerName: hostPart}
		dialer := &net.Dialer{Timeout: 10 * time.Second}
		var tlsConn *tls.Conn
		tlsConn, err = tls.DialWithDialer(dialer, familyNetwork("tcp", t.family), host, tlsConfig)
		if err == nil {
			conn = tlsConn
			err = common.ResolveAndVerifyDANE(hostPart, port, tlsConn.ConnectionState().PeerCertificates)
		}
	} else {
		conn, err = net.DialTimeout(familyNetwork("tcp", t.family), host, 10*time.Second)
	}

	if err == nil {
		err = conn.SetDeadline(time.Now().Add(checkTimeout))
	}
	if err == nil {
		sconn, err = smtp.NewClient(conn, hostPart)
	}
	if err == nil && t.check.Helo != "" {
		err = sconn.Hello(t.check.Helo)
	}
	if err == nil && t.check.StartTls && !t.check.Tls {
		tlsConfig := &tls.Config{ServerName: hostPart}
		err = sconn.StartTLS(tlsConfig)
		if err == nil {
			if tlsConn, ok := sconn.TLSConnectionState(); ok {
				err = common.ResolveAndVerifyDANE(hostPart, port, tlsConn.PeerCertificates)
			}
		}
	}
	if err == nil && t.auth != nil {
		if err = sconn.Auth(t.auth); err != nil {
			err = t.withholdCredentials(err)
		}
	}
	if err == nil && t.check.VerifyAssertion != "" {
		err = sconn.Verify(t.check.VerifyAssertion)
	}
	if err == nil && t.check.FromAssertion != "" {
		err = sconn.Mail(t.check.FromAssertion)
	}
	if err == nil && t.check.ToAssertion != "" {
		err = sconn.Rcpt(t.check.ToAssertion)
	}
	if conn != nil {
		defer conn.Close()
	}
	if sconn != nil {
		defer func() {
			_ = sconn.Text.PrintfLine("QUIT")
			_ = sconn.Close()
		}()
	}

	duration := time.Since(start)

	result := &CheckResult{
		ResponseTime: duration,
	}

	if err != nil {
		result.Success = false
		result.Error = err
		result.Message = fmt.Sprintf("SMTP check failed: %s", errorText(err))
		return result, nil
	}

	result.Success = true
	result.Message = fmt.Sprintf("SMTP check successful (response time: %v)", duration)

	return result, nil
}

func (t *SMTPDriver) withholdCredentials(err error) error {
	text := err.Error()
	for _, secret := range t.secrets {
		if !strings.Contains(text, secret) {
			continue
		}
		var reply *textproto.Error
		if errors.As(err, &reply) {
			return fmt.Errorf("AUTH rejected with %d; the reply is withheld because it repeats the credentials", reply.Code)
		}
		return errors.New("AUTH failed; the error is withheld because it repeats the credentials")
	}
	return err
}

func (t *SMTPDriver) GetEndpoint() string {
	return t.endpoint
}

func (t *SMTPDriver) GetType() string {
	return "smtp"
}
