// SPDX-License-Identifier: AGPL-3.0-only

package plan_test

import (
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"go.miloapis.com/netbox-provider/internal/fleet"
	"go.miloapis.com/netbox-provider/internal/netbox"
	"go.miloapis.com/netbox-provider/internal/plan"
)

func TestModelKey(t *testing.T) {
	for _, tc := range []struct{ slug, model, want string }{
		{"example-1u", "Example 1U Server", "example-1u"},
		{"", "Example 1U Server", "example-1u-server"},
		{"example_1u", "", "example-1u"},
		{"", "  DCS-7050CX3-32S-R  ", "dcs-7050cx3-32s-r"},
		{"", "Model/Rev.2 (EU)", "model-rev-2-eu"},
		{"", "Ünïcode Box", "n-code-box"},
		{"", "---", ""},
	} {
		if got := plan.ModelKey(tc.slug, tc.model); got != tc.want {
			t.Errorf("ModelKey(%q, %q) = %q, want %q", tc.slug, tc.model, got, tc.want)
		}
	}
}

func TestCatalogRecordsAndDeviceTypeRefs(t *testing.T) {
	p := plan.Build(input(t))
	manufacturer := candidate(t, p, fleet.KindManufacturer, "dcim/manufacturers/50")
	if !equalJSON(spec(manufacturer), map[string]any{"displayName": "Example Vendor", "canonicalName": "Example Vendor"}) {
		t.Errorf("manufacturer %v", spec(manufacturer))
	}
	server := candidate(t, p, fleet.KindDeviceType, "dcim/device-types/500")
	want := map[string]any{
		"displayName":     "Example 1U Server",
		"manufacturerRef": map[string]any{"name": manufacturer.Name},
		"model":           "example-1u",
		"modelShape":      map[string]any{"heightUnits": float64(1), "fullDepth": true},
		"componentTemplates": []any{
			map[string]any{"id": "interface-templates-700", "name": "eth0", "type": "NetworkPort", "connector": "1000base-t"},
			map[string]any{"id": "interface-templates-701", "name": "eth1", "type": "NetworkPort", "connector": "25gbase-x-sfp28"},
			map[string]any{"id": "power-port-templates-710", "name": "PSU1", "type": "PowerInlet", "connector": "iec-60320-c14"},
		},
	}
	if !equalJSON(spec(server), want) {
		t.Errorf("device type %v", spec(server))
	}
	if !equalJSON(field(t, candidate(t, p, fleet.KindDeviceType, "dcim/device-types/503"), "componentTemplates"),
		[]any{map[string]any{"id": "rear-port-templates-720", "name": "rear1", "type": "RearPort", "connector": "mpo", "positions": float64(2)}}) {
		t.Error("rear port template positions")
	}
	if hasCandidate(p, fleet.KindManufacturer, "dcim/manufacturers/51") || hasCandidate(p, fleet.KindDeviceType, "dcim/device-types/502") {
		t.Error("the type of an excluded device was imported")
	}
	for _, source := range []string{"dcim/devices/1000", "dcim/devices/1002"} {
		d := candidate(t, p, fleet.KindDevice, source)
		if field(t, d, "deviceTypeRef", "name") != server.Name {
			t.Errorf("%s deviceTypeRef %v", source, field(t, d, "deviceTypeRef"))
		}
		found := false
		for _, dep := range d.DependsOn {
			found = found || dep == server.Key()
		}
		if !found {
			t.Errorf("%s does not wait for its DeviceType: %v", source, d.DependsOn)
		}
	}
	if field(t, candidate(t, p, fleet.KindDevice, "dcim/devices/1002"), "identity", "unidentified") != true {
		t.Fatal("an unidentified Device carries a deviceTypeRef too")
	}
}

func TestModelKeyCollisionsAreQuarantined(t *testing.T) {
	in := input(t)
	in.Snapshot.DeviceTypes = append(in.Snapshot.DeviceTypes, netbox.DeviceType{ID: 505, Model: "Example 1U Server rev B", Slug: "example_1u", Manufacturer: &netbox.NamedRef{ID: 50, Name: "Example Vendor"}})
	device(in, 1008).DeviceType = &netbox.Ref{ID: 505}
	p := plan.Build(in)
	for _, source := range []string{"dcim/device-types/500", "dcim/device-types/505"} {
		c := candidate(t, p, fleet.KindDeviceType, source)
		if c.Action != plan.ActionQuarantined || c.Reason != plan.ReasonModelKeyCollision ||
			!strings.Contains(c.Message, "dcim/device-types/500") || !strings.Contains(c.Message, "dcim/device-types/505") {
			t.Fatalf("%s %+v", source, c)
		}
	}
	// Devices of a quarantined type are still created, without a reference.
	d := candidate(t, p, fleet.KindDevice, "dcim/devices/1000")
	if d.Action != plan.ActionCreate || field(t, d, "deviceTypeRef") != nil {
		t.Fatalf("device %+v ref %v", d, field(t, d, "deviceTypeRef"))
	}
}

func TestInvalidManufacturerNamesAreUnresolved(t *testing.T) {
	for _, name := range []string{" Leading space", "Trailing space ", "Ünïcode", strings.Repeat("x", 254), ""} {
		in := input(t)
		in.Snapshot.Manufacturers[0].Name = name
		got := outcomes(plan.Build(in))
		if got["record dcim/manufacturers/50"].reason != plan.ReasonCanonicalNameInvalid || got["record dcim/device-types/500"].reason != plan.ReasonDependencyBlocked {
			t.Errorf("%q: %+v %+v", name, got["record dcim/manufacturers/50"], got["record dcim/device-types/500"])
		}
		if got["Device dcim/devices/1000"].action != plan.ActionCreate {
			t.Errorf("%q: the device waits for the catalog: %+v", name, got["Device dcim/devices/1000"])
		}
	}
}

