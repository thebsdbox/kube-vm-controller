package controller

import (
	"context"
	"sort"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	vmv1 "github.com/thebsdbox/kube-vm-controller/api/v1alpha1"
)

const (
	// SchedulerControllerName names the leader-elected placement controller.
	SchedulerControllerName = "virtualmachine-scheduler"

	unschedulableRetry = 15 * time.Second
	reasonNoNode       = "NoAvailableNode"
)

// SchedulerReconciler assigns unplaced VirtualMachines to a node that runs a
// Ready agent. The VM stays on that node for life, since its state lives there.
type SchedulerReconciler struct {
	client.Client
	Recorder record.EventRecorder
	// AgentNamespace and AgentSelector identify the agent pods.
	AgentNamespace string
	AgentSelector  labels.Selector

	mu sync.Mutex
	// assumed holds placements not yet visible in the informer cache, so that
	// back-to-back reconciles spread VMs instead of piling onto one node.
	assumed map[types.NamespacedName]string
}

// Reconcile implements reconcile.Reconciler.
func (r *SchedulerReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)
	var vm vmv1.VirtualMachine
	if err := r.Get(ctx, req.NamespacedName, &vm); err != nil {
		r.forget(req.NamespacedName)
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if !vm.DeletionTimestamp.IsZero() || EffectiveNode(&vm) != "" {
		r.forget(req.NamespacedName)
		return ctrl.Result{}, nil
	}

	orig := vm.DeepCopy()
	node, err := r.pick(ctx, &vm)
	if err != nil {
		return ctrl.Result{}, err
	}
	if node == "" {
		if vm.Status.Phase == "" {
			vm.Status.Phase = vmv1.PhasePending
		}
		setCondition(&vm, vmv1.ConditionScheduled, metav1.ConditionFalse, reasonNoNode,
			"No ready node with a kube-vm agent matches the node selector")
		if !equality.Semantic.DeepEqual(orig.Status, vm.Status) {
			r.Recorder.Event(&vm, corev1.EventTypeWarning, reasonNoNode, "No ready node with a kube-vm agent matches the node selector")
			if err := r.Status().Patch(ctx, &vm, client.MergeFromWithOptions(orig, client.MergeFromWithOptimisticLock{})); err != nil {
				return ctrl.Result{}, err
			}
		}
		return ctrl.Result{RequeueAfter: unschedulableRetry}, nil
	}

	vm.Status.NodeName = node
	if vm.Status.Phase == "" || vm.Status.Phase == vmv1.PhasePending {
		vm.Status.Phase = vmv1.PhaseScheduled
	}
	setCondition(&vm, vmv1.ConditionScheduled, metav1.ConditionTrue, "Scheduled", "Assigned to node "+node)
	if err := r.Status().Patch(ctx, &vm, client.MergeFromWithOptions(orig, client.MergeFromWithOptimisticLock{})); err != nil {
		return ctrl.Result{}, err
	}
	r.assume(req.NamespacedName, node)
	r.Recorder.Eventf(&vm, corev1.EventTypeNormal, "Scheduled", "Assigned to node %s", node)
	log.Info("scheduled virtual machine", "node", node)
	return ctrl.Result{}, nil
}

// pick returns the eligible node hosting the fewest VMs, or "" when none is eligible.
func (r *SchedulerReconciler) pick(ctx context.Context, vm *vmv1.VirtualMachine) (string, error) {
	agents, err := r.agentNodes(ctx)
	if err != nil {
		return "", err
	}
	if len(agents) == 0 {
		return "", nil
	}

	var nodes corev1.NodeList
	if err := r.List(ctx, &nodes); err != nil {
		return "", err
	}
	var candidates []string
	for i := range nodes.Items {
		n := &nodes.Items[i]
		if !agents[n.Name] || n.Spec.Unschedulable || !nodeReady(n) {
			continue
		}
		if !labels.SelectorFromSet(vm.Spec.NodeSelector).Matches(labels.Set(n.Labels)) {
			continue
		}
		candidates = append(candidates, n.Name)
	}
	if len(candidates) == 0 {
		return "", nil
	}

	load, err := r.load(ctx)
	if err != nil {
		return "", err
	}
	sort.Slice(candidates, func(i, j int) bool {
		a, b := candidates[i], candidates[j]
		if load[a] != load[b] {
			return load[a] < load[b]
		}
		return a < b
	})
	return candidates[0], nil
}

