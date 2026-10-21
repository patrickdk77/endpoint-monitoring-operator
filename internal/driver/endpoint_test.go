package driver

import (
	"strings"
	"testing"
)

func TestHostPort(t *testing.T) {
	tests := []struct {
		endpoint, wantHost, wantPort string
	}{
		{"example.test", "example.test", "6379"},
		{"example.test:6380", "example.test", "6380"},
		{"example.test:", "example.test", "6379"},
		{"192.0.2.1", "192.0.2.1", "6379"},
		{"192.0.2.1:6380", "192.0.2.1", "6380"},
		{"::1", "::1", "6379"},
		{"[::1]", "::1", "6379"},
		{"[::1]:6380", "::1", "6380"},
		{"2001:db8::1", "2001:db8::1", "6379"},
		{"[2001:db8::1]", "2001:db8::1", "6379"},
	}
	for _, tc := range tests {
		host, port := hostPort(tc.endpoint, "6379")
		if host != tc.wantHost || port != tc.wantPort {
			t.Errorf("hostPort(%q) = %q, %q, want %q, %q", tc.endpoint, host, port, tc.wantHost, tc.wantPort)
		}
	}
}

func TestHostDriversRejectURLAndUserEndpoints(t *testing.T) {
	build := map[string]func(string) (Driver, error){
		"tcp":      func(e string) (Driver, error) { return NewTCPDriver(e, "") },
		"ping":     func(e string) (Driver, error) { return NewPingDriver(e, "") },
		"redis":    func(e string) (Driver, error) { return NewREDISDriver(e, "", nil, "default", nil) },
		"smtp":     func(e string) (Driver, error) { return NewSMTPDriver(e, "", nil, "default", nil) },
		"tls":      func(e string) (Driver, error) { return NewTLSDriver(e, "", nil) },
		"mysql":    func(e string) (Driver, error) { return NewMYSQLDriver(e, "", nil, "default", nil) },
		"postgres": func(e string) (Driver, error) { return NewPostgresDriver(e, "", nil, "default", nil) },
		"mongodb":  func(e string) (Driver, error) { return NewMongoDBDriver(e, "", nil, "default", nil) },
		"dns":      func(e string) (Driver, error) { return NewDNSDriver(e, "", nil) },
	}
	for name, newDriver := range build {
		for _, endpoint := range []string{
			"redis://default:pa/ss@127.0.0.1:1/0",
			"default:s3cret@127.0.0.1:1",
			"tcp://127.0.0.1:1",
			"user@example.test",
		} {
			d, err := newDriver(endpoint)
			if d != nil || err != errNotHostPort || strings.Contains(err.Error(), "s3cret") || strings.Contains(err.Error(), "pa/ss") {
				t.Errorf("%s %q: driver=%v err=%v", name, endpoint, d, err)
			}
		}
		for _, endpoint := range []string{"127.0.0.1:1", "example.test", "[::1]:25", "::1"} {
			if _, err := newDriver(endpoint); err != nil {
				t.Errorf("%s %q: %v", name, endpoint, err)
			}
		}
	}
}