func TestImmutableCatalogChangesAreQuarantined(t *testing.T) {
	// The device type moved to another manufacturer in NetBox. Devices of
	// that type then also change identity, so they are held as well.
	in := input(t)
	in.Snapshot.Manufacturers = append(in.Snapshot.Manufacturers, netbox.Manufacturer{ID: 52, Name: "Example Other Vendor"})
	for i := range in.Snapshot.DeviceTypes {
		if in.Snapshot.DeviceTypes[i].ID == 500 {
			in.Snapshot.DeviceTypes[i].Manufacturer = &netbox.NamedRef{ID: 52}
		}
	}
	c := candidate(t, plan.Build(withExisting(in, previous(t)...)), fleet.KindDeviceType, "dcim/device-types/500")
	if c.Action != plan.ActionQuarantined || c.Reason != plan.ReasonImmutableChanged || !strings.Contains(c.Message, "manufacturerRef immutable") {
		t.Fatalf("%+v", c)
	}

	// A device moved to another device type in NetBox keeps its reference.
	// The new type has the same manufacturer and model, so the Device's own
	// identity is unchanged and only the catalog reference would move.
	in = input(t)
	in.Snapshot.DeviceTypes = append(in.Snapshot.DeviceTypes, netbox.DeviceType{ID: 506, Model: "Example 1U Server", Slug: "example-1u-v2", Manufacturer: &netbox.NamedRef{ID: 50, Name: "Example Vendor"}})
	device(in, 1000).DeviceType = &netbox.Ref{ID: 506}
	p := plan.Build(withExisting(in, previous(t)...))
	d := candidate(t, p, fleet.KindDevice, "dcim/devices/1000")
	original := candidate(t, plan.Build(input(t)), fleet.KindDeviceType, "dcim/device-types/500").Name
	if d.Action != plan.ActionQuarantined || d.Reason != plan.ReasonImmutableChanged || !strings.Contains(d.Message, "never removes or rebinds") ||
		field(t, d, "deviceTypeRef", "name") != original {
		t.Fatalf("%+v ref %v", d, field(t, d, "deviceTypeRef"))
	}
}

func TestOperatorWrittenCatalogRecordsAreNeverAdopted(t *testing.T) {
	// An operator's Manufacturer with the same canonical name, ignoring case.
	operator := object(fleet.KindManufacturer, "operator-vendor", map[string]any{"displayName": "Example Vendor", "canonicalName": "EXAMPLE VENDOR"}, nil)
	p := plan.Build(withExisting(input(t), operator))
	m := candidate(t, p, fleet.KindManufacturer, "dcim/manufacturers/50")
	if m.Action != plan.ActionQuarantined || m.Reason != plan.ReasonCatalogDuplicate || !strings.Contains(m.Message, "operator-vendor") {
		t.Fatalf("manufacturer %+v", m)
	}
	if got := outcomes(p)["DeviceType dcim/device-types/500"]; got.reason != plan.ReasonDependencyBlocked {
		t.Fatalf("device type %+v", got)
	}
	d := candidate(t, p, fleet.KindDevice, "dcim/devices/1000")
	if d.Action != plan.ActionCreate || field(t, d, "deviceTypeRef") != nil {
		t.Fatalf("device %+v", d)
	}

	// An operator's DeviceType for the same manufacturer and model.
	objs := previous(t)
	var manufacturerName string
	for _, o := range objs {
		if o.GetKind() == fleet.KindManufacturer {
			manufacturerName = o.GetName()
		}
	}
	var kept []unstructured.Unstructured
	for _, o := range objs {
		if o.GetKind() != fleet.KindDeviceType {
			kept = append(kept, o)
		}
	}
	operatorType := object(fleet.KindDeviceType, "operator-1u", map[string]any{
		"displayName": "Example 1U", "manufacturerRef": map[string]any{"name": manufacturerName}, "model": "example-1u",
	}, nil)
	c := candidate(t, plan.Build(withExisting(input(t), append(kept, operatorType)...)), fleet.KindDeviceType, "dcim/device-types/500")
	if c.Action != plan.ActionQuarantined || c.Reason != plan.ReasonCatalogDuplicate || !strings.Contains(c.Message, "operator-1u") {
		t.Fatalf("device type %+v", c)
	}
}

func TestDeviceTypeRefsAreAddedLaterAndNeverRemoved(t *testing.T) {
	// Devices applied while their type was not yet in the catalog.
	before := input(t)
	before.Snapshot.Manufacturers = nil
	var objs []unstructured.Unstructured
	for _, c := range plan.Build(before).Candidates {
		objs = append(objs, *applied(c.Object.DeepCopy()))
	}
	d := candidate(t, plan.Build(withExisting(input(t), objs...)), fleet.KindDevice, "dcim/devices/1000")
	if d.Action != plan.ActionUpdate || field(t, d, "deviceTypeRef") == nil {
		t.Fatalf("adding a reference: %+v", d)
	}

	// A Device with a reference keeps it when its type stops being written.
	after := input(t)
	after.Snapshot.Manufacturers = nil
	d = candidate(t, plan.Build(withExisting(after, previous(t)...)), fleet.KindDevice, "dcim/devices/1000")
	if d.Action != plan.ActionUnchanged || field(t, d, "deviceTypeRef") == nil {
		t.Fatalf("keeping a reference: %+v ref %v", d, field(t, d, "deviceTypeRef"))
	}
}
