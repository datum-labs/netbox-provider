// SPDX-License-Identifier: AGPL-3.0-only

// Package plan turns a NetBox snapshot, a reviewed mapping, and the existing
// Fleet records into an additive plan.
//
// Build is a pure function. The plan never contains an update, patch,
// adoption or deletion: each target is either created, already present and
// equal, or blocked with a reason. Existing records are never overwritten,
// whoever wrote them.
package plan

import (
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	netboxv1alpha1 "go.miloapis.com/netbox-provider/api/v1alpha1"
	"go.miloapis.com/netbox-provider/internal/fleet"
	"go.miloapis.com/netbox-provider/internal/netbox"
)

// Reasons are stable CamelCase causes reported in status.
const (
	ReasonNew                   = "New"
	ReasonUnchanged             = "Unchanged"
	ReasonNameConflict          = "NameConflict"
	ReasonExistingDiffers       = "ExistingDiffers"
	ReasonExistingDeleting      = "ExistingDeleting"
	ReasonSourceAlreadyRecorded = "SourceAlreadyRecorded"
	ReasonIdentityCollision     = "IdentityCollision"
	ReasonDependencyBlocked     = "DependencyBlocked"
	ReasonPremisesUnmapped      = "PremisesUnmapped"
	ReasonFacilityUnavailable   = "FacilityUnavailable"
	ReasonMappedSourceAbsent    = "MappedSourceAbsent"
	ReasonDeviceUnmapped        = "DeviceUnmapped"
	ReasonExcludedByMapping     = "ExcludedByMapping"
	ReasonExcludedByRole        = "ExcludedByRole"
	ReasonDeviceTypeAbsent      = "DeviceTypeAbsent"
	ReasonModelAbsent           = "ModelAbsent"
	ReasonRackAbsent            = "RackAbsent"
	ReasonFieldTooLong          = "FieldTooLong"
	ReasonMissingSource         = "MissingFromSnapshot"
	ReasonIdentityUnavailable   = "IdentityUnavailable"
)

// Source record types: the NetBox collection paths, used in this
// controller's correlation labels.
const (
	TypeRack   = netbox.CollectionRacks
	TypeDevice = netbox.CollectionDevices
)

// Action is what happens to a planned target.
type Action string

// Actions. Create is the only action with an effect.
const (
	ActionCreate      Action = "Create"
	ActionUnchanged   Action = "Unchanged"
	ActionQuarantined Action = "Quarantined"
	ActionUnresolved  Action = "Unresolved"
)

// Candidate is one planned Fleet record.
type Candidate struct {
	Kind      string
	Name      string
	Source    string
	Object    *unstructured.Unstructured
	Action    Action
	Reason    string
	Message   string
	DependsOn []string
}

// Key is Kind/name.
func (c *Candidate) Key() string { return c.Kind + "/" + c.Name }

// Record is a source record that produced no candidate.
type Record struct {
	Source  string
	State   netboxv1alpha1.RecordState
	Reason  string
	Message string
}

// Existing holds the Fleet records of the target namespace, by kind. Every
// listed kind must be complete; a partial list must not reach Build.
type Existing map[string][]unstructured.Unstructured

// Input is everything Build reads.
type Input struct {
	// System is the opaque source identifier (NetBoxSource.spec.sourceID).
	System    string
	Namespace string
	Snapshot  *netbox.Snapshot
	Mapping   netboxv1alpha1.NetBoxMappingSpec
	Existing  Existing
}

// Plan is the result of Build.
type Plan struct {
	Candidates []*Candidate
	Records    []Record
	// Retained lists existing records from this source that the snapshot
	// no longer contains. They are retained, never deleted.
	Retained []Record
}

// Count returns the number of candidates with an action.
func (p *Plan) Count(action Action) int {
	n := 0
	for _, c := range p.Candidates {
		if c.Action == action {
			n++
		}
	}
	return n
}

