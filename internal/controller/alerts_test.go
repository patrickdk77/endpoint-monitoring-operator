package controller

import (
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	monitoringv1alpha1 "github.com/patrickdk77/endpoint-monitoring-operator/api/v1alpha1"
)

const closedEndpoint = "127.0.0.1:1"

type alertHook struct {
	*httptest.Server
	code   atomic.Int32
	hits   atomic.Int32
	mu     sync.Mutex
	bodies []string
}

func newAlertHook(code int) *alertHook {
	h := &alertHook{}
	h.code.Store(int32(code))
	h.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		h.mu.Lock()
		h.bodies = append(h.bodies, string(body))
		h.mu.Unlock()
		h.hits.Add(1)
		w.WriteHeader(int(h.code.Load()))
	}))
	DeferCleanup(h.Close)
	return h
}

func (h *alertHook) lastBody() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.bodies) == 0 {
		return ""
	}
	return h.bodies[len(h.bodies)-1]
}

func templateHook(code int, alertOn ...string) (*alertHook, *monitoringv1alpha1.WebhookConfig) {
	h := newAlertHook(code)
	return h, &monitoringv1alpha1.WebhookConfig{
		Enabled:    true,
		WebhookURL: h.URL,
		Contents:   "{{.LastStatus}}|{{.Status}}|{{.LastStatusChange}}|{{.StatusTime}}",
		AlertOn:    alertOn,
	}
}

