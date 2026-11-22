// SPDX-License-Identifier: AGPL-3.0-only

package controller

import (
	"context"
	"encoding/json"
	"go.miloapis.com/netbox-provider/internal/plan"
	"k8s.io/apimachinery/pkg/runtime"
	"os"
	"path/filepath"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/yaml"

	netboxv1alpha1 "go.miloapis.com/netbox-provider/api/v1alpha1"
	"go.miloapis.com/netbox-provider/internal/fleet"
	"go.miloapis.com/netbox-provider/internal/netbox/netboxtest"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

const (
	testSystem   = "example-netbox"
	testToken    = "synthetic-token-value"
	testFacility = "example-premises-a"
)

var testNow = time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)

func newNamespace() string {
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "netbox-test-"}}
	Expect(k8sClient.Create(ctx, ns)).To(Succeed())
	return ns.Name
}

func fleetObject(kind, namespace, name string, spec map[string]any) *unstructured.Unstructured {
	obj := &unstructured.Unstructured{Object: map[string]any{"spec": spec}}
	obj.SetGroupVersionKind(fleet.GVK(kind))
	obj.SetNamespace(namespace)
	obj.SetName(name)
	return obj
}

func createFacility(namespace string) {
	Expect(k8sClient.Create(ctx, fleetObject(fleet.KindFacility, namespace, testFacility, map[string]any{
		"displayName": "Example premises A",
	}))).To(Succeed())
}

func fixtureMapping(namespace string) *netboxv1alpha1.NetBoxMapping {
	data, err := os.ReadFile(filepath.Join(netboxtest.FixtureDir(), "mapping.yaml"))
	Expect(err).NotTo(HaveOccurred())
	var m netboxv1alpha1.NetBoxMapping
	Expect(yaml.UnmarshalStrict(data, &m)).To(Succeed())
	m.Namespace = namespace
	return &m
}

func tokenSecret(namespace, key, value string) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: "netbox-token"},
		Data:       map[string][]byte{key: []byte(value)},
	}
}

func newSource(namespace, url string, mode netboxv1alpha1.SourceMode) *netboxv1alpha1.NetBoxSource {
	return &netboxv1alpha1.NetBoxSource{
		ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: "example"},
		Spec: netboxv1alpha1.NetBoxSourceSpec{
			URL:            url,
			SourceID:       testSystem,
			TokenSecretRef: netboxv1alpha1.SecretKeyReference{Name: "netbox-token"},
			MappingRef:     netboxv1alpha1.LocalObjectReference{Name: "example"},
			Target:         netboxv1alpha1.FleetTarget{Namespace: namespace, Writer: netboxv1alpha1.FleetWriterOperator},
			Mode:           mode,
		},
	}
}

// writeRecorder fails the test on any Fleet write other than a server-side
// apply by plan.FieldManager without force, and records every apply. There is
// no Create, Update, non-apply Patch, Delete, or status write.
type writeRecorder struct {
	mu      sync.Mutex
	applies []string
}

// Applies returns Kind/name for every apply, in order.
func (w *writeRecorder) Applies() []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]string{}, w.applies...)
}

