// Package podnetwork implements the experimental, Pod-local TAP handoff protocol.
package podnetwork

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net"
	"net/netip"
	"strings"
)

const (
	Version        = 1
	MaxMessage     = 8192
	ReadyCondition = "vm.kube-vm.io/NetworkReady"
)

type Identity struct {
	VMUID      string `json:"vm_uid"`
	PodUID     string `json:"pod_uid"`
	Generation string `json:"generation,omitempty"`
	IPAddress  string `json:"ip_address,omitempty"`
}

type Route struct {
	Destination string `json:"destination,omitempty"`
	Gateway     string `json:"gateway,omitempty"`
	LinkScope   bool   `json:"link_scope,omitempty"`
	Metric      int    `json:"metric,omitempty"`
}

type Config struct {
	Version int `json:"version"`
	Identity
	Namespace string  `json:"namespace"`
	MAC       string  `json:"mac"`
	Address   string  `json:"address"`
	MTU       int     `json:"mtu"`
	Routes    []Route `json:"routes"`
	Resolver  string  `json:"resolver"`
}

type Request struct {
	Action string `json:"action"`
	Identity
}

type Response struct {
	Config *Config `json:"config,omitempty"`
	Error  string  `json:"error,omitempty"`
}

func (c *Config) Validate() error {
	if c.Version != Version || c.VMUID == "" || c.PodUID == "" || c.Generation == "" || c.Namespace == "" {
		return fmt.Errorf("invalid Pod-network version or identity")
	}
	prefix, err := netip.ParsePrefix(c.Address)
	if err != nil || !prefix.Addr().Is4() || prefix.Addr().IsUnspecified() || prefix.Addr().IsMulticast() || prefix.Addr().String() != c.IPAddress {
		return fmt.Errorf("invalid Pod IPv4 address %q", c.Address)
	}
	mac, err := net.ParseMAC(c.MAC)
	if err != nil || len(mac) != 6 || mac[0]&1 != 0 {
		return fmt.Errorf("invalid Pod MAC %q", c.MAC)
	}
	if c.MTU < 576 || c.MTU > 65535 || len(c.Routes) == 0 || len(c.Routes) > 16 {
		return fmt.Errorf("unsupported MTU or route count")
	}
	for _, route := range c.Routes {
		if route.Destination != "" {
			p, err := netip.ParsePrefix(route.Destination)
			if err != nil || !p.Addr().Is4() {
				return fmt.Errorf("invalid IPv4 route %q", route.Destination)
			}
		}
		if route.Gateway != "" {
			gw, err := netip.ParseAddr(route.Gateway)
			if err != nil || !gw.Is4() || gw.IsUnspecified() || gw.IsMulticast() {
				return fmt.Errorf("invalid IPv4 gateway %q", route.Gateway)
			}
		}
		if route.Metric < 0 {
			return fmt.Errorf("invalid route metric")
		}
	}
	if c.Resolver == "" || len(c.Resolver) > 2048 || strings.ContainsRune(c.Resolver, 0) {
		return fmt.Errorf("missing or oversized Pod resolver configuration")
	}
	return nil
}

func (c *Config) Matches(id Identity) error {
	if c.VMUID != id.VMUID || c.PodUID != id.PodUID ||
		(id.Generation != "" && c.Generation != id.Generation) ||
		(id.IPAddress != "" && c.IPAddress != id.IPAddress) {
		return fmt.Errorf("Pod-network identity or allocation changed")
	}
	return nil
}

func (c *Config) Encode() (string, error) {
	if err := c.Validate(); err != nil {
		return "", err
	}
	b, err := json.Marshal(c)
	if err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}
