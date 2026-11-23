// SPDX-License-Identifier: AGPL-3.0-only

package plan_test

import (
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	netboxv1alpha1 "go.miloapis.com/netbox-provider/api/v1alpha1"
	"go.miloapis.com/netbox-provider/internal/fleet"
	"go.miloapis.com/netbox-provider/internal/netbox"
	"go.miloapis.com/netbox-provider/internal/plan"
)

func ptr[T any](v T) *T { return &v }

func term(objectType string, id int64) []netbox.Termination {
	return []netbox.Termination{{ObjectType: objectType, ObjectID: id}}
}

func addComponent(in plan.Input, collection string, c netbox.Component) {
	in.Snapshot.Components[collection] = append(in.Snapshot.Components[collection], c)
}

func device(in plan.Input, id int64) *netbox.Device {
	for i := range in.Snapshot.Devices {
		if in.Snapshot.Devices[i].ID == id {
			return &in.Snapshot.Devices[i]
		}
	}
	return nil
}

func TestComponentsAndInternalMappings(t *testing.T) {
	p := plan.Build(input(t))
	server := candidate(t, p, fleet.KindDevice, "dcim/devices/1000")
	components := field(t, server, "components").([]any)
	var ids []string
	for _, c := range components {
		ids = append(ids, c.(map[string]any)["id"].(string))
	}
	// Each component takes the ID of the device type's template it was made from.
	if !equalJSON(ids, []string{"interface-templates-700", "interface-templates-701", "power-port-templates-710"}) {
		t.Errorf("server components %v; logical interfaces are not components", ids)
	}
	eth0 := components[0].(map[string]any)
	if eth0["name"] != "eth0" || eth0["type"] != "NetworkPort" || eth0["connector"] != "1000base-t" {
		t.Errorf("eth0 %v", eth0)
	}
	pdu := candidate(t, p, fleet.KindDevice, "dcim/devices/1008")
	want := []any{
		map[string]any{"from": map[string]any{"componentID": "power-outlets-3100", "position": float64(1)}, "to": map[string]any{"componentID": "power-ports-3010", "position": float64(1)}},
		map[string]any{"from": map[string]any{"componentID": "power-outlets-3101", "position": float64(1)}, "to": map[string]any{"componentID": "power-ports-3010", "position": float64(1)}},
	}
	if !equalJSON(field(t, pdu, "internalMappings"), want) {
		t.Errorf("pdu internal mappings %v", field(t, pdu, "internalMappings"))
	}
}

func TestFrontPortsMapToExplicitRearPositions(t *testing.T) {
	setup := func(t *testing.T, rearPositions *int64, fronts ...netbox.Component) map[string]outcome {
		in := input(t)
		addComponent(in, netbox.CollectionRearPorts, netbox.Component{ID: 8000, Device: &netbox.Ref{ID: 1007}, Name: "rear", Type: &netbox.Choice{Value: "lc"}, Positions: rearPositions})
		for _, f := range fronts {
			addComponent(in, netbox.CollectionFrontPorts, f)
		}
		p := plan.Build(in)
		if got := outcomes(p); got["Device dcim/devices/1007"].action == plan.ActionCreate {
			sw := candidate(t, p, fleet.KindDevice, "dcim/devices/1007")
			mappings, _, _ := unstructured.NestedSlice(sw.Object.Object, "spec", "internalMappings")
			// The fixture's own front port contributes two mappings.
			if len(mappings) != len(fronts)+2 {
				t.Errorf("mappings %v", mappings)
			}
		}
		return outcomes(p)
	}
	front := func(id, position int64) netbox.Component {
		return netbox.Component{ID: id, Device: &netbox.Ref{ID: 1007}, Name: "front", Type: &netbox.Choice{Value: "lc"}, RearPort: &netbox.Ref{ID: 8000}, RearPortPosition: ptr(position)}
	}
	if got := setup(t, ptr[int64](2), front(8100, 1), front(8101, 2)); got["Device dcim/devices/1007"].action != plan.ActionCreate {
		t.Errorf("valid front to rear mapping: %+v", got["Device dcim/devices/1007"])
	}
	for name, got := range map[string]map[string]outcome{
		"rear port without positions":     setup(t, nil, front(8100, 1)),
		"position beyond the rear port":   setup(t, ptr[int64](1), front(8100, 2)),
		"two fronts on one rear position": setup(t, ptr[int64](2), front(8100, 1), front(8101, 1)),
		"front port without a rear port":  setup(t, ptr[int64](2), netbox.Component{ID: 8100, Device: &netbox.Ref{ID: 1007}, Name: "front"}),
	} {
		t.Run(name, func(t *testing.T) {
			if got["record dcim/devices/1007"].reason != plan.ReasonComponentsInvalid || got["DeviceClaim dcim/devices/1007"].action != plan.ActionCreate {
				t.Fatalf("device %+v claim %+v", got["record dcim/devices/1007"], got["DeviceClaim dcim/devices/1007"])
			}
			// The switch's cable cannot reference a Device that is not created.
			if got["record dcim/cables/5000"].reason != plan.ReasonEndpointUnavailable {
				t.Fatalf("cable %+v", got["record dcim/cables/5000"])
			}
		})
	}
}

