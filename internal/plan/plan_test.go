// SPDX-License-Identifier: AGPL-3.0-only

package plan_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/yaml"

	netboxv1alpha1 "go.miloapis.com/netbox-provider/api/v1alpha1"
	"go.miloapis.com/netbox-provider/internal/fleet"
	"go.miloapis.com/netbox-provider/internal/netbox"
	"go.miloapis.com/netbox-provider/internal/netbox/netboxtest"
	"go.miloapis.com/netbox-provider/internal/plan"
)

const system = "example-netbox"

var observedAt = time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)

func load[T any](t *testing.T, collection string) []T {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(netboxtest.FixtureDir(), strings.ReplaceAll(collection, "/", "_")+".json"))
	if err != nil {
		t.Fatal(err)
	}
	var out []T
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func snapshot(t *testing.T) *netbox.Snapshot {
	return &netbox.Snapshot{
		ObservedAt:    observedAt,
		Sites:         load[netbox.Site](t, netbox.CollectionSites),
		Locations:     load[netbox.Location](t, netbox.CollectionLocations),
		Racks:         load[netbox.Rack](t, netbox.CollectionRacks),
		DeviceTypes:   load[netbox.DeviceType](t, netbox.CollectionDeviceTypes),
		Devices:       load[netbox.Device](t, netbox.CollectionDevices),
		Components:    components(t),
		Cables:        load[netbox.Cable](t, netbox.CollectionCables),
		PowerPanels:   load[netbox.PowerPanel](t, netbox.CollectionPowerPanels),
		PowerFeeds:    load[netbox.PowerFeed](t, netbox.CollectionPowerFeeds),
		IPAddresses:   load[netbox.IPAddress](t, netbox.CollectionIPAddresses),
		Manufacturers: load[netbox.Manufacturer](t, netbox.CollectionManufacturers),
		Templates:     templates(t),
	}
}

func templates(t *testing.T) map[string][]netbox.ComponentTemplate {
	out := map[string][]netbox.ComponentTemplate{}
	for _, collection := range netbox.TemplateCollections {
		out[collection] = load[netbox.ComponentTemplate](t, collection)
	}
	return out
}

func components(t *testing.T) map[string][]netbox.Component {
	out := map[string][]netbox.Component{}
	for _, collection := range netbox.ComponentCollections {
		out[collection] = load[netbox.Component](t, collection)
	}
	return out
}

