package controller

import (
	"context"
	"errors"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/event"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	vmv1 "github.com/thebsdbox/kube-vm-controller/api/v1alpha1"
	"github.com/thebsdbox/kube-vm-controller/internal/kubevm"
)

const (
	// AgentControllerName names the per-node controller.
	AgentControllerName = "virtualmachine-agent"

	defaultSyncPeriod     = 10 * time.Second
	stopPollInterval      = 2 * time.Second
	failedRetryInterval   = 30 * time.Second
	minRestartInterval    = 10 * time.Second
	sshSecretSuffix       = "-ssh"
	sshPrivateKeyKey      = "ssh-privatekey"
	sshPublicKeyKey       = "ssh-publickey"
	sshUsernameKey        = "username"
	reasonDaemonDown      = "DaemonUnavailable"
	reasonStartFailed     = "StartFailed"
	reasonRestarting      = "Restarting"
	reasonRunning         = "Running"
	reasonNotRunning      = "NotRunning"
	reasonRestartRequired = "LaunchSettingsChanged"
)

// AgentReconciler runs on every VM host and drives the local kube-vm daemon
// to match the VirtualMachine objects assigned to its node.
type AgentReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder record.EventRecorder
	Daemon   kubevm.Client
	NodeName string
	// SyncPeriod is how often VM state is polled from the daemon.
	SyncPeriod time.Duration
	// Now is overridable for tests.
	Now func() time.Time
}

func (r *AgentReconciler) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

func (r *AgentReconciler) syncPeriod() time.Duration {
	if r.SyncPeriod > 0 {
		return r.SyncPeriod
	}
	return defaultSyncPeriod
}

// EffectiveNode is the node that should host the VM: the pinned spec.nodeName
// or else the node chosen by the scheduler.
func EffectiveNode(vm *vmv1.VirtualMachine) string {
	if vm.Spec.NodeName != "" {
		return vm.Spec.NodeName
	}
	return vm.Status.NodeName
}

// Reconcile implements reconcile.Reconciler.
func (r *AgentReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)
	var vm vmv1.VirtualMachine
	if err := r.Get(ctx, req.NamespacedName, &vm); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if EffectiveNode(&vm) != r.NodeName {
		return ctrl.Result{}, nil
	}

	if !vm.DeletionTimestamp.IsZero() {
		return r.reconcileDelete(ctx, &vm)
	}

	if controllerutil.AddFinalizer(&vm, vmv1.FinalizerName) {
		base := vm.DeepCopy()
		controllerutil.RemoveFinalizer(base, vmv1.FinalizerName)
		if err := r.Patch(ctx, &vm, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{})); err != nil {
			return ctrl.Result{}, err
		}
	}

	res, err := r.reconcileVM(ctx, &vm)
	if err != nil {
		log.Error(err, "reconciling virtual machine")
	}
	return res, err
}

func (r *AgentReconciler) patchStatus(ctx context.Context, vm *vmv1.VirtualMachine, orig *vmv1.VirtualMachine) error {
	if equality.Semantic.DeepEqual(orig.Status, vm.Status) {
		return nil
	}
	return r.Status().Patch(ctx, vm, client.MergeFromWithOptions(orig, client.MergeFromWithOptimisticLock{}))
}

