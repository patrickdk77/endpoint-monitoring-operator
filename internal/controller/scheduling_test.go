package controller

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/config"
	"sigs.k8s.io/controller-runtime/pkg/metrics"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	monitoringv1alpha1 "github.com/patrickdk77/endpoint-monitoring-operator/api/v1alpha1"
)

func reconcilesCounted(controllerName string) float64 {
	families, err := metrics.Registry.Gather()
	Expect(err).NotTo(HaveOccurred())
	var total float64
	for _, f := range families {
		if f.GetName() != "controller_runtime_reconcile_total" {
			continue
		}
		for _, m := range f.GetMetric() {
			for _, l := range m.GetLabel() {
				if l.GetName() == "controller" && l.GetValue() == controllerName {
					total += m.GetCounter().GetValue()
				}
			}
		}
	}
	return total
}

// countingTCP accepts TCP connections and counts them.
func countingTCP() (string, *atomic.Int32) {
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	Expect(err).NotTo(HaveOccurred())
	DeferCleanup(func() { ln.Close() })
	var n atomic.Int32
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			n.Add(1)
			c.Close()
		}
	}()
	return ln.Addr().String(), &n
}

// startManager runs the reconciler in a real manager until the spec ends.
func startManager(workers int) {
	skip := true
	mgr, err := ctrl.NewManager(cfg, ctrl.Options{
		Scheme:                 scheme.Scheme,
		Metrics:                metricsserver.Options{BindAddress: "0"},
		HealthProbeBindAddress: "0",
		Controller:             config.Controller{SkipNameValidation: &skip},
	})
	Expect(err).NotTo(HaveOccurred())
	Expect((&EndpointMonitorReconciler{
		Client:                  mgr.GetClient(),
		Scheme:                  mgr.GetScheme(),
		Recorder:                events.NewFakeRecorder(100),
		MaxConcurrentReconciles: workers,
	}).SetupWithManager(mgr)).To(Succeed())
	mctx, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- mgr.Start(mctx) }()
	DeferCleanup(func() {
		cancel()
		Eventually(done, 10*time.Second).Should(Receive(BeNil()))
	})
	Expect(mgr.GetCache().WaitForCacheSync(mctx)).To(BeTrue())
}

