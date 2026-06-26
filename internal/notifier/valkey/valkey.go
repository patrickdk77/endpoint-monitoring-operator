package valkey

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	v1 "github.com/patrickdk77/endpoint-monitoring-operator/api/v1alpha1"
	"github.com/patrickdk77/endpoint-monitoring-operator/internal/notifier"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const defaultRetentionDays = 90

type ValkeyNotifier struct {
	cfg              *v1.ValkeyConfig
	retentionSeconds int
}

func New(config *v1.ValkeyConfig) (notifier.Notifier, error) {
	if config == nil || !config.Enabled || config.Endpoint == "" {
		return nil, fmt.Errorf("invalid Valkey config")
	}
	if len(config.Dashboards) == 0 {
		return nil, fmt.Errorf("valkey notifier requires at least one dashboard")
	}
	retentionDays := defaultRetentionDays
	if config.RetentionDays != nil {
		retentionDays = *config.RetentionDays
	}
	return &ValkeyNotifier{
		cfg:              config,
		retentionSeconds: retentionDays * 24 * 3600,
	}, nil
}

func (v *ValkeyNotifier) SendAlert(status string, values *notifier.NoticeValues, k8sClient client.Client) error {
	if status != "success" && status != "failure" {
		return nil
	}

	username, password, err := v.credentials(values.Namespace, k8sClient)
	if err != nil {
		return err
	}

	c, err := Dial(v.cfg.Endpoint, v.cfg.Tls, username, password, v.cfg.DB)
	if err != nil {
		return err
	}
	defer c.Close()

	now := time.Now().UTC()
	ts := strconv.FormatInt(now.Unix(), 10)
	hourEpoch := strconv.FormatInt(now.Truncate(time.Hour).Unix(), 10)
	svc := values.Name
	if v.cfg.Name != "" {
		svc = v.cfg.Name
	}

	statusVal := "f"
	if status == "success" {
		statusVal = "s"
	}

	detailVal := strconv.FormatInt(values.ResponseTimeMs, 10)
	if status == "failure" && values.Message != "" {
		detailVal += "|" + sanitizeMessage(values.Message)
	}

	var cmds [][]string
	for _, dash := range v.cfg.Dashboards {
		statusKey := fmt.Sprintf("emo:%s:%s:s:%s", dash, svc, hourEpoch)
		detailKey := fmt.Sprintf("emo:%s:%s:d:%s", dash, svc, hourEpoch)

		cmds = append(cmds,
			[]string{"HSET", statusKey, ts, statusVal},
			[]string{"HEXPIRE", statusKey, strconv.Itoa(v.retentionSeconds), "FIELDS", "1", ts},
			[]string{"HSET", detailKey, ts, detailVal},
			[]string{"HEXPIRE", detailKey, strconv.Itoa(v.retentionSeconds), "FIELDS", "1", ts},
			[]string{"SADD", "emo:dashboards", dash},
			[]string{"SADD", fmt.Sprintf("emo:%s:svcs", dash), svc},
			[]string{"HSET", fmt.Sprintf("emo:%s:%s:meta", dash, svc),
				"endpoint", values.Endpoint,
				"driver", values.Driver,
				"name", svc,
			},
		)
	}

	return c.Pipeline(cmds)
}

func (v *ValkeyNotifier) credentials(namespace string, k8sClient client.Client) (string, string, error) {
	if v.cfg.SecretRef.Name == "" {
		return "", "", nil
	}
	secret, err := notifier.GetSecret(v.cfg.SecretRef.Name, namespace, k8sClient)
	if err != nil {
		return "", "", fmt.Errorf("unable to read valkey secret: %w", err)
	}
	var username, password string
	if u, ok := secret.Data["username"]; ok {
		username = string(u)
	}
	if p, ok := secret.Data["password"]; ok {
		password = string(p)
	}
	return username, password, nil
}

func sanitizeMessage(msg string) string {
	msg = strings.ReplaceAll(msg, "\r", " ")
	msg = strings.ReplaceAll(msg, "\n", " ")
	msg = strings.ReplaceAll(msg, "|", "/")
	if len(msg) > 500 {
		msg = msg[:500]
	}
	return msg
}
