package slack

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/patrickdk77/endpoint-monitoring-operator/api/v1alpha1"
	"github.com/patrickdk77/endpoint-monitoring-operator/internal/notifier"
)

func TestSlackAcceptsAny2xx(t *testing.T) {
	for _, tc := range []struct {
		code   int
		wantOK bool
	}{
		{http.StatusOK, true}, {http.StatusNoContent, true},
		{http.StatusNotModified, false}, {http.StatusBadRequest, false}, {http.StatusInternalServerError, false},
	} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(tc.code)
		}))
		n, err := New(&v1alpha1.SlackConfig{Enabled: true, WebhookURL: srv.URL})
		if err != nil {
			t.Fatal(err)
		}
		err = n.SendAlert("failure", &notifier.NoticeValues{AlertMessage: "api is down"}, nil)
		srv.Close()
		if (err == nil) != tc.wantOK {
			t.Fatalf("status %d: err=%v", tc.code, err)
		}
	}
}
