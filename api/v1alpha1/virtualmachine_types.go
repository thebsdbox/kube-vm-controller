package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// FinalizerName is held on a VirtualMachine while its kube-vm instance exists on a node.
const FinalizerName = "vm.kube-vm.io/finalizer"

// RunStrategy controls whether the controller keeps the VM running.
// +kubebuilder:validation:Enum=Always;Once;Halted
type RunStrategy string

const (
	// RunStrategyAlways starts the VM and restarts it whenever it stops.
	RunStrategyAlways RunStrategy = "Always"
	// RunStrategyOnce starts the VM a single time and leaves it stopped afterwards.
	RunStrategyOnce RunStrategy = "Once"
	// RunStrategyHalted keeps the VM stopped.
	RunStrategyHalted RunStrategy = "Halted"
)

// UpdateStrategy controls what happens when launch-time settings change.
// +kubebuilder:validation:Enum=Restart;Manual
type UpdateStrategy string

const (
	// UpdateStrategyRestart restarts the VM to apply changed launch settings.
	UpdateStrategyRestart UpdateStrategy = "Restart"
	// UpdateStrategyManual only reports RestartRequired; settings apply on the next start.
	UpdateStrategyManual UpdateStrategy = "Manual"
)

// NetworkType selects how a guest NIC is attached.
// +kubebuilder:validation:Enum=NAT;TAP;Pod
type NetworkType string

const (
	// NetworkTypeNAT creates a TAP device with host NAT.
	NetworkTypeNAT NetworkType = "NAT"
	// NetworkTypeTAP attaches to an existing host TAP device.
	NetworkTypeTAP NetworkType = "TAP"
	// NetworkTypePod delegates a holder Pod's IPv4 to the guest (experimental).
	NetworkTypePod NetworkType = "Pod"
)

// Phase is a coarse summary of the VM lifecycle.
type Phase string

const (
	PhasePending     Phase = "Pending"
	PhaseScheduled   Phase = "Scheduled"
	PhaseStarting    Phase = "Starting"
	PhaseRunning     Phase = "Running"
	PhaseIdle        Phase = "Idle"
	PhasePaused      Phase = "Paused"
	PhaseStopped     Phase = "Stopped"
	PhaseFailed      Phase = "Failed"
	PhaseTerminating Phase = "Terminating"
)

// Condition types reported on a VirtualMachine.
const (
	ConditionScheduled       = "Scheduled"
	ConditionReady           = "Ready"
	ConditionRestartRequired = "RestartRequired"
)

// BootSpec configures how the guest is booted.
type BootSpec struct {
	// Kernel is a kernel image path or URL on the node.
	// +optional
	Kernel string `json:"kernel,omitempty"`
	// Initrd is an initial ramdisk path or URL on the node.
	// +optional
	Initrd string `json:"initrd,omitempty"`
	// Cmdline is the guest kernel command line. When empty kube-vm uses
	// "console=hvc0 root=/dev/vda reboot=t".
	// +optional
	Cmdline string `json:"cmdline,omitempty"`
	// Firmware is an experimental firmware image path or URL used instead of a kernel.
	// +optional
	Firmware string `json:"firmware,omitempty"`
	// ISO is an ISO path or URL attached as a read-only block device.
	// +optional
	ISO string `json:"iso,omitempty"`
	// SerialConsole additionally enables a serial console.
	// +optional
	SerialConsole bool `json:"serialConsole,omitempty"`
}

// MemoryBalancing opts the VM in to virtio balloon memory redistribution.
type MemoryBalancing struct {
	// MinMiB is the guest floor in MiB. When unset kube-vm applies its own default.
	// +kubebuilder:validation:Minimum=1
	// +optional
	MinMiB *int32 `json:"minMiB,omitempty"`
}

