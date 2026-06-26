package valkey

import (
	"testing"
)

func TestEncodeCommand(t *testing.T) {
	got := string(encodeCommand([]string{"HSET", "key", "field", "value"}))
	want := "*4\r\n$4\r\nHSET\r\n$3\r\nkey\r\n$5\r\nfield\r\n$5\r\nvalue\r\n"
	if got != want {
		t.Fatalf("encodeCommand mismatch:\n%q\nwant\n%q", got, want)
	}
}
