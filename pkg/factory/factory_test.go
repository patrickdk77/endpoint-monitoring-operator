package factory

import (
	"net"
	"strings"
	"testing"

	"github.com/patrickdk77/endpoint-monitoring-operator/api/v1alpha1"
)

func tcpListener(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			c.Close()
		}
	}()
	return ln.Addr().String()
}

func monitor(driverType, endpoint, family string) *v1alpha1.EndpointMonitor {
	return &v1alpha1.EndpointMonitor{Spec: v1alpha1.EndpointMonitorSpec{
		Driver:        driverType,
		Endpoint:      endpoint,
		IpFamily:      family,
		TlsCheck:      &v1alpha1.TlsCheck{},
		HttpJsonCheck: &v1alpha1.HttpJsonCheck{},
		SmtpCheck:     &v1alpha1.SmtpCheck{},
		RedisCheck:    &v1alpha1.RedisCheck{},
		DnsCheck:      &v1alpha1.DnsCheck{},
	}}
}

func TestCreateDriverPassesFamily(t *testing.T) {
	endpoint := tcpListener(t)
	tests := []struct {
		family  string
		success bool
		inMsg   string
	}{
		{"", true, "TCP check successful"},
		{"ipv4", true, "TCP check successful"},
		{"ipv6", false, "no suitable address"},
	}
	for _, tc := range tests {
		t.Run(tc.family, func(t *testing.T) {
			m := monitor("tcp", endpoint, tc.family)
			d, err := NewDriver(m.Spec.Driver, m.Spec.Endpoint, m, "default", nil)
			if err != nil {
				t.Fatal(err)
			}
			res, err := d.Check()
			if err != nil {
				t.Fatal(err)
			}
			if res.Success != tc.success || !strings.Contains(res.Message, tc.inMsg) {
				t.Fatalf("success=%v %s", res.Success, res.Message)
			}
		})
	}
}

func TestCreateDriverAllChecksBothFamilies(t *testing.T) {
	m := monitor("tcp", tcpListener(t), "all")
	d, err := NewDriver(m.Spec.Driver, m.Spec.Endpoint, m, "default", nil)
	if err != nil {
		t.Fatal(err)
	}
	res, err := d.Check()
	if err != nil {
		t.Fatal(err)
	}
	if res.Success || !strings.HasPrefix(res.Message, "ipv4: TCP check successful") || !strings.Contains(res.Message, "; ipv6: TCP check failed") {
		t.Fatalf("success=%v %s", res.Success, res.Message)
	}
	if d.GetType() != "tcp" || d.GetEndpoint() != m.Spec.Endpoint {
		t.Fatalf("type=%s endpoint=%s", d.GetType(), d.GetEndpoint())
	}
}

func TestCreateDriverEveryTypeEveryFamily(t *testing.T) {
	endpoints := map[string]string{
		"http": "http://127.0.0.1:1/", "http-json": "http://127.0.0.1:1/",
		"opensearch": "http://127.0.0.1:1", "trino": "http://127.0.0.1:1",
		"smtp": "127.0.0.1:1", "redis": "127.0.0.1:1", "tls": "127.0.0.1:1",
		"tcp": "127.0.0.1:1", "dns": "example.test", "ping": "127.0.0.1",
		"mysql": "127.0.0.1:1",
	}
	for driverType, endpoint := range endpoints {
		for _, family := range []string{"", "ipv4", "ipv6", "all"} {
			m := monitor(driverType, endpoint, family)
			d, err := NewDriver(driverType, endpoint, m, "default", nil)
			if err != nil {
				t.Fatalf("%s/%q: %v", driverType, family, err)
			}
			if d.GetType() != driverType {
				t.Fatalf("%s/%q: type %s", driverType, family, d.GetType())
			}
		}
	}
}

func TestCreateDriverWiresFamilyForEveryType(t *testing.T) {
	endpoints := map[string]string{
		"http": "http://127.0.0.1:1/", "http-json": "http://127.0.0.1:1/",
		"opensearch": "http://127.0.0.1:1", "trino": "http://127.0.0.1:1",
		"smtp": "127.0.0.1:1", "redis": "127.0.0.1:1", "tls": "127.0.0.1:1",
		"tcp": "127.0.0.1:1", "dns": "example.test", "ping": "127.0.0.1",
		"mysql": "127.0.0.1:1",
	}
	refused := func(driverType string) string {
		if driverType == "dns" {
			return "no ipv6 address"
		}
		return "no suitable address"
	}
	for driverType, endpoint := range endpoints {
		t.Run(driverType, func(t *testing.T) {
			m := monitor(driverType, endpoint, "ipv6")
			m.Spec.DnsCheck.Server = "127.0.0.1:1"
			m.Spec.DnsCheck.Type = "A"
			d, err := NewDriver(driverType, endpoint, m, "default", nil)
			if err != nil {
				t.Fatal(err)
			}
			res, _ := d.Check()
			if res.Success || !strings.Contains(res.Message, refused(driverType)) {
				t.Fatalf("ipv6 reached an IPv4 literal: success=%v %s", res.Success, res.Message)
			}

			m.Spec.IpFamily = "all"
			d, err = NewDriver(driverType, endpoint, m, "default", nil)
			if err != nil {
				t.Fatal(err)
			}
			res, _ = d.Check()
			v4, v6, found := strings.Cut(res.Message, "; ipv6: ")
			if res.Success || !found || !strings.HasPrefix(v4, "ipv4: ") || strings.Contains(v4, refused(driverType)) || !strings.Contains(v6, refused(driverType)) {
				t.Fatalf("all did not check over ipv4 and then ipv6: success=%v %s", res.Success, res.Message)
			}
		})
	}
}

func TestCreateDriverRejectsBadInput(t *testing.T) {
	tests := []struct {
		name, driverType, family, wantErr string
	}{
		{"unknown family", "tcp", "ipv5", "unsupported ipFamily: ipv5"},
		{"family is case sensitive", "tcp", "IPv4", "unsupported ipFamily: IPv4"},
		{"unknown driver", "bogus", "", "unsupported driver type: bogus"},
		{"unknown driver with all", "bogus", "all", "unsupported driver type: bogus"},
		{"empty endpoint with all", "tcp", "all", "endpoint cannot be empty"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			endpoint := "127.0.0.1:1"
			if tc.name == "empty endpoint with all" {
				endpoint = ""
			}
			m := monitor(tc.driverType, endpoint, tc.family)
			_, err := NewDriver(tc.driverType, endpoint, m, "default", nil)
			if err == nil || err.Error() != tc.wantErr {
				t.Fatalf("err=%v, want %q", err, tc.wantErr)
			}
		})
	}
}
