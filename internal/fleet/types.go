// SPDX-License-Identifier: AGPL-3.0-only

// Package fleet is this controller's consumer-side view of Fleet's physical
// inventory API (fleet.datumapis.com/v1alpha1).
//
// Fleet's Go types are in an internal package and cannot be imported, so this
// package mirrors only the fields the controller writes or compares, and all
// API traffic uses unstructured objects. The mirror is verified against the
// vendored Fleet CRD in test/fleet/crds by the envtest admission tests: a
// field that drifts from Fleet's schema fails those tests, and unknown fields
// would be pruned or rejected by the API server rather than silently kept.
//
// Nothing in this package is NetBox-specific. Fleet's inventory schema has no
// record-authorship fields: who wrote a record comes from audit logs and when
// from metadata.creationTimestamp.
package fleet

import (
	"encoding/json"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// Group and Version of Fleet's inventory API.
const (
	Group   = "fleet.datumapis.com"
	Version = "v1alpha1"
)

// Kinds this controller reads. It creates only Rack, DeviceClaim and Device,
// and reads Facility to resolve mapped premises.
const (
	KindFacility    = "Facility"
	KindRack        = "Rack"
	KindDeviceClaim = "DeviceClaim"
	KindDevice      = "Device"
)

// ReadKinds are listed to observe existing Fleet state, in creation order.
var ReadKinds = []string{KindFacility, KindRack, KindDeviceClaim, KindDevice}

// WriteKinds are the only kinds ever created, in dependency order.
var WriteKinds = []string{KindRack, KindDeviceClaim, KindDevice}

// GVK returns the GroupVersionKind for a Fleet inventory kind.
func GVK(kind string) schema.GroupVersionKind {
	return schema.GroupVersionKind{Group: Group, Version: Version, Kind: kind}
}

// ListGVK returns the list GroupVersionKind for a Fleet inventory kind.
func ListGVK(kind string) schema.GroupVersionKind {
	return GVK(kind + "List")
}

// ExternalReference is Fleet's {system, type, id} reference. It is read from
// existing Devices' providerHostRef for collision checks and never written.
type ExternalReference struct {
	System string `json:"system"`
	Type   string `json:"type"`
	ID     string `json:"id"`
}

// LocalReference names an object in the same namespace.
type LocalReference struct {
	Name string `json:"name"`
}

// RackSpec is the written subset of Rack.spec.
type RackSpec struct {
	DisplayName string         `json:"displayName"`
	FacilityRef LocalReference `json:"facilityRef"`
	HeightUnits int32          `json:"heightUnits,omitempty"`
}

// ExpectedIdentity is the written subset of DeviceClaim.spec.expectedIdentity.
type ExpectedIdentity struct {
	Manufacturer string `json:"manufacturer,omitempty"`
	Model        string `json:"model"`
	Serial       string `json:"serial,omitempty"`
}

// Placement is typed installation placement. Exact rack units are never
// written by this controller.
type Placement struct {
	Scope       string          `json:"scope"`
	FacilityRef *LocalReference `json:"facilityRef,omitempty"`
	RackRef     *LocalReference `json:"rackRef,omitempty"`
}

// DeviceClaimSpec is the written subset of DeviceClaim.spec.
type DeviceClaimSpec struct {
	DisplayName      string           `json:"displayName,omitempty"`
	Lifecycle        string           `json:"lifecycle"`
	ExpectedIdentity ExpectedIdentity `json:"expectedIdentity"`
	Placement        Placement        `json:"placement"`
}

// ModelShape is the written subset of Device.spec.modelShape.
type ModelShape struct {
	HeightUnits *float64 `json:"heightUnits,omitempty"`
	FullDepth   *bool    `json:"fullDepth,omitempty"`
}

// DeviceIdentity is the written subset of Device.spec.identity.
type DeviceIdentity struct {
	Manufacturer string `json:"manufacturer,omitempty"`
	Model        string `json:"model"`
	Serial       string `json:"serial,omitempty"`
	SystemUUID   string `json:"systemUUID,omitempty"`
}

// DeviceSpec is the written subset of Device.spec.
type DeviceSpec struct {
	DisplayName       string             `json:"displayName,omitempty"`
	ModelShape        *ModelShape        `json:"modelShape,omitempty"`
	AssetTag          string             `json:"assetTag,omitempty"`
	ProviderHostRef   *ExternalReference `json:"providerHostRef,omitempty"`
	TopologyAuthority string             `json:"topologyAuthority,omitempty"`
	ClaimRef          LocalReference     `json:"claimRef"`
	Identity          DeviceIdentity     `json:"identity"`
}

// NewObject returns an unstructured Fleet object with the given spec and
// metadata. Fleet ignores labels and annotations.
func NewObject(kind, namespace, name string, labels, annotations map[string]string, spec any) (*unstructured.Unstructured, error) {
	data, err := json.Marshal(spec)
	if err != nil {
		return nil, err
	}
	var specMap map[string]any
	if err := json.Unmarshal(data, &specMap); err != nil {
		return nil, err
	}
	obj := &unstructured.Unstructured{Object: map[string]any{"spec": specMap}}
	obj.SetGroupVersionKind(GVK(kind))
	obj.SetNamespace(namespace)
	obj.SetName(name)
	obj.SetLabels(labels)
	obj.SetAnnotations(annotations)
	return obj, nil
}
