package kubevm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"strings"
	"time"
)

const (
	dialTimeout    = 2 * time.Second
	defaultTimeout = 20 * time.Second
	startTimeout   = 90 * time.Second
)

// SocketClient talks to a kube-vm daemon over its Unix control socket.
type SocketClient struct {
	Path string
}

var _ Client = (*SocketClient)(nil)

// NewSocketClient returns a client for the given socket path.
func NewSocketClient(path string) *SocketClient {
	if path == "" {
		path = DefaultSocket
	}
	return &SocketClient{Path: path}
}

// do sends one request. The daemon has no read deadline, so every call is
// bounded by ctx or by a per-action timeout.
func (c *SocketClient) do(ctx context.Context, action string, payload map[string]any) (*Response, error) {
	timeout := defaultTimeout
	if action == ActionStart {
		timeout = startTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	d := net.Dialer{Timeout: dialTimeout}
	conn, err := d.DialContext(ctx, "unix", c.Path)
	if err != nil {
		return nil, fmt.Errorf("connecting to kube-vm daemon: %w", err)
	}
	defer conn.Close()
	if dl, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(dl)
	}
	stop := context.AfterFunc(ctx, func() { _ = conn.SetDeadline(time.Unix(1, 0)) })
	defer stop()

	if err := json.NewEncoder(conn).Encode(Request{Action: action, Payload: payload}); err != nil {
		return nil, fmt.Errorf("sending %s request: %w", action, err)
	}
	var resp Response
	if err := json.NewDecoder(conn).Decode(&resp); err != nil {
		return nil, fmt.Errorf("reading %s response: %w", action, err)
	}
	if resp.Error != "" || !resp.OK {
		msg := resp.Error
		if msg == "" {
			msg = "request failed"
		}
		return &resp, &DaemonError{Action: action, Message: msg}
	}
	return &resp, nil
}

func asDaemonError(err error, target **DaemonError) bool { return errors.As(err, target) }

func uidPayload(uid string) map[string]any { return map[string]any{"uid": uid} }

func isUnknown(msg string) bool {
	msg = strings.ToLower(msg)
	return strings.Contains(msg, "not running") ||
		strings.Contains(msg, "unknown uid") ||
		strings.Contains(msg, "not found")
}

// Examine implements Client.
func (c *SocketClient) Examine(ctx context.Context, uid string) (*VMDetails, error) {
	resp, err := c.do(ctx, ActionExamine, uidPayload(uid))
	if err != nil {
		var de *DaemonError
		if asDaemonError(err, &de) && isUnknown(de.Message) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	if resp.VM == nil {
		return nil, ErrNotFound
	}
	return resp.VM, nil
}

// Start implements Client.
func (c *SocketClient) Start(ctx context.Context, payload map[string]any) error {
	_, err := c.do(ctx, ActionStart, payload)
	return err
}

// Pause implements Client.
func (c *SocketClient) Pause(ctx context.Context, uid string) error {
	_, err := c.do(ctx, ActionPause, uidPayload(uid))
	return err
}

// Resume implements Client.
func (c *SocketClient) Resume(ctx context.Context, uid string) error {
	_, err := c.do(ctx, ActionResume, uidPayload(uid))
	return err
}

// Shutdown implements Client.
func (c *SocketClient) Shutdown(ctx context.Context, uid string) error {
	_, err := c.do(ctx, ActionShutdown, uidPayload(uid))
	return err
}

// Delete implements Client.
func (c *SocketClient) Delete(ctx context.Context, uid string) error {
	_, err := c.do(ctx, ActionDelete, uidPayload(uid))
	var de *DaemonError
	if asDaemonError(err, &de) && isUnknown(de.Message) {
		return ErrNotFound
	}
	return err
}

// SetActivity implements Client.
func (c *SocketClient) SetActivity(ctx context.Context, uid string, timeout, diskTimeout time.Duration, wakeOnNetwork bool) error {
	_, err := c.do(ctx, ActionActivityTimeout, map[string]any{
		"uid":                   uid,
		"activity_timeout":      timeout.String(),
		"activity_disk_timeout": diskTimeout.String(),
		"wake_on_network":       wakeOnNetwork,
	})
	return err
}

// Ping implements Client.
func (c *SocketClient) Ping(ctx context.Context) error {
	_, err := c.do(ctx, ActionList, nil)
	return err
}

func (c *SocketClient) CheckPodNetworkSupport(ctx context.Context) error {
	response, err := c.do(ctx, "pod-network-support", nil)
	if err != nil {
		return err
	}
	if response.Message != "v1" {
		return fmt.Errorf("unsupported Pod-network protocol %q", response.Message)
	}
	return nil
}
