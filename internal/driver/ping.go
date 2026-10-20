package driver

import (
	"fmt"
	"net"
	"time"
)

type PingDriver struct {
	endpoint string
	family   string
}

func NewPingDriver(endpoint, family string) (Driver, error) {
	if endpoint == "" {
		return nil, fmt.Errorf("endpoint cannot be empty")
	}

	return &PingDriver{endpoint: endpoint, family: family}, nil
}

func (p *PingDriver) Check() (*CheckResult, error) {
	start := time.Now()

	// Simple connectivity check
	host, port := hostPort(p.endpoint, "80")
	conn, err := net.DialTimeout(familyNetwork("tcp", p.family), net.JoinHostPort(host, port), 5*time.Second)
	duration := time.Since(start)

	result := &CheckResult{
		ResponseTime: duration,
	}

	if err != nil {
		result.Success = false
		result.Error = err
		result.Message = fmt.Sprintf("Ping check failed: %v", err)
		return result, nil
	}

	defer conn.Close()

	result.Success = true
	result.Message = fmt.Sprintf("Ping check successful (response time: %v)", duration)

	return result, nil
}

func (p *PingDriver) GetEndpoint() string {
	return p.endpoint
}

func (p *PingDriver) GetType() string {
	return "ping"
}