func TestRackUnitsPlacement(t *testing.T) {
	claim := candidate(t, plan.Build(input(t)), fleet.KindDeviceClaim, "dcim/devices/1007")
	want := map[string]any{"scope": "RackUnits", "rackRef": map[string]any{"name": candidate(t, plan.Build(input(t)), fleet.KindRack, "dcim/racks/100").Name}, "position": float64(40), "height": float64(1), "face": "front"}
	if !equalJSON(field(t, claim, "placement"), want) {
		t.Errorf("placement %v", field(t, claim, "placement"))
	}
	for name, mutate := range map[string]func(*netbox.Device){
		"no position":         func(d *netbox.Device) { d.Position = nil },
		"fractional position": func(d *netbox.Device) { d.Position = ptr(netbox.Decimal(40.5)) },
		"no face":             func(d *netbox.Device) { d.Face = nil },
		"beyond the rack":     func(d *netbox.Device) { d.Position = ptr(netbox.Decimal(100)); d.DeviceType = &netbox.Ref{ID: 505} },
		"zero-unit type":      func(d *netbox.Device) { d.DeviceType = &netbox.Ref{ID: 504} },
	} {
		t.Run(name, func(t *testing.T) {
			in := input(t)
			in.Snapshot.DeviceTypes = append(in.Snapshot.DeviceTypes, netbox.DeviceType{ID: 505, Model: "Example 2U Switch", UHeight: ptr(netbox.Decimal(2))})
			mutate(device(in, 1007))
			got := outcomes(plan.Build(in))
			if got["record dcim/devices/1007"].reason != plan.ReasonRackUnitsUnavailable {
				t.Fatalf("%+v", got["record dcim/devices/1007"])
			}
			if _, ok := got["DeviceClaim dcim/devices/1007"]; ok {
				t.Fatal("a claim was planned without exact rack units")
			}
		})
	}
}

