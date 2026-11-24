// SPDX-License-Identifier: AGPL-3.0-only

package controller

import (
	"fmt"
	"sort"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	netboxv1alpha1 "go.miloapis.com/netbox-provider/api/v1alpha1"
	"go.miloapis.com/netbox-provider/internal/fleet"
	"go.miloapis.com/netbox-provider/internal/plan"
)

// reportCableReferences records, in status, the condition and the metrics,
// the Cables that name a component their Device no longer has and the
// Devices left interim. interim holds, by Device key, the Devices this
// reconcile left interim and the Cables each waits for. In Observe mode
// nothing is written, so the planned writes are assumed to land and no Device
// is interim.
func (r *NetBoxSourceReconciler) reportCableReferences(source *netboxv1alpha1.NetBoxSource, p *plan.Plan, existing plan.Existing,
	interim map[string][]string, uncertain int, set func(string, metav1.ConditionStatus, string, string)) {
	observe := source.Spec.Mode != netboxv1alpha1.SourceModeCreate
	written := func(c *plan.Candidate) bool {
		switch c.Action {
		case actionCreated, actionUpdated, plan.ActionUnchanged:
			return true
		case plan.ActionCreate, plan.ActionUpdate:
			return observe
		}
		return false
	}
	held := map[string]bool{}
	for key := range interim {
		held[key] = true
	}
	refs := plan.UndeliveredReferences(p, existing, written, held)

	since := map[string]metav1.Time{}
	for _, d := range source.Status.InterimDevices {
		since[d.Name] = d.Since
	}
	var devices []netboxv1alpha1.InterimDevice
	for _, c := range p.Candidates {
		waiting, ok := interim[c.Key()]
		if !ok {
			continue
		}
		d := netboxv1alpha1.InterimDevice{Name: c.Name, Source: c.Source, Message: truncate(c.Message, 256), Since: metav1.NewTime(r.now())}
		if previous, ok := since[c.Name]; ok {
			d.Since = previous
		}
		if len(waiting) > maxWaitingFor {
			waiting = waiting[:maxWaitingFor]
		}
		d.WaitingFor = waiting
		devices = append(devices, d)
	}
	sort.Slice(devices, func(i, j int) bool { return devices[i].Name < devices[j].Name })
	status := &source.Status
	status.InterimDeviceCount = int32(len(devices))
	status.InterimDevicesTruncated = len(devices) > maxInterimDevices
	if len(devices) > maxInterimDevices {
		devices = devices[:maxInterimDevices]
	}
	status.InterimDevices = devices
	status.UndeliveredCableReferences = int32(len(refs))
	setSourceMetrics(client.ObjectKeyFromObject(source), int(status.InterimDeviceCount), len(refs))

	interimMessage := ""
	if n := status.InterimDeviceCount; n > 0 {
		var names []string
		for _, d := range devices {
			names = append(names, fleet.KindDevice+"/"+d.Name)
		}
		interimMessage = fmt.Sprintf("%d Devices still hold old components until the Cables they wait for are written: %s; see status.interimDevices", n, listNames(names, int(n)))
	}
	switch {
	case len(refs) > 0:
		var names []string
		for _, ref := range refs {
			names = append(names, fmt.Sprintf("%s/%s -> %s/%s component %s", fleet.KindCable, ref.Cable, fleet.KindDevice, ref.Device, ref.ComponentID))
		}
		verb := "name"
		if observe {
			verb = "would name, once this plan is written,"
		}
		message := fmt.Sprintf("%d Cable endpoints that this plan does not write %s components their Devices no longer have: %s", len(refs), verb, listNames(names, len(names)))
		if interimMessage != "" {
			message += "; " + interimMessage
		}
		set(ConditionCableReferencesDelivered, metav1.ConditionFalse, ReasonUndeliveredReferences, message)
	case interimMessage != "":
		set(ConditionCableReferencesDelivered, metav1.ConditionFalse, ReasonInterimDevices, interimMessage)
	case uncertain > 0:
		set(ConditionCableReferencesDelivered, metav1.ConditionUnknown, ReasonApplyUncertain, "some writes have an unknown outcome")
	default:
		set(ConditionCableReferencesDelivered, metav1.ConditionTrue, ReasonDelivered, "every Cable names components its Device has")
	}
}

// listNames joins the first namedInMessage names and counts the rest of
// total.
func listNames(names []string, total int) string {
	if len(names) > namedInMessage {
		names = names[:namedInMessage]
	}
	if total > len(names) {
		return fmt.Sprintf("%s and %d more", strings.Join(names, ", "), total-len(names))
	}
	return strings.Join(names, ", ")
}
