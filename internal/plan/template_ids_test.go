// SPDX-License-Identifier: AGPL-3.0-only

package plan_test

import (
	"strings"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"go.miloapis.com/netbox-provider/internal/fleet"
	"go.miloapis.com/netbox-provider/internal/netbox"
	"go.miloapis.com/netbox-provider/internal/plan"
)

func addTemplate(in plan.Input, collection string, t netbox.ComponentTemplate) {
	in.Snapshot.Templates[collection] = append(in.Snapshot.Templates[collection], t)
}

// componentIDs returns a Device candidate's component IDs by NetBox name.
func componentIDs(t *testing.T, p *plan.Plan, source string) map[string][]string {
	t.Helper()
	out := map[string][]string{}
	for _, c := range field(t, candidate(t, p, fleet.KindDevice, source), "components").([]any) {
		m := c.(map[string]any)
		out[m["name"].(string)] = append(out[m["name"].(string)], m["id"].(string))
	}
	return out
}

func laneEnd(t *testing.T, p *plan.Plan, source, side string) string {
	t.Helper()
	lane := field(t, candidate(t, p, fleet.KindCable, source), "lanes").([]any)[0].(map[string]any)
	return lane[side].(map[string]any)["componentID"].(string)
}

func TestComponentTakesTheTemplateIDOfItsNameAndType(t *testing.T) {
	in := input(t)
	// A template of another type with the same name is not a match.
	addTemplate(in, netbox.CollectionConsolePortTemplates, netbox.ComponentTemplate{ID: 730, DeviceType: &netbox.Ref{ID: 500}, Name: "eth1", Type: &netbox.Choice{Value: "rj-45"}})
	ids := componentIDs(t, plan.Build(in), "dcim/devices/1000")
	want := map[string][]string{
		"eth0": {"interface-templates-700"},
		"eth1": {"interface-templates-701"},
		"PSU1": {"power-port-templates-710"},
	}
	if !equalJSON(ids, want) {
		t.Fatalf("components %v, want %v", ids, want)
	}
	// Each device of the type takes the same template IDs.
	if ids := componentIDs(t, plan.Build(input(t)), "dcim/devices/1002"); ids["eth0"][0] != "interface-templates-700" {
		t.Fatalf("second device of the type %v", ids)
	}
}

func TestComponentWithoutATemplateKeepsItsInstanceID(t *testing.T) {
	in := input(t)
	// A component renamed in NetBox, one a module installed, and one added by
	// hand have no template of their own.
	for i := range in.Snapshot.Components[netbox.CollectionInterfaces] {
		if c := &in.Snapshot.Components[netbox.CollectionInterfaces][i]; c.ID == 2001 {
			c.Name = "uplink"
		}
	}
	addComponent(in, netbox.CollectionInterfaces, netbox.Component{ID: 2030, Device: &netbox.Ref{ID: 1000}, Name: "eth2", Type: &netbox.Choice{Value: "10gbase-x-sfpp"}})
	addTemplate(in, netbox.CollectionInterfaceTemplates, netbox.ComponentTemplate{ID: 703, DeviceType: &netbox.Ref{ID: 500}, Name: "eth3", Type: &netbox.Choice{Value: "10gbase-x-sfpp"}})
	addComponent(in, netbox.CollectionInterfaces, netbox.Component{ID: 2031, Device: &netbox.Ref{ID: 1000}, Name: "eth3", Type: &netbox.Choice{Value: "10gbase-x-sfpp"}, Module: &netbox.Ref{ID: 40}})
	p := plan.Build(in)
	ids := componentIDs(t, p, "dcim/devices/1000")
	for name, id := range map[string]string{"uplink": "interfaces-2001", "eth2": "interfaces-2030", "eth3": "interfaces-2031", "eth0": "interface-templates-700"} {
		if len(ids[name]) != 1 || ids[name][0] != id {
			t.Errorf("%s has IDs %v, want %s", name, ids[name], id)
		}
	}
	// The front port has no template either.
	if ids := componentIDs(t, p, "dcim/devices/1007"); ids["front1"][0] != "front-ports-2101" || ids["rear1"][0] != "rear-port-templates-720" {
		t.Errorf("switch components %v", ids)
	}
}

