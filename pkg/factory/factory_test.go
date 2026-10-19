package factory

import (
	"errors"
	"net"
	"os"
	"reflect"
	"strings"
	"testing"

	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"

	"github.com/patrickdk77/endpoint-monitoring-operator/api/v1alpha1"
	"github.com/patrickdk77/endpoint-monitoring-operator/internal/notifier"
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

var testEndpoints = map[string]string{
	"http": "http://127.0.0.1:1/", "http-json": "http://127.0.0.1:1/",
	"opensearch": "http://127.0.0.1:1", "trino": "http://127.0.0.1:1",
	"smtp": "127.0.0.1:1", "redis": "127.0.0.1:1", "tls": "127.0.0.1:1",
	"tcp": "127.0.0.1:1", "dns": "example.test", "ping": "127.0.0.1",
	"mysql": "127.0.0.1:1",
}

func TestCreateDriverEveryTypeEveryFamily(t *testing.T) {
	for driverType, endpoint := range testEndpoints {
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
	refused := func(driverType string) string {
		if driverType == "dns" {
			return "no ipv6 address"
		}
		return "no suitable address"
	}
	for driverType, endpoint := range testEndpoints {
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

type fakeNotifier struct {
	err   error
	calls int
}

func (f *fakeNotifier) SendAlert(status string, values *notifier.NoticeValues, c client.Client) error {
	f.calls++
	return f.err
}

func TestCompositeNotifierReportsFailedNotifiers(t *testing.T) {
	errSlack := errors.New("slack down")
	errWebhook := errors.New("webhook down")
	tests := []struct {
		name      string
		errs      []error
		wantNames []string
		wantErrs  []error
	}{
		{"all delivered", []error{nil, nil, nil}, nil, nil},
		{"one failed", []error{errSlack, nil, nil}, []string{"slack"}, []error{errSlack}},
		{"first and last failed", []error{errSlack, nil, errWebhook}, []string{"slack", "webhook"}, []error{errSlack, errWebhook}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			fakes := []*fakeNotifier{{err: tc.errs[0]}, {err: tc.errs[1]}, {err: tc.errs[2]}}
			c := &CompositeNotifier{notifiers: []namedNotifier{
				{"slack", fakes[0]}, {"discord", fakes[1]}, {"webhook", fakes[2]},
			}}
			err := c.SendAlert("failure", &notifier.NoticeValues{}, nil)
			for i, f := range fakes {
				if f.calls != 1 {
					t.Fatalf("notifier %d called %d times; every notifier must be tried", i, f.calls)
				}
			}
			if tc.wantNames == nil {
				if err != nil {
					t.Fatalf("err = %v, want nil", err)
				}
				return
			}
			var sendErr *SendError
			if !errors.As(err, &sendErr) {
				t.Fatalf("err %T is not a *SendError", err)
			}
			if !reflect.DeepEqual(sendErr.Notifiers, tc.wantNames) {
				t.Fatalf("notifiers %v, want %v", sendErr.Notifiers, tc.wantNames)
			}
			for _, want := range tc.wantErrs {
				if !errors.Is(err, want) {
					t.Fatalf("%v does not wrap %v", err, want)
				}
			}
			if !strings.HasPrefix(err.Error(), "failed to send alerts: [") {
				t.Fatalf("error text changed: %q", err.Error())
			}
		})
	}
}

func TestCRDDriverEnumMatchesFactory(t *testing.T) {
	data, err := os.ReadFile("../../config/crd/bases/monitoring.licious.app_endpointmonitors.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var crd struct {
		Spec struct {
			Versions []struct {
				Schema struct {
					OpenAPIV3Schema struct {
						Properties struct {
							Spec struct {
								Properties struct {
									Driver struct {
										Enum []string `json:"enum"`
									} `json:"driver"`
								} `json:"properties"`
							} `json:"spec"`
						} `json:"properties"`
					} `json:"openAPIV3Schema"`
				} `json:"schema"`
			} `json:"versions"`
		} `json:"spec"`
	}
	if err := yaml.Unmarshal(data, &crd); err != nil {
		t.Fatal(err)
	}
	enum := crd.Spec.Versions[0].Schema.OpenAPIV3Schema.Properties.Spec.Properties.Driver.Enum
	if len(enum) == 0 {
		t.Fatal("no driver enum in the CRD")
	}

	tested := map[string]bool{}
	for driverType := range testEndpoints {
		tested[driverType] = true
	}
	for _, driverType := range enum {
		if !tested[driverType] {
			t.Errorf("driver %q is in the CRD enum but not in testEndpoints", driverType)
			continue
		}
		delete(tested, driverType)
		m := monitor(driverType, testEndpoints[driverType], "")
		if _, err := NewDriver(driverType, testEndpoints[driverType], m, "default", nil); err != nil {
			t.Errorf("driver %q is in the CRD enum but the factory rejects it: %v", driverType, err)
		}
	}
	for driverType := range tested {
		t.Errorf("driver %q is not in the CRD enum, so the API server rejects it", driverType)
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
