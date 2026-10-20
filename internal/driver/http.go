package driver

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	v1 "github.com/patrickdk77/endpoint-monitoring-operator/api/v1alpha1"
	"github.com/patrickdk77/endpoint-monitoring-operator/internal/version"
)

type HTTPDriver struct {
	endpoint  string
	client    *http.Client
	check     *v1.TlsCheck
	httpCheck *v1.HttpCheck
}

func NewHTTPDriver(endpoint, family string, check *v1.TlsCheck, httpCheck *v1.HttpCheck) (Driver, error) {
	if endpoint == "" {
		return nil, fmt.Errorf("endpoint cannot be empty")
	}

	return &HTTPDriver{
		endpoint:  endpoint,
		client:    httpClient(family, 30*time.Second),
		check:     check,
		httpCheck: httpCheck,
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
			notAfter := resp.TLS.PeerCertificates[0].NotAfter
			if days := int(time.Until(notAfter).Hours() / 24); days < h.check.DaysToExpire {
				err = fmt.Errorf("certificate expires at %s, within the %d-day alert window", notAfter.UTC().Format(time.RFC3339), h.check.DaysToExpire)
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
		result.Message = fmt.Sprintf("HTTP check failed: %s", errorText(err))
		return result, nil
	}

	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		if h.httpCheck != nil && h.httpCheck.BodyContains != "" {
			bodyBytes, err := readBody(resp.Body)
			if err != nil && !errors.Is(err, errBodyTooLarge) {
				result.Success = false
				result.Error = err
				result.Message = fmt.Sprintf("HTTP check failed to read body: %s", errorText(err))
				return result, nil
			}
			if !strings.Contains(string(bodyBytes), h.httpCheck.BodyContains) {
				where := "body"
				if err != nil {
					where = "the first 1 MiB of the body"
				}
				result.Success = false
				result.Message = fmt.Sprintf("HTTP check failed: %s does not contain '%s' (status: %d)", where, h.httpCheck.BodyContains, resp.StatusCode)
				return result, nil
			}
		}

		result.Success = true
		result.Message = fmt.Sprintf("HTTP check successful (status: %d, response time: %v)", resp.StatusCode, duration)
	} else {
		result.Success = false
		result.Message = fmt.Sprintf("HTTP check failed (status: %d)", resp.StatusCode)
	}

	return result, nil
}

func (h *HTTPDriver) GetEndpoint() string {
	return h.endpoint
}

func (h *HTTPDriver) GetType() string {
	return "http"
}
