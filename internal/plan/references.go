// SPDX-License-Identifier: AGPL-3.0-only

package plan

import (
	"sort"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"go.miloapis.com/netbox-provider/internal/fleet"
)

// CableReference is a Cable endpoint that names a component its Device does
// not have.
type CableReference struct {
	// Cable and Device are Fleet record names.
	Cable  string
	Device string
	// ComponentID is the component the Cable names.
	ComponentID string
}

// UndeliveredReferences returns each Cable endpoint that names a component
// its Device lacks once p's writes land, for the Devices p plans. written
// reports whether a candidate's write landed, or would land in Observe mode.
// A Device whose key interim holds has its interim components, since its
// interim landed even when its planned apply then failed. Any other written
// Device has its planned components, and the rest keep their components in
// existing.
// A written Cable has its planned lanes, and any other Cable keeps its lanes
// in existing. Devices this plan does not have as candidates are not checked.
// The result is sorted by Cable, Device and component.
func UndeliveredReferences(p *Plan, existing Existing, written func(*Candidate) bool, interim map[string]bool) []CableReference {
	devices := map[string]*unstructured.Unstructured{}
	for i := range existing[fleet.KindDevice] {
		devices[existing[fleet.KindDevice][i].GetName()] = &existing[fleet.KindDevice][i]
	}
	cables := map[string]*unstructured.Unstructured{}
	for i := range existing[fleet.KindCable] {
		cables[existing[fleet.KindCable][i].GetName()] = &existing[fleet.KindCable][i]
	}
	components := map[string]map[string]bool{}
	for _, c := range p.Candidates {
		switch c.Kind {
		case fleet.KindDevice:
			obj := devices[c.Name]
			switch {
			case interim[c.Key()] && c.Interim != nil:
				// The interim landed, whether or not the planned apply did.
				obj = c.Interim
			case written(c):
				obj = c.Object
			}
			if obj == nil {
				continue
			}
			ids := map[string]bool{}
			comps, _, _ := unstructured.NestedSlice(obj.Object, "spec", "components")
			for _, comp := range comps {
				id, _ := asMap(comp)["id"].(string)
				ids[id] = true
			}
			components[c.Name] = ids
		case fleet.KindCable:
			if written(c) {
				cables[c.Name] = c.Object
			}
		}
	}
	var out []CableReference
	for name, cable := range cables {
		lanes, _, _ := unstructured.NestedSlice(cable.Object, "spec", "lanes")
		for _, lane := range lanes {
			for _, side := range []string{"a", "b"} {
				device, _, _ := unstructured.NestedString(asMap(lane), side, "deviceRef", "name")
				id, _, _ := unstructured.NestedString(asMap(lane), side, "componentID")
				if ids, ok := components[device]; ok && !ids[id] {
					out = append(out, CableReference{Cable: name, Device: device, ComponentID: id})
				}
			}
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Cable != out[j].Cable {
			return out[i].Cable < out[j].Cable
		}
		if out[i].Device != out[j].Device {
			return out[i].Device < out[j].Device
		}
		return out[i].ComponentID < out[j].ComponentID
	})
	return out
}
