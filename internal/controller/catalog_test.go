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

var _ = Describe("hardware catalog", func() {
	deviceName := plan.Name(plan.SourceKey{Source: testSystem, Type: plan.TypeDevice, ID: "1000"}, fleet.KindDevice)
	typeName := plan.Name(plan.SourceKey{Source: testSystem, Type: plan.TypeDeviceType, ID: "500"}, fleet.KindDeviceType)
	device := func(h *harness) *unstructured.Unstructured {
		obj := &unstructured.Unstructured{}
		obj.SetGroupVersionKind(fleet.GVK(fleet.KindDevice))
		Expect(k8sClient.Get(ctx, client.ObjectKey{Namespace: h.namespace, Name: deviceName}, obj)).To(Succeed())
		return obj
	}

	It("adds deviceTypeRef to Devices it applied once their type is written, and Fleet refuses to rebind it", func() {
		h := newHarness().withSourceAndToken(netboxv1alpha1.SourceModeCreate)
		// A name Fleet rejects leaves the catalog unwritten at first.
		h.server.Set(netbox.CollectionManufacturers, 50, "name", " Example Vendor")
		_, err := h.reconcile()
		Expect(err).NotTo(HaveOccurred())
		Expect(recordFor(h.source(), "dcim/manufacturers/50", "").Reason).To(Equal(plan.ReasonCanonicalNameInvalid))
		_, has, _ := unstructured.NestedFieldNoCopy(device(h).Object, "spec", "deviceTypeRef")
		Expect(has).To(BeFalse(), "the Device is created without waiting for its type")

		h.server.Set(netbox.CollectionManufacturers, 50, "name", "Example Vendor")
		h.r.Now = func() time.Time { return testNow.Add(time.Hour) }
		_, err = h.reconcile()
		Expect(err).NotTo(HaveOccurred())
		s := h.source()
		Expect(recordFor(s, "dcim/device-types/500", "DeviceType").State).To(Equal(netboxv1alpha1.RecordStateCreated))
		Expect(recordFor(s, "dcim/devices/1000", "Device").State).To(Equal(netboxv1alpha1.RecordStateUpdated))
		ref, _, _ := unstructured.NestedString(device(h).Object, "spec", "deviceTypeRef", "name")
		Expect(ref).To(Equal(typeName))

		// Fleet itself rejects rebinding, which is why the plan never tries.
		rebind := &unstructured.Unstructured{Object: map[string]any{"spec": map[string]any{"deviceTypeRef": map[string]any{"name": "another-type"}}}}
		rebind.SetGroupVersionKind(fleet.GVK(fleet.KindDevice))
		rebind.SetNamespace(h.namespace)
		rebind.SetName(deviceName)
		err = k8sClient.Apply(ctx, client.ApplyConfigurationFromUnstructured(rebind), client.FieldOwner("example-operator"), client.ForceOwnership)
		Expect(apierrors.IsInvalid(err)).To(BeTrue(), "%v", err)
		Expect(err.Error()).To(ContainSubstring("cannot be removed or rebound"))
	})
})
