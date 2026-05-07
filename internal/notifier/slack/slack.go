package slack

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/patrickdk77/endpoint-monitoring-operator/api/v1alpha1"
	"github.com/patrickdk77/endpoint-monitoring-operator/internal/notifier"
	"github.com/patrickdk77/endpoint-monitoring-operator/internal/version"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type SlackNotifier struct {
	cfg *v1alpha1.SlackConfig
}

func New(config *v1alpha1.SlackConfig) (notifier.Notifier, error) {
	if config == nil || !config.Enabled || config.WebhookURL == "" {
		return nil, fmt.Errorf("invalid Slack config")
	}
	return &SlackNotifier{cfg: config}, nil
}

func (s *SlackNotifier) SendAlert(status string, values *notifier.NoticeValues, client client.Client) error {
	if !s.shouldAlert(status) {
		return nil // silently skip
	}

	payload := map[string]string{"text": values.AlertMessage}
	jsonData, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("failed to marshal slack payload: %w", err)
	}

	req, err := http.NewRequest(http.MethodPost, s.cfg.WebhookURL, bytes.NewBuffer(jsonData))
	if err != nil {
		return fmt.Errorf("failed to create slack request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", version.UserAgent)
	httpClient := &http.Client{}
	resp, err := httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("failed to send slack alert: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return fmt.Errorf("non-200 response from slack: %s", resp.Status)
	}

	return nil
}

func (s *SlackNotifier) shouldAlert(status string) bool {
	return notifier.ShouldAlert(s.cfg.AlertOn, status)
}