func (r *AgentReconciler) reconcileDelete(ctx context.Context, vm *vmv1.VirtualMachine) (ctrl.Result, error) {
	if !controllerutil.ContainsFinalizer(vm, vmv1.FinalizerName) {
		return ctrl.Result{}, nil
	}
	id := VMID(vm)
	orig := vm.DeepCopy()

	details, err := r.Daemon.Examine(ctx, id)
	switch {
	case kubevm.IsNotFound(err):
		return ctrl.Result{}, r.removeFinalizer(ctx, vm)
	case err != nil:
		r.markDaemonUnavailable(vm, err)
		if perr := r.patchStatus(ctx, vm, orig); perr != nil {
			return ctrl.Result{}, perr
		}
		return ctrl.Result{RequeueAfter: r.syncPeriod()}, nil
	}

	if details.State != kubevm.StateStopped {
		vm.Status.Phase = vmv1.PhaseTerminating
		if err := r.Daemon.Shutdown(ctx, id); err != nil {
			r.markDaemonUnavailable(vm, err)
		}
		if err := r.patchStatus(ctx, vm, orig); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: stopPollInterval}, nil
	}

	switch err := r.Daemon.Delete(ctx, id); {
	case err == nil, kubevm.IsNotFound(err):
		r.Recorder.Event(vm, corev1.EventTypeNormal, "Deleted", "Virtual machine removed from node "+r.NodeName)
		return ctrl.Result{}, r.removeFinalizer(ctx, vm)
	case kubevm.IsDaemonError(err):
		// Typically "still stopping"; retry shortly.
		return ctrl.Result{RequeueAfter: stopPollInterval}, nil
	default:
		return ctrl.Result{RequeueAfter: r.syncPeriod()}, nil
	}
}

func (r *AgentReconciler) removeFinalizer(ctx context.Context, vm *vmv1.VirtualMachine) error {
	base := vm.DeepCopy()
	controllerutil.RemoveFinalizer(vm, vmv1.FinalizerName)
	return client.IgnoreNotFound(r.Patch(ctx, vm, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{})))
}

func (r *AgentReconciler) markDaemonUnavailable(vm *vmv1.VirtualMachine, err error) {
	setCondition(vm, vmv1.ConditionReady, metav1.ConditionUnknown, reasonDaemonDown, err.Error())
}

func setCondition(vm *vmv1.VirtualMachine, t string, status metav1.ConditionStatus, reason, msg string) {
	meta.SetStatusCondition(&vm.Status.Conditions, metav1.Condition{
		Type: t, Status: status, Reason: reason, Message: msg, ObservedGeneration: vm.Generation,
	})
}

func (r *AgentReconciler) reconcileVM(ctx context.Context, vm *vmv1.VirtualMachine) (ctrl.Result, error) {
	orig := vm.DeepCopy()
	id := VMID(vm)
	hash := LaunchHash(&vm.Spec)

	vm.Status.NodeName = r.NodeName
	vm.Status.VMID = id
	vm.Status.ObservedGeneration = vm.Generation
	if !meta.IsStatusConditionTrue(vm.Status.Conditions, vmv1.ConditionScheduled) {
		setCondition(vm, vmv1.ConditionScheduled, metav1.ConditionTrue, "Scheduled", "Assigned to node "+r.NodeName)
	}

	details, err := r.Daemon.Examine(ctx, id)
	if err != nil && !kubevm.IsNotFound(err) {
		r.markDaemonUnavailable(vm, err)
		if perr := r.patchStatus(ctx, vm, orig); perr != nil {
			return ctrl.Result{}, perr
		}
		return ctrl.Result{RequeueAfter: r.syncPeriod()}, nil
	}
	if kubevm.IsNotFound(err) {
		details = nil
	}

	// A VM that already runs under this id but that this object has no record of is adopted as is.
	if details != nil && vm.Status.LaunchHash == "" {
		vm.Status.LaunchHash = hash
		vm.Status.StartedAt = startedAt(details, r.now())
	}

	requeue := r.syncPeriod()
	var started bool
	var lifecycleErr error

	if details == nil || details.State == kubevm.StateStopped {
		var wait time.Duration
		started, wait, lifecycleErr = r.reconcileStopped(ctx, vm, hash)
		if wait > 0 {
			requeue = wait
		}
	} else {
		var wait time.Duration
		wait, lifecycleErr = r.reconcileLive(ctx, vm, details, hash)
		if wait > 0 {
			requeue = wait
		}
	}

	if started {
		if d, derr := r.Daemon.Examine(ctx, id); derr == nil {
			details = d
		}
	}
	r.applyDetails(vm, details)

	if lifecycleErr != nil {
		r.Recorder.Event(vm, corev1.EventTypeWarning, reasonStartFailed, lifecycleErr.Error())
		var le *launchError
		var de *kubevm.DaemonError
		switch {
		case errors.As(lifecycleErr, &le):
			msg := le.Error()
			if errors.As(le, &de) {
				msg = de.Message
			}
			vm.Status.LastError = msg
			vm.Status.Phase = vmv1.PhaseFailed
			setCondition(vm, vmv1.ConditionReady, metav1.ConditionFalse, reasonStartFailed, msg)
			requeue = failedRetryInterval
		case errors.As(lifecycleErr, &de):
			vm.Status.LastError = de.Message
		default:
			r.markDaemonUnavailable(vm, lifecycleErr)
		}
	}

	if err := r.patchStatus(ctx, vm, orig); err != nil {
		return ctrl.Result{}, err
	}
	if started {
		if err := r.ensureSSHSecret(ctx, vm, details, true); err != nil {
			return ctrl.Result{}, err
		}
	} else if lifecycleErr == nil && vm.Status.SSHSecret != "" && details != nil {
		if err := r.ensureSSHSecret(ctx, vm, details, false); err != nil {
			return ctrl.Result{}, err
		}
	}
	return ctrl.Result{RequeueAfter: requeue}, nil
}