func TestCablesNeverReferenceRecordsThatAreNotCreated(t *testing.T) {
	// The switch collides with an existing Device, so it is quarantined and
	// its claim and cable are blocked rather than left dangling.
	existing := object(fleet.KindDevice, "legacy", map[string]any{"claimRef": map[string]any{"name": "legacy-slot"}, "identity": map[string]any{"model": "x", "serial": "SYNTH-CHASSIS-0007"}}, nil)
	got := outcomes(plan.Build(withExisting(input(t), existing)))
	if got["Device dcim/devices/1007"].reason != plan.ReasonIdentityCollision || got["Cable dcim/cables/5000"] != (outcome{plan.ActionUnresolved, plan.ReasonDependencyBlocked}) {
		t.Fatalf("device %+v cable %+v", got["Device dcim/devices/1007"], got["Cable dcim/cables/5000"])
	}

	// A planned feed's rack that is not created blocks the feed and its cable.
	in := input(t)
	in.Existing[fleet.KindRack] = []unstructured.Unstructured{object(fleet.KindRack, candidate(t, plan.Build(input(t)), fleet.KindRack, "dcim/racks/100").Name, map[string]any{"displayName": "other"}, nil)}
	got = outcomes(plan.Build(in))
	if got["Rack dcim/racks/100"].reason != plan.ReasonNameConflict || got["PowerFeed dcim/power-feeds/6000"].reason != plan.ReasonDependencyBlocked ||
		got["Cable dcim/cables/5002"].reason != plan.ReasonDependencyBlocked {
		t.Fatalf("rack %+v feed %+v cable %+v", got["Rack dcim/racks/100"], got["PowerFeed dcim/power-feeds/6000"], got["Cable dcim/cables/5002"])
	}

	// Unresolved and excluded devices have no Device for a cable to reach.
	got = outcomes(plan.Build(input(t)))
	for _, source := range []string{"dcim/cables/5003", "dcim/cables/5004"} {
		if got["record "+source].reason != plan.ReasonEndpointUnavailable {
			t.Errorf("%s %+v", source, got["record "+source])
		}
	}
}

func TestCableMedia(t *testing.T) {
	cases := map[string]struct {
		cable  netbox.Cable
		mutate func(*plan.Input)
		want   outcome
		medium string
	}{
		"reviewed data type": {
			cable: netbox.Cable{ID: 9000, Type: "cat6", ATerminations: term("dcim.interface", 2000), BTerminations: term("dcim.interface", 2013)},
			want:  outcome{plan.ActionCreate, plan.ReasonNew}, medium: "Copper",
		},
		"power from endpoints": {
			cable: netbox.Cable{ID: 9000, Type: "cat6", ATerminations: term("dcim.powerport", 3000), BTerminations: term("dcim.poweroutlet", 3101)},
			want:  outcome{plan.ActionCreate, plan.ReasonNew}, medium: "Power",
		},
		"console from endpoints": {
			cable: netbox.Cable{ID: 9000, ATerminations: term("dcim.consoleport", 4000), BTerminations: term("dcim.consoleserverport", 4100)},
			mutate: func(in *plan.Input) {
				addComponent(*in, netbox.CollectionConsoleServerPorts, netbox.Component{ID: 4100, Device: &netbox.Ref{ID: 1000}, Name: "ttyS0", Type: &netbox.Choice{Value: "rj-45"}})
			},
			want: outcome{plan.ActionCreate, plan.ReasonNew}, medium: "Console",
		},
		"untyped with a reviewed default": {
			cable:  netbox.Cable{ID: 9000, ATerminations: term("dcim.interface", 2000), BTerminations: term("dcim.interface", 2013)},
			mutate: func(in *plan.Input) { in.Mapping.UntypedCableMedium = netboxv1alpha1.CableMediumUnknown },
			want:   outcome{plan.ActionCreate, plan.ReasonNew}, medium: "Unknown",
		},
		"untyped without a default": {
			cable: netbox.Cable{ID: 9000, ATerminations: term("dcim.interface", 2000), BTerminations: term("dcim.interface", 2013)},
			want:  outcome{"Unresolved", plan.ReasonCableMediumUnreviewed},
		},
		"outlet to outlet": {
			cable: netbox.Cable{ID: 9000, ATerminations: term("dcim.poweroutlet", 3100), BTerminations: term("dcim.poweroutlet", 3101)},
			want:  outcome{"Unresolved", plan.ReasonInvalidPowerCable},
		},
		"console to data": {
			cable: netbox.Cable{ID: 9000, Type: "cat6", ATerminations: term("dcim.consoleport", 4000), BTerminations: term("dcim.interface", 2000)},
			want:  outcome{"Unresolved", plan.ReasonMixedEndpoints},
		},
		"several terminations": {
			cable: netbox.Cable{ID: 9000, Type: "cat6", ATerminations: append(term("dcim.interface", 2000), term("dcim.interface", 2001)...), BTerminations: term("dcim.interface", 2013)},
			want:  outcome{"Unsupported", plan.ReasonMultiTermination},
		},
		"multi-position rear port": {
			cable: netbox.Cable{ID: 9000, Type: "cat6", ATerminations: term("dcim.rearport", 8000), BTerminations: term("dcim.interface", 2000)},
			mutate: func(in *plan.Input) {
				addComponent(*in, netbox.CollectionRearPorts, netbox.Component{ID: 8000, Device: &netbox.Ref{ID: 1007}, Name: "rear", Positions: ptr[int64](4)})
			},
			want: outcome{"Unresolved", plan.ReasonMultiPosition},
		},
		"endpoint outside the scope": {
			cable: netbox.Cable{ID: 9000, Type: "cat6", ATerminations: term("dcim.interface", 2000), BTerminations: term("dcim.interface", 99999)},
			want:  outcome{"Unresolved", plan.ReasonEndpointUnavailable},
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			in := input(t)
			if tc.mutate != nil {
				tc.mutate(&in)
			}
			tc.cable.Status = &netbox.Choice{Value: "connected"}
			in.Snapshot.Cables = []netbox.Cable{tc.cable}
			p := plan.Build(in)
			got := outcomes(p)
			key := "record dcim/cables/9000"
			if tc.want.action == plan.ActionCreate {
				key = "Cable dcim/cables/9000"
			}
			if got[key] != tc.want {
				t.Fatalf("%+v, want %+v (all %v)", got[key], tc.want, got)
			}
			if tc.medium != "" {
				c := candidate(t, p, fleet.KindCable, "dcim/cables/9000")
				if field(t, c, "medium") != tc.medium || len(c.DependsOn) != 2 {
					t.Fatalf("medium %v deps %v", field(t, c, "medium"), c.DependsOn)
				}
			}
		})
	}
}

