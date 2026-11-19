package kubevm

import (
	"context"
	"net"
	"path/filepath"
	"testing"
	"time"
)

func TestSocketClientAgainstFake(t *testing.T) {
	f := NewFake()
	sock := filepath.Join(t.TempDir(), "c.sock")
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	go func() { _ = f.Serve(l) }()

	c := NewSocketClient(sock)
	ctx := context.Background()

	if err := c.Ping(ctx); err != nil {
		t.Fatalf("ping: %v", err)
	}
	if _, err := c.Examine(ctx, "a"); !IsNotFound(err) {
		t.Fatalf("examine unknown: want ErrNotFound, got %v", err)
	}
	if err := c.Delete(ctx, "a"); !IsNotFound(err) {
		t.Fatalf("delete unknown: want ErrNotFound, got %v", err)
	}
	if err := c.Shutdown(ctx, "a"); err != nil {
		t.Fatalf("shutdown unknown must succeed: %v", err)
	}

	payload := map[string]any{
		"uid": "a", "mem": 512, "cpu": 2,
		"nics": []map[string]any{{"nat": true}},
	}
	if err := c.Start(ctx, payload); err != nil {
		t.Fatalf("start: %v", err)
	}
	err = c.Start(ctx, payload)
	if !IsDaemonError(err) {
		t.Fatalf("duplicate start: want DaemonError, got %v", err)
	}
	vm, err := c.Examine(ctx, "a")
	if err != nil || vm.State != StateRunning || vm.NATGuestIP == "" || vm.MemMiB != 512 {
		t.Fatalf("examine = %+v, %v", vm, err)
	}
	if err := c.SetActivity(ctx, "a", time.Minute, 0, false); err != nil {
		t.Fatal(err)
	}
	if vm, _ = c.Examine(ctx, "a"); vm.ActivityTimeout != "1m0s" || vm.WakeOnNetwork {
		t.Fatalf("activity not applied: %+v", vm)
	}
	if err := c.Pause(ctx, "a"); err != nil {
		t.Fatal(err)
	}
	if err := c.Delete(ctx, "a"); !IsDaemonError(err) {
		t.Fatalf("delete of live vm must be a daemon error, got %v", err)
	}
	if err := c.Shutdown(ctx, "a"); err != nil {
		t.Fatal(err)
	}
	if err := c.Delete(ctx, "a"); err != nil {
		t.Fatalf("delete stopped: %v", err)
	}
}

func TestSocketClientTransportError(t *testing.T) {
	c := NewSocketClient(filepath.Join(t.TempDir(), "missing.sock"))
	err := c.Ping(context.Background())
	if err == nil || IsDaemonError(err) {
		t.Fatalf("want transport error, got %v", err)
	}
}
