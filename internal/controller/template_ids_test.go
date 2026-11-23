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
