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

// Kinds this controller reads. It creates Racks, PowerFeeds, DeviceClaims,
// Devices and Cables, and reads Facility to resolve mapped premises.
const (
	KindFacility    = "Facility"
	KindRack        = "Rack"
	KindPowerFeed   = "PowerFeed"
	KindDeviceClaim = "DeviceClaim"
	KindDevice      = "Device"
	KindCable       = "Cable"
)

// ReadKinds are listed to observe existing Fleet state, in creation order.
var ReadKinds = []string{KindFacility, KindRack, KindPowerFeed, KindDeviceClaim, KindDevice, KindCable}

// WriteKinds are the only kinds ever created, in dependency order: a Cable
// references Devices and PowerFeeds, a Device its DeviceClaim, and claims and
// feeds their Rack.
var WriteKinds = []string{KindRack, KindPowerFeed, KindDeviceClaim, KindDevice, KindCable}

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

// Placement is typed installation placement. Position, Height and Face are
// set only for RackUnits placement.
type Placement struct {
	Scope       string          `json:"scope"`
	FacilityRef *LocalReference `json:"facilityRef,omitempty"`
	RackRef     *LocalReference `json:"rackRef,omitempty"`
	Position    int32           `json:"position,omitempty"`
	Height      int32           `json:"height,omitempty"`
	Face        string          `json:"face,omitempty"`
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
	// Unidentified records that the Device has no recorded hardware
	// identity. It is only ever true, never set with a serial or system
	// UUID, and always set with UnidentifiedReason. Such a Device can be
	// cabled but never enrolled as capacity.
	Unidentified       bool   `json:"unidentified,omitempty"`
	UnidentifiedReason string `json:"unidentifiedReason,omitempty"`
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
	Components        []Component        `json:"components,omitempty"`
	InternalMappings  []InternalMapping  `json:"internalMappings,omitempty"`
}

// Component types.
const (
	ComponentNetworkPort       = "NetworkPort"
	ComponentPowerInlet        = "PowerInlet"
	ComponentPowerOutlet       = "PowerOutlet"
	ComponentConsolePort       = "ConsolePort"
	ComponentConsoleServerPort = "ConsoleServerPort"
	ComponentFrontPort         = "FrontPort"
	ComponentRearPort          = "RearPort"
	ComponentModule            = "Module"
	ComponentInventoryItem     = "InventoryItem"
	ComponentDeviceBay         = "DeviceBay"
)

// Component is a typed part of a Device, or a PowerFeed termination.
type Component struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Type      string `json:"type"`
	Connector string `json:"connector,omitempty"`
	Positions int32  `json:"positions,omitempty"`
	Serial    string `json:"serial,omitempty"`
	AssetTag  string `json:"assetTag,omitempty"`
}

// ComponentPosition is one position of a component.
type ComponentPosition struct {
	ComponentID string `json:"componentID"`
	Position    int32  `json:"position"`
}

// InternalMapping is a fixed path inside a Device: a front port position to a
// rear port position, or a power outlet to the inlet that feeds it.
type InternalMapping struct {
	From ComponentPosition `json:"from"`
	To   ComponentPosition `json:"to"`
}

// Cable media.
const (
	MediumCopper  = "Copper"
	MediumFiber   = "Fiber"
	MediumPower   = "Power"
	MediumConsole = "Console"
	MediumUnknown = "Unknown"
)

// CableEndpoint is one end of a cable lane: a component of a Device or a
// PowerFeed termination.
type CableEndpoint struct {
	DeviceRef    *LocalReference `json:"deviceRef,omitempty"`
	PowerFeedRef *LocalReference `json:"powerFeedRef,omitempty"`
	ComponentID  string          `json:"componentID"`
	Position     int32           `json:"position"`
}

// CableLane is one conductor path of a cable.
type CableLane struct {
	ID string        `json:"id"`
	A  CableEndpoint `json:"a"`
	B  CableEndpoint `json:"b"`
}

// CableSpec is the written subset of Cable.spec.
type CableSpec struct {
	DisplayName string      `json:"displayName"`
	Medium      string      `json:"medium"`
	Lanes       []CableLane `json:"lanes"`
}

// PowerSupply is the written subset of PowerFeed.spec.supply.
type PowerSupply struct {
	Type     string `json:"type"`
	VoltageV int32  `json:"voltageV,omitempty"`
	CurrentA int32  `json:"currentA,omitempty"`
	Phase    string `json:"phase,omitempty"`
}

// PowerFeedSpec is the written subset of PowerFeed.spec.
type PowerFeedSpec struct {
	DisplayName  string          `json:"displayName"`
	FacilityRef  LocalReference  `json:"facilityRef"`
	RackRef      *LocalReference `json:"rackRef,omitempty"`
	Supply       *PowerSupply    `json:"supply,omitempty"`
	Terminations []Component     `json:"terminations"`
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
