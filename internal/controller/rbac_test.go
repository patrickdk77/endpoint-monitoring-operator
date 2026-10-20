package controller

import (
	"bufio"
	"bytes"
	"context"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	authorizationv1 "k8s.io/api/authorization/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/yaml"

	monitoringv1alpha1 "github.com/patrickdk77/endpoint-monitoring-operator/api/v1alpha1"
	"github.com/patrickdk77/endpoint-monitoring-operator/internal/notifier"
)

var _ = Describe("EndpointMonitor under the operator's own RBAC", func() {
	It("checks a monitor that reads a Secret with only the permissions in config/rbac/role.yaml", func() {
		old := notifier.Timeout
		notifier.Timeout = 3 * time.Second
		DeferCleanup(func() { notifier.Timeout = old })

		raw, err := os.ReadFile(filepath.Join("..", "..", "config", "rbac", "role.yaml"))
		Expect(err).NotTo(HaveOccurred())
		docs := utilyaml.NewYAMLReader(bufio.NewReader(bytes.NewReader(raw)))
		doc, err := docs.Read()
		Expect(err).NotTo(HaveOccurred())
		_, err = docs.Read()
		Expect(err).To(MatchError(io.EOF), "role.yaml holds more than one document")
		var role rbacv1.ClusterRole
		Expect(yaml.Unmarshal(doc, &role)).To(Succeed())
		Expect(role.AggregationRule).To(BeNil())
		Expect(role.Labels).NotTo(HaveKey(HavePrefix("rbac.authorization.k8s.io/aggregate-to-")))
		rawBinding, err := os.ReadFile(filepath.Join("..", "..", "config", "rbac", "role_binding.yaml"))
		Expect(err).NotTo(HaveOccurred())
		var deployed rbacv1.ClusterRoleBinding
		Expect(yaml.Unmarshal(rawBinding, &deployed)).To(Succeed())
		Expect(deployed.RoleRef).To(Equal(rbacv1.RoleRef{APIGroup: "rbac.authorization.k8s.io", Kind: role.Kind, Name: role.Name}))
		Expect(role.Rules).To(ConsistOf(
			rbacv1.PolicyRule{APIGroups: []string{"monitoring.licious.app"}, Resources: []string{"endpointmonitors"}, Verbs: []string{"get", "list", "watch"}},
			rbacv1.PolicyRule{APIGroups: []string{"monitoring.licious.app"}, Resources: []string{"endpointmonitors/status"}, Verbs: []string{"update"}},
			rbacv1.PolicyRule{APIGroups: []string{""}, Resources: []string{"secrets"}, Verbs: []string{"get"}},
			rbacv1.PolicyRule{APIGroups: []string{"events.k8s.io"}, Resources: []string{"events"}, Verbs: []string{"create", "patch"}},
		))
		role.Name = "rbac-test-manager-role"
		Expect(k8sClient.Create(ctx, &role)).To(Succeed())
		DeferCleanup(func() { Expect(k8sClient.Delete(ctx, &role)).To(Succeed()) })

		sa := &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: "rbac-test-operator", Namespace: "default"}}
		Expect(k8sClient.Create(ctx, sa)).To(Succeed())
		DeferCleanup(func() { Expect(k8sClient.Delete(ctx, sa)).To(Succeed()) })
		binding := &rbacv1.ClusterRoleBinding{
			ObjectMeta: metav1.ObjectMeta{Name: "rbac-test-operator"},
			RoleRef:    rbacv1.RoleRef{APIGroup: "rbac.authorization.k8s.io", Kind: "ClusterRole", Name: role.Name},
			Subjects:   []rbacv1.Subject{{Kind: "ServiceAccount", Name: sa.Name, Namespace: sa.Namespace}},
		}
		Expect(k8sClient.Create(ctx, binding)).To(Succeed())
		DeferCleanup(func() { Expect(k8sClient.Delete(ctx, binding)).To(Succeed()) })

		secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "rbac-test-redis", Namespace: "default"}, StringData: map[string]string{"password": "pw"}}
		Expect(k8sClient.Create(ctx, secret)).To(Succeed())
		DeferCleanup(func() { Expect(k8sClient.Delete(ctx, secret)).To(Succeed()) })

		saCfg := rest.CopyConfig(cfg)
		saCfg.Impersonate = rest.ImpersonationConfig{UserName: "system:serviceaccount:default:rbac-test-operator"}
		direct, err := client.New(saCfg, client.Options{Scheme: scheme.Scheme})
		Expect(err).NotTo(HaveOccurred())
		Eventually(func() error {
			return direct.Get(ctx, types.NamespacedName{Name: secret.Name, Namespace: secret.Namespace}, &corev1.Secret{})
		}).WithTimeout(10 * time.Second).Should(Succeed())
		err = direct.List(ctx, &corev1.SecretList{}, client.InNamespace("default"))
		Expect(apierrors.IsForbidden(err)).To(BeTrue(), "listing Secrets: %v", err)

		for _, want := range []struct {
			verb, group, resource, subresource string
			allowed                            bool
		}{
			{"get", "monitoring.licious.app", "endpointmonitors", "", true},
			{"list", "monitoring.licious.app", "endpointmonitors", "", true},
			{"watch", "monitoring.licious.app", "endpointmonitors", "", true},
			{"update", "monitoring.licious.app", "endpointmonitors", "status", true},
			{"get", "", "secrets", "", true},
			{"create", "events.k8s.io", "events", "", true},
			{"patch", "events.k8s.io", "events", "", true},
			{"list", "", "secrets", "", false},
			{"watch", "", "secrets", "", false},
			{"create", "monitoring.licious.app", "endpointmonitors", "", false},
			{"update", "monitoring.licious.app", "endpointmonitors", "", false},
			{"patch", "monitoring.licious.app", "endpointmonitors", "", false},
			{"delete", "monitoring.licious.app", "endpointmonitors", "", false},
			{"patch", "monitoring.licious.app", "endpointmonitors", "status", false},
			{"get", "monitoring.licious.app", "endpointmonitors", "status", false},
			{"update", "monitoring.licious.app", "endpointmonitors", "finalizers", false},
		} {
			review := &authorizationv1.SelfSubjectAccessReview{Spec: authorizationv1.SelfSubjectAccessReviewSpec{
				ResourceAttributes: &authorizationv1.ResourceAttributes{
					Namespace: "default", Verb: want.verb, Group: want.group, Resource: want.resource, Subresource: want.subresource,
				},
			}}
			Expect(direct.Create(ctx, review)).To(Succeed())
			Expect(review.Status.Allowed).To(Equal(want.allowed), "%s %s/%s %s", want.verb, want.group, want.resource, want.subresource)
		}

		mgr, err := ctrl.NewManager(saCfg, ctrl.Options{
			Scheme:                 scheme.Scheme,
			Client:                 ClientOptions(),
			Metrics:                metricsserver.Options{BindAddress: "0"},
			HealthProbeBindAddress: "0",
		})
		Expect(err).NotTo(HaveOccurred())
		mgrCtx, stop := context.WithCancel(ctx)
		DeferCleanup(stop)
		go func() {
			defer GinkgoRecover()
			Expect(mgr.Start(mgrCtx)).To(Succeed())
		}()
		_, err = mgr.GetCache().GetInformer(mgrCtx, &monitoringv1alpha1.EndpointMonitor{})
		Expect(err).NotTo(HaveOccurred())
		Expect(mgr.GetCache().WaitForCacheSync(mgrCtx)).To(BeTrue())

		hook := newAlertHook(http.StatusNoContent)
		m := &monitoringv1alpha1.EndpointMonitor{
			ObjectMeta: metav1.ObjectMeta{Name: "rbac-test-monitor", Namespace: "default"},
			Spec: monitoringv1alpha1.EndpointMonitorSpec{
				Driver:        "redis",
				Endpoint:      closedEndpoint,
				CheckInterval: 60,
				RedisCheck:    &monitoringv1alpha1.RedisCheck{SecretRef: monitoringv1alpha1.SecretRef{Name: secret.Name}},
				Notify:        monitoringv1alpha1.NotifyConfig{Discord: &monitoringv1alpha1.DiscordConfig{Enabled: true, WebhookURL: hook.URL}},
			},
		}
		Expect(k8sClient.Create(ctx, m)).To(Succeed())
		DeferCleanup(func() { Expect(k8sClient.Delete(ctx, m)).To(Succeed()) })

		recorder := events.NewFakeRecorder(10)
		r := &EndpointMonitorReconciler{Client: mgr.GetClient(), Scheme: mgr.GetScheme(), Recorder: recorder}
		nn := types.NamespacedName{Name: m.Name, Namespace: m.Namespace}
		Eventually(func(g Gomega) {
			_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: nn})
			g.Expect(err).NotTo(HaveOccurred())
			var got monitoringv1alpha1.EndpointMonitor
			g.Expect(k8sClient.Get(ctx, nn, &got)).To(Succeed())
			g.Expect(got.Status.LastStatus).To(Equal("failure"))
		}).WithTimeout(20 * time.Second).Should(Succeed())
		Expect(recorder.Events).To(BeEmpty())
		Expect(hook.hits.Load()).To(BeEquivalentTo(1))
	})
})