// Disk attaches one virtio block device. Exactly one source must be set.
// +kubebuilder:validation:XValidation:rule="[has(self.baseDisk), has(self.path), has(self.url), has(self.memory)].filter(x, x).size() == 1",message="exactly one of baseDisk, path, url or memory must be set"
// +kubebuilder:validation:XValidation:rule="!has(self.overlay) || has(self.path)",message="overlay requires path"
// +kubebuilder:validation:XValidation:rule="!(has(self.readOnly) && self.readOnly) || !(has(self.baseDisk) || has(self.overlay))",message="a copy-on-write disk cannot be readOnly"
// +kubebuilder:validation:XValidation:rule="!has(self.url) || self.url.startsWith('http://') || self.url.startsWith('https://')",message="url must be http or https"
type Disk struct {
	// BaseDisk names an image in the node's kube-vm base-disk catalog. kube-vm
	// creates a per-VM copy-on-write overlay, so the base image is never modified.
	// At most one baseDisk is allowed; it is attached after the other disks.
	// +kubebuilder:validation:MinLength=1
	// +optional
	BaseDisk string `json:"baseDisk,omitempty"`
	// Path is an absolute image path on the node.
	// +kubebuilder:validation:Pattern=`^/[^?#]*$`
	// +optional
	Path string `json:"path,omitempty"`
	// Overlay makes path a copy-on-write base by redirecting writes to this absolute overlay path.
	// +kubebuilder:validation:Pattern=`^/[^?#]*$`
	// +optional
	Overlay string `json:"overlay,omitempty"`
	// URL is a remote http(s) image; remote disks are read-only.
	// +optional
	URL string `json:"url,omitempty"`
	// Memory creates a RAM-backed disk of this size.
	// +optional
	Memory *resource.Quantity `json:"memory,omitempty"`
	// ReadOnly attaches the disk read-only (path and memory disks).
	// +optional
	ReadOnly bool `json:"readOnly,omitempty"`
}

// NetworkInterface attaches one virtio NIC.
// +kubebuilder:validation:XValidation:rule="self.type == 'TAP' ? has(self.tapName) : !has(self.tapName)",message="tapName is required for TAP and only allowed for TAP"
// +kubebuilder:validation:XValidation:rule="self.type != 'Pod' || (!has(self.mac) && !has(self.switch))",message="Pod networking delegates the CNI MAC and cannot use switch"
type NetworkInterface struct {
	// Type selects NAT, TAP, or experimental Pod IPv4 delegation.
	Type NetworkType `json:"type"`
	// TapName is the host TAP device for type TAP.
	// +kubebuilder:validation:MaxLength=15
	// +kubebuilder:validation:MinLength=1
	// +optional
	TapName string `json:"tapName,omitempty"`
	// MAC overrides the generated guest MAC address.
	// +kubebuilder:validation:Pattern=`^([0-9A-Fa-f]{2}:){5}[0-9A-Fa-f]{2}$`
	// +optional
	MAC string `json:"mac,omitempty"`
	// Switch registers the TAP port with the node's kube-vm-switch daemon.
	// +optional
	Switch bool `json:"switch,omitempty"`
}

// VsockSpec enables the guest virtio-vsock device.
type VsockSpec struct {
	// Port is the guest init proxy port (ginit.vsock_port).
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=4294967295
	Port int64 `json:"port"`
	// CID is the guest context ID; kube-vm uses 3 when unset.
	// +kubebuilder:validation:Minimum=3
	// +optional
	CID *int64 `json:"cid,omitempty"`
}

// IOUringSpec opts in to experimental io_uring block I/O.
type IOUringSpec struct {
	// QueueDepth is the maximum requests per batch; 0 selects the kube-vm default.
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=256
	// +optional
	QueueDepth int32 `json:"queueDepth,omitempty"`
}

