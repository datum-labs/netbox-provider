// SPDX-License-Identifier: AGPL-3.0-only

package controller

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
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

// vendoredFleetCRDDigest is recorded in test/fleet/README.md with its source
// commit. Re-vendoring must update both.
const vendoredFleetCRDDigest = "023ae431bfd9ff566fa511a64b7bdc247c1bd0a0d4ae912e014a07bf21848f45"

var _ = Describe("Fleet admission of generated records", func() {
	It("uses the recorded vendored Fleet CRD", func() {
		data, err := os.ReadFile(filepath.Join("..", "..", "test", "fleet", "crds", "fleet.datumapis.com.yaml"))
		Expect(err).NotTo(HaveOccurred())
		sum := sha256.Sum256(data)
		Expect(hex.EncodeToString(sum[:])).To(Equal(vendoredFleetCRDDigest))
		readme, err := os.ReadFile(filepath.Join("..", "..", "test", "fleet", "README.md"))
		Expect(err).NotTo(HaveOccurred())
		Expect(string(readme)).To(ContainSubstring(vendoredFleetCRDDigest))
	})

	It("admits every candidate with no provenance, and rejects re-creation", func() {
		h := newHarness()
		p := fixturePlan(h)
		Expect(p.Count(plan.ActionCreate)).To(Equal(15))
		kinds := map[string]bool{}
		for _, kind := range fleet.WriteKinds {
			for _, c := range p.Candidates {
				if c.Kind == kind && c.Action == plan.ActionCreate {
					Expect(k8sClient.Create(ctx, c.Object.DeepCopy())).To(Succeed(), c.Key())
					kinds[kind] = true
				}
			}
		}
		// Fleet admission keeps what was written: nothing was pruned.
		for _, c := range p.Candidates {
			stored := &unstructured.Unstructured{}
			stored.SetGroupVersionKind(fleet.GVK(c.Kind))
			Expect(k8sClient.Get(ctx, clientKey(h.namespace, c.Name), stored)).To(Succeed())
			action, reason, _ := plan.Compare(stored, c.Object)
			Expect(action).To(Equal(plan.ActionUnchanged), c.Key()+" "+reason)
			Expect(stored.Object["spec"]).NotTo(HaveKey("provenance"), c.Key())
			Expect(stored.Object["spec"]).NotTo(HaveKey("reportedPlacement"), c.Key())
			Expect(stored.GetLabels()).To(HaveKeyWithValue(plan.LabelSource, testSystem))
		}
		Expect(kinds).To(Equal(map[string]bool{fleet.KindRack: true, fleet.KindPowerFeed: true, fleet.KindDeviceClaim: true, fleet.KindDevice: true, fleet.KindCable: true}))
		// A second create of the same name never overwrites.
		for _, c := range p.Candidates {
			err := k8sClient.Create(ctx, c.Object.DeepCopy())
			Expect(apierrors.IsAlreadyExists(err)).To(BeTrue(), c.Key())
		}
	})

	It("prunes provenance by default and rejects it under strict field validation", func() {
		h := newHarness()
		p := fixturePlan(h)
		for _, c := range p.Candidates {
			if c.Kind != fleet.KindRack {
				continue
			}
			withProvenance := func(name string) *unstructured.Unstructured {
				obj := c.Object.DeepCopy()
				obj.SetName(name)
				Expect(unstructured.SetNestedField(obj.Object, map[string]any{"method": "Manual"}, "spec", "provenance")).To(Succeed())
				return obj
			}
			err := k8sClient.Create(ctx, withProvenance("netbox-provenance-strict"), client.FieldValidation("Strict"))
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("provenance"))

			pruned := withProvenance("netbox-provenance-pruned")
			Expect(k8sClient.Create(ctx, pruned, client.FieldValidation("Ignore"))).To(Succeed())
			stored := &unstructured.Unstructured{}
			stored.SetGroupVersionKind(fleet.GVK(fleet.KindRack))
			Expect(k8sClient.Get(ctx, clientKey(h.namespace, pruned.GetName()), stored)).To(Succeed())
			Expect(stored.Object["spec"]).NotTo(HaveKey("provenance"))
			return
		}
		Fail("no rack candidate")
	})

	It("admits the unidentified marker only without serial, system UUID or BMC", func() {
		h := newHarness()
		p := fixturePlan(h)
		var marked, identified *unstructured.Unstructured
		for _, c := range p.Candidates {
			if c.Kind != fleet.KindDevice {
				continue
			}
			if v, _, _ := unstructured.NestedBool(c.Object.Object, "spec", "identity", "unidentified"); v {
				marked = c.Object
			} else if identified == nil {
				identified = c.Object
			}
		}
		Expect(marked).NotTo(BeNil(), "the fixture plans an unidentified Device")
		Expect(identified).NotTo(BeNil())
		probe := func(base *unstructured.Unstructured, name string, mutate func(*unstructured.Unstructured)) error {
			device := base.DeepCopy()
			device.SetName(name)
			mutate(device)
			return k8sClient.Create(ctx, device)
		}
		for name, tc := range map[string]struct {
			base    *unstructured.Unstructured
			mutate  func(*unstructured.Unstructured)
			message string
		}{
			"neither identity nor marker": {identified, func(d *unstructured.Unstructured) {
				unstructured.RemoveNestedField(d.Object, "spec", "identity", "serial")
			}, "a device needs a serial or system UUID, or unidentified set to true with a reason"},
			"marker with a serial": {marked, func(d *unstructured.Unstructured) {
				_ = unstructured.SetNestedField(d.Object, "SYNTH-PROBE-0001", "spec", "identity", "serial")
			}, "an unidentified device cannot carry a serial or system UUID"},
			"marker without a reason": {marked, func(d *unstructured.Unstructured) {
				unstructured.RemoveNestedField(d.Object, "spec", "identity", "unidentifiedReason")
			}, "unidentified and unidentifiedReason must be set together"},
			"marker false": {marked, func(d *unstructured.Unstructured) {
				_ = unstructured.SetNestedField(d.Object, false, "spec", "identity", "unidentified")
			}, "unidentified may only be true"},
			"marker with a BMC": {marked, func(d *unstructured.Unstructured) {
				_ = unstructured.SetNestedField(d.Object, "https://bmc.example.invalid", "spec", "bmc", "endpoint")
			}, "a device without hardware identity cannot record a BMC endpoint"},
		} {
			err := probe(tc.base, "netbox-probe-"+strings.ReplaceAll(name, " ", "-"), tc.mutate)
			Expect(apierrors.IsInvalid(err)).To(BeTrue(), "%s: %v", name, err)
			Expect(err.Error()).To(ContainSubstring(tc.message), name)
		}
	})
})

