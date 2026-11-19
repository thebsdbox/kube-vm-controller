package kubevm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"regexp"
	"sync"
	"time"
)

var uidPattern = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)

// Fake is an in-memory kube-vm daemon for tests and for cmd/fake-daemon. It
// follows the daemon semantics the controller relies on, but never boots a VM.
type Fake struct {
	mu  sync.Mutex
	vms map[string]*VMDetails
	n   int

	// Starts records every start payload received, in order.
	Starts []map[string]any
	// Calls records "<action>:<uid>" for each lifecycle call, in order.
	Calls []string
	// StartErr, when set, makes Start fail with a daemon error.
	StartErr string
	// Unavailable simulates an unreachable daemon (transport error).
	Unavailable bool
	// StopOnShutdown makes Shutdown move the VM to stopped immediately (default behaviour of New).
	StopOnShutdown bool
}

var _ Client = (*Fake)(nil)

// NewFake returns an empty fake daemon whose Shutdown stops VMs immediately.
func NewFake() *Fake {
	return &Fake{vms: map[string]*VMDetails{}, StopOnShutdown: true}
}

var errUnavailable = errors.New("connecting to kube-vm daemon: connection refused")

func (f *Fake) record(action, uid string) { f.Calls = append(f.Calls, action+":"+uid) }

// SetState forces a VM state, as if the guest or the idle monitor changed it.
func (f *Fake) SetState(uid, state string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if vm := f.vms[uid]; vm != nil {
		vm.State = state
	}
}

// Add inserts a VM directly, as if it was started outside the controller.
func (f *Fake) Add(vm VMDetails) {
	f.mu.Lock()
	defer f.mu.Unlock()
	v := vm
	f.vms[vm.UID] = &v
}

// Get returns a copy of a VM, or nil.
func (f *Fake) Get(uid string) *VMDetails {
	f.mu.Lock()
	defer f.mu.Unlock()
	if vm := f.vms[uid]; vm != nil {
		c := *vm
		return &c
	}
	return nil
}

// CallLog returns a copy of the recorded calls.
func (f *Fake) CallLog() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.Calls...)
}

// StartPayloads returns a copy of the recorded start payloads.
func (f *Fake) StartPayloads() []map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]map[string]any(nil), f.Starts...)
}

// Examine implements Client.
func (f *Fake) Examine(_ context.Context, uid string) (*VMDetails, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.Unavailable {
		return nil, errUnavailable
	}
	vm := f.vms[uid]
	if vm == nil {
		return nil, ErrNotFound
	}
	c := *vm
	c.NetworkInterfaces = append([]NetworkInterface(nil), vm.NetworkInterfaces...)
	return &c, nil
}

// Start implements Client.
func (f *Fake) Start(_ context.Context, payload map[string]any) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.Unavailable {
		return errUnavailable
	}
	uid, _ := payload["uid"].(string)
	f.record(ActionStart, uid)
	if f.StartErr != "" {
		return &DaemonError{Action: ActionStart, Message: f.StartErr}
	}
	if !uidPattern.MatchString(uid) {
		return &DaemonError{Action: ActionStart, Message: "invalid uid"}
	}
	if vm := f.vms[uid]; vm != nil && vm.State != StateStopped {
		return &DaemonError{Action: ActionStart, Message: "vm: uid already exists"}
	}
	f.Starts = append(f.Starts, payload)
	f.n++
	vm := &VMDetails{
		UID:           uid,
		State:         StateRunning,
		StartedAt:     time.Now().UTC().Format(time.RFC3339),
		WakeOnNetwork: true,
		SSHPrivateKey: "fake-private-key-" + uid,
		SSHPublicKey:  "ssh-ed25519 AAAAfake " + uid,
		SSHUser:       "root",
	}
	vm.MemMiB = toInt(payload["mem"])
	vm.NumCPU = toInt(payload["cpu"])
	if s, ok := payload["activity_timeout"].(string); ok {
		vm.ActivityTimeout = s
	}
	if s, ok := payload["activity_disk_timeout"].(string); ok {
		vm.ActivityDiskTimeout = s
	}
	if b, ok := payload["wake_on_network"].(bool); ok {
		vm.WakeOnNetwork = b
	}
	if nics, ok := payload["nics"].([]map[string]any); ok {
		f.addNICs(vm, nics)
	} else if raw, ok := payload["nics"].([]any); ok {
		var nics []map[string]any
		for _, r := range raw {
			if m, ok := r.(map[string]any); ok {
				nics = append(nics, m)
			}
		}
		f.addNICs(vm, nics)
	}
	f.vms[uid] = vm
	return nil
}

func (f *Fake) addNICs(vm *VMDetails, nics []map[string]any) {
	for i, nic := range nics {
		ni := NetworkInterface{Tap: fmt.Sprintf("tap%d-%d", f.n, i)}
		ni.NAT, _ = nic["nat"].(bool)
		ni.MAC, _ = nic["mac"].(string)
		ni.Switch, _ = nic["switch"].(bool)
		if t, ok := nic["tap"].(string); ok && t != "" {
			ni.Tap = t
		}
		if ni.NAT {
			ni.NATGateway = fmt.Sprintf("192.168.%d.1", 100+f.n)
			ni.NATGuestIP = fmt.Sprintf("192.168.%d.2", 100+f.n)
			if !vm.NAT {
				vm.NAT = true
				vm.NATGateway = ni.NATGateway
				vm.NATGuestIP = ni.NATGuestIP
			}
		}
		vm.NetworkInterfaces = append(vm.NetworkInterfaces, ni)
	}
}