type builder struct {
	in         Input
	plan       *Plan
	facilities map[string]*unstructured.Unstructured
	byKey      map[string]*Candidate
	rackByID   map[int64]*Candidate
	premises   map[netboxv1alpha1.PremisesSourceType]map[int64]string
	sourceAt   string
}

// Build computes the plan. It never mutates its input.
func Build(in Input) *Plan {
	b := &builder{
		in:         in,
		plan:       &Plan{},
		facilities: map[string]*unstructured.Unstructured{},
		byKey:      map[string]*Candidate{},
		rackByID:   map[int64]*Candidate{},
		premises:   map[netboxv1alpha1.PremisesSourceType]map[int64]string{},
		sourceAt:   in.Snapshot.ObservedAt.UTC().Format(time.RFC3339),
	}
	for i := range in.Existing[fleet.KindFacility] {
		f := &in.Existing[fleet.KindFacility][i]
		b.facilities[f.GetName()] = f
	}
	b.mapPremises()
	b.racks()
	b.devices()
	b.reconcile()
	b.collisions()
	b.propagate()
	b.retained()
	return b.plan
}

func (b *builder) mapPremises() {
	sites := map[int64]bool{}
	for _, s := range b.in.Snapshot.Sites {
		sites[s.ID] = true
	}
	locations := map[int64]bool{}
	for _, l := range b.in.Snapshot.Locations {
		locations[l.ID] = true
	}
	b.premises[netboxv1alpha1.PremisesSourceSite] = map[int64]string{}
	b.premises[netboxv1alpha1.PremisesSourceLocation] = map[int64]string{}
	for _, m := range b.in.Mapping.Facilities {
		present := sites[m.SourceID]
		collection := netbox.CollectionSites
		if m.SourceType == netboxv1alpha1.PremisesSourceLocation {
			present = locations[m.SourceID]
			collection = netbox.CollectionLocations
		}
		if !present {
			b.record(collection+"/"+strconv.FormatInt(m.SourceID, 10), netboxv1alpha1.RecordStateUnresolved, ReasonMappedSourceAbsent,
				"the mapping names a site or location that the scoped snapshot does not contain")
			continue
		}
		b.premises[m.SourceType][m.SourceID] = m.FacilityRef.Name
	}
}

// facility resolves premises: a mapped location wins over its site. The
// Facility must already exist in the target namespace and not be deleting.
func (b *builder) facility(site, location *netbox.Ref) (string, string, string) {
	name := ""
	if location != nil {
		name = b.premises[netboxv1alpha1.PremisesSourceLocation][location.ID]
	}
	if name == "" && site != nil {
		name = b.premises[netboxv1alpha1.PremisesSourceSite][site.ID]
	}
	if name == "" {
		return "", ReasonPremisesUnmapped, "no reviewed Facility mapping for this site or location"
	}
	f, ok := b.facilities[name]
	if !ok || f.GetDeletionTimestamp() != nil {
		return "", ReasonFacilityUnavailable, fmt.Sprintf("mapped Facility %q does not exist in the target namespace or is being deleted", name)
	}
	return name, "", ""
}

func (b *builder) ref(typ string, id int64) SourceKey {
	return SourceKey{Source: b.in.System, Type: typ, ID: strconv.FormatInt(id, 10)}
}

func (b *builder) add(kind, source string, spec any, ref SourceKey, deps ...string) *Candidate {
	name := Name(ref, kind)
	annotations := map[string]string{
		AnnotationSnapshotDigest:  b.in.Snapshot.Digest(),
		AnnotationMappingRevision: b.in.Mapping.Revision,
		AnnotationObservedAt:      b.sourceAt,
	}
	obj, err := fleet.NewObject(kind, b.in.Namespace, name, ref.Labels(), annotations, spec)
	c := &Candidate{Kind: kind, Name: name, Source: source, Object: obj, Action: ActionCreate, Reason: ReasonNew, DependsOn: deps}
	if err != nil {
		c.Action, c.Reason, c.Message = ActionUnresolved, ReasonFieldTooLong, "record cannot be encoded"
	}
	b.plan.Candidates = append(b.plan.Candidates, c)
	b.byKey[c.Key()] = c
	return c
}

