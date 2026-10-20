package driver

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	v1 "github.com/patrickdk77/endpoint-monitoring-operator/api/v1alpha1"
	"github.com/patrickdk77/endpoint-monitoring-operator/internal/version"
)

type HTTPJSONDriver struct {
	endpoint       string
	client         *http.Client
	assertions     map[string]string
	expectedStatus int
}

func NewHTTPJSONDriver(endpoint, family string, check *v1.HttpJsonCheck) (Driver, error) {
	if endpoint == "" || check == nil {
		return nil, fmt.Errorf("invalid endpoint or config for http-json")
	}

	client := httpClient(family, 30*time.Second)
	if check.ExpectedStatusCode >= 300 && check.ExpectedStatusCode < 400 {
		client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	}
	return &HTTPJSONDriver{
		endpoint:       endpoint,
		assertions:     check.JsonAssertions,
		expectedStatus: check.ExpectedStatusCode,
		client:         client,
	}, nil
}

func (h *HTTPJSONDriver) Check() (*CheckResult, error) {
	start := time.Now()

	req, err := http.NewRequest(http.MethodGet, h.endpoint, nil)
	var resp *http.Response
	if err == nil {
		req.Header.Set("User-Agent", version.UserAgent)
		resp, err = h.client.Do(req)
	}

	duration := time.Since(start)
	result := &CheckResult{ResponseTime: duration}
	if err != nil {
		result.Success = false
		result.Error = err
		result.Message = fmt.Sprintf("HTTP-JSON request failed: %s", errorText(err))
		return result, nil
	}
	if resp != nil {
		defer resp.Body.Close()
	}

	if h.expectedStatus != 0 && resp.StatusCode != h.expectedStatus {
		result.Success = false
		result.Message = fmt.Sprintf("HTTP-JSON check failed (status: %d, expected %d)", resp.StatusCode, h.expectedStatus)
		return result, nil
	}
	if h.expectedStatus == 0 && (resp.StatusCode < 200 || resp.StatusCode >= 300) {
		result.Success = false
		result.Message = fmt.Sprintf("HTTP-JSON check failed (status: %d, expected 2xx)", resp.StatusCode)
		return result, nil
	}

	body, err := readBody(resp.Body)
	if errors.Is(err, errBodyTooLarge) {
		result.Success = false
		result.Error = err
		result.Message = "HTTP-JSON check failed: response body exceeds 1 MiB"
		return result, nil
	}
	if err != nil {
		result.Success = false
		result.Error = err
		result.Message = "failed to read HTTP response body"
		return result, nil
	}

	var payload map[string]interface{}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	err = decoder.Decode(&payload)
	if err == nil {
		if _, errEnd := decoder.Token(); errEnd != io.EOF {
			err = errors.New("unexpected data after the JSON value")
		}
	}
	if err != nil {
		result.Success = false
		result.Error = err
		result.Message = "invalid JSON response"
		return result, nil
	}

	paths := make([]string, 0, len(h.assertions))
	for path := range h.assertions {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	for _, path := range paths {
		expected := h.assertions[path]
		actual, ok := resolveJSONPath(payload, path)
		if !ok || !jsonValueMatches(actual, expected) {
			result.Success = false
			result.Message = fmt.Sprintf("assertion failed at '%s': expected '%s', got '%v'", path, expected, actual)
			return result, nil
		}
	}

	result.Success = true
	result.Message = fmt.Sprintf("HTTP-JSON check successful (response time: %v)", duration)
	return result, nil
}

func jsonValueMatches(actual interface{}, expected string) bool {
	if fmt.Sprintf("%v", actual) == expected {
		return true
	}
	number, ok := actual.(json.Number)
	if !ok {
		return false
	}
	a, okA := exactNumber(number.String())
	b, okB := exactNumber(expected)
	return okA && okB && a.Cmp(b) == 0
}

var jsonNumberLiteral = regexp.MustCompile(`^-?(0|[1-9][0-9]*)(\.[0-9]+)?([eE][+-]?[0-9]+)?$`)

func exactNumber(s string) (*big.Rat, bool) {
	if len(s) > 100 || !jsonNumberLiteral.MatchString(s) {
		return nil, false
	}
	if _, exp, found := strings.Cut(strings.ToLower(s), "e"); found {
		if e, err := strconv.Atoi(exp); err != nil || e < -1000 || e > 1000 {
			return nil, false
		}
	}
	return new(big.Rat).SetString(s)
}

func (h *HTTPJSONDriver) GetEndpoint() string {
	return h.endpoint
}

func (h *HTTPJSONDriver) GetType() string {
	return "http-json"
}

// resolveJSONPath traverses a map[string]interface{} based on dot-separated path (e.g., "cluster.status")
func resolveJSONPath(data map[string]interface{}, path string) (interface{}, bool) {
	keys := strings.Split(path, ".")
	var current interface{} = data

	for _, key := range keys {
		m, ok := current.(map[string]interface{})
		if !ok {
			return nil, false
		}
		current, ok = m[key]
		if !ok {
			return nil, false
		}
	}
	return current, true
}
