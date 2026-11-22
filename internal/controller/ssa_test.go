// SPDX-License-Identifier: AGPL-3.0-only

package controller

import (
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"

	netboxv1alpha1 "go.miloapis.com/netbox-provider/api/v1alpha1"
	"go.miloapis.com/netbox-provider/internal/fleet"
	"go.miloapis.com/netbox-provider/internal/netbox"
	"go.miloapis.com/netbox-provider/internal/plan"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("server-side apply field ownership", func() {
	deviceName := plan.Name(plan.SourceKey{Source: testSystem, Type: plan.TypeDevice, ID: "1000"}, fleet.KindDevice)
	claimName := plan.Name(plan.SourceKey{Source: testSystem, Type: plan.TypeDevice, ID: "1000"}, fleet.KindDeviceClaim)

	// created runs one Create-mode reconcile and returns the harness.
	created := func() *harness {
		h := newHarness().withSourceAndToken(netboxv1alpha1.SourceModeCreate)
		_, err := h.reconcile()
		Expect(err).NotTo(HaveOccurred())
		Expect(h.recorder.Applies()).To(HaveLen(19))
		return h
	}
	device := func(h *harness) *unstructured.Unstructured {
		obj := &unstructured.Unstructured{}
		obj.SetGroupVersionKind(fleet.GVK(fleet.KindDevice))
		Expect(k8sClient.Get(ctx, client.ObjectKey{Namespace: h.namespace, Name: deviceName}, obj)).To(Succeed())
		return obj
	}
	// otherApply applies spec fields to the Device as another field manager.
	otherApply := func(h *harness, manager string, spec map[string]any, force bool) error {
		obj := &unstructured.Unstructured{Object: map[string]any{"spec": spec}}
		obj.SetGroupVersionKind(fleet.GVK(fleet.KindDevice))
		obj.SetNamespace(h.namespace)
		obj.SetName(deviceName)
		opts := []client.ApplyOption{client.FieldOwner(manager)}
		if force {
			opts = append(opts, client.ForceOwnership)
		}
		return k8sClient.Apply(ctx, client.ApplyConfigurationFromUnstructured(obj), opts...)
	}
	later := func(h *harness, d time.Duration) {
		h.r.Now = func() time.Time { return testNow.Add(d) }
		_, err := h.reconcile()
		Expect(err).NotTo(HaveOccurred())
	}

	It("records netbox-provider as the apply manager of exactly the fields it writes", func() {
		h := created()
		for _, kind := range fleet.WriteKinds {
			for i := range h.list(kind) {
				obj := h.list(kind)[i]
				Expect(plan.AppliedBy(&obj, plan.FieldManager)).To(BeTrue(), obj.GetName())
				for _, entry := range obj.GetManagedFields() {
					Expect(entry.Manager).To(Equal(plan.FieldManager), "only this controller has written %s", obj.GetName())
					Expect(entry.Operation).To(Equal(metav1.ManagedFieldsOperationApply))
					Expect(entry.Subresource).To(BeEmpty(), "status is never written")
				}
			}
		}
	})

	It("keeps another manager's field, and updates the fields it owns when NetBox changes them", func() {
		h := created()
		Expect(otherApply(h, "example-collector", map[string]any{"unmappedMACAddresses": []any{"02:00:00:00:00:01"}}, false)).To(Succeed())

		h.server.Set(netbox.CollectionDevices, 1000, "asset_tag", "SYNTH-ASSET-0009")
		later(h, time.Hour)
		Expect(h.recorder.Applies()).To(HaveLen(20), "only the changed Device is applied again")
		Expect(h.recorder.Applies()[19]).To(Equal(fleet.KindDevice + "/" + deviceName))
		s := h.source()
		Expect(recordFor(s, "dcim/devices/1000", "Device").State).To(Equal(netboxv1alpha1.RecordStateUpdated))
		Expect(s.Status.Plan.Updated).To(Equal(int32(1)))

		d := device(h)
		tag, _, _ := unstructured.NestedString(d.Object, "spec", "assetTag")
		Expect(tag).To(Equal("SYNTH-ASSET-0009"))
		macs, _, _ := unstructured.NestedStringSlice(d.Object, "spec", "unmappedMACAddresses")
		Expect(macs).To(Equal([]string{"02:00:00:00:00:01"}), "another manager's field survives the apply")
		Expect(plan.AppliedBy(d, "example-collector")).To(BeTrue())

		// A second reconcile is a no-op.
		later(h, 2*time.Hour)
		Expect(h.recorder.Applies()).To(HaveLen(20))
		Expect(h.source().Status.Plan.Unchanged).To(Equal(int32(19)))
	})

	It("quarantines a conflict with another manager instead of forcing ownership", func() {
		h := created()
		Expect(otherApply(h, "example-operator", map[string]any{"displayName": "operator-chosen-name"}, true)).To(Succeed())

		later(h, time.Hour)
		Expect(h.recorder.Applies()).To(HaveLen(20), "the drifted Device is applied once, without force")
		rec := recordFor(h.source(), "dcim/devices/1000", "Device")
		Expect(rec.State).To(Equal(netboxv1alpha1.RecordStateQuarantined))
		Expect(rec.Reason).To(Equal(plan.ReasonApplyConflict))
		Expect(rec.Message).To(SatisfyAll(ContainSubstring("example-operator"), ContainSubstring(".spec.displayName")))
		name, _, _ := unstructured.NestedString(device(h).Object, "spec", "displayName")
		Expect(name).To(Equal("operator-chosen-name"), "the other manager keeps its field")
	})

	It("quarantines a change to an immutable field without writing it", func() {
		h := created()
		h.server.Set(netbox.CollectionDevices, 1000, "serial", "SYNTH-CHASSIS-0099")
		later(h, time.Hour)
		rec := recordFor(h.source(), "dcim/devices/1000", "Device")
		Expect(rec.State).To(Equal(netboxv1alpha1.RecordStateQuarantined))
		Expect(rec.Reason).To(Equal(plan.ReasonImmutableChanged))
		Expect(rec.Message).To(ContainSubstring("a hardware swap is a new Device"))
		Expect(h.recorder.Applies()[19:]).NotTo(ContainElement(fleet.KindDevice+"/"+deviceName), "the Device is not applied again")
		serial, _, _ := unstructured.NestedString(device(h).Object, "spec", "identity", "serial")
		Expect(serial).To(Equal("SYNTH-CHASSIS-0001"))
		// The claim moves with its Device, so its update is held too.
		Expect(h.recorder.Applies()[19:]).NotTo(ContainElement(fleet.KindDeviceClaim + "/" + claimName))
		Expect(recordFor(h.source(), "dcim/devices/1000", "DeviceClaim").Reason).To(Equal(plan.ReasonDependencyBlocked))
	})

	It("adds components to a Device it applied before components were imported", func() {
		h := newHarness()
		p := fixturePlan(h)
		for _, kind := range fleet.WriteKinds {
			for _, c := range p.Candidates {
				if c.Kind != kind || c.Action != plan.ActionCreate {
					continue
				}
				obj := c.Object.DeepCopy()
				if kind == fleet.KindCable {
					continue
				}
				if kind == fleet.KindDevice {
					// What an earlier version of this controller applied.
					unstructured.RemoveNestedField(obj.Object, "spec", "components")
					unstructured.RemoveNestedField(obj.Object, "spec", "internalMappings")
					annotations := obj.GetAnnotations()
					annotations[plan.AnnotationAppliedDigest] = plan.AppliedDigest(obj)
					obj.SetAnnotations(annotations)
				}
				Expect(k8sClient.Apply(ctx, client.ApplyConfigurationFromUnstructured(obj), client.FieldOwner(plan.FieldManager))).To(Succeed())
			}
		}
		h.withSourceAndToken(netboxv1alpha1.SourceModeCreate)
		_, err := h.reconcile()
		Expect(err).NotTo(HaveOccurred())
		s := h.source()
		Expect(recordFor(s, "dcim/devices/1000", "Device").State).To(Equal(netboxv1alpha1.RecordStateUpdated))
		components, _, _ := unstructured.NestedSlice(device(h).Object, "spec", "components")
		Expect(components).To(HaveLen(3))
		Expect(recordFor(s, "dcim/cables/5000", "Cable").State).To(Equal(netboxv1alpha1.RecordStateCreated), "cables to its new components are created")
		Expect(s.Status.Plan.Quarantined).To(BeZero())
	})

	It("makes no write calls in Observe mode", func() {
		h := newHarness().withSourceAndToken(netboxv1alpha1.SourceModeObserve)
		_, err := h.reconcile()
		Expect(err).NotTo(HaveOccurred())
		Expect(h.recorder.Applies()).To(BeEmpty())
		Expect(fleetCount(h)).To(BeZero())
		Expect(h.source().Status.Plan.Create).To(Equal(int32(19)))
	})
})
