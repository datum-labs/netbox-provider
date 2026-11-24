// SPDX-License-Identifier: AGPL-3.0-only

package controller

import (
	"errors"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"

	netboxv1alpha1 "go.miloapis.com/netbox-provider/api/v1alpha1"
	"go.miloapis.com/netbox-provider/internal/fleet"
	"go.miloapis.com/netbox-provider/internal/netbox"
	"go.miloapis.com/netbox-provider/internal/plan"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("Cable references in status", func() {
	deviceName := plan.Name(plan.SourceKey{Source: testSystem, Type: plan.TypeDevice, ID: "1000"}, fleet.KindDevice)
	cableName := plan.Name(plan.SourceKey{Source: testSystem, Type: plan.TypeCable, ID: "5000"}, fleet.KindCable)
	renamed := []struct {
		collection string
		id         int
		name       string
	}{
		{netbox.CollectionInterfaceTemplates, 700, "eth0"},
		{netbox.CollectionInterfaceTemplates, 701, "eth1"},
		{netbox.CollectionPowerPortTemplates, 710, "PSU1"},
		{netbox.CollectionRearPortTemplates, 720, "rear1"},
	}
	gauges := func(h *harness) (float64, float64) {
		return testutil.ToFloat64(interimDevicesGauge.WithLabelValues(h.namespace, "example")),
			testutil.ToFloat64(undeliveredCableReferencesGauge.WithLabelValues(h.namespace, "example"))
	}
	// withInstanceIDs applies the fixture as an earlier version did, with
	// every component under its instance ID, and then restores the template
	// names, so the next reconcile moves components to template IDs.
	withInstanceIDs := func() *harness {
		h := newHarness().withSourceAndToken(netboxv1alpha1.SourceModeCreate)
		for _, t := range renamed {
			h.server.Set(t.collection, t.id, "name", t.name+" (renamed)")
		}
		_, err := h.reconcile()
		Expect(err).NotTo(HaveOccurred())
		for _, t := range renamed {
			h.server.Set(t.collection, t.id, "name", t.name)
		}
		return h
	}
	at := func(h *harness, d time.Duration) { h.r.Now = func() time.Time { return testNow.Add(d) } }

	It("is True with zero gauges when every Cable names components its Device has", func() {
		h := newHarness().withSourceAndToken(netboxv1alpha1.SourceModeCreate)
		_, err := h.reconcile()
		Expect(err).NotTo(HaveOccurred())
		s := h.source()
		Expect(condition(s, ConditionCableReferencesDelivered)).To(SatisfyAll(
			HaveField("Status", metav1.ConditionTrue), HaveField("Reason", ReasonDelivered)))
		Expect(s.Status.InterimDeviceCount).To(BeZero())
		Expect(s.Status.InterimDevices).To(BeEmpty())
		Expect(s.Status.UndeliveredCableReferences).To(BeZero())
		interim, undelivered := gauges(h)
		Expect(interim).To(BeZero())
		Expect(undelivered).To(BeZero())
	})

	It("lists a Device left interim by a blocked Cable write, and recovers once the Cable lands", func() {
		h := withInstanceIDs()
		h.recorder.applyError = func(kind, name string) error {
			if kind == fleet.KindCable && name == cableName {
				return apierrors.NewForbidden(schema.GroupResource{Group: fleet.Group, Resource: "cables"}, name, errors.New("synthetic denial"))
			}
			return nil
		}
		for i, d := range []time.Duration{time.Hour, 2 * time.Hour} {
			at(h, d)
			_, err := h.reconcile()
			Expect(err).NotTo(HaveOccurred())
			s := h.source()
			Expect(condition(s, ConditionCableReferencesDelivered)).To(SatisfyAll(
				HaveField("Status", metav1.ConditionFalse), HaveField("Reason", ReasonInterimDevices),
				HaveField("Message", SatisfyAll(ContainSubstring("1 Devices still hold old components"), ContainSubstring("Device/"+deviceName)))))
			Expect(s.Status.InterimDeviceCount).To(Equal(int32(1)))
			Expect(s.Status.InterimDevicesTruncated).To(BeFalse())
			Expect(s.Status.InterimDevices).To(HaveLen(1))
			Expect(s.Status.InterimDevices[0]).To(SatisfyAll(
				HaveField("Name", deviceName), HaveField("Source", "dcim/devices/1000"),
				HaveField("WaitingFor", []string{cableName}), HaveField("Message", ContainSubstring(cableName))))
			Expect(s.Status.InterimDevices[0].Since.Time).To(BeTemporally("==", testNow.Add(time.Hour)), "reconcile %d keeps when the Device was first left interim", i+1)
			Expect(s.Status.UndeliveredCableReferences).To(BeZero(), "the interim keeps the component the Cable names")
			interim, undelivered := gauges(h)
			Expect(interim).To(Equal(1.0))
			Expect(undelivered).To(BeZero())
		}

		h.recorder.applyError = nil
		at(h, 3*time.Hour)
		_, err := h.reconcile()
		Expect(err).NotTo(HaveOccurred())
		s := h.source()
		Expect(condition(s, ConditionCableReferencesDelivered)).To(SatisfyAll(
			HaveField("Status", metav1.ConditionTrue), HaveField("Reason", ReasonDelivered)))
		Expect(s.Status.InterimDeviceCount).To(BeZero())
		Expect(s.Status.InterimDevices).To(BeEmpty())
		interim, _ := gauges(h)
		Expect(interim).To(BeZero())
	})

	It("reports a Cable this plan does not write that names a component its Device dropped, on every reconcile", func() {
		h := withInstanceIDs()
		// The Cable leaves NetBox, so it is retained in Fleet and not written.
		h.server.Remove(netbox.CollectionCables, 5000)
		for _, d := range []time.Duration{time.Hour, 2 * time.Hour} {
			at(h, d)
			_, err := h.reconcile()
			Expect(err).NotTo(HaveOccurred())
			s := h.source()
			Expect(condition(s, ConditionCableReferencesDelivered)).To(SatisfyAll(
				HaveField("Status", metav1.ConditionFalse), HaveField("Reason", ReasonUndeliveredReferences),
				HaveField("Message", SatisfyAll(
					ContainSubstring("1 Cable endpoints"),
					ContainSubstring("Cable/"+cableName+" -> Device/"+deviceName+" component interfaces-2001")))))
			Expect(s.Status.UndeliveredCableReferences).To(Equal(int32(1)))
			Expect(s.Status.InterimDeviceCount).To(BeZero())
			interim, undelivered := gauges(h)
			Expect(interim).To(BeZero())
			Expect(undelivered).To(Equal(1.0))
		}
	})

	It("drops a deleted source's series", func() {
		h := newHarness().withSourceAndToken(netboxv1alpha1.SourceModeCreate)
		_, err := h.reconcile()
		Expect(err).NotTo(HaveOccurred())
		Expect(testutil.CollectAndCount(interimDevicesGauge)).To(BeNumerically(">", 0))
		Expect(k8sClient.Delete(ctx, h.source())).To(Succeed())
		_, err = h.reconcile()
		Expect(err).NotTo(HaveOccurred())
		for _, gauge := range []interface {
			DeleteLabelValues(...string) bool
		}{interimDevicesGauge, undeliveredCableReferencesGauge} {
			Expect(gauge.DeleteLabelValues(h.namespace, "example")).To(BeFalse(), "the series is already gone")
		}
	})
})
