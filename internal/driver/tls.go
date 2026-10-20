package driver

import (
	"crypto/tls"
	"fmt"
	"net"
	"time"

	v1 "github.com/patrickdk77/endpoint-monitoring-operator/api/v1alpha1"
	"github.com/patrickdk77/endpoint-monitoring-operator/internal/common"
)

type TLSDriver struct {
	endpoint string
	family   string
	check    *v1.TlsCheck
}

func NewTLSDriver(endpoint, family string, check *v1.TlsCheck) (Driver, error) {
	if endpoint == "" {
		return nil, fmt.Errorf("endpoint cannot be empty")
	}
	if check == nil {
		check = &v1.TlsCheck{}
	}

	return &TLSDriver{endpoint: endpoint, family: family, check: check}, nil
}

func (t *TLSDriver) target() (addr, serverName, port string) {
	host, port := hostPort(t.endpoint, "443")
	serverName = host
	if t.check.Host != "" {
		serverName = t.check.Host
	}
	return net.JoinHostPort(host, port), serverName, port
}

func (t *TLSDriver) Check() (*CheckResult, error) {
	start := time.Now()

	addr, serverName, port := t.target()
	dialer := &net.Dialer{Timeout: 10 * time.Second}
	conn, err := tls.DialWithDialer(dialer, familyNetwork("tcp", t.family), addr, &tls.Config{ServerName: serverName})
	if err == nil && t.check.DaysToExpire > 0 {
		notAfter := conn.ConnectionState().PeerCertificates[0].NotAfter
		if days := int(time.Until(notAfter).Hours() / 24); days < t.check.DaysToExpire {
			err = fmt.Errorf("certificate expires at %s, within the %d-day alert window", notAfter.UTC().Format(time.RFC3339), t.check.DaysToExpire)
		}
	}
	if err == nil {
		err = common.ResolveAndVerifyDANE(serverName, port, conn.ConnectionState().PeerCertificates)
	}
	if conn != nil {
		defer conn.Close()
	}

	duration := time.Since(start)

	result := &CheckResult{
		ResponseTime: duration,
	}

	if err != nil {
		result.Success = false
		result.Error = err
		result.Message = fmt.Sprintf("TLS check failed: %s", errorText(err))
		return result, nil
	}

	result.Success = true
	result.Message = fmt.Sprintf("TLS check successful (response time: %v)", duration)

	return result, nil
}

func (t *TLSDriver) GetEndpoint() string {
	return t.endpoint
}

func (t *TLSDriver) GetType() string {
	return "tls"
}