var _ = Describe("EndpointMonitor alerts", func() {
	var (
		endpoint string
		recorder *events.FakeRecorder
		r        *EndpointMonitorReconciler
	)

	BeforeEach(func() {
		ln, err := net.Listen("tcp4", "127.0.0.1:0")
		Expect(err).NotTo(HaveOccurred())
		go func() {
			for {
				c, err := ln.Accept()
				if err != nil {
					return
				}
				c.Close()
			}
		}()
		DeferCleanup(func() { ln.Close() })
		endpoint = ln.Addr().String()
		recorder = events.NewFakeRecorder(20)
		r = &EndpointMonitorReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), Recorder: recorder}
	})

	create := func(name string, notify monitoringv1alpha1.NotifyConfig) types.NamespacedName {
		m := &monitoringv1alpha1.EndpointMonitor{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
			Spec: monitoringv1alpha1.EndpointMonitorSpec{
				Driver:        "tcp",
				Endpoint:      endpoint,
				CheckInterval: 60,
				Notify:        notify,
			},
		}
		Expect(k8sClient.Create(ctx, m)).To(Succeed())
		DeferCleanup(func() { Expect(k8sClient.Delete(ctx, m)).To(Succeed()) })
		return types.NamespacedName{Name: name, Namespace: "default"}
	}

	get := func(nn types.NamespacedName) monitoringv1alpha1.EndpointMonitor {
		var m monitoringv1alpha1.EndpointMonitor
		Expect(k8sClient.Get(ctx, nn, &m)).To(Succeed())
		return m
	}

	setEndpoint := func(nn types.NamespacedName, ep string) {
		m := get(nn)
		m.Spec.Endpoint = ep
		Expect(k8sClient.Update(ctx, &m)).To(Succeed())
	}

	check := func(nn types.NamespacedName) {
		m := get(nn)
		if !m.Status.LastCheckedTime.IsZero() {
			m.Status.LastCheckedTime = metav1.NewTime(time.Now().Add(-2 * time.Minute))
			Expect(k8sClient.Status().Update(ctx, &m)).To(Succeed())
		}
		res, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: nn})
		Expect(err).NotTo(HaveOccurred())
		Expect(res.RequeueAfter).To(Equal(60 * time.Second))
	}

	notified := func(nn types.NamespacedName) *string {
		return get(nn).Status.LastNotifiedStatus
	}

	alertFailed := func(alert, notifiers string) string {
		return "Warning AlertFailed " + alert + " alert was not sent by " + notifiers + "; the operator log has the error"
	}

	Context("when a notifier fails", func() {
		It("still sends the change alert when the status alert fails", func() {
			failing, changes := newAlertHook(http.StatusInternalServerError), newAlertHook(http.StatusNoContent)
			nn := create("status-alert-fails", monitoringv1alpha1.NotifyConfig{
				Slack:   &monitoringv1alpha1.SlackConfig{Enabled: true, WebhookURL: failing.URL, AlertOn: []string{"success"}},
				Discord: &monitoringv1alpha1.DiscordConfig{Enabled: true, WebhookURL: changes.URL, AlertOn: []string{"change"}},
			})
			check(nn)
			Expect(failing.hits.Load()).To(BeEquivalentTo(1))
			Expect(changes.hits.Load()).To(BeEquivalentTo(1))
			Expect(recorder.Events).To(Receive(Equal(alertFailed("success", "slack"))))
			Expect(recorder.Events).To(BeEmpty())
			Expect(notified(nn)).To(HaveValue(Equal("success")))
		})

		It("still sends the status alert when the change alert fails", func() {
			failing, statuses := newAlertHook(http.StatusInternalServerError), newAlertHook(http.StatusNoContent)
			nn := create("change-alert-fails", monitoringv1alpha1.NotifyConfig{
				Slack:   &monitoringv1alpha1.SlackConfig{Enabled: true, WebhookURL: failing.URL, AlertOn: []string{"change"}},
				Discord: &monitoringv1alpha1.DiscordConfig{Enabled: true, WebhookURL: statuses.URL, AlertOn: []string{"success"}},
			})
			check(nn)
			Expect(statuses.hits.Load()).To(BeEquivalentTo(1))
			Expect(recorder.Events).To(Receive(Equal(alertFailed("change", "slack"))))
			Expect(recorder.Events).To(BeEmpty())
			m := get(nn)
			Expect(m.Status.LastStatus).To(Equal("success"))
			Expect(m.Status.LastNotifiedStatus).To(HaveValue(BeEmpty()))
		})

		It("records one event per failed alert", func() {
			failing := newAlertHook(http.StatusInternalServerError)
			nn := create("both-alerts-fail", monitoringv1alpha1.NotifyConfig{
				Discord: &monitoringv1alpha1.DiscordConfig{Enabled: true, WebhookURL: failing.URL, AlertOn: []string{"success", "change"}},
			})
			check(nn)
			Expect(failing.hits.Load()).To(BeEquivalentTo(2))
			Expect(recorder.Events).To(Receive(Equal(alertFailed("success", "discord"))))
			Expect(recorder.Events).To(Receive(Equal(alertFailed("change", "discord"))))
			Expect(recorder.Events).To(BeEmpty())
		})

		It("names every failed notifier without the error text", func() {
			secretPath := "/services/T000/B000/do-not-leak"
			nn := create("event-hides-urls", monitoringv1alpha1.NotifyConfig{
				Slack:   &monitoringv1alpha1.SlackConfig{Enabled: true, WebhookURL: "http://" + closedEndpoint + secretPath, AlertOn: []string{"success"}},
				Discord: &monitoringv1alpha1.DiscordConfig{Enabled: true, WebhookURL: "http://" + closedEndpoint + secretPath, AlertOn: []string{"success"}},
				Webhook: &monitoringv1alpha1.WebhookConfig{Enabled: true, WebhookURL: "http://" + closedEndpoint + secretPath, AlertOn: []string{"success"}},
			})
			check(nn)
			var event string
			Expect(recorder.Events).To(Receive(&event))
			Expect(event).To(Equal(alertFailed("success", "slack, discord, webhook")))
			Expect(event).NotTo(ContainSubstring("do-not-leak"))
			Expect(recorder.Events).To(BeEmpty())
		})
	})

	It("retries a change alert until it is delivered, then stops", func() {
		changes := newAlertHook(http.StatusInternalServerError)
		nn := create("change-retried", monitoringv1alpha1.NotifyConfig{
			Discord: &monitoringv1alpha1.DiscordConfig{Enabled: true, WebhookURL: changes.URL, AlertOn: []string{"change"}},
		})
		check(nn)
		Expect(changes.hits.Load()).To(BeEquivalentTo(1))
		Expect(recorder.Events).To(Receive(Equal(alertFailed("change", "discord"))))
		Expect(notified(nn)).To(HaveValue(BeEmpty()))

		check(nn)
		Expect(changes.hits.Load()).To(BeEquivalentTo(2))
		Expect(recorder.Events).To(Receive(Equal(alertFailed("change", "discord"))))

		changes.code.Store(http.StatusNoContent)
		check(nn)
		Expect(changes.hits.Load()).To(BeEquivalentTo(3))
		Expect(notified(nn)).To(HaveValue(Equal("success")))

		check(nn)
		Expect(changes.hits.Load()).To(BeEquivalentTo(3))
		Expect(recorder.Events).To(BeEmpty())
	})

	It("announces each change once", func() {
		changes, hook := templateHook(http.StatusOK, "change")
		nn := create("each-change-once", monitoringv1alpha1.NotifyConfig{Webhook: hook})
		check(nn)
		Expect(changes.hits.Load()).To(BeEquivalentTo(1))
		Expect(changes.lastBody()).To(Equal("|success||"))

		check(nn)
		Expect(changes.hits.Load()).To(BeEquivalentTo(1))

		setEndpoint(nn, closedEndpoint)
		check(nn)
		Expect(changes.hits.Load()).To(BeEquivalentTo(2))
		Expect(changes.lastBody()).To(MatchRegexp(`^success\|failure\|\d{4}-\d\d-\d\d [^|]+\|\d+(h|m|s)`))
		Expect(notified(nn)).To(HaveValue(Equal("failure")))

		check(nn)
		Expect(changes.hits.Load()).To(BeEquivalentTo(2))
		Expect(recorder.Events).To(BeEmpty())
	})

	It("keeps the status alert on the checked status while a change alert is pending", func() {
		failing := newAlertHook(http.StatusInternalServerError)
		statuses, hook := templateHook(http.StatusOK, "success")
		nn := create("pending-change", monitoringv1alpha1.NotifyConfig{
			Discord: &monitoringv1alpha1.DiscordConfig{Enabled: true, WebhookURL: failing.URL, AlertOn: []string{"change"}},
			Webhook: hook,
		})
		check(nn)
		Expect(statuses.lastBody()).To(Equal("|success||"))
		check(nn)
		Expect(statuses.lastBody()).To(MatchRegexp(`^success\|success\|\d{4}-`))
		Expect(notified(nn)).To(HaveValue(BeEmpty()))
	})

	It("describes the announced status in a retried change alert", func() {
		changes, hook := templateHook(http.StatusOK, "change")
		nn := create("retry-period", monitoringv1alpha1.NotifyConfig{Webhook: hook})
		check(nn)

		m := get(nn)
		upSince := metav1.NewTime(time.Now().Add(-72 * time.Hour).Truncate(time.Second))
		m.Status.LastStatusChange = upSince
		m.Status.LastNotifiedStatusChange = upSince
		Expect(k8sClient.Status().Update(ctx, &m)).To(Succeed())

		changes.code.Store(http.StatusInternalServerError)
		setEndpoint(nn, closedEndpoint)
		check(nn)
		Expect(recorder.Events).To(Receive(Equal(alertFailed("change", "webhook"))))
		firstTry := changes.lastBody()
		Expect(firstTry).To(MatchRegexp(`^success\|failure\|[^|]+\|72h0m\d+s$`))

		m = get(nn)
		m.Status.LastStatusChange = metav1.NewTime(m.Status.LastStatusChange.Add(-10 * time.Minute))
		m.Status.LastNotifiedStatusChange = metav1.NewTime(upSince.Add(-10 * time.Minute))
		Expect(k8sClient.Status().Update(ctx, &m)).To(Succeed())
		changes.code.Store(http.StatusOK)
		check(nn)
		retry := changes.lastBody()
		Expect(retry).To(MatchRegexp(`^success\|failure\|[^|]+\|72h0m\d+s$`))
		Expect(strings.Split(retry, "|")[3]).To(Equal(strings.Split(firstTry, "|")[3]), "the announced status lasted until the failure began, not until the retry")
		m = get(nn)
		Expect(m.Status.LastNotifiedStatus).To(HaveValue(Equal("failure")))
		Expect(m.Status.LastNotifiedStatusChange).To(Equal(m.Status.LastStatusChange))
	})

	It("does not announce a flap whose change alert failed", func() {
		changes := newAlertHook(http.StatusNoContent)
		nn := create("flap", monitoringv1alpha1.NotifyConfig{
			Discord: &monitoringv1alpha1.DiscordConfig{Enabled: true, WebhookURL: changes.URL, AlertOn: []string{"change"}},
		})
		check(nn)
		Expect(changes.hits.Load()).To(BeEquivalentTo(1))

		changes.code.Store(http.StatusInternalServerError)
		setEndpoint(nn, closedEndpoint)
		check(nn)
		Expect(changes.hits.Load()).To(BeEquivalentTo(2))
		Expect(recorder.Events).To(Receive(Equal(alertFailed("change", "discord"))))

		setEndpoint(nn, endpoint)
		check(nn)
		Expect(changes.hits.Load()).To(BeEquivalentTo(2))
		Expect(notified(nn)).To(HaveValue(Equal("success")))

		changes.code.Store(http.StatusNoContent)
		setEndpoint(nn, closedEndpoint)
		check(nn)
		Expect(changes.hits.Load()).To(BeEquivalentTo(3))
		Expect(notified(nn)).To(HaveValue(Equal("failure")))
		Expect(recorder.Events).To(BeEmpty())
	})

	It("announces the status again after the status is cleared", func() {
		changes := newAlertHook(http.StatusNoContent)
		nn := create("status-cleared", monitoringv1alpha1.NotifyConfig{
			Discord: &monitoringv1alpha1.DiscordConfig{Enabled: true, WebhookURL: changes.URL, AlertOn: []string{"change"}},
		})
		check(nn)
		m := get(nn)
		m.Status = monitoringv1alpha1.EndpointMonitorStatus{}
		Expect(k8sClient.Status().Update(ctx, &m)).To(Succeed())
		check(nn)
		Expect(changes.hits.Load()).To(BeEquivalentTo(2))
	})

	Context("on a monitor stored before lastNotifiedStatus existed", func() {
		age := func(nn types.NamespacedName, lastStatus string) metav1.Time {
			m := get(nn)
			since := metav1.NewTime(time.Now().Add(-time.Hour).Truncate(time.Second))
			m.Status.LastStatus = lastStatus
			m.Status.LastStatusChange = since
			m.Status.LastCheckedTime = metav1.NewTime(time.Now().Add(-2 * time.Minute))
			m.Status.LastNotifiedStatus = nil
			m.Status.LastNotifiedStatusChange = metav1.Time{}
			Expect(k8sClient.Status().Update(ctx, &m)).To(Succeed())
			return since
		}

		It("does not announce the status it already had", func() {
			changes := newAlertHook(http.StatusNoContent)
			nn := create("upgraded-same", monitoringv1alpha1.NotifyConfig{
				Discord: &monitoringv1alpha1.DiscordConfig{Enabled: true, WebhookURL: changes.URL, AlertOn: []string{"change"}},
			})
			since := age(nn, "success")
			check(nn)
			Expect(changes.hits.Load()).To(BeZero())
			m := get(nn)
			Expect(m.Status.LastNotifiedStatus).To(HaveValue(Equal("success")))
			Expect(m.Status.LastNotifiedStatusChange.Equal(&since)).To(BeTrue())
		})

		It("still announces a real change", func() {
			changes, hook := templateHook(http.StatusOK, "change")
			nn := create("upgraded-changed", monitoringv1alpha1.NotifyConfig{Webhook: hook})
			age(nn, "failure")
			check(nn)
			Expect(changes.hits.Load()).To(BeEquivalentTo(1))
			Expect(changes.lastBody()).To(MatchRegexp(`^failure\|success\|[^|]+\|1h0m\d+s$`))
			Expect(notified(nn)).To(HaveValue(Equal("success")))
		})
	})
})
