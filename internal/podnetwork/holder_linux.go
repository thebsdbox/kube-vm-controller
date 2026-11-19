//go:build linux

package podnetwork

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
)

const tapName = "kvmtap0"
const bridgeName = "kvmbr0"

// Prepare only operates in the caller's Pod namespace. State survives container,
// but not Pod, replacement; the namespace identity detects sandbox recreation.
func Prepare(id Identity, statePath string) (*Config, *os.File, error) {
	namespace, err := os.Readlink("/proc/self/ns/net")
	if err != nil {
		return nil, nil, err
	}
	link, err := netlink.LinkByName("eth0")
	if err != nil {
		return nil, nil, err
	}
	if link.Type() != "veth" {
		return nil, nil, fmt.Errorf("Pod networking requires veth, got %s", link.Type())
	}
	var cfg Config
	data, err := os.ReadFile(statePath)
	if err == nil {
		if err := json.Unmarshal(data, &cfg); err != nil {
			return nil, nil, fmt.Errorf("read saved network: %w", err)
		}
		if err := cfg.Validate(); err != nil {
			return nil, nil, err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, nil, err
	}
	if cfg.Namespace != namespace {
		cfg, err = discover(link, id, namespace)
		if err != nil {
			return nil, nil, err
		}
		data, err = json.Marshal(cfg)
		if err != nil {
			return nil, nil, err
		}
		if err := os.WriteFile(statePath+".tmp", data, 0600); err != nil {
			return nil, nil, err
		}
		if err := os.Rename(statePath+".tmp", statePath); err != nil {
			return nil, nil, err
		}
	}
	if err := cfg.Matches(id); err != nil {
		return nil, nil, err
	}
	file, err := wire(link, &cfg)
	if err != nil {
		return nil, nil, err
	}
	return &cfg, file, nil
}

func discover(link netlink.Link, id Identity, namespace string) (Config, error) {
	cfg := Config{Version: Version, Identity: id, Namespace: namespace, MAC: link.Attrs().HardwareAddr.String(), MTU: link.Attrs().MTU}
	addresses, err := netlink.AddrList(link, netlink.FAMILY_V4)
	if err != nil {
		return cfg, err
	}
	if len(addresses) != 1 {
		return cfg, fmt.Errorf("expected one Pod IPv4 address, got %d", len(addresses))
	}
	cfg.Address = addresses[0].IPNet.String()
	cfg.IPAddress = addresses[0].IP.String()
	ipv6, err := netlink.AddrList(link, netlink.FAMILY_V6)
	if err != nil {
		return cfg, err
	}
	for _, address := range ipv6 {
		if !address.IP.IsLinkLocalUnicast() {
			return cfg, fmt.Errorf("dual-stack Pod networking is not supported")
		}
	}
	nonce := make([]byte, 16)
	if _, err := rand.Read(nonce); err != nil {
		return cfg, err
	}
	cfg.Generation = hex.EncodeToString(nonce)
	routes, err := netlink.RouteList(link, netlink.FAMILY_V4)
	if err != nil {
		return cfg, err
	}
	for _, route := range routes {
		if route.Table != unix.RT_TABLE_MAIN || len(route.MultiPath) != 0 || route.Encap != nil ||
			(route.Type != 0 && route.Type != unix.RTN_UNICAST) ||
			(route.Scope != netlink.SCOPE_LINK && route.Scope != netlink.SCOPE_UNIVERSE) {
			return cfg, fmt.Errorf("unsupported Pod route: %s", route.String())
		}
		r := Route{LinkScope: route.Scope == netlink.SCOPE_LINK, Metric: route.Priority}
		if route.Dst != nil {
			r.Destination = route.Dst.String()
		}
		if route.Gw != nil {
			r.Gateway = route.Gw.String()
		}
		cfg.Routes = append(cfg.Routes, r)
	}
	resolver, err := os.ReadFile("/etc/resolv.conf")
	if err != nil {
		return cfg, err
	}
	cfg.Resolver = string(resolver)
	return cfg, cfg.Validate()
}

func wire(uplink netlink.Link, cfg *Config) (*os.File, error) {
	fd, err := unix.Open("/dev/net/tun", unix.O_RDWR|unix.O_CLOEXEC|unix.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), tapName)
	ok := false
	defer func() {
		if !ok {
			_ = file.Close()
		}
	}()
	req, err := unix.NewIfreq(tapName)
	if err != nil {
		return nil, err
	}
	req.SetUint16(unix.IFF_TAP | unix.IFF_NO_PI)
	if err := unix.IoctlIfreq(fd, unix.TUNSETIFF, req); err != nil {
		return nil, fmt.Errorf("open Pod TAP: %w", err)
	}
	tap, err := netlink.LinkByName(tapName)
	if err != nil {
		return nil, err
	}
	bridge, err := netlink.LinkByName(bridgeName)
	if err != nil {
		var notFound netlink.LinkNotFoundError
		if !errors.As(err, &notFound) {
			return nil, err
		}
		b := &netlink.Bridge{LinkAttrs: netlink.LinkAttrs{Name: bridgeName, MTU: cfg.MTU}}
		if err := netlink.LinkAdd(b); err != nil {
			return nil, err
		}
		bridge = b
	} else if bridge.Type() != "bridge" {
		return nil, fmt.Errorf("%s is not a bridge", bridgeName)
	}
	// The guest owns the original MAC; the Pod stack must not consume its frames.
	mac, err := net.ParseMAC(cfg.MAC)
	if err != nil {
		return nil, err
	}
	mac[0] = (mac[0] | 2) & 0xfe
	mac[5] ^= 0x80
	if err := netlink.LinkSetHardwareAddr(uplink, mac); err != nil {
		return nil, err
	}
	if err := netlink.LinkSetHardwareAddr(bridge, mac); err != nil {
		return nil, err
	}
	for _, link := range []netlink.Link{tap, bridge} {
		if err := netlink.LinkSetMTU(link, cfg.MTU); err != nil {
			return nil, err
		}
	}
	addresses, err := netlink.AddrList(uplink, netlink.FAMILY_V4)
	if err != nil {
		return nil, err
	}
	for _, address := range addresses {
		if address.IP.String() != cfg.IPAddress {
			return nil, fmt.Errorf("unexpected address on Pod uplink")
		}
		if err := netlink.AddrDel(uplink, &address); err != nil {
			return nil, err
		}
	}
	for _, link := range []netlink.Link{uplink, tap} {
		if err := netlink.LinkSetMaster(link, bridge); err != nil {
			return nil, err
		}
		if err := netlink.LinkSetUp(link); err != nil {
			return nil, err
		}
	}
	if err := netlink.LinkSetUp(bridge); err != nil {
		return nil, err
	}
	ok = true
	return file, nil
}

