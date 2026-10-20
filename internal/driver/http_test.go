package driver

import (
	"crypto/tls"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	v1 "github.com/patrickdk77/endpoint-monitoring-operator/api/v1alpha1"
)

func jsonServer(t *testing.T, status int, body string) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(status)
		fmt.Fprint(w, body)
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

func paddedJSON(size int, fields string) string {
	head, tail := `{"pad":"`, `",`+fields+`}`
	return head + strings.Repeat("x", size-len(head)-len(tail)) + tail
}

func TestHTTPJSONDriver(t *testing.T) {
	big := paddedJSON(maxBody+1, `"status":"UP"`)
	atLimit := paddedJSON(maxBody, `"status":"UP"`)
	tests := []struct {
		name       string
		status     int
		body       string
		expect     int
		assertions map[string]string
		wantOK     bool
		wantMsg    string
	}{
		{"match", 200, `{"status":"UP"}`, 0, map[string]string{"status": "UP"}, true, "HTTP-JSON check successful"},
		{"any 2xx", 201, `{"status":"UP"}`, 0, map[string]string{"status": "UP"}, true, "HTTP-JSON check successful"},
		{"2xx without a body", 204, ``, 0, map[string]string{"status": "UP"}, false, "invalid JSON response"},
		{"server error with matching body", 500, `{"status":"UP"}`, 0, map[string]string{"status": "UP"}, false, "HTTP-JSON check failed (status: 500, expected 2xx)"},
		{"redirect status", 304, `{"status":"UP"}`, 0, map[string]string{"status": "UP"}, false, "HTTP-JSON check failed (status: 304, expected 2xx)"},
		{"expected status", 503, `{"status":"MAINTENANCE"}`, 503, map[string]string{"status": "MAINTENANCE"}, true, "HTTP-JSON check successful"},
		{"other status than expected", 200, `{"status":"UP"}`, 503, map[string]string{"status": "UP"}, false, "HTTP-JSON check failed (status: 200, expected 503)"},
		{"large integer", 200, `{"count":1000000}`, 0, map[string]string{"count": "1000000"}, true, "HTTP-JSON check successful"},
		{"integer beyond float64", 200, `{"id":12345678901234567890}`, 0, map[string]string{"id": "12345678901234567890"}, true, "HTTP-JSON check successful"},
		{"decimal", 200, `{"ratio":0.25}`, 0, map[string]string{"ratio": "0.25"}, true, "HTTP-JSON check successful"},
		{"boolean", 200, `{"ready":true}`, 0, map[string]string{"ready": "true"}, true, "HTTP-JSON check successful"},
		{"nested path", 200, `{"cluster":{"status":"green"}}`, 0, map[string]string{"cluster.status": "green"}, true, "HTTP-JSON check successful"},
		{"wrong value", 200, `{"count":7}`, 0, map[string]string{"count": "8"}, false, "assertion failed at 'count': expected '8', got '7'"},
		{"missing path", 200, `{"cluster":{}}`, 0, map[string]string{"cluster.status": "green"}, false, "assertion failed at 'cluster.status': expected 'green', got '<nil>'"},
		{"path through a scalar", 200, `{"cluster":"down"}`, 0, map[string]string{"cluster.status": "green"}, false, "assertion failed at 'cluster.status'"},
		{"first failing assertion in key order", 200, `{"a":1,"b":2,"c":3,"d":4,"e":5,"f":6,"g":7,"h":8}`, 0,
			map[string]string{"h": "x", "c": "x", "f": "x", "a": "x", "g": "x", "b": "x", "e": "x", "d": "x"}, false, "assertion failed at 'a'"},
		{"not JSON", 200, `<html>`, 0, map[string]string{"status": "UP"}, false, "invalid JSON response"},
		{"top-level array", 200, `[{"status":"UP"}]`, 0, map[string]string{"status": "UP"}, false, "invalid JSON response"},
		{"body over the read limit", 200, big, 0, map[string]string{"status": "UP"}, false, "HTTP-JSON check failed: response body exceeds 1 MiB"},
		{"body at the read limit", 200, atLimit, 0, map[string]string{"status": "UP"}, true, "HTTP-JSON check successful"},
		{"trailing whitespace", 200, "{\"status\":\"UP\"}\n", 0, map[string]string{"status": "UP"}, true, "HTTP-JSON check successful"},
		{"a second JSON value", 200, `{"status":"UP"}{"status":"DOWN"}`, 0, map[string]string{"status": "UP"}, false, "invalid JSON response"},
		{"trailing markup", 200, `{"status":"UP"} <html>oops</html>`, 0, map[string]string{"status": "UP"}, false, "invalid JSON response"},
		{"float with a trailing zero", 200, `{"ratio":1.0}`, 0, map[string]string{"ratio": "1"}, true, "HTTP-JSON check successful"},
		{"decimal with a trailing zero", 200, `{"price":100.50}`, 0, map[string]string{"price": "100.5"}, true, "HTTP-JSON check successful"},
		{"exponent in the response", 200, `{"n":1e3}`, 0, map[string]string{"n": "1000"}, true, "HTTP-JSON check successful"},
		{"exponent in the assertion", 200, `{"n":1000}`, 0, map[string]string{"n": "1e3"}, true, "HTTP-JSON check successful"},
		{"integers one apart beyond float64", 200, `{"id":12345678901234567890}`, 0, map[string]string{"id": "12345678901234567891"}, false,
			"assertion failed at 'id': expected '12345678901234567891', got '12345678901234567890'"},
		{"hex is not a JSON number", 200, `{"n":16}`, 0, map[string]string{"n": "0x10"}, false, "assertion failed at 'n'"},
		{"number as a string is compared as text", 200, `{"n":"1.0"}`, 0, map[string]string{"n": "1"}, false, "assertion failed at 'n'"},
		{"huge exponent", 200, `{"n":1e999999999}`, 0, map[string]string{"n": "1"}, false, "assertion failed at 'n'"},
		{"huge exponents that differ", 200, `{"n":1e999999999}`, 0, map[string]string{"n": "2e999999999"}, false, "assertion failed at 'n'"},
		{"tiny exponent is not zero", 200, `{"n":1e-999999999}`, 0, map[string]string{"n": "0"}, false, "assertion failed at 'n'"},
		{"digits past any float precision", 200, `{"n":1.5` + strings.Repeat("0", 150) + `1}`, 0, map[string]string{"n": "1.5"}, false, "assertion failed at 'n'"},
		{"quoted number in the assertion", 200, `{"n":1}`, 0, map[string]string{"n": `"1"`}, false, "assertion failed at 'n'"},
		{"padded number in the assertion", 200, `{"n":1}`, 0, map[string]string{"n": " 1"}, false, "assertion failed at 'n'"},
		{"negative zero", 200, `{"n":-0}`, 0, map[string]string{"n": "0"}, true, "HTTP-JSON check successful"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			url := jsonServer(t, tc.status, tc.body)
			d, err := NewHTTPJSONDriver(url, "", &v1.HttpJsonCheck{ExpectedStatusCode: tc.expect, JsonAssertions: tc.assertions})
			if err != nil {
				t.Fatal(err)
			}
			res := checkWithin(t, d, 5*time.Second)
			if res.Success != tc.wantOK || !strings.HasPrefix(res.Message, tc.wantMsg) {
				t.Fatalf("success=%v %q", res.Success, res.Message)
			}
		})
	}
}

