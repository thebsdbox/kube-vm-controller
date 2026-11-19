package controller

import (
	"context"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	vmv1 "github.com/thebsdbox/kube-vm-controller/api/v1alpha1"
	"github.com/thebsdbox/kube-vm-controller/internal/kubevm"
)

type agentEnv struct {
	t      *testing.T
	r      *AgentReconciler
	daemon *kubevm.Fake
	c      client.Client
	clock  *time.Time
}

func testScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	if err := vmv1.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	return s
}

func newAgentEnv(t *testing.T, objs ...client.Object) *agentEnv {
	t.Helper()
	s := testScheme(t)
	c := fake.NewClientBuilder().WithScheme(s).WithStatusSubresource(&vmv1.VirtualMachine{}).WithObjects(objs...).Build()
	d := kubevm.NewFake()
	now := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	env := &agentEnv{t: t, daemon: d, c: c, clock: &now}
	env.r = &AgentReconciler{
		Client: c, Scheme: s, Recorder: record.NewFakeRecorder(100), Daemon: d, NodeName: "node-a",
		Now: func() time.Time { return *env.clock },
	}
	return env
}

func scheduledVM(name string) *vmv1.VirtualMachine {
	vm := testVM()
	vm.Namespace, vm.Name = "default", name
	vm.Spec.RunStrategy = vmv1.RunStrategyAlways
	vm.Spec.Network = []vmv1.NetworkInterface{{Type: vmv1.NetworkTypeNAT}}
	vm.Status.NodeName = "node-a"
	return vm
}

func (e *agentEnv) reconcile(vm *vmv1.VirtualMachine) reconcile.Result {
	e.t.Helper()
	res, err := e.r.Reconcile(context.Background(), reconcile.Request{NamespacedName: client.ObjectKeyFromObject(vm)})
	if err != nil {
		e.t.Fatalf("reconcile: %v", err)
	}
	return res
}

func (e *agentEnv) get(vm *vmv1.VirtualMachine) *vmv1.VirtualMachine {
	e.t.Helper()
	var out vmv1.VirtualMachine
	if err := e.c.Get(context.Background(), client.ObjectKeyFromObject(vm), &out); err != nil {
		e.t.Fatal(err)
	}
	return &out
}

func (e *agentEnv) update(vm *vmv1.VirtualMachine, mutate func(*vmv1.VirtualMachine)) {
	e.t.Helper()
	cur := e.get(vm)
	mutate(cur)
	if err := e.c.Update(context.Background(), cur); err != nil {
		e.t.Fatal(err)
	}
}

func (e *agentEnv) advance(d time.Duration) { *e.clock = e.clock.Add(d) }

func countCalls(calls []string, prefix string) int {
	n := 0
	for _, c := range calls {
		if strings.HasPrefix(c, prefix) {
			n++
		}
	}
	return n
}

func TestAgentStartsVM(t *testing.T) {
	vm := scheduledVM("a")
	e := newAgentEnv(t, vm)
	res := e.reconcile(vm)
	if res.RequeueAfter != defaultSyncPeriod {
		t.Fatalf("requeue = %v", res.RequeueAfter)
	}

	got := e.get(vm)
	if !controllerutil.ContainsFinalizer(got, vmv1.FinalizerName) {
		t.Fatal("finalizer not added")
	}
	id := VMID(vm)
	starts := e.daemon.StartPayloads()
	if len(starts) != 1 || starts[0]["uid"] != id {
		t.Fatalf("starts = %v", starts)
	}
	st := got.Status
	if st.Phase != vmv1.PhaseRunning || st.VMID != id || st.IPAddress == "" || st.StartedAt == nil || st.LaunchHash == "" {
		t.Fatalf("status = %+v", st)
	}
	if !meta.IsStatusConditionTrue(st.Conditions, vmv1.ConditionReady) || !meta.IsStatusConditionTrue(st.Conditions, vmv1.ConditionScheduled) {
		t.Fatalf("conditions = %+v", st.Conditions)
	}
	if len(st.Interfaces) != 1 || st.Interfaces[0].Type != vmv1.NetworkTypeNAT {
		t.Fatalf("interfaces = %+v", st.Interfaces)
	}

	e.reconcile(vm)
	if n := len(e.daemon.StartPayloads()); n != 1 {
		t.Fatalf("second reconcile started the VM again (%d starts)", n)
	}
}

