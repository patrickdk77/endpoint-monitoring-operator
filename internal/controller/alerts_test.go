package controller

import (
	"context"
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
	"sigs.k8s.io/controller-runtime/pkg/client"
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

// staleReads answers Gets for a frozen monitor from a snapshot, as a
// manager's cache can after a write it has not seen yet.
type staleReads struct {
	client.Client
	mu   sync.Mutex
	snap map[types.NamespacedName]*monitoringv1alpha1.EndpointMonitor
}

func (s *staleReads) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	if m, ok := obj.(*monitoringv1alpha1.EndpointMonitor); ok {
		s.mu.Lock()
		snap := s.snap[key]
		s.mu.Unlock()
		if snap != nil {
			snap.DeepCopyInto(m)
			return nil
		}
	}
	return s.Client.Get(ctx, key, obj, opts...)
}

func (s *staleReads) freeze(nn types.NamespacedName) {
	var m monitoringv1alpha1.EndpointMonitor
	Expect(s.Client.Get(context.Background(), nn, &m)).To(Succeed())
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.snap == nil {
		s.snap = map[types.NamespacedName]*monitoringv1alpha1.EndpointMonitor{}
	}
	s.snap[nn] = &m
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

	It("keeps the endpoint's password and query out of alerts", func() {
		h := newAlertHook(http.StatusNoContent)
		m := &monitoringv1alpha1.EndpointMonitor{
			ObjectMeta: metav1.ObjectMeta{Name: "endpoint-credentials", Namespace: "default"},
			Spec: monitoringv1alpha1.EndpointMonitorSpec{
				Driver:        "http",
				Endpoint:      "http://monitor:s3cret@" + endpoint + "/health?token=t0ken",
				CheckInterval: 60,
				Notify: monitoringv1alpha1.NotifyConfig{Webhook: &monitoringv1alpha1.WebhookConfig{
					Enabled: true, WebhookURL: h.URL, Contents: "{{.Endpoint}}|{{.AlertMessage}}", AlertOn: []string{"failure"},
				}},
			},
		}
		Expect(k8sClient.Create(ctx, m)).To(Succeed())
		DeferCleanup(func() { Expect(k8sClient.Delete(ctx, m)).To(Succeed()) })
		check(types.NamespacedName{Name: m.Name, Namespace: m.Namespace})
		Expect(h.hits.Load()).To(BeEquivalentTo(1))
		body := h.lastBody()
		Expect(body).To(HavePrefix("http://monitor:***@" + endpoint + "/health|http monitor for http://monitor:***@" + endpoint + "/health is unhealthy"))
		Expect(body).NotTo(ContainSubstring("s3cret"))
		Expect(body).NotTo(ContainSubstring("t0ken"))
	})

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

	Context("when a webhook fails in a way a later try may deliver", func() {
		sequence := func(codes ...int) *alertHook {
			h := &alertHook{}
			h.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				i := int(h.hits.Add(1)) - 1
				w.WriteHeader(codes[min(i, len(codes)-1)])
			}))
			DeferCleanup(h.Close)
			return h
		}
		webhook := func(h *alertHook, alertOn ...string) monitoringv1alpha1.NotifyConfig {
			return monitoringv1alpha1.NotifyConfig{Webhook: &monitoringv1alpha1.WebhookConfig{Enabled: true, WebhookURL: h.URL, AlertOn: alertOn}}
		}
		retried := func(name string, notify monitoringv1alpha1.NotifyConfig) types.NamespacedName {
			nn := create(name, notify)
			DeferCleanup(func() { r.Retries.Stop(nn) })
			check(nn)
			return nn
		}

		BeforeEach(func() {
			r.Retries = &AlertRetries{Tries: 5, Delay: 20 * time.Millisecond}
		})

		It("tries the defaults five times, 30 seconds apart", func() {
			a := NewAlertRetries()
			Expect(a.Tries).To(Equal(5))
			Expect(a.Delay).To(Equal(30 * time.Second))
		})

		It("resends a status alert until the webhook takes it, without an event", func() {
			h := sequence(http.StatusInternalServerError, http.StatusServiceUnavailable, http.StatusNoContent)
			retried("retry-delivers", webhook(h, "success"))
			Eventually(h.hits.Load).Should(BeEquivalentTo(3))
			Consistently(h.hits.Load, 150*time.Millisecond).Should(BeEquivalentTo(3))
			Expect(recorder.Events).To(BeEmpty())
		})

		It("retries a 429", func() {
			h := sequence(http.StatusTooManyRequests, http.StatusNoContent)
			retried("retry-429", webhook(h, "success"))
			Eventually(h.hits.Load).Should(BeEquivalentTo(2))
			Consistently(h.hits.Load, 150*time.Millisecond).Should(BeEquivalentTo(2))
			Expect(recorder.Events).To(BeEmpty())
		})

		It("gives up after five tries and records one event", func() {
			h := sequence(http.StatusInternalServerError)
			retried("retry-gives-up", webhook(h, "success"))
			Eventually(recorder.Events).Should(Receive(Equal(alertFailed("success", "webhook"))))
			Expect(h.hits.Load()).To(BeEquivalentTo(5))
			Consistently(h.hits.Load, 150*time.Millisecond).Should(BeEquivalentTo(5))
			Expect(recorder.Events).To(BeEmpty())
		})

		It("does not retry another 4xx and records the event at once", func() {
			h := sequence(http.StatusBadRequest)
			retried("no-retry-400", webhook(h, "success"))
			Expect(recorder.Events).To(Receive(Equal(alertFailed("success", "webhook"))))
			Consistently(h.hits.Load, 150*time.Millisecond).Should(BeEquivalentTo(1))
		})

		It("stops at a failure a later try will not fix", func() {
			h := sequence(http.StatusInternalServerError, http.StatusBadRequest)
			retried("retry-then-400", webhook(h, "success"))
			Eventually(recorder.Events).Should(Receive(Equal(alertFailed("success", "webhook"))))
			Expect(h.hits.Load()).To(BeEquivalentTo(2))
			Consistently(h.hits.Load, 150*time.Millisecond).Should(BeEquivalentTo(2))
		})

		It("retries a request that does not complete", func() {
			ln, err := net.Listen("tcp4", "127.0.0.1:0")
			Expect(err).NotTo(HaveOccurred())
			DeferCleanup(func() { ln.Close() })
			var accepted atomic.Int32
			go func() {
				for {
					c, err := ln.Accept()
					if err != nil {
						return
					}
					accepted.Add(1)
					c.Close()
				}
			}()
			retried("retry-unfinished", monitoringv1alpha1.NotifyConfig{Webhook: &monitoringv1alpha1.WebhookConfig{
				Enabled: true, WebhookURL: "http://" + ln.Addr().String() + "/", AlertOn: []string{"success"},
			}})
			Eventually(recorder.Events).Should(Receive(Equal(alertFailed("success", "webhook"))))
			Expect(accepted.Load()).To(BeNumerically(">=", 5))
		})

		It("records a change alert that a retry delivered, so the next check does not resend it", func() {
			h := sequence(http.StatusInternalServerError, http.StatusNoContent)
			nn := retried("retry-change", webhook(h, "change"))
			Eventually(func() *string { return notified(nn) }).Should(HaveValue(Equal("success")))
			Expect(h.hits.Load()).To(BeEquivalentTo(2))
			check(nn)
			Consistently(h.hits.Load, 150*time.Millisecond).Should(BeEquivalentTo(2))
			Expect(recorder.Events).To(BeEmpty())
		})

		It("leaves a change alert pending when another notifier failed too", func() {
			h := sequence(http.StatusInternalServerError, http.StatusNoContent)
			discord := newAlertHook(http.StatusInternalServerError)
			notify := webhook(h, "change")
			notify.Discord = &monitoringv1alpha1.DiscordConfig{Enabled: true, WebhookURL: discord.URL, AlertOn: []string{"change"}}
			nn := retried("retry-change-other-failed", notify)
			Expect(recorder.Events).To(Receive(Equal(alertFailed("change", "discord"))))
			Eventually(h.hits.Load).Should(BeEquivalentTo(2))
			Consistently(func() *string { return notified(nn) }, 150*time.Millisecond).Should(HaveValue(BeEmpty()))
			check(nn)
			Expect(h.hits.Load()).To(BeEquivalentTo(3))
			Expect(discord.hits.Load()).To(BeEquivalentTo(2))
		})

		It("stops a pending retry when the monitor is checked again", func() {
			r.Retries.Delay = time.Hour
			h := sequence(http.StatusInternalServerError, http.StatusNoContent)
			nn := retried("retry-stopped-by-check", webhook(h, "success"))
			Expect(h.hits.Load()).To(BeEquivalentTo(1))
			check(nn)
			Expect(h.hits.Load()).To(BeEquivalentTo(2))
			Expect(recorder.Events).To(Receive(Equal(alertFailed("success", "webhook"))))
			Expect(recorder.Events).To(BeEmpty())
			Expect(r.Retries.Stop(nn)).To(BeFalse())
		})

		It("drops a stale change alert after the status flaps back", func() {
			r.Retries.Delay = time.Hour
			h := sequence(http.StatusNoContent, http.StatusInternalServerError)
			nn := retried("retry-flap", webhook(h, "change"))
			Expect(h.hits.Load()).To(BeEquivalentTo(1))
			setEndpoint(nn, closedEndpoint)
			check(nn)
			Expect(h.hits.Load()).To(BeEquivalentTo(2))
			setEndpoint(nn, endpoint)
			check(nn)
			Expect(recorder.Events).To(Receive(Equal(alertFailed("change", "webhook"))))
			Expect(r.Retries.Stop(nn)).To(BeFalse())
			Expect(h.hits.Load()).To(BeEquivalentTo(2))
			Expect(notified(nn)).To(HaveValue(Equal("success")))
			Expect(recorder.Events).To(BeEmpty())
		})

		It("lets a try in flight finish before the next check reads the status", func() {
			started, release := make(chan struct{}), make(chan struct{})
			h := &alertHook{}
			h.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				switch h.hits.Add(1) {
				case 1:
					w.WriteHeader(http.StatusInternalServerError)
					return
				case 2:
					close(started)
					<-release
				}
				w.WriteHeader(http.StatusNoContent)
			}))
			DeferCleanup(h.Close)
			nn := retried("retry-in-flight", webhook(h, "change"))
			Eventually(started).Should(BeClosed())
			m := get(nn)
			m.Status.LastCheckedTime = metav1.NewTime(time.Now().Add(-2 * time.Minute))
			Expect(k8sClient.Status().Update(ctx, &m)).To(Succeed())
			time.AfterFunc(100*time.Millisecond, func() { close(release) })
			_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: nn})
			Expect(err).NotTo(HaveOccurred())
			Expect(h.hits.Load()).To(BeEquivalentTo(2))
			Expect(notified(nn)).To(HaveValue(Equal("success")))
		})

		It("records one event per failed alert while the webhook keeps failing", func() {
			r.Retries.Delay = 100 * time.Millisecond
			h := sequence(http.StatusInternalServerError)
			nn := retried("retry-keeps-failing", webhook(h, "success"))
			check(nn)
			Expect(recorder.Events).To(Receive(Equal(alertFailed("success", "webhook"))))
			check(nn)
			Expect(recorder.Events).To(Receive(Equal(alertFailed("success", "webhook"))))
			Eventually(recorder.Events, time.Second).Should(Receive(Equal(alertFailed("success", "webhook"))))
			Consistently(recorder.Events, 200*time.Millisecond).Should(BeEmpty())
		})

		It("reads the status a retry recorded past a stale cache", func() {
			started, release := make(chan struct{}), make(chan struct{})
			h := &alertHook{}
			h.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				switch h.hits.Add(1) {
				case 1:
					w.WriteHeader(http.StatusInternalServerError)
					return
				case 2:
					close(started)
					<-release
				}
				w.WriteHeader(http.StatusNoContent)
			}))
			DeferCleanup(h.Close)
			stale := &staleReads{Client: k8sClient}
			r.Client, r.APIReader = stale, k8sClient
			nn := retried("retry-stale-cache", webhook(h, "change"))
			Eventually(started).Should(BeClosed())
			m := get(nn)
			m.Status.LastCheckedTime = metav1.NewTime(time.Now().Add(-2 * time.Minute))
			Expect(k8sClient.Status().Update(ctx, &m)).To(Succeed())
			stale.freeze(nn)
			m = get(nn)
			m.Annotations = map[string]string{"touched": "after the snapshot"}
			Expect(k8sClient.Update(ctx, &m)).To(Succeed())
			time.AfterFunc(100*time.Millisecond, func() { close(release) })
			_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: nn})
			Expect(err).NotTo(HaveOccurred())
			Expect(h.hits.Load()).To(BeEquivalentTo(2))
			Expect(notified(nn)).To(HaveValue(Equal("success")))
		})

		It("reads past a stale cache after a retry recorded the change before the next check", func() {
			r.Retries.Delay = 200 * time.Millisecond
			h := sequence(http.StatusInternalServerError, http.StatusNoContent)
			stale := &staleReads{Client: k8sClient}
			r.Client, r.APIReader = stale, k8sClient
			nn := retried("retry-wrote-before-check", webhook(h, "change"))
			m := get(nn)
			m.Status.LastCheckedTime = metav1.NewTime(time.Now().Add(-2 * time.Minute))
			Expect(k8sClient.Status().Update(ctx, &m)).To(Succeed())
			stale.freeze(nn)
			Eventually(func() *string { return notified(nn) }).Should(HaveValue(Equal("success")))
			Eventually(func() bool {
				r.Retries.mu.Lock()
				defer r.Retries.mu.Unlock()
				return len(r.Retries.pending[nn]) == 0
			}).Should(BeTrue())
			_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: nn})
			Expect(err).NotTo(HaveOccurred())
			Expect(h.hits.Load()).To(BeEquivalentTo(2))
		})

		It("stops retrying a monitor that was deleted", func() {
			r.Retries.Delay = 500 * time.Millisecond
			h := sequence(http.StatusInternalServerError)
			m := &monitoringv1alpha1.EndpointMonitor{
				ObjectMeta: metav1.ObjectMeta{Name: "retry-deleted", Namespace: "default"},
				Spec:       monitoringv1alpha1.EndpointMonitorSpec{Driver: "tcp", Endpoint: endpoint, CheckInterval: 60, Notify: webhook(h, "success")},
			}
			Expect(k8sClient.Create(ctx, m)).To(Succeed())
			nn := types.NamespacedName{Name: m.Name, Namespace: m.Namespace}
			DeferCleanup(func() { r.Retries.Stop(nn) })
			check(nn)
			Expect(k8sClient.Delete(ctx, m)).To(Succeed())
			Consistently(h.hits.Load, time.Second).Should(BeEquivalentTo(1))
			Expect(recorder.Events).To(BeEmpty())
		})

		It("stops the pending retries of a deleted monitor when it is reconciled", func() {
			r.Retries.Delay = time.Hour
			h := sequence(http.StatusInternalServerError)
			m := &monitoringv1alpha1.EndpointMonitor{
				ObjectMeta: metav1.ObjectMeta{Name: "retry-deleted-reconciled", Namespace: "default"},
				Spec:       monitoringv1alpha1.EndpointMonitorSpec{Driver: "tcp", Endpoint: endpoint, CheckInterval: 60, Notify: webhook(h, "success")},
			}
			Expect(k8sClient.Create(ctx, m)).To(Succeed())
			nn := types.NamespacedName{Name: m.Name, Namespace: m.Namespace}
			DeferCleanup(func() { r.Retries.Stop(nn) })
			check(nn)
			Expect(k8sClient.Delete(ctx, m)).To(Succeed())
			_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: nn})
			Expect(err).NotTo(HaveOccurred())
			Expect(r.Retries.Stop(nn)).To(BeFalse())
			Expect(recorder.Events).To(BeEmpty())
		})

		It("does not record a change the status has moved past", func() {
			h := sequence(http.StatusNoContent)
			nn := retried("retry-moved-on", webhook(h, "change"))
			m := get(nn)
			Expect(r.markAnnounced(ctx, nn, announced{status: "failure", since: m.Status.LastStatusChange})).To(Succeed())
			Expect(r.markAnnounced(ctx, nn, announced{status: "success", since: metav1.NewTime(m.Status.LastStatusChange.Add(-time.Hour))})).To(Succeed())
			Expect(notified(nn)).To(HaveValue(Equal("success")))
			m = get(nn)
			m.Status.LastNotifiedStatus = nil
			m.Status.LastNotifiedStatusChange = metav1.Time{}
			Expect(k8sClient.Status().Update(ctx, &m)).To(Succeed())
			Expect(r.markAnnounced(ctx, nn, announced{status: "failure", since: m.Status.LastStatusChange})).To(Succeed())
			Expect(notified(nn)).To(BeNil())
			Expect(r.markAnnounced(ctx, nn, announced{status: "success", since: m.Status.LastStatusChange})).To(Succeed())
			Expect(notified(nn)).To(HaveValue(Equal("success")))
		})

		It("sends each alert once when Tries is 1", func() {
			r.Retries.Tries = 1
			h := sequence(http.StatusInternalServerError)
			retried("retry-tries-one", webhook(h, "success"))
			Expect(recorder.Events).To(Receive(Equal(alertFailed("success", "webhook"))))
			Consistently(h.hits.Load, 150*time.Millisecond).Should(BeEquivalentTo(1))
		})
	})
})
