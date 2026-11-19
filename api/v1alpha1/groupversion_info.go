// Package v1alpha1 contains the API types for the vm.kube-vm.io group.
// +kubebuilder:object:generate=true
// +groupName=vm.kube-vm.io
package v1alpha1

import (
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/scheme"
)

var (
	// GroupVersion is the group and version used to register these objects.
	GroupVersion = schema.GroupVersion{Group: "vm.kube-vm.io", Version: "v1alpha1"}

	// SchemeBuilder adds the types in this package to a scheme.
	SchemeBuilder = &scheme.Builder{GroupVersion: GroupVersion}

	// AddToScheme registers the types in this package with a scheme.
	AddToScheme = SchemeBuilder.AddToScheme
)