// Serve keeps the TAP open across VM restarts. An acquire connection owns one
// duplicated descriptor until the consumer closes its connection.
func Serve(ctx context.Context, socket string, cfg *Config, tap *os.File) error {
	if err := cfg.Validate(); err != nil {
		return err
	}
	if info, err := os.Lstat(socket); err == nil {
		if info.Mode()&os.ModeSocket == 0 {
			return fmt.Errorf("refusing to replace non-socket %s", socket)
		}
		if err := os.Remove(socket); err != nil {
			return err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	listener, err := net.ListenUnix("unixpacket", &net.UnixAddr{Name: socket, Net: "unixpacket"})
	if err != nil {
		return err
	}
	defer listener.Close()
	if err := os.Chmod(socket, 0600); err != nil {
		return err
	}
	stop := context.AfterFunc(ctx, func() { _ = listener.Close() })
	defer stop()
	var mu sync.Mutex
	active := false
	var wg sync.WaitGroup
	defer wg.Wait()
	for {
		conn, err := listener.AcceptUnix()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer conn.Close()
			stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
			defer stop()
			_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
			var request Request
			buf := make([]byte, MaxMessage)
			n, err := conn.Read(buf)
			if err == nil {
				err = json.Unmarshal(buf[:n], &request)
			}
			if err == nil {
				err = cfg.Matches(request.Identity)
			}
			if err == nil && request.Action != "status" && request.Action != "acquire" {
				err = fmt.Errorf("unknown attachment action")
			}
			owned := false
			if err == nil && request.Action == "acquire" {
				if request.Generation == "" || request.IPAddress == "" {
					err = fmt.Errorf("acquire requires allocation and generation")
				} else {
					mu.Lock()
					if active {
						err = fmt.Errorf("TAP already has an active consumer")
					} else {
						active, owned = true, true
					}
					mu.Unlock()
				}
			}
			if owned {
				defer func() { mu.Lock(); active = false; mu.Unlock() }()
			}
			response := Response{Config: cfg}
			if err != nil {
				response = Response{Error: err.Error()}
			}
			data, marshalErr := json.Marshal(response)
			if marshalErr != nil {
				slog.Error("encode attachment response", "error", marshalErr)
				return
			}
			var rights []byte
			if owned {
				rights = unix.UnixRights(int(tap.Fd()))
			}
			if _, _, err := conn.WriteMsgUnix(data, rights, nil); err != nil {
				slog.Error("send attachment response", "error", err)
				return
			}
			if owned {
				_ = conn.SetDeadline(time.Time{})
				if _, err := io.Copy(io.Discard, conn); err != nil && ctx.Err() == nil {
					slog.Error("attachment connection", "error", err)
				}
			}
		}()
	}
}

func SocketPath(root, vmID string) string { return filepath.Join(root, vmID, "network.sock") }
