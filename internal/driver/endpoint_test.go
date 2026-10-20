package driver

import "testing"

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