func mapping(t *testing.T) netboxv1alpha1.NetBoxMappingSpec {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(netboxtest.FixtureDir(), "mapping.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var m netboxv1alpha1.NetBoxMapping
	if err := yaml.UnmarshalStrict(data, &m); err != nil {
		t.Fatal(err)
	}
	return m.Spec
}

func object(kind, name string, spec map[string]any, labels map[string]string) unstructured.Unstructured {
	obj := unstructured.Unstructured{Object: map[string]any{"spec": spec}}
	obj.SetGroupVersionKind(fleet.GVK(kind))
	obj.SetNamespace("inventory")
	obj.SetName(name)
	obj.SetLabels(labels)
	return obj
}

func facility(name string) unstructured.Unstructured {
	return object(fleet.KindFacility, name, map[string]any{"displayName": "Example premises"}, nil)
}

func input(t *testing.T) plan.Input {
	return plan.Input{
		System:    system,
		Namespace: "inventory",
		Snapshot:  snapshot(t),
		Mapping:   mapping(t),
		Existing:  plan.Existing{fleet.KindFacility: {facility("example-premises-a")}},
	}
}

func key(typ, id string) plan.SourceKey { return plan.SourceKey{Source: system, Type: typ, ID: id} }

type outcome struct {
	action plan.Action
	reason string
}

func outcomes(p *plan.Plan) map[string]outcome {
	out := map[string]outcome{}
	for _, c := range p.Candidates {
		out[c.Kind+" "+c.Source] = outcome{c.Action, c.Reason}
	}
	for _, r := range p.Records {
		out["record "+r.Source] = outcome{plan.Action(r.State), r.Reason}
	}
	return out
}

func candidate(t *testing.T, p *plan.Plan, kind, source string) *plan.Candidate {
	t.Helper()
	for _, c := range p.Candidates {
		if c.Kind == kind && c.Source == source {
			return c
		}
	}
	t.Fatalf("no %s candidate for %s", kind, source)
	return nil
}

func hasCandidate(p *plan.Plan, kind, source string) bool {
	for _, c := range p.Candidates {
		if c.Kind == kind && c.Source == source {
			return true
		}
	}
	return false
}

func spec(c *plan.Candidate) map[string]any { return c.Object.Object["spec"].(map[string]any) }

func field(t *testing.T, c *plan.Candidate, path ...string) any {
	t.Helper()
	value, _, err := unstructured.NestedFieldNoCopy(c.Object.Object, append([]string{"spec"}, path...)...)
	if err != nil {
		t.Fatal(err)
	}
	return value
}

func equalJSON(a, b any) bool {
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	return string(x) == string(y)
}

func TestFixturePlan(t *testing.T) {
	p := plan.Build(input(t))
	want := map[string]outcome{
		"Rack dcim/racks/100":                {plan.ActionCreate, plan.ReasonNew},
		"Manufacturer dcim/manufacturers/50": {plan.ActionCreate, plan.ReasonNew},
		"DeviceType dcim/device-types/500":   {plan.ActionCreate, plan.ReasonNew},
		"DeviceType dcim/device-types/503":   {plan.ActionCreate, plan.ReasonNew},
		"DeviceType dcim/device-types/504":   {plan.ActionCreate, plan.ReasonNew},
		"record dcim/device-types/501":       {"Unresolved", plan.ReasonModelAbsent},
		"PowerFeed dcim/power-feeds/6000":    {plan.ActionCreate, plan.ReasonNew},
		"DeviceClaim dcim/devices/1000":      {plan.ActionCreate, plan.ReasonNew},
		"Device dcim/devices/1000":           {plan.ActionCreate, plan.ReasonNew},
		"DeviceClaim dcim/devices/1001":      {plan.ActionCreate, plan.ReasonNew},
		"DeviceClaim dcim/devices/1002":      {plan.ActionCreate, plan.ReasonNew},
		"Device dcim/devices/1002":           {plan.ActionCreate, plan.ReasonNew},
		"DeviceClaim dcim/devices/1007":      {plan.ActionCreate, plan.ReasonNew},
		"Device dcim/devices/1007":           {plan.ActionCreate, plan.ReasonNew},
		"DeviceClaim dcim/devices/1008":      {plan.ActionCreate, plan.ReasonNew},
		"Device dcim/devices/1008":           {plan.ActionCreate, plan.ReasonNew},
		"Cable dcim/cables/5000":             {plan.ActionCreate, plan.ReasonNew},
		"Cable dcim/cables/5001":             {plan.ActionCreate, plan.ReasonNew},
		"Cable dcim/cables/5002":             {plan.ActionCreate, plan.ReasonNew},
		"Cable dcim/cables/5008":             {plan.ActionCreate, plan.ReasonNew},
		"record dcim/devices/1001":           {"Unresolved", plan.ReasonIdentityUnavailable},
		"record dcim/racks/101":              {"Unresolved", plan.ReasonPremisesUnmapped},
		"record dcim/devices/1003":           {"Excluded", plan.ReasonExcludedByRole},
		"record dcim/devices/1004":           {"Unresolved", plan.ReasonDeviceUnmapped},
		"record dcim/devices/1005":           {"Unresolved", plan.ReasonModelAbsent},
		"record dcim/devices/1006":           {"Unresolved", plan.ReasonDependencyBlocked},
		"record dcim/power-feeds/6001":       {"Excluded", plan.ReasonNotInstalled},
		"record dcim/power-feeds/6002":       {"Unresolved", plan.ReasonPowerPanelAbsent},
		"record dcim/cables/5003":            {"Unresolved", plan.ReasonEndpointUnavailable},
		"record dcim/cables/5004":            {"Unresolved", plan.ReasonEndpointUnavailable},
		"record dcim/cables/5005":            {"Excluded", plan.ReasonNotInstalled},
		"record dcim/cables/5006":            {"Unresolved", plan.ReasonCableMediumUnreviewed},
		"record dcim/cables/5007":            {"Unresolved", plan.ReasonEndpointUnsupported},
	}
	got := outcomes(p)
	for k, w := range want {
		if got[k] != w {
			t.Errorf("%s = %+v, want %+v", k, got[k], w)
		}
	}
	if len(got) != len(want) {
		t.Errorf("got %d outcomes, want %d: %v", len(got), len(want), got)
	}

	rack := candidate(t, p, fleet.KindRack, "dcim/racks/100")
	if field(t, rack, "facilityRef", "name") != "example-premises-a" || field(t, rack, "heightUnits") != float64(42) || field(t, rack, "displayName") != "Example Rack 01" {
		t.Errorf("rack spec %v", spec(rack))
	}

	device := candidate(t, p, fleet.KindDevice, "dcim/devices/1000")
	claim := candidate(t, p, fleet.KindDeviceClaim, "dcim/devices/1000")
	if field(t, device, "identity", "serial") != "SYNTH-CHASSIS-0001" || field(t, device, "assetTag") != "SYNTH-ASSET-0001" ||
		field(t, device, "identity", "model") != "Example 1U Server" || field(t, device, "identity", "manufacturer") != "Example Vendor" ||
		field(t, device, "claimRef", "name") != claim.Name || field(t, device, "topologyAuthority") != "Cable" ||
		field(t, device, "modelShape", "heightUnits") != float64(1) || field(t, device, "modelShape", "fullDepth") != true {
		t.Errorf("device spec %v", spec(device))
	}
	wantLabels := map[string]string{plan.LabelSource: system, plan.LabelType: "dcim.devices", plan.LabelID: "1000"}
	if !equalJSON(device.Object.GetLabels(), wantLabels) {
		t.Errorf("labels %v", device.Object.GetLabels())
	}
	annotations := device.Object.GetAnnotations()
	if annotations[plan.AnnotationSnapshotDigest] != input(t).Snapshot.Digest() || annotations[plan.AnnotationMappingRevision] != "example-reviewed-1" ||
		annotations[plan.AnnotationObservedAt] != "2026-01-02T03:04:05Z" {
		t.Errorf("annotations %v", annotations)
	}
	if field(t, claim, "lifecycle") != "Installation" || field(t, claim, "placement", "scope") != "RackMembership" ||
		field(t, claim, "placement", "rackRef", "name") != rack.Name || field(t, claim, "expectedIdentity", "serial") != "SYNTH-CHASSIS-0001" {
		t.Errorf("claim spec %v", spec(claim))
	}

	// A provider host ID is never a serial; the plan is kept, no Device.
	if field(t, candidate(t, p, fleet.KindDeviceClaim, "dcim/devices/1001"), "expectedIdentity", "serial") != nil {
		t.Error("provider host ID copied into the claim's expected serial")
	}
	unknownClaim := candidate(t, p, fleet.KindDeviceClaim, "dcim/devices/1002")
	if field(t, unknownClaim, "placement", "scope") != "Facility" || field(t, unknownClaim, "placement", "facilityRef", "name") != "example-premises-a" ||
		field(t, unknownClaim, "placement", "rackRef") != nil {
		t.Errorf("facility placement %v", field(t, unknownClaim, "placement"))
	}

	for _, c := range p.Candidates {
		encoded, _ := json.Marshal(c.Object.Object)
		for _, forbidden := range []string{`"provenance"`, "recordedBy", "recordedAt", `"source"`, "sourceIdentity", "sourceRef", "upstreamSources", "Import", `"status"`, "must-not-be-retained"} {
			if strings.Contains(string(encoded), forbidden) {
				t.Errorf("%s contains %s", c.Key(), forbidden)
			}
		}
		if c.Kind != fleet.KindDevice || c.Source != "dcim/devices/1000" {
			for _, forbidden := range []string{`"bmc"`, "https://"} {
				if strings.Contains(string(encoded), forbidden) {
					t.Errorf("%s contains %s; only the listed server role gets a BMC", c.Key(), forbidden)
				}
			}
		}
		if c.Object.GetNamespace() != "inventory" || c.Object.GetAPIVersion() != "fleet.datumapis.com/v1alpha1" {
			t.Errorf("%s target %s/%s", c.Key(), c.Object.GetAPIVersion(), c.Object.GetNamespace())
		}
	}
}

func TestNamesAndLabelsAreStable(t *testing.T) {
	k := key("dcim/devices", "1000")
	if k.String() != "dcim/devices/1000" {
		t.Fatalf("key %q", k.String())
	}
	// Golden value: changing the rule renames every record this controller
	// has created, which would make them unrecognizable.
	if name := plan.Name(k, fleet.KindDevice); name != "netbox-d6e24bd6be606a8873be3ef8" {
		t.Fatalf("name rule changed: %s", name)
	}
	name := plan.Name(k, fleet.KindDevice)
	if !regexp.MustCompile(`^netbox-[0-9a-f]{24}$`).MatchString(name) {
		t.Fatalf("name %q", name)
	}
	for _, other := range []string{
		plan.Name(k, fleet.KindDeviceClaim),
		plan.Name(plan.SourceKey{Source: "another-netbox", Type: k.Type, ID: k.ID}, fleet.KindDevice),
		plan.Name(key("dcim/racks", "1000"), fleet.KindDevice),
		plan.Name(key("dcim/devices", "10001"), fleet.KindDevice),
	} {
		if other == name {
			t.Fatalf("name collision %q", other)
		}
	}
	// Labels round-trip, including the collection's slash.
	obj := object(fleet.KindDevice, name, map[string]any{}, k.Labels())
	if got, ok := plan.KeyOf(&obj); !ok || got != k {
		t.Fatalf("KeyOf = %+v %v", got, ok)
	}
	for labelKey, value := range k.Labels() {
		if len(value) > 63 || !regexp.MustCompile(`^[A-Za-z0-9]([-A-Za-z0-9_.]*[A-Za-z0-9])?$`).MatchString(value) {
			t.Errorf("label %s=%q is not a valid label value", labelKey, value)
		}
	}
	// Renaming the NetBox device renames nothing in Fleet.
	in := input(t)
	before := candidate(t, plan.Build(in), fleet.KindDevice, "dcim/devices/1000").Name
	in.Snapshot.Devices[0].Name = "renamed-in-netbox"
	if after := candidate(t, plan.Build(in), fleet.KindDevice, "dcim/devices/1000").Name; after != before {
		t.Fatalf("rename changed the Fleet name: %s -> %s", before, after)
	}
}

func TestDeviceWithoutSerialIsRecordedAsUnidentified(t *testing.T) {
	in := input(t)
	for i := range in.Snapshot.Devices {
		if in.Snapshot.Devices[i].ID == 1000 {
			in.Snapshot.Devices[i].Serial = "" // reviewed as chassis, but NetBox has none
		}
	}
	p := plan.Build(in)
	device := candidate(t, p, fleet.KindDevice, "dcim/devices/1000")
	claim := candidate(t, p, fleet.KindDeviceClaim, "dcim/devices/1000")
	want := map[string]any{"manufacturer": "Example Vendor", "model": "Example 1U Server", "unidentified": true, "unidentifiedReason": plan.UnidentifiedNoSerial}
	if device.Action != plan.ActionCreate || !equalJSON(field(t, device, "identity"), want) {
		t.Fatalf("%s %v", device.Action, field(t, device, "identity"))
	}
	if field(t, claim, "expectedIdentity", "serial") != nil || field(t, device, "claimRef", "name") != claim.Name {
		t.Fatal("an unidentified Device binds only to a claim that expects no serial")
	}

	// An unreviewed serial is dropped, never written beside the marker.
	unknown := candidate(t, p, fleet.KindDevice, "dcim/devices/1002")
	if field(t, unknown, "identity", "serial") != nil || field(t, unknown, "identity", "unidentifiedReason") != plan.UnidentifiedSerialUnreviewed {
		t.Fatalf("unknown-serial identity %v", field(t, unknown, "identity"))
	}

	// A provider host ID is a provider's record, not missing identity.
	if hasCandidate(p, fleet.KindDevice, "dcim/devices/1001") || outcomes(p)["record dcim/devices/1001"].reason != plan.ReasonIdentityUnavailable {
		t.Fatal("a provider-hosted server was recorded as unidentified hardware")
	}
}

func TestFacilityMustExistAndNotBeDeleting(t *testing.T) {
	for name, existing := range map[string]plan.Existing{
		"absent": {},
		"deleting": func() plan.Existing {
			f := facility("example-premises-a")
			f.SetDeletionTimestamp(&metav1.Time{Time: observedAt})
			return plan.Existing{fleet.KindFacility: {f}}
		}(),
		"other name": {fleet.KindFacility: {facility("example-premises-b")}},
	} {
		t.Run(name, func(t *testing.T) {
			in := input(t)
			in.Existing = existing
			p := plan.Build(in)
			placed := 0
			for _, c := range p.Candidates {
				if c.Kind != fleet.KindManufacturer && c.Kind != fleet.KindDeviceType {
					placed++
				}
			}
			// The catalog is not placed, so it does not depend on a Facility.
			if placed != 0 {
				t.Fatalf("planned %d placed candidates without the mapped Facility", placed)
			}
			if got := outcomes(p)["record dcim/racks/100"]; got.reason != plan.ReasonFacilityUnavailable {
				t.Fatalf("rack outcome %+v", got)
			}
		})
	}
}

func TestLocationMappingWinsOverSite(t *testing.T) {
	in := input(t)
	in.Mapping.Facilities = append(in.Mapping.Facilities, netboxv1alpha1.FacilityMapping{
		SourceType: netboxv1alpha1.PremisesSourceSite, SourceID: 2, FacilityRef: netboxv1alpha1.LocalObjectReference{Name: "example-premises-b"},
	}, netboxv1alpha1.FacilityMapping{
		SourceType: netboxv1alpha1.PremisesSourceSite, SourceID: 1, FacilityRef: netboxv1alpha1.LocalObjectReference{Name: "example-premises-b"},
	})
	in.Existing[fleet.KindFacility] = append(in.Existing[fleet.KindFacility], facility("example-premises-b"))
	p := plan.Build(in)
	if got := field(t, candidate(t, p, fleet.KindRack, "dcim/racks/100"), "facilityRef", "name"); got != "example-premises-a" {
		t.Errorf("rack with a mapped location used %v", got)
	}
	if got := field(t, candidate(t, p, fleet.KindRack, "dcim/racks/101"), "facilityRef", "name"); got != "example-premises-b" {
		t.Errorf("rack without a location used %v", got)
	}
	if got := outcomes(p)["Device dcim/devices/1006"]; got.action != plan.ActionCreate {
		t.Errorf("device in a now-mapped rack: %+v", got)
	}
}

func TestMappedSourceAbsentAndExclusions(t *testing.T) {
	in := input(t)
	in.Mapping.Facilities = append(in.Mapping.Facilities, netboxv1alpha1.FacilityMapping{
		SourceType: netboxv1alpha1.PremisesSourceSite, SourceID: 99, FacilityRef: netboxv1alpha1.LocalObjectReference{Name: "example-premises-a"},
	})
	in.Mapping.Devices = append(in.Mapping.Devices, netboxv1alpha1.DeviceMapping{ID: 1004, Classification: netboxv1alpha1.DeviceClassificationExcluded, Reason: "synthetic spare"})
	in.Mapping.Exclusions = append(in.Mapping.Exclusions, netboxv1alpha1.RoleExclusion{RoleSlugs: []string{"server"}, Reason: "synthetic exclusion"})
	got := outcomes(plan.Build(in))
	if got["record dcim/sites/99"].reason != plan.ReasonMappedSourceAbsent {
		t.Errorf("absent mapped source: %+v", got["record dcim/sites/99"])
	}
	for _, id := range []string{"1000", "1001", "1002", "1004"} {
		if got["record dcim/devices/"+id].reason != plan.ReasonExcludedByRole {
			t.Errorf("device %s: %+v", id, got["record dcim/devices/"+id])
		}
	}
}

// previous returns what an earlier run created, with an older observation,
// as it would now exist in Fleet.
func previous(t *testing.T) []unstructured.Unstructured {
	in := input(t)
	in.Snapshot.ObservedAt = observedAt.Add(-24 * time.Hour)
	in.Mapping.Revision = "example-reviewed-0"
	var out []unstructured.Unstructured
	for _, c := range plan.Build(in).Candidates {
		out = append(out, *applied(c.Object.DeepCopy()))
	}
	return out
}

// applied records a server-side apply by this controller, as the API server
// would after a write.
func applied(obj *unstructured.Unstructured) *unstructured.Unstructured {
	obj.SetUID(types.UID("uid-" + obj.GetName()))
	obj.SetManagedFields(append(obj.GetManagedFields(), metav1.ManagedFieldsEntry{
		Manager: plan.FieldManager, Operation: metav1.ManagedFieldsOperationApply, APIVersion: "fleet.datumapis.com/v1alpha1",
		FieldsV1: ownedListItems(obj),
	}))
	return obj
}

func withExisting(in plan.Input, objs ...unstructured.Unstructured) plan.Input {
	for _, obj := range objs {
		in.Existing[obj.GetKind()] = append(in.Existing[obj.GetKind()], obj)
	}
	return in
}

func TestRecordsCorrelatedByLabelsAreUnchanged(t *testing.T) {
	objs := previous(t)
	for i := range objs {
		if objs[i].GetKind() == fleet.KindDevice {
			// Fields another writer added are tolerated and never touched.
			_ = unstructured.SetNestedStringSlice(objs[i].Object, []string{"02:00:00:00:00:01"}, "spec", "unmappedMACAddresses")
		}
	}
	p := plan.Build(withExisting(input(t), objs...))
	if p.Count(plan.ActionUnchanged) != len(objs) || p.Count(plan.ActionCreate) != 0 {
		t.Fatalf("outcomes %v", outcomes(p))
	}
}

func TestSameNameWithoutOurLabelsIsNeverAdopted(t *testing.T) {
	objs := previous(t)
	for i := range objs {
		objs[i].SetLabels(nil) // identical spec, but not written by this controller
	}
	p := plan.Build(withExisting(input(t), objs...))
	if p.Count(plan.ActionUnchanged) != 0 {
		t.Fatalf("adopted records without labels: %v", outcomes(p))
	}
	rack := candidate(t, p, fleet.KindRack, "dcim/racks/100")
	if rack.Action != plan.ActionQuarantined || rack.Reason != plan.ReasonNameConflict {
		t.Fatalf("rack %+v", rack)
	}
}

func TestOwnedFieldChangeIsUpdated(t *testing.T) {
	objs := previous(t)
	for i := range objs {
		if objs[i].GetKind() == fleet.KindRack {
			// Applied before NetBox changed the rack's height.
			_ = unstructured.SetNestedField(objs[i].Object, int64(48), "spec", "heightUnits")
			annotations := objs[i].GetAnnotations()
			annotations[plan.AnnotationAppliedDigest] = "sha256:previous"
			objs[i].SetAnnotations(annotations)
		}
	}
	p := plan.Build(withExisting(input(t), objs...))
	rack := candidate(t, p, fleet.KindRack, "dcim/racks/100")
	if rack.Action != plan.ActionUpdate || rack.Reason != plan.ReasonOwnedFieldsChanged || rack.ExistingUID != types.UID("uid-"+rack.Name) {
		t.Fatalf("rack %+v", rack)
	}
	if field(t, rack, "heightUnits") != float64(42) {
		t.Fatalf("the update applies NetBox's value: %v", field(t, rack, "heightUnits"))
	}
	got := outcomes(p)
	if got["Device dcim/devices/1000"].action != plan.ActionUnchanged || got["DeviceClaim dcim/devices/1000"].action != plan.ActionUnchanged {
		t.Fatalf("an update of the rack changes nothing else: %v", got)
	}

	// A field this controller applied before but no longer maps is removed by
	// the next apply, so a stale digest alone means an update.
	objs = previous(t)
	for i := range objs {
		if objs[i].GetKind() == fleet.KindDevice {
			annotations := objs[i].GetAnnotations()
			annotations[plan.AnnotationAppliedDigest] = "sha256:previous"
			objs[i].SetAnnotations(annotations)
		}
	}
	if got := outcomes(plan.Build(withExisting(input(t), objs...)))["Device dcim/devices/1000"]; got.action != plan.ActionUpdate {
		t.Fatalf("stale digest: %+v", got)
	}
}

func TestRecordNotAppliedByThisControllerIsNeverAdopted(t *testing.T) {
	objs := previous(t)
	for i := range objs {
		objs[i].SetManagedFields([]metav1.ManagedFieldsEntry{{Manager: "kubectl", Operation: metav1.ManagedFieldsOperationUpdate}})
	}
	p := plan.Build(withExisting(input(t), objs...))
	if n := p.Count(plan.ActionUnchanged) + p.Count(plan.ActionUpdate); n != 0 {
		t.Fatalf("%d records with our labels but another manager were adopted", n)
	}
	if rack := candidate(t, p, fleet.KindRack, "dcim/racks/100"); rack.Reason != plan.ReasonNotApplyOwned {
		t.Fatalf("rack %+v", rack)
	}
}

func TestImmutableChangesAreQuarantined(t *testing.T) {
	for name, tc := range map[string]struct {
		kind, source string
		mutate       func(*unstructured.Unstructured)
		message      string
	}{
		"hardware identity": {fleet.KindDevice, "dcim/devices/1000", func(o *unstructured.Unstructured) {
			_ = unstructured.SetNestedField(o.Object, "SYNTH-CHASSIS-OLD", "spec", "identity", "serial")
		}, "a hardware swap is a new Device"},
		"claim": {fleet.KindDevice, "dcim/devices/1000", func(o *unstructured.Unstructured) {
			_ = unstructured.SetNestedField(o.Object, "another-claim", "spec", "claimRef", "name")
		}, "claimRef immutable"},
		"placement": {fleet.KindDeviceClaim, "dcim/devices/1007", func(o *unstructured.Unstructured) {
			_ = unstructured.SetNestedField(o.Object, int64(39), "spec", "placement", "position")
		}, "explicit handover"},
	} {
		t.Run(name, func(t *testing.T) {
			objs := previous(t)
			for i := range objs {
				if objs[i].GetKind() == tc.kind {
					if ref, _ := plan.KeyOf(&objs[i]); ref.String() == tc.source {
						tc.mutate(&objs[i])
					}
				}
			}
			c := candidate(t, plan.Build(withExisting(input(t), objs...)), tc.kind, tc.source)
			if c.Action != plan.ActionQuarantined || c.Reason != plan.ReasonImmutableChanged || !strings.Contains(c.Message, tc.message) {
				t.Fatalf("%+v", c)
			}
		})
	}
}

func TestExistingNameFromAnotherWriterIsQuarantined(t *testing.T) {
	in := input(t)
	rackName := candidate(t, plan.Build(in), fleet.KindRack, "dcim/racks/100").Name
	flux := object(fleet.KindRack, rackName, map[string]any{
		"displayName": "Example Rack 01", "facilityRef": map[string]any{"name": "example-premises-a"},
	}, map[string]string{"kustomize.toolkit.fluxcd.io/name": "example-inventory"})
	p := plan.Build(withExisting(in, flux))
	rack := candidate(t, p, fleet.KindRack, "dcim/racks/100")
	if rack.Action != plan.ActionQuarantined || rack.Reason != plan.ReasonNameConflict || !strings.Contains(rack.Message, "Flux") {
		t.Fatalf("rack %+v", rack)
	}
	got := outcomes(p)
	for _, k := range []string{"DeviceClaim dcim/devices/1000", "Device dcim/devices/1000", "DeviceClaim dcim/devices/1001"} {
		if got[k].reason != plan.ReasonDependencyBlocked {
			t.Errorf("%s %+v", k, got[k])
		}
	}
	if got["DeviceClaim dcim/devices/1002"].action != plan.ActionCreate {
		t.Errorf("independent claim blocked: %+v", got["DeviceClaim dcim/devices/1002"])
	}
}

func TestSourceLabelledUnderAnotherNameIsNotDuplicated(t *testing.T) {
	earlier := object(fleet.KindDevice, "netbox-renamed-earlier", map[string]any{
		"claimRef": map[string]any{"name": "earlier-claim"},
		"identity": map[string]any{"model": "Example 1U Server", "serial": "OTHER-SERIAL"},
	}, key("dcim/devices", "1000").Labels())
	got := outcomes(plan.Build(withExisting(input(t), earlier)))
	if got["Device dcim/devices/1000"].reason != plan.ReasonSourceAlreadyRecorded {
		t.Fatalf("device %+v", got["Device dcim/devices/1000"])
	}
	if got["DeviceClaim dcim/devices/1000"].reason != plan.ReasonDependencyBlocked {
		t.Fatalf("claim %+v", got["DeviceClaim dcim/devices/1000"])
	}
}

func TestIdentityCollisionsAreQuarantined(t *testing.T) {
	manual := func(identity map[string]any, extra map[string]any) unstructured.Unstructured {
		s := map[string]any{"claimRef": map[string]any{"name": "legacy-slot"}, "identity": identity}
		for k, v := range extra {
			s[k] = v
		}
		return object(fleet.KindDevice, "legacy", s, nil) // not written by this controller
	}
	for name, existing := range map[string]unstructured.Unstructured{
		"serial":                           manual(map[string]any{"model": "x", "serial": "SYNTH-CHASSIS-0001"}, nil),
		"asset tag":                        manual(map[string]any{"model": "x", "serial": "OTHER"}, map[string]any{"assetTag": "SYNTH-ASSET-0001"}),
		"serial recorded as provider host": manual(map[string]any{"model": "x", "serial": "OTHER"}, map[string]any{"providerHostRef": map[string]any{"system": "example-hosting", "type": "host", "id": "SYNTH-CHASSIS-0001"}}),
	} {
		t.Run(name, func(t *testing.T) {
			got := outcomes(plan.Build(withExisting(input(t), existing)))
			if got["Device dcim/devices/1000"].reason != plan.ReasonIdentityCollision || got["DeviceClaim dcim/devices/1000"].reason != plan.ReasonDependencyBlocked {
				t.Fatalf("%+v %+v", got["Device dcim/devices/1000"], got["DeviceClaim dcim/devices/1000"])
			}
		})
	}
	t.Run("two source devices", func(t *testing.T) {
		in := input(t)
		for i := range in.Snapshot.Devices {
			if in.Snapshot.Devices[i].ID == 1002 {
				in.Snapshot.Devices[i].AssetTag = "SYNTH-ASSET-0001"
			}
		}
		for i := range in.Mapping.Devices {
			if in.Mapping.Devices[i].ID == 1002 {
				in.Mapping.Devices[i].SerialMeaning = netboxv1alpha1.SerialMeaningChassis
			}
		}
		got := outcomes(plan.Build(in))
		if got["Device dcim/devices/1000"].reason != plan.ReasonIdentityCollision || got["Device dcim/devices/1002"].reason != plan.ReasonIdentityCollision {
			t.Fatalf("%+v %+v", got["Device dcim/devices/1000"], got["Device dcim/devices/1002"])
		}
	})
}

func TestOmittedSourceRecordsAreRetained(t *testing.T) {
	objs := previous(t)
	foreign := object(fleet.KindRack, "foreign", map[string]any{}, plan.SourceKey{Source: "another-netbox", Type: "dcim/racks", ID: "7"}.Labels())
	in := withExisting(input(t), append(objs, foreign)...)
	in.Snapshot.Devices = in.Snapshot.Devices[1:] // device 1000 disappears from NetBox
	p := plan.Build(in)
	if len(p.Retained) != 2 {
		t.Fatalf("retained %+v", p.Retained)
	}
	for _, r := range p.Retained {
		if r.Source != "dcim/devices/1000" || r.State != netboxv1alpha1.RecordStateRetainedMissingSource {
			t.Errorf("retained %+v", r)
		}
	}
	for _, c := range p.Candidates {
		if c.Source == "dcim/devices/1000" {
			t.Errorf("planned %s for an omitted source", c.Key())
		}
	}
}
