// SPDX-License-Identifier: AGPL-3.0-only

package controller

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	netboxv1alpha1 "go.miloapis.com/netbox-provider/api/v1alpha1"
	"go.miloapis.com/netbox-provider/internal/fleet"
	"go.miloapis.com/netbox-provider/internal/netbox"
	"go.miloapis.com/netbox-provider/internal/plan"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

func fleetCount(h *harness) int {
	n := 0
	for _, kind := range fleet.WriteKinds {
		n += len(h.list(kind))
	}
	return n
}

var _ = Describe("NetBoxSource reconcile", func() {
	It("observes only by default: plans, reports, and writes nothing", func() {
		h := newHarness().withSourceAndToken("")
		result, err := h.reconcile()
		Expect(err).NotTo(HaveOccurred())
		Expect(result.RequeueAfter).To(Equal(30 * time.Minute))
		Expect(h.recorder.Creates()).To(BeEmpty())
		Expect(fleetCount(h)).To(Equal(0))

		s := h.source()
		Expect(s.Spec.Mode).To(Equal(netboxv1alpha1.SourceModeObserve))
		Expect(condition(s, ConditionConfigurationResolved).Status).To(Equal(metav1.ConditionTrue))
		Expect(condition(s, ConditionSourceObserved).Status).To(Equal(metav1.ConditionTrue))
		Expect(condition(s, ConditionFleetObserved).Status).To(Equal(metav1.ConditionTrue))
		Expect(condition(s, ConditionApplied)).To(SatisfyAll(
			HaveField("Status", metav1.ConditionFalse), HaveField("Reason", ReasonObserveOnly), HaveField("Message", ContainSubstring("13 records would be created"))))
		// The fixture deliberately leaves records unresolved, so review is required.
		Expect(condition(s, ConditionReady)).To(SatisfyAll(HaveField("Status", metav1.ConditionFalse), HaveField("Reason", ReasonReviewRequired)))
		Expect(*s.Status.Plan).To(Equal(netboxv1alpha1.PlanSummary{Create: 13, Unresolved: 11, Excluded: 3}))
		Expect(s.Status.LastObservation.SnapshotDigest).To(HavePrefix("sha256:"))
		Expect(s.Status.LastObservation.MappingRevision).To(Equal("example-reviewed-1"))
		Expect(s.Status.Records[0].State).To(Equal(netboxv1alpha1.RecordStateUnresolved), "conflicts and unresolved records are listed first")
		encoded, _ := json.Marshal(s.Status)
		for _, private := range []string{"SYNTH-CHASSIS", "SYNTH-ASSET", "SYNTH-HOST", testToken} {
			Expect(string(encoded)).NotTo(ContainSubstring(private), "status must not carry source field values or credentials")
		}
	})

	It("creates new records only after the explicit Create opt-in, then is a no-op", func() {
		h := newHarness().withSourceAndToken(netboxv1alpha1.SourceModeCreate)
		_, err := h.reconcile()
		Expect(err).NotTo(HaveOccurred())
		Expect(h.recorder.Creates()).To(HaveLen(13))
		Expect(h.list(fleet.KindRack)).To(HaveLen(1))
		Expect(h.list(fleet.KindPowerFeed)).To(HaveLen(1))
		Expect(h.list(fleet.KindDeviceClaim)).To(HaveLen(5))
		Expect(h.list(fleet.KindDevice)).To(HaveLen(3), "devices without a reviewed chassis serial are not created")
		Expect(h.list(fleet.KindCable)).To(HaveLen(3), "cables are created only between created records")
		s := h.source()
		Expect(condition(s, ConditionApplied)).To(SatisfyAll(HaveField("Status", metav1.ConditionTrue), HaveField("Reason", ReasonApplied)))
		Expect(s.Status.Plan.Created).To(Equal(int32(13)))

		// Every kind is created after the kinds it references.
		creates := h.recorder.Creates()
		order := map[string]int{}
		for i, kind := range fleet.WriteKinds {
			order[kind] = i
		}
		for i := 1; i < len(creates); i++ {
			Expect(order[strings.Split(creates[i-1], "/")[0]]).To(BeNumerically("<=", order[strings.Split(creates[i], "/")[0]]), creates)
		}
		for _, d := range h.list(fleet.KindDevice) {
			claim, _, _ := unstructured.NestedString(d.Object, "spec", "claimRef", "name")
			Expect(claim).NotTo(BeEmpty())
		}
		for _, kind := range fleet.WriteKinds {
			for _, obj := range h.list(kind) {
				Expect(obj.Object["spec"]).NotTo(HaveKey("provenance"))
				labels := obj.GetLabels()
				Expect(labels).To(HaveLen(3))
				Expect(labels).To(HaveKeyWithValue(plan.LabelSource, testSystem))
				Expect(obj.GetName()).To(Equal(plan.Name(plan.SourceKey{Source: testSystem, Type: strings.ReplaceAll(labels[plan.LabelType], ".", "/"), ID: labels[plan.LabelID]}, kind)))
			}
		}

		h.r.Now = func() time.Time { return testNow.Add(time.Hour) }
		_, err = h.reconcile()
		Expect(err).NotTo(HaveOccurred())
		Expect(h.recorder.Creates()).To(HaveLen(13), "records correlated by our labels are unchanged; nothing is written")
		s = h.source()
		Expect(s.Status.Plan.Unchanged).To(Equal(int32(13)))
		Expect(s.Status.Plan.Create).To(BeZero())
		Expect(condition(s, ConditionApplied).Message).To(Equal("no new records to create"))
	})

	It("rejects an existing name from another writer and never overwrites it", func() {
		h := newHarness()
		rackName := plan.Name(plan.SourceKey{Source: testSystem, Type: plan.TypeRack, ID: "100"}, fleet.KindRack)
		flux := fleetObject(fleet.KindRack, h.namespace, rackName, map[string]any{
			"displayName": "Operator rack", "facilityRef": map[string]any{"name": testFacility},
		})
		flux.SetLabels(map[string]string{"kustomize.toolkit.fluxcd.io/name": "example-inventory", "kustomize.toolkit.fluxcd.io/namespace": "example"})
		Expect(k8sClient.Create(ctx, flux)).To(Succeed())
		before := flux.GetResourceVersion()
		h.withSourceAndToken(netboxv1alpha1.SourceModeCreate)

		_, err := h.reconcile()
		Expect(err).NotTo(HaveOccurred())
		rack := &unstructured.Unstructured{}
		rack.SetGroupVersionKind(fleet.GVK(fleet.KindRack))
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(flux), rack)).To(Succeed())
		Expect(rack.GetResourceVersion()).To(Equal(before))
		s := h.source()
		Expect(recordFor(s, "dcim/racks/100", "Rack")).To(SatisfyAll(
			HaveField("State", netboxv1alpha1.RecordStateQuarantined), HaveField("Reason", plan.ReasonNameConflict), HaveField("Message", ContainSubstring("Flux"))))
		Expect(recordFor(s, "dcim/devices/1000", "Device").Reason).To(Equal(plan.ReasonDependencyBlocked))
		// Only the claim that does not depend on the rack is created.
		Expect(h.recorder.Creates()).To(HaveLen(1))
		Expect(condition(s, ConditionReady).Reason).To(Equal(ReasonReviewRequired))
	})

	It("never adopts an identical record that lacks this controller's labels", func() {
		h := newHarness()
		p := fixturePlan(h)
		var unlabeled *unstructured.Unstructured
		for _, c := range p.Candidates {
			if c.Kind == fleet.KindRack {
				unlabeled = c.Object.DeepCopy()
				unlabeled.SetLabels(map[string]string{"example.com/owner": "operator"})
			}
		}
		Expect(k8sClient.Create(ctx, unlabeled)).To(Succeed())
		before := unlabeled.GetResourceVersion()
		h.withSourceAndToken(netboxv1alpha1.SourceModeCreate)
		_, err := h.reconcile()
		Expect(err).NotTo(HaveOccurred())
		Expect(recordFor(h.source(), "dcim/racks/100", "Rack")).To(SatisfyAll(
			HaveField("State", netboxv1alpha1.RecordStateQuarantined), HaveField("Reason", plan.ReasonNameConflict)))
		current := &unstructured.Unstructured{}
		current.SetGroupVersionKind(fleet.GVK(fleet.KindRack))
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(unlabeled), current)).To(Succeed())
		Expect(current.GetResourceVersion()).To(Equal(before))
		Expect(current.GetLabels()).NotTo(HaveKey(plan.LabelSource))
	})

	It("plans the claim but not the Device when no chassis serial is known", func() {
		h := newHarness().withSourceAndToken(netboxv1alpha1.SourceModeCreate)
		_, err := h.reconcile()
		Expect(err).NotTo(HaveOccurred())
		s := h.source()
		Expect(recordFor(s, "dcim/devices/1002", "")).To(SatisfyAll(
			HaveField("State", netboxv1alpha1.RecordStateUnresolved), HaveField("Reason", plan.ReasonIdentityUnavailable)))
		Expect(recordFor(s, "dcim/devices/1002", "DeviceClaim").State).To(Equal(netboxv1alpha1.RecordStateCreated))
		Expect(h.recorder.Creates()).NotTo(ContainElement("Device/" + plan.Name(plan.SourceKey{Source: testSystem, Type: plan.TypeDevice, ID: "1002"}, fleet.KindDevice)))
	})

	It("quarantines an identity collision with an existing Device", func() {
		h := newHarness()
		legacy := fleetObject(fleet.KindDevice, h.namespace, "legacy-device", map[string]any{
			"claimRef": map[string]any{"name": "legacy-slot"},
			"identity": map[string]any{"model": "Example 1U Server", "serial": "SYNTH-CHASSIS-0001"},
		})
		Expect(k8sClient.Create(ctx, legacy)).To(Succeed())
		h.withSourceAndToken(netboxv1alpha1.SourceModeCreate)
		_, err := h.reconcile()
		Expect(err).NotTo(HaveOccurred())
		s := h.source()
		Expect(recordFor(s, "dcim/devices/1000", "Device")).To(SatisfyAll(
			HaveField("State", netboxv1alpha1.RecordStateQuarantined), HaveField("Reason", plan.ReasonIdentityCollision)))
		Expect(recordFor(s, "dcim/devices/1000", "DeviceClaim").Reason).To(Equal(plan.ReasonDependencyBlocked))
		Expect(h.recorder.Creates()).NotTo(ContainElement("Device/" + plan.Name(plan.SourceKey{Source: testSystem, Type: plan.TypeDevice, ID: "1000"}, fleet.KindDevice)))
		Expect(recordFor(s, "dcim/cables/5000", "Cable").Reason).To(Equal(plan.ReasonDependencyBlocked), "a cable never references a Device that is not created")
		for _, created := range h.recorder.Creates() {
			Expect(created).NotTo(Equal("Cable/" + plan.Name(plan.SourceKey{Source: testSystem, Type: plan.TypeCable, ID: "5000"}, fleet.KindCable)))
		}
	})

	It("never deletes records whose source disappears", func() {
		h := newHarness().withSourceAndToken(netboxv1alpha1.SourceModeCreate)
		_, err := h.reconcile()
		Expect(err).NotTo(HaveOccurred())
		Expect(fleetCount(h)).To(Equal(13))

		h.server.Remove(netbox.CollectionDevices, 1000)
		h.r.Now = func() time.Time { return testNow.Add(time.Hour) }
		_, err = h.reconcile()
		Expect(err).NotTo(HaveOccurred())
		Expect(fleetCount(h)).To(Equal(13))
		s := h.source()
		Expect(s.Status.Plan.RetainedMissingSource).To(Equal(int32(2)))
		Expect(recordFor(s, "dcim/devices/1000", "")).To(HaveField("State", netboxv1alpha1.RecordStateRetainedMissingSource))
	})

	It("reports NetBox failures as Unknown and writes nothing", func() {
		h := newHarness().withSourceAndToken(netboxv1alpha1.SourceModeCreate)
		h.server.Fail(netbox.CollectionDevices, http.StatusInternalServerError)
		_, err := h.reconcile()
		Expect(err).To(HaveOccurred())
		Expect(h.recorder.Creates()).To(BeEmpty(), "a partial read never produces writes")
		s := h.source()
		Expect(condition(s, ConditionSourceObserved)).To(SatisfyAll(HaveField("Status", metav1.ConditionUnknown), HaveField("Reason", ReasonReadFailed)))
		Expect(condition(s, ConditionReady).Status).To(Equal(metav1.ConditionUnknown))
		Expect(condition(s, ConditionApplied).Status).To(Equal(metav1.ConditionUnknown))
		Expect(condition(s, ConditionSourceObserved).Message).NotTo(ContainSubstring("sensitive"))

		h.server.Fail(netbox.CollectionDevices, 0)
		_, err = h.reconcile()
		Expect(err).NotTo(HaveOccurred())
		Expect(h.recorder.Creates()).To(HaveLen(13))
		Expect(condition(h.source(), ConditionSourceObserved).Status).To(Equal(metav1.ConditionTrue))
	})

	It("does not retry a rejected token for at least an hour", func() {
		h := newHarness().withSourceAndToken(netboxv1alpha1.SourceModeCreate)
		h.server.Fail(netbox.CollectionSites, http.StatusUnauthorized)
		result, err := h.reconcile()
		Expect(err).NotTo(HaveOccurred(), "a rejected token is not returned to the rate limiter")
		Expect(result.RequeueAfter).To(BeNumerically(">=", rejectedCredentialsInterval))
		Expect(h.server.Requests()).To(Equal(1), "one rejected request per attempt")
		Expect(h.recorder.Creates()).To(BeEmpty())
		s := h.source()
		Expect(condition(s, ConditionConfigurationResolved)).To(SatisfyAll(HaveField("Status", metav1.ConditionFalse), HaveField("Reason", ReasonTokenRejected)))
		Expect(condition(s, ConditionSourceObserved).Status).To(Equal(metav1.ConditionUnknown))
		Expect(condition(s, ConditionReady)).To(SatisfyAll(HaveField("Status", metav1.ConditionFalse), HaveField("Reason", ReasonTokenRejected)))
		Expect(condition(s, ConditionReady).Message).NotTo(ContainSubstring("sensitive"))
	})

	It("reports Fleet read failures as Unknown and writes nothing", func() {
		h := newHarness().withSourceAndToken(netboxv1alpha1.SourceModeCreate)
		// The Fleet API is unavailable while listing.
		base, err := client.NewWithWatch(cfg, client.Options{})
		Expect(err).NotTo(HaveOccurred())
		h.r.Fleet = interceptor.NewClient(base, interceptor.Funcs{
			List: func(context.Context, client.WithWatch, client.ObjectList, ...client.ListOption) error {
				return apierrors.NewServiceUnavailable("synthetic outage")
			},
		})
		_, err = h.reconcile()
		Expect(err).To(HaveOccurred())
		s := h.source()
		Expect(condition(s, ConditionFleetObserved).Status).To(Equal(metav1.ConditionUnknown))
		Expect(condition(s, ConditionReady).Status).To(Equal(metav1.ConditionUnknown))
		Expect(fleetCount(h)).To(BeZero())
	})

	Describe("token Secret handling", func() {
		It("does not contact NetBox without a Secret", func() {
			h := newHarness()
			Expect(k8sClient.Create(ctx, newSource(h.namespace, h.server.RootURL(), netboxv1alpha1.SourceModeCreate))).To(Succeed())
			result, err := h.reconcile()
			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).To(Equal(pollInterval))
			Expect(h.server.Requests()).To(BeZero())
			s := h.source()
			Expect(condition(s, ConditionConfigurationResolved)).To(SatisfyAll(HaveField("Status", metav1.ConditionFalse), HaveField("Reason", ReasonSecretNotFound)))
			Expect(condition(s, ConditionSourceObserved).Status).To(Equal(metav1.ConditionUnknown))
			Expect(condition(s, ConditionReady).Status).To(Equal(metav1.ConditionFalse))
		})

		DescribeTable("rejects unusable Secrets without contacting NetBox",
			func(key, value, reason string) {
				h := newHarness()
				Expect(k8sClient.Create(ctx, tokenSecret(h.namespace, key, value))).To(Succeed())
				Expect(k8sClient.Create(ctx, newSource(h.namespace, h.server.RootURL(), netboxv1alpha1.SourceModeCreate))).To(Succeed())
				_, err := h.reconcile()
				Expect(err).NotTo(HaveOccurred())
				Expect(h.server.Requests()).To(BeZero())
				s := h.source()
				Expect(condition(s, ConditionConfigurationResolved).Reason).To(Equal(reason))
				encoded, _ := json.Marshal(s)
				Expect(string(encoded)).NotTo(ContainSubstring(value))
			},
			Entry("missing key", "other", "synthetic-token-elsewhere", ReasonSecretKeyMissing),
			Entry("whitespace", "token", "synthetic token", ReasonTokenInvalid),
			Entry("empty", "token", "  \n", ReasonTokenInvalid),
		)

		It("sends the token only in the configured Authorization scheme", func() {
			h := newHarness()
			Expect(k8sClient.Create(ctx, tokenSecret(h.namespace, "api-token", testToken+"\n"))).To(Succeed())
			s := newSource(h.namespace, h.server.RootURL(), "")
			s.Spec.TokenSecretRef.Key = "api-token"
			s.Spec.AuthScheme = netboxv1alpha1.AuthSchemeToken
			Expect(k8sClient.Create(ctx, s)).To(Succeed())
			_, err := h.reconcile()
			Expect(err).NotTo(HaveOccurred())
			Expect(h.server.Authorizations()).NotTo(BeEmpty())
			for _, auth := range h.server.Authorizations() {
				Expect(auth).To(Equal("Token " + testToken))
			}
			encoded, _ := json.Marshal(h.source())
			Expect(string(encoded)).NotTo(ContainSubstring(testToken))
		})
	})

	It("reports a missing mapping and does not contact NetBox", func() {
		h := newHarness()
		Expect(k8sClient.Create(ctx, tokenSecret(h.namespace, "token", testToken))).To(Succeed())
		s := newSource(h.namespace, h.server.RootURL(), netboxv1alpha1.SourceModeCreate)
		s.Spec.MappingRef.Name = "absent"
		Expect(k8sClient.Create(ctx, s)).To(Succeed())
		_, err := h.reconcile()
		Expect(err).NotTo(HaveOccurred())
		Expect(condition(h.source(), ConditionConfigurationResolved).Reason).To(Equal(ReasonMappingNotFound))
		Expect(h.server.Requests()).To(BeZero())
	})

	It("does nothing while suspended", func() {
		h := newHarness()
		Expect(k8sClient.Create(ctx, tokenSecret(h.namespace, "token", testToken))).To(Succeed())
		s := newSource(h.namespace, h.server.RootURL(), netboxv1alpha1.SourceModeCreate)
		s.Spec.Suspend = true
		Expect(k8sClient.Create(ctx, s)).To(Succeed())
		_, err := h.reconcile()
		Expect(err).NotTo(HaveOccurred())
		Expect(h.server.Requests()).To(BeZero())
		Expect(fleetCount(h)).To(BeZero())
		Expect(condition(h.source(), ConditionReady).Reason).To(Equal(ReasonSuspended))
	})

	It("reconciles through the manager and re-plans when the mapping changes", func() {
		h := newHarness()
		Expect(k8sClient.Create(ctx, tokenSecret(h.namespace, "token", testToken))).To(Succeed())
		s := newSource(h.namespace, h.server.RootURL(), "")
		s.Spec.MappingRef.Name = "late"
		Expect(k8sClient.Create(ctx, s)).To(Succeed())

		mgr, err := ctrl.NewManager(cfg, ctrl.Options{
			Scheme:  testScheme,
			Metrics: metricsserver.Options{BindAddress: "0"},
			Cache:   cache.Options{DefaultNamespaces: map[string]cache.Config{h.namespace: {}}},
		})
		Expect(err).NotTo(HaveOccurred())
		r := *h.r
		r.Client, r.APIReader = mgr.GetClient(), mgr.GetAPIReader()
		Expect(r.SetupWithManager(mgr)).To(Succeed())
		mgrCtx, stop := context.WithCancel(ctx)
		DeferCleanup(stop)
		go func() {
			defer GinkgoRecover()
			Expect(mgr.Start(mgrCtx)).To(Succeed())
		}()

		Eventually(func() string { return condition(h.source(), ConditionConfigurationResolved).Reason }, 20*time.Second).Should(Equal(ReasonMappingNotFound))
		late := fixtureMapping(h.namespace)
		late.Name = "late"
		Expect(k8sClient.Create(ctx, late)).To(Succeed())
		Eventually(func() string { return condition(h.source(), ConditionApplied).Reason }, 20*time.Second).Should(Equal(ReasonObserveOnly))
		Expect(h.recorder.Creates()).To(BeEmpty())
	})
})
