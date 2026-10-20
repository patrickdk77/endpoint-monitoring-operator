package driver

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	v1 "github.com/patrickdk77/endpoint-monitoring-operator/api/v1alpha1"
)

// readRESP reads one RESP array of bulk strings, the form redisEncode writes.
func readRESP(r *bufio.Reader) ([]string, error) {
	header, err := r.ReadString('\n')
	if err != nil {
		return nil, err
	}
	if !strings.HasPrefix(header, "*") {
		return nil, fmt.Errorf("not an array: %q", header)
	}
	n, err := strconv.Atoi(strings.TrimSpace(header[1:]))
	if err != nil {
		return nil, err
	}
	args := make([]string, n)
	for i := range args {
		size, err := r.ReadString('\n')
		if err != nil {
			return nil, err
		}
		l, err := strconv.Atoi(strings.TrimSpace(size[1:]))
		if err != nil {
			return nil, err
		}
		buf := make([]byte, l+2)
		if _, err := io.ReadFull(r, buf); err != nil {
			return nil, err
		}
		args[i] = string(buf[:l])
	}
	return args, nil
}

type fakeRedis struct {
	mu       sync.Mutex
	commands [][]string
	closed   int
}

// serve answers each command with the reply named for it, or +OK.
func (f *fakeRedis) serve(replies map[string]string) func(net.Conn) {
	return func(c net.Conn) {
		defer func() {
			f.mu.Lock()
			f.closed++
			f.mu.Unlock()
		}()
		r := bufio.NewReader(c)
		for {
			args, err := readRESP(r)
			if err != nil {
				return
			}
			f.mu.Lock()
			f.commands = append(f.commands, args)
			f.mu.Unlock()
			reply, ok := replies[args[0]]
			if !ok {
				reply = "+OK"
			}
			if reply == "close" {
				return
			}
			fmt.Fprintf(c, "%s\r\n", reply)
		}
	}
}

// sentBeforeClose returns every command the client sent, once the
// connection has ended.
func (f *fakeRedis) sentBeforeClose(t *testing.T) [][]string {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		f.mu.Lock()
		closed, commands := f.closed, append([][]string(nil), f.commands...)
		f.mu.Unlock()
		if closed > 0 {
			return commands
		}
		if time.Now().After(deadline) {
			t.Fatalf("the connection never ended; commands so far %q", commands)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func secretClient(t *testing.T, data map[string]string) client.Client {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "creds", Namespace: "default"}, Data: map[string][]byte{}}
	for k, v := range data {
		secret.Data[k] = []byte(v)
	}
	return fake.NewClientBuilder().WithScheme(scheme).WithObjects(secret).Build()
}

