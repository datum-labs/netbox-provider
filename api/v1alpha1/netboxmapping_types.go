// SPDX-License-Identifier: AGPL-3.0-only

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// PremisesSourceType is the NetBox object class that maps to a Facility.
// +kubebuilder:validation:Enum=Site;Location
type PremisesSourceType string

const (
	// PremisesSourceSite is a NetBox dcim/sites record.
	PremisesSourceSite PremisesSourceType = "Site"
	// PremisesSourceLocation is a NetBox dcim/locations record. A mapped
	// location takes precedence over its site.
	PremisesSourceLocation PremisesSourceType = "Location"
)

// FacilityMapping maps one NetBox site or location to an existing Fleet
// Facility. The controller never creates Facilities: premises identity is
// operator-owned, and a NetBox site name is not proof of premises.
type FacilityMapping struct {
	// SourceType is Site or Location.
	SourceType PremisesSourceType `json:"sourceType"`

	// SourceID is the NetBox numeric ID of the site or location.
	//
	// +kubebuilder:validation:Minimum=1
	SourceID int64 `json:"sourceID"`

	// FacilityRef names an existing Facility in the target namespace.
	FacilityRef LocalObjectReference `json:"facilityRef"`
}

// DeviceClassification is the reviewed meaning of a NetBox device record.
// +kubebuilder:validation:Enum=Physical;Excluded
type DeviceClassification string

const (
	// DeviceClassificationPhysical is an installed physical asset.
	DeviceClassificationPhysical DeviceClassification = "Physical"
	// DeviceClassificationExcluded is not imported. A reason is required.
	DeviceClassificationExcluded DeviceClassification = "Excluded"
)

// SerialMeaning is the reviewed meaning of a NetBox device's serial field.
// +kubebuilder:validation:Enum=Chassis;ProviderHostID;Unknown
type SerialMeaning string

const (
	// SerialMeaningChassis records the value as Device.spec.identity.serial.
	SerialMeaningChassis SerialMeaning = "Chassis"
	// SerialMeaningProviderHostID marks the value as a provider host ID,
	// never a chassis serial. It is not written; without a chassis serial
	// the Device is not created.
	SerialMeaningProviderHostID SerialMeaning = "ProviderHostID"
	// SerialMeaningUnknown drops the value. Without a chassis serial the
	// Device is not created; its DeviceClaim still is.
	SerialMeaningUnknown SerialMeaning = "Unknown"
)

// PlacementScope is the placement precision an import may assert.
// +kubebuilder:validation:Enum=Facility;RackMembership;RackUnits
type PlacementScope string

const (
	// PlacementScopeFacility asserts only the mapped Facility.
	PlacementScopeFacility PlacementScope = "Facility"
	// PlacementScopeRackMembership asserts the rack, not a position.
	PlacementScopeRackMembership PlacementScope = "RackMembership"
	// PlacementScopeRackUnits asserts the rack units NetBox records: the
	// device's position and face, and its device type's height. A device
	// without all three, or with a fractional height, stays unresolved;
	// rack units are never inferred.
	PlacementScopeRackUnits PlacementScope = "RackUnits"
)

// CableMedium is the reviewed medium of a data cable.
// +kubebuilder:validation:Enum=Copper;Fiber;Unknown
type CableMedium string

const (
	CableMediumCopper  CableMedium = "Copper"
	CableMediumFiber   CableMedium = "Fiber"
	CableMediumUnknown CableMedium = "Unknown"
)

// CableMediumRule gives the medium of data cables of some NetBox cable types.
type CableMediumRule struct {
	// CableTypes are NetBox cable type values, such as cat6 or smf.
	//
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=64
	// +kubebuilder:validation:items:MinLength=1
	// +kubebuilder:validation:items:MaxLength=64
	// +listType=set
	CableTypes []string `json:"cableTypes"`

	// Medium is the Fleet medium of those cables.
	Medium CableMedium `json:"medium"`
}

// DeviceMapping is the reviewed interpretation of one NetBox device.
//
// +kubebuilder:validation:XValidation:rule="self.classification == 'Excluded' ? (has(self.reason) && !has(self.serialMeaning) && !has(self.placement)) : (has(self.serialMeaning) && has(self.placement))",message="excluded devices need a reason; physical devices need serialMeaning and placement"
type DeviceMapping struct {
	// ID is the NetBox device ID.
	//
	// +kubebuilder:validation:Minimum=1
	ID int64 `json:"id"`

	// Classification is Physical or Excluded.
	Classification DeviceClassification `json:"classification"`

	// Reason explains an exclusion.
	//
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=256
	// +optional
	Reason string `json:"reason,omitempty"`

	// SerialMeaning is the reviewed meaning of the NetBox serial field.
	// +optional
	SerialMeaning SerialMeaning `json:"serialMeaning,omitempty"`

	// Placement is the placement precision to assert.
	// +optional
	Placement PlacementScope `json:"placement,omitempty"`
}

