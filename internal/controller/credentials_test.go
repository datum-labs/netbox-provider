// SPDX-License-Identifier: AGPL-3.0-only

package controller

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/go-logr/logr/funcr"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	netboxv1alpha1 "go.miloapis.com/netbox-provider/api/v1alpha1"
	"go.miloapis.com/netbox-provider/internal/fleet"
	"go.miloapis.com/netbox-provider/internal/netbox/netboxtest"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// fixtureOOBPassword is the synthetic oob_password custom field value on a
// fixture device.
const fixtureOOBPassword = "synthetic-oob-secret-7f3a"

var _ = Describe("credential custom fields", func() {
	It("never reach the snapshot, its digest input, the plan, Fleet, status or logs", func() {
		raw, err := os.ReadFile(filepath.Join(netboxtest.FixtureDir(), "dcim_devices.json"))
		Expect(err).NotTo(HaveOccurred())
		Expect(string(raw)).To(ContainSubstring(`"oob_password": "`+fixtureOOBPassword+`"`), "NetBox serves the credential")

		h := newHarness().withSourceAndToken(netboxv1alpha1.SourceModeCreate)
		var mu sync.Mutex
		var logs strings.Builder
		logger := funcr.New(func(prefix, args string) {
			mu.Lock()
			defer mu.Unlock()
			logs.WriteString(prefix + " " + args + "\n")
		}, funcr.Options{Verbosity: 10})
		_, err = h.r.Reconcile(log.IntoContext(ctx, logger), reconcile.Request{NamespacedName: types.NamespacedName{Namespace: h.namespace, Name: "example"}})
		Expect(err).NotTo(HaveOccurred())

		snapshot, err := netboxSnapshot(h)
		Expect(err).NotTo(HaveOccurred())
		// The digest is the sha256 of exactly this encoding.
		digestInput, err := json.Marshal(snapshot)
		Expect(err).NotTo(HaveOccurred())
		var planned []any
		p := fixturePlan(h)
		for _, c := range p.Candidates {
			planned = append(planned, c.Object.Object, c.Message)
		}
		planned = append(planned, p.Records, p.Retained)
		planJSON, err := json.Marshal(planned)
		Expect(err).NotTo(HaveOccurred())
		var written []any
		for _, kind := range fleet.WriteKinds {
			for _, obj := range h.list(kind) {
				written = append(written, obj.Object)
			}
		}
		Expect(written).NotTo(BeEmpty())
		fleetRecords, err := json.Marshal(written)
		Expect(err).NotTo(HaveOccurred())
		status, err := json.Marshal(h.source().Status)
		Expect(err).NotTo(HaveOccurred())
		mu.Lock()
		captured := logs.String()
		mu.Unlock()
		Expect(captured).To(ContainSubstring("reconciled NetBox source"), "the controller's logs were captured")

		for name, content := range map[string]string{
			"decoded snapshot":      fmt.Sprintf("%+v", *snapshot),
			"snapshot digest input": string(digestInput),
			"plan":                  string(planJSON),
			"Fleet records":         string(fleetRecords),
			"NetBoxSource status":   string(status),
			"controller logs":       captured,
		} {
			for _, forbidden := range []string{fixtureOOBPassword, "oob_password", "custom_fields", "must-not-be-retained"} {
				Expect(content).NotTo(ContainSubstring(forbidden), "%s carries %q", name, forbidden)
			}
		}
	})
})
