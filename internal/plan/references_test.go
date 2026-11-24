// SPDX-License-Identifier: AGPL-3.0-only

package plan_test

import (
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"go.miloapis.com/netbox-provider/internal/fleet"
	"go.miloapis.com/netbox-provider/internal/plan"
)

// landed reports every planned write as landed, except those of blocked.
func landed(blocked ...string) func(*plan.Candidate) bool {
	return func(c *plan.Candidate) bool {
		for _, key := range blocked {
			if c.Key() == key {
				return false
			}
		}
		return c.Action == plan.ActionCreate || c.Action == plan.ActionUpdate || c.Action == plan.ActionUnchanged
	}
}

func TestUndeliveredReferences(t *testing.T) {
	device := plan.Name(key(plan.TypeDevice, "1000"), fleet.KindDevice)
	cable := plan.Name(key(plan.TypeCable, "5000"), fleet.KindCable)
	power := plan.Name(key(plan.TypeCable, "5001"), fleet.KindCable)
	existing := instanceIDRecords(t)
	moved := func(in plan.Input) plan.Input { return withExisting(in, existing...) }

	// Nothing applied yet, and everything applied: no Cable names a missing
	// component.
	in := input(t)
	if refs := plan.UndeliveredReferences(plan.Build(in), in.Existing, landed(), nil); len(refs) != 0 {
		t.Fatalf("clean plan %v", refs)
	}
	in = moved(input(t))
	p := plan.Build(in)
	if refs := plan.UndeliveredReferences(p, in.Existing, landed(), nil); len(refs) != 0 {
		t.Fatalf("staged move %v", refs)
	}

	// A Cable rewrite that does not land leaves the Device interim: it keeps
	// the old component, so the Cable still names one it has.
	server := candidate(t, p, fleet.KindDevice, "dcim/devices/1000")
	held := map[string]bool{server.Key(): true}
	if refs := plan.UndeliveredReferences(p, in.Existing, landed(fleet.KindCable+"/"+cable), held); len(refs) != 0 {
		t.Fatalf("interim Device %v", refs)
	}
	// Without the interim, the Cable names a component the Device dropped.
	want := []plan.CableReference{{Cable: cable, Device: device, ComponentID: "interfaces-2001"}}
	if refs := plan.UndeliveredReferences(p, in.Existing, landed(fleet.KindCable+"/"+cable), nil); !equalJSON(refs, want) {
		t.Fatalf("Device applied without its interim: %v, want %v", refs, want)
	}

	// A Cable this plan does not write still names the old component once
	// the Device drops it: without reviewed media, neither data Cable is
	// written. The power cable moves with its Device.
	unwritten := []plan.CableReference{
		{Cable: plan.Name(key(plan.TypeCable, "5008"), fleet.KindCable), Device: plan.Name(key(plan.TypeDevice, "1002"), fleet.KindDevice), ComponentID: "interfaces-2020"},
		want[0],
	}
	in = moved(input(t))
	in.Mapping.CableMedia = nil
	p = plan.Build(in)
	if hasCandidate(p, fleet.KindCable, "dcim/cables/5000") {
		t.Fatalf("cable 5000 is planned: %v", outcomes(p))
	}
	if c := candidate(t, p, fleet.KindCable, "dcim/cables/5001"); c.Action != plan.ActionUpdate || c.Name != power {
		t.Fatalf("power cable %s %s", c.Action, c.Name)
	}
	if refs := plan.UndeliveredReferences(p, in.Existing, landed(), nil); !equalJSON(refs, unwritten) {
		t.Fatalf("unwritten Cables %v, want %v", refs, unwritten)
	}
	// They are still reported once the Devices are applied and unchanged.
	in = moved(input(t))
	in.Mapping.CableMedia = nil
	for _, c := range plan.Build(in).Candidates {
		if c.Kind == fleet.KindDevice && c.Action == plan.ActionUpdate {
			in.Existing[fleet.KindDevice] = replace(in.Existing[fleet.KindDevice], *applied(c.Object.DeepCopy()))
		}
	}
	p = plan.Build(in)
	if c := candidate(t, p, fleet.KindDevice, "dcim/devices/1000"); c.Action != plan.ActionUnchanged {
		t.Fatalf("server %s", c.Action)
	}
	written := plan.UndeliveredReferences(p, in.Existing, landed(), nil)
	if !equalJSON(written, unwritten) {
		t.Fatalf("after the Devices are applied %v, want %v", written, unwritten)
	}
}

// replace swaps the record with obj's name for obj.
func replace(objs []unstructured.Unstructured, obj unstructured.Unstructured) []unstructured.Unstructured {
	out := append([]unstructured.Unstructured{}, objs...)
	for i := range out {
		if out[i].GetName() == obj.GetName() {
			out[i] = obj
		}
	}
	return out
}
