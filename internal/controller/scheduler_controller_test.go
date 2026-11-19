package controller

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	vmv1 "github.com/thebsdbox/kube-vm-controller/api/v1alpha1"
)

const agentNS = "kube-vm-system"

func testNode(name string, ready bool, lbls map[string]string) *corev1.Node {
	st := corev1.ConditionTrue
	if !ready {
		st = corev1.ConditionFalse
	}
	return &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: name, Labels: lbls},
		Status:     corev1.NodeStatus{Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: st}}},
	}
}

func agentPod(node string, ready bool) *corev1.Pod {
	st := corev1.ConditionTrue
	if !ready {
		st = corev1.ConditionFalse
	}
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: agentNS, Name: "agent-" + node, Labels: map[string]string{"app": "kube-vm-agent"}},
		Spec:       corev1.PodSpec{NodeName: node},
		Status: corev1.PodStatus{
			Phase:      corev1.PodRunning,
			Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: st}},
		},
	}
}

func newScheduler(t *testing.T, objs ...client.Object) (*SchedulerReconciler, client.Client) {
	t.Helper()
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithStatusSubresource(&vmv1.VirtualMachine{}).WithObjects(objs...).Build()
	return &SchedulerReconciler{
		Client: c, Recorder: record.NewFakeRecorder(100), AgentNamespace: agentNS,
		AgentSelector: labels.SelectorFromSet(labels.Set{"app": "kube-vm-agent"}),
	}, c
}

func schedule(t *testing.T, r *SchedulerReconciler, c client.Client, name string) (*vmv1.VirtualMachine, reconcile.Result) {
	t.Helper()
	key := client.ObjectKey{Namespace: "default", Name: name}
	res, err := r.Reconcile(context.Background(), reconcile.Request{NamespacedName: key})
	if err != nil {
		t.Fatal(err)
	}
	var vm vmv1.VirtualMachine
	if err := c.Get(context.Background(), key, &vm); err != nil {
		t.Fatal(err)
	}
	return &vm, res
}

func unplacedVM(name string) *vmv1.VirtualMachine {
	return &vmv1.VirtualMachine{ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: name}}
}

func TestSchedulerPicksLeastLoadedEligibleNode(t *testing.T) {
	busy := unplacedVM("busy")
	busy.Status.NodeName = "n1"
	cordoned := testNode("n3", true, nil)
	cordoned.Spec.Unschedulable = true
	r, c := newScheduler(t,
		testNode("n1", true, nil), testNode("n2", true, nil), cordoned,
		testNode("n4", false, nil), testNode("n5", true, nil), // n4 not ready, n5 has no agent
		agentPod("n1", true), agentPod("n2", true), agentPod("n3", true), agentPod("n4", true),
		busy, unplacedVM("new"),
	)
	vm, _ := schedule(t, r, c, "new")
	if vm.Status.NodeName != "n2" || vm.Status.Phase != vmv1.PhaseScheduled {
		t.Fatalf("status = %+v", vm.Status)
	}
}

func TestSchedulerSpreadsBeforeCacheCatchesUp(t *testing.T) {
	r, c := newScheduler(t,
		testNode("n1", true, nil), testNode("n2", true, nil),
		agentPod("n1", true), agentPod("n2", true),
		unplacedVM("a"), unplacedVM("b"),
	)
	a, _ := schedule(t, r, c, "a")
	b, _ := schedule(t, r, c, "b")
	if a.Status.NodeName == "" || b.Status.NodeName == "" || a.Status.NodeName == b.Status.NodeName {
		t.Fatalf("VMs placed on %q and %q", a.Status.NodeName, b.Status.NodeName)
	}
}

func TestSchedulerHonoursNodeSelectorAndAgentReadiness(t *testing.T) {
	vm := unplacedVM("a")
	vm.Spec.NodeSelector = map[string]string{"disk": "ssd"}
	r, c := newScheduler(t,
		testNode("n1", true, nil), testNode("n2", true, map[string]string{"disk": "ssd"}), testNode("n3", true, map[string]string{"disk": "ssd"}),
		agentPod("n1", true), agentPod("n2", false), agentPod("n3", true),
		vm,
	)
	got, _ := schedule(t, r, c, "a")
	if got.Status.NodeName != "n3" {
		t.Fatalf("scheduled on %q, want n3", got.Status.NodeName)
	}
}

func TestSchedulerNoAvailableNode(t *testing.T) {
	r, c := newScheduler(t, testNode("n1", true, nil), unplacedVM("a"))
	got, res := schedule(t, r, c, "a")
	if got.Status.NodeName != "" || got.Status.Phase != vmv1.PhasePending || res.RequeueAfter != unschedulableRetry {
		t.Fatalf("status = %+v, requeue %v", got.Status, res.RequeueAfter)
	}
	if len(got.Status.Conditions) != 1 || got.Status.Conditions[0].Reason != reasonNoNode {
		t.Fatalf("conditions = %+v", got.Status.Conditions)
	}
}

func TestSchedulerLeavesPlacedAndPinnedVMsAlone(t *testing.T) {
	placed := unplacedVM("placed")
	placed.Status.NodeName = "n9"
	pinned := unplacedVM("pinned")
	pinned.Spec.NodeName = "n1"
	r, c := newScheduler(t, testNode("n1", true, nil), agentPod("n1", true), placed, pinned)
	if got, _ := schedule(t, r, c, "placed"); got.Status.NodeName != "n9" {
		t.Fatal("placed VM was moved")
	}
	if got, _ := schedule(t, r, c, "pinned"); got.Status.NodeName != "" {
		t.Fatal("scheduler must not touch pinned VMs")
	}
}
