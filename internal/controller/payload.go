package controller

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	vmv1 "github.com/thebsdbox/kube-vm-controller/api/v1alpha1"
	"github.com/thebsdbox/kube-vm-controller/internal/podnetwork"
)

const (
	maxVMIDPrefix = 24
	// maxProvisionBytes is kube-vm's limit for all provisioned file content.
	maxProvisionBytes = 4 << 20
)

// VMID returns the uid used for the VM inside the node's kube-vm daemon. It is
// stable for the life of the object and unique across recreated objects.
func VMID(vm *vmv1.VirtualMachine) string {
	var b strings.Builder
	for _, r := range vm.Namespace + "-" + vm.Name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '.', r == '_', r == '-':
			b.WriteRune(r)
		default:
			b.WriteRune('-')
		}
	}
	prefix := b.String()
	if len(prefix) > maxVMIDPrefix {
		prefix = prefix[:maxVMIDPrefix]
	}
	sum := sha256.Sum256([]byte(types.UID(vm.UID)))
	return prefix + "-" + hex.EncodeToString(sum[:4])
}

// launchSettings is the part of the spec that can only change by restarting the VM.
type launchSettings struct {
	CPUs            int32                   `json:"cpus"`
	MemoryMiB       int32                   `json:"memoryMiB"`
	MemoryBalancing *vmv1.MemoryBalancing   `json:"memoryBalancing,omitempty"`
	Boot            *vmv1.BootSpec          `json:"boot,omitempty"`
	Disks           []vmv1.Disk             `json:"disks,omitempty"`
	AutoCOW         bool                    `json:"autoCOW,omitempty"`
	Network         []vmv1.NetworkInterface `json:"network,omitempty"`
	Vsock           *vmv1.VsockSpec         `json:"vsock,omitempty"`
	IOUring         *vmv1.IOUringSpec       `json:"ioUring,omitempty"`
}

// LaunchHash fingerprints the settings that require a restart to change. Idle
// policy, provisioning, run strategy, pause and node placement are excluded.
func LaunchHash(spec *vmv1.VirtualMachineSpec) string {
	data, _ := json.Marshal(launchSettings{
		CPUs: spec.CPUs, MemoryMiB: spec.MemoryMiB, MemoryBalancing: spec.MemoryBalancing,
		Boot: spec.Boot, Disks: spec.Disks, AutoCOW: spec.AutoCOW, Network: spec.Network,
		Vsock: spec.Vsock, IOUring: spec.IOUring,
	})
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:8])
}

// BlockSpec renders a disk as a kube-vm block specifier. Catalog base disks
// are not block specs; they use the separate base_disk payload key.
func BlockSpec(d vmv1.Disk) (string, bool) {
	var s string
	switch {
	case d.BaseDisk != "":
		return "", false
	case d.Path != "" && d.Overlay != "":
		return "cow:" + d.Path + "?overlay=" + d.Overlay, true
	case d.Path != "":
		s = d.Path
	case d.URL != "":
		return d.URL, true
	case d.Memory != nil:
		s = "mem:" + strconv.FormatInt(d.Memory.Value(), 10)
	default:
		return "", false
	}
	if d.ReadOnly {
		s += ":ro"
	}
	return s, true
}

// ProvisionPayload is the resolved first-boot provisioning for a start request.
type ProvisionPayload struct {
	Files []map[string]any
	SSH   map[string]any
}

// BuildStartPayload converts a VM spec into a kube-vm "start" payload.
// provision may be nil, in which case no provisioning is requested.
func BuildStartPayload(vm *vmv1.VirtualMachine, provision *ProvisionPayload) map[string]any {
	spec := &vm.Spec
	p := map[string]any{
		"uid": VMID(vm),
		"mem": spec.MemoryMiB,
		"cpu": spec.CPUs,
	}
	if b := spec.Boot; b != nil {
		setIf(p, "kernel", b.Kernel)
		setIf(p, "initrd", b.Initrd)
		setIf(p, "cmdline", b.Cmdline)
		setIf(p, "firmware", b.Firmware)
		setIf(p, "iso", b.ISO)
		if b.SerialConsole {
			p["serial_console"] = true
		}
	}
	if mb := spec.MemoryBalancing; mb != nil {
		p["memory_balancing"] = true
		if mb.MinMiB != nil {
			p["memory_min"] = *mb.MinMiB
		}
	}

	var blocks []string
	for _, d := range spec.Disks {
		if d.BaseDisk != "" {
			p["base_disk"] = d.BaseDisk
			continue
		}
		if s, ok := BlockSpec(d); ok {
			blocks = append(blocks, s)
		}
	}
	if len(blocks) > 0 {
		p["block"] = blocks
	}
	if spec.AutoCOW {
		p["auto_cow"] = true
	}

	var nics []map[string]any
	for _, n := range spec.Network {
		if n.Type == vmv1.NetworkTypePod {
			id := podnetwork.Identity{VMUID: string(vm.UID)}
			if binding := vm.Status.PodNetwork; binding != nil {
				id.PodUID, id.Generation, id.IPAddress = binding.PodUID, binding.Generation, binding.IPAddress
			}
			p["pod_network"] = id
			continue
		}
		nic := map[string]any{"nat": n.Type == vmv1.NetworkTypeNAT}
		setIf(nic, "tap", n.TapName)
		setIf(nic, "mac", n.MAC)
		if n.Switch {
			nic["switch"] = true
		}
		nics = append(nics, nic)
	}
	if len(nics) > 0 {
		p["nics"] = nics
	}

	if v := spec.Vsock; v != nil {
		p["vsock"] = v.Port
		if v.CID != nil {
			p["vsock-cid"] = *v.CID
		}
	}
	if io := spec.IOUring; io != nil {
		p["io_uring_block"] = true
		if io.QueueDepth > 0 {
			p["io_uring_queue_depth"] = io.QueueDepth
		}
	}

	if idle := spec.Idle; idle != nil {
		t, dt, wake := IdleSettings(idle)
		p["activity_timeout"] = t.String()
		p["activity_disk_timeout"] = dt.String()
		p["wake_on_network"] = wake
	}

	if provision != nil {
		if len(provision.Files) > 0 {
			p["provision"] = provision.Files
		}
		if provision.SSH != nil {
			p["ssh_access"] = provision.SSH
		}
	}
	return p
}