func TestExactNumber(t *testing.T) {
	tests := []struct {
		literal string
		want    bool
	}{
		{"0", true}, {"-0", true}, {"12", true}, {"1.5e+3", true}, {"1E-3", true},
		{"1e1000", true}, {"1e-1000", true}, {"1e1001", false}, {"1e-1001", false},
		{strings.Repeat("1", 100), true}, {strings.Repeat("1", 101), false},
		{"0x10", false}, {"01", false}, {"+1", false}, {".5", false}, {"1.", false}, {"1/2", false}, {" 1", false}, {`"1"`, false}, {"", false},
	}
	for _, tc := range tests {
		if _, ok := exactNumber(tc.literal); ok != tc.want {
			t.Errorf("exactNumber(%q) ok=%v, want %v", tc.literal, ok, tc.want)
		}
	}
}

func TestHTTPJSONDriverRedirects(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/final" {
			fmt.Fprint(w, `{"status":"UP"}`)
			return
		}
		w.Header().Set("Location", "/final")
		w.WriteHeader(http.StatusMovedPermanently)
		fmt.Fprint(w, `{"status":"MOVED"}`)
	}))
	t.Cleanup(srv.Close)
	tests := []struct {
		name    string
		expect  int
		status  string
		wantOK  bool
		wantMsg string
	}{
		{"followed by default", 0, "UP", true, "HTTP-JSON check successful"},
		{"followed for an expected 200", 200, "UP", true, "HTTP-JSON check successful"},
		{"not followed for an expected 301", 301, "MOVED", true, "HTTP-JSON check successful"},
		{"not followed for an expected 302", 302, "MOVED", false, "HTTP-JSON check failed (status: 301, expected 302)"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			d, _ := NewHTTPJSONDriver(srv.URL, "", &v1.HttpJsonCheck{ExpectedStatusCode: tc.expect, JsonAssertions: map[string]string{"status": tc.status}})
			res, _ := d.Check()
			if res.Success != tc.wantOK || !strings.HasPrefix(res.Message, tc.wantMsg) {
				t.Fatalf("success=%v %q", res.Success, res.Message)
			}
		})
	}
}

