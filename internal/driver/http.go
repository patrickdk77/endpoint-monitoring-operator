package driver

import (
	"fmt"
	"net/http"
	"time"

	v1 "github.com/patrickdk77/endpoint-monitoring-operator/api/v1alpha1"
	"github.com/patrickdk77/endpoint-monitoring-operator/internal/version"
)

type HTTPDriver struct {
	endpoint string
	client   *http.Client
	check    *v1.TlsCheck
}

func NewHTTPDriver(endpoint string, check *v1.TlsCheck) (Driver, error) {
	if endpoint == "" {
		return nil, fmt.Errorf("endpoint cannot be empty")
	}

	return &HTTPDriver{
		endpoint: endpoint,
		client: &http.Client{
			Timeout: 30 * time.Second,
		},
		check: check,
	}, nil
}

func (h *HTTPDriver) Check() (*CheckResult, error) {
	start := time.Now()

	req, err := http.NewRequest(http.MethodGet, h.endpoint, nil)

	var resp *http.Response
	if err == nil {
		req.Header.Set("User-Agent", version.UserAgent)
		resp, err = h.client.Do(req)
	}

	duration := time.Since(start)
	result := &CheckResult{
		ResponseTime: duration,
	}

	if err == nil && resp != nil && resp.TLS != nil && h.check != nil && len(resp.TLS.PeerCertificates) > 0 {
		if h.check.DaysToExpire > 0 {
			days := int(time.Until(resp.TLS.PeerCertificates[0].NotAfter).Hours() / 24)
			if days < h.check.DaysToExpire {
				err = fmt.Errorf("certificate expires in %d days, alert at under %d", days, h.check.DaysToExpire)
			}
		}
		if err == nil && h.check.Host != "" {
			err = resp.TLS.PeerCertificates[0].VerifyHostname(h.check.Host)
		}
	}

	if resp != nil {
		defer resp.Body.Close()
	}

	if err != nil {
		result.Success = false
		result.Error = err
		result.Message = fmt.Sprintf("HTTP check failed: %v", err)
		return result, nil
	}

	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		result.Success = true
		result.Message = fmt.Sprintf("HTTP check successful (status: %d, response time: %v)", resp.StatusCode, duration)
	} else {
		result.Success = false
		result.Message = fmt.Sprintf("HTTP check failed (status: %d, response time: %v)", resp.StatusCode, duration)
	}

	return result, nil
}

func (h *HTTPDriver) GetEndpoint() string {
	return h.endpoint
}

func (h *HTTPDriver) GetType() string {
	return "http"
}