// RoleExclusion excludes every device whose NetBox role has one of the slugs.
// Exclusions win over device mappings.
type RoleExclusion struct {
	// RoleSlugs are NetBox device role slugs.
	//
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=64
	// +listType=set
	RoleSlugs []string `json:"roleSlugs"`

	// Reason explains the exclusion.
	//
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=256
	Reason string `json:"reason"`
}

// BMCMapping records BMC endpoints from NetBox's out-of-band IPs.
type BMCMapping struct {
	// RoleSlugs are the NetBox device roles whose out-of-band IP is a
	// Redfish BMC, such as servers. Other devices' out-of-band IPs, for
	// example a switch's management address, are never written as a BMC.
	//
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=64
	// +kubebuilder:validation:items:MinLength=1
	// +kubebuilder:validation:items:MaxLength=100
	// +listType=set
	RoleSlugs []string `json:"roleSlugs"`

	// VRFIDs are NetBox VRFs whose out-of-band IPs may be used. An address in
	// the global table (no VRF) is always usable; an address in any other VRF
	// is never treated as global, and writes no BMC.
	//
	// +kubebuilder:validation:MaxItems=64
	// +kubebuilder:validation:items:Minimum=1
	// +listType=set
	// +optional
	VRFIDs []int64 `json:"vrfIDs,omitempty"`
}

// NetBoxMappingSpec is the reviewed interpretation of a NetBox source.
type NetBoxMappingSpec struct {
	// Revision identifies the reviewed mapping content. It is written to
	// every created record as the netbox.miloapis.com/mapping-revision
	// annotation. Change it whenever the mapping changes.
	//
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	Revision string `json:"revision"`

	// Facilities maps NetBox sites and locations to existing Facilities.
	// Unmapped premises stay unresolved.
	//
	// +kubebuilder:validation:MaxItems=256
	// +listType=map
	// +listMapKey=sourceType
	// +listMapKey=sourceID
	// +optional
	Facilities []FacilityMapping `json:"facilities,omitempty"`

	// Devices holds per-device reviewed decisions. Unlisted devices stay
	// unresolved; nothing is imported by default.
	//
	// +kubebuilder:validation:MaxItems=4096
	// +listType=map
	// +listMapKey=id
	// +optional
	Devices []DeviceMapping `json:"devices,omitempty"`

	// Exclusions exclude whole device roles, for example hosted virtual
	// machines recorded in DCIM.
	//
	// +kubebuilder:validation:MaxItems=64
	// +optional
	Exclusions []RoleExclusion `json:"exclusions,omitempty"`

	// CableMedia gives the reviewed medium of data cables by NetBox cable
	// type. Power and console cables take their medium from the components
	// they connect. A data cable whose type has no rule stays unresolved.
	//
	// +kubebuilder:validation:MaxItems=64
	// +optional
	CableMedia []CableMediumRule `json:"cableMedia,omitempty"`

	// UntypedCableMedium is the medium of data cables that have no NetBox
	// cable type. When unset, they stay unresolved.
	//
	// +optional
	UntypedCableMedium CableMedium `json:"untypedCableMedium,omitempty"`

	// BMC writes Device.spec.bmc.endpoint as https://<IPv4>:443 from the
	// device's NetBox out-of-band IP, for Devices with a chassis serial whose
	// role is listed. A device without a usable IPv4 out-of-band IP gets no
	// BMC; the Device is still created. Unset writes no BMC.
	//
	// +optional
	BMC *BMCMapping `json:"bmc,omitempty"`
}

// NetBoxMapping holds reviewed import decisions for a NetBoxSource.
//
// +kubebuilder:object:root=true
// +kubebuilder:printcolumn:name="Revision",type=string,JSONPath=`.spec.revision`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
type NetBoxMapping struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec NetBoxMappingSpec `json:"spec,omitempty"`
}

// +kubebuilder:object:root=true

// NetBoxMappingList contains a list of NetBoxMapping.
type NetBoxMappingList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []NetBoxMapping `json:"items"`
}

func init() {
	SchemeBuilder.Register(func(scheme *runtime.Scheme) error {
		scheme.AddKnownTypes(GroupVersion, &NetBoxMapping{}, &NetBoxMappingList{})
		metav1.AddToGroupVersion(scheme, GroupVersion)
		return nil
	})
}