func TestPowerFeedAndCableSpecs(t *testing.T) {
	p := plan.Build(input(t))
	rack := candidate(t, p, fleet.KindRack, "dcim/racks/100")
	feed := candidate(t, p, fleet.KindPowerFeed, "dcim/power-feeds/6000")
	want := map[string]any{
		"displayName":  "Example Feed A",
		"facilityRef":  map[string]any{"name": "example-premises-a"},
		"rackRef":      map[string]any{"name": rack.Name},
		"supply":       map[string]any{"type": "AC", "voltageV": float64(208), "currentA": float64(30), "phase": "single-phase"},
		"terminations": []any{map[string]any{"id": "output", "name": "output", "type": "PowerOutlet"}},
	}
	if !equalJSON(feed.Object.Object["spec"], want) {
		t.Errorf("feed spec %v", feed.Object.Object["spec"])
	}
	cable := candidate(t, p, fleet.KindCable, "dcim/cables/5002")
	lanes := []any{map[string]any{
		"id": "lane-1",
		"a":  map[string]any{"deviceRef": map[string]any{"name": candidate(t, p, fleet.KindDevice, "dcim/devices/1008").Name}, "componentID": "power-ports-3010", "position": float64(1)},
		"b":  map[string]any{"powerFeedRef": map[string]any{"name": feed.Name}, "componentID": "output", "position": float64(1)},
	}}
	if field(t, cable, "medium") != "Power" || !equalJSON(field(t, cable, "lanes"), lanes) || field(t, cable, "displayName") != cable.Name {
		t.Errorf("cable spec %v", cable.Object.Object["spec"])
	}
	if field(t, candidate(t, p, fleet.KindCable, "dcim/cables/5001"), "displayName") != "Example power A" {
		t.Error("a cable label is its display name")
	}
}

