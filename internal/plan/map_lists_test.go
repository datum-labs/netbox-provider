// SPDX-License-Identifier: AGPL-3.0-only

package plan_test

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"go.miloapis.com/netbox-provider/internal/fleet"
	"go.miloapis.com/netbox-provider/internal/plan"
)

// ownedListItems records the map-list items of obj in managed-fields form
// (`k:{...}` under each list's `f:` path), as the API server does for the
// manager that applied them. Only map-list items matter to the comparison.
func ownedListItems(obj *unstructured.Unstructured) *metav1.FieldsV1 {
	fields := map[string]any{}
	for path, keys := range fleet.MapLists[obj.GetKind()] {
		segments := strings.Split(path, ".")
		items, _, _ := unstructured.NestedSlice(obj.Object, segments...)
		if len(items) == 0 {
			continue
		}
		node := fields
		for _, segment := range segments {
			next, ok := node["f:"+segment].(map[string]any)
			if !ok {
				next = map[string]any{}
				node["f:"+segment] = next
			}
			node = next
		}
		for _, item := range items {
			key := map[string]any{}
			for _, k := range keys {
				key[k] = item.(map[string]any)[k]
			}
			data, _ := json.Marshal(key)
			node["k:"+string(data)] = map[string]any{".": map[string]any{}}
		}
	}
	data, _ := json.Marshal(fields)
	return metav1.NewFieldsV1(string(data))
}

// listItems returns the items of a map-list on obj, for editing in place.
func listItems(t *testing.T, obj *unstructured.Unstructured, path ...string) []any {
	t.Helper()
	items, _, err := unstructured.NestedSlice(obj.Object, path...)
	if err != nil || len(items) == 0 {
		t.Fatalf("%s %s has no %v", obj.GetKind(), obj.GetName(), path)
	}
	return items
}

// otherWriter records another manager's apply on obj, owning nothing this
// controller compares.
func otherWriter(obj *unstructured.Unstructured) {
	obj.SetManagedFields(append(obj.GetManagedFields(), metav1.ManagedFieldsEntry{
		Manager: "example-layout-writer", Operation: metav1.ManagedFieldsOperationApply, APIVersion: "fleet.datumapis.com/v1alpha1",
		FieldsV1: metav1.NewFieldsV1(`{}`),
	}))
}

// addOtherWritersFields gives every template on a DeviceType the fields a
// layout writer adds, and every component on a Device an asset tag, as another
// manager would. It is false for a record with no such items.
func addOtherWritersFields(t *testing.T, obj *unstructured.Unstructured) bool {
	t.Helper()
	path := map[string][]string{fleet.KindDeviceType: {"spec", "componentTemplates"}, fleet.KindDevice: {"spec", "components"}}[obj.GetKind()]
	if items, _, _ := unstructured.NestedSlice(obj.Object, path...); path == nil || len(items) == 0 {
		return false
	}
	switch obj.GetKind() {
	case fleet.KindDeviceType:
		templates := listItems(t, obj, "spec", "componentTemplates")
		for i, item := range templates {
			template := item.(map[string]any)
			template["face"] = "Front"
			template["layout"] = map[string]any{"bank": "ports", "row": int64(1), "column": int64(i + 1)}
		}
		_ = unstructured.SetNestedSlice(obj.Object, templates, "spec", "componentTemplates")
		_ = unstructured.SetNestedField(obj.Object, map[string]any{"banks": []any{map[string]any{"id": "ports", "face": "Front", "rows": int64(1), "columns": int64(len(templates))}}}, "spec", "faceplate")
	case fleet.KindDevice:
		components := listItems(t, obj, "spec", "components")
		for i, item := range components {
			item.(map[string]any)["assetTag"] = "EXAMPLE-TAG-" + string(rune('A'+i))
		}
		_ = unstructured.SetNestedSlice(obj.Object, components, "spec", "components")
	}
	otherWriter(obj)
	return true
}

// withOtherWriters returns an earlier run's records after another manager
// added fields to every DeviceType template and Device component.
func withOtherWriters(t *testing.T) ([]unstructured.Unstructured, map[string]bool) {
	t.Helper()
	objs := previous(t)
	touched := map[string]bool{}
	for i := range objs {
		if addOtherWritersFields(t, &objs[i]) {
			touched[objs[i].GetKind()] = true
		}
	}
	if !touched[fleet.KindDeviceType] || !touched[fleet.KindDevice] {
		t.Fatalf("the fixture needs a DeviceType with templates and a Device with components: %v", touched)
	}
	return objs, touched
}

func TestOtherWritersFieldsOnMapListItemsAreUnchanged(t *testing.T) {
	objs, _ := withOtherWriters(t)
	p := plan.Build(withExisting(input(t), objs...))
	if p.Count(plan.ActionUnchanged) != len(objs) || p.Count(plan.ActionUpdate) != 0 {
		t.Fatalf("another writer's fields on template and component items were treated as drift: %v", outcomes(p))
	}
}

// changeFirstItem changes a field this controller owns on the first item of a
// map-list.
func changeFirstItem(t *testing.T, obj *unstructured.Unstructured, path ...string) {
	t.Helper()
	items := listItems(t, obj, path...)
	items[0].(map[string]any)["name"] = "changed-elsewhere"
	_ = unstructured.SetNestedSlice(obj.Object, items, path...)
}

