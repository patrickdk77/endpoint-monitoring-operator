package controller

import (
	"context"
	"net"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	monitoringv1alpha1 "github.com/patrickdk77/endpoint-monitoring-operator/api/v1alpha1"
)

func TestReconcileStoredInvalidSpec(t *testing.T) {
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	var probes atomic.Int32
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			probes.Add(1)
			c.Close()
		}
	}()

	scheme := runtime.NewScheme()
	if err := monitoringv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name      string
		driver    string
		interval  int
		wantRetry time.Duration
		wantEvent string
	}{
		{"zero interval", "tcp", 0, 0, "Warning CannotCheck checkInterval must be between 1 and 31536000, got 0"},
		{"negative interval", "tcp", -5, 0, "Warning CannotCheck checkInterval must be between 1 and 31536000, got -5"},
		{"interval over the maximum", "tcp", 31536001, 0, "Warning CannotCheck checkInterval must be between 1 and 31536000, got 31536001"},
		{"interval that overflows a Duration", "tcp", 9223372037, 0, "Warning CannotCheck checkInterval must be between 1 and 31536000, got 9223372037"},
		{"unknown driver", "kafka", 30, 30 * time.Second, "Warning CannotCheck cannot create driver: unsupported driver type: kafka"},
		{"unknown driver, long interval", "kafka", 3600, 30 * time.Second, "Warning CannotCheck cannot create driver: unsupported driver type: kafka"},
		{"empty driver", "", 10, 10 * time.Second, "Warning CannotCheck cannot create driver: unsupported driver type: "},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			m := &monitoringv1alpha1.EndpointMonitor{
				ObjectMeta: metav1.ObjectMeta{Name: "stored", Namespace: "default"},
				Spec: monitoringv1alpha1.EndpointMonitorSpec{
					Driver:        tc.driver,
					Endpoint:      ln.Addr().String(),
					CheckInterval: tc.interval,
					Notify: monitoringv1alpha1.NotifyConfig{
						Discord: &monitoringv1alpha1.DiscordConfig{Enabled: true, WebhookURL: "http://127.0.0.1:1"},
					},
				},
			}
			c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(m).WithStatusSubresource(m).Build()
			recorder := events.NewFakeRecorder(10)
			r := &EndpointMonitorReconciler{Client: c, Scheme: scheme, Recorder: recorder}
			before := probes.Load()

			res, err := r.Reconcile(context.Background(), reconcile.Request{NamespacedName: types.NamespacedName{Name: "stored", Namespace: "default"}})
			if err != nil {
				t.Fatalf("a spec that cannot be checked must not be retried with backoff: %v", err)
			}
			if res != (ctrl.Result{RequeueAfter: tc.wantRetry}) {
				t.Fatalf("result %+v, want RequeueAfter %v", res, tc.wantRetry)
			}
			if len(recorder.Events) != 1 {
				t.Fatalf("%d events, want 1", len(recorder.Events))
			}
			if got := <-recorder.Events; !strings.HasPrefix(got, tc.wantEvent) {
				t.Fatalf("event %q, want prefix %q", got, tc.wantEvent)
			}
			time.Sleep(50 * time.Millisecond)
			if probes.Load() != before {
				t.Fatal("a spec that cannot be checked was probed")
			}
			var got monitoringv1alpha1.EndpointMonitor
			if err := c.Get(context.Background(), types.NamespacedName{Name: "stored", Namespace: "default"}, &got); err != nil {
				t.Fatal(err)
			}
			if !got.Status.LastCheckedTime.IsZero() || got.Status.LastStatus != "" {
				t.Fatalf("status changed: %+v", got.Status)
			}
		})
	}
}
