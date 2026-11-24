// SPDX-License-Identifier: AGPL-3.0-only

package controller

import (
	"sync"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"

	netboxv1alpha1 "go.miloapis.com/netbox-provider/api/v1alpha1"
	"go.miloapis.com/netbox-provider/internal/fleet"
	"go.miloapis.com/netbox-provider/internal/netbox"
	"go.miloapis.com/netbox-provider/internal/plan"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("template component IDs", func() {
	deviceName := plan.Name(plan.SourceKey{Source: testSystem, Type: plan.TypeDevice, ID: "1000"}, fleet.KindDevice)
	cableName := plan.Name(plan.SourceKey{Source: testSystem, Type: plan.TypeCable, ID: "5000"}, fleet.KindCable)

	// templates are the fixture's component templates that match components.
	templates := []struct {
		collection string
		id         int
		name       string
	}{
		{netbox.CollectionInterfaceTemplates, 700, "eth0"},
		{netbox.CollectionInterfaceTemplates, 701, "eth1"},
		{netbox.CollectionPowerPortTemplates, 710, "PSU1"},
		{netbox.CollectionRearPortTemplates, 720, "rear1"},
	}

	// dangling returns each Cable endpoint that names a component its Device
	// does not have.
	dangling := func(namespace string) []string {
		components := map[string]bool{}
		devices := &unstructured.UnstructuredList{}
		devices.SetGroupVersionKind(fleet.ListGVK(fleet.KindDevice))
		Expect(k8sClient.List(ctx, devices, client.InNamespace(namespace))).To(Succeed())
		for _, d := range devices.Items {
			comps, _, _ := unstructured.NestedSlice(d.Object, "spec", "components")
			for _, c := range comps {
				components[d.GetName()+"/"+c.(map[string]any)["id"].(string)] = true
			}
		}
		cables := &unstructured.UnstructuredList{}
		cables.SetGroupVersionKind(fleet.ListGVK(fleet.KindCable))
		Expect(k8sClient.List(ctx, cables, client.InNamespace(namespace))).To(Succeed())
		var out []string
		for _, cable := range cables.Items {
			lanes, _, _ := unstructured.NestedSlice(cable.Object, "spec", "lanes")
			for _, lane := range lanes {
				for _, side := range []string{"a", "b"} {
					device, _, _ := unstructured.NestedString(lane.(map[string]any), side, "deviceRef", "name")
					id, _, _ := unstructured.NestedString(lane.(map[string]any), side, "componentID")
					if device != "" && !components[device+"/"+id] {
						out = append(out, cable.GetName()+" -> "+device+"/"+id)
					}
				}
			}
		}
		return out
	}
	get := func(namespace, kind, name string) *unstructured.Unstructured {
		obj := &unstructured.Unstructured{}
		obj.SetGroupVersionKind(fleet.GVK(kind))
		Expect(k8sClient.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, obj)).To(Succeed())
		return obj
	}
	componentIDs := func(obj *unstructured.Unstructured) []string {
		comps, _, _ := unstructured.NestedSlice(obj.Object, "spec", "components")
		var out []string
		for _, c := range comps {
			out = append(out, c.(map[string]any)["id"].(string))
		}
		return out
	}

	It("moves an owned Device and its Cables to template IDs without a Cable naming a missing component", func() {
		h := newHarness().withSourceAndToken(netboxv1alpha1.SourceModeCreate)
		// An earlier version gave every component its instance ID. Renaming
		// the templates reproduces those records.
		for _, t := range templates {
			h.server.Set(t.collection, t.id, "name", t.name+" (renamed)")
		}
		_, err := h.reconcile()
		Expect(err).NotTo(HaveOccurred())
		Expect(componentIDs(get(h.namespace, fleet.KindDevice, deviceName))).To(Equal([]string{"interfaces-2000", "interfaces-2001", "power-ports-3000"}))
		Expect(dangling(h.namespace)).To(BeEmpty())

		for _, t := range templates {
			h.server.Set(t.collection, t.id, "name", t.name)
		}
		// Check every state Fleet passes through: before each apply, and
		// after the last one.
		var mu sync.Mutex
		var seen []string
		h.recorder.beforeApply = func(kind, name string) {
			mu.Lock()
			defer mu.Unlock()
			seen = append(seen, dangling(h.namespace)...)
		}
		before := len(h.recorder.Applies())
		h.r.Now = func() time.Time { return testNow.Add(time.Hour) }
		_, err = h.reconcile()
		Expect(err).NotTo(HaveOccurred())
		h.recorder.beforeApply = nil
		Expect(seen).To(BeEmpty(), "a Cable named a component its Device did not have")
		Expect(dangling(h.namespace)).To(BeEmpty())

		// The Device is applied with its interim, the Cables move, and then
		// the planned Device drops the old IDs.
		applies := h.recorder.Applies()[before:]
		index := func(key string, from int) int {
			for i := from; i < len(applies); i++ {
				if applies[i] == key {
					return i
				}
			}
			return -1
		}
		first := index(fleet.KindDevice+"/"+deviceName, 0)
		cable := index(fleet.KindCable+"/"+cableName, 0)
		second := index(fleet.KindDevice+"/"+deviceName, first+1)
		Expect(first).To(BeNumerically(">=", 0), "%v", applies)
		Expect(cable).To(BeNumerically(">", first), "%v", applies)
		Expect(second).To(BeNumerically(">", cable), "%v", applies)

		device := get(h.namespace, fleet.KindDevice, deviceName)
		Expect(componentIDs(device)).To(Equal([]string{"interface-templates-700", "interface-templates-701", "power-port-templates-710"}))
		lanes, _, _ := unstructured.NestedSlice(get(h.namespace, fleet.KindCable, cableName).Object, "spec", "lanes")
		Expect(lanes[0].(map[string]any)["a"].(map[string]any)["componentID"]).To(Equal("interface-templates-701"))
		// Every template-backed component ID is one of its DeviceType's
		// template IDs.
		typeName, _, _ := unstructured.NestedString(device.Object, "spec", "deviceTypeRef", "name")
		tmpls, _, _ := unstructured.NestedSlice(get(h.namespace, fleet.KindDeviceType, typeName).Object, "spec", "componentTemplates")
		templateIDs := map[string]bool{}
		for _, t := range tmpls {
			templateIDs[t.(map[string]any)["id"].(string)] = true
		}
		for _, id := range componentIDs(device) {
			Expect(templateIDs).To(HaveKey(id))
		}
		s := h.source()
		Expect(condition(s, ConditionApplied).Status).To(BeEquivalentTo("True"), "%+v", condition(s, ConditionApplied))
		Expect(recordFor(s, "dcim/devices/1000", "Device").State).To(Equal(netboxv1alpha1.RecordStateUpdated))

		// The next reconcile writes nothing.
		before = len(h.recorder.Applies())
		h.r.Now = func() time.Time { return testNow.Add(2 * time.Hour) }
		_, err = h.reconcile()
		Expect(err).NotTo(HaveOccurred())
		Expect(h.recorder.Applies()[before:]).To(BeEmpty())
		Expect(h.source().Status.Plan.Update).To(BeZero())
	})

	type move struct {
		name string
		// change edits NetBox.
		change func(h *harness)
		// component is the moved component's NetBox name, and id its ID.
		component, id string
		// typeWritten is whether the DeviceType changes too.
		typeWritten bool
	}
	// A template edit also updates the DeviceType; a component rename changes
	// only the Device and its Cable.
	DescribeTable("moves one component between its template and instance IDs with one Device update each way",
		func(steps ...move) {
			h := newHarness().withSourceAndToken(netboxv1alpha1.SourceModeCreate)
			_, err := h.reconcile()
			Expect(err).NotTo(HaveOccurred())
			deviceKey, cableKey := fleet.KindDevice+"/"+deviceName, fleet.KindCable+"/"+cableName
			typeName, _, _ := unstructured.NestedString(get(h.namespace, fleet.KindDevice, deviceName).Object, "spec", "deviceTypeRef", "name")
			typeKey := fleet.KindDeviceType + "/" + typeName

			var mu sync.Mutex
			var seen []string
			h.recorder.beforeApply = func(kind, name string) {
				mu.Lock()
				defer mu.Unlock()
				seen = append(seen, dangling(h.namespace)...)
			}
			DeferCleanup(func() { h.recorder.beforeApply = nil })
			at := testNow
			for _, step := range steps {
				By(step.name)
				step.change(h)
				before := len(h.recorder.Applies())
				at = at.Add(time.Hour)
				h.r.Now = func() time.Time { return at }
				_, err := h.reconcile()
				Expect(err).NotTo(HaveOccurred())
				Expect(dangling(h.namespace)).To(BeEmpty())
				mu.Lock()
				Expect(seen).To(BeEmpty(), "a Cable named a component its Device did not have")
				mu.Unlock()

				// The Device is one Update, applied as its interim and then as
				// planned; the Cable, and the DeviceType when it changes, are
				// applied once.
				writes := map[string]int{}
				for _, key := range h.recorder.Applies()[before:] {
					writes[key]++
				}
				want := map[string]int{deviceKey: 2, cableKey: 1}
				if step.typeWritten {
					want[typeKey] = 1
				}
				Expect(writes).To(Equal(want))
				s := h.source()
				Expect(s.Status.Plan.Updated).To(Equal(int32(len(want))))
				Expect(recordFor(s, "dcim/devices/1000", "Device").State).To(Equal(netboxv1alpha1.RecordStateUpdated))
				comps, _, _ := unstructured.NestedSlice(get(h.namespace, fleet.KindDevice, deviceName).Object, "spec", "components")
				var ids []string
				for _, c := range comps {
					if c.(map[string]any)["name"] == step.component {
						ids = append(ids, c.(map[string]any)["id"].(string))
					}
				}
				Expect(ids).To(Equal([]string{step.id}))
				lanes, _, _ := unstructured.NestedSlice(get(h.namespace, fleet.KindCable, cableName).Object, "spec", "lanes")
				Expect(lanes[0].(map[string]any)["a"].(map[string]any)["componentID"]).To(Equal(step.id))

				// The next reconcile writes nothing.
				before = len(h.recorder.Applies())
				at = at.Add(time.Hour)
				_, err = h.reconcile()
				Expect(err).NotTo(HaveOccurred())
				Expect(h.recorder.Applies()[before:]).To(BeEmpty())
				Expect(h.source().Status.Plan.Update).To(BeZero())
			}
		},
		Entry("template renamed and back",
			move{"renaming the eth1 template", func(h *harness) {
				h.server.Set(netbox.CollectionInterfaceTemplates, 701, "name", "eth1 (renamed)")
			}, "eth1", "interfaces-2001", true},
			move{"undoing the rename", func(h *harness) { h.server.Set(netbox.CollectionInterfaceTemplates, 701, "name", "eth1") }, "eth1", "interface-templates-701", true},
		),
		Entry("template deleted and restored",
			move{"deleting the eth1 template", func(h *harness) { h.server.Remove(netbox.CollectionInterfaceTemplates, 701) }, "eth1", "interfaces-2001", true},
			move{"restoring it", func(h *harness) {
				h.server.Add(netbox.CollectionInterfaceTemplates, map[string]any{
					"id": float64(701), "device_type": map[string]any{"id": float64(500)}, "name": "eth1", "type": map[string]any{"value": "25gbase-x-sfp28"},
				})
			}, "eth1", "interface-templates-701", true},
		),
		Entry("component renamed in NetBox and back",
			move{"renaming eth1 on the device", func(h *harness) { h.server.Set(netbox.CollectionInterfaces, 2001, "name", "uplink") }, "uplink", "interfaces-2001", false},
			move{"undoing the rename", func(h *harness) { h.server.Set(netbox.CollectionInterfaces, 2001, "name", "eth1") }, "eth1", "interface-templates-701", false},
		),
	)

	It("keeps another writer's field on the old component entry, and is quarantined until that writer lets go", func() {
		h := newHarness().withSourceAndToken(netboxv1alpha1.SourceModeCreate)
		_, err := h.reconcile()
		Expect(err).NotTo(HaveOccurred())
		// An operator records an asset tag on the template-backed eth1.
		tag := func(components []any) *unstructured.Unstructured {
			return fleetObject(fleet.KindDevice, h.namespace, deviceName, map[string]any{"components": components})
		}
		Expect(k8sClient.Apply(ctx, client.ApplyConfigurationFromUnstructured(tag([]any{
			map[string]any{"id": "interface-templates-701", "assetTag": "EXAMPLE-TAG-1"},
		})), client.FieldOwner("example-operator"))).To(Succeed())
		var mu sync.Mutex
		var seen []string
		h.recorder.beforeApply = func(kind, name string) {
			mu.Lock()
			defer mu.Unlock()
			seen = append(seen, dangling(h.namespace)...)
		}
		DeferCleanup(func() { h.recorder.beforeApply = nil })
		eth1 := func() map[string]map[string]any {
			comps, _, _ := unstructured.NestedSlice(get(h.namespace, fleet.KindDevice, deviceName).Object, "spec", "components")
			out := map[string]map[string]any{}
			for _, c := range comps {
				if c.(map[string]any)["name"] == "eth1" {
					out[c.(map[string]any)["id"].(string)] = c.(map[string]any)
				}
			}
			return out
		}
		reconcileAt := func(d time.Duration) map[string]int {
			before := len(h.recorder.Applies())
			h.r.Now = func() time.Time { return testNow.Add(d) }
			_, err := h.reconcile()
			Expect(err).NotTo(HaveOccurred())
			Expect(dangling(h.namespace)).To(BeEmpty())
			mu.Lock()
			Expect(seen).To(BeEmpty(), "a Cable named a component its Device did not have")
			mu.Unlock()
			writes := map[string]int{}
			for _, key := range h.recorder.Applies()[before:] {
				writes[key]++
			}
			return writes
		}
		deviceKey, cableKey := fleet.KindDevice+"/"+deviceName, fleet.KindCable+"/"+cableName
		typeName, _, _ := unstructured.NestedString(get(h.namespace, fleet.KindDevice, deviceName).Object, "spec", "deviceTypeRef", "name")

		By("renaming the template, so eth1 moves to its instance ID")
		h.server.Set(netbox.CollectionInterfaceTemplates, 701, "name", "eth1 (renamed)")
		Expect(reconcileAt(time.Hour)).To(Equal(map[string]int{deviceKey: 2, cableKey: 1, fleet.KindDeviceType + "/" + typeName: 1}))
		// Dropping the old entry would leave it with only the operator's
		// asset tag, and Fleet requires a component's name and type, so the
		// planned Device is rejected and the interim stays.
		Expect(eth1()).To(HaveLen(2))
		Expect(eth1()["interface-templates-701"]).To(HaveKeyWithValue("assetTag", "EXAMPLE-TAG-1"))
		Expect(eth1()["interfaces-2001"]).NotTo(HaveKey("assetTag"), "another writer's field is not carried to the new ID")
		s := h.source()
		Expect(recordFor(s, "dcim/devices/1000", "Device")).To(SatisfyAll(
			HaveField("State", netboxv1alpha1.RecordStateQuarantined), HaveField("Reason", "Rejected"),
			HaveField("Message", ContainSubstring("Required value"))))
		Expect(s.Status.InterimDevices).To(ConsistOf(SatisfyAll(HaveField("Name", deviceName), HaveField("WaitingFor", BeEmpty()))))
		Expect(condition(s, ConditionCableReferencesDelivered).Reason).To(Equal(ReasonInterimDevices))

		By("reconciling again while the operator keeps its field")
		Expect(reconcileAt(2 * time.Hour)).To(Equal(map[string]int{deviceKey: 1}))
		Expect(eth1()).To(HaveLen(2))
		s = h.source()
		Expect(recordFor(s, "dcim/devices/1000", "Device").Reason).To(Equal("Rejected"))
		Expect(condition(s, ConditionReady).Reason).To(Equal(ReasonReviewRequired))
		Expect(s.Status.InterimDeviceCount).To(BeZero(), "the Device is no longer staged; its quarantine is what reports it")
		Expect(condition(s, ConditionCableReferencesDelivered).Status).To(BeEquivalentTo("True"), "every Cable names a component the Device has")

		By("the operator releasing its field")
		Expect(k8sClient.Apply(ctx, client.ApplyConfigurationFromUnstructured(tag([]any{})), client.FieldOwner("example-operator"))).To(Succeed())
		Expect(reconcileAt(3 * time.Hour)).To(Equal(map[string]int{deviceKey: 1}))
		Expect(eth1()).To(SatisfyAll(HaveLen(1), HaveKey("interfaces-2001")))
		Expect(recordFor(h.source(), "dcim/devices/1000", "Device").State).To(Equal(netboxv1alpha1.RecordStateUpdated))
		Expect(reconcileAt(4 * time.Hour)).To(BeEmpty())
	})

	It("keeps the old components while a Cable that names them is not written", func() {
		h := newHarness().withSourceAndToken(netboxv1alpha1.SourceModeCreate)
		for _, t := range templates {
			h.server.Set(t.collection, t.id, "name", t.name+" (renamed)")
		}
		_, err := h.reconcile()
		Expect(err).NotTo(HaveOccurred())
		for _, t := range templates {
			h.server.Set(t.collection, t.id, "name", t.name)
		}
		h.recorder.applyError = func(kind, name string) error {
			if kind == fleet.KindCable && name == cableName {
				return apierrors.NewServiceUnavailable("synthetic outage")
			}
			return nil
		}
		h.r.Now = func() time.Time { return testNow.Add(time.Hour) }
		_, err = h.reconcile()
		Expect(err).NotTo(HaveOccurred())
		Expect(dangling(h.namespace)).To(BeEmpty())
		Expect(componentIDs(get(h.namespace, fleet.KindDevice, deviceName))).To(ContainElements("interface-templates-701", "interfaces-2001"))
		Expect(condition(h.source(), ConditionApplied).Status).To(BeEquivalentTo("Unknown"))

		// Once the Cable is written, the old components go.
		h.recorder.applyError = nil
		h.r.Now = func() time.Time { return testNow.Add(2 * time.Hour) }
		_, err = h.reconcile()
		Expect(err).NotTo(HaveOccurred())
		Expect(dangling(h.namespace)).To(BeEmpty())
		Expect(componentIDs(get(h.namespace, fleet.KindDevice, deviceName))).To(Equal([]string{"interface-templates-700", "interface-templates-701", "power-port-templates-710"}))
	})
})
