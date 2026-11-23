// SPDX-License-Identifier: AGPL-3.0-only

package controller

import (
	"fmt"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"

	netboxv1alpha1 "go.miloapis.com/netbox-provider/api/v1alpha1"
	"go.miloapis.com/netbox-provider/internal/fleet"
	"go.miloapis.com/netbox-provider/internal/netbox"
	"go.miloapis.com/netbox-provider/internal/plan"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// otherManager is a writer that owns layout fields on records this controller
// applied, as a hand-maintained catalog would.
const otherManager = "example-layout-writer"

var _ = Describe("fields other managers own on map-list items", func() {
	// applyAsOther server-side applies only the given spec, keyed items
	// included, as otherManager.
	applyAsOther := func(kind, namespace, name string, spec map[string]any) {
		obj := &unstructured.Unstructured{Object: map[string]any{"spec": spec}}
		obj.SetGroupVersionKind(fleet.GVK(kind))
		obj.SetNamespace(namespace)
		obj.SetName(name)
		Expect(k8sClient.Apply(ctx, client.ApplyConfigurationFromUnstructured(obj), client.FieldOwner(otherManager))).To(Succeed(), kind+"/"+name)
	}
	managers := func(obj unstructured.Unstructured) []string {
		var out []string
		for _, entry := range obj.GetManagedFields() {
			out = append(out, entry.Manager)
		}
		return out
	}

	It("are not drift: a reconcile after another manager adds layout fields writes nothing and keeps them", func() {
		h := newHarness().withSourceAndToken(netboxv1alpha1.SourceModeCreate)
		_, err := h.reconcile()
		Expect(err).NotTo(HaveOccurred())

		laidOut := map[string]int{}
		for _, deviceType := range h.list(fleet.KindDeviceType) {
			templates, _, _ := unstructured.NestedSlice(deviceType.Object, "spec", "componentTemplates")
			if len(templates) == 0 {
				continue
			}
			var layout []any
			for i, item := range templates {
				layout = append(layout, map[string]any{
					"id":     item.(map[string]any)["id"],
					"face":   "Front",
					"layout": map[string]any{"bank": "ports", "row": int64(1), "column": int64(i + 1)},
				})
			}
			applyAsOther(fleet.KindDeviceType, h.namespace, deviceType.GetName(), map[string]any{
				"faceplate":          map[string]any{"banks": []any{map[string]any{"id": "ports", "face": "Front", "rows": int64(1), "columns": int64(len(templates))}}},
				"componentTemplates": layout,
			})
			laidOut[deviceType.GetName()] = len(templates)
		}
		tagged := map[string]int{}
		for _, device := range h.list(fleet.KindDevice) {
			components, _, _ := unstructured.NestedSlice(device.Object, "spec", "components")
			if len(components) == 0 {
				continue
			}
			var tags []any
			for i, item := range components {
				tags = append(tags, map[string]any{"id": item.(map[string]any)["id"], "assetTag": fmt.Sprintf("EXAMPLE-TAG-%d", i)})
			}
			applyAsOther(fleet.KindDevice, h.namespace, device.GetName(), map[string]any{"components": tags})
			tagged[device.GetName()] = len(components)
		}
		Expect(laidOut).NotTo(BeEmpty(), "the fixture needs a DeviceType with templates")
		Expect(tagged).NotTo(BeEmpty(), "the fixture needs a Device with components")

		before := len(h.recorder.Applies())
		h.r.Now = func() time.Time { return testNow.Add(time.Hour) }
		_, err = h.reconcile()
		Expect(err).NotTo(HaveOccurred())
		Expect(h.recorder.Applies()[before:]).To(BeEmpty(), "another manager's fields on map-list items are not drift")
		s := h.source()
		Expect(s.Status.Plan.Update).To(BeZero())
		Expect(s.Status.Plan.Unchanged).To(Equal(int32(19)))

		// The other manager's fields and ownership survive, beside ours.
		for _, deviceType := range h.list(fleet.KindDeviceType) {
			count, ok := laidOut[deviceType.GetName()]
			if !ok {
				continue
			}
			Expect(managers(deviceType)).To(ContainElements(plan.FieldManager, otherManager))
			Expect(deviceType.Object["spec"]).To(HaveKey("faceplate"))
			templates, _, _ := unstructured.NestedSlice(deviceType.Object, "spec", "componentTemplates")
			Expect(templates).To(HaveLen(count))
			for _, item := range templates {
				Expect(item).To(HaveKey("layout"))
				Expect(item).To(HaveKey("name"), "the fields this controller applied are kept")
			}
		}
		for _, device := range h.list(fleet.KindDevice) {
			if _, ok := tagged[device.GetName()]; !ok {
				continue
			}
			Expect(managers(device)).To(ContainElements(plan.FieldManager, otherManager))
			components, _, _ := unstructured.NestedSlice(device.Object, "spec", "components")
			for _, item := range components {
				Expect(item).To(HaveKey("assetTag"))
			}
		}
	})

	It("still removes an item NetBox dropped, and keeps an item only another manager owns", func() {
		h := newHarness().withSourceAndToken(netboxv1alpha1.SourceModeCreate)
		_, err := h.reconcile()
		Expect(err).NotTo(HaveOccurred())

		const dropped = "interface-templates-701"
		var name string
		var templates []any
		for _, deviceType := range h.list(fleet.KindDeviceType) {
			items, _, _ := unstructured.NestedSlice(deviceType.Object, "spec", "componentTemplates")
			for _, item := range items {
				if item.(map[string]any)["id"] == dropped {
					name, templates = deviceType.GetName(), items
				}
			}
		}
		Expect(name).NotTo(BeEmpty(), "the fixture needs template "+dropped)
		// Layout on every template but the one NetBox will drop, and a whole
		// template only the other manager owns.
		var layout []any
		for i, item := range templates {
			if id := item.(map[string]any)["id"]; id != dropped {
				layout = append(layout, map[string]any{"id": id, "face": "Front", "layout": map[string]any{"bank": "ports", "row": int64(1), "column": int64(i + 1)}})
			}
		}
		layout = append(layout, map[string]any{"id": "example-extra", "name": "example-extra", "type": "NetworkPort", "face": "Rear"})
		applyAsOther(fleet.KindDeviceType, h.namespace, name, map[string]any{
			"faceplate":          map[string]any{"banks": []any{map[string]any{"id": "ports", "face": "Front", "rows": int64(1), "columns": int64(len(templates))}}},
			"componentTemplates": layout,
		})

		appliedTo := func(from int) []string {
			var out []string
			for _, key := range h.recorder.Applies()[from:] {
				if key == fleet.KindDeviceType+"/"+name {
					out = append(out, key)
				}
			}
			return out
		}
		before := len(h.recorder.Applies())
		h.r.Now = func() time.Time { return testNow.Add(time.Hour) }
		_, err = h.reconcile()
		Expect(err).NotTo(HaveOccurred())
		Expect(appliedTo(before)).To(BeEmpty(), "an item only another manager owns is not drift")

		h.server.Remove(netbox.CollectionInterfaceTemplates, 701)
		before = len(h.recorder.Applies())
		h.r.Now = func() time.Time { return testNow.Add(2 * time.Hour) }
		_, err = h.reconcile()
		Expect(err).NotTo(HaveOccurred())
		Expect(appliedTo(before)).To(HaveLen(1), "a template this controller applied and NetBox dropped is drift")

		var ids []any
		for _, deviceType := range h.list(fleet.KindDeviceType) {
			if deviceType.GetName() != name {
				continue
			}
			items, _, _ := unstructured.NestedSlice(deviceType.Object, "spec", "componentTemplates")
			for _, item := range items {
				m := item.(map[string]any)
				ids = append(ids, m["id"])
				if m["id"] != "example-extra" {
					Expect(m).To(HaveKey("layout"), "the other manager's layout survives the re-apply")
				}
			}
		}
		Expect(ids).NotTo(ContainElement(dropped))
		Expect(ids).To(ContainElement("example-extra"))
	})
})