func TestAmbiguousTemplateKeepsTheInstanceID(t *testing.T) {
	in := input(t)
	// Two templates of one type with the same name and component type.
	addTemplate(in, netbox.CollectionInterfaceTemplates, netbox.ComponentTemplate{ID: 704, DeviceType: &netbox.Ref{ID: 500}, Name: "eth0", Type: &netbox.Choice{Value: "1000base-t"}})
	p := plan.Build(in)
	ids := componentIDs(t, p, "dcim/devices/1000")
	if ids["eth0"][0] != "interfaces-2000" || ids["eth1"][0] != "interface-templates-701" {
		t.Fatalf("components %v", ids)
	}
	// The DeviceType still records both templates under their own IDs.
	templates := field(t, candidate(t, p, fleet.KindDeviceType, "dcim/device-types/500"), "componentTemplates").([]any)
	var templateIDs []string
	for _, tmpl := range templates {
		templateIDs = append(templateIDs, tmpl.(map[string]any)["id"].(string))
	}
	if !equalJSON(templateIDs, []string{"interface-templates-700", "interface-templates-701", "interface-templates-704", "power-port-templates-710"}) {
		t.Fatalf("templates %v", templateIDs)
	}
}

func TestComponentIDsStayUniqueWithinADevice(t *testing.T) {
	in := input(t)
	// NetBox names inventory items uniquely only under one parent, so two
	// items can share the name of one template. Neither takes it.
	addTemplate(in, netbox.CollectionInventoryItemTemplates, netbox.ComponentTemplate{ID: 740, DeviceType: &netbox.Ref{ID: 500}, Name: "Disk"})
	addTemplate(in, netbox.CollectionInventoryItemTemplates, netbox.ComponentTemplate{ID: 741, DeviceType: &netbox.Ref{ID: 500}, Name: "Fan"})
	addComponent(in, netbox.CollectionInventoryItems, netbox.Component{ID: 4100, Device: &netbox.Ref{ID: 1000}, Name: "Disk"})
	addComponent(in, netbox.CollectionInventoryItems, netbox.Component{ID: 4101, Device: &netbox.Ref{ID: 1000}, Name: "Disk"})
	addComponent(in, netbox.CollectionInventoryItems, netbox.Component{ID: 4102, Device: &netbox.Ref{ID: 1000}, Name: "Fan"})
	p := plan.Build(in)
	ids := componentIDs(t, p, "dcim/devices/1000")
	if !equalJSON(ids["Disk"], []string{"inventory-items-4100", "inventory-items-4101"}) || !equalJSON(ids["Fan"], []string{"inventory-item-templates-741"}) {
		t.Fatalf("components %v", ids)
	}
	if got := outcomes(p)["Device dcim/devices/1000"]; got.action != plan.ActionCreate {
		t.Fatalf("device %+v", got)
	}
	for _, c := range p.Candidates {
		if c.Kind != fleet.KindDevice {
			continue
		}
		seen := map[string]bool{}
		for _, comp := range field(t, c, "components").([]any) {
			id := comp.(map[string]any)["id"].(string)
			if seen[id] {
				t.Errorf("%s has component ID %s twice", c.Source, id)
			}
			seen[id] = true
		}
	}
}

func TestCableEndpointsUseTheFinalComponentIDs(t *testing.T) {
	p := plan.Build(input(t))
	for source, want := range map[string]string{
		"dcim/cables/5000": "interface-templates-701",
		"dcim/cables/5001": "power-port-templates-710",
	} {
		if got := laneEnd(t, p, source, "a"); got != want {
			t.Errorf("%s A side names %s, want %s", source, got, want)
		}
	}
	// A component without a template keeps its instance ID on the far side,
	// and a feed endpoint is unchanged.
	if got := laneEnd(t, p, "dcim/cables/5000", "b"); got != "interfaces-2010" {
		t.Errorf("cable 5000 B side %s", got)
	}
	if got := laneEnd(t, p, "dcim/cables/5002", "b"); got != "output" {
		t.Errorf("feed endpoint %s", got)
	}
}

func TestInternalMappingsUseTheFinalComponentIDs(t *testing.T) {
	in := input(t)
	addTemplate(in, netbox.CollectionPowerPortTemplates, netbox.ComponentTemplate{ID: 750, DeviceType: &netbox.Ref{ID: 504}, Name: "Inlet", Type: &netbox.Choice{Value: "iec-60320-c20"}})
	addTemplate(in, netbox.CollectionPowerOutletTemplates, netbox.ComponentTemplate{ID: 751, DeviceType: &netbox.Ref{ID: 504}, Name: "Outlet 1", Type: &netbox.Choice{Value: "iec-60320-c13"}})
	p := plan.Build(in)
	pdu := candidate(t, p, fleet.KindDevice, "dcim/devices/1008")
	want := []any{
		map[string]any{"from": map[string]any{"componentID": "power-outlet-templates-751", "position": float64(1)}, "to": map[string]any{"componentID": "power-port-templates-750", "position": float64(1)}},
		map[string]any{"from": map[string]any{"componentID": "power-outlets-3101", "position": float64(1)}, "to": map[string]any{"componentID": "power-port-templates-750", "position": float64(1)}},
	}
	if !equalJSON(field(t, pdu, "internalMappings"), want) {
		t.Fatalf("pdu internal mappings %v", field(t, pdu, "internalMappings"))
	}
	if got := laneEnd(t, p, "dcim/cables/5001", "b"); got != "power-outlet-templates-751" {
		t.Errorf("outlet endpoint %s", got)
	}
}