// launchError means the VM cannot be started as specified: its provisioning
// could not be resolved or the daemon rejected the start request.
type launchError struct{ err error }

func (e *launchError) Error() string { return e.err.Error() }
func (e *launchError) Unwrap() error { return e.err }

func startedAt(d *kubevm.VMDetails, fallback time.Time) *metav1.Time {
	if t, err := time.Parse(time.RFC3339, d.StartedAt); err == nil {
		return &metav1.Time{Time: t}
	}
	return &metav1.Time{Time: fallback}
}

// reconcileStopped handles a VM that is absent from the daemon or stopped in
// it. It reports whether a start was issued and how soon to look again.
func (r *AgentReconciler) reconcileStopped(ctx context.Context, vm *vmv1.VirtualMachine, hash string) (started bool, wait time.Duration, err error) {
	if !r.shouldStart(vm, hash) {
		return false, 0, nil
	}
	if last := vm.Status.StartedAt; last != nil && vm.Spec.RunStrategy != vmv1.RunStrategyOnce {
		if since := r.now().Sub(last.Time); since < minRestartInterval {
			return false, minRestartInterval - since, nil
		}
	}

	first := vm.Status.StartedAt == nil
	var prov *ProvisionPayload
	if first {
		if prov, err = ResolveProvisioning(ctx, r.Client, vm); err != nil {
			return false, 0, &launchError{err}
		}
	}
	payload := BuildStartPayload(vm, prov)
	if err := r.Daemon.Start(ctx, payload); err != nil {
		if kubevm.IsDaemonError(err) {
			err = &launchError{err}
		}
		return false, 0, err
	}

	now := metav1.NewTime(r.now())
	vm.Status.StartedAt = &now
	vm.Status.LaunchHash = hash
	vm.Status.PauseRequested = false
	vm.Status.LastError = ""
	meta.RemoveStatusCondition(&vm.Status.Conditions, vmv1.ConditionRestartRequired)
	if first && prov != nil && prov.SSH != nil {
		vm.Status.SSHSecret = vm.Name + sshSecretSuffix
	}
	r.Recorder.Eventf(vm, corev1.EventTypeNormal, "Started", "Started virtual machine %s on node %s", VMID(vm), r.NodeName)
	return true, 0, nil
}

// shouldStart applies the run strategy to a VM that is not running.
func (r *AgentReconciler) shouldStart(vm *vmv1.VirtualMachine, hash string) bool {
	switch vm.Spec.RunStrategy {
	case vmv1.RunStrategyHalted:
		return false
	case vmv1.RunStrategyOnce:
		return vm.Status.StartedAt == nil || vm.Status.LaunchHash != hash
	default:
		return true
	}
}

