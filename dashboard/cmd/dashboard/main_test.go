package main

import (
	"net/http"
	"testing"
)

func TestHTTPServerLimitsHowLongHeadersMayTake(t *testing.T) {
	srv := newHTTPServer(":0", http.NotFoundHandler())
	if srv.ReadHeaderTimeout <= 0 {
		t.Fatalf("ReadHeaderTimeout = %v; a client could hold a connection open by sending headers slowly", srv.ReadHeaderTimeout)
	}
}