func (b *builder) record(source string, state netboxv1alpha1.RecordState, reason, message string) {
	b.plan.Records = append(b.plan.Records, Record{Source: source, State: state, Reason: reason, Message: message})
}

func displayName(name, fallback string) string {
	if name == "" || len(name) > 253 {
		return fallback
	}
	return name
}

func (b *builder) racks() {
	for _, rack := range b.in.Snapshot.Racks {
		source := TypeRack + "/" + strconv.FormatInt(rack.ID, 10)
		facility, reason, message := b.facility(rack.Site, rack.Location)
		if facility == "" {
			b.record(source, netboxv1alpha1.RecordStateUnresolved, reason, message)
			continue
		}
		ref := b.ref(TypeRack, rack.ID)
		spec := fleet.RackSpec{
			DisplayName: displayName(rack.Name, Name(ref, fleet.KindRack)),
			FacilityRef: fleet.LocalReference{Name: facility},
		}
		if rack.UHeight != nil && float64(*rack.UHeight) == float64(int32(*rack.UHeight)) && *rack.UHeight >= 1 && *rack.UHeight <= 100 {
			spec.HeightUnits = int32(*rack.UHeight)
		}
		b.rackByID[rack.ID] = b.add(fleet.KindRack, source, spec, ref)
	}
}

func (b *builder) devices() {
	types := map[int64]netbox.DeviceType{}
	for _, t := range b.in.Snapshot.DeviceTypes {
		types[t.ID] = t
	}
	mappings := map[int64]netboxv1alpha1.DeviceMapping{}
	for _, m := range b.in.Mapping.Devices {
		mappings[m.ID] = m
	}
	excludedRoles := map[string]string{}
	for _, e := range b.in.Mapping.Exclusions {
		for _, slug := range e.RoleSlugs {
			excludedRoles[slug] = e.Reason
		}
	}
	for _, device := range b.in.Snapshot.Devices {
		source := TypeDevice + "/" + strconv.FormatInt(device.ID, 10)
		if device.Role != nil {
			if reason, ok := excludedRoles[device.Role.Slug]; ok {
				b.record(source, netboxv1alpha1.RecordStateExcluded, ReasonExcludedByRole, reason)
				continue
			}
		}
		m, ok := mappings[device.ID]
		if !ok {
			b.record(source, netboxv1alpha1.RecordStateUnresolved, ReasonDeviceUnmapped, "no reviewed classification for this device; nothing is imported by default")
			continue
		}
		if m.Classification == netboxv1alpha1.DeviceClassificationExcluded {
			b.record(source, netboxv1alpha1.RecordStateExcluded, ReasonExcludedByMapping, m.Reason)
			continue
		}
		b.device(source, device, m, types)
	}
}

func tooLong(values ...string) bool {
	for _, v := range values {
		if len(v) > 253 {
			return true
		}
	}
	return false
}