var _ = Describe("EndpointMonitor scheduling", func() {
	newHook := func() string {
		hook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusNoContent)
		}))
		DeferCleanup(hook.Close)
		return hook.URL
	}

	create := func(name string, spec monitoringv1alpha1.EndpointMonitorSpec) types.NamespacedName {
		m := &monitoringv1alpha1.EndpointMonitor{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"}, Spec: spec}
		Expect(k8sClient.Create(ctx, m)).To(Succeed())
		DeferCleanup(func() { Expect(k8sClient.Delete(ctx, m)).To(Succeed()) })
		return types.NamespacedName{Name: name, Namespace: "default"}
	}

	get := func(nn types.NamespacedName) monitoringv1alpha1.EndpointMonitor {
		var m monitoringv1alpha1.EndpointMonitor
		Expect(k8sClient.Get(ctx, nn, &m)).To(Succeed())
		return m
	}

	edit := func(nn types.NamespacedName, mutate func(*monitoringv1alpha1.EndpointMonitorSpec)) {
		m := get(nn)
		mutate(&m.Spec)
		Expect(k8sClient.Update(ctx, &m)).To(Succeed())
	}

	tcpSpec := func(endpoint, hook string) monitoringv1alpha1.EndpointMonitorSpec {
		return monitoringv1alpha1.EndpointMonitorSpec{
			Driver: "tcp", Endpoint: endpoint, CheckInterval: 60,
			Notify: monitoringv1alpha1.NotifyConfig{Discord: &monitoringv1alpha1.DiscordConfig{Enabled: true, WebhookURL: hook}},
		}
	}

	Context("with the reconciler called directly", func() {
		var (
			recorder *events.FakeRecorder
			r        *EndpointMonitorReconciler
		)
		BeforeEach(func() {
			recorder = events.NewFakeRecorder(10)
			r = &EndpointMonitorReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), Recorder: recorder}
		})
		reconcileOnce := func(nn types.NamespacedName) reconcile.Result {
			res, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: nn})
			Expect(err).NotTo(HaveOccurred())
			return res
		}

		It("checks again right away when the spec changes", func() {
			endpoint, probes := countingTCP()
			nn := create("spec-change", tcpSpec(endpoint, newHook()))

			reconcileOnce(nn)
			Eventually(probes.Load).Should(BeEquivalentTo(1))
			m := get(nn)
			Expect(m.Status.ObservedGeneration).To(Equal(m.Generation))

			res := reconcileOnce(nn)
			Expect(res.RequeueAfter).To(And(BeNumerically(">", 0), BeNumerically("<=", 60*time.Second)))
			Consistently(probes.Load, 200*time.Millisecond).Should(BeEquivalentTo(1))

			edit(nn, func(s *monitoringv1alpha1.EndpointMonitorSpec) { s.CheckInterval = 120 })
			res = reconcileOnce(nn)
			Expect(res.RequeueAfter).To(Equal(120 * time.Second))
			Eventually(probes.Load).Should(BeEquivalentTo(2))
			m = get(nn)
			Expect(m.Generation).To(BeEquivalentTo(2))
			Expect(m.Status.ObservedGeneration).To(Equal(m.Generation))

			res = reconcileOnce(nn)
			Expect(res.RequeueAfter).To(And(BeNumerically(">", 60*time.Second), BeNumerically("<=", 120*time.Second)))
			Consistently(probes.Load, 200*time.Millisecond).Should(BeEquivalentTo(2))
		})

		It("retries an edit it cannot check and checks the fix right away", func() {
			endpoint, probes := countingTCP()
			nn := create("spec-broken", tcpSpec(endpoint, newHook()))
			reconcileOnce(nn)
			Eventually(probes.Load).Should(BeEquivalentTo(1))

			edit(nn, func(s *monitoringv1alpha1.EndpointMonitorSpec) { s.Driver = "http-json" })
			res := reconcileOnce(nn)
			Expect(res.RequeueAfter).To(Equal(30 * time.Second))
			Expect(recorder.Events).To(Receive(HavePrefix("Warning CannotCheck ")))
			Consistently(probes.Load, 200*time.Millisecond).Should(BeEquivalentTo(1))
			Expect(get(nn).Status.ObservedGeneration).To(BeEquivalentTo(1))

			res = reconcileOnce(nn)
			Expect(res.RequeueAfter).To(Equal(30 * time.Second))
			Expect(recorder.Events).To(Receive(HavePrefix("Warning CannotCheck ")))

			edit(nn, func(s *monitoringv1alpha1.EndpointMonitorSpec) { s.Driver = "tcp" })
			res = reconcileOnce(nn)
			Expect(res.RequeueAfter).To(Equal(60 * time.Second))
			Eventually(probes.Load).Should(BeEquivalentTo(2))
			m := get(nn)
			Expect(m.Generation).To(BeEquivalentTo(3))
			Expect(m.Status.ObservedGeneration).To(Equal(m.Generation))
			Expect(recorder.Events).To(BeEmpty())
		})

		It("keeps the interval when observedGeneration was never stored", func() {
			endpoint, probes := countingTCP()
			nn := create("no-observed-generation", tcpSpec(endpoint, newHook()))
			reconcileOnce(nn)
			Eventually(probes.Load).Should(BeEquivalentTo(1))

			m := get(nn)
			m.Status.ObservedGeneration = 0
			Expect(k8sClient.Status().Update(ctx, &m)).To(Succeed())
			for i := 0; i < 3; i++ {
				res := reconcileOnce(nn)
				Expect(res.RequeueAfter).To(And(BeNumerically(">", 0), BeNumerically("<=", 60*time.Second)))
			}
			Consistently(probes.Load, 200*time.Millisecond).Should(BeEquivalentTo(1))
		})
	})

	Context("in a running manager", func() {
		It("checks monitors in parallel, ignores its own status writes and picks up spec edits", func() {
			var inFlight, maxInFlight atomic.Int32
			slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				n := inFlight.Add(1)
				defer inFlight.Add(-1)
				for {
					m := maxInFlight.Load()
					if n <= m || maxInFlight.CompareAndSwap(m, n) {
						break
					}
				}
				time.Sleep(time.Second)
			}))
			DeferCleanup(slow.Close)

			startManager(2)
			before := reconcilesCounted("endpointmonitor")
			reconciles := func() float64 { return reconcilesCounted("endpointmonitor") - before }

			hook := newHook()
			var monitors []types.NamespacedName
			for _, name := range []string{"parallel-a", "parallel-b"} {
				monitors = append(monitors, create(name, monitoringv1alpha1.EndpointMonitorSpec{
					Driver: "http", Endpoint: slow.URL, CheckInterval: 60,
					Notify: monitoringv1alpha1.NotifyConfig{Discord: &monitoringv1alpha1.DiscordConfig{Enabled: true, WebhookURL: hook}},
				}))
			}
			for _, nn := range monitors {
				Eventually(func() bool {
					m := get(nn)
					return m.Status.LastCheckedTime.IsZero()
				}, 10*time.Second).Should(BeFalse())
			}
			Expect(maxInFlight.Load()).To(BeEquivalentTo(2))
			Eventually(reconciles, 5*time.Second).Should(BeEquivalentTo(2))
			Consistently(reconciles, 2*time.Second).Should(BeEquivalentTo(2))

			edit(monitors[0], func(s *monitoringv1alpha1.EndpointMonitorSpec) { s.CheckInterval = 120 })
			Eventually(func() int64 { return get(monitors[0]).Status.ObservedGeneration }, 10*time.Second).Should(BeEquivalentTo(2))
			Eventually(reconciles, 5*time.Second).Should(BeEquivalentTo(3))
			Consistently(reconciles, time.Second).Should(BeEquivalentTo(3))
		})

		It("keeps checking monitors that existed before it started", func() {
			endpoint, probes := countingTCP()
			hook := newHook()
			old := tcpSpec(endpoint, hook)
			old.CheckInterval = 1
			create("starved-old", old)

			startManager(1)
			slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				time.Sleep(1500 * time.Millisecond)
			}))
			DeferCleanup(slow.Close)
			Eventually(probes.Load, 5*time.Second).Should(BeEquivalentTo(1))
			for _, name := range []string{"starving-b1", "starving-b2"} {
				create(name, monitoringv1alpha1.EndpointMonitorSpec{
					Driver: "http", Endpoint: slow.URL, CheckInterval: 1,
					Notify: monitoringv1alpha1.NotifyConfig{Discord: &monitoringv1alpha1.DiscordConfig{Enabled: true, WebhookURL: hook}},
				})
			}
			start := probes.Load()
			time.Sleep(8 * time.Second)
			Expect(probes.Load()-start).To(BeNumerically(">=", 1), "the monitor listed at startup was not checked while newer ones kept the worker busy")
		})
	})
})
