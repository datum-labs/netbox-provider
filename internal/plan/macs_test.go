// SPDX-License-Identifier: AGPL-3.0-only

package plan_test

import (
	"fmt"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"go.miloapis.com/netbox-provider/internal/fleet"
	"go.miloapis.com/netbox-provider/internal/netbox"
	"go.miloapis.com/netbox-provider/internal/plan"
)

// fixtureMACs are the expected interfaces of the fixture server, from a
// primary MAC object and a legacy dashed MAC; its LAG's MAC is not one.
var fixtureMACs = []any{
	map[string]any{"name": "eth0", "macAddress": "02:00:00:aa:10:00"},
	map[string]any{"name": "eth1", "macAddress": "02:00:00:aa:10:01"},
}

func iface(id, device int64, name, typ, mac string) netbox.Component {
	return netbox.Component{ID: id, Device: &netbox.Ref{ID: device}, Name: name, Type: &netbox.Choice{Value: typ}, MACAddress: mac}
}

func TestExpectedMACs(t *testing.T) {
	tests := []struct {
		name string
		add  []netbox.Component
		// server and sw are the expected interfaces of the fixture server
		// and switch; nil means none are written.
		server, sw []any
		// note is part of the server's or, with no server note, the
		// switch's plan message; empty means neither has a MAC note.
		note string
	}{
		{name: "fixture", server: fixtureMACs},
		{
			name:   "virtual and bridge interfaces are never interfaces",
			add:    []netbox.Component{iface(8000, 1000, "vlan10", "virtual", "02:00:00:aa:20:00"), iface(8001, 1000, "br0", "bridge", "02:00:00:aa:20:01")},
			server: fixtureMACs,
		},
		{
			name:   "normalized to lowercase colon form",
			add:    []netbox.Component{iface(8000, 1007, "mgmt1", "1000base-t", " 0200.00AA.2000 ")},
			server: fixtureMACs,
			sw:     []any{map[string]any{"name": "mgmt1", "macAddress": "02:00:00:aa:20:00"}},
		},
		{
			name: "not EUI-48 unicast",
			add: []netbox.Component{
				iface(8000, 1007, "a", "1000base-t", "not-a-mac"),
				iface(8001, 1007, "b", "1000base-t", "02:00:00:ff:fe:aa:20:00"),
				iface(8002, 1007, "c", "1000base-t", "00:00:00:00:00:00"),
				iface(8003, 1007, "d", "1000base-t", "01:00:5e:00:00:01"),
			},
			server: fixtureMACs,
			note:   "not an EUI-48 unicast MAC: interfaces-8000, interfaces-8001, interfaces-8002 and 1 more",
		},
		{
			name:   "a MAC on two interfaces of one device is left empty on both",
			add:    []netbox.Component{iface(8000, 1000, "eth2", "1000base-t", "02:00:00:AA:10:00")},
			server: []any{fixtureMACs[1]},
			note:   "MAC recorded on more than one interface: interface-templates-700, interfaces-8000",
		},
		{
			name:   "a MAC on interfaces of two devices is left empty on both",
			add:    []netbox.Component{iface(8000, 1007, "mgmt1", "1000base-t", "02:00:00:aa:10:01")},
			server: []any{fixtureMACs[0]},
			note:   "MAC recorded on more than one interface: interface-templates-701",
		},
		{
			name:   "a name longer than Fleet allows",
			add:    []netbox.Component{iface(8000, 1007, strings.Repeat("n", 64), "1000base-t", "02:00:00:aa:20:00")},
			server: fixtureMACs,
			note:   "interface name too long or not unique: interfaces-8000",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			in := input(t)
			for _, c := range tt.add {
				addComponent(in, netbox.CollectionInterfaces, c)
			}
			p := plan.Build(in)
			server := candidate(t, p, fleet.KindDevice, "dcim/devices/1000")
			sw := candidate(t, p, fleet.KindDevice, "dcim/devices/1007")
			for _, c := range []*plan.Candidate{server, sw} {
				if c.Action != plan.ActionCreate {
					t.Fatalf("%s %s/%s %s; an unusable MAC never blocks a Device", c.Source, c.Action, c.Reason, c.Message)
				}
			}
			if got := field(t, server, "interfaces"); !equalJSON(got, tt.server) && (tt.server != nil || got != nil) {
				t.Errorf("server interfaces %v, want %v", got, tt.server)
			}
			if got := field(t, sw, "interfaces"); !equalJSON(got, tt.sw) && (tt.sw != nil || got != nil) {
				t.Errorf("switch interfaces %v, want %v", got, tt.sw)
			}
			messages := server.Message + "|" + sw.Message
			if tt.note == "" && strings.Contains(messages, "MAC") || !strings.Contains(messages, tt.note) {
				t.Errorf("messages %q, want %q", messages, tt.note)
			}
		})
	}
}

func TestMoreMACsThanFleetAllowsWritesNone(t *testing.T) {
	in := input(t)
	for i := range 33 {
		addComponent(in, netbox.CollectionInterfaces, iface(int64(8000+i), 1007, fmt.Sprintf("p%d", i), "1000base-t", fmt.Sprintf("02:00:00:aa:30:%02x", i)))
	}
	sw := candidate(t, plan.Build(in), fleet.KindDevice, "dcim/devices/1007")
	if sw.Action != plan.ActionCreate || field(t, sw, "interfaces") != nil || !strings.Contains(sw.Message, "33 interfaces have one, more than Fleet's limit of 32") {
		t.Errorf("switch %s %v %q", sw.Action, field(t, sw, "interfaces"), sw.Message)
	}
}

func TestExpectedMACsFollowNetBox(t *testing.T) {
	for _, tt := range []struct {
		name string
		mac  string
		want []any
	}{
		{"a changed MAC is re-applied", "02:00:00:aa:10:09", []any{fixtureMACs[0], map[string]any{"name": "eth1", "macAddress": "02:00:00:aa:10:09"}}},
		{"a MAC NetBox no longer records is removed", "", []any{fixtureMACs[0]}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			in := input(t)
			for i, c := range in.Snapshot.Components[netbox.CollectionInterfaces] {
				if c.ID == 2001 {
					in.Snapshot.Components[netbox.CollectionInterfaces][i].MACAddress = tt.mac
				}
			}
			p := plan.Build(withExisting(in, previous(t)...))
			server := candidate(t, p, fleet.KindDevice, "dcim/devices/1000")
			if server.Action != plan.ActionUpdate || p.Count(plan.ActionUpdate) != 1 {
				t.Fatalf("outcomes %v", outcomes(p))
			}
			interfaces, _, _ := unstructured.NestedSlice(server.Object.Object, "spec", "interfaces")
			if !equalJSON(interfaces, tt.want) {
				t.Errorf("interfaces %v, want %v", interfaces, tt.want)
			}
		})
	}
}
