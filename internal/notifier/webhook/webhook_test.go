package webhook

import (
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/patrickdk77/endpoint-monitoring-operator/api/v1alpha1"
	"github.com/patrickdk77/endpoint-monitoring-operator/internal/notifier"
)

func hook(t *testing.T, code int) (string, *atomic.Int32) {
	t.Helper()
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(code)
	}))
	t.Cleanup(srv.Close)
	return srv.URL, &hits
}

func TestWebhookReportsTemplateAndSecretErrors(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	noSecrets := fake.NewClientBuilder().WithScheme(scheme).Build()
	tests := []struct {
		name    string
		cfg     v1alpha1.WebhookConfig
		wantErr string
	}{
		{"template naming a missing field", v1alpha1.WebhookConfig{Contents: "{{.Nope}}"}, "failed to execute the contents template"},
		{"authorization secret that does not exist", v1alpha1.WebhookConfig{Authorization: v1alpha1.SecretRef{Name: "absent"}}, "unable to read the authorization secret"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			url, hits := hook(t, http.StatusOK)
			cfg := tc.cfg
			cfg.Enabled, cfg.WebhookURL = true, url
			n, err := New(&cfg)
			if err != nil {
				t.Fatal(err)
			}
			err = n.SendAlert("failure", &notifier.NoticeValues{Status: "failure", Namespace: "default"}, noSecrets)
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("err = %v, want %q", err, tc.wantErr)
			}
			var temporary *notifier.RetryableError
			if errors.As(err, &temporary) {
				t.Fatalf("err = %v is marked retryable", err)
			}
			if hits.Load() != 0 {
				t.Fatalf("the webhook was called %d times", hits.Load())
			}
		})
	}
}

func TestWebhookNewRejectsUnparsableTemplate(t *testing.T) {
	_, err := New(&v1alpha1.WebhookConfig{Enabled: true, WebhookURL: "http://127.0.0.1:1/", Contents: "{{.Status"})
	if err == nil || !strings.Contains(err.Error(), "failed to parse the contents template") {
		t.Fatalf("err = %v", err)
	}
}

func TestWebhookAcceptsAny2xx(t *testing.T) {
	for _, tc := range []struct {
		code   int
		wantOK bool
	}{
		{http.StatusOK, true}, {http.StatusCreated, true}, {http.StatusAccepted, true}, {http.StatusNoContent, true},
		{http.StatusNotModified, false}, {http.StatusBadRequest, false}, {http.StatusInternalServerError, false},
	} {
		for _, contents := range []string{"", "{{.Status}}"} {
			url, hits := hook(t, tc.code)
			n, err := New(&v1alpha1.WebhookConfig{Enabled: true, WebhookURL: url, Contents: contents})
			if err != nil {
				t.Fatal(err)
			}
			err = n.SendAlert("failure", &notifier.NoticeValues{Status: "failure"}, nil)
			if (err == nil) != tc.wantOK || hits.Load() != 1 {
				t.Fatalf("status %d contents %q: err=%v hits=%d", tc.code, contents, err, hits.Load())
			}
		}
	}
}

func TestWebhookMarksOnly429And5xxRetryable(t *testing.T) {
	for _, tc := range []struct {
		code      int
		retryable bool
	}{
		{http.StatusTooManyRequests, true}, {http.StatusInternalServerError, true}, {http.StatusBadGateway, true},
		{http.StatusServiceUnavailable, true}, {http.StatusGatewayTimeout, true},
		{http.StatusNotModified, false}, {http.StatusBadRequest, false}, {http.StatusUnauthorized, false},
		{http.StatusForbidden, false}, {http.StatusNotFound, false}, {http.StatusRequestEntityTooLarge, false},
	} {
		url, _ := hook(t, tc.code)
		n, err := New(&v1alpha1.WebhookConfig{Enabled: true, WebhookURL: url})
		if err != nil {
			t.Fatal(err)
		}
		err = n.SendAlert("failure", &notifier.NoticeValues{Status: "failure"}, nil)
		var temporary *notifier.RetryableError
		if err == nil || errors.As(err, &temporary) != tc.retryable {
			t.Fatalf("status %d: err = %v, want retryable %v", tc.code, err, tc.retryable)
		}
	}
}

func TestWebhookMarksARequestThatDoesNotCompleteRetryable(t *testing.T) {
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
	n, err := New(&v1alpha1.WebhookConfig{Enabled: true, WebhookURL: "http://" + ln.Addr().String() + "/"})
	if err != nil {
		t.Fatal(err)
	}
	err = n.SendAlert("failure", &notifier.NoticeValues{Status: "failure"}, nil)
	var temporary *notifier.RetryableError
	if !errors.As(err, &temporary) {
		t.Fatalf("err = %v, want it marked retryable", err)
	}
}
