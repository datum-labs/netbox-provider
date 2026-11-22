// SPDX-License-Identifier: AGPL-3.0-only

package controller

import (
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	netboxv1alpha1 "go.miloapis.com/netbox-provider/api/v1alpha1"
	"go.miloapis.com/netbox-provider/internal/netbox"
)

func TestReadLimitsOverrideOnlyTheirFields(t *testing.T) {
	if got := readLimits(nil); got != netbox.DefaultLimits {
		t.Fatalf("defaults %+v", got)
	}
	got := readLimits(&netboxv1alpha1.ReadLimits{MaxRecordsPerCollection: 500000})
	want := netbox.DefaultLimits
	want.Records, want.Pages = 500000, 2501
	if got != want {
		t.Fatalf("records only: %+v", got)
	}
	got = readLimits(&netboxv1alpha1.ReadLimits{MaxMiBPerCollection: 256, CollectionTimeout: &metav1.Duration{Duration: 10 * time.Minute}})
	want = netbox.DefaultLimits
	want.CollectionBytes, want.CollectionTimeout = 256<<20, 10*time.Minute
	if got != want {
		t.Fatalf("bytes and timeout: %+v", got)
	}
}
