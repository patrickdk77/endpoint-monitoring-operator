package driver

import (
	"bufio"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
	"time"

	v1 "github.com/patrickdk77/endpoint-monitoring-operator/api/v1alpha1"
	"github.com/patrickdk77/endpoint-monitoring-operator/internal/notifier"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type REDISDriver struct {
	endpoint string
	family   string
	auth     []string
	check    *v1.RedisCheck
}

func NewREDISDriver(endpoint, family string, check *v1.RedisCheck, namespace string, client client.Client) (Driver, error) {
	if endpoint == "" {
		return nil, fmt.Errorf("endpoint cannot be empty")
	}
	if check == nil {
		check = &v1.RedisCheck{}
	}

	var auth []string
	if check.SecretRef.Name != "" {
		secret, err := notifier.GetSecret(check.SecretRef.Name, namespace, client)
		if err != nil {
			return nil, fmt.Errorf("unable to read secret: %w", err)
		}

		var username, password string
		if usernameSecret, ok := secret.Data["username"]; ok {
			username = string(usernameSecret)
		}
		if passwordSecret, ok := secret.Data["password"]; ok {
			password = string(passwordSecret)
		}
		if password != "" {
			auth = []string{"AUTH", password}
		}
		if username != "" && password != "" {
			auth = []string{"AUTH", username, password}
		}
	}
	return &REDISDriver{
		endpoint: endpoint,
		family:   family,
		auth:     auth,
		check:    check,
	}, nil
}

func (t *REDISDriver) Check() (*CheckResult, error) {
	start := time.Now()

	var conn net.Conn
	var err error

	hostPart, port := hostPort(t.endpoint, "6379")
	host := net.JoinHostPort(hostPart, port)

	if t.check.Tls {
		tlsConfig := &tls.Config{ServerName: hostPart}
		dialer := &net.Dialer{Timeout: 10 * time.Second}
		var tlsConn *tls.Conn
		tlsConn, err = tls.DialWithDialer(dialer, familyNetwork("tcp", t.family), host, tlsConfig)
		if err == nil {
			conn = tlsConn
		}
	} else {
		conn, err = net.DialTimeout(familyNetwork("tcp", t.family), host, 10*time.Second)
	}
	if conn != nil {
		defer conn.Close()
	}
	if err == nil {
		err = conn.SetDeadline(time.Now().Add(10 * time.Second))
	}
	var r *bufio.Reader
	if err == nil {
		r = bufio.NewReaderSize(conn, 64<<10)
	}
	if err == nil && t.auth != nil {
		err = redisCommand(conn, r, "+OK", t.auth...)
	}
	if err == nil {
		err = redisCommand(conn, r, "+PONG", "PING")
	}
	if err == nil {
		_, _ = conn.Write(redisEncode([]string{"QUIT"}))
	}

	duration := time.Since(start)

	result := &CheckResult{
		ResponseTime: duration,
	}

	if err != nil {
		result.Success = false
		result.Error = err
		result.Message = fmt.Sprintf("REDIS check failed: %s", errorText(err))
		return result, nil
	}

	result.Success = true
	result.Message = fmt.Sprintf("REDIS check successful (response time: %v)", duration)

	return result, nil
}

func redisCommand(conn net.Conn, r *bufio.Reader, want string, args ...string) error {
	if _, err := conn.Write(redisEncode(args)); err != nil {
		return fmt.Errorf("%s: %w", args[0], err)
	}
	line, err := r.ReadSlice('\n')
	if errors.Is(err, bufio.ErrBufferFull) {
		return fmt.Errorf("%s: reply longer than 64 KiB", args[0])
	}
	if err != nil {
		return fmt.Errorf("%s: %w", args[0], err)
	}
	reply := strings.TrimRight(string(line), "\r\n")
	if reply == want {
		return nil
	}
	if args[0] == "AUTH" {
		reply, _, _ = strings.Cut(reply, " ")
	}
	if len(reply) > 128 {
		reply = reply[:128] + "..."
	}
	return fmt.Errorf("%s: unexpected reply %q", args[0], reply)
}

func redisEncode(args []string) []byte {
	var b strings.Builder
	b.WriteString("*" + strconv.Itoa(len(args)) + "\r\n")
	for _, arg := range args {
		b.WriteString("$" + strconv.Itoa(len(arg)) + "\r\n" + arg + "\r\n")
	}
	return []byte(b.String())
}

func (t *REDISDriver) GetEndpoint() string {
	return t.endpoint
}

func (t *REDISDriver) GetType() string {
	return "redis"
}
