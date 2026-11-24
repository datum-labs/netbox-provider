// SPDX-License-Identifier: AGPL-3.0-only

package controller

import (
	"github.com/prometheus/client_golang/prometheus"
	"k8s.io/apimachinery/pkg/types"

	"go.miloapis.com/instrumentation/labels"
	"go.miloapis.com/instrumentation/metrics"
)

// sourceLabels name the NetBoxSource a series describes.
var sourceLabels = []string{labels.ResourceNamespace, labels.ResourceName}

var (
	interimDevicesGauge = metrics.NewGaugeVec(prometheus.GaugeOpts{
		Name: "netbox_provider_interim_devices",
		Help: "Devices the last reconcile of a NetBoxSource left holding both the old and new IDs of moved components, because a Cable rewrite they wait for has not landed.",
	}, sourceLabels)
	undeliveredCableReferencesGauge = metrics.NewGaugeVec(prometheus.GaugeOpts{
		Name: "netbox_provider_undelivered_cable_references",
		Help: "Cable endpoints in Fleet that name a component their Device no longer has, among the Devices a NetBoxSource plans, after its last reconcile.",
	}, sourceLabels)
)

func setSourceMetrics(source types.NamespacedName, interim, undelivered int) {
	interimDevicesGauge.WithLabelValues(source.Namespace, source.Name).Set(float64(interim))
	undeliveredCableReferencesGauge.WithLabelValues(source.Namespace, source.Name).Set(float64(undelivered))
}

// forgetSourceMetrics drops a deleted source's series.
func forgetSourceMetrics(source types.NamespacedName) {
	interimDevicesGauge.DeleteLabelValues(source.Namespace, source.Name)
	undeliveredCableReferencesGauge.DeleteLabelValues(source.Namespace, source.Name)
}
