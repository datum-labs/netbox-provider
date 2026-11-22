// SPDX-License-Identifier: AGPL-3.0-only

package controller

import (
	"strings"
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

var _ = Describe("another writer between plan and apply", func() {
	rackName := plan.Name(plan.SourceKey{Source: testSystem, Type: plan.TypeRack, ID: "100"}, fleet.KindRack)

	// once runs f the first time an apply targets kind/name.
	once := func(h *harness, kind, name string, f func()) {
		var done sync.Once
		h.recorder.beforeApply = func(k, n string) {
			if k == kind && n == name {
				done.Do(f)
			}
		}
	}
	get := func(h *harness, kind, name string) *unstructured.Unstructured {
		obj := &unstructured.Unstructured{}
		obj.SetGroupVersionKind(fleet.GVK(kind))
		Expect(k8sClient.Get(ctx, client.ObjectKey{Namespace: h.namespace, Name: name}, obj)).To(Succeed())
		return obj
	}

	It("releases every field it applied to a record another writer created, and adopts nothing", func() {
		h := newHarness().withSourceAndToken(netboxv1alpha1.SourceModeCreate)
		// The other writer's Rack agrees with NetBox on the name and Facility,
		// so the create apply merges into it without a conflict.
		once(h, fleet.KindRack, rackName, func() {
			operator := fleetObject(fleet.KindRack, h.namespace, rackName, map[string]any{
				"displayName": "Example Rack 01", "facilityRef": map[string]any{"name": testFacility},
			})
			Expect(k8sClient.Create(ctx, operator, client.FieldOwner("example-operator"))).To(Succeed())
		})
		_, err := h.reconcile()
		Expect(err).NotTo(HaveOccurred())

		rec := recordFor(h.source(), "dcim/racks/100", "Rack")
		Expect(rec.State).To(Equal(netboxv1alpha1.RecordStateQuarantined))
		Expect(rec.Reason).To(Equal(plan.ReasonNameConflict))
		Expect(rec.Message).To(ContainSubstring("fields were released"))

		rack := get(h, fleet.KindRack, rackName)
		for key := range rack.GetLabels() {
			Expect(key).NotTo(HavePrefix("netbox.miloapis.com/"), "no correlation label is left on the other writer's record")
		}
		for key := range rack.GetAnnotations() {
			Expect(key).NotTo(HavePrefix("netbox.miloapis.com/"))
		}
		Expect(plan.AppliedBy(rack, plan.FieldManager)).To(BeFalse(), "netbox-provider owns nothing on it")
		for _, entry := range rack.GetManagedFields() {
			Expect(entry.Manager).To(Equal("example-operator"))
		}
		_, has, _ := unstructured.NestedFieldNoCopy(rack.Object, "spec", "heightUnits")
		Expect(has).To(BeFalse(), "a field only this controller applied is removed")
		name, _, _ := unstructured.NestedString(rack.Object, "spec", "displayName")
		Expect(name).To(Equal("Example Rack 01"), "the other writer keeps its fields")

		// Records that reference the Rack are not written either.
		Expect(recordFor(h.source(), "dcim/devices/1000", "DeviceClaim").Reason).To(Equal(plan.ReasonDependencyBlocked))
		// The next reconcile sees the other writer's record and leaves it alone.
		applies := len(h.recorder.Applies())
		h.recorder.beforeApply = nil
		h.r.Now = func() time.Time { return testNow.Add(time.Hour) }
		_, err = h.reconcile()
		Expect(err).NotTo(HaveOccurred())
		Expect(h.recorder.Applies()[applies:]).NotTo(ContainElement(fleet.KindRack + "/" + rackName))
		Expect(recordFor(h.source(), "dcim/racks/100", "Rack").Reason).To(Equal(plan.ReasonNameConflict))
	})

	It("retries a failed release on later reconciles and holds the record until it succeeds", func() {
		h := newHarness().withSourceAndToken(netboxv1alpha1.SourceModeCreate)
		once(h, fleet.KindRack, rackName, func() {
			operator := fleetObject(fleet.KindRack, h.namespace, rackName, map[string]any{
				"displayName": "Example Rack 01", "facilityRef": map[string]any{"name": testFacility},
			})
			Expect(k8sClient.Create(ctx, operator, client.FieldOwner("example-operator"))).To(Succeed())
		})
		// The create apply succeeds; every later apply to the Rack, which is
		// a release, fails until failing is cleared.
		var mu sync.Mutex
		rackApplies, failing := 0, true
		h.recorder.applyError = func(kind, name string) error {
			mu.Lock()
			defer mu.Unlock()
			if kind != fleet.KindRack || name != rackName {
				return nil
			}
			rackApplies++
			if rackApplies > 1 && failing {
				return apierrors.NewServiceUnavailable("synthetic release failure")
			}
			return nil
		}
		ownedByUs := func() bool { return plan.AppliedBy(get(h, fleet.KindRack, rackName), plan.FieldManager) }

		_, err := h.reconcile()
		Expect(err).NotTo(HaveOccurred())
		rec := recordFor(h.source(), "dcim/racks/100", "Rack")
		Expect(rec.State).To(Equal(netboxv1alpha1.RecordStateQuarantined))
		Expect(rec.Reason).To(Equal(plan.ReasonNameConflict))
		Expect(rec.Message).To(ContainSubstring("retried by later reconciles"))
		Expect(condition(h.source(), ConditionApplied).Status).To(BeEquivalentTo("Unknown"))
		Expect(condition(h.source(), ConditionReady).Status).To(BeEquivalentTo("Unknown"))
		pending := h.source().Status.PendingReleases
		Expect(pending).To(HaveLen(1))
		Expect(pending[0].Kind).To(Equal(fleet.KindRack))
		Expect(pending[0].Name).To(Equal(rackName))
		Expect(pending[0].UID).To(Equal(string(get(h, fleet.KindRack, rackName).GetUID())))
		Expect(ownedByUs()).To(BeTrue(), "the failed release left this controller's fields")

		// The release fails again. The record still carries this controller's
		// labels and ownership, so only the pending entry keeps it from being
		// planned as Unchanged and adopted.
		h.r.Now = func() time.Time { return testNow.Add(time.Hour) }
		_, err = h.reconcile()
		Expect(err).NotTo(HaveOccurred())
		rec = recordFor(h.source(), "dcim/racks/100", "Rack")
		Expect(rec.State).To(Equal(netboxv1alpha1.RecordStateQuarantined))
		Expect(rec.Reason).To(Equal(plan.ReasonNameConflict))
		Expect(rec.Message).To(ContainSubstring("not yet released"))
		Expect(condition(h.source(), ConditionApplied).Reason).To(Equal(ReasonReleasePending))
		Expect(condition(h.source(), ConditionApplied).Status).To(BeEquivalentTo("Unknown"))
		Expect(condition(h.source(), ConditionReady).Status).To(BeEquivalentTo("Unknown"))
		Expect(h.source().Status.PendingReleases).To(HaveLen(1))
		Expect(ownedByUs()).To(BeTrue())

		// The release succeeds on the next reconcile.
		mu.Lock()
		failing = false
		mu.Unlock()
		h.r.Now = func() time.Time { return testNow.Add(2 * time.Hour) }
		_, err = h.reconcile()
		Expect(err).NotTo(HaveOccurred())
		Expect(h.source().Status.PendingReleases).To(BeEmpty())
		rack := get(h, fleet.KindRack, rackName)
		Expect(plan.AppliedBy(rack, plan.FieldManager)).To(BeFalse(), "netbox-provider owns nothing on it")
		for key := range rack.GetLabels() {
			Expect(key).NotTo(HavePrefix("netbox.miloapis.com/"))
		}
		name, _, _ := unstructured.NestedString(rack.Object, "spec", "displayName")
		Expect(name).To(Equal("Example Rack 01"), "the other writer keeps its fields")
		rec = recordFor(h.source(), "dcim/racks/100", "Rack")
		Expect(rec.Reason).To(Equal(plan.ReasonNameConflict))
		Expect(rec.Message).To(ContainSubstring("released on a later reconcile"))
		Expect(condition(h.source(), ConditionReady).Reason).To(Equal(ReasonReviewRequired))

		// From then on the record is the other writer's, and nothing is applied to it.
		applies := len(h.recorder.Applies())
		h.r.Now = func() time.Time { return testNow.Add(3 * time.Hour) }
		_, err = h.reconcile()
		Expect(err).NotTo(HaveOccurred())
		Expect(h.recorder.Applies()[applies:]).NotTo(ContainElement(fleet.KindRack + "/" + rackName))
		Expect(recordFor(h.source(), "dcim/racks/100", "Rack").Reason).To(Equal(plan.ReasonNameConflict))
	})

	It("never applies an update to a record replaced after it was observed", func() {
		h := newHarness().withSourceAndToken(netboxv1alpha1.SourceModeCreate)
		_, err := h.reconcile()
		Expect(err).NotTo(HaveOccurred())

		// NetBox changes the rack, so the next reconcile updates it. Between
		// plan and apply, another writer replaces the Rack with one that
		// agrees on every field, so only the UID precondition can tell.
		h.server.Set(netbox.CollectionRacks, 100, "u_height", 44)
		once(h, fleet.KindRack, rackName, func() {
			Expect(k8sClient.Delete(ctx, get(h, fleet.KindRack, rackName))).To(Succeed())
			replacement := fleetObject(fleet.KindRack, h.namespace, rackName, map[string]any{
				"displayName": "Example Rack 01", "facilityRef": map[string]any{"name": testFacility},
			})
			Expect(k8sClient.Create(ctx, replacement, client.FieldOwner("example-operator"))).To(Succeed())
		})
		h.r.Now = func() time.Time { return testNow.Add(time.Hour) }
		_, err = h.reconcile()
		Expect(err).NotTo(HaveOccurred())

		rec := recordFor(h.source(), "dcim/racks/100", "Rack")
		Expect(rec.Message).To(ContainSubstring("replaced or deleted"))
		Expect(condition(h.source(), ConditionApplied).Status).To(BeEquivalentTo("Unknown"))
		rack := get(h, fleet.KindRack, rackName)
		Expect(plan.AppliedBy(rack, plan.FieldManager)).To(BeFalse(), "nothing was applied to the replacement")
		for key := range rack.GetLabels() {
			Expect(strings.HasPrefix(key, "netbox.miloapis.com/")).To(BeFalse())
		}
		_, has, _ := unstructured.NestedFieldNoCopy(rack.Object, "spec", "heightUnits")
		Expect(has).To(BeFalse())
	})
})