// IdleSpec configures automatic pausing of idle VMs. It is applied live
// without restarting the VM.
// +kubebuilder:validation:XValidation:rule="!has(self.diskPauseAfter) || (has(self.pauseAfter) && duration(self.pauseAfter) > duration('0s') && duration(self.diskPauseAfter) > duration(self.pauseAfter))",message="diskPauseAfter requires a nonzero pauseAfter that is shorter than diskPauseAfter"
type IdleSpec struct {
	// PauseAfter pauses a VM in memory after this much inactivity. Zero disables.
	// +optional
	PauseAfter *metav1.Duration `json:"pauseAfter,omitempty"`
	// DiskPauseAfter moves a paused VM's RAM to disk after this much inactivity.
	// +optional
	DiskPauseAfter *metav1.Duration `json:"diskPauseAfter,omitempty"`
	// WakeOnNetwork resumes an idle-paused VM on network traffic. Defaults to true.
	// +optional
	WakeOnNetwork *bool `json:"wakeOnNetwork,omitempty"`
}

// ProvisionFile writes a file or directory into the guest's copy-on-write
// overlay before the first boot.
// +kubebuilder:validation:XValidation:rule="(has(self.directory) && self.directory) ? !(has(self.content) || has(self.data) || has(self.secretKeyRef) || has(self.configMapKeyRef)) : [has(self.content), has(self.data), has(self.secretKeyRef), has(self.configMapKeyRef)].filter(x, x).size() == 1",message="files need exactly one content source; directories need none"
type ProvisionFile struct {
	// Path is the absolute guest path.
	// +kubebuilder:validation:Pattern=`^/.+`
	Path string `json:"path"`
	// Directory creates a directory instead of a file.
	// +optional
	Directory bool `json:"directory,omitempty"`
	// Content is inline text.
	// +optional
	Content *string `json:"content,omitempty"`
	// Data is inline binary content (base64 in YAML/JSON).
	// +optional
	Data []byte `json:"data,omitempty"`
	// SecretKeyRef reads content from a Secret key in the VM's namespace.
	// +optional
	SecretKeyRef *corev1.SecretKeySelector `json:"secretKeyRef,omitempty"`
	// ConfigMapKeyRef reads content from a ConfigMap key in the VM's namespace.
	// +optional
	ConfigMapKeyRef *corev1.ConfigMapKeySelector `json:"configMapKeyRef,omitempty"`
	// Mode is an octal permission string such as "0600". Defaults to 0600 (files) or 0700 (directories).
	// +kubebuilder:validation:Pattern=`^0?[0-7]{3,4}$`
	// +optional
	Mode string `json:"mode,omitempty"`
	// UID owns the entry in the guest. Defaults to 0.
	// +kubebuilder:validation:Minimum=0
	// +optional
	UID *int32 `json:"uid,omitempty"`
	// GID owns the entry in the guest. Defaults to 0.
	// +kubebuilder:validation:Minimum=0
	// +optional
	GID *int32 `json:"gid,omitempty"`
}

// SSHAccess authorizes the VM's generated SSH key for an existing guest account.
// +kubebuilder:validation:XValidation:rule="has(self.user) || has(self.home)",message="user or home is required"
type SSHAccess struct {
	// User is an existing guest account name.
	// +optional
	User string `json:"user,omitempty"`
	// Home is the account's absolute home directory in the guest.
	// +kubebuilder:validation:Pattern=`^/.+`
	// +optional
	Home string `json:"home,omitempty"`
}

// ProvisioningSpec describes one-shot first-boot guest provisioning. It needs a
// copy-on-write disk (baseDisk or overlay) and is not re-applied on restarts.
type ProvisioningSpec struct {
	// Files are written into the guest overlay (at most 128, 4 MiB total).
	// +kubebuilder:validation:MaxItems=128
	// +optional
	Files []ProvisionFile `json:"files,omitempty"`
	// SSH authorizes the VM's generated key and stores the key pair in the
	// Secret "<name>-ssh" (keys ssh-privatekey, ssh-publickey, username).
	// +optional
	SSH *SSHAccess `json:"ssh,omitempty"`
}