func (b *builder) device(source string, device netbox.Device, m netboxv1alpha1.DeviceMapping, types map[int64]netbox.DeviceType) {
	unresolved := func(reason, message string) {
		b.record(source, netboxv1alpha1.RecordStateUnresolved, reason, message)
	}
	if device.DeviceType == nil {
		unresolved(ReasonDeviceTypeAbsent, "device has no device type")
		return
	}
	dt, ok := types[device.DeviceType.ID]
	if !ok {
		unresolved(ReasonDeviceTypeAbsent, "device type is not in the snapshot")
		return
	}
	if dt.Model == "" {
		unresolved(ReasonModelAbsent, "device type has no model; Fleet requires one")
		return
	}
	manufacturer := ""
	if dt.Manufacturer != nil {
		manufacturer = dt.Manufacturer.Name
	}
	if tooLong(dt.Model, manufacturer, device.Serial, device.AssetTag) {
		b.record(source, netboxv1alpha1.RecordStateUnsupported, ReasonFieldTooLong, "a source field exceeds Fleet's 253 character limit")
		return
	}

	var placement fleet.Placement
	var deps []string
	switch m.Placement {
	case netboxv1alpha1.PlacementScopeRackMembership:
		if device.Rack == nil {
			unresolved(ReasonRackAbsent, "RackMembership placement needs a rack in NetBox")
			return
		}
		rack, ok := b.rackByID[device.Rack.ID]
		if !ok {
			unresolved(ReasonDependencyBlocked, "the device's rack is not in the snapshot or is unresolved")
			return
		}
		placement = fleet.Placement{Scope: string(netboxv1alpha1.PlacementScopeRackMembership), RackRef: &fleet.LocalReference{Name: rack.Name}}
		deps = append(deps, rack.Key())
	default:
		facility, reason, message := b.facility(device.Site, device.Location)
		if facility == "" {
			unresolved(reason, message)
			return
		}
		placement = fleet.Placement{Scope: string(netboxv1alpha1.PlacementScopeFacility), FacilityRef: &fleet.LocalReference{Name: facility}}
	}

	ref := b.ref(TypeDevice, device.ID)
	claimName := Name(ref, fleet.KindDeviceClaim)
	expected := fleet.ExpectedIdentity{Manufacturer: manufacturer, Model: dt.Model}
	identity := fleet.DeviceIdentity{Manufacturer: manufacturer, Model: dt.Model}
	spec := fleet.DeviceSpec{
		DisplayName:       displayName(device.Name, ""),
		AssetTag:          device.AssetTag,
		TopologyAuthority: "Cable",
		ClaimRef:          fleet.LocalReference{Name: claimName},
	}
	if m.SerialMeaning == netboxv1alpha1.SerialMeaningChassis {
		identity.Serial = device.Serial
		expected.Serial = device.Serial
	}
	spec.Identity = identity
	shape := fleet.ModelShape{}
	if dt.UHeight != nil && *dt.UHeight >= 0 && *dt.UHeight <= 100 {
		h := float64(*dt.UHeight)
		shape.HeightUnits = &h
	}
	if dt.IsFullDepth != nil {
		v := *dt.IsFullDepth
		shape.FullDepth = &v
	}
	if shape != (fleet.ModelShape{}) {
		spec.ModelShape = &shape
	}
	claim := fleet.DeviceClaimSpec{
		DisplayName:      displayName(device.Name, ""),
		Lifecycle:        "Installation",
		ExpectedIdentity: expected,
		Placement:        placement,
	}
	claimCandidate := b.add(fleet.KindDeviceClaim, source, claim, ref, deps...)
	// Fleet requires a serial or system UUID for a manually recorded Device.
	// Without a reviewed chassis serial the installation plan is still
	// recorded, and registration is deferred; identity is never invented.
	if identity.Serial == "" {
		message := "no reviewed chassis serial or system UUID; the DeviceClaim is planned and the Device is not created"
		if m.SerialMeaning == netboxv1alpha1.SerialMeaningProviderHostID {
			message = "the serial is a provider host ID, not a chassis serial; the DeviceClaim is planned and the Device is not created"
		}
		b.record(source, netboxv1alpha1.RecordStateUnresolved, ReasonIdentityUnavailable, message)
		return
	}
	b.add(fleet.KindDevice, source, spec, ref, claimCandidate.Key())
}