// reconcileLive handles a VM that exists and is not stopped.
func (r *AgentReconciler) reconcileLive(ctx context.Context, vm *vmv1.VirtualMachine, d *kubevm.VMDetails, hash string) (time.Duration, error) {
	id := VMID(vm)

	if vm.Spec.RunStrategy == vmv1.RunStrategyHalted {
		return stopPollInterval, r.Daemon.Shutdown(ctx, id)
	}

	if vm.Status.LaunchHash != hash {
		if vm.Spec.UpdateStrategy == vmv1.UpdateStrategyManual {
			setCondition(vm, vmv1.ConditionRestartRequired, metav1.ConditionTrue, reasonRestartRequired,
				"Launch settings changed; restart the virtual machine to apply them")
		} else {
			r.Recorder.Event(vm, corev1.EventTypeNormal, reasonRestarting, "Launch settings changed; restarting virtual machine")
			setCondition(vm, vmv1.ConditionRestartRequired, metav1.ConditionTrue, reasonRestarting, "Restarting to apply changed launch settings")
			return stopPollInterval, r.Daemon.Shutdown(ctx, id)
		}
	} else {
		meta.RemoveStatusCondition(&vm.Status.Conditions, vmv1.ConditionRestartRequired)
	}

	switch {
	case vm.Spec.Paused:
		if d.State != kubevm.StatePaused {
			if err := r.Daemon.Pause(ctx, id); err != nil {
				return 0, err
			}
			d.State = kubevm.StatePaused
		}
		vm.Status.PauseRequested = true
	case vm.Status.PauseRequested:
		if d.State == kubevm.StatePaused {
			if err := r.Daemon.Resume(ctx, id); err != nil {
				return 0, err
			}
			d.State = kubevm.StateRunning
		}
		vm.Status.PauseRequested = false
	}

	if idle := vm.Spec.Idle; idle != nil {
		timeout, disk, wake := IdleSettings(idle)
		if !idleMatches(d, timeout, disk, wake) {
			if err := r.Daemon.SetActivity(ctx, id, timeout, disk, wake); err != nil {
				return 0, err
			}
		}
	}
	return 0, nil
}

func idleMatches(d *kubevm.VMDetails, timeout, disk time.Duration, wake bool) bool {
	parse := func(s string) time.Duration {
		v, _ := time.ParseDuration(s)
		return v
	}
	return parse(d.ActivityTimeout) == timeout && parse(d.ActivityDiskTimeout) == disk && d.WakeOnNetwork == wake
}

// applyDetails copies observed daemon state into the status.
func (r *AgentReconciler) applyDetails(vm *vmv1.VirtualMachine, d *kubevm.VMDetails) {
	st := &vm.Status
	if d == nil {
		st.IPAddress = ""
		st.Interfaces = nil
		st.Phase = vmv1.PhaseStopped
		if st.StartedAt == nil && vm.Spec.RunStrategy != vmv1.RunStrategyHalted {
			st.Phase = vmv1.PhaseScheduled
		}
		setCondition(vm, vmv1.ConditionReady, metav1.ConditionFalse, reasonNotRunning, "Virtual machine is not running")
		return
	}

	switch d.State {
	case kubevm.StateRunning:
		st.Phase = vmv1.PhaseRunning
	case kubevm.StateIdle:
		st.Phase = vmv1.PhaseIdle
	case kubevm.StatePaused:
		st.Phase = vmv1.PhasePaused
	default:
		st.Phase = vmv1.PhaseStopped
	}

	st.IPAddress = d.NATGuestIP
	st.Interfaces = nil
	for _, n := range d.NetworkInterfaces {
		is := vmv1.InterfaceStatus{Name: n.Tap, Type: vmv1.NetworkTypeTAP, MAC: n.MAC, IPAddress: n.NATGuestIP, Gateway: n.NATGateway}
		if n.NAT {
			is.Type = vmv1.NetworkTypeNAT
		}
		if st.IPAddress == "" {
			st.IPAddress = n.NATGuestIP
		}
		st.Interfaces = append(st.Interfaces, is)
	}

	switch st.Phase {
	case vmv1.PhaseRunning, vmv1.PhaseIdle:
		st.LastError = ""
		setCondition(vm, vmv1.ConditionReady, metav1.ConditionTrue, reasonRunning, "Virtual machine is "+d.State)
	case vmv1.PhasePaused:
		setCondition(vm, vmv1.ConditionReady, metav1.ConditionFalse, "Paused", "Virtual machine is paused")
	default:
		if d.LastError != "" {
			st.LastError = d.LastError
		}
		setCondition(vm, vmv1.ConditionReady, metav1.ConditionFalse, reasonNotRunning, "Virtual machine is stopped")
	}
}