// VirtualMachineSpec is the desired state of a kube-vm virtual machine.
// +kubebuilder:validation:XValidation:rule="(has(self.nodeName) ? self.nodeName : ”) == (has(oldSelf.nodeName) ? oldSelf.nodeName : ”)",message="nodeName is immutable"
// +kubebuilder:validation:XValidation:rule="!has(self.disks) || self.disks.filter(d, has(d.baseDisk)).size() <= 1",message="at most one disk may use baseDisk"
// +kubebuilder:validation:XValidation:rule="!has(self.memoryBalancing) || !has(self.memoryBalancing.minMiB) || self.memoryBalancing.minMiB <= self.memoryMiB",message="memoryBalancing.minMiB must not exceed memoryMiB"
// +kubebuilder:validation:XValidation:rule="!has(self.provisioning) || (has(self.disks) && self.disks.exists(d, has(d.baseDisk) || has(d.overlay)))",message="provisioning requires a copy-on-write disk (baseDisk or overlay)"
// +kubebuilder:validation:XValidation:rule="!(has(self.autoCOW) && self.autoCOW) || (has(self.disks) && self.disks.exists(d, has(d.baseDisk) || has(d.path)))",message="autoCOW requires a baseDisk or path disk"
// +kubebuilder:validation:XValidation:rule="has(self.boot) && (has(self.boot.kernel) || has(self.boot.firmware))",message="boot.kernel or boot.firmware is required"
// +kubebuilder:validation:XValidation:rule="!has(self.network) || !self.network.exists(n, n.type == 'Pod') || (self.network.size() == 1 && has(self.boot) && has(self.boot.kernel) && has(self.boot.initrd) && !has(self.boot.firmware))",message="experimental Pod networking requires a single NIC, kernel, and kube-vm-init initrd, without firmware"
type VirtualMachineSpec struct {
	// NodeName pins the VM to a node running the agent. When unset the scheduler
	// chooses a node. It is immutable because VM state lives on one host.
	// +optional
	NodeName string `json:"nodeName,omitempty"`
	// NodeSelector restricts scheduler placement to nodes with these labels.
	// +optional
	NodeSelector map[string]string `json:"nodeSelector,omitempty"`

	// RunStrategy controls start/stop behaviour.
	// +kubebuilder:default=Always
	// +optional
	RunStrategy RunStrategy `json:"runStrategy,omitempty"`
	// Paused pauses the running VM in memory; clearing it resumes the VM.
	// +optional
	Paused bool `json:"paused,omitempty"`
	// UpdateStrategy controls how launch-setting changes are applied.
	// +kubebuilder:default=Restart
	// +optional
	UpdateStrategy UpdateStrategy `json:"updateStrategy,omitempty"`

	// CPUs is the number of vCPUs.
	// +kubebuilder:default=1
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=254
	CPUs int32 `json:"cpus,omitempty"`
	// MemoryMiB is the guest RAM in MiB.
	// +kubebuilder:default=1024
	// +kubebuilder:validation:Minimum=16
	MemoryMiB int32 `json:"memoryMiB,omitempty"`
	// MemoryBalancing opts in to automatic memory redistribution between guests.
	// +optional
	MemoryBalancing *MemoryBalancing `json:"memoryBalancing,omitempty"`

	// Boot configures the kernel/firmware boot.
	// +optional
	Boot *BootSpec `json:"boot,omitempty"`
	// Disks are attached as virtio block devices.
	// +kubebuilder:validation:MaxItems=16
	// +listType=atomic
	// +optional
	Disks []Disk `json:"disks,omitempty"`
	// AutoCOW wraps writable local disks in automatically managed copy-on-write overlays.
	// +optional
	AutoCOW bool `json:"autoCOW,omitempty"`
	// Network attaches virtio NICs.
	// +kubebuilder:validation:MaxItems=8
	// +listType=atomic
	// +optional
	Network []NetworkInterface `json:"network,omitempty"`
	// Vsock enables the guest virtio-vsock device.
	// +optional
	Vsock *VsockSpec `json:"vsock,omitempty"`
	// IOUring opts in to experimental io_uring block I/O.
	// +optional
	IOUring *IOUringSpec `json:"ioUring,omitempty"`

	// Idle configures automatic pausing of inactive VMs.
	// +optional
	Idle *IdleSpec `json:"idle,omitempty"`
	// Provisioning writes files and authorizes SSH before the first boot.
	// +optional
	Provisioning *ProvisioningSpec `json:"provisioning,omitempty"`
}

