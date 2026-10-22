package controller

import (
	"context"
	"errors"
	"slices"
	"sync"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	monitorv1alpha1 "github.com/patrickdk77/endpoint-monitoring-operator/api/v1alpha1"
	notifierTypes "github.com/patrickdk77/endpoint-monitoring-operator/internal/notifier"
	"github.com/patrickdk77/endpoint-monitoring-operator/pkg/factory"
)

// AlertRetries resends in the background an alert whose notifier failed in
// a way a later try may not repeat. Tries counts the first try. A newer
// check of the monitor stops its pending retries, and an alert they did not
// deliver gets its AlertFailed event then.
type AlertRetries struct {
	Tries int
	Delay time.Duration

	mu      sync.Mutex
	pending map[types.NamespacedName][]*alertRetry
	wrote   map[types.NamespacedName]bool
}

type alertRetry struct {
	stop chan struct{}
	done chan struct{}
}

// NewAlertRetries tries each alert five times, 30 seconds apart.
func NewAlertRetries() *AlertRetries {
	return &AlertRetries{Tries: 5, Delay: 30 * time.Second}
}

// Stop stops the monitor's pending retries and waits for a try in flight.
// It reports whether a retry was pending or has written the monitor's
// status since the last Stop, so the caller then reads the status past the
// cache.
func (a *AlertRetries) Stop(nn types.NamespacedName) bool {
	if a == nil {
		return false
	}
	a.mu.Lock()
	runs := a.pending[nn]
	wrote := a.wrote[nn]
	delete(a.pending, nn)
	a.mu.Unlock()
	for _, run := range runs {
		close(run.stop)
		<-run.done
	}
	a.mu.Lock()
	delete(a.wrote, nn)
	a.mu.Unlock()
	return len(runs) > 0 || wrote
}

func (a *AlertRetries) noteWrite(nn types.NamespacedName) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.wrote == nil {
		a.wrote = map[types.NamespacedName]bool{}
	}
	a.wrote[nn] = true
}

func (a *AlertRetries) add(nn types.NamespacedName) *alertRetry {
	run := &alertRetry{stop: make(chan struct{}), done: make(chan struct{})}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.pending == nil {
		a.pending = map[types.NamespacedName][]*alertRetry{}
	}
	a.pending[nn] = append(a.pending[nn], run)
	return run
}

func (a *AlertRetries) remove(nn types.NamespacedName, run *alertRetry) {
	a.mu.Lock()
	defer a.mu.Unlock()
	runs := slices.DeleteFunc(a.pending[nn], func(r *alertRetry) bool { return r == run })
	if len(runs) == 0 {
		delete(a.pending, nn)
	} else {
		a.pending[nn] = runs
	}
}

// announced is the change a change alert reports: the new status and when
// it began.
type announced struct {
	status string
	since  metav1.Time
}

// queueRetries retries in the background each notifier whose failure a
// later try may not repeat, and returns the failures to report now. When a
// change alert's only failure is retried, the retry that delivers it
// records the change as announced.
func (r *EndpointMonitorReconciler) queueRetries(ctx context.Context, monitor *monitorv1alpha1.EndpointMonitor, alert string, values notifierTypes.NoticeValues, err error, change *announced) error {
	var sendErr *factory.SendError
	if r.Retries == nil || r.Retries.Tries < 2 || !errors.As(err, &sendErr) {
		return err
	}
	rest := &factory.SendError{}
	var retried []int
	for i, e := range sendErr.Errs {
		var temporary *notifierTypes.RetryableError
		if errors.As(e, &temporary) && i < len(sendErr.Failed) {
			retried = append(retried, i)
			continue
		}
		rest.Notifiers = append(rest.Notifiers, sendErr.Notifiers[i])
		rest.Errs = append(rest.Errs, e)
	}
	if len(rest.Errs) > 0 || len(retried) > 1 {
		change = nil
	}
	nn := client.ObjectKeyFromObject(monitor)
	for _, i := range retried {
		log.FromContext(ctx).Info("Alert failed; retrying", "name", monitor.Name, "alert", alert, "notifier", sendErr.Notifiers[i], "error", sendErr.Errs[i].Error())
		run := r.Retries.add(nn)
		go r.retryAlert(context.WithoutCancel(ctx), monitor.DeepCopy(), run, sendErr.Notifiers[i], sendErr.Failed[i], alert, values, change, sendErr.Errs[i])
	}
	if len(rest.Errs) == 0 {
		return nil
	}
	return rest
}

func (r *EndpointMonitorReconciler) retryAlert(ctx context.Context, monitor *monitorv1alpha1.EndpointMonitor, run *alertRetry, name string, n notifierTypes.Notifier, alert string, values notifierTypes.NoticeValues, change *announced, err error) {
	nn := client.ObjectKeyFromObject(monitor)
	defer close(run.done)
	defer r.Retries.remove(nn, run)
	logger := log.FromContext(ctx).WithValues("name", nn.Name, "alert", alert, "notifier", name)
	for try := 2; try <= r.Retries.Tries; try++ {
		select {
		case <-run.stop:
			var current monitorv1alpha1.EndpointMonitor
			if getErr := r.reader().Get(ctx, nn, &current); apierrors.IsNotFound(getErr) {
				return
			} else if getErr == nil {
				monitor = &current
			}
			r.alertFailed(ctx, monitor, alert, &factory.SendError{Notifiers: []string{name}, Errs: []error{err}})
			return
		case <-time.After(r.Retries.Delay):
		}
		var current monitorv1alpha1.EndpointMonitor
		if getErr := r.reader().Get(ctx, nn, &current); apierrors.IsNotFound(getErr) {
			return
		} else if getErr == nil {
			monitor = &current
		}
		if err = n.SendAlert(alert, &values, r.Client); err == nil {
			logger.Info("Alert sent on retry", "try", try)
			if change != nil {
				if err := r.markAnnounced(ctx, nn, *change); err != nil {
					logger.Error(err, "Failed to record the announced change")
				} else {
					r.Retries.noteWrite(nn)
				}
			}
			return
		}
		var temporary *notifierTypes.RetryableError
		if !errors.As(err, &temporary) {
			break
		}
		if try < r.Retries.Tries {
			logger.Info("Alert retry failed", "try", try, "error", err.Error())
		}
	}
	r.alertFailed(ctx, monitor, alert, &factory.SendError{Notifiers: []string{name}, Errs: []error{err}})
}

// markAnnounced records a change alert that a retry delivered, unless the
// status has moved on since.
func (r *EndpointMonitorReconciler) markAnnounced(ctx context.Context, nn types.NamespacedName, change announced) error {
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var m monitorv1alpha1.EndpointMonitor
		if err := r.reader().Get(ctx, nn, &m); err != nil {
			return client.IgnoreNotFound(err)
		}
		if m.Status.LastStatus != change.status || !m.Status.LastStatusChange.Equal(&change.since) {
			return nil
		}
		status := change.status
		m.Status.LastNotifiedStatus = &status
		m.Status.LastNotifiedStatusChange = m.Status.LastStatusChange
		return r.Status().Update(ctx, &m)
	})
}

// reader reads past the manager's cache, so a status that a retry has just
// written is seen at once.
func (r *EndpointMonitorReconciler) reader() client.Reader {
	if r.APIReader != nil {
		return r.APIReader
	}
	return r.Client
}