func recordingFleetClient(rec *writeRecorder) client.Client {
	forbid := func(verb string) error {
		defer GinkgoRecover()
		Fail("the reconciler must never " + verb + " a Fleet record")
		return nil
	}
	c, err := client.NewWithWatch(cfg, client.Options{})
	Expect(err).NotTo(HaveOccurred())
	return interceptor.NewClient(c, interceptor.Funcs{
		Apply: func(ctx context.Context, c client.WithWatch, obj runtime.ApplyConfiguration, opts ...client.ApplyOption) error {
			applyOpts := &client.ApplyOptions{}
			applyOpts.ApplyOptions(opts)
			if applyOpts.Force != nil && *applyOpts.Force {
				return forbid("force ownership of")
			}
			if applyOpts.FieldManager != plan.FieldManager {
				return forbid("apply with a field manager other than " + plan.FieldManager + " to")
			}
			data, err := json.Marshal(obj)
			Expect(err).NotTo(HaveOccurred())
			var meta struct {
				Kind     string `json:"kind"`
				Metadata struct {
					Name string `json:"name"`
				} `json:"metadata"`
				Status any `json:"status"`
			}
			Expect(json.Unmarshal(data, &meta)).To(Succeed())
			if meta.Status != nil {
				return forbid("apply status to")
			}
			rec.mu.Lock()
			rec.applies = append(rec.applies, meta.Kind+"/"+meta.Metadata.Name)
			rec.mu.Unlock()
			return c.Apply(ctx, obj, opts...)
		},
		Create: func(context.Context, client.WithWatch, client.Object, ...client.CreateOption) error {
			return forbid("create (rather than apply)")
		},
		Update: func(context.Context, client.WithWatch, client.Object, ...client.UpdateOption) error {
			return forbid("update")
		},
		Patch: func(context.Context, client.WithWatch, client.Object, client.Patch, ...client.PatchOption) error {
			return forbid("patch (rather than apply)")
		},
		Delete: func(context.Context, client.WithWatch, client.Object, ...client.DeleteOption) error {
			return forbid("delete")
		},
		DeleteAllOf: func(context.Context, client.WithWatch, client.Object, ...client.DeleteAllOfOption) error {
			return forbid("delete")
		},
		SubResourceUpdate: func(context.Context, client.Client, string, client.Object, ...client.SubResourceUpdateOption) error {
			return forbid("update the status of")
		},
		SubResourcePatch: func(context.Context, client.Client, string, client.Object, client.Patch, ...client.SubResourcePatchOption) error {
			return forbid("patch the status of")
		},
		SubResourceApply: func(context.Context, client.Client, string, runtime.ApplyConfiguration, ...client.SubResourceApplyOption) error {
			return forbid("apply the status of")
		},
	})
}

type harness struct {
	namespace string
	server    *netboxtest.Server
	recorder  *writeRecorder
	r         *NetBoxSourceReconciler
}

// newHarness creates a namespace with the mapped Facility, the fixture
// mapping, and a synthetic NetBox. It does not create the Secret or source.
func newHarness() *harness {
	data, err := netboxtest.LoadFixture()
	Expect(err).NotTo(HaveOccurred())
	h := &harness{namespace: newNamespace(), server: netboxtest.New(data), recorder: &writeRecorder{}}
	DeferCleanup(h.server.Close)
	createFacility(h.namespace)
	Expect(k8sClient.Create(ctx, fixtureMapping(h.namespace))).To(Succeed())
	h.r = &NetBoxSourceReconciler{
		Client:     k8sClient,
		APIReader:  k8sClient,
		Fleet:      recordingFleetClient(h.recorder),
		HTTPClient: h.server.Client(),
		Now:        func() time.Time { return testNow },
	}
	return h
}

func (h *harness) withSourceAndToken(mode netboxv1alpha1.SourceMode) *harness {
	Expect(k8sClient.Create(ctx, tokenSecret(h.namespace, "token", testToken))).To(Succeed())
	Expect(k8sClient.Create(ctx, newSource(h.namespace, h.server.RootURL(), mode))).To(Succeed())
	return h
}

func (h *harness) reconcile() (reconcile.Result, error) {
	return h.r.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Namespace: h.namespace, Name: "example"}})
}

func (h *harness) source() *netboxv1alpha1.NetBoxSource {
	var s netboxv1alpha1.NetBoxSource
	Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: h.namespace, Name: "example"}, &s)).To(Succeed())
	return &s
}

func (h *harness) list(kind string) []unstructured.Unstructured {
	list := &unstructured.UnstructuredList{}
	list.SetGroupVersionKind(fleet.ListGVK(kind))
	Expect(k8sClient.List(ctx, list, client.InNamespace(h.namespace))).To(Succeed())
	return list.Items
}

func condition(s *netboxv1alpha1.NetBoxSource, conditionType string) metav1.Condition {
	for _, c := range s.Status.Conditions {
		if c.Type == conditionType {
			return c
		}
	}
	return metav1.Condition{}
}

func recordFor(s *netboxv1alpha1.NetBoxSource, source, kind string) netboxv1alpha1.RecordStatus {
	for _, r := range s.Status.Records {
		if r.Source == source && (kind == "" || len(r.Target) > len(kind) && r.Target[:len(kind)+1] == kind+"/") {
			return r
		}
	}
	return netboxv1alpha1.RecordStatus{}
}