// ensureSSHSecret stores the VM's generated key pair. At start it always
// writes the Secret; later it only recreates a Secret that has gone missing.
func (r *AgentReconciler) ensureSSHSecret(ctx context.Context, vm *vmv1.VirtualMachine, d *kubevm.VMDetails, overwrite bool) error {
	if d == nil || d.SSHPrivateKey == "" || vm.Status.SSHSecret == "" {
		return nil
	}
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: vm.Namespace, Name: vm.Status.SSHSecret}}
	err := r.Get(ctx, client.ObjectKeyFromObject(secret), secret)
	exists := err == nil
	if err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	if exists && !overwrite {
		return nil
	}
	user := d.SSHUser
	if ssh := vm.Spec.Provisioning; ssh != nil && ssh.SSH != nil && ssh.SSH.User != "" {
		user = ssh.SSH.User
	}
	secret.Type = corev1.SecretTypeOpaque
	secret.Data = map[string][]byte{
		sshPrivateKeyKey: []byte(d.SSHPrivateKey),
		sshPublicKeyKey:  []byte(d.SSHPublicKey),
		sshUsernameKey:   []byte(user),
	}
	if err := controllerutil.SetControllerReference(vm, secret, r.Scheme); err != nil {
		return err
	}
	if exists {
		return r.Update(ctx, secret)
	}
	if err := r.Create(ctx, secret); err != nil {
		return fmt.Errorf("creating SSH secret: %w", err)
	}
	return nil
}

// SetupWithManager registers the agent controller.
func (r *AgentReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if r.Recorder == nil {
		r.Recorder = mgr.GetEventRecorderFor(AgentControllerName) //nolint:staticcheck
	}
	return ctrl.NewControllerManagedBy(mgr).
		Named(AgentControllerName).
		For(&vmv1.VirtualMachine{}, builder.WithPredicates(r.predicate())).
		Complete(r)
}

// predicate limits events to VMs on this node and to changes that need action,
// so that the agent's own status writes do not re-trigger reconciles.
func (r *AgentReconciler) predicate() predicate.Predicate {
	mine := func(o client.Object) bool {
		vm, ok := o.(*vmv1.VirtualMachine)
		return ok && EffectiveNode(vm) == r.NodeName
	}
	return predicate.Funcs{
		CreateFunc:  func(e event.CreateEvent) bool { return mine(e.Object) },
		DeleteFunc:  func(event.DeleteEvent) bool { return false },
		GenericFunc: func(e event.GenericEvent) bool { return mine(e.Object) },
		UpdateFunc: func(e event.UpdateEvent) bool {
			oldVM, ok1 := e.ObjectOld.(*vmv1.VirtualMachine)
			newVM, ok2 := e.ObjectNew.(*vmv1.VirtualMachine)
			if !ok1 || !ok2 || EffectiveNode(newVM) != r.NodeName {
				return false
			}
			return oldVM.Generation != newVM.Generation ||
				oldVM.DeletionTimestamp.IsZero() != newVM.DeletionTimestamp.IsZero() ||
				EffectiveNode(oldVM) != EffectiveNode(newVM)
		},
	}
}
