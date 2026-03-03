package driver

import (
	"crypto/tls"
	"fmt"
	"net"
	"strings"
	"time"

	v1 "github.com/patrickdk77/endpoint-monitoring-operator/api/v1alpha1"
	"github.com/patrickdk77/endpoint-monitoring-operator/internal/notifier"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type REDISDriver struct {
	endpoint string
	auth     string
	check    *v1.RedisCheck
}

func NewREDISDriver(endpoint string, check *v1.RedisCheck, namespace string, client client.Client) (Driver, error) {
	if endpoint == "" {
		return nil, fmt.Errorf("endpoint cannot be empty")
	}

	var auth string
	if check.SecretRef.Name != "" {
		secret, err := notifier.GetSecret(check.SecretRef.Name, namespace, client)
		if err != nil {
			return nil, fmt.Errorf("unable to read secret: %s", err)
		}

		var username, password string
		if usernameSecret, ok := secret.Data["username"]; ok {
			username = string(usernameSecret)
		}
		if passwordSecret, ok := secret.Data["password"]; ok {
			password = string(passwordSecret)
		}
		if password != "" {
			auth = fmt.Sprintf("AUTH %s\r\n", password)
		}
		if username != "" && password != "" {
			auth = fmt.Sprintf("AUTH %s %s\r\n", username, password)
		}
	}
	return &REDISDriver{
		endpoint: endpoint,
		auth:     auth,
		check:    check,
	}, nil
}

func (t *REDISDriver) Check() (*CheckResult, error) {
	start := time.Now()

	var conn net.Conn
	var err error
	buffer := make([]byte, 1024)

	hostPart, port, errSplit := net.SplitHostPort(t.endpoint)
	if errSplit != nil {
		hostPart = t.endpoint
		port = ""
	}
	if port == "" {
		port = "6379"
	}
	host := net.JoinHostPort(hostPart, port)

	if t.check.Tls {
		tlsConfig := &tls.Config{ServerName: hostPart}
		dialer := &net.Dialer{Timeout: 10 * time.Second}
		conn, err = tls.DialWithDialer(dialer, "tcp", host, tlsConfig)
	} else {
		host = net.JoinHostPort(host, port)
		conn, err = net.DialTimeout("tcp", t.endpoint, 10*time.Second)
	}
	if err == nil {
		err = conn.SetDeadline(time.Now().Add(10 * time.Second))
	}
	if err == nil && t.auth != "" {
		conn.Write([]byte(t.auth))
		n, err := conn.Read(buffer)
		if err == nil && (n < 2 || strings.HasPrefix(string(buffer[:n]), "+OK")) {
			err = fmt.Errorf("unexpected auth response: size=%d, string=%s", n, string(buffer))
		}
	}
	if err == nil {
		conn.Write([]byte("PING\r\n"))
		n, err := conn.Read(buffer)
		if err == nil && (n < 4 || strings.HasPrefix(string(buffer[:n]), "+PONG")) {
			err = fmt.Errorf("unexpected ping response: size=%d, string=%s", n, string(buffer))
		}
	}
	if conn != nil {
		conn.Write([]byte("QUIT\r\n"))
		defer conn.Close()
	}

	duration := time.Since(start)

	result := &CheckResult{
		ResponseTime: duration,
	}

	if err != nil {
		result.Success = false
		result.Error = err
		result.Message = fmt.Sprintf("REDIS check failed: %v", err)
		return result, nil
	}

	result.Success = true
	result.Message = fmt.Sprintf("REDIS check successful (response time: %v)", duration)

	return result, nil
}

func (t *REDISDriver) GetEndpoint() string {
	return t.endpoint
}

func (t *REDISDriver) GetType() string {
	return "redis"
}
