// SPDX-License-Identifier: AGPL-3.0-only

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// ReadLimits bound one NetBox collection read, such as dcim/interfaces. A read
// that exceeds a bound fails as a whole, and nothing is planned from it.
type ReadLimits struct {
	// MaxRecordsPerCollection bounds the records in one collection. The page
	// bound scales with it at 200 records per page. Default 20000.
	//
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=1000000
	// +optional
	MaxRecordsPerCollection int32 `json:"maxRecordsPerCollection,omitempty"`

	// MaxMiBPerCollection bounds the response bytes of one collection, in
	// MiB. Each page is still bounded at 4 MiB. Default 64.
	//
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=4096
	// +optional
	MaxMiBPerCollection int32 `json:"maxMiBPerCollection,omitempty"`

	// CollectionTimeout bounds the read of one collection. Each request is
	// also bounded at 30 seconds. Default 2m.
	//
	// +kubebuilder:validation:XValidation:rule="duration(self) >= duration('10s') && duration(self) <= duration('30m')",message="collectionTimeout must be between 10s and 30m"
	// +optional
	CollectionTimeout *metav1.Duration `json:"collectionTimeout,omitempty"`
}

// SourceMode selects whether the controller may write to Fleet.
// +kubebuilder:validation:Enum=Observe;Create
type SourceMode string

const (
	// SourceModeObserve reads NetBox and Fleet and reports the plan. It never
	// writes Fleet records. This is the default.
	SourceModeObserve SourceMode = "Observe"

	// SourceModeCreate additionally writes the plan to Fleet by server-side
	// apply with field manager netbox-provider: it creates missing records and
	// re-applies the fields it owns when NetBox changes them. It never forces
	// ownership, adopts a record it did not apply, changes an immutable field,
	// writes status, or deletes a record.
	SourceModeCreate SourceMode = "Create"
)

// AuthScheme is the HTTP Authorization scheme for the NetBox API token.
// +kubebuilder:validation:Enum=Bearer;Token
type AuthScheme string

const (
	// AuthSchemeBearer is used by NetBox v2 API tokens.
	AuthSchemeBearer AuthScheme = "Bearer"
	// AuthSchemeToken is used by legacy NetBox v1 API tokens.
	AuthSchemeToken AuthScheme = "Token"
)

// FleetWriter names the writer identity this controller acts as. Only the
// direct-API Operator writer exists; the controller never writes records that
// another writer, such as Flux, reconciles.
// +kubebuilder:validation:Enum=Operator
type FleetWriter string

// FleetWriterOperator is the direct-API operator writer.
const FleetWriterOperator FleetWriter = "Operator"

// SecretKeyReference names a key in a Secret in the NetBoxSource's namespace.
type SecretKeyReference struct {
	// Name of the Secret, in the same namespace as the NetBoxSource.
	//
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	Name string `json:"name"`

	// Key within the Secret's data that holds the API token.
	//
	// +kubebuilder:default=token
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	// +optional
	Key string `json:"key,omitempty"`
}

// LocalObjectReference names an object in the same namespace.
type LocalObjectReference struct {
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	Name string `json:"name"`
}

// SourceScope narrows which NetBox records are read. An empty scope reads
// every site the token can see.
type SourceScope struct {
	// SiteIDs restricts racks, locations and devices to these NetBox site IDs.
	// The filter is sent to NetBox and must be preserved by every pagination
	// link.
	//
	// +kubebuilder:validation:MaxItems=64
	// +listType=set
	// +optional
	SiteIDs []int64 `json:"siteIDs,omitempty"`
}

// FleetTarget is where, and as which writer, Fleet records are written.
type FleetTarget struct {
	// Namespace in the Fleet (Milo provider project) control plane that holds
	// the inventory records.
	//
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="target namespace is immutable; create a new NetBoxSource for a new target"
	Namespace string `json:"namespace"`

	// Writer is the explicit writer identity for created records.
	Writer FleetWriter `json:"writer"`
}

