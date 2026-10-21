package controller

import (
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	monitoringv1alpha1 "github.com/patrickdk77/endpoint-monitoring-operator/api/v1alpha1"
)

var _ = Describe("EndpointMonitor ipFamily", func() {
	newMonitor := func(name, endpoint string, families []string, webhook string) *monitoringv1alpha1.EndpointMonitor {
		return &monitoringv1alpha1.EndpointMonitor{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
			Spec: monitoringv1alpha1.EndpointMonitorSpec{
				Driver:        "tcp",
				Endpoint:      endpoint,
				CheckInterval: 60,
				IpFamily:      families,
				Notify: monitoringv1alpha1.NotifyConfig{
					Discord: &monitoringv1alpha1.DiscordConfig{Enabled: true, WebhookURL: webhook},
				},
			},
		}
	}

	It("rejects values other than ipv4 and ipv6, duplicates and more than two", func() {
		for _, families := range [][]string{{"ipv5"}, {"IPv4"}, {"all"}, {""}, {"ipv4", "ipv4"}, {"ipv4", "ipv6", "ipv4"}} {
			err := k8sClient.Create(ctx, newMonitor("family-invalid", "127.0.0.1:1", families, "http://127.0.0.1:1"))
			Expect(apierrors.IsInvalid(err)).To(BeTrue(), "ipFamily %q: %v", families, err)
			Expect(err.Error()).To(ContainSubstring("spec.ipFamily"))
		}
	})

	It("accepts no families, ipv4, ipv6 and both in either order", func() {
		for _, families := range [][]string{nil, {}, {"ipv4"}, {"ipv6"}, {"ipv4", "ipv6"}, {"ipv6", "ipv4"}} {
			m := newMonitor("family-valid", "127.0.0.1:1", families, "http://127.0.0.1:1")
			Expect(k8sClient.Create(ctx, m)).To(Succeed(), "ipFamily %q", families)
			var got monitoringv1alpha1.EndpointMonitor
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: m.Name, Namespace: m.Namespace}, &got)).To(Succeed())
			if len(families) == 0 {
				Expect(got.Spec.IpFamily).To(BeEmpty())
			} else {
				Expect([]string(got.Spec.IpFamily)).To(Equal(families))
			}
			Expect(k8sClient.Delete(ctx, m)).To(Succeed())
		}
	})

	It("lets a second apply owner replace the families, not merge with the first", func() {
		apply := func(owner string, families []any, force bool) error {
			u := &unstructured.Unstructured{Object: map[string]any{
				"apiVersion": "monitoring.licious.app/v1alpha1",
				"kind":       "EndpointMonitor",
				"metadata":   map[string]any{"name": "family-owners", "namespace": "default"},
				"spec": map[string]any{
					"driver": "tcp", "endpoint": "127.0.0.1:1", "checkInterval": int64(60), "notify": map[string]any{},
					"ipFamily": families,
				},
			}}
			opts := []client.ApplyOption{client.FieldOwner(owner)}
			if force {
				opts = append(opts, client.ForceOwnership)
			}
			return k8sClient.Apply(ctx, client.ApplyConfigurationFromUnstructured(u), opts...)
		}
		Expect(apply("first", []any{"ipv4"}, false)).To(Succeed())
		DeferCleanup(func() {
			Expect(k8sClient.Delete(ctx, &monitoringv1alpha1.EndpointMonitor{ObjectMeta: metav1.ObjectMeta{Name: "family-owners", Namespace: "default"}})).To(Succeed())
		})
		err := apply("second", []any{"ipv6"}, false)
		Expect(apierrors.IsConflict(err)).To(BeTrue(), "%v", err)
		Expect(apply("second", []any{"ipv6"}, true)).To(Succeed())
		var got monitoringv1alpha1.EndpointMonitor
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "family-owners", Namespace: "default"}, &got)).To(Succeed())
		Expect([]string(got.Spec.IpFamily)).To(Equal([]string{"ipv6"}))
	})

	It("checks over the listed families", func() {
		ln, err := net.Listen("tcp4", "127.0.0.1:0")
		Expect(err).NotTo(HaveOccurred())
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
		hook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusNoContent)
		}))
		defer hook.Close()

		r := &EndpointMonitorReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), Recorder: events.NewFakeRecorder(10)}
		for i, tc := range []struct {
			families []string
			want     string
		}{
			{nil, "success"},
			{[]string{"ipv4"}, "success"},
			{[]string{"ipv6"}, "failure"},
			{[]string{"ipv4", "ipv6"}, "failure"},
			{[]string{"ipv6", "ipv4"}, "failure"},
		} {
			m := newMonitor(fmt.Sprintf("family-%d", i), ln.Addr().String(), tc.families, hook.URL)
			Expect(k8sClient.Create(ctx, m)).To(Succeed())
			nn := types.NamespacedName{Name: m.Name, Namespace: m.Namespace}
			_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: nn})
			Expect(err).NotTo(HaveOccurred())
			var got monitoringv1alpha1.EndpointMonitor
			Expect(k8sClient.Get(ctx, nn, &got)).To(Succeed())
			Expect(got.Status.LastStatus).To(Equal(tc.want), "ipFamily %q", tc.families)
			Expect(k8sClient.Delete(ctx, m)).To(Succeed())
		}
	})
})