func TestCablesToUnidentifiedDevicesAreCreatable(t *testing.T) {
	p := plan.Build(input(t))
	unidentified := candidate(t, p, fleet.KindDevice, "dcim/devices/1002")
	if field(t, unidentified, "identity", "unidentified") != true {
		t.Fatalf("identity %v", field(t, unidentified, "identity"))
	}
	cable := candidate(t, p, fleet.KindCable, "dcim/cables/5008")
	lane := field(t, cable, "lanes").([]any)[0].(map[string]any)
	if cable.Action != plan.ActionCreate || lane["a"].(map[string]any)["deviceRef"].(map[string]any)["name"] != unidentified.Name ||
		lane["a"].(map[string]any)["componentID"] != "interface-templates-700" {
		t.Fatalf("cable %s %v", cable.Action, lane)
	}
	found := false
	for _, dep := range cable.DependsOn {
		found = found || dep == unidentified.Key()
	}
	if !found {
		t.Fatalf("cable deps %v", cable.DependsOn)
	}
	// A provider-hosted server has no Device, so its cable stays unresolved.
	if got := outcomes(p)["record dcim/cables/5003"]; got.reason != plan.ReasonEndpointUnavailable {
		t.Fatalf("%+v", got)
	}
}

func TestFrontPortPositionMappings(t *testing.T) {
	// NetBox 4.5 and later maps each front port position separately. The rear
	// port was made from a template and the front port was not.
	sw := candidate(t, plan.Build(input(t)), fleet.KindDevice, "dcim/devices/1007")
	want := []any{
		map[string]any{"from": map[string]any{"componentID": "front-ports-2101", "position": float64(1)}, "to": map[string]any{"componentID": "rear-port-templates-720", "position": float64(1)}},
		map[string]any{"from": map[string]any{"componentID": "front-ports-2101", "position": float64(2)}, "to": map[string]any{"componentID": "rear-port-templates-720", "position": float64(2)}},
	}
	if !equalJSON(field(t, sw, "internalMappings"), want) {
		t.Fatalf("internal mappings %v", field(t, sw, "internalMappings"))
	}
	for name, mutate := range map[string]func(*netbox.Component){
		"position beyond the front port": func(c *netbox.Component) { c.RearPorts[1].Position = ptr[int64](3) },
		"one position mapped twice":      func(c *netbox.Component) { c.RearPorts[1].Position = ptr[int64](1) },
		"one rear position mapped twice": func(c *netbox.Component) { c.RearPorts[1].RearPortPosition = ptr[int64](1) },
		"rear port on another device":    func(c *netbox.Component) { id := netbox.ID(3000); c.RearPorts[1].RearPort = &id },
		"incomplete mapping":             func(c *netbox.Component) { c.RearPorts[0].RearPortPosition = nil },
	} {
		t.Run(name, func(t *testing.T) {
			in := input(t)
			mutate(&in.Snapshot.Components[netbox.CollectionFrontPorts][0])
			got := outcomes(plan.Build(in))
			if got["record dcim/devices/1007"].reason != plan.ReasonComponentsInvalid {
				t.Fatalf("%+v", got["record dcim/devices/1007"])
			}
		})
	}
}

func TestBMCEndpoint(t *testing.T) {
	endpoint := func(address string, vrf *netbox.Ref, allowed ...int64) (string, string) {
		vrfs := map[int64]bool{}
		for _, v := range allowed {
			vrfs[v] = true
		}
		ips := map[int64]netbox.IPAddress{1: {ID: 1, Address: address, VRF: vrf}}
		return plan.BMCEndpoint(netbox.Device{OOBIP: &netbox.IPAddressRef{ID: 1, Address: address}}, ips, vrfs)
	}
	for address, want := range map[string]string{
		"192.0.2.10/24":         "https://192.0.2.10:443",
		"192.0.2.10":            "https://192.0.2.10:443",
		"::ffff:192.0.2.10/120": "https://192.0.2.10:443",
		"2001:db8::7/64":        "",
		"0.0.0.0/0":             "",
		"0.1.2.3/8":             "",
		"240.0.0.1/4":           "",
		"250.1.2.3/24":          "",
		"255.255.255.255/32":    "",
		"127.0.0.1/8":           "",
		"169.254.0.5/16":        "",
		"224.0.0.5/24":          "",
		"not-an-address":        "",
		"":                      "",
	} {
		got, problem := endpoint(address, nil)
		if got != want || (want == "") == (problem == "") {
			t.Errorf("%q: endpoint %q problem %q, want %q", address, got, problem, want)
		}
	}
	if got, problem := endpoint("192.0.2.10/24", &netbox.Ref{ID: 5}); got != "" || !strings.Contains(problem, "VRF 5") {
		t.Errorf("an address in a VRF the mapping does not allow was used: %q %q", got, problem)
	}
	if got, _ := endpoint("192.0.2.10/24", &netbox.Ref{ID: 5}, 5); got != "https://192.0.2.10:443" {
		t.Errorf("an allowed VRF: %q", got)
	}
	if got, problem := plan.BMCEndpoint(netbox.Device{}, nil, nil); got != "" || problem == "" {
		t.Errorf("no out-of-band IP: %q %q", got, problem)
	}
	if got, problem := plan.BMCEndpoint(netbox.Device{OOBIP: &netbox.IPAddressRef{ID: 2, Address: "192.0.2.10/24"}}, nil, nil); got != "" || !strings.Contains(problem, "not in the snapshot") {
		t.Errorf("the nested address is never used without its record: %q %q", got, problem)
	}
}

