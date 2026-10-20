package driver

import "time"

// checkTimeout bounds the drivers that have no timeout of their own.
var checkTimeout = 30 * time.Second

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
