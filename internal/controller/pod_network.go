package controller

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"

	vmv1 "github.com/thebsdbox/kube-vm-controller/api/v1alpha1"
	"github.com/thebsdbox/kube-vm-controller/internal/kubevm"
	"github.com/thebsdbox/kube-vm-controller/internal/podnetwork"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

func usesPodNetwork(vm *vmv1.VirtualMachine) bool {
	for _, nic := range vm.Spec.Network {
		if nic.Type == vmv1.NetworkTypePod {
			return true
		}
	}
	return false
}

func liveVM(details *kubevm.VMDetails) bool {
	return details != nil && details.State != kubevm.StateStopped
}

func bindingIdentity(vm *vmv1.VirtualMachine) podnetwork.Identity {
	id := podnetwork.Identity{VMUID: string(vm.UID)}
	if b := vm.Status.PodNetwork; b != nil {
		id.PodUID, id.Generation, id.IPAddress = b.PodUID, b.Generation, b.IPAddress
	}
	return id
}

func (r *AgentReconciler) networkRoot() string {
	if r.PodNetworkDir != "" {
		return r.PodNetworkDir
	}
	return "/run/kube-vm/pod-network"
}

func holderLabels(vm *vmv1.VirtualMachine) map[string]string {
	labels := map[string]string{}
	for k, v := range vm.Labels {
		if !strings.HasPrefix(k, "vm.kube-vm.io/") {
			labels[k] = v
		}
	}
	labels["app.kubernetes.io/component"] = "network-holder"
	labels["vm.kube-vm.io/vm"] = vm.Name
	labels["vm.kube-vm.io/uid"] = string(vm.UID)
	return labels
}

func (r *AgentReconciler) holderPod(vm *vmv1.VirtualMachine) *corev1.Pod {
	no, yes, root := false, true, int64(0)
	directory, device := corev1.HostPathDirectory, corev1.HostPathCharDev
	grace := int64(30)
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "kvm-net-" + VMID(vm), Namespace: vm.Namespace, Labels: holderLabels(vm)},
		Spec: corev1.PodSpec{
			NodeName: EffectiveNode(vm), AutomountServiceAccountToken: &no, TerminationGracePeriodSeconds: &grace,
			ReadinessGates: []corev1.PodReadinessGate{{ConditionType: podnetwork.ReadyCondition}},
			Containers: []corev1.Container{{
				Name: "network", Image: r.PodNetworkImage, Command: []string{"/network-holder"},
				Env: []corev1.EnvVar{
					{Name: "VM_UID", Value: string(vm.UID)},
					{Name: "POD_UID", ValueFrom: &corev1.EnvVarSource{FieldRef: &corev1.ObjectFieldSelector{FieldPath: "metadata.uid"}}},
					{Name: "POD_IP", ValueFrom: &corev1.EnvVarSource{FieldRef: &corev1.ObjectFieldSelector{FieldPath: "status.podIP"}}},
				},
				SecurityContext: &corev1.SecurityContext{RunAsUser: &root, AllowPrivilegeEscalation: &no, ReadOnlyRootFilesystem: &yes, Capabilities: &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}, Add: []corev1.Capability{"NET_ADMIN"}}},
				VolumeMounts:    []corev1.VolumeMount{{Name: "network", MountPath: "/network"}, {Name: "state", MountPath: "/state"}, {Name: "tun", MountPath: "/dev/net/tun"}},
				ReadinessProbe:  &corev1.Probe{ProbeHandler: corev1.ProbeHandler{Exec: &corev1.ExecAction{Command: []string{"/network-holder", "--check"}}}, PeriodSeconds: 2, TimeoutSeconds: 4},
			}},
			Volumes: []corev1.Volume{
				{Name: "network", VolumeSource: corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{Path: filepath.Join(r.networkRoot(), VMID(vm)), Type: &directory}}},
				{Name: "state", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
				{Name: "tun", VolumeSource: corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{Path: "/dev/net/tun", Type: &device}}},
			},
		},
	}
}