func (r *SchedulerReconciler) agentNodes(ctx context.Context) (map[string]bool, error) {
	var pods corev1.PodList
	opts := []client.ListOption{client.InNamespace(r.AgentNamespace)}
	if r.AgentSelector != nil {
		opts = append(opts, client.MatchingLabelsSelector{Selector: r.AgentSelector})
	}
	if err := r.List(ctx, &pods, opts...); err != nil {
		return nil, err
	}
	out := map[string]bool{}
	for i := range pods.Items {
		p := &pods.Items[i]
		if p.Spec.NodeName != "" && p.DeletionTimestamp.IsZero() && podReady(p) {
			out[p.Spec.NodeName] = true
		}
	}
	return out, nil
}

// load counts VMs per node, including placements the cache has not caught up with.
func (r *SchedulerReconciler) load(ctx context.Context) (map[string]int, error) {
	var vms vmv1.VirtualMachineList
	if err := r.List(ctx, &vms); err != nil {
		return nil, err
	}
	load := map[string]int{}
	seen := map[types.NamespacedName]bool{}
	for i := range vms.Items {
		v := &vms.Items[i]
		key := client.ObjectKeyFromObject(v)
		seen[key] = true
		if n := EffectiveNode(v); n != "" {
			load[n]++
		}
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	for key, node := range r.assumed {
		v := findVM(vms.Items, key)
		if !seen[key] || EffectiveNode(v) != "" {
			delete(r.assumed, key)
			continue
		}
		load[node]++
	}
	return load, nil
}

func findVM(items []vmv1.VirtualMachine, key types.NamespacedName) *vmv1.VirtualMachine {
	for i := range items {
		if items[i].Namespace == key.Namespace && items[i].Name == key.Name {
			return &items[i]
		}
	}
	return nil
}

func (r *SchedulerReconciler) assume(key types.NamespacedName, node string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.assumed == nil {
		r.assumed = map[types.NamespacedName]string{}
	}
	r.assumed[key] = node
}

func (r *SchedulerReconciler) forget(key types.NamespacedName) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.assumed, key)
}

func nodeReady(n *corev1.Node) bool {
	for _, c := range n.Status.Conditions {
		if c.Type == corev1.NodeReady {
			return c.Status == corev1.ConditionTrue
		}
	}
	return false
}

func podReady(p *corev1.Pod) bool {
	if p.Status.Phase != corev1.PodRunning {
		return false
	}
	for _, c := range p.Status.Conditions {
		if c.Type == corev1.PodReady {
			return c.Status == corev1.ConditionTrue
		}
	}
	return false
}

// SetupWithManager registers the scheduler controller.
func (r *SchedulerReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if r.Recorder == nil {
		r.Recorder = mgr.GetEventRecorderFor(SchedulerControllerName) //nolint:staticcheck
	}
	unplaced := func(o client.Object) bool {
		vm, ok := o.(*vmv1.VirtualMachine)
		return ok && vm.DeletionTimestamp.IsZero() && EffectiveNode(vm) == ""
	}
	return ctrl.NewControllerManagedBy(mgr).
		Named(SchedulerControllerName).
		For(&vmv1.VirtualMachine{}, builder.WithPredicates(predicate.Funcs{
			CreateFunc:  func(e event.CreateEvent) bool { return unplaced(e.Object) },
			DeleteFunc:  func(event.DeleteEvent) bool { return false },
			GenericFunc: func(e event.GenericEvent) bool { return unplaced(e.Object) },
			UpdateFunc: func(e event.UpdateEvent) bool {
				return unplaced(e.ObjectNew) && e.ObjectOld.GetGeneration() != e.ObjectNew.GetGeneration()
			},
		})).
		Complete(r)
}
