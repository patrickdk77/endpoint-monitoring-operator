package notifier

import (
	"context"
	"errors"
	"testing"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func TestDisplayEndpoint(t *testing.T) {
	tests := []struct{ in, want string }{
		{"https://monitor:s3cret@example.test/health?token=t0ken#frag", "https://monitor:***@example.test/health"},
		{"HTTP://monitor:s3cret@example.test/p", "http://monitor:***@example.test/p"},
		{" https://monitor:s3cret@example.test/p", "https://monitor:***@example.test/p"},
		{"\thttp://monitor:s3cret@example.test/p\n", "http://monitor:***@example.test/p"},
		{"//monitor:s3cret@example.test/p?token=t0ken", "//monitor:***@example.test/p"},
		{"ftp://monitor:s3cret@example.test/p", "ftp://monitor:***@example.test/p"},
		{"http://example.test:8080/health", "http://example.test:8080/health"},
		{"http://monitor@example.test/p?x=1", "http://monitor@example.test/p"},
		{"http://example.test/?", "http://example.test/"},
		{"http://monitor:pa/ss@127.0.0.1:1/health", "(invalid URL)"},
		{"http://monitor:s3cret@127.0.0.1:1x/health", "(invalid URL)"},
		{"127.0.0.1:6379", "127.0.0.1:6379"},
		{"example.test", "example.test"},
		{"[::1]:25", "[::1]:25"},
	}
	for _, tc := range tests {
		if got := DisplayEndpoint(tc.in); got != tc.want {
			t.Errorf("DisplayEndpoint(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestGetSecretGivesUpAfterTimeout(t *testing.T) {
	old := Timeout
	Timeout = 100 * time.Millisecond
	t.Cleanup(func() { Timeout = old })
	stalled := fake.NewClientBuilder().WithInterceptorFuncs(interceptor.Funcs{
		Get: func(ctx context.Context, _ client.WithWatch, _ client.ObjectKey, _ client.Object, _ ...client.GetOption) error {
			<-ctx.Done()
			return ctx.Err()
		},
	}).Build()
	done := make(chan error, 1)
	go func() {
		_, err := GetSecret("creds", "default", stalled)
		done <- err
	}()
	select {
	case err := <-done:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("err = %v, want a deadline", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("GetSecret is still waiting after 2s with a 100ms timeout")
	}
}
