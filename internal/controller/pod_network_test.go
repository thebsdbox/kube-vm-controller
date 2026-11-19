package controller

import (
	"context"
	"fmt"
	"testing"
	"time"

	vmv1 "github.com/thebsdbox/kube-vm-controller/api/v1alpha1"
	"github.com/thebsdbox/kube-vm-controller/internal/kubevm"
	"github.com/thebsdbox/kube-vm-controller/internal/podnetwork"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

func podVM() *vmv1.VirtualMachine {
	vm := scheduledVM("pod-net")
	vm.Spec.Network = []vmv1.NetworkInterface{{Type: vmv1.NetworkTypePod}}
	vm.Spec.Boot = &vmv1.BootSpec{Kernel: "/kernel", Initrd: "/pod-initrd"}
	return vm
}

func setupPodEnv(t *testing.T, vm *vmv1.VirtualMachine, prepared bool) (*agentEnv, *podnetwork.Config) {
	t.Helper()
	e := newAgentEnv(t, vm)
	e.r.PodNetworkDir = t.TempDir()
	e.r.PodNetworkImage = "test/holder"
	cfg := &podnetwork.Config{Version: 1, Identity: podnetwork.Identity{VMUID: string(vm.UID), PodUID: "holder-uid", Generation: "sandbox-1", IPAddress: "10.42.0.8"},
		Namespace: "net:[123]", MAC: "02:00:00:00:00:08", Address: "10.42.0.8/32", MTU: 1450,
		Routes: []podnetwork.Route{{Destination: "10.42.0.1/32", LinkScope: true}, {Gateway: "10.42.0.1"}}, Resolver: "nameserver 10.96.0.10\nsearch default.svc.cluster.local svc.cluster.local\n"}
	e.r.NetworkStatus = func(_ context.Context, _ string, id podnetwork.Identity) (*podnetwork.Config, error) {
		if err := cfg.Matches(id); err != nil {
			return nil, err
		}
		copy := *cfg
		return &copy, nil
	}
	if prepared {
		pod := e.r.holderPod(vm)
		pod.UID = "holder-uid"
		pod.Status = corev1.PodStatus{Phase: corev1.PodRunning, PodIP: cfg.IPAddress, Conditions: []corev1.PodCondition{{Type: corev1.ContainersReady, Status: corev1.ConditionTrue}}}
		if err := controllerutil.SetControllerReference(vm, pod, e.r.Scheme); err != nil {
			t.Fatal(err)
		}
		if err := e.c.Create(context.Background(), pod); err != nil {
			t.Fatal(err)
		}
	}
	return e, cfg
}

func TestPodNetworkWaitsForHolder(t *testing.T) {
	vm := podVM()
	e, _ := setupPodEnv(t, vm, false)
	e.reconcile(vm)
	e.reconcile(vm)
	if len(e.daemon.StartPayloads()) != 0 {
		t.Fatal("started before CNI and helper readiness")
	}
	var pod corev1.Pod
	if err := e.c.Get(context.Background(), client.ObjectKeyFromObject(e.r.holderPod(vm)), &pod); err != nil {
		t.Fatal(err)
	}
	if !metav1.IsControlledBy(&pod, vm) || pod.Spec.NodeName != "node-a" || *pod.Spec.AutomountServiceAccountToken {
		t.Fatalf("unsafe holder: %+v", pod.Spec)
	}
	if got := e.get(vm); meta.IsStatusConditionTrue(got.Status.Conditions, vmv1.ConditionReady) {
		t.Fatal("premature VM readiness")
	}
}

func TestPodNetworkStartsWithRealPodIdentity(t *testing.T) {
	vm := podVM()
	e, cfg := setupPodEnv(t, vm, true)
	e.reconcile(vm)
	got := e.get(vm)
	if got.Status.IPAddress != cfg.IPAddress || got.Status.Interfaces[0].Type != vmv1.NetworkTypePod || got.Status.PodNetwork.Generation != cfg.Generation {
		t.Fatalf("wrong Pod status: %+v", got.Status)
	}
	payload := e.daemon.StartPayloads()[0]
	if payload["pod_network"] != cfg.Identity {
		t.Fatalf("binding not passed: %+v", payload)
	}
	if _, exists := payload["nics"]; exists {
		t.Fatal("Pod binding degraded to legacy TAP/NAT")
	}
	var pod corev1.Pod
	if err := e.c.Get(context.Background(), client.ObjectKeyFromObject(e.r.holderPod(vm)), &pod); err != nil {
		t.Fatal(err)
	}
	ready := false
	for _, c := range pod.Status.Conditions {
		if c.Type == podnetwork.ReadyCondition {
			ready = c.Status == corev1.ConditionTrue
		}
	}
	if !ready {
		t.Fatal("running bound VM not ready for Service endpoints")
	}
	e.reconcile(vm)
	if len(e.daemon.StartPayloads()) != 1 {
		t.Fatal("repeated start")
	}
}

func TestPodNetworkRejectsUnavailableOrInvalidBinding(t *testing.T) {
	for _, mode := range []string{"disabled", "old-daemon", "wrong-pod", "bad-config", "helper-down"} {
		t.Run(mode, func(t *testing.T) {
			vm := podVM()
			e, cfg := setupPodEnv(t, vm, true)
			switch mode {
			case "disabled":
				e.r.PodNetworkImage = ""
			case "old-daemon":
				e.daemon.PodNetworkUnsupported = true
			case "wrong-pod":
				cfg.PodUID = "other-pod"
			case "bad-config":
				cfg.Address = "192.0.2.1/32"
			case "helper-down":
				e.r.NetworkStatus = func(context.Context, string, podnetwork.Identity) (*podnetwork.Config, error) {
					return nil, fmt.Errorf("helper unavailable")
				}
			}
			e.reconcile(vm)
			if len(e.daemon.StartPayloads()) != 0 || e.get(vm).Status.LastError == "" {
				t.Fatal("invalid binding accepted or error not surfaced")
			}
		})
	}
}

func TestPodNetworkFencesReplacedSandbox(t *testing.T) {
	vm := podVM()
	e, cfg := setupPodEnv(t, vm, true)
	e.reconcile(vm)
	cfg.Generation = "sandbox-2"
	e.reconcile(vm)
	if countCalls(e.daemon.CallLog(), "shutdown:") != 1 || len(e.daemon.StartPayloads()) != 1 {
		t.Fatal("replacement did not fence existing VM")
	}
	e.advance(time.Minute)
	e.reconcile(vm)
	if len(e.daemon.StartPayloads()) != 2 || e.get(vm).Status.PodNetwork.Generation != cfg.Generation {
		t.Fatal("VM did not bind replacement sandbox")
	}
	if countCalls(e.daemon.CallLog(), "delete:") != 0 {
		t.Fatal("network recovery deleted disk overlays")
	}
}

func TestPodNetworkPreservesRunStrategies(t *testing.T) {
	for _, strategy := range []vmv1.RunStrategy{vmv1.RunStrategyOnce, vmv1.RunStrategyHalted} {
		t.Run(string(strategy), func(t *testing.T) {
			vm := podVM()
			e, _ := setupPodEnv(t, vm, true)
			e.reconcile(vm)
			e.update(vm, func(v *vmv1.VirtualMachine) { v.Spec.RunStrategy = strategy })
			e.daemon.SetState(VMID(vm), kubevm.StateStopped)
			e.advance(time.Minute)
			e.reconcile(vm)
			if len(e.daemon.StartPayloads()) != 1 {
				t.Fatal("stopped Once/Halted VM relaunched")
			}
			var pod corev1.Pod
			if err := e.c.Get(context.Background(), client.ObjectKeyFromObject(e.r.holderPod(vm)), &pod); err != nil {
				t.Fatal("holder allocation lost:", err)
			}
		})
	}
}

func TestManualNATToPodNetworkDoesNotRestart(t *testing.T) {
	vm := scheduledVM("manual")
	e := newAgentEnv(t, vm)
	e.reconcile(vm)
	e.update(vm, func(v *vmv1.VirtualMachine) {
		v.Spec.Network = []vmv1.NetworkInterface{{Type: vmv1.NetworkTypePod}}
		v.Spec.UpdateStrategy = vmv1.UpdateStrategyManual
	})
	e.reconcile(vm)
	if countCalls(e.daemon.CallLog(), "shutdown:") != 0 || !meta.IsStatusConditionTrue(e.get(vm).Status.Conditions, vmv1.ConditionRestartRequired) {
		t.Fatal("Pod mode change bypassed Manual strategy")
	}
}

func TestPodNetworkCleansHolderWhenDaemonVMIsAbsent(t *testing.T) {
	vm := podVM()
	controllerutil.AddFinalizer(vm, vmv1.FinalizerName)
	e, _ := setupPodEnv(t, vm, true)
	if err := e.c.Delete(context.Background(), vm); err != nil {
		t.Fatal(err)
	}
	e.reconcile(vm)
	e.reconcile(vm)
	var pods corev1.PodList
	if err := e.c.List(context.Background(), &pods); err != nil {
		t.Fatal(err)
	}
	if len(pods.Items) != 0 {
		t.Fatal("network holder leaked")
	}
	if err := e.c.Get(context.Background(), client.ObjectKeyFromObject(vm), &vmv1.VirtualMachine{}); client.IgnoreNotFound(err) != nil || err == nil {
		t.Fatalf("VM finalizer not removed: %v", err)
	}
}