func TestJSONDriversReadAtMost1MiB(t *testing.T) {
	opensearch := `"status":"green"`
	trino := `"coordinator":true,"starting":false`
	tests := []struct {
		name    string
		build   func(url string) (Driver, error)
		body    string
		wantOK  bool
		wantMsg string
	}{
		{"opensearch over the limit", func(u string) (Driver, error) { return NewOpenSearchDriver(u, "") }, paddedJSON(maxBody+1, opensearch), false,
			"OpenSearch check failed to parse response: response body exceeds 1 MiB"},
		{"opensearch at the limit", func(u string) (Driver, error) { return NewOpenSearchDriver(u, "") }, paddedJSON(maxBody, opensearch), true, "OpenSearch cluster is healthy"},
		{"trino over the limit", func(u string) (Driver, error) { return NewTrinoDriver(u, "") }, paddedJSON(maxBody+1, trino), false,
			"Trino check failed to parse response: response body exceeds 1 MiB"},
		{"trino at the limit", func(u string) (Driver, error) { return NewTrinoDriver(u, "") }, paddedJSON(maxBody, trino), true, "Trino check successful"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			d, _ := tc.build(jsonServer(t, 200, tc.body))
			res, _ := d.Check()
			if res.Success != tc.wantOK || !strings.HasPrefix(res.Message, tc.wantMsg) {
				t.Fatalf("success=%v %q", res.Success, res.Message)
			}
		})
	}
}

func TestDriversReportShortBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "1000")
		fmt.Fprint(w, `{"status":"UP"}`)
	}))
	t.Cleanup(srv.Close)
	httpJSON, _ := NewHTTPJSONDriver(srv.URL, "", &v1.HttpJsonCheck{JsonAssertions: map[string]string{"status": "UP"}})
	httpBody, _ := NewHTTPDriver(srv.URL, "", nil, &v1.HttpCheck{BodyContains: "UP"})
	opensearch, _ := NewOpenSearchDriver(srv.URL, "")
	trino, _ := NewTrinoDriver(srv.URL, "")
	for _, tc := range []struct {
		d       Driver
		wantMsg string
	}{
		{httpJSON, "failed to read HTTP response body"},
		{httpBody, "HTTP check failed to read body: unexpected EOF"},
		{opensearch, "OpenSearch check failed to parse response: unexpected EOF"},
		{trino, "Trino check failed to parse response: unexpected EOF"},
	} {
		res, _ := tc.d.Check()
		if res.Success || res.Message != tc.wantMsg {
			t.Errorf("%s: success=%v %q", tc.d.GetType(), res.Success, res.Message)
		}
	}
}

