package controller

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	monitoringv1alpha1 "github.com/patrickdk77/endpoint-monitoring-operator/api/v1alpha1"
)

func TestAlertFailureReportedWhenRecordingTheChangeFails(t *testing.T) {
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			c.Close()
		}
	}()
	failing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer failing.Close()
	var changes atomic.Int32
	delivered := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		changes.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer delivered.Close()

	scheme := runtime.NewScheme()
	if err := monitoringv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	m := &monitoringv1alpha1.EndpointMonitor{
		ObjectMeta: metav1.ObjectMeta{Name: "conflict", Namespace: "default"},
		Spec: monitoringv1alpha1.EndpointMonitorSpec{
			Driver:        "tcp",
			Endpoint:      ln.Addr().String(),
			CheckInterval: 60,
			Notify: monitoringv1alpha1.NotifyConfig{
				Discord: &monitoringv1alpha1.DiscordConfig{Enabled: true, WebhookURL: failing.URL, AlertOn: []string{"success"}},
				Webhook: &monitoringv1alpha1.WebhookConfig{Enabled: true, WebhookURL: delivered.URL, AlertOn: []string{"change"}},
			},
		},
	}
	var statusWrites atomic.Int32
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(m).WithStatusSubresource(m).
		WithInterceptorFuncs(interceptor.Funcs{
			SubResourceUpdate: func(ctx context.Context, c client.Client, sub string, obj client.Object, opts ...client.SubResourceUpdateOption) error {
				if statusWrites.Add(1) == 2 {
					return apierrors.NewConflict(schema.GroupResource{Group: "monitoring.licious.app", Resource: "endpointmonitors"}, obj.GetName(), nil)
				}
				return c.SubResource(sub).Update(ctx, obj, opts...)
			},
		}).Build()
	recorder := events.NewFakeRecorder(10)
	r := &EndpointMonitorReconciler{Client: c, Scheme: scheme, Recorder: recorder}

	_, err = r.Reconcile(context.Background(), reconcile.Request{NamespacedName: types.NamespacedName{Name: "conflict", Namespace: "default"}})
	if !apierrors.IsConflict(err) {
		t.Fatalf("err = %v, want the conflict from recording the change", err)
	}
	if changes.Load() != 1 {
		t.Fatalf("change alerts sent: %d, want 1", changes.Load())
	}
	if len(recorder.Events) != 1 {
		t.Fatalf("%d events, want the failed status alert only", len(recorder.Events))
	}
	want := "Warning AlertFailed success alert was not sent by discord; the operator log has the error"
	if got := <-recorder.Events; got != want {
		t.Fatalf("event %q, want %q", got, want)
	}
}
