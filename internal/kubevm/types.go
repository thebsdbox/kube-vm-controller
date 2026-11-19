// Package kubevm is a client for the kube-vm daemon control socket.
package kubevm

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/thebsdbox/kube-vm-controller/internal/podnetwork"
)

// DefaultSocket is where `kube-vm --server` listens.
const DefaultSocket = "/run/kube-vm/control.sock"

// Daemon-reported VM states.
const (
	StateRunning = "running"
	StateIdle    = "idle"
	StatePaused  = "paused"
	StateStopped = "stopped"
)

// Control socket actions.
const (
	ActionStart           = "start"
	ActionExamine         = "examine"
	ActionPause           = "pause"
	ActionResume          = "resume"
	ActionShutdown        = "shutdown"
	ActionDelete          = "delete"
	ActionActivityTimeout = "activity-timeout"
	ActionList            = "list"
)

// Request is one control socket request.
type Request struct {
	Action  string         `json:"action"`
	Payload map[string]any `json:"payload,omitempty"`
}

// Response is one control socket response.
type Response struct {
	OK      bool       `json:"ok"`
	UID     string     `json:"uid,omitempty"`
	State   string     `json:"state,omitempty"`
	VM      *VMDetails `json:"vm,omitempty"`
	Message string     `json:"message,omitempty"`
	Error   string     `json:"error,omitempty"`
}

// NetworkInterface is a NIC reported by examine.
type NetworkInterface struct {
	Tap        string `json:"tap"`
	NAT        bool   `json:"nat"`
	Vhost      bool   `json:"vhost"`
	MAC        string `json:"mac,omitempty"`
	Switch     bool   `json:"switch,omitempty"`
	NATSubnet  string `json:"nat_subnet,omitempty"`
	NATGateway string `json:"nat_gateway,omitempty"`
	NATGuestIP string `json:"nat_guest_ip,omitempty"`
}

// VMDetails is the subset of the daemon's examine output the controller uses.
type VMDetails struct {
	PodNetwork          *podnetwork.Identity `json:"pod_network,omitempty"`
	UID                 string               `json:"uid,omitempty"`
	State               string               `json:"state,omitempty"`
	StartedAt           string               `json:"started_at,omitempty"`
	MemMiB              int                  `json:"mem_mib,omitempty"`
	NumCPU              int                  `json:"num_cpu,omitempty"`
	ActivityTimeout     string               `json:"activity_timeout,omitempty"`
	ActivityDiskTimeout string               `json:"activity_disk_timeout,omitempty"`
	WakeOnNetwork       bool                 `json:"wake_on_network"`
	NAT                 bool                 `json:"nat,omitempty"`
	NATGateway          string               `json:"nat_gateway,omitempty"`
	NATGuestIP          string               `json:"nat_guest_ip,omitempty"`
	NetworkInterfaces   []NetworkInterface   `json:"network_interfaces,omitempty"`
	SSHPrivateKey       string               `json:"ssh_private_key,omitempty"`
	SSHPublicKey        string               `json:"ssh_public_key,omitempty"`
	SSHUser             string               `json:"ssh_user,omitempty"`
	LastError           string               `json:"last_error,omitempty"`
}

// ErrNotFound means the daemon has no VM with the requested uid.
var ErrNotFound = errors.New("kubevm: vm not found")

// DaemonError is an error reported by the daemon itself, as opposed to a
// failure to talk to it.
type DaemonError struct {
	Action  string
	Message string
}

func (e *DaemonError) Error() string {
	return fmt.Sprintf("kube-vm %s: %s", e.Action, e.Message)
}

// IsDaemonError reports whether err came from the daemon rather than the transport.
func IsDaemonError(err error) bool {
	var de *DaemonError
	return errors.As(err, &de)
}

// IsNotFound reports whether err means the VM does not exist in the daemon.
func IsNotFound(err error) bool { return errors.Is(err, ErrNotFound) }

// Client is the VM lifecycle API the controller needs.
type Client interface {
	// Examine returns VM details, or ErrNotFound.
	Examine(ctx context.Context, uid string) (*VMDetails, error)
	// Start launches a VM from a daemon start payload (which includes "uid").
	Start(ctx context.Context, payload map[string]any) error
	Pause(ctx context.Context, uid string) error
	Resume(ctx context.Context, uid string) error
	// Shutdown asks the guest to stop; unknown uids are not an error.
	Shutdown(ctx context.Context, uid string) error
	// Delete removes a stopped VM and its managed overlays. Unknown uids return ErrNotFound.
	Delete(ctx context.Context, uid string) error
	// SetActivity updates idle policy on a live VM. Zero durations disable.
	SetActivity(ctx context.Context, uid string, timeout, diskTimeout time.Duration, wakeOnNetwork bool) error
	// Ping checks that the daemon socket is reachable.
	Ping(ctx context.Context) error
}

// PodNetworkClient is optional so legacy daemon integrations remain usable.
type PodNetworkClient interface {
	CheckPodNetworkSupport(ctx context.Context) error
}
