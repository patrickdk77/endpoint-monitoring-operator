/*
Copyright 2025.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controller

import (
	"net"
	"net/http"
	"net/http/httptest"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	Endpointmonitoringv1alpha1 "github.com/patrickdk77/endpoint-monitoring-operator/api/v1alpha1"
)

var _ = Describe("EndpointMonitor Controller", func() {
	var (
		endpoint string
		hookURL  string
		recorder *events.FakeRecorder
		r        *EndpointMonitorReconciler
		cleanup  []func()
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
		hook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusNoContent)
		}))
		endpoint, hookURL = ln.Addr().String(), hook.URL
		cleanup = []func(){func() { ln.Close() }, hook.Close}

		recorder = events.NewFakeRecorder(10)
		r = &EndpointMonitorReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), Recorder: recorder}
	})

	AfterEach(func() {
		for _, f := range cleanup {
			f()
		}
	})

	validSpec := func() Endpointmonitoringv1alpha1.EndpointMonitorSpec {
		return Endpointmonitoringv1alpha1.EndpointMonitorSpec{
			Driver:        "tcp",
			Endpoint:      endpoint,
			CheckInterval: 60,
			Notify: Endpointmonitoringv1alpha1.NotifyConfig{
				Discord: &Endpointmonitoringv1alpha1.DiscordConfig{Enabled: true, WebhookURL: hookURL},
			},
		}
	}

	create := func(name string, spec Endpointmonitoringv1alpha1.EndpointMonitorSpec) types.NamespacedName {
		m := &Endpointmonitoringv1alpha1.EndpointMonitor{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
			Spec:       spec,
		}
		Expect(k8sClient.Create(ctx, m)).To(Succeed())
		DeferCleanup(func() { Expect(k8sClient.Delete(ctx, m)).To(Succeed()) })
		return types.NamespacedName{Name: name, Namespace: "default"}
	}

	reconcileOnce := func(nn types.NamespacedName) ctrl.Result {
		res, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: nn})
		Expect(err).NotTo(HaveOccurred())
		return res
	}

	get := func(nn types.NamespacedName) Endpointmonitoringv1alpha1.EndpointMonitor {
		var m Endpointmonitoringv1alpha1.EndpointMonitor
		Expect(k8sClient.Get(ctx, nn, &m)).To(Succeed())
		return m
	}

	Context("When reconciling a resource", func() {
		It("checks the endpoint and schedules the next check", func() {
			nn := create("valid", validSpec())
			res := reconcileOnce(nn)
			Expect(res.RequeueAfter).To(Equal(60 * time.Second))
			m := get(nn)
			Expect(m.Status.LastStatus).To(Equal("success"))
			Expect(m.Status.LastCheckedTime.IsZero()).To(BeFalse())
			Expect(recorder.Events).To(BeEmpty())
		})

		It("waits for the check interval before checking again", func() {
			nn := create("valid-again", validSpec())
			reconcileOnce(nn)
			checked := get(nn).Status.LastCheckedTime
			res := reconcileOnce(nn)
			Expect(res.RequeueAfter).To(And(BeNumerically(">", 0), BeNumerically("<=", 60*time.Second)))
			Expect(get(nn).Status.LastCheckedTime).To(Equal(checked))
		})

		It("ignores a monitor that no longer exists", func() {
			res := reconcileOnce(types.NamespacedName{Name: "gone", Namespace: "default"})
			Expect(res).To(Equal(ctrl.Result{}))
			Expect(recorder.Events).To(BeEmpty())
		})
	})

	Context("When the spec cannot be checked", func() {
		DescribeTable("reports an event and retries soon",
			func(name string, mutate func(*Endpointmonitoringv1alpha1.EndpointMonitorSpec), wantEvent string) {
				spec := validSpec()
				mutate(&spec)
				nn := create(name, spec)
				res := reconcileOnce(nn)
				Expect(res.RequeueAfter).To(Equal(30 * time.Second))
				Expect(recorder.Events).To(Receive(HavePrefix("Warning CannotCheck " + wantEvent)))
				Expect(recorder.Events).To(BeEmpty())
				m := get(nn)
				Expect(m.Status.LastCheckedTime.IsZero()).To(BeTrue())
			},
			Entry("missing secret", "missing-secret", func(s *Endpointmonitoringv1alpha1.EndpointMonitorSpec) {
				s.Driver = "redis"
				s.RedisCheck = &Endpointmonitoringv1alpha1.RedisCheck{SecretRef: Endpointmonitoringv1alpha1.SecretRef{Name: "does-not-exist"}}
			}, "cannot create driver: unable to read secret"),
			Entry("driver block missing", "no-http-json-check", func(s *Endpointmonitoringv1alpha1.EndpointMonitorSpec) {
				s.Driver = "http-json"
			}, "cannot create driver: invalid endpoint or config for http-json"),
			Entry("empty endpoint", "empty-endpoint", func(s *Endpointmonitoringv1alpha1.EndpointMonitorSpec) {
				s.Endpoint = ""
			}, "cannot create driver: endpoint cannot be empty"),
			Entry("no notifier enabled", "no-notifier", func(s *Endpointmonitoringv1alpha1.EndpointMonitorSpec) {
				s.Notify.Discord.Enabled = false
			}, "no notifiers enabled"),
			Entry("notifier without a URL", "no-webhook-url", func(s *Endpointmonitoringv1alpha1.EndpointMonitorSpec) {
				s.Notify.Discord.WebhookURL = ""
			}, "failed to create Discord notifier"),
		)

		It("retries at the check interval when that is sooner", func() {
			spec := validSpec()
			spec.CheckInterval = 5
			spec.Notify.Discord.Enabled = false
			nn := create("short-interval", spec)
			res := reconcileOnce(nn)
			Expect(res.RequeueAfter).To(Equal(5 * time.Second))
			Expect(recorder.Events).To(Receive(HavePrefix("Warning CannotCheck no notifiers enabled")))
		})

		It("checks again once the missing secret exists", func() {
			spec := validSpec()
			spec.Driver = "redis"
			spec.RedisCheck = &Endpointmonitoringv1alpha1.RedisCheck{SecretRef: Endpointmonitoringv1alpha1.SecretRef{Name: "late-secret"}}
			nn := create("late-secret", spec)
			res := reconcileOnce(nn)
			Expect(res.RequeueAfter).To(Equal(30 * time.Second))
			Expect(recorder.Events).To(Receive(ContainSubstring("unable to read secret")))

			secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "late-secret", Namespace: "default"}, StringData: map[string]string{"password": "x"}}
			Expect(k8sClient.Create(ctx, secret)).To(Succeed())
			DeferCleanup(func() { Expect(k8sClient.Delete(ctx, secret)).To(Succeed()) })
			res = reconcileOnce(nn)
			Expect(res.RequeueAfter).To(Equal(60 * time.Second))
			m := get(nn)
			Expect(m.Status.LastCheckedTime.IsZero()).To(BeFalse())
		})
	})

	Context("When the API server validates a spec", func() {
		DescribeTable("rejects",
			func(name, field string, mutate func(*Endpointmonitoringv1alpha1.EndpointMonitorSpec)) {
				spec := validSpec()
				mutate(&spec)
				m := &Endpointmonitoringv1alpha1.EndpointMonitor{
					ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
					Spec:       spec,
				}
				err := k8sClient.Create(ctx, m)
				if err == nil {
					DeferCleanup(func() { Expect(k8sClient.Delete(ctx, m)).To(Succeed()) })
				}
				Expect(apierrors.IsInvalid(err)).To(BeTrue(), "%v", err)
				Expect(err.Error()).To(ContainSubstring(field))
			},
			Entry("an empty driver", "empty-driver", "spec.driver", func(s *Endpointmonitoringv1alpha1.EndpointMonitorSpec) { s.Driver = "" }),
			Entry("an unknown driver", "unknown-driver", "spec.driver", func(s *Endpointmonitoringv1alpha1.EndpointMonitorSpec) { s.Driver = "kafka" }),
			Entry("a driver in the wrong case", "upper-driver", "spec.driver", func(s *Endpointmonitoringv1alpha1.EndpointMonitorSpec) { s.Driver = "HTTP" }),
			Entry("checkInterval 0", "zero-interval", "spec.checkInterval", func(s *Endpointmonitoringv1alpha1.EndpointMonitorSpec) { s.CheckInterval = 0 }),
			Entry("a negative checkInterval", "negative-interval", "spec.checkInterval", func(s *Endpointmonitoringv1alpha1.EndpointMonitorSpec) { s.CheckInterval = -30 }),
			Entry("a checkInterval over the maximum", "huge-interval", "spec.checkInterval", func(s *Endpointmonitoringv1alpha1.EndpointMonitorSpec) {
				s.CheckInterval = Endpointmonitoringv1alpha1.MaxCheckInterval + 1
			}),
			Entry("a checkInterval that overflows a Duration", "overflow-interval", "spec.checkInterval", func(s *Endpointmonitoringv1alpha1.EndpointMonitorSpec) {
				s.CheckInterval = 9223372037
			}),
			Entry("an unknown slack alertOn", "slack-alerton", "spec.notify.slack.alertOn", func(s *Endpointmonitoringv1alpha1.EndpointMonitorSpec) {
				s.Notify.Slack = &Endpointmonitoringv1alpha1.SlackConfig{AlertOn: []string{"failure", "sometimes"}}
			}),
			Entry("an unknown email alertOn", "email-alerton", "spec.notify.email.alertOn", func(s *Endpointmonitoringv1alpha1.EndpointMonitorSpec) {
				s.Notify.Email = &Endpointmonitoringv1alpha1.EmailConfig{To: []string{"a@example.test"}, AlertOn: []string{"recovery"}}
			}),
			Entry("an unknown discord alertOn", "discord-alerton", "spec.notify.discord.alertOn", func(s *Endpointmonitoringv1alpha1.EndpointMonitorSpec) {
				s.Notify.Discord.AlertOn = []string{"Failure"}
			}),
			Entry("an unknown webhook alertOn", "webhook-alerton", "spec.notify.webhook.alertOn", func(s *Endpointmonitoringv1alpha1.EndpointMonitorSpec) {
				s.Notify.Webhook = &Endpointmonitoringv1alpha1.WebhookConfig{AlertOn: []string{""}}
			}),
		)

		It("rejects a monitor without a spec", func() {
			for name, body := range map[string]map[string]any{
				"no-spec":   {},
				"null-spec": {"spec": nil},
			} {
				u := &unstructured.Unstructured{Object: body}
				u.SetAPIVersion("monitoring.licious.app/v1alpha1")
				u.SetKind("EndpointMonitor")
				u.SetName(name)
				u.SetNamespace("default")
				err := k8sClient.Create(ctx, u)
				if err == nil {
					DeferCleanup(func() { Expect(k8sClient.Delete(ctx, u)).To(Succeed()) })
				}
				Expect(apierrors.IsInvalid(err)).To(BeTrue(), "%s: %v", name, err)
				Expect(err.Error()).To(ContainSubstring("spec"))
			}
		})

		It("accepts every driver and alertOn value", func() {
			for i, driverType := range []string{"http", "http-json", "smtp", "redis", "tls", "tcp", "dns", "ping", "trino", "opensearch", "mysql"} {
				spec := validSpec()
				spec.Driver = driverType
				spec.CheckInterval = 1
				if i%2 == 1 {
					spec.CheckInterval = Endpointmonitoringv1alpha1.MaxCheckInterval
				}
				all := []string{"success", "failure", "change"}
				spec.Notify.Slack = &Endpointmonitoringv1alpha1.SlackConfig{AlertOn: all}
				spec.Notify.Email = &Endpointmonitoringv1alpha1.EmailConfig{To: []string{"a@example.test"}, AlertOn: all}
				spec.Notify.Discord.AlertOn = all
				spec.Notify.Webhook = &Endpointmonitoringv1alpha1.WebhookConfig{AlertOn: all}
				create("accepted-"+driverType, spec)
			}
		})
	})
})