// reconcile compares candidates with existing records of the same name and
// with existing records carrying the same correlation labels under another
// name.
func (b *builder) reconcile() {
	existing := map[string]*unstructured.Unstructured{}
	bySource := map[string]string{}
	for kind, items := range b.in.Existing {
		for i := range items {
			obj := &items[i]
			existing[kind+"/"+obj.GetName()] = obj
			if ref, ok := KeyOf(obj); ok {
				bySource[kind+"/"+ref.qualified()] = obj.GetName()
			}
		}
	}
	for _, c := range b.plan.Candidates {
		if c.Action != ActionCreate {
			continue
		}
		ref, _ := KeyOf(c.Object)
		current, ok := existing[c.Key()]
		switch {
		case ok && current.GetDeletionTimestamp() != nil:
			c.Action, c.Reason, c.Message = ActionQuarantined, ReasonExistingDeleting, "a record with this name is being deleted; nothing is written"
		case ok:
			c.Action, c.Reason, c.Message = Compare(current, c.Object)
		default:
			if other, found := bySource[c.Kind+"/"+ref.qualified()]; found && other != c.Name {
				c.Action, c.Reason = ActionQuarantined, ReasonSourceAlreadyRecorded
				c.Message = fmt.Sprintf("%s %q already carries this source record's labels; no duplicate is created", c.Kind, other)
			}
		}
	}
}

// Compare classifies an existing record with the candidate's name. A record
// without this controller's matching correlation labels was not written by it
// and is never adopted. Otherwise it is Unchanged only when every spec field
// the candidate would write is present and equal, ignoring annotations. Fields that another writer added
// are allowed and never touched.
func Compare(current, candidate *unstructured.Unstructured) (Action, string, string) {
	want, _ := KeyOf(candidate)
	got, ok := KeyOf(current)
	if !ok || got != want {
		writer := "another writer"
		for key := range current.GetLabels() {
			if strings.HasSuffix(key, ".toolkit.fluxcd.io/name") {
				writer = "Flux"
			}
		}
		return ActionQuarantined, ReasonNameConflict, fmt.Sprintf("a record with this name exists from %s without this source's labels; it is never overwritten or adopted", writer)
	}
	currentSpec := normalize(current.Object["spec"])
	candidateSpec := normalize(candidate.Object["spec"])
	if !subset(candidateSpec, currentSpec) {
		return ActionQuarantined, ReasonExistingDiffers, "the record this controller created differs from NetBox now; corrections are reviewed, never last-import-wins"
	}
	return ActionUnchanged, ReasonUnchanged, ""
}

func normalize(v any) map[string]any {
	data, _ := json.Marshal(v)
	out := map[string]any{}
	_ = json.Unmarshal(data, &out)
	return out
}

// subset reports whether every field of want is in got with an equal value.
// Lists must be equal.
func subset(want, got map[string]any) bool {
	for key, w := range want {
		g, ok := got[key]
		if !ok {
			return false
		}
		wm, wIsMap := w.(map[string]any)
		gm, gIsMap := g.(map[string]any)
		if wIsMap && gIsMap {
			if !subset(wm, gm) {
				return false
			}
			continue
		}
		if !reflect.DeepEqual(w, g) {
			return false
		}
	}
	return true
}

