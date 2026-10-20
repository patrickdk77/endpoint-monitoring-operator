package driver

import (
	"fmt"
	"net"
	"time"
)

type TCPDriver struct {
	endpoint string
	family   string
}

func NewTCPDriver(endpoint, family string) (Driver, error) {
	if endpoint == "" {
		return nil, fmt.Errorf("endpoint cannot be empty")
	}
	if err := checkHostEndpoint(endpoint); err != nil {
		return nil, err
	}

	return &TCPDriver{endpoint: endpoint, family: family}, nil
}

func (t *TCPDriver) Check() (*CheckResult, error) {
	start := time.Now()

	conn, err := net.DialTimeout(familyNetwork("tcp", t.family), t.endpoint, 10*time.Second)
	duration := time.Since(start)

	result := &CheckResult{
		ResponseTime: duration,
	}

	if err != nil {
		result.Success = false
		result.Error = err
		result.Message = fmt.Sprintf("TCP check failed: %s", errorText(err))
		return result, nil
	}

	defer conn.Close()

	result.Success = true
	result.Message = fmt.Sprintf("TCP check successful (response time: %v)", duration)

	return result, nil
}

func (t *TCPDriver) GetEndpoint() string {
	return t.endpoint
}

func (t *TCPDriver) GetType() string {
	return "tcp"
}
