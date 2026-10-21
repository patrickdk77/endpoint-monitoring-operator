package slack

import (
	"io"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/patrickdk77/endpoint-monitoring-operator/api/v1alpha1"
	"github.com/patrickdk77/endpoint-monitoring-operator/internal/notifier"
)

func TestSendAlertGivesUpOnStalledEndpoint(t *testing.T) {
	old := notifier.Timeout
	notifier.Timeout = 200 * time.Millisecond
	t.Cleanup(func() { notifier.Timeout = old })

	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				_, _ = io.Copy(io.Discard, c)
			}()
		}
	}()

	n, err := New(&v1alpha1.SlackConfig{Enabled: true, WebhookURL: "http://" + ln.Addr().String() + "/hook"})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		done <- n.SendAlert("failure", &notifier.NoticeValues{Status: "failure", AlertMessage: "down"}, nil)
	}()
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "Client.Timeout") {
			t.Fatalf("err = %v, want a client timeout", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("SendAlert still waiting on an endpoint that never answers")
	}
}