func TestBMCInAnotherVRFIsNotWritten(t *testing.T) {
	in := input(t)
	in.Snapshot.IPAddresses[0].VRF = &netbox.Ref{ID: 5}
	server := candidate(t, plan.Build(in), fleet.KindDevice, "dcim/devices/1000")
	if field(t, server, "bmc") != nil || !strings.Contains(server.Message, "VRF 5") || server.Action != plan.ActionCreate {
		t.Fatalf("bmc %v message %q", field(t, server, "bmc"), server.Message)
	}
	in.Mapping.BMC.VRFIDs = []int64{5}
	if field(t, candidate(t, plan.Build(in), fleet.KindDevice, "dcim/devices/1000"), "bmc", "endpoint") != "https://192.0.2.10:443" {
		t.Fatal("an allowed VRF's address was not used")
	}
}

func TestDevicesNeverShareABMCEndpoint(t *testing.T) {
	// Two planned Devices with the same out-of-band address, as from two VRFs.
	in := input(t)
	in.Mapping.BMC.RoleSlugs = append(in.Mapping.BMC.RoleSlugs, "switch")
	device(in, 1007).OOBIP = &netbox.IPAddressRef{ID: 9000}
	p := plan.Build(in)
	server, sw := candidate(t, p, fleet.KindDevice, "dcim/devices/1000"), candidate(t, p, fleet.KindDevice, "dcim/devices/1007")
	for _, c := range []*plan.Candidate{server, sw} {
		if c.Action != plan.ActionQuarantined || c.Reason != plan.ReasonBMCCollision ||
			!strings.Contains(c.Message, server.Name) || !strings.Contains(c.Message, sw.Name) {
			t.Fatalf("%s %+v", c.Source, c)
		}
	}

	// An existing Device from another writer with the same endpoint.
	other := object(fleet.KindDevice, "operator-recorded", map[string]any{
		"claimRef": map[string]any{"name": "operator-slot"}, "identity": map[string]any{"model": "x", "serial": "OTHER"},
		"bmc": map[string]any{"endpoint": "https://192.0.2.10:443"},
	}, nil)
	c := candidate(t, plan.Build(withExisting(input(t), other)), fleet.KindDevice, "dcim/devices/1000")
	if c.Reason != plan.ReasonBMCCollision || !strings.Contains(c.Message, "operator-recorded") || !strings.Contains(c.Message, c.Name) {
		t.Fatalf("%+v", c)
	}
}

