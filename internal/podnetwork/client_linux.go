//go:build linux

package podnetwork

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"time"
)

func Status(ctx context.Context, path string, id Identity) (*Config, error) {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	conn, err := (&net.Dialer{}).DialContext(ctx, "unixpacket", path)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	}
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()
	b, err := json.Marshal(Request{Action: "status", Identity: id})
	if err != nil {
		return nil, err
	}
	if _, err := conn.Write(b); err != nil {
		return nil, err
	}
	b = make([]byte, MaxMessage)
	n, err := conn.Read(b)
	if err != nil {
		return nil, err
	}
	var response Response
	if err := json.Unmarshal(b[:n], &response); err != nil {
		return nil, err
	}
	if response.Error != "" {
		return nil, fmt.Errorf("network holder: %s", response.Error)
	}
	if response.Config == nil {
		return nil, fmt.Errorf("network holder returned no configuration")
	}
	if err := response.Config.Validate(); err != nil {
		return nil, err
	}
	if err := response.Config.Matches(id); err != nil {
		return nil, err
	}
	return response.Config, nil
}