func TestAgentIgnoresOtherNodes(t *testing.T) {
	vm := scheduledVM("a")
	vm.Status.NodeName = "node-b"
	e := newAgentEnv(t, vm)
	e.reconcile(vm)
	if len(e.daemon.CallLog()) != 0 || controllerutil.ContainsFinalizer(e.get(vm), vmv1.FinalizerName) {
		t.Fatal("agent acted on a VM assigned to another node")
	}

	pinned := scheduledVM("p")
	pinned.Spec.NodeName = "node-a"
	pinned.Status.NodeName = ""
	e2 := newAgentEnv(t, pinned)
	e2.reconcile(pinned)
	if got := e2.get(pinned); got.Status.NodeName != "node-a" || got.Status.Phase != vmv1.PhaseRunning {
		t.Fatalf("pinned VM not started: %+v", got.Status)
	}
}

func TestAgentRestartsOnLaunchChange(t *testing.T) {
	vm := scheduledVM("a")
	e := newAgentEnv(t, vm)
	e.reconcile(vm)
	e.advance(time.Minute)

	e.update(vm, func(v *vmv1.VirtualMachine) { v.Spec.CPUs = 4 })
	res := e.reconcile(vm)
	if res.RequeueAfter != stopPollInterval || countCalls(e.daemon.CallLog(), "shutdown:") != 1 {
		t.Fatalf("expected shutdown and fast requeue, got %v %v", res, e.daemon.CallLog())
	}
	e.reconcile(vm)
	starts := e.daemon.StartPayloads()
	if len(starts) != 2 || starts[1]["cpu"] != int32(4) {
		t.Fatalf("expected restart with 4 cpus, got %v", starts)
	}
	if countCalls(e.daemon.CallLog(), "delete:") != 0 {
		t.Fatal("restart must never delete the VM (it destroys disk overlays)")
	}
	if got := e.get(vm); got.Status.Phase != vmv1.PhaseRunning || meta.IsStatusConditionTrue(got.Status.Conditions, vmv1.ConditionRestartRequired) {
		t.Fatalf("status after restart = %+v", got.Status)
	}
}

func TestAgentManualUpdateStrategy(t *testing.T) {
	vm := scheduledVM("a")
	vm.Spec.UpdateStrategy = vmv1.UpdateStrategyManual
	e := newAgentEnv(t, vm)
	e.reconcile(vm)
	e.update(vm, func(v *vmv1.VirtualMachine) { v.Spec.MemoryMiB = 4096 })
	e.reconcile(vm)
	if countCalls(e.daemon.CallLog(), "shutdown:") != 0 {
		t.Fatal("manual strategy must not restart the VM")
	}
	if !meta.IsStatusConditionTrue(e.get(vm).Status.Conditions, vmv1.ConditionRestartRequired) {
		t.Fatal("RestartRequired not set")
	}
}

func TestAgentRestartsStoppedGuestWithBackoff(t *testing.T) {
	vm := scheduledVM("a")
	e := newAgentEnv(t, vm)
	e.reconcile(vm)
	e.daemon.SetState(VMID(vm), kubevm.StateStopped)

	res := e.reconcile(vm)
	if len(e.daemon.StartPayloads()) != 1 || res.RequeueAfter <= 0 || res.RequeueAfter > minRestartInterval {
		t.Fatalf("restart must wait for the minimum interval: %v, %d starts", res, len(e.daemon.StartPayloads()))
	}
	e.advance(minRestartInterval)
	e.reconcile(vm)
	if len(e.daemon.StartPayloads()) != 2 {
		t.Fatal("stopped VM with runStrategy Always was not restarted")
	}
}

func TestAgentRunStrategies(t *testing.T) {
	once := scheduledVM("once")
	once.Spec.RunStrategy = vmv1.RunStrategyOnce
	e := newAgentEnv(t, once)
	e.reconcile(once)
	e.daemon.SetState(VMID(once), kubevm.StateStopped)
	e.advance(time.Minute)
	e.reconcile(once)
	if len(e.daemon.StartPayloads()) != 1 || e.get(once).Status.Phase != vmv1.PhaseStopped {
		t.Fatal("Once VM must stay stopped after it exits")
	}

	e.update(once, func(v *vmv1.VirtualMachine) { v.Spec.MemoryMiB = 2222 })
	e.reconcile(once)
	if len(e.daemon.StartPayloads()) != 2 {
		t.Fatal("Once VM must relaunch when launch settings change")
	}

	e.update(once, func(v *vmv1.VirtualMachine) { v.Spec.RunStrategy = vmv1.RunStrategyHalted })
	e.reconcile(once)
	if got := e.daemon.Get(VMID(once)); got.State != kubevm.StateStopped {
		t.Fatalf("Halted VM still %s", got.State)
	}

	e.update(once, func(v *vmv1.VirtualMachine) { v.Spec.RunStrategy = vmv1.RunStrategyAlways })
	e.advance(time.Minute)
	e.reconcile(once)
	if got := e.daemon.Get(VMID(once)); got.State != kubevm.StateRunning {
		t.Fatalf("VM not restarted after Halted -> Always, state %s", got.State)
	}
}