func TestBMCIsWrittenOnlyForListedRolesWithHardwareIdentity(t *testing.T) {
	p := plan.Build(input(t))
	server := candidate(t, p, fleet.KindDevice, "dcim/devices/1000")
	if field(t, server, "bmc", "endpoint") != "https://192.0.2.10:443" || server.Message != "" {
		t.Fatalf("server bmc %v message %q", field(t, server, "bmc"), server.Message)
	}
	unidentified := candidate(t, p, fleet.KindDevice, "dcim/devices/1002")
	if field(t, unidentified, "bmc") != nil || !strings.Contains(unidentified.Message, "without hardware identity") || unidentified.Action != plan.ActionCreate {
		t.Fatalf("unidentified bmc %v message %q", field(t, unidentified, "bmc"), unidentified.Message)
	}
	// A switch's out-of-band IP is a management address, not a BMC.
	if field(t, candidate(t, p, fleet.KindDevice, "dcim/devices/1007"), "bmc") != nil {
		t.Fatal("a BMC was written for a role the mapping does not list")
	}
	in := input(t)
	in.Mapping.BMC = nil
	if field(t, candidate(t, plan.Build(in), fleet.KindDevice, "dcim/devices/1000"), "bmc") != nil {
		t.Fatal("a BMC was written without the mapping's opt-in")
	}
	in = input(t)
	in.Mapping.BMC.RoleSlugs = []string{"server", "switch"}
	sw := candidate(t, plan.Build(in), fleet.KindDevice, "dcim/devices/1007")
	if field(t, sw, "bmc") != nil || !strings.Contains(sw.Message, "IPv6") || sw.Action != plan.ActionCreate {
		t.Fatalf("IPv6 bmc %v message %q", field(t, sw, "bmc"), sw.Message)
	}
}

func TestComponentsThatCannotBeRecordedExactly(t *testing.T) {
	for name, mutate := range map[string]func(*plan.Input){
		"more than 256 components": func(in *plan.Input) {
			for i := int64(0); i < 254; i++ {
				addComponent(*in, netbox.CollectionInterfaces, netbox.Component{ID: 20000 + i, Device: &netbox.Ref{ID: 1000}, Name: "extra", Type: &netbox.Choice{Value: "1000base-t"}})
			}
		},
		"a name longer than Fleet allows": func(in *plan.Input) {
			addComponent(*in, netbox.CollectionInterfaces, netbox.Component{ID: 20000, Device: &netbox.Ref{ID: 1000}, Name: strings.Repeat("n", 254)})
		},
		"an outlet fed by another device's power port": func(in *plan.Input) {
			addComponent(*in, netbox.CollectionPowerOutlets, netbox.Component{ID: 20000, Device: &netbox.Ref{ID: 1000}, Name: "outlet", PowerPort: &netbox.Ref{ID: 3010}})
		},
	} {
		t.Run(name, func(t *testing.T) {
			in := input(t)
			mutate(&in)
			got := outcomes(plan.Build(in))
			if got["record dcim/devices/1000"].reason != plan.ReasonComponentsInvalid || got["DeviceClaim dcim/devices/1000"].action != plan.ActionCreate {
				t.Fatalf("device %+v claim %+v", got["record dcim/devices/1000"], got["DeviceClaim dcim/devices/1000"])
			}
		})
	}
	// Exactly 256 components, its three plus 253, is still within the limit.
	in := input(t)
	for i := int64(0); i < 253; i++ {
		addComponent(in, netbox.CollectionInterfaces, netbox.Component{ID: 20000 + i, Device: &netbox.Ref{ID: 1000}, Name: "extra", Type: &netbox.Choice{Value: "1000base-t"}})
	}
	if got := outcomes(plan.Build(in))["Device dcim/devices/1000"]; got.action != plan.ActionCreate {
		t.Fatalf("256 components: %+v", got)
	}
}

func TestPowerFeedRackMustBeInThePanelsFacility(t *testing.T) {
	in := input(t)
	// The panel moves to a location mapped to another Facility.
	in.Snapshot.PowerPanels[0].Location = nil
	in.Snapshot.PowerPanels[0].Site = &netbox.Ref{ID: 2}
	in.Mapping.Facilities = append(in.Mapping.Facilities, netboxv1alpha1.FacilityMapping{SourceType: netboxv1alpha1.PremisesSourceSite, SourceID: 2, FacilityRef: netboxv1alpha1.LocalObjectReference{Name: "example-premises-b"}})
	in.Existing[fleet.KindFacility] = append(in.Existing[fleet.KindFacility], facility("example-premises-b"))
	got := outcomes(plan.Build(in))
	if got["record dcim/power-feeds/6000"].reason != plan.ReasonRackFacilityMismatch {
		t.Fatalf("feed %+v", got["record dcim/power-feeds/6000"])
	}
	if got["record dcim/cables/5002"].reason != plan.ReasonEndpointUnavailable {
		t.Fatalf("cable %+v", got["record dcim/cables/5002"])
	}
}

