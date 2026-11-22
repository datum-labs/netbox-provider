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

// PlacementScope is the placement precision an import may assert. Exact rack
// units are never imported.
// +kubebuilder:validation:Enum=Facility;RackMembership
type PlacementScope string

const (
	// PlacementScopeFacility asserts only the mapped Facility.
	PlacementScopeFacility PlacementScope = "Facility"
	// PlacementScopeRackMembership asserts the rack, not a position.
	PlacementScopeRackMembership PlacementScope = "RackMembership"
)

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
