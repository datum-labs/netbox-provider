// SPDX-License-Identifier: AGPL-3.0-only

// Package v1alpha1 contains API Schema definitions for the netbox v1alpha1 API group.
//
// The group holds this controller's own configuration: which NetBox instance to
// read (NetBoxSource) and the reviewed interpretation of its records
// (NetBoxMapping). Fleet never reads or knows about these kinds.
// +kubebuilder:object:generate=true
// +groupName=netbox.miloapis.com
package v1alpha1

import (
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

var (
	// GroupVersion is group version used to register these objects.
	GroupVersion = schema.GroupVersion{Group: "netbox.miloapis.com", Version: "v1alpha1"}

	// SchemeBuilder is used to add go types to the GroupVersionKind scheme.
	SchemeBuilder = &runtime.SchemeBuilder{}

	// AddToScheme adds the types in this group-version to the given scheme.
	AddToScheme = SchemeBuilder.AddToScheme
)
