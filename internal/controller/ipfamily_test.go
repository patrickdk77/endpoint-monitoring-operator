package controller

import (
	"net"
	"net/http"
	"net/http/httptest"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	monitoringv1alpha1 "github.com/patrickdk77/endpoint-monitoring-operator/api/v1alpha1"
)

var _ = Describe("EndpointMonitor ipFamily", func() {
	newMonitor := func(name, endpoint, family, webhook string) *monitoringv1alpha1.EndpointMonitor {
		return &monitoringv1alpha1.EndpointMonitor{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
			Spec: monitoringv1alpha1.EndpointMonitorSpec{
				Driver:        "tcp",
				Endpoint:      endpoint,
				CheckInterval: 60,
				IpFamily:      family,
				Notify: monitoringv1alpha1.NotifyConfig{
					Discord: &monitoringv1alpha1.DiscordConfig{Enabled: true, WebhookURL: webhook},
				},
			},
		}
	}

	It("rejects a value other than ipv4, ipv6 or all", func() {
		for _, family := range []string{"ipv5", "IPv4", "both", "any"} {
			err := k8sClient.Create(ctx, newMonitor("family-invalid", "127.0.0.1:1", family, "http://127.0.0.1:1"))
			Expect(apierrors.IsInvalid(err)).To(BeTrue(), "ipFamily %q: %v", family, err)
		}
	})

	It("accepts ipv4, ipv6, all and unset", func() {
		for _, family := range []string{"ipv4", "ipv6", "all", ""} {
			m := newMonitor("family-valid", "127.0.0.1:1", family, "http://127.0.0.1:1")
			Expect(k8sClient.Create(ctx, m)).To(Succeed(), "ipFamily %q", family)
			var got monitoringv1alpha1.EndpointMonitor
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: m.Name, Namespace: m.Namespace}, &got)).To(Succeed())
			Expect(got.Spec.IpFamily).To(Equal(family))
			Expect(k8sClient.Delete(ctx, m)).To(Succeed())
		}
	})

	It("checks over the configured family", func() {
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
		for family, want := range map[string]string{"ipv4": "success", "ipv6": "failure", "all": "failure"} {
			m := newMonitor("family-"+family, ln.Addr().String(), family, hook.URL)
			Expect(k8sClient.Create(ctx, m)).To(Succeed())
			nn := types.NamespacedName{Name: m.Name, Namespace: m.Namespace}
			_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: nn})
			Expect(err).NotTo(HaveOccurred())
			var got monitoringv1alpha1.EndpointMonitor
			Expect(k8sClient.Get(ctx, nn, &got)).To(Succeed())
			Expect(got.Status.LastStatus).To(Equal(want), "ipFamily %s", family)
			Expect(k8sClient.Delete(ctx, m)).To(Succeed())
		}
	})
})
