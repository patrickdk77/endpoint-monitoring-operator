package notifier

import (
	"context"
	"net/url"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Timeout bounds each request the Slack, Discord, webhook, email and SES
// notifiers make; valkey keeps its own deadlines.
var Timeout = 30 * time.Second

type Notifier interface {
	SendAlert(status string, values *NoticeValues, client client.Client) error
}

type NoticeValues struct {
	Status           string
	Endpoint         string
	Driver           string
	Healthy          string
	Name             string
	Response         string
	ResponseTimeMs   int64
	LastStatus       string
	LastChecked      string
	LastStatusChange string
	CurrentTime      string
	StatusTime       string
	Message          string
	Namespace        string
	AlertMessage     string
}

// ShouldAlert returns true if the given status is in the allowed list, or if the list is empty and status is "failure".
func ShouldAlert(allowed []string, status string) bool {
	if len(allowed) == 0 {
		// Default to alert only on failure
		return status == "failure"
	}
	for _, a := range allowed {
		if a == status {
			return true
		}
	}
	return false
}

func DisplayEndpoint(endpoint string) string {
	trimmed := strings.TrimSpace(endpoint)
	if !strings.Contains(trimmed, "://") && !strings.HasPrefix(trimmed, "//") {
		return endpoint
	}
	u, err := url.Parse(trimmed)
	if err != nil {
		return "(invalid URL)"
	}
	u.RawQuery, u.ForceQuery, u.Fragment, u.RawFragment = "", false, "", ""
	if _, ok := u.User.Password(); ok {
		return strings.Replace(u.String(), u.User.String()+"@", u.User.Username()+":***@", 1)
	}
	return u.String()
}

func GetSecret(name string, namespace string, client client.Client) (*corev1.Secret, error) {
	secretName := types.NamespacedName{
		Namespace: namespace,
		Name:      name,
	}
	secret := &corev1.Secret{}
	err := client.Get(context.TODO(), secretName, secret)
	if err != nil {
		return nil, err
	}
	return secret, nil
}