// NetBoxSourceSpec defines one NetBox instance and how it is imported.
type NetBoxSourceSpec struct {
	// URL is the HTTPS installation root of NetBox, including any path
	// prefix, but not the /api suffix. It must not contain credentials, a
	// query, or a fragment.
	//
	// +kubebuilder:validation:MinLength=9
	// +kubebuilder:validation:MaxLength=2048
	// +kubebuilder:validation:XValidation:rule="isURL(self) && url(self).getScheme() == 'https' && url(self).getHostname() != '' && !self.contains('@') && !self.contains('?') && !self.contains('#')",message="url must be an https URL without credentials, query or fragment"
	URL string `json:"url"`

	// SourceID is the stable, operator-assigned identifier of this NetBox
	// instance. It is written as the netbox.miloapis.com/source label on every
	// Fleet record and is part of every generated name, so it is immutable.
	// Keep it when the URL changes; assign a new one after a database restore
	// or fork that can reuse record IDs. It is not a hostname.
	//
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="sourceID is immutable"
	SourceID string `json:"sourceID"`

	// TokenSecretRef names the Secret key holding a read-only NetBox API token.
	TokenSecretRef SecretKeyReference `json:"tokenSecretRef"`

	// AuthScheme is the Authorization header scheme.
	//
	// +kubebuilder:default=Bearer
	// +optional
	AuthScheme AuthScheme `json:"authScheme,omitempty"`

	// Scope narrows what is read from NetBox.
	//
	// +optional
	Scope SourceScope `json:"scope,omitempty"`

	// MappingRef names the NetBoxMapping, in the same namespace, that holds
	// the reviewed interpretation of this source's records.
	MappingRef LocalObjectReference `json:"mappingRef"`

	// Target is the Fleet namespace and writer.
	Target FleetTarget `json:"target"`

	// Mode is Observe (default, read-only) or Create (additive creation of new
	// records). Switching to Create is the explicit ownership opt-in.
	//
	// +kubebuilder:default=Observe
	// +optional
	Mode SourceMode `json:"mode,omitempty"`

	// ReadLimits bound each NetBox collection read. Unset fields keep their
	// defaults.
	//
	// +optional
	ReadLimits *ReadLimits `json:"readLimits,omitempty"`

	// Interval between complete re-reads of NetBox.
	//
	// +kubebuilder:default="30m"
	// +kubebuilder:validation:XValidation:rule="duration(self) >= duration('1m') && duration(self) <= duration('24h')",message="interval must be between 1m and 24h"
	// +optional
	Interval *metav1.Duration `json:"interval,omitempty"`

	// Suspend stops reading and writing. Existing status is retained.
	//
	// +optional
	Suspend bool `json:"suspend,omitempty"`
}

// RecordState is the outcome planned for one source record or Fleet target.
// +kubebuilder:validation:Enum=Create;Created;Update;Updated;Unchanged;Quarantined;Unresolved;Excluded;Unsupported;RetainedMissingSource
type RecordState string

const (
	// RecordStateCreate means a new record would be created (Observe mode) or
	// is about to be created (Create mode).
	RecordStateCreate RecordState = "Create"
	// RecordStateCreated means the record was created in this reconcile.
	RecordStateCreated RecordState = "Created"
	// RecordStateUpdate means a field this controller owns differs from
	// NetBox and would be re-applied (Observe mode) or is being re-applied.
	RecordStateUpdate RecordState = "Update"
	// RecordStateUpdated means owned fields were re-applied in this reconcile.
	RecordStateUpdated RecordState = "Updated"
	// RecordStateUnchanged means an equal record from this source exists.
	RecordStateUnchanged RecordState = "Unchanged"
	// RecordStateQuarantined means a conflict with existing Fleet state blocks
	// the record. Nothing is written for it.
	RecordStateQuarantined RecordState = "Quarantined"
	// RecordStateUnresolved means the mapping or a dependency needs review.
	RecordStateUnresolved RecordState = "Unresolved"
	// RecordStateExcluded means the mapping explicitly excludes the record.
	RecordStateExcluded RecordState = "Excluded"
	// RecordStateUnsupported means the record cannot be represented faithfully.
	RecordStateUnsupported RecordState = "Unsupported"
	// RecordStateRetainedMissingSource means a Fleet record from this source
	// no longer has a source record. It is retained, never deleted.
	RecordStateRetainedMissingSource RecordState = "RetainedMissingSource"
)

// RecordStatus reports one notable record. It never carries serials, asset
// tags, or other source field values.
type RecordStatus struct {
	// Source is the source record key, such as dcim/devices/12.
	// +optional
	Source string `json:"source,omitempty"`
	// Target is the Fleet Kind/name, when one was planned.
	// +optional
	Target string `json:"target,omitempty"`
	// State is the planned or achieved outcome.
	State RecordState `json:"state"`
	// Reason is a CamelCase machine-readable cause.
	// +optional
	Reason string `json:"reason,omitempty"`
	// Message is a short human-readable explanation.
	// +optional
	Message string `json:"message,omitempty"`
}

// ObservationSummary describes the last complete NetBox read.
type ObservationSummary struct {
	// SnapshotDigest is the sha256 of the canonical, field-allowlisted records.
	SnapshotDigest string `json:"snapshotDigest"`
	// ObservedAt is when the read started.
	ObservedAt metav1.Time `json:"observedAt"`
	// MappingRevision is the NetBoxMapping revision the plan used.
	MappingRevision string `json:"mappingRevision"`
	// Counts holds the number of records read per collection.
	// +optional
	Counts map[string]int32 `json:"counts,omitempty"`
}

// PlanSummary counts plan outcomes.
type PlanSummary struct {
	Create                int32 `json:"create"`
	Created               int32 `json:"created"`
	Update                int32 `json:"update"`
	Updated               int32 `json:"updated"`
	Unchanged             int32 `json:"unchanged"`
	Quarantined           int32 `json:"quarantined"`
	Unresolved            int32 `json:"unresolved"`
	Excluded              int32 `json:"excluded"`
	Unsupported           int32 `json:"unsupported"`
	RetainedMissingSource int32 `json:"retainedMissingSource"`
}