var _ = Describe("netbox.miloapis.com admission", func() {
	var namespace string
	BeforeEach(func() { namespace = newNamespace() })

	It("defaults to observe-only with a 30m interval and Bearer auth", func() {
		s := newSource(namespace, "https://netbox.example.invalid/prefix", "")
		Expect(k8sClient.Create(ctx, s)).To(Succeed())
		Expect(s.Spec.Mode).To(Equal(netboxv1alpha1.SourceModeObserve))
		Expect(s.Spec.Interval.Duration).To(Equal(30 * time.Minute))
		Expect(s.Spec.AuthScheme).To(Equal(netboxv1alpha1.AuthSchemeBearer))
		Expect(s.Spec.TokenSecretRef.Key).To(Equal("token"))
	})

	DescribeTable("rejects invalid sources",
		func(mutate func(*netboxv1alpha1.NetBoxSource), message string) {
			s := newSource(namespace, "https://netbox.example.invalid", "")
			mutate(s)
			err := k8sClient.Create(ctx, s)
			Expect(apierrors.IsInvalid(err)).To(BeTrue(), "%v", err)
			Expect(err.Error()).To(ContainSubstring(message))
		},
		Entry("plain HTTP", func(s *netboxv1alpha1.NetBoxSource) { s.Spec.URL = "http://netbox.example.invalid" }, "https URL"),
		Entry("credentials", func(s *netboxv1alpha1.NetBoxSource) { s.Spec.URL = "https://user:pw@netbox.example.invalid" }, "https URL"),
		Entry("query", func(s *netboxv1alpha1.NetBoxSource) { s.Spec.URL = "https://netbox.example.invalid/?token=x" }, "https URL"),
		Entry("non-Operator writer", func(s *netboxv1alpha1.NetBoxSource) { s.Spec.Target.Writer = "Flux" }, "Unsupported value"),
		Entry("unknown mode", func(s *netboxv1alpha1.NetBoxSource) { s.Spec.Mode = "Sync" }, "Unsupported value"),
		Entry("uppercase source ID", func(s *netboxv1alpha1.NetBoxSource) { s.Spec.SourceID = "Example" }, "sourceID"),
		Entry("short interval", func(s *netboxv1alpha1.NetBoxSource) { s.Spec.Interval = &metav1.Duration{Duration: 10 * time.Second} }, "between 1m and 24h"),
	)

	It("keeps the source identity and target immutable", func() {
		s := newSource(namespace, "https://netbox.example.invalid", "")
		Expect(k8sClient.Create(ctx, s)).To(Succeed())
		s.Spec.SourceID = "another-netbox"
		Expect(k8sClient.Update(ctx, s)).To(MatchError(ContainSubstring("sourceID is immutable")))
		Expect(k8sClient.Get(ctx, clientKey(namespace, s.Name), s)).To(Succeed())
		s.Spec.Target.Namespace = "elsewhere"
		Expect(k8sClient.Update(ctx, s)).To(MatchError(ContainSubstring("target namespace is immutable")))
		Expect(k8sClient.Get(ctx, clientKey(namespace, s.Name), s)).To(Succeed())
		s.Spec.URL = "https://netbox-renamed.example.invalid"
		Expect(k8sClient.Update(ctx, s)).To(Succeed(), "URL changes keep the source identity")
	})

	It("admits the fixture mapping", func() {
		Expect(k8sClient.Create(ctx, fixtureMapping(namespace))).To(Succeed())
	})

	DescribeTable("rejects invalid mappings",
		func(mutate func(*netboxv1alpha1.NetBoxMappingSpec), message string) {
			m := fixtureMapping(namespace)
			mutate(&m.Spec)
			err := k8sClient.Create(ctx, m)
			Expect(apierrors.IsInvalid(err)).To(BeTrue(), "%v", err)
			Expect(err.Error()).To(ContainSubstring(message))
		},
		Entry("excluded without a reason", func(m *netboxv1alpha1.NetBoxMappingSpec) {
			m.Devices = append(m.Devices, netboxv1alpha1.DeviceMapping{ID: 77, Classification: netboxv1alpha1.DeviceClassificationExcluded})
		}, "excluded devices need a reason"),
		Entry("physical without serial meaning", func(m *netboxv1alpha1.NetBoxMappingSpec) {
			m.Devices = append(m.Devices, netboxv1alpha1.DeviceMapping{ID: 77, Classification: netboxv1alpha1.DeviceClassificationPhysical, Placement: netboxv1alpha1.PlacementScopeFacility})
		}, "physical devices need serialMeaning and placement"),
		Entry("power as a reviewed data medium", func(m *netboxv1alpha1.NetBoxMappingSpec) {
			m.CableMedia = append(m.CableMedia, netboxv1alpha1.CableMediumRule{CableTypes: []string{"cat6"}, Medium: "Power"})
		}, "Unsupported value"),
		Entry("unknown placement", func(m *netboxv1alpha1.NetBoxMappingSpec) {
			m.Devices = append(m.Devices, netboxv1alpha1.DeviceMapping{ID: 77, Classification: netboxv1alpha1.DeviceClassificationPhysical,
				SerialMeaning: netboxv1alpha1.SerialMeaningUnknown, Placement: "NonRackUnitMount"})
		}, "Unsupported value"),
		Entry("duplicate device", func(m *netboxv1alpha1.NetBoxMappingSpec) { m.Devices = append(m.Devices, m.Devices[0]) }, "Duplicate value"),
		Entry("duplicate premises", func(m *netboxv1alpha1.NetBoxMappingSpec) { m.Facilities = append(m.Facilities, m.Facilities[0]) }, "Duplicate value"),
		Entry("missing revision", func(m *netboxv1alpha1.NetBoxMappingSpec) { m.Revision = "" }, "revision"),
	)
})

func clientKey(namespace, name string) client.ObjectKey {
	return client.ObjectKey{Namespace: namespace, Name: name}
}

func fixturePlan(h *harness) *plan.Plan {
	existing, err := h.r.observeFleet(ctx, h.namespace)
	Expect(err).NotTo(HaveOccurred())
	snapshot, err := netboxSnapshot(h)
	Expect(err).NotTo(HaveOccurred())
	return plan.Build(plan.Input{
		System: testSystem, Namespace: h.namespace, Snapshot: snapshot, Mapping: fixtureMapping(h.namespace).Spec,
		Existing: existing,
	})
}

func netboxSnapshot(h *harness) (*netbox.Snapshot, error) {
	client, err := netbox.New(netbox.Config{BaseURL: h.server.RootURL(), Token: testToken, Scheme: "Bearer", HTTPClient: h.server.Client()})
	if err != nil {
		return nil, err
	}
	return client.Snapshot(ctx, netbox.Scope{}, testNow)
}