func containersReady(pod *corev1.Pod) bool {
	for _, condition := range pod.Status.Conditions {
		if condition.Type == corev1.ContainersReady {
			return condition.Status == corev1.ConditionTrue
		}
	}
	return false
}

// reconcilePodNetwork returns false until CNI and the helper have prepared a
// binding. It fences a running guest before accepting a replacement allocation.
func (r *AgentReconciler) reconcilePodNetwork(ctx context.Context, vm *vmv1.VirtualMachine, details *kubevm.VMDetails) (bool, error) {
	if r.PodNetworkImage == "" {
		return false, fmt.Errorf("Pod networking is disabled; configure --pod-network-image with the experimental helper image")
	}
	if usesPodNetwork(vm) && (len(vm.Spec.Network) != 1 || vm.Spec.Boot == nil || vm.Spec.Boot.Kernel == "" || vm.Spec.Boot.Initrd == "" || vm.Spec.Boot.Firmware != "") {
		return false, fmt.Errorf("experimental Pod networking requires one NIC and a kernel/kube-vm-init initrd")
	}
	capable, ok := r.Daemon.(kubevm.PodNetworkClient)
	if !ok {
		return false, fmt.Errorf("daemon does not implement Pod networking")
	}
	if err := capable.CheckPodNetworkSupport(ctx); err != nil {
		return false, err
	}
	desired := r.holderPod(vm)
	if err := controllerutil.SetControllerReference(vm, desired, r.Scheme); err != nil {
		return false, err
	}
	var pod corev1.Pod
	err := r.Get(ctx, client.ObjectKeyFromObject(desired), &pod)
	if apierrors.IsNotFound(err) {
		if liveVM(details) {
			return false, r.Daemon.Shutdown(ctx, VMID(vm))
		}
		if !r.shouldStart(vm, LaunchHash(&vm.Spec)) && vm.Status.PodNetwork == nil {
			return true, nil
		}
		if err := os.MkdirAll(filepath.Join(r.networkRoot(), VMID(vm)), 0700); err != nil {
			return false, err
		}
		if err := r.Create(ctx, desired); err != nil {
			return false, err
		}
		vm.Status.PodNetwork = &vmv1.PodNetworkStatus{PodName: desired.Name, PodUID: string(desired.UID)}
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if !metav1.IsControlledBy(&pod, vm) || pod.Spec.NodeName != EffectiveNode(vm) {
		return false, fmt.Errorf("network-holder Pod ownership or node mismatch")
	}
	if !reflect.DeepEqual(pod.Labels, desired.Labels) {
		base := pod.DeepCopy()
		pod.Labels = desired.Labels
		if err := r.Patch(ctx, &pod, client.MergeFrom(base)); err != nil {
			return false, err
		}
	}
	if vm.Status.PodNetwork == nil {
		vm.Status.PodNetwork = &vmv1.PodNetworkStatus{PodName: pod.Name, PodUID: string(pod.UID)}
	}
	if !pod.DeletionTimestamp.IsZero() || pod.Status.Phase == corev1.PodFailed || pod.Status.Phase == corev1.PodSucceeded {
		if liveVM(details) {
			return false, r.Daemon.Shutdown(ctx, VMID(vm))
		}
		if pod.DeletionTimestamp.IsZero() {
			uid := pod.UID
			if err := r.Delete(ctx, &pod, &client.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &uid}}); err != nil {
				return false, client.IgnoreNotFound(err)
			}
		}
		return false, nil
	}
	if pod.Status.PodIP == "" || pod.Status.Phase != corev1.PodRunning || !containersReady(&pod) {
		if err := r.setHolderReady(ctx, &pod, false); err != nil {
			return false, err
		}
		if liveVM(details) {
			return false, r.Daemon.Shutdown(ctx, VMID(vm))
		}
		return false, nil
	}
	id := podnetwork.Identity{VMUID: string(vm.UID), PodUID: string(pod.UID), IPAddress: pod.Status.PodIP}
	read := r.NetworkStatus
	if read == nil {
		read = podnetwork.Status
	}
	config, err := read(ctx, podnetwork.SocketPath(r.networkRoot(), VMID(vm)), id)
	if err != nil {
		return false, err
	}
	if err := config.Validate(); err != nil {
		return false, err
	}
	if err := config.Matches(id); err != nil {
		return false, err
	}
	if liveVM(details) && (details.PodNetwork == nil || *details.PodNetwork != config.Identity) {
		if err := r.setHolderReady(ctx, &pod, false); err != nil {
			return false, err
		}
		return false, r.Daemon.Shutdown(ctx, VMID(vm))
	}
	vm.Status.PodNetwork = &vmv1.PodNetworkStatus{PodName: pod.Name, PodUID: string(pod.UID), Generation: config.Generation, IPAddress: config.IPAddress, MAC: config.MAC}
	return true, nil
}