func TestHTTPDriverBodyContains(t *testing.T) {
	tests := []struct {
		name    string
		status  int
		body    string
		want    string
		wantOK  bool
		wantMsg string
	}{
		{"found", 200, "service OK", "OK", true, "HTTP check successful (status: 200"},
		{"not found", 200, "service DOWN", "OK", false, "HTTP check failed: body does not contain 'OK' (status: 200)"},
		{"found past the read limit", 200, strings.Repeat("x", maxBody) + "OK", "OK", false, "HTTP check failed: the first 1 MiB of the body does not contain 'OK' (status: 200)"},
		{"found at the end of the read limit", 200, strings.Repeat("x", maxBody-2) + "OK", "OK", true, "HTTP check successful (status: 200"},
		{"found at the start of a body over the limit", 200, "OK" + strings.Repeat("x", maxBody), "OK", true, "HTTP check successful (status: 200"},
		{"server error", 500, "OK", "OK", false, "HTTP check failed (status: 500)"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			d, _ := NewHTTPDriver(jsonServer(t, tc.status, tc.body), "", nil, &v1.HttpCheck{BodyContains: tc.want})
			res, _ := d.Check()
			if res.Success != tc.wantOK || !strings.HasPrefix(res.Message, tc.wantMsg) {
				t.Fatalf("success=%v %q", res.Success, res.Message)
			}
		})
	}
}

func TestFailureMessagesAreStable(t *testing.T) {
	var delay atomic.Int64
	var uptime atomic.Int64
	slow := func(status int, body func() string) string {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			time.Sleep(time.Duration(delay.Load()))
			w.WriteHeader(status)
			fmt.Fprint(w, body())
		}))
		t.Cleanup(srv.Close)
		return srv.URL
	}
	fixed := func(s string) func() string { return func() string { return s } }
	trinoBody := func() string {
		return fmt.Sprintf(`{"coordinator":true,"starting":true,"uptime":"%dm","nodeVersion":{"version":"450"},"environment":"prod"}`, uptime.Add(1))
	}
	var shards atomic.Int64
	shards.Store(20)
	recovering := func(status string) func() string {
		return func() string {
			return fmt.Sprintf(`{"status":"%s","number_of_nodes":%d,"unassigned_shards":%d}`, status, 3+shards.Load()%2, shards.Add(-1))
		}
	}

	drivers := map[string]Driver{}
	drivers["http status"], _ = NewHTTPDriver(slow(500, fixed("")), "", nil, nil)
	drivers["http body"], _ = NewHTTPDriver(slow(200, fixed("DOWN")), "", nil, &v1.HttpCheck{BodyContains: "OK"})
	drivers["opensearch status"], _ = NewOpenSearchDriver(slow(503, fixed("")), "")
	drivers["opensearch yellow"], _ = NewOpenSearchDriver(slow(200, recovering("yellow")), "")
	drivers["opensearch red"], _ = NewOpenSearchDriver(slow(200, recovering("red")), "")
	drivers["trino status"], _ = NewTrinoDriver(slow(503, fixed("")), "")
	drivers["trino starting"], _ = NewTrinoDriver(slow(200, trinoBody), "")

	for name, d := range drivers {
		t.Run(name, func(t *testing.T) {
			delay.Store(0)
			first, _ := d.Check()
			delay.Store(int64(30 * time.Millisecond))
			second, _ := d.Check()
			if first.Success || second.Success {
				t.Fatalf("expected failures: %q / %q", first.Message, second.Message)
			}
			if first.Message != second.Message {
				t.Fatalf("the failure message changed between checks:\n%s\n%s", first.Message, second.Message)
			}
		})
	}
}