func setIf(m map[string]any, key, val string) {
	if val != "" {
		m[key] = val
	}
}

// IdleSettings returns the effective idle policy; wake-on-network defaults to true.
func IdleSettings(idle *vmv1.IdleSpec) (timeout, diskTimeout time.Duration, wake bool) {
	wake = true
	if idle.PauseAfter != nil {
		timeout = idle.PauseAfter.Duration
	}
	if idle.DiskPauseAfter != nil {
		diskTimeout = idle.DiskPauseAfter.Duration
	}
	if idle.WakeOnNetwork != nil {
		wake = *idle.WakeOnNetwork
	}
	return
}

// ResolveProvisioning reads every provisioned file's content and returns the
// daemon payload. Referenced Secrets and ConfigMaps are read from the VM's namespace.
func ResolveProvisioning(ctx context.Context, r client.Reader, vm *vmv1.VirtualMachine) (*ProvisionPayload, error) {
	prov := vm.Spec.Provisioning
	if prov == nil {
		return nil, nil
	}
	out := &ProvisionPayload{}
	total := 0
	for _, f := range prov.Files {
		entry := map[string]any{"path": f.Path}
		if f.Directory {
			entry["directory"] = true
		} else {
			data, err := fileContent(ctx, r, vm.Namespace, f)
			if err != nil {
				return nil, fmt.Errorf("provisioning file %q: %w", f.Path, err)
			}
			total += len(data)
			entry["data"] = base64.StdEncoding.EncodeToString(data)
		}
		setIf(entry, "mode", f.Mode)
		if f.UID != nil {
			entry["uid"] = *f.UID
		}
		if f.GID != nil {
			entry["gid"] = *f.GID
		}
		out.Files = append(out.Files, entry)
	}
	if total > maxProvisionBytes {
		return nil, fmt.Errorf("provisioned file content is %d bytes, above the %d byte limit", total, maxProvisionBytes)
	}
	if ssh := prov.SSH; ssh != nil {
		out.SSH = map[string]any{}
		setIf(out.SSH, "user", ssh.User)
		setIf(out.SSH, "home", ssh.Home)
	}
	return out, nil
}

func fileContent(ctx context.Context, r client.Reader, ns string, f vmv1.ProvisionFile) ([]byte, error) {
	switch {
	case f.Content != nil:
		return []byte(*f.Content), nil
	case f.Data != nil:
		return f.Data, nil
	case f.SecretKeyRef != nil:
		ref := f.SecretKeyRef
		var s corev1.Secret
		if err := r.Get(ctx, client.ObjectKey{Namespace: ns, Name: ref.Name}, &s); err != nil {
			if apierrors.IsNotFound(err) && ref.Optional != nil && *ref.Optional {
				return nil, nil
			}
			return nil, fmt.Errorf("reading Secret %q: %w", ref.Name, err)
		}
		v, ok := s.Data[ref.Key]
		if !ok && (ref.Optional == nil || !*ref.Optional) {
			return nil, fmt.Errorf("Secret %q has no key %q", ref.Name, ref.Key)
		}
		return v, nil
	case f.ConfigMapKeyRef != nil:
		ref := f.ConfigMapKeyRef
		var cm corev1.ConfigMap
		if err := r.Get(ctx, client.ObjectKey{Namespace: ns, Name: ref.Name}, &cm); err != nil {
			if apierrors.IsNotFound(err) && ref.Optional != nil && *ref.Optional {
				return nil, nil
			}
			return nil, fmt.Errorf("reading ConfigMap %q: %w", ref.Name, err)
		}
		if v, ok := cm.Data[ref.Key]; ok {
			return []byte(v), nil
		}
		if v, ok := cm.BinaryData[ref.Key]; ok {
			return v, nil
		}
		if ref.Optional == nil || !*ref.Optional {
			return nil, fmt.Errorf("ConfigMap %q has no key %q", ref.Name, ref.Key)
		}
		return nil, nil
	}
	return nil, nil
}