func TestTurningOnOrFixingABMCUpdatesOwnedDevices(t *testing.T) {
	// Imported before the mapping listed any BMC role.
	before := input(t)
	before.Mapping.BMC = nil
	var objs []unstructured.Unstructured
	for _, c := range plan.Build(before).Candidates {
		objs = append(objs, *applied(c.Object.DeepCopy()))
	}
	p := plan.Build(withExisting(input(t), objs...))
	server := candidate(t, p, fleet.KindDevice, "dcim/devices/1000")
	if server.Action != plan.ActionUpdate || field(t, server, "bmc", "endpoint") != "https://192.0.2.10:443" {
		t.Fatalf("turning on bmc: %+v", server)
	}
	if got := outcomes(p)["DeviceClaim dcim/devices/1000"]; got.action != plan.ActionUnchanged {
		t.Fatalf("the claim is unaffected: %+v", got)
	}

	// A later fix to the address in NetBox.
	in := input(t)
	in.Snapshot.IPAddresses[0].Address = "192.0.2.20/24"
	server = candidate(t, plan.Build(withExisting(in, previous(t)...)), fleet.KindDevice, "dcim/devices/1000")
	if server.Action != plan.ActionUpdate || field(t, server, "bmc", "endpoint") != "https://192.0.2.20:443" {
		t.Fatalf("fixing the endpoint: %+v", server)
	}
}

func TestEndpointKey(t *testing.T) {
	ip := "192.0.2.10:443"
	for endpoint, want := range map[string]string{
		"https://192.0.2.10:443":             ip,
		"https://192.0.2.10":                 ip,
		"HTTPS://192.0.2.10/redfish/v1":      ip,
		"https://[::ffff:192.0.2.10]:443":    ip,
		"http://192.0.2.10":                  "192.0.2.10:80",
		"https://192.0.2.10:8443":            "192.0.2.10:8443",
		"https://[2001:DB8::1]":              "[2001:db8::1]:443",
		"https://BMC.Example.Invalid.":       "host bmc.example.invalid:443",
		"https://bmc.example.invalid:443/x/": "host bmc.example.invalid:443",
		"ftp://192.0.2.10":                   "",
		"not a url":                          "",
	} {
		got, ok := plan.EndpointKey(endpoint)
		if got != want || ok != (want != "") {
			t.Errorf("%q: key %q ok %v, want %q", endpoint, got, ok, want)
		}
	}
}

func TestBMCCollisionsCompareNormalizedEndpoints(t *testing.T) {
	existing := func(endpoint string) unstructured.Unstructured {
		return object(fleet.KindDevice, "operator-recorded", map[string]any{
			"claimRef": map[string]any{"name": "operator-slot"}, "identity": map[string]any{"model": "x", "serial": "OTHER"},
			"bmc": map[string]any{"endpoint": endpoint},
		}, nil)
	}
	for endpoint, collides := range map[string]bool{
		"https://192.0.2.10":                     true,
		"HTTPS://192.0.2.10/redfish/v1":          true,
		"https://[::ffff:192.0.2.10]:443":        true,
		"https://192.0.2.10:8443":                false,
		"https://192.0.2.11":                     false,
		"https://bmc-of-the-same-server.example": false,
	} {
		c := candidate(t, plan.Build(withExisting(input(t), existing(endpoint))), fleet.KindDevice, "dcim/devices/1000")
		if got := c.Reason == plan.ReasonBMCCollision; got != collides {
			t.Errorf("%q: collision %v, want %v (%+v)", endpoint, got, collides, c)
		}
	}
}