func TestPingDriverEndpointWithPort(t *testing.T) {
	for _, network := range []string{"tcp4", "tcp6"} {
		t.Run(network, func(t *testing.T) {
			endpoint, accepted := tcpServer(func(net.Conn) {})(t, network)
			d, _ := NewPingDriver(endpoint, "")
			res, _ := d.Check()
			if !res.Success || !waitAccepted(accepted) {
				t.Fatalf("ping of %s: success=%v %s", endpoint, res.Success, res.Message)
			}
		})
	}
}

func httpsServer(t *testing.T, cert tls.Certificate) (string, chan string) {
	t.Helper()
	sni := make(chan string, 4)
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	srv.TLS = &tls.Config{
		Certificates: []tls.Certificate{cert},
		GetConfigForClient: func(hello *tls.ClientHelloInfo) (*tls.Config, error) {
			select {
			case sni <- hello.ServerName:
			default:
			}
			return nil, nil
		},
	}
	srv.StartTLS()
	t.Cleanup(srv.Close)
	return srv.URL, sni
}

func TestHTTPDriverTlsCheckHost(t *testing.T) {
	tests := []struct {
		name      string
		cert      tls.Certificate
		family    string
		check     *v1.TlsCheck
		wantOK    bool
		wantInMsg string
		wantSNI   string
	}{
		{"certificate for tlsCheck.host", nameOnlyCert, "", &v1.TlsCheck{Host: "tls-host.test"}, true, "HTTP check successful", "tls-host.test"},
		{"certificate for tlsCheck.host over a pinned family", nameOnlyCert, FamilyIPv4, &v1.TlsCheck{Host: "tls-host.test"}, true, "HTTP check successful", "tls-host.test"},
		{"certificate for another name than the endpoint", nameOnlyCert, "", &v1.TlsCheck{}, false,
			"x509: cannot validate certificate for 127.0.0.1 because it doesn't contain any IP SANs", ""},
		{"tlsCheck.host not on the certificate", testCert, "", &v1.TlsCheck{Host: "other.test"}, false,
			"x509: certificate is valid for localhost, not other.test", "other.test"},
		{"certificate for the endpoint", testCert, "", &v1.TlsCheck{}, true, "HTTP check successful", ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			url, sni := httpsServer(t, tc.cert)
			d, _ := NewHTTPDriver(url, tc.family, tc.check, nil)
			res, _ := d.Check()
			if res.Success != tc.wantOK || !strings.Contains(res.Message, tc.wantInMsg) {
				t.Fatalf("success=%v %s", res.Success, res.Message)
			}
			select {
			case got := <-sni:
				if got != tc.wantSNI {
					t.Fatalf("SNI %q, want %q", got, tc.wantSNI)
				}
			default:
				t.Fatal("the server saw no ClientHello")
			}
		})
	}
	if cfg := http.DefaultTransport.(*http.Transport).TLSClientConfig; cfg != nil && cfg.ServerName != "" {
		t.Fatalf("http.DefaultTransport was given ServerName %q", cfg.ServerName)
	}
}

func TestHTTPDriverTlsCheckHostLeavesNoConnectionOpen(t *testing.T) {
	var open atomic.Int64
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	srv.TLS = &tls.Config{Certificates: []tls.Certificate{nameOnlyCert}}
	srv.Config.ConnState = func(c net.Conn, state http.ConnState) {
		switch state {
		case http.StateNew:
			open.Add(1)
		case http.StateClosed, http.StateHijacked:
			open.Add(-1)
		}
	}
	srv.StartTLS()
	t.Cleanup(srv.Close)
	for i := 0; i < 4; i++ {
		d, _ := NewHTTPDriver(srv.URL, "", &v1.TlsCheck{Host: "tls-host.test"}, nil)
		if res, _ := d.Check(); !res.Success {
			t.Fatalf("check %d: %s", i, res.Message)
		}
	}
	deadline := time.Now().Add(2 * time.Second)
	for open.Load() != 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if n := open.Load(); n != 0 {
		t.Fatalf("%d connections still open after the checks", n)
	}
}