func (r *AgentReconciler) setHolderReady(ctx context.Context, pod *corev1.Pod, ready bool) error {
	status := corev1.ConditionFalse
	if ready {
		status = corev1.ConditionTrue
	}
	base := pod.DeepCopy()
	found := false
	for i := range pod.Status.Conditions {
		c := &pod.Status.Conditions[i]
		if c.Type == podnetwork.ReadyCondition {
			if c.Status == status {
				return nil
			}
			c.Status, c.LastTransitionTime = status, metav1.NewTime(r.now())
			c.Reason = "VMNetworkState"
			found = true
		}
	}
	if !found {
		pod.Status.Conditions = append(pod.Status.Conditions, corev1.PodCondition{Type: podnetwork.ReadyCondition, Status: status, Reason: "VMNetworkState", LastTransitionTime: metav1.NewTime(r.now())})
	}
	return r.Status().Patch(ctx, pod, client.MergeFrom(base))
}

func (r *AgentReconciler) syncHolderReady(ctx context.Context, vm *vmv1.VirtualMachine, ready bool) error {
	if vm.Status.PodNetwork == nil {
		return nil
	}
	var pod corev1.Pod
	if err := r.Get(ctx, types.NamespacedName{Namespace: vm.Namespace, Name: vm.Status.PodNetwork.PodName}, &pod); err != nil {
		return client.IgnoreNotFound(err)
	}
	if !metav1.IsControlledBy(&pod, vm) {
		return fmt.Errorf("network-holder ownership changed")
	}
	return r.setHolderReady(ctx, &pod, ready)
}

func (r *AgentReconciler) cleanupPodNetwork(ctx context.Context, vm *vmv1.VirtualMachine) (bool, error) {
	name := r.holderPod(vm).Name
	var pod corev1.Pod
	err := r.Get(ctx, types.NamespacedName{Namespace: vm.Namespace, Name: name}, &pod)
	if err == nil {
		if !metav1.IsControlledBy(&pod, vm) {
			return false, fmt.Errorf("refusing to delete unowned network-holder Pod")
		}
		if err := r.setHolderReady(ctx, &pod, false); err != nil {
			return false, err
		}
		uid := pod.UID
		return false, client.IgnoreNotFound(r.Delete(ctx, &pod, &client.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &uid}}))
	}
	if !apierrors.IsNotFound(err) {
		return false, err
	}
	if err := os.Remove(podnetwork.SocketPath(r.networkRoot(), VMID(vm))); err != nil && !errors.Is(err, os.ErrNotExist) {
		return false, err
	}
	if err := os.Remove(filepath.Join(r.networkRoot(), VMID(vm))); err != nil && !errors.Is(err, os.ErrNotExist) {
		return false, err
	}
	vm.Status.PodNetwork = nil
	return true, nil
}
