package driver

import (
	"crypto/tls"
	"fmt"
	"net"
	"time"

	v1 "github.com/patrickdk77/endpoint-monitoring-operator/api/v1alpha1"
)

type TLSDriver struct {
	endpoint string
	check    *v1.TlsCheck
}

func NewTLSDriver(endpoint string, check *v1.TlsCheck) (Driver, error) {
	if endpoint == "" {
		return nil, fmt.Errorf("endpoint cannot be empty")
	}

	return &TLSDriver{endpoint: endpoint, check: check}, nil
}

func (t *TLSDriver) Check() (*CheckResult, error) {
	start := time.Now()

	dialer := &net.Dialer{Timeout: 10 * time.Second}
	conn, err := tls.DialWithDialer(dialer, "tcp", t.endpoint, nil)
	if err == nil {
		hostPart, _, _ := net.SplitHostPort(t.endpoint)
		if t.check.Host != "" {
			hostPart = t.check.Host
		}
		err = conn.VerifyHostname(hostPart)
	}
	if err == nil && t.check.DaysToExpire > 0 {
		days := int(conn.ConnectionState().PeerCertificates[0].NotAfter.Sub(time.Now()).Hours() / 24)
		if days < t.check.DaysToExpire {
			err = fmt.Errorf("cerificate expires in %d days, alert at under %d", days, t.check.DaysToExpire)
		}
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
		result.Message = fmt.Sprintf("TLS check failed: %v", err)
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