// NetBoxSourceStatus is the observed state of a NetBoxSource.
type NetBoxSourceStatus struct {
	// ObservedGeneration is the most recent generation observed.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// Conditions: ConfigurationResolved, SourceObserved, FleetObserved,
	// Applied, CableReferencesDelivered and Ready. Uncertainty is Unknown,
	// never True.
	//
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// LastObservation describes the last complete NetBox read.
	// +optional
	LastObservation *ObservationSummary `json:"lastObservation,omitempty"`

	// Plan counts outcomes of the last complete plan.
	// +optional
	Plan *PlanSummary `json:"plan,omitempty"`

	// Records lists notable records, conflicts first. It is bounded.
	//
	// +kubebuilder:validation:MaxItems=100
	// +optional
	Records []RecordStatus `json:"records,omitempty"`

	// RecordsTruncated is true when Records omits entries.
	// +optional
	RecordsTruncated bool `json:"recordsTruncated,omitempty"`

	// UndeliveredCableReferences counts the Cable endpoints in Fleet that name
	// a component their Device does not have once this reconcile's writes
	// land, among the Devices this source plans. Such a Cable is one this
	// plan does not write, so it still names a component the Device dropped.
	// The CableReferencesDelivered condition names the first few.
	// +optional
	UndeliveredCableReferences int32 `json:"undeliveredCableReferences,omitempty"`

	// InterimDeviceCount counts the Devices left in their interim state by
	// the last reconcile, including any InterimDevices omits.
	// +optional
	InterimDeviceCount int32 `json:"interimDeviceCount,omitempty"`

	// InterimDevices lists Devices that still hold both the old and the new
	// IDs of components that moved, because a Cable rewrite they wait for
	// has not landed. It is bounded.
	//
	// +kubebuilder:validation:MaxItems=100
	// +listType=atomic
	// +optional
	InterimDevices []InterimDevice `json:"interimDevices,omitempty"`

	// InterimDevicesTruncated is true when InterimDevices omits entries.
	// +optional
	InterimDevicesTruncated bool `json:"interimDevicesTruncated,omitempty"`

	// PendingReleases lists records another writer created while this
	// controller was applying them, whose fields this controller has not yet
	// released. Each is retried on every Create-mode reconcile, and its target
	// stays quarantined, never adopted, until the release succeeds. While the
	// list is full, nothing is written.
	//
	// +kubebuilder:validation:MaxItems=100
	// +listType=atomic
	// +optional
	PendingReleases []PendingRelease `json:"pendingReleases,omitempty"`
}

// InterimDevice is a Device whose interim apply kept the old components its
// Cables name, and whose planned apply, which drops them, has not landed.
type InterimDevice struct {
	// Name is the Fleet Device name.
	Name string `json:"name"`
	// Source is the source record key, such as dcim/devices/12.
	Source string `json:"source"`
	// WaitingFor names the Cables whose rewrite has not landed. It is empty
	// when the planned apply itself failed.
	//
	// +kubebuilder:validation:MaxItems=16
	// +listType=atomic
	// +optional
	WaitingFor []string `json:"waitingFor,omitempty"`
	// Since is when a reconcile first left the Device interim.
	Since metav1.Time `json:"since"`
	// Message says why the Device is still interim.
	// +optional
	Message string `json:"message,omitempty"`
}

// PendingRelease is a record whose fields this controller still has to
// release.
type PendingRelease struct {
	// Kind is the Fleet kind.
	Kind string `json:"kind"`
	// Namespace is the Fleet namespace.
	Namespace string `json:"namespace"`
	// Name is the Fleet record name.
	Name string `json:"name"`
	// UID is the record's UID. A record with another UID is a different
	// record, which this controller never wrote.
	UID string `json:"uid"`
	// Since is when the first release failed.
	Since metav1.Time `json:"since"`
	// Message is the last release failure.
	// +optional
	Message string `json:"message,omitempty"`
}

// NetBoxSource connects one NetBox instance to one Fleet inventory namespace.
//
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Source",type=string,JSONPath=`.spec.sourceID`
// +kubebuilder:printcolumn:name="Mode",type=string,JSONPath=`.spec.mode`
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
type NetBoxSource struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   NetBoxSourceSpec   `json:"spec,omitempty"`
	Status NetBoxSourceStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// NetBoxSourceList contains a list of NetBoxSource.
type NetBoxSourceList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []NetBoxSource `json:"items"`
}

func init() {
	SchemeBuilder.Register(func(scheme *runtime.Scheme) error {
		scheme.AddKnownTypes(GroupVersion, &NetBoxSource{}, &NetBoxSourceList{})
		metav1.AddToGroupVersion(scheme, GroupVersion)
		return nil
	})
}