// InterfaceStatus reports an attached NIC.
type InterfaceStatus struct {
	// Name is the host TAP device.
	// +optional
	Name string `json:"name,omitempty"`
	// Type is NAT, TAP, or Pod.
	// +optional
	Type NetworkType `json:"type,omitempty"`
	// MAC is the guest MAC address, when known.
	// +optional
	MAC string `json:"mac,omitempty"`
	// IPAddress is the NAT guest address.
	// +optional
	IPAddress string `json:"ipAddress,omitempty"`
	// Gateway is the NAT gateway address.
	// +optional
	Gateway string `json:"gateway,omitempty"`
}

// PodNetworkStatus identifies a holder and its currently prepared CNI allocation.
type PodNetworkStatus struct {
	PodName string `json:"podName"`
	PodUID  string `json:"podUID"`
	// +optional
	Generation string `json:"generation,omitempty"`
	// +optional
	IPAddress string `json:"ipAddress,omitempty"`
	// +optional
	MAC string `json:"mac,omitempty"`
}

// VirtualMachineStatus is the observed state of a VirtualMachine.
type VirtualMachineStatus struct {
	// PodNetwork records the experimental network-holder binding, when allocated.
	// +optional
	PodNetwork *PodNetworkStatus `json:"podNetwork,omitempty"`
	// Phase summarises the lifecycle.
	// +optional
	Phase Phase `json:"phase,omitempty"`
	// NodeName is the node hosting the VM.
	// +optional
	NodeName string `json:"nodeName,omitempty"`
	// VMID is the identifier of the VM inside the node's kube-vm daemon.
	// +optional
	VMID string `json:"vmID,omitempty"`
	// IPAddress is the Pod IPv4 for Pod networking, otherwise the first NAT guest address.
	// +optional
	IPAddress string `json:"ipAddress,omitempty"`
	// Interfaces lists attached NICs.
	// +optional
	Interfaces []InterfaceStatus `json:"interfaces,omitempty"`
	// StartedAt is when the VM was last started by the controller.
	// +optional
	StartedAt *metav1.Time `json:"startedAt,omitempty"`
	// LaunchHash identifies the launch settings the VM was started with.
	// +optional
	LaunchHash string `json:"launchHash,omitempty"`
	// PauseRequested records that spec.paused (not idle detection) paused the VM.
	// +optional
	PauseRequested bool `json:"pauseRequested,omitempty"`
	// SSHSecret names the Secret holding the generated SSH key pair.
	// +optional
	SSHSecret string `json:"sshSecret,omitempty"`
	// LastError is the most recent start or daemon error.
	// +optional
	LastError string `json:"lastError,omitempty"`
	// ObservedGeneration is the spec generation last reconciled by the agent.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
	// Conditions are standard Kubernetes conditions.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// VirtualMachine is a kube-vm virtual machine hosted on a node.
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:path=virtualmachines,shortName=kvm;kubevm,categories=all
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Node",type=string,JSONPath=`.status.nodeName`
// +kubebuilder:printcolumn:name="IP",type=string,JSONPath=`.status.ipAddress`
// +kubebuilder:printcolumn:name="CPUs",type=integer,JSONPath=`.spec.cpus`,priority=1
// +kubebuilder:printcolumn:name="Memory",type=integer,JSONPath=`.spec.memoryMiB`,priority=1
// +kubebuilder:printcolumn:name="VM ID",type=string,JSONPath=`.status.vmID`,priority=1
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
type VirtualMachine struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   VirtualMachineSpec   `json:"spec,omitempty"`
	Status VirtualMachineStatus `json:"status,omitempty"`
}

// VirtualMachineList is a list of VirtualMachine.
// +kubebuilder:object:root=true
type VirtualMachineList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []VirtualMachine `json:"items"`
}

func init() {
	SchemeBuilder.Register(&VirtualMachine{}, &VirtualMachineList{})
}
