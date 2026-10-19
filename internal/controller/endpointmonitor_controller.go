package controller

import (
	"context"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	monitorv1alpha1 "github.com/patrickdk77/endpoint-monitoring-operator/api/v1alpha1"
	notifierTypes "github.com/patrickdk77/endpoint-monitoring-operator/internal/notifier"
	"github.com/patrickdk77/endpoint-monitoring-operator/pkg/factory"
)

// setupRetry caps how long a monitor whose driver or notifier cannot be
// built waits before the next attempt, so a Secret created after the
// monitor is picked up without waiting a long checkInterval.
const setupRetry = 30 * time.Second

type EndpointMonitorReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder events.EventRecorder
}

func (r *EndpointMonitorReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	logger.Info("Reconcile triggered",
		"name", req.Name,
		"namespace", req.Namespace,
		"timestamp", time.Now().Format(time.RFC3339))

	var monitor monitorv1alpha1.EndpointMonitor
	if err := r.Get(ctx, req.NamespacedName, &monitor); err != nil {
		if errors.IsNotFound(err) {
			logger.Info("EndpointMonitor resource not found. Ignoring since object must be deleted.")
			return ctrl.Result{}, nil
		}
		logger.Error(err, "Failed to get EndpointMonitor")
		return ctrl.Result{}, err
	}

	if monitor.Spec.CheckInterval < 1 || monitor.Spec.CheckInterval > monitorv1alpha1.MaxCheckInterval {
		err := fmt.Errorf("checkInterval must be between 1 and %d, got %d", monitorv1alpha1.MaxCheckInterval, monitor.Spec.CheckInterval)
		return r.cannotCheck(ctx, &monitor, err, 0), nil
	}

	now := time.Now()
	checkInterval := time.Duration(monitor.Spec.CheckInterval) * time.Second
	nextCheckTime := monitor.Status.LastCheckedTime.Time.Add(checkInterval)

	if !monitor.Status.LastCheckedTime.IsZero() && now.Before(nextCheckTime) {
		requeueAfter := time.Until(nextCheckTime)
		logger.Info("Skipping reconcile; checkInterval not yet elapsed",
			"name", monitor.Name,
			"lastChecked", monitor.Status.LastCheckedTime.Time,
			"nextCheckDue", nextCheckTime,
			"requeueAfter", requeueAfter)
		return ctrl.Result{RequeueAfter: requeueAfter}, nil
	}

	retry := min(checkInterval, setupRetry)
	driver, err := factory.NewDriver(monitor.Spec.Driver, monitor.Spec.Endpoint, &monitor, req.Namespace, r.Client)
	if err != nil {
		return r.cannotCheck(ctx, &monitor, fmt.Errorf("cannot create driver: %w", err), retry), nil
	}

	notifier, err := factory.NewNotifier(&monitor.Spec.Notify)
	if err != nil {
		return r.cannotCheck(ctx, &monitor, err, retry), nil
	}

	result, err := driver.Check()
	if err != nil {
		logger.Error(err, "Failed to perform health check")
		return ctrl.Result{}, err
	}

	noticeValues := notifierTypes.NoticeValues{
		// save values that can be overwritten by the Status().Update
		LastStatus:       monitor.Status.LastStatus,
		LastStatusChange: monitor.Status.LastStatusChange.Time.String(),
		StatusTime:       now.Sub(monitor.Status.LastStatusChange.Time).Round(time.Second).String(),
		Status:           "failure",
		Healthy:          "unhealthy",
		Endpoint:         driver.GetEndpoint(),
		Driver:           driver.GetType(),
		Name:             monitor.Name,
		Message:          result.Message,
		Response:         result.ResponseTime.String(),
		ResponseTimeMs:   result.ResponseTime.Milliseconds(),
		CurrentTime:      now.String(),
		Namespace:        req.Namespace,
		AlertMessage:     "",
	}
	if result.Success {
		noticeValues.Status = "success"
		noticeValues.Healthy = "healthy"
	}

	updated := false
	nowMetaTime := metav1.NewTime(now)
	if monitor.Status.LastStatus != noticeValues.Status {
		monitor.Status.LastStatus = noticeValues.Status
		monitor.Status.LastStatusChange = nowMetaTime
		updated = true
	}
	if monitor.Status.LastCheckedTime != nowMetaTime {
		monitor.Status.LastCheckedTime = nowMetaTime
		updated = true
	}

	// Save updates to resource beofre sending notification, incase of notification failure
	// also fix out of order updates if the notification takes longer than the next check interval
	// and the next update saves before this cone
	if updated {
		if err := r.Status().Update(ctx, &monitor); err != nil {
			logger.Error(err, "Failed to update EndpointMonitor status")
			return ctrl.Result{}, err
		}
	}

	// calculate notice values that are not time sensitive
	noticeValues.AlertMessage = fmt.Sprintf("%s monitor for %s is %s\n%s", noticeValues.Driver, noticeValues.Endpoint, noticeValues.Healthy, noticeValues.Message)

	if err := notifier.SendAlert(noticeValues.Status, &noticeValues, r.Client); err != nil {
		logger.Error(err, "Failed to send alert")
		return ctrl.Result{}, err
	}

	if noticeValues.LastStatus != noticeValues.Status {
		if err := notifier.SendAlert("change", &noticeValues, r.Client); err != nil {
			logger.Error(err, "Failed to send alert")
			return ctrl.Result{}, err
		}
	}

	logger.Info("Reconciliation complete",
		"name", monitor.Name,
		"status", noticeValues.Status,
		"responseTime", noticeValues.Response)

	return ctrl.Result{RequeueAfter: checkInterval}, nil
}

// cannotCheck reports a monitor that cannot be checked and retries it after
// retry; zero waits for the spec to change.
func (r *EndpointMonitorReconciler) cannotCheck(ctx context.Context, monitor *monitorv1alpha1.EndpointMonitor, err error, retry time.Duration) ctrl.Result {
	log.FromContext(ctx).Error(err, "Cannot check EndpointMonitor", "name", monitor.Name)
	r.Recorder.Eventf(monitor, nil, corev1.EventTypeWarning, "CannotCheck", "Check", "%s", err.Error())
	return ctrl.Result{RequeueAfter: retry}
}

func (r *EndpointMonitorReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&monitorv1alpha1.EndpointMonitor{}).
		Complete(r)
}