func TestRedisDriver(t *testing.T) {
	const password = "pass word with spaces"
	tests := []struct {
		name        string
		secret      map[string]string
		replies     map[string]string
		wantOK      bool
		wantInMsg   string
		wantCommand [][]string
	}{
		{"no auth", nil, map[string]string{"PING": "+PONG"}, true, "REDIS check successful",
			[][]string{{"PING"}, {"QUIT"}}},
		{"password", map[string]string{"password": password}, map[string]string{"PING": "+PONG"}, true, "REDIS check successful",
			[][]string{{"AUTH", password}, {"PING"}, {"QUIT"}}},
		{"username and password", map[string]string{"username": "monitor", "password": password}, map[string]string{"PING": "+PONG"}, true, "REDIS check successful",
			[][]string{{"AUTH", "monitor", password}, {"PING"}, {"QUIT"}}},
		{"username without password sends no AUTH", map[string]string{"username": "monitor"}, map[string]string{"PING": "+PONG"}, true, "REDIS check successful",
			[][]string{{"PING"}, {"QUIT"}}},
		{"wrong password", map[string]string{"password": password}, map[string]string{"AUTH": "-WRONGPASS invalid username-password pair"}, false, `AUTH: unexpected reply "-WRONGPASS"`,
			[][]string{{"AUTH", password}}},
		{"AUTH error that echoes the password", map[string]string{"password": password}, map[string]string{"AUTH": "-ERR unknown command 'AUTH', with args beginning with: '" + password + "'"}, false, `AUTH: unexpected reply "-ERR"`,
			[][]string{{"AUTH", password}}},
		{"reply that only starts with +OK", map[string]string{"password": password}, map[string]string{"AUTH": "+OKAY"}, false, `AUTH: unexpected reply "+OKAY"`,
			[][]string{{"AUTH", password}}},
		{"reply that only starts with +PONG", nil, map[string]string{"PING": "+PONGX"}, false, `PING: unexpected reply "+PONGX"`,
			[][]string{{"PING"}}},
		{"long reply is cut", nil, map[string]string{"PING": "-ERR " + strings.Repeat("x", 1000)}, false, fmt.Sprintf("PING: unexpected reply %q", "-ERR "+strings.Repeat("x", 123)+"..."),
			[][]string{{"PING"}}},
		{"auth required", nil, map[string]string{"PING": "-NOAUTH Authentication required."}, false, `PING: unexpected reply "-NOAUTH Authentication required."`,
			[][]string{{"PING"}}},
		{"error reply", nil, map[string]string{"PING": "-ERR unknown command"}, false, `PING: unexpected reply "-ERR unknown command"`,
			[][]string{{"PING"}}},
		{"not redis", nil, map[string]string{"PING": "HTTP/1.1 400 Bad Request"}, false, `PING: unexpected reply "HTTP/1.1 400 Bad Request"`,
			[][]string{{"PING"}}},
		{"pong in lower case", nil, map[string]string{"PING": "+pong"}, false, `PING: unexpected reply "+pong"`,
			[][]string{{"PING"}}},
		{"connection closed", nil, map[string]string{"PING": "close"}, false, "PING: EOF",
			[][]string{{"PING"}}},
	}
	for _, tc := range tests {
		for _, overTLS := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/tls=%v", tc.name, overTLS), func(t *testing.T) {
				f := &fakeRedis{}
				serve := tcpServer(f.serve(tc.replies))
				if overTLS {
					serve = tlsServer(f.serve(tc.replies))
				}
				endpoint, _ := serve(t, "tcp4")
				check := &v1.RedisCheck{Tls: overTLS}
				var c client.Client
				if tc.secret != nil {
					check.SecretRef.Name = "creds"
					c = secretClient(t, tc.secret)
				}
				d, err := NewREDISDriver(endpoint, "", check, "default", c)
				if err != nil {
					t.Fatal(err)
				}
				res, _ := d.Check()
				if res.Success != tc.wantOK || !strings.Contains(res.Message, tc.wantInMsg) {
					t.Fatalf("success=%v %s", res.Success, res.Message)
				}
				if strings.Contains(res.Message, password) {
					t.Fatalf("the message leaks the password: %s", res.Message)
				}
				if got, want := fmt.Sprintf("%q", f.sentBeforeClose(t)), fmt.Sprintf("%q", tc.wantCommand); got != want {
					t.Fatalf("commands %s, want %s", got, want)
				}
			})
		}
	}
}

func TestRedisDriverReadsAtMost64KiB(t *testing.T) {
	endpoint, _ := tcpServer(func(c net.Conn) {
		r := bufio.NewReader(c)
		if _, err := readRESP(r); err != nil {
			return
		}
		_, _ = c.Write([]byte(strings.Repeat("x", 128<<10)))
		_, _ = io.Copy(io.Discard, r)
	})(t, "tcp4")
	d, _ := NewREDISDriver(endpoint, "", &v1.RedisCheck{}, "default", nil)
	res, _ := d.Check()
	if res.Success || !strings.Contains(res.Message, "PING: reply longer than 64 KiB") {
		t.Fatalf("success=%v %s", res.Success, res.Message)
	}
}

func TestRedisDriverWithoutRedisCheck(t *testing.T) {
	f := &fakeRedis{}
	endpoint, _ := tcpServer(f.serve(map[string]string{"PING": "+PONG"}))(t, "tcp4")
	d, err := NewREDISDriver(endpoint, "", nil, "default", nil)
	if err != nil {
		t.Fatal(err)
	}
	if res, _ := d.Check(); !res.Success {
		t.Fatalf("a monitor without redisCheck failed: %s", res.Message)
	}
}

func TestRedisDriverMissingSecret(t *testing.T) {
	c := secretClient(t, nil)
	_, err := NewREDISDriver("127.0.0.1:6379", "", &v1.RedisCheck{SecretRef: v1.SecretRef{Name: "absent"}}, "default", c)
	if err == nil || !apierrors.IsNotFound(err) {
		t.Fatalf("err = %v, want a wrapped NotFound", err)
	}
}