func toInt(v any) int {
	switch n := v.(type) {
	case int:
		return n
	case int32:
		return int(n)
	case int64:
		return int(n)
	case float64:
		return int(n)
	}
	return 0
}

// Pause implements Client.
func (f *Fake) Pause(_ context.Context, uid string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.Unavailable {
		return errUnavailable
	}
	f.record(ActionPause, uid)
	vm := f.vms[uid]
	if vm == nil || vm.State == StateStopped {
		return &DaemonError{Action: ActionPause, Message: "vm: instance is not running"}
	}
	vm.State = StatePaused
	return nil
}

// Resume implements Client.
func (f *Fake) Resume(_ context.Context, uid string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.Unavailable {
		return errUnavailable
	}
	f.record(ActionResume, uid)
	vm := f.vms[uid]
	if vm == nil || vm.State == StateStopped {
		return &DaemonError{Action: ActionResume, Message: "vm: instance is not running"}
	}
	vm.State = StateRunning
	return nil
}

// Shutdown implements Client.
func (f *Fake) Shutdown(_ context.Context, uid string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.Unavailable {
		return errUnavailable
	}
	f.record(ActionShutdown, uid)
	if vm := f.vms[uid]; vm != nil && f.StopOnShutdown {
		vm.State = StateStopped
	}
	return nil
}

// Delete implements Client.
func (f *Fake) Delete(_ context.Context, uid string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.Unavailable {
		return errUnavailable
	}
	f.record(ActionDelete, uid)
	vm := f.vms[uid]
	if vm == nil {
		return ErrNotFound
	}
	if vm.State != StateStopped {
		return &DaemonError{Action: ActionDelete, Message: "vm: instance is still stopping"}
	}
	delete(f.vms, uid)
	return nil
}

// SetActivity implements Client.
func (f *Fake) SetActivity(_ context.Context, uid string, timeout, diskTimeout time.Duration, wake bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.Unavailable {
		return errUnavailable
	}
	f.record(ActionActivityTimeout, uid)
	vm := f.vms[uid]
	if vm == nil {
		return &DaemonError{Action: ActionActivityTimeout, Message: "vm: instance is not running"}
	}
	vm.ActivityTimeout = timeout.String()
	vm.ActivityDiskTimeout = diskTimeout.String()
	vm.WakeOnNetwork = wake
	return nil
}

// Ping implements Client.
func (f *Fake) Ping(context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.Unavailable {
		return errUnavailable
	}
	return nil
}

// Serve answers control socket requests on l until it is closed.
func (f *Fake) Serve(l net.Listener) error {
	for {
		conn, err := l.Accept()
		if err != nil {
			return err
		}
		go f.handle(conn)
	}
}

func (f *Fake) handle(conn net.Conn) {
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(30 * time.Second))
	var req Request
	if err := json.NewDecoder(conn).Decode(&req); err != nil {
		return
	}
	ctx := context.Background()
	uid, _ := req.Payload["uid"].(string)
	var err error
	resp := Response{OK: true, UID: uid}
	switch req.Action {
	case ActionStart:
		err = f.Start(ctx, req.Payload)
	case ActionExamine:
		var vm *VMDetails
		vm, err = f.Examine(ctx, uid)
		if errors.Is(err, ErrNotFound) {
			err = &DaemonError{Action: req.Action, Message: "vm: instance is not running"}
		}
		resp.VM = vm
	case ActionPause:
		err = f.Pause(ctx, uid)
	case ActionResume:
		err = f.Resume(ctx, uid)
	case ActionShutdown:
		err = f.Shutdown(ctx, uid)
	case ActionDelete:
		err = f.Delete(ctx, uid)
		if errors.Is(err, ErrNotFound) {
			err = &DaemonError{Action: req.Action, Message: "unknown uid"}
		}
	case ActionActivityTimeout:
		var timeout, disk time.Duration
		if s, ok := req.Payload["activity_timeout"].(string); ok {
			timeout, _ = time.ParseDuration(s)
		}
		if s, ok := req.Payload["activity_disk_timeout"].(string); ok {
			disk, _ = time.ParseDuration(s)
		}
		wake, _ := req.Payload["wake_on_network"].(bool)
		err = f.SetActivity(ctx, uid, timeout, disk, wake)
	case ActionList:
	default:
		err = &DaemonError{Action: req.Action, Message: "unknown action"}
	}
	if err != nil {
		var de *DaemonError
		if errors.As(err, &de) {
			resp = Response{Error: de.Message}
		} else {
			resp = Response{Error: err.Error()}
		}
	}
	_ = json.NewEncoder(conn).Encode(resp)
}