func TestOwnedFieldOnAMapListItemStillUpdatesBesideOtherWritersFields(t *testing.T) {
	for _, tc := range []struct {
		kind string
		path []string
	}{
		{fleet.KindDeviceType, []string{"spec", "componentTemplates"}},
		{fleet.KindDevice, []string{"spec", "components"}},
	} {
		t.Run(tc.kind, func(t *testing.T) {
			objs, _ := withOtherWriters(t)
			changed := ""
			for i := range objs {
				if objs[i].GetKind() == tc.kind && changed == "" && hasItems(&objs[i], tc.path...) {
					changeFirstItem(t, &objs[i], tc.path...)
					changed = objs[i].GetName()
				}
			}
			p := plan.Build(withExisting(input(t), objs...))
			for _, c := range p.Candidates {
				want := plan.ActionUnchanged
				if c.Kind == tc.kind && c.Name == changed {
					want = plan.ActionUpdate
				}
				if c.Action != want {
					t.Errorf("%s: got %s, want %s", c.Key(), c.Action, want)
				}
			}
		})
	}
}

// addItem appends an item to a map-list. When ours, this controller's managed
// fields record it, as after a Device's interim apply: the applied digest is
// already the plan's, but the item is still this controller's. Otherwise
// another manager added it.
func addItem(t *testing.T, obj *unstructured.Unstructured, ours bool, path ...string) {
	t.Helper()
	items := listItems(t, obj, path...)
	extra := map[string]any{"id": "example-removed", "name": "example-removed", "type": "NetworkPort"}
	_ = unstructured.SetNestedSlice(obj.Object, append(items, extra), path...)
	if !ours {
		return
	}
	entries := obj.GetManagedFields()
	for i := range entries {
		if entries[i].Manager == plan.FieldManager {
			entries[i].FieldsV1 = ownedListItems(obj)
		}
	}
	obj.SetManagedFields(entries)
}

func TestUnwantedItemThisControllerOwnsUpdatesButAnotherWritersItemDoesNot(t *testing.T) {
	for _, tc := range []struct {
		kind string
		path []string
	}{
		{fleet.KindDeviceType, []string{"spec", "componentTemplates"}},
		{fleet.KindDevice, []string{"spec", "components"}},
	} {
		for _, ours := range []bool{true, false} {
			t.Run(fmt.Sprintf("%s/ours=%v", tc.kind, ours), func(t *testing.T) {
				objs, _ := withOtherWriters(t)
				extended := ""
				for i := range objs {
					if objs[i].GetKind() == tc.kind && extended == "" && hasItems(&objs[i], tc.path...) {
						addItem(t, &objs[i], ours, tc.path...)
						extended = objs[i].GetName()
					}
				}
				p := plan.Build(withExisting(input(t), objs...))
				for _, c := range p.Candidates {
					want := plan.ActionUnchanged
					if ours && c.Kind == tc.kind && c.Name == extended {
						// This controller applied the item and the plan no
						// longer has it: applying without it removes it.
						want = plan.ActionUpdate
					}
					if c.Action != want {
						t.Errorf("%s: got %s, want %s", c.Key(), c.Action, want)
					}
				}
			})
		}
	}
}

func TestExtraItemsCountAsDriftWhenOwnershipIsUnknown(t *testing.T) {
	objs, _ := withOtherWriters(t)
	extended := ""
	for i := range objs {
		if objs[i].GetKind() != fleet.KindDeviceType || extended != "" || !hasItems(&objs[i], "spec", "componentTemplates") {
			continue
		}
		addItem(t, &objs[i], false, "spec", "componentTemplates")
		// Managed fields without a fields set: which items are ours is unknown.
		entries := objs[i].GetManagedFields()
		for j := range entries {
			if entries[j].Manager == plan.FieldManager {
				entries[j].FieldsV1 = nil
			}
		}
		objs[i].SetManagedFields(entries)
		extended = objs[i].GetName()
	}
	p := plan.Build(withExisting(input(t), objs...))
	for _, c := range p.Candidates {
		want := plan.ActionUnchanged
		if c.Kind == fleet.KindDeviceType && c.Name == extended {
			want = plan.ActionUpdate
		}
		if c.Action != want {
			t.Errorf("%s: got %s, want %s", c.Key(), c.Action, want)
		}
	}
}

func TestAtomicListsStayStrict(t *testing.T) {
	objs := previous(t)
	changed := ""
	for i := range objs {
		mappings, _, _ := unstructured.NestedSlice(objs[i].Object, "spec", "internalMappings")
		if objs[i].GetKind() != fleet.KindDevice || len(mappings) == 0 || changed != "" {
			continue
		}
		// An atomic list compares as a whole: an extra entry is a difference.
		_ = unstructured.SetNestedSlice(objs[i].Object, append(mappings, mappings[0]), "spec", "internalMappings")
		changed = objs[i].GetName()
	}
	if changed == "" {
		t.Fatal("the fixture needs a Device with internal mappings")
	}
	p := plan.Build(withExisting(input(t), objs...))
	if c := candidate(t, p, fleet.KindDevice, sourceOf(t, p, fleet.KindDevice, changed)); c.Action != plan.ActionUpdate {
		t.Fatalf("an atomic list must compare whole: %+v", c)
	}
}

func hasItems(obj *unstructured.Unstructured, path ...string) bool {
	items, _, _ := unstructured.NestedSlice(obj.Object, path...)
	return len(items) > 0
}

func sourceOf(t *testing.T, p *plan.Plan, kind, name string) string {
	t.Helper()
	for _, c := range p.Candidates {
		if c.Kind == kind && c.Name == name {
			return c.Source
		}
	}
	t.Fatalf("no %s %s", kind, name)
	return ""
}
