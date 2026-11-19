package podnetwork

import (
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func testConfig() *Config {
	return &Config{Version: 1, Identity: Identity{VMUID: "vm", PodUID: "pod", Generation: "sandbox", IPAddress: "10.0.0.2"},
		Namespace: "net:[123]", MAC: "02:00:00:00:00:02", Address: "10.0.0.2/32", MTU: 1450,
		Routes: []Route{{Destination: "10.0.0.1/32", LinkScope: true}, {Gateway: "10.0.0.1"}},
		Resolver: "nameserver 10.96.0.10\nsearch default.svc.cluster.local\n"}
}

func TestConfigRejectsInvalidAllocations(t *testing.T) {
	for _, mutate := range []func(*Config){
		func(c *Config) { c.Version = 2 },
		func(c *Config) { c.Generation = "" },
		func(c *Config) { c.Address = "10.0.0.3/32" },
		func(c *Config) { c.Address = "fd00::1/64"; c.IPAddress = "fd00::1" },
		func(c *Config) { c.Routes[0].Gateway = "invalid" },
		func(c *Config) { c.MTU = 0 },
	} {
		cfg := testConfig()
		mutate(cfg)
		if err := cfg.Validate(); err == nil { t.Fatalf("accepted invalid config: %+v", cfg) }
	}
}

func TestHolderDescriptorHandoff(t *testing.T) {
	cfg := testConfig()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	tap, err := os.CreateTemp(t.TempDir(), "descriptor")
	if err != nil { t.Fatal(err) }
	defer tap.Close()
	if _, err := tap.WriteString("packet-backend"); err != nil { t.Fatal(err) }
	path := filepath.Join(t.TempDir(), "net.sock")
	done := make(chan error, 1)
	go func() { done <- Serve(ctx, path, cfg, tap) }()
	deadline := time.Now().Add(3*time.Second)
	for {
		if _, err := Status(ctx, path, cfg.Identity); err == nil { break }
		if time.Now().After(deadline) { t.Fatal("holder never became ready") }
		time.Sleep(time.Millisecond)
	}
	wrong := cfg.Identity
	wrong.PodUID = "another-pod"
	if _, err := Status(ctx, path, wrong); err == nil { t.Fatal("accepted unrelated Pod") }
	acquire := func() (*net.UnixConn, Response, []int) {
		t.Helper()
		conn, err := net.DialUnix("unixpacket", nil, &net.UnixAddr{Name: path, Net: "unixpacket"})
		if err != nil { t.Fatal(err) }
		_ = conn.SetDeadline(time.Now().Add(3*time.Second))
		data, err := json.Marshal(Request{Action: "acquire", Identity: cfg.Identity})
		if err != nil { t.Fatal(err) }
		if _, err := conn.Write(data); err != nil { t.Fatal(err) }
		data, oob := make([]byte, MaxMessage), make([]byte, unix.CmsgSpace(4))
		n, oobn, _, _, err := conn.ReadMsgUnix(data, oob)
		if err != nil { t.Fatal(err) }
		var response Response
		if err := json.Unmarshal(data[:n], &response); err != nil { t.Fatal(err) }
		msgs, err := unix.ParseSocketControlMessage(oob[:oobn])
		if err != nil { t.Fatal(err) }
		var fds []int
		for _, msg := range msgs {
			rights, err := unix.ParseUnixRights(&msg)
			if err != nil { t.Fatal(err) }
			fds = append(fds, rights...)
		}
		return conn, response, fds
	}
	conn, response, fds := acquire()
	defer conn.Close()
	if response.Error != "" || len(fds) != 1 { t.Fatalf("handoff failed: %+v %v", response, fds) }
	file := os.NewFile(uintptr(fds[0]), "received")
	buf := make([]byte, 14)
	if _, err := file.ReadAt(buf, 0); err != nil { t.Fatal(err) }
	if string(buf) != "packet-backend" { t.Fatalf("wrong descriptor: %q", buf) }
	if err := file.Close(); err != nil { t.Fatal(err) }
	if _, err := tap.Stat(); err != nil { t.Fatal("consumer closed helper-owned descriptor:", err) }
	second, response, fds := acquire()
	second.Close()
	for _, fd := range fds { unix.Close(fd) }
	if response.Error == "" || len(fds) != 0 { t.Fatal("accepted concurrent consumers") }
	conn.Close()
	cancel()
	if err := <-done; err != nil { t.Fatal(err) }
}