func TestAgentPause(t *testing.T) {
	vm := scheduledVM("a")
	e := newAgentEnv(t, vm)
	e.reconcile(vm)
	id := VMID(vm)

	e.update(vm, func(v *vmv1.VirtualMachine) { v.Spec.Paused = true })
	e.reconcile(vm)
	got := e.get(vm)
	if e.daemon.Get(id).State != kubevm.StatePaused || got.Status.Phase != vmv1.PhasePaused || !got.Status.PauseRequested {
		t.Fatalf("not paused: %+v", got.Status)
	}
	e.update(vm, func(v *vmv1.VirtualMachine) { v.Spec.Paused = false })
	e.reconcile(vm)
	if got := e.get(vm); e.daemon.Get(id).State != kubevm.StateRunning || got.Status.PauseRequested || got.Status.Phase != vmv1.PhaseRunning {
		t.Fatalf("not resumed: %+v", got.Status)
	}

	// A pause made by the daemon's idle monitor must be left to wake on network traffic.
	e.daemon.SetState(id, kubevm.StatePaused)
	e.reconcile(vm)
	if e.daemon.Get(id).State != kubevm.StatePaused || e.get(vm).Status.Phase != vmv1.PhasePaused {
		t.Fatal("idle-paused VM must not be resumed by the controller")
	}
}

func TestAgentAppliesIdleLive(t *testing.T) {
	vm := scheduledVM("a")
	e := newAgentEnv(t, vm)
	e.reconcile(vm)
	e.update(vm, func(v *vmv1.VirtualMachine) {
		v.Spec.Idle = &vmv1.IdleSpec{PauseAfter: &metav1.Duration{Duration: 5 * time.Minute}}
	})
	e.reconcile(vm)
	e.reconcile(vm)
	if n := countCalls(e.daemon.CallLog(), "activity-timeout:"); n != 1 {
		t.Fatalf("activity-timeout called %d times, want exactly once", n)
	}
	if countCalls(e.daemon.CallLog(), "shutdown:") != 0 || len(e.daemon.StartPayloads()) != 1 {
		t.Fatal("idle changes must not restart the VM")
	}
	if got := e.daemon.Get(VMID(vm)); got.ActivityTimeout != "5m0s" || !got.WakeOnNetwork {
		t.Fatalf("daemon activity = %+v", got)
	}
}

func TestAgentStartFailure(t *testing.T) {
	vm := scheduledVM("a")
	e := newAgentEnv(t, vm)
	e.daemon.StartErr = "kernel not found"
	res := e.reconcile(vm)
	got := e.get(vm)
	if got.Status.Phase != vmv1.PhaseFailed || got.Status.LastError != "kernel not found" || res.RequeueAfter != failedRetryInterval {
		t.Fatalf("status = %+v, requeue %v", got.Status, res.RequeueAfter)
	}
	e.daemon.StartErr = ""
	e.reconcile(vm)
	if got := e.get(vm); got.Status.Phase != vmv1.PhaseRunning || got.Status.LastError != "" {
		t.Fatalf("did not recover: %+v", got.Status)
	}
}

func TestAgentDaemonUnavailable(t *testing.T) {
	vm := scheduledVM("a")
	e := newAgentEnv(t, vm)
	e.reconcile(vm)
	e.daemon.Unavailable = true
	res := e.reconcile(vm)
	got := e.get(vm)
	cond := meta.FindStatusCondition(got.Status.Conditions, vmv1.ConditionReady)
	if cond == nil || cond.Reason != reasonDaemonDown || res.RequeueAfter == 0 {
		t.Fatalf("condition = %+v, requeue %v", cond, res.RequeueAfter)
	}
	if got.Status.Phase != vmv1.PhaseRunning {
		t.Fatalf("phase must not change when the daemon is unreachable, got %s", got.Status.Phase)
	}
}