// instanceIDRecords returns what an earlier version applied, when every
// component had its instance ID.
func instanceIDRecords(t *testing.T) []unstructured.Unstructured {
	in := input(t)
	in.Snapshot.ObservedAt = observedAt.Add(-24 * time.Hour)
	in.Mapping.Revision = "example-reviewed-0"
	for collection := range in.Snapshot.Templates {
		for i := range in.Snapshot.Templates[collection] {
			in.Snapshot.Templates[collection][i].Name += " (renamed)"
		}
	}
	var out []unstructured.Unstructured
	for _, c := range plan.Build(in).Candidates {
		out = append(out, *applied(c.Object.DeepCopy()))
	}
	return out
}

func TestRenamedComponentsAreKeptUntilTheirCablesMove(t *testing.T) {
	p := plan.Build(withExisting(input(t), instanceIDRecords(t)...))
	server := candidate(t, p, fleet.KindDevice, "dcim/devices/1000")
	if server.Action != plan.ActionUpdate || server.Interim == nil {
		t.Fatalf("server %s/%s interim %v", server.Action, server.Reason, server.Interim != nil)
	}
	ids := func(obj *unstructured.Unstructured) []string {
		comps, _, _ := unstructured.NestedSlice(obj.Object, "spec", "components")
		var out []string
		for _, c := range comps {
			out = append(out, c.(map[string]any)["id"].(string))
		}
		return out
	}
	if got := ids(server.Object); !equalJSON(got, []string{"interface-templates-700", "interface-templates-701", "power-port-templates-710"}) {
		t.Errorf("planned components %v", got)
	}
	// The interim adds only the old IDs existing Cables name: eth0 has no
	// imported cable, so its old ID is dropped at once.
	if got := ids(server.Interim); !equalJSON(got, []string{"interface-templates-700", "interface-templates-701", "power-port-templates-710", "interfaces-2001", "power-ports-3000"}) {
		t.Errorf("interim components %v", got)
	}
	var holders []string
	for _, source := range []string{"dcim/cables/5000", "dcim/cables/5001"} {
		c := candidate(t, p, fleet.KindCable, source)
		if c.Action != plan.ActionUpdate {
			t.Errorf("%s %s", source, c.Action)
		}
		holders = append(holders, c.Key())
	}
	for _, h := range holders {
		if !strings.Contains(strings.Join(server.Holders, " "), h) || len(server.Holders) != len(holders) {
			t.Errorf("holders %v, want %v", server.Holders, holders)
		}
	}
	// A Device whose old IDs no Cable names is applied in one step.
	sw := candidate(t, p, fleet.KindDevice, "dcim/devices/1007")
	if sw.Action != plan.ActionUpdate || sw.Interim != nil {
		t.Errorf("switch %s interim %v", sw.Action, sw.Interim != nil)
	}

	// Once applied, the next plan writes nothing.
	var written []unstructured.Unstructured
	for _, c := range p.Candidates {
		written = append(written, *applied(c.Object.DeepCopy()))
	}
	in := input(t)
	in.Existing = map[string][]unstructured.Unstructured{fleet.KindFacility: {facility("example-premises-a")}}
	next := plan.Build(withExisting(in, written...))
	if next.Count(plan.ActionCreate)+next.Count(plan.ActionUpdate) != 0 {
		t.Fatalf("second plan writes %d creates and %d updates", next.Count(plan.ActionCreate), next.Count(plan.ActionUpdate))
	}
}

func TestCablesThePlanDoesNotWriteAreReported(t *testing.T) {
	existing := instanceIDRecords(t)
	// The cable is left out of the next plan: its medium is no longer reviewed.
	in := input(t)
	in.Mapping.CableMedia = nil
	p := plan.Build(withExisting(in, existing...))
	server := candidate(t, p, fleet.KindDevice, "dcim/devices/1000")
	cable := candidate(t, plan.Build(withExisting(input(t), existing...)), fleet.KindCable, "dcim/cables/5000")
	if server.Action != plan.ActionUpdate || !strings.Contains(server.Message, cable.Key()) {
		t.Fatalf("server %s message %q", server.Action, server.Message)
	}
	for _, h := range server.Holders {
		if h == cable.Key() {
			t.Fatalf("an unwritten cable is waited for: %v", server.Holders)
		}
	}
}
