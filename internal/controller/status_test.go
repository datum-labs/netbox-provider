// SPDX-License-Identifier: AGPL-3.0-only

package controller

import (
	"encoding/json"
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

// statusWriter stands in for Fleet discovery, which owns Device.status.
const statusWriter = "example-bmc-discovery"

var _ = Describe("Device status another writer owns", func() {
	deviceName := plan.Name(plan.SourceKey{Source: testSystem, Type: plan.TypeDevice, ID: "1000"}, fleet.KindDevice)

	get := func(h *harness) *unstructured.Unstructured {
		obj := &unstructured.Unstructured{}
		obj.SetGroupVersionKind(fleet.GVK(fleet.KindDevice))
		Expect(k8sClient.Get(ctx, client.ObjectKey{Namespace: h.namespace, Name: deviceName}, obj)).To(Succeed())
		return obj
	}
	// writeStatus applies a synthetic discovery report to the status
	// subresource as statusWriter.
	writeStatus := func(h *harness) {
		obj := &unstructured.Unstructured{Object: map[string]any{"status": map[string]any{
			"observedAt": "2026-01-02T03:00:00Z",
			"reported": map[string]any{
				"systemUUID": "00000000-0000-4000-8000-000000000001",
				"interfaces": []any{map[string]any{"name": "eth0", "macAddress": "02:00:00:aa:10:00"}},
			},
			"conditions": []any{map[string]any{
				"type": "Matched", "status": "True", "reason": "SpecMatches", "message": "synthetic",
				"lastTransitionTime": "2026-01-02T03:00:00Z",
			}},
		}}}
		obj.SetGroupVersionKind(fleet.GVK(fleet.KindDevice))
		obj.SetNamespace(h.namespace)
		obj.SetName(deviceName)
		Expect(k8sClient.Status().Apply(ctx, client.ApplyConfigurationFromUnstructured(obj), client.FieldOwner(statusWriter))).To(Succeed())
	}
	// statusOf is the Device's status and the status writer's managed fields,
	// as JSON.
	statusOf := func(obj *unstructured.Unstructured) (string, string) {
		status, err := json.Marshal(obj.Object["status"])
		Expect(err).NotTo(HaveOccurred())
		var owned []metav1.ManagedFieldsEntry
		for _, entry := range obj.GetManagedFields() {
			Expect(entry.Subresource == "status").To(Equal(entry.Manager == statusWriter), "only the status writer writes status")
			if entry.Manager == statusWriter {
				entry.Time = nil
				owned = append(owned, entry)
			}
		}
		fields, err := json.Marshal(owned)
		Expect(err).NotTo(HaveOccurred())
		return string(status), string(fields)
	}
	reconcileAt := func(h *harness, d time.Duration) {
		h.r.Now = func() time.Time { return testNow.Add(d) }
		_, err := h.reconcile()
		Expect(err).NotTo(HaveOccurred())
	}
	deviceApplies := func(h *harness, from int) int {
		n := 0
		for _, key := range h.recorder.Applies()[from:] {
			if key == fleet.KindDevice+"/"+deviceName {
				n++
			}
		}
		return n
	}
	created := func() (*harness, string, string) {
		h := newHarness().withSourceAndToken(netboxv1alpha1.SourceModeCreate)
		_, err := h.reconcile()
		Expect(err).NotTo(HaveOccurred())
		Expect(recordFor(h.source(), "dcim/devices/1000", "Device").State).To(Equal(netboxv1alpha1.RecordStateCreated))
		writeStatus(h)
		status, fields := statusOf(get(h))
		Expect(status).To(ContainSubstring("00000000-0000-4000-8000-000000000001"), "the test CRD keeps the status writer's fields")
		return h, status, fields
	}

	It("is kept byte for byte, and is not drift, on a no-op reconcile", func() {
		h, status, fields := created()
		before := len(h.recorder.Applies())
		reconcileAt(h, time.Hour)
		Expect(deviceApplies(h, before)).To(BeZero(), "a status another writer filled is not drift")
		Expect(recordFor(h.source(), "dcim/devices/1000", "Device").State).To(Equal(netboxv1alpha1.RecordStateUnchanged))
		after, afterFields := statusOf(get(h))
		Expect(after).To(Equal(status))
		Expect(afterFields).To(Equal(fields))
	})

	It("is kept byte for byte when the Device's spec is updated", func() {
		h, status, fields := created()
		h.server.Set(netbox.CollectionDevices, 1000, "asset_tag", "SYNTH-ASSET-0009")
		before := len(h.recorder.Applies())
		reconcileAt(h, time.Hour)
		Expect(deviceApplies(h, before)).To(Equal(1))
		Expect(recordFor(h.source(), "dcim/devices/1000", "Device").State).To(Equal(netboxv1alpha1.RecordStateUpdated))
		d := get(h)
		tag, _, _ := unstructured.NestedString(d.Object, "spec", "assetTag")
		Expect(tag).To(Equal("SYNTH-ASSET-0009"))
		after, afterFields := statusOf(d)
		Expect(after).To(Equal(status))
		Expect(afterFields).To(Equal(fields))
	})

	It("is kept byte for byte when a create lands on a Device another writer made", func() {
		h := newHarness()
		var planned *unstructured.Unstructured
		for _, c := range fixturePlan(h).Candidates {
			if c.Kind == fleet.KindDevice && c.Name == deviceName {
				planned = c.Object
			}
		}
		Expect(planned).NotTo(BeNil())
		Expect(planned.Object).NotTo(HaveKey("status"), "the planned Device carries no status")
		h.withSourceAndToken(netboxv1alpha1.SourceModeCreate)
		var status, fields string
		h.recorder.beforeApply = func(kind, name string) {
			if kind != fleet.KindDevice || name != deviceName || status != "" {
				return
			}
			// Another writer creates the Device with the spec NetBox implies,
			// and discovery reports on it, between observation and apply.
			spec, _, _ := unstructured.NestedMap(planned.Object, "spec")
			Expect(k8sClient.Create(ctx, fleetObject(fleet.KindDevice, h.namespace, deviceName, spec), client.FieldOwner("example-operator"))).To(Succeed())
			writeStatus(h)
			status, fields = statusOf(get(h))
		}
		_, err := h.reconcile()
		Expect(err).NotTo(HaveOccurred())
		Expect(status).To(ContainSubstring("00000000-0000-4000-8000-000000000001"))
		rec := recordFor(h.source(), "dcim/devices/1000", "Device")
		Expect(rec.Reason).To(Equal(plan.ReasonNameConflict))
		Expect(rec.Message).To(ContainSubstring("fields were released"))
		d := get(h)
		Expect(plan.AppliedBy(d, plan.FieldManager)).To(BeFalse(), "the create and its release left netbox-provider owning nothing")
		after, afterFields := statusOf(d)
		Expect(after).To(Equal(status))
		Expect(afterFields).To(Equal(fields))
	})
})