func TestAgentProvisioningAndSSHSecret(t *testing.T) {
	vm := scheduledVM("a")
	vm.Spec.Disks = []vmv1.Disk{{BaseDisk: "ubuntu"}}
	vm.Spec.Provisioning = &vmv1.ProvisioningSpec{
		Files: []vmv1.ProvisionFile{{Path: "/etc/motd", Content: ptr("hi")}},
		SSH:   &vmv1.SSHAccess{User: "ubuntu"},
	}
	e := newAgentEnv(t, vm)
	e.reconcile(vm)

	first := e.daemon.StartPayloads()[0]
	if first["provision"] == nil || first["ssh_access"] == nil || first["base_disk"] != "ubuntu" {
		t.Fatalf("first start lacks provisioning: %v", first)
	}
	var sec corev1.Secret
	if err := e.c.Get(context.Background(), client.ObjectKey{Namespace: "default", Name: "a-ssh"}, &sec); err != nil {
		t.Fatalf("ssh secret: %v", err)
	}
	if string(sec.Data[sshUsernameKey]) != "ubuntu" || len(sec.Data[sshPrivateKeyKey]) == 0 || len(sec.Data[sshPublicKeyKey]) == 0 {
		t.Fatalf("secret data = %v", sec.Data)
	}
	if !metav1.IsControlledBy(&sec, e.get(vm)) {
		t.Fatal("secret must be owned by the VM")
	}

	// Restarts reuse the provisioned overlay: no provisioning, no new secret contents.
	e.daemon.SetState(VMID(vm), kubevm.StateStopped)
	e.advance(time.Minute)
	e.reconcile(vm)
	second := e.daemon.StartPayloads()[1]
	if second["provision"] != nil || second["ssh_access"] != nil {
		t.Fatalf("provisioning repeated on restart: %v", second)
	}

	// A deleted Secret is recreated.
	if err := e.c.Delete(context.Background(), &sec); err != nil {
		t.Fatal(err)
	}
	e.reconcile(vm)
	if err := e.c.Get(context.Background(), client.ObjectKeyFromObject(&sec), &corev1.Secret{}); err != nil {
		t.Fatalf("secret not recreated: %v", err)
	}
}

func TestAgentProvisioningErrorMarksFailed(t *testing.T) {
	vm := scheduledVM("a")
	vm.Spec.Provisioning = &vmv1.ProvisioningSpec{Files: []vmv1.ProvisionFile{{
		Path:         "/etc/x",
		SecretKeyRef: &corev1.SecretKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: "nope"}, Key: "k"},
	}}}
	e := newAgentEnv(t, vm)
	res := e.reconcile(vm)
	got := e.get(vm)
	if got.Status.Phase != vmv1.PhaseFailed || !strings.Contains(got.Status.LastError, "nope") || res.RequeueAfter != failedRetryInterval {
		t.Fatalf("status = %+v", got.Status)
	}
	if len(e.daemon.StartPayloads()) != 0 {
		t.Fatal("VM started despite unresolved provisioning")
	}
}

func TestAgentAdoptsExistingVM(t *testing.T) {
	vm := scheduledVM("a")
	e := newAgentEnv(t, vm)
	e.daemon.Add(kubevm.VMDetails{UID: VMID(vm), State: kubevm.StateRunning, WakeOnNetwork: true})
	e.reconcile(vm)
	if len(e.daemon.StartPayloads()) != 0 {
		t.Fatal("existing VM must be adopted, not restarted")
	}
	if got := e.get(vm); got.Status.LaunchHash == "" || got.Status.Phase != vmv1.PhaseRunning {
		t.Fatalf("status = %+v", got.Status)
	}
}

func TestAgentDeleteFlow(t *testing.T) {
	vm := scheduledVM("a")
	e := newAgentEnv(t, vm)
	e.daemon.StopOnShutdown = false
	e.reconcile(vm)
	if err := e.c.Delete(context.Background(), e.get(vm)); err != nil {
		t.Fatal(err)
	}

	res := e.reconcile(vm)
	if res.RequeueAfter != stopPollInterval || e.get(vm).Status.Phase != vmv1.PhaseTerminating {
		t.Fatalf("expected shutdown in progress, got %v %s", res, e.get(vm).Status.Phase)
	}
	if countCalls(e.daemon.CallLog(), "delete:") != 0 {
		t.Fatal("must not delete before the VM is stopped")
	}

	e.daemon.SetState(VMID(vm), kubevm.StateStopped)
	e.reconcile(vm)
	if e.daemon.Get(VMID(vm)) != nil {
		t.Fatal("VM was not deleted from the daemon")
	}
	if err := e.c.Get(context.Background(), client.ObjectKeyFromObject(vm), &vmv1.VirtualMachine{}); !apierrors.IsNotFound(err) {
		t.Fatalf("finalizer not removed, get err = %v", err)
	}
}

func TestAgentDeleteWhenVMAlreadyGone(t *testing.T) {
	vm := scheduledVM("a")
	e := newAgentEnv(t, vm)
	e.reconcile(vm)
	e.daemon.SetState(VMID(vm), kubevm.StateStopped)
	_ = e.daemon.Delete(context.Background(), VMID(vm))
	if err := e.c.Delete(context.Background(), e.get(vm)); err != nil {
		t.Fatal(err)
	}
	e.reconcile(vm)
	if err := e.c.Get(context.Background(), client.ObjectKeyFromObject(vm), &vmv1.VirtualMachine{}); !apierrors.IsNotFound(err) {
		t.Fatalf("finalizer not removed, get err = %v", err)
	}
}
