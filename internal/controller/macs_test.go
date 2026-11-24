// SPDX-License-Identifier: AGPL-3.0-only

package controller

import (
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

var _ = Describe("expected MACs", func() {
	deviceName := plan.Name(plan.SourceKey{Source: testSystem, Type: plan.TypeDevice, ID: "1000"}, fleet.KindDevice)
	interfaces := func(h *harness) []any {
		obj := &unstructured.Unstructured{}
		obj.SetGroupVersionKind(fleet.GVK(fleet.KindDevice))
		Expect(k8sClient.Get(ctx, client.ObjectKey{Namespace: h.namespace, Name: deviceName}, obj)).To(Succeed())
		items, _, _ := unstructured.NestedSlice(obj.Object, "spec", "interfaces")
		return items
	}

	It("are written in Fleet's form, and a MAC NetBox records twice is removed from both interfaces", func() {
		h := newHarness().withSourceAndToken(netboxv1alpha1.SourceModeCreate)
		_, err := h.reconcile()
		Expect(err).NotTo(HaveOccurred())
		Expect(interfaces(h)).To(Equal([]any{
			map[string]any{"name": "eth0", "macAddress": "02:00:00:aa:10:00"},
			map[string]any{"name": "eth1", "macAddress": "02:00:00:aa:10:01"},
		}), "from the primary MAC object and the legacy field; the LAG's MAC is not written")

		// Fleet itself rejects a MAC recorded on two interfaces of a Device.
		dup := fleetObject(fleet.KindDevice, h.namespace, deviceName, map[string]any{"interfaces": []any{
			map[string]any{"name": "eth0", "macAddress": "02:00:00:aa:10:00"},
			map[string]any{"name": "example-other", "macAddress": "02:00:00:aa:10:00"},
		}})
		err = k8sClient.Apply(ctx, client.ApplyConfigurationFromUnstructured(dup), client.FieldOwner("example-operator"))
		Expect(apierrors.IsInvalid(err)).To(BeTrue(), "%v", err)
		Expect(err.Error()).To(ContainSubstring("a MAC address may be recorded only once"))

		h.server.Set(netbox.CollectionInterfaces, 2001, "mac_address", "02:00:00:AA:10:00")
		h.r.Now = func() time.Time { return testNow.Add(time.Hour) }
		_, err = h.reconcile()
		Expect(err).NotTo(HaveOccurred())
		Expect(recordFor(h.source(), "dcim/devices/1000", "Device").State).To(Equal(netboxv1alpha1.RecordStateUpdated))
		Expect(interfaces(h)).To(BeEmpty(), "neither interface keeps an ambiguous MAC")

		// Status names the MACs left out.
		h.r.Now = func() time.Time { return testNow.Add(2 * time.Hour) }
		_, err = h.reconcile()
		Expect(err).NotTo(HaveOccurred())
		rec := recordFor(h.source(), "dcim/devices/1000", "Device")
		Expect(rec.State).To(Equal(netboxv1alpha1.RecordStateUnchanged))
		Expect(rec.Message).To(ContainSubstring("MAC recorded on more than one interface: interface-templates-700, interface-templates-701"))
	})
})
