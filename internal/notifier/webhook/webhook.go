package webhook

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
	"text/template"

	"github.com/patrickdk77/endpoint-monitoring-operator/api/v1alpha1"
	"github.com/patrickdk77/endpoint-monitoring-operator/internal/notifier"
	"github.com/patrickdk77/endpoint-monitoring-operator/internal/version"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type WebhookNotifier struct {
	cfg      *v1alpha1.WebhookConfig
	contents *template.Template
}

func New(config *v1alpha1.WebhookConfig) (notifier.Notifier, error) {
	if config == nil || !config.Enabled || config.WebhookURL == "" {
		return nil, fmt.Errorf("invalid Webhook config")
	}
	w := &WebhookNotifier{cfg: config}
	if config.Contents != "" {
		tmpl, err := template.New("contents").Parse(config.Contents)
		if err != nil {
			return nil, fmt.Errorf("failed to parse the contents template: %w", err)
		}
		w.contents = tmpl
	}
	return w, nil
}

func (w *WebhookNotifier) SendAlert(status string, values *notifier.NoticeValues, client client.Client) error {
	if !w.shouldAlert(status) {
		return nil // silently skip
	}

	contentType := "application/json"
	if w.cfg.ContentType != "" {
		contentType = w.cfg.ContentType
	}

	var req *http.Request
	if w.contents != nil {
		var buf bytes.Buffer
		err := w.contents.Execute(&buf, values)
		if err != nil {
			return fmt.Errorf("WEBHOOK: failed to execute the contents template: %w", err)
		}

		req, err = http.NewRequest(http.MethodPost, w.cfg.WebhookURL, io.Reader(&buf))
		if err != nil {
			return fmt.Errorf("WEBHOOK: failed to create POST webhook: %w", err)
		}
		req.Header.Add("Content-Type", contentType)
	} else {
		var err error
		req, err = http.NewRequest(http.MethodGet, w.cfg.WebhookURL, nil)
		if err != nil {
			return fmt.Errorf("WEBHOOK: failed to GET webhook alert: %w", err)
		}
	}

	req.Header.Set("User-Agent", version.UserAgent)

	if w.cfg.Authorization.Name != "" {
		secret, err := notifier.GetSecret(w.cfg.Authorization.Name, values.Namespace, client)
		if err != nil {
			return fmt.Errorf("WEBHOOK: unable to read the authorization secret: %w", err)
		}
		if auth, ok := secret.Data["Raw"]; ok {
			req.Header.Add("Authorization", string(auth))
		}
		if auth, ok := secret.Data["Basic"]; ok {
			req.Header.Add("Authorization", "Basic "+base64.StdEncoding.EncodeToString(auth))
		}
		if auth, ok := secret.Data["Bearer"]; ok {
			req.Header.Add("Authorization", "Bearer "+string(auth))
		}
	}

	httpClient := &http.Client{Timeout: notifier.Timeout}
	resp, err := httpClient.Do(req)
	if err != nil {
		return &notifier.RetryableError{Err: fmt.Errorf("WEBHOOK: failed to make http webhook request: %w", err)}
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		err := fmt.Errorf("WEBHOOK: non-2xx response from webhook: %s", resp.Status)
		if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500 {
			return &notifier.RetryableError{Err: err}
		}
		return err
	}

	return nil
}

func (w *WebhookNotifier) shouldAlert(status string) bool {
	return notifier.ShouldAlert(w.cfg.AlertOn, status)
}
