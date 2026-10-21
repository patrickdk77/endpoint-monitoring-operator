//go:build docker

package docker

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	v1 "github.com/patrickdk77/endpoint-monitoring-operator/api/v1alpha1"
	"github.com/patrickdk77/endpoint-monitoring-operator/internal/driver"
)

const password = "e2etest"

var creds = v1.SecretRef{Name: "creds"}

// label marks every container and network a run creates, with the run's id
// as its value.
const label = "emo-e2e"

var runID = randomHex()

var (
	creating sync.RWMutex
	stopping bool
)

func randomHex() string {
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func TestMain(m *testing.M) {
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-signals
		signal.Stop(signals)
		creating.Lock()
		stopping = true
		creating.Unlock()
		sweep()
		os.Exit(1)
	}()
	code := m.Run()
	sweep()
	os.Exit(code)
}

// sweep removes whatever this run created and did not remove itself.
func sweep() {
	filter := "label=" + label + "=" + runID
	if out, _ := exec.Command("docker", "ps", "-aq", "--filter", filter).Output(); len(out) > 0 {
		_ = exec.Command("docker", append([]string{"rm", "-f", "-v"}, strings.Fields(string(out))...)...).Run()
	}
	if out, _ := exec.Command("docker", "network", "ls", "-q", "--filter", filter).Output(); len(out) > 0 {
		_ = exec.Command("docker", append([]string{"network", "rm"}, strings.Fields(string(out))...)...).Run()
	}
}

// docker runs a docker command in its own process group, so a Ctrl-C meant
// for the run cannot stop it halfway.
func docker(t *testing.T, args ...string) string {
	t.Helper()
	var stderr bytes.Buffer
	cmd := exec.Command("docker", args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("docker %s: %v\n%s%s", strings.Join(args, " "), err, out, stderr.Bytes())
	}
	return strings.TrimSpace(string(out))
}

// create runs a docker command that creates a container or a network. Once
// the run is interrupted it creates nothing, so the sweep sees everything.
func create(t *testing.T, args ...string) string {
	t.Helper()
	creating.RLock()
	defer creating.RUnlock()
	if stopping {
		t.Fatal("the run was interrupted")
	}
	return docker(t, args...)
}

type server struct {
	image      string
	port       int
	env        []string
	network    string
	name       string
	entrypoint string
	args       []string
}

// start runs the server detached with its port published on loopback, and
// removes the container when the test ends. It returns the container and
// the published host:port.
func start(t *testing.T, s server) (string, string) {
	t.Helper()
	args := []string{"create", "--label", label + "=" + runID, "-p", fmt.Sprintf("127.0.0.1::%d", s.port)}
	if s.network != "" {
		args = append(args, "--network", s.network, "--network-alias", s.name)
	}
	for _, e := range s.env {
		args = append(args, "-e", e)
	}
	if s.entrypoint != "" {
		args = append(args, "--entrypoint", s.entrypoint)
	}
	args = append(append(args, s.image), s.args...)
	id := create(t, args...)
	t.Cleanup(func() { _ = exec.Command("docker", "rm", "-f", "-v", id).Run() })
	docker(t, "start", id)
	endpoint, _, _ := strings.Cut(docker(t, "port", id, fmt.Sprintf("%d/tcp", s.port)), "\n")
	return id, endpoint
}

func network(t *testing.T) string {
	t.Helper()
	name := "emo-e2e-" + randomHex()
	create(t, "network", "create", "--label", label+"="+runID, name)
	t.Cleanup(func() { _ = exec.Command("docker", "network", "rm", name).Run() })
	return name
}

// run runs a shell script in the container until it succeeds or the limit
// passes, for servers that are still starting.
func run(t *testing.T, id, script string, limit time.Duration) string {
	t.Helper()
	deadline := time.Now().Add(limit)
	for {
		out, err := exec.Command("docker", "exec", id, "sh", "-c", script).CombinedOutput()
		if err == nil {
			return strings.TrimSpace(string(out))
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s: %v\n%s\n%s", script, err, out, logs(id))
		}
		time.Sleep(time.Second)
	}
}

func logs(id string) string {
	out, _ := exec.Command("docker", "logs", "--tail", "40", id).CombinedOutput()
	return string(out)
}

// passes checks until one passes or the limit passes, for servers that
// accept connections before they are ready.
func passes(t *testing.T, id string, d driver.Driver, limit time.Duration) {
	t.Helper()
	deadline := time.Now().Add(limit)
	for {
		res, err := d.Check()
		if err == nil && res.Success {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the check never passed: %v %+v\n%s", err, res, logs(id))
		}
		time.Sleep(time.Second)
	}
}

// fails checks until one fails with a message containing want, or the limit
// passes.
func fails(t *testing.T, d driver.Driver, want string, limit time.Duration) {
	t.Helper()
	deadline := time.Now().Add(limit)
	var last string
	for {
		res, err := d.Check()
		if err == nil && !res.Success && strings.Contains(res.Message, want) {
			return
		}
		if err != nil {
			last = err.Error()
		} else {
			last = res.Message
		}
		if time.Now().After(deadline) {
			t.Fatalf("the check never failed with %q; last: %s", want, last)
		}
		time.Sleep(time.Second)
	}
}

// secrets returns a client holding the Secret creds in the default
// namespace.
func secrets(t *testing.T, data map[string]string) client.Client {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: creds.Name, Namespace: "default"}, StringData: data, Data: map[string][]byte{}}
	for k, v := range data {
		secret.Data[k] = []byte(v)
	}
	return fake.NewClientBuilder().WithScheme(scheme).WithObjects(secret).Build()
}

func login(user, pass string) map[string]string {
	return map[string]string{"username": user, "password": pass}
}
