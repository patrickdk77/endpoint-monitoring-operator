package server

import (
	"testing"

	"github.com/patrickdk77/endpoint-monitoring-operator/dashboard/internal/store"
)

func TestHandlerRegistersWithoutConflict(t *testing.T) {
	s, err := New(&store.Store{}, 90)
	if err != nil {
		t.Fatal(err)
	}
	// Must not panic: Go 1.22+ ServeMux rejects overlapping /static/ and /{dash}/{name}.
	_ = s.Handler()
}