// collisions quarantines Device candidates whose identity facts match another
// Device, existing or planned. Matching is evidence for human adjudication,
// never a merge.
func (b *builder) collisions() {
	type device struct {
		name string
		spec fleet.DeviceSpec
		c    *Candidate
	}
	var all []device
	decode := func(obj *unstructured.Unstructured) fleet.DeviceSpec {
		var spec fleet.DeviceSpec
		data, _ := json.Marshal(obj.Object["spec"])
		_ = json.Unmarshal(data, &spec)
		return spec
	}
	planned := map[string]bool{}
	for _, c := range b.plan.Candidates {
		if c.Kind == fleet.KindDevice {
			planned[c.Name] = true
			all = append(all, device{name: c.Name, spec: decode(c.Object), c: c})
		}
	}
	for i := range b.in.Existing[fleet.KindDevice] {
		obj := &b.in.Existing[fleet.KindDevice][i]
		if !planned[obj.GetName()] {
			all = append(all, device{name: obj.GetName(), spec: decode(obj)})
		}
	}
	matches := func(a, o fleet.DeviceSpec) bool {
		eq := func(x, y string) bool { return x != "" && x == y }
		hostEq := func(x, y *fleet.ExternalReference) bool { return x != nil && y != nil && *x == *y }
		hostID := func(x *fleet.ExternalReference) string {
			if x == nil {
				return ""
			}
			return x.ID
		}
		return eq(a.Identity.Serial, o.Identity.Serial) || eq(a.Identity.SystemUUID, o.Identity.SystemUUID) ||
			eq(a.AssetTag, o.AssetTag) || hostEq(a.ProviderHostRef, o.ProviderHostRef) ||
			eq(hostID(a.ProviderHostRef), o.Identity.Serial) || eq(a.Identity.Serial, hostID(o.ProviderHostRef))
	}
	for i, a := range all {
		if a.c == nil || a.c.Action != ActionCreate {
			continue
		}
		for j, o := range all {
			if i == j || !matches(a.spec, o.spec) {
				continue
			}
			a.c.Action, a.c.Reason = ActionQuarantined, ReasonIdentityCollision
			a.c.Message = fmt.Sprintf("serial, system UUID, asset tag or provider host ID matches Device %q; adjudicate before import", o.name)
			break
		}
	}
}

// propagate blocks creates whose dependencies will not exist, and keeps a
// source record's claim and device together.
func (b *builder) propagate() {
	usable := func(c *Candidate) bool { return c.Action == ActionCreate || c.Action == ActionUnchanged }
	for changed := true; changed; {
		changed = false
		cohorts := map[string][]*Candidate{}
		for _, c := range b.plan.Candidates {
			if c.Kind != fleet.KindRack {
				cohorts[c.Source] = append(cohorts[c.Source], c)
			}
		}
		for _, c := range b.plan.Candidates {
			if c.Action != ActionCreate {
				continue
			}
			for _, dep := range c.DependsOn {
				if d := b.byKey[dep]; d != nil && !usable(d) {
					c.Action, c.Reason, c.Message = ActionUnresolved, ReasonDependencyBlocked, dep+" is not created: "+d.Reason
					changed = true
					break
				}
			}
			if c.Action != ActionCreate {
				continue
			}
			for _, peer := range cohorts[c.Source] {
				if peer != c && !usable(peer) {
					c.Action, c.Reason, c.Message = ActionUnresolved, ReasonDependencyBlocked, peer.Key()+" from the same source record is blocked: "+peer.Reason
					changed = true
					break
				}
			}
		}
	}
}

func (b *builder) retained() {
	present := map[string]bool{}
	for _, r := range b.in.Snapshot.Racks {
		present[TypeRack+"/"+strconv.FormatInt(r.ID, 10)] = true
	}
	for _, d := range b.in.Snapshot.Devices {
		present[TypeDevice+"/"+strconv.FormatInt(d.ID, 10)] = true
	}
	for _, kind := range fleet.WriteKinds {
		for i := range b.in.Existing[kind] {
			obj := &b.in.Existing[kind][i]
			ref, ok := KeyOf(obj)
			if !ok || ref.Source != b.in.System || present[ref.String()] {
				continue
			}
			b.plan.Retained = append(b.plan.Retained, Record{
				Source:  ref.String(),
				State:   netboxv1alpha1.RecordStateRetainedMissingSource,
				Reason:  ReasonMissingSource,
				Message: kind + "/" + obj.GetName() + " is retained; absence from NetBox never deletes, retires or frees a record",
			})
		}
	}
	sort.Slice(b.plan.Retained, func(i, j int) bool { return b.plan.Retained[i].Message < b.plan.Retained[j].Message })
}
