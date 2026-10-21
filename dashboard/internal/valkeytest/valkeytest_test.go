package valkeytest

import (
	"context"
	"testing"

	"github.com/valkey-io/valkey-go"
)

func client(t *testing.T, srv *Server) valkey.Client {
	t.Helper()
	c, err := valkey.NewClient(valkey.ClientOption{InitAddress: []string{srv.Addr()}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.Close)
	return c
}

func TestExpiryAndDeletionFollowValkey(t *testing.T) {
	ctx := context.Background()
	srv := Start(t)
	c := client(t, srv)
	do := func(cmd ...string) valkey.ValkeyResult {
		return c.Do(ctx, c.B().Arbitrary(cmd[0]).Args(cmd[1:]...).Build())
	}

	srv.HSet("h", "a", "1")
	srv.HSet("h", "b", "2")
	if got, err := do("HEXPIRE", "h", "60", "FIELDS", "1", "a").AsIntSlice(); err != nil || len(got) != 1 || got[0] != 1 {
		t.Fatalf("HEXPIRE 60 = %v, %v; want [1]", got, err)
	}
	if ttl, ok := srv.FieldTTL("h", "a"); !ok || ttl != 60 {
		t.Fatalf("field TTL = %d, %v; want 60", ttl, ok)
	}
	if got, err := do("HEXPIRE", "h", "0", "FIELDS", "1", "a").AsIntSlice(); err != nil || len(got) != 1 || got[0] != 2 {
		t.Fatalf("HEXPIRE 0 = %v, %v; want [2], the field deleted", got, err)
	}
	if _, ok := srv.HGetAll("h")["a"]; ok {
		t.Fatal("HEXPIRE 0 left the field in place")
	}
	if err := do("HEXPIRE", "h", "60", "FIELDS", "2", "b").Error(); err == nil {
		t.Fatal("HEXPIRE accepted a numfields that does not match the fields given")
	}

	if n, err := do("HDEL", "h", "b").AsInt64(); err != nil || n != 1 {
		t.Fatalf("HDEL = %d, %v; want 1", n, err)
	}
	if n, err := do("EXPIRE", "h", "60").AsInt64(); err != nil || n != 0 {
		t.Fatalf("EXPIRE on a hash HDEL emptied = %d, %v; want 0, the key gone", n, err)
	}

	srv.HSet("k", "a", "1")
	if n, err := do("EXPIRE", "k", "0").AsInt64(); err != nil || n != 1 {
		t.Fatalf("EXPIRE 0 = %d, %v; want 1", n, err)
	}
	if len(srv.HGetAll("k")) != 0 {
		t.Fatal("EXPIRE 0 left the key in place")
	}
}
