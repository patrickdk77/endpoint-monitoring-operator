package driver

import (
	"errors"
	"io"
	"time"
)

// checkTimeout bounds the drivers that have no timeout of their own.
var checkTimeout = 30 * time.Second

// maxBody caps how much of a response body a check reads.
const maxBody = 1 << 20

var errBodyTooLarge = errors.New("response body exceeds 1 MiB")

func readBody(r io.Reader) ([]byte, error) {
	body, err := io.ReadAll(io.LimitReader(r, maxBody+1))
	if err == nil && len(body) > maxBody {
		return body[:maxBody], errBodyTooLarge
	}
	return body, err
}

// CheckResult represents the result of a health check
type CheckResult struct {
	Success      bool
	ResponseTime time.Duration
	Error        error
	Message      string
}

// Driver interface for different monitoring types
type Driver interface {
	Check() (*CheckResult, error)
	GetEndpoint() string
	GetType() string
}
