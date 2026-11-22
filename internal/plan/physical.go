// SPDX-License-Identifier: AGPL-3.0-only

package plan

import (
	"fmt"
	"net"
	"net/netip"
	"strconv"
	"strings"

	netboxv1alpha1 "go.miloapis.com/netbox-provider/api/v1alpha1"
	"go.miloapis.com/netbox-provider/internal/fleet"
	"go.miloapis.com/netbox-provider/internal/netbox"
)

// Reasons for components, power feeds and cables.
const (
	ReasonNotInstalled          = "NotInstalled"
	ReasonComponentsInvalid     = "ComponentsInvalid"
	ReasonRackUnitsUnavailable  = "RackUnitsUnavailable"
	ReasonPowerPanelAbsent      = "PowerPanelAbsent"
	ReasonMultiTermination      = "MultiTermination"
	ReasonEndpointUnsupported   = "EndpointUnsupported"
	ReasonEndpointUnavailable   = "EndpointUnavailable"
	ReasonMultiPosition         = "MultiPositionTermination"
	ReasonInvalidPowerCable     = "InvalidPowerCable"
	ReasonMixedEndpoints        = "MixedEndpoints"
	ReasonCableMediumUnreviewed = "CableMediumUnreviewed"
)

// Source record types for power feeds and cables.
const (
	TypePowerFeed = netbox.CollectionPowerFeeds
	TypeCable     = netbox.CollectionCables
)

// maxComponents is Fleet's limit on a Device's components.
const maxComponents = 256

// feedOutput is the one termination of every imported PowerFeed.
const feedOutput = "output"

var componentTypes = map[string]string{
	netbox.CollectionInterfaces:         fleet.ComponentNetworkPort,
	netbox.CollectionPowerPorts:         fleet.ComponentPowerInlet,
	netbox.CollectionPowerOutlets:       fleet.ComponentPowerOutlet,
	netbox.CollectionConsolePorts:       fleet.ComponentConsolePort,
	netbox.CollectionConsoleServerPorts: fleet.ComponentConsoleServerPort,
	netbox.CollectionFrontPorts:         fleet.ComponentFrontPort,
	netbox.CollectionRearPorts:          fleet.ComponentRearPort,
	netbox.CollectionModules:            fleet.ComponentModule,
	netbox.CollectionInventoryItems:     fleet.ComponentInventoryItem,
	netbox.CollectionDeviceBays:         fleet.ComponentDeviceBay,
}

// terminationCollections maps a cable termination's NetBox object type to
// its component collection.
var terminationCollections = map[string]string{
	"dcim.interface":         netbox.CollectionInterfaces,
	"dcim.powerport":         netbox.CollectionPowerPorts,
	"dcim.poweroutlet":       netbox.CollectionPowerOutlets,
	"dcim.consoleport":       netbox.CollectionConsolePorts,
	"dcim.consoleserverport": netbox.CollectionConsoleServerPorts,
	"dcim.frontport":         netbox.CollectionFrontPorts,
	"dcim.rearport":          netbox.CollectionRearPorts,
}

// logicalInterfaceTypes are interfaces that belong to networking, not to
// physical inventory. They are not components and cannot terminate a cable.
var logicalInterfaceTypes = map[string]bool{"virtual": true, "lag": true, "bridge": true}

func componentID(collection string, id int64) string {
	return strings.TrimPrefix(collection, "dcim/") + "-" + strconv.FormatInt(id, 10)
}

// deviceParts is the component inventory of one NetBox device.
type deviceParts struct {
	components []fleet.Component
	mappings   []fleet.InternalMapping
	// problem is why the parts cannot be recorded exactly. A Device is never
	// created with a partial component list: records are create-only, so a
	// dropped component could never be added later.
	problem string
}

// owned is one component, indexed for cable endpoints.
type owned struct {
	device    int64
	component fleet.Component
}

// collectComponents groups every physical component by device and indexes
// them for cable endpoints.
func (b *builder) collectComponents() {
	b.parts = map[int64]*deviceParts{}
	b.components = map[string]owned{}
	b.logical = map[string]bool{}
	part := func(device int64) *deviceParts {
		if b.parts[device] == nil {
			b.parts[device] = &deviceParts{}
		}
		return b.parts[device]
	}
	for _, collection := range netbox.ComponentCollections {
		for _, record := range b.in.Snapshot.Components[collection] {
			if record.Device == nil {
				continue
			}
			key := collection + "/" + strconv.FormatInt(record.ID, 10)
			if collection == netbox.CollectionInterfaces && logicalInterfaceTypes[netbox.ChoiceValue(record.Type)] {
				b.logical[key] = true
				continue
			}
			p := part(record.Device.ID)
			c := fleet.Component{
				ID:       componentID(collection, record.ID),
				Name:     record.Name,
				Type:     componentTypes[collection],
				Serial:   record.Serial,
				AssetTag: record.AssetTag,
			}
			if c.Name == "" {
				c.Name = record.Display
			}
			if c.Name == "" {
				c.Name = c.ID
			}
			if collection != netbox.CollectionModules && collection != netbox.CollectionInventoryItems && collection != netbox.CollectionDeviceBays {
				c.Connector = netbox.ChoiceValue(record.Type)
			}
			if tooLong(c.Name, c.Connector, c.Serial, c.AssetTag) {
				p.problem = c.ID + " has a field longer than Fleet's 253 character limit"
			}
			if record.Positions != nil || collection == netbox.CollectionRearPorts {
				if record.Positions == nil || *record.Positions < 1 || *record.Positions > 256 {
					p.problem = c.ID + " has no position count from 1 to 256; it is never inferred"
				} else {
					c.Positions = int32(*record.Positions)
				}
			}
			p.components = append(p.components, c)
			b.components[key] = owned{device: record.Device.ID, component: c}
		}
	}
	for device, p := range b.parts {
		if len(p.components) > maxComponents {
			p.problem = fmt.Sprintf("%d components exceed Fleet's limit of %d", len(p.components), maxComponents)
		}
		b.internalMappings(device, p)
	}
}

// internalMappings records front-to-rear and outlet-to-inlet paths. A path to
// a component of another device, or without a valid rear position, makes the
// parts inexact.
func (b *builder) internalMappings(device int64, p *deviceParts) {
	usedRear := map[fleet.ComponentPosition]bool{}
	for _, collection := range []string{netbox.CollectionFrontPorts, netbox.CollectionPowerOutlets} {
		for _, record := range b.in.Snapshot.Components[collection] {
			if record.Device == nil || record.Device.ID != device {
				continue
			}
			if collection == netbox.CollectionFrontPorts {
				b.frontPortMappings(device, p, record, usedRear)
				continue
			}
			from := fleet.ComponentPosition{ComponentID: componentID(collection, record.ID), Position: 1}
			if record.PowerPort == nil {
				continue
			}
			target, ok := b.components[netbox.CollectionPowerPorts+"/"+strconv.FormatInt(record.PowerPort.ID, 10)]
			if !ok || target.device != device {
				p.problem = from.ComponentID + " is fed by a power port that is not on this device"
				continue
			}
			p.mappings = append(p.mappings, fleet.InternalMapping{From: from, To: fleet.ComponentPosition{ComponentID: target.component.ID, Position: 1}})
		}
	}
}

// frontPortMappings records a front port's paths to rear port positions,
// from NetBox's per-position mappings or its older single mapping.
func (b *builder) frontPortMappings(device int64, p *deviceParts, record netbox.Component, usedRear map[fleet.ComponentPosition]bool) {
	id := componentID(netbox.CollectionFrontPorts, record.ID)
	type mapping struct{ from, rear, rearPosition *int64 }
	var mappings []mapping
	for _, m := range record.RearPorts {
		var rear *int64
		if m.RearPort != nil {
			v := int64(*m.RearPort)
			rear = &v
		}
		mappings = append(mappings, mapping{from: m.Position, rear: rear, rearPosition: m.RearPortPosition})
	}
	if len(mappings) == 0 && record.RearPort != nil {
		one := int64(1)
		mappings = append(mappings, mapping{from: &one, rear: &record.RearPort.ID, rearPosition: record.RearPortPosition})
	}
	if len(mappings) == 0 {
		p.problem = id + " has no rear port and position; the path is never inferred"
		return
	}
	frontPositions := int64(1)
	if record.Positions != nil {
		frontPositions = *record.Positions
	}
	usedFront := map[int64]bool{}
	for _, m := range mappings {
		if m.from == nil || m.rear == nil || m.rearPosition == nil || *m.from < 1 || *m.from > frontPositions || *m.rearPosition < 1 || *m.rearPosition > 256 {
			p.problem = id + " has an incomplete rear port mapping; the path is never inferred"
			return
		}
		if usedFront[*m.from] {
			p.problem = id + " maps one position to more than one rear port position"
			return
		}
		usedFront[*m.from] = true
		target, ok := b.components[netbox.CollectionRearPorts+"/"+strconv.FormatInt(*m.rear, 10)]
		if !ok || target.device != device || int64(target.component.Positions) < *m.rearPosition {
			p.problem = id + " maps to a rear port position that is not on this device"
			return
		}
		to := fleet.ComponentPosition{ComponentID: target.component.ID, Position: int32(*m.rearPosition)}
		if usedRear[to] {
			p.problem = to.ComponentID + " position " + strconv.Itoa(int(to.Position)) + " is mapped from more than one front port"
			return
		}
		usedRear[to] = true
		p.mappings = append(p.mappings, fleet.InternalMapping{From: fleet.ComponentPosition{ComponentID: id, Position: int32(*m.from)}, To: to})
	}
}

// bmcPort is the port of every BMC endpoint written from NetBox. NetBox
// records an out-of-band address but no port; Redfish is served over HTTPS
// on 443, and Fleet's Redfish executor requires the port to be explicit.
const bmcPort = "443"

// bmcEndpoint returns https://<IPv4>:443 from a device's NetBox out-of-band
// IP, or why there is none. Fleet's Redfish executor accepts only a literal
// IPv4 endpoint, so an IPv6 address is not written.
func bmcEndpoint(device netbox.Device) (string, string) {
	if device.OOBIP == nil || device.OOBIP.Address == "" {
		return "", "NetBox records no out-of-band IP"
	}
	address := device.OOBIP.Address
	var addr netip.Addr
	if prefix, err := netip.ParsePrefix(address); err == nil {
		addr = prefix.Addr()
	} else if a, err := netip.ParseAddr(address); err == nil {
		addr = a
	} else {
		return "", "the out-of-band IP is not an IP address"
	}
	addr = addr.Unmap()
	switch {
	case !addr.Is4():
		return "", "the out-of-band IP is IPv6; Fleet's Redfish executor accepts only IPv4"
	case addr.IsUnspecified() || addr.IsLoopback() || addr.IsMulticast() || addr.IsLinkLocalUnicast():
		return "", "the out-of-band IP is not a usable unicast address"
	}
	return "https://" + net.JoinHostPort(addr.String(), bmcPort), ""
}

// rackUnits returns RackUnits placement for a device, or a reason it cannot
// be asserted exactly.
func rackUnits(device netbox.Device, dt netbox.DeviceType, rack string) (fleet.Placement, string) {
	integral := func(d *netbox.Decimal) (int32, bool) {
		if d == nil || float64(*d) != float64(int32(*d)) {
			return 0, false
		}
		return int32(*d), true
	}
	position, ok := integral(device.Position)
	if !ok || position < 1 || position > 100 {
		return fleet.Placement{}, "NetBox has no whole-unit rack position for this device"
	}
	height, ok := integral(dt.UHeight)
	if !ok || height < 1 || position+height > 101 {
		return fleet.Placement{}, "the device type has no whole-unit height from 1 that fits the rack"
	}
	face := netbox.ChoiceValue(device.Face)
	if face != "front" && face != "rear" {
		return fleet.Placement{}, "NetBox records no rack face for this device"
	}
	return fleet.Placement{
		Scope:    string(netboxv1alpha1.PlacementScopeRackUnits),
		RackRef:  &fleet.LocalReference{Name: rack},
		Position: position,
		Height:   height,
		Face:     face,
	}, ""
}

func (b *builder) powerFeeds() {
	panels := map[int64]netbox.PowerPanel{}
	for _, p := range b.in.Snapshot.PowerPanels {
		panels[p.ID] = p
	}
	for _, feed := range b.in.Snapshot.PowerFeeds {
		source := TypePowerFeed + "/" + strconv.FormatInt(feed.ID, 10)
		if netbox.ChoiceValue(feed.Status) == "planned" {
			b.record(source, netboxv1alpha1.RecordStateExcluded, ReasonNotInstalled, "NetBox status is planned; the feed is not installed")
			continue
		}
		if feed.PowerPanel == nil {
			b.record(source, netboxv1alpha1.RecordStateUnresolved, ReasonPowerPanelAbsent, "the feed has no power panel, so its premises are unknown")
			continue
		}
		panel, ok := panels[feed.PowerPanel.ID]
		if !ok {
			b.record(source, netboxv1alpha1.RecordStateUnresolved, ReasonPowerPanelAbsent, "the feed's power panel is not in the snapshot")
			continue
		}
		facility, reason, message := b.facility(panel.Site, panel.Location)
		if facility == "" {
			b.record(source, netboxv1alpha1.RecordStateUnresolved, reason, message)
			continue
		}
		ref := b.ref(TypePowerFeed, feed.ID)
		spec := fleet.PowerFeedSpec{
			DisplayName:  displayName(feed.Name, Name(ref, fleet.KindPowerFeed)),
			FacilityRef:  fleet.LocalReference{Name: facility},
			Supply:       supply(feed),
			Terminations: []fleet.Component{{ID: feedOutput, Name: feedOutput, Type: fleet.ComponentPowerOutlet}},
		}
		var deps []string
		if feed.Rack != nil {
			rack, ok := b.rackByID[feed.Rack.ID]
			if !ok {
				b.record(source, netboxv1alpha1.RecordStateUnresolved, ReasonDependencyBlocked, "the feed's rack is not in the snapshot or is unresolved")
				continue
			}
			spec.RackRef = &fleet.LocalReference{Name: rack.Name}
			deps = append(deps, rack.Key())
		}
		b.feedByID[feed.ID] = b.add(fleet.KindPowerFeed, source, spec, ref, deps...)
	}
}

func supply(feed netbox.PowerFeed) *fleet.PowerSupply {
	s := &fleet.PowerSupply{Type: strings.ToUpper(netbox.ChoiceValue(feed.Supply))}
	if s.Type != "AC" && s.Type != "DC" {
		s.Type = "Unknown"
	}
	if feed.Voltage != nil && *feed.Voltage >= 1 && *feed.Voltage <= 100000 {
		s.VoltageV = int32(*feed.Voltage)
	}
	if feed.Amperage != nil && *feed.Amperage >= 1 && *feed.Amperage <= 100000 {
		s.CurrentA = int32(*feed.Amperage)
	}
	if phase := netbox.ChoiceValue(feed.Phase); phase == "single-phase" || phase == "three-phase" {
		s.Phase = phase
	}
	return s
}

// endpoint is one resolved cable end.
type endpoint struct {
	end  fleet.CableEndpoint
	typ  string
	dep  string
	feed bool
}

// cableEnd resolves a termination to a planned Device component or PowerFeed.
func (b *builder) cableEnd(t netbox.Termination) (endpoint, string, string) {
	if t.ObjectType == "dcim.powerfeed" {
		feed, ok := b.feedByID[t.ObjectID]
		if !ok {
			return endpoint{}, ReasonEndpointUnavailable, fmt.Sprintf("power feed %d is not planned", t.ObjectID)
		}
		return endpoint{
			end:  fleet.CableEndpoint{PowerFeedRef: &fleet.LocalReference{Name: feed.Name}, ComponentID: feedOutput, Position: 1},
			typ:  fleet.ComponentPowerOutlet,
			dep:  feed.Key(),
			feed: true,
		}, "", ""
	}
	collection, ok := terminationCollections[t.ObjectType]
	if !ok {
		return endpoint{}, ReasonEndpointUnsupported, fmt.Sprintf("a %s termination has no Fleet equivalent", t.ObjectType)
	}
	key := collection + "/" + strconv.FormatInt(t.ObjectID, 10)
	if b.logical[key] {
		return endpoint{}, ReasonEndpointUnsupported, key + " is a logical interface, not a physical component"
	}
	c, ok := b.components[key]
	if !ok {
		return endpoint{}, ReasonEndpointUnavailable, key + " is not in the scoped snapshot"
	}
	device, ok := b.deviceByID[c.device]
	if !ok {
		return endpoint{}, ReasonEndpointUnavailable, fmt.Sprintf("%s belongs to %s/%d, which has no planned Device", key, TypeDevice, c.device)
	}
	if c.component.Positions > 1 {
		return endpoint{}, ReasonMultiPosition, key + " has several positions; lanes are never inferred"
	}
	return endpoint{
		end: fleet.CableEndpoint{DeviceRef: &fleet.LocalReference{Name: device.Name}, ComponentID: c.component.ID, Position: 1},
		typ: c.component.Type,
		dep: device.Key(),
	}, "", ""
}

// medium derives power and console media from the endpoints, and data media
// from the reviewed cable type rules.
func (b *builder) medium(cable netbox.Cable, a, z endpoint) (string, string, string) {
	power := func(t string) bool { return t == fleet.ComponentPowerOutlet || t == fleet.ComponentPowerInlet }
	console := func(t string) bool { return t == fleet.ComponentConsolePort || t == fleet.ComponentConsoleServerPort }
	switch {
	case power(a.typ) || power(z.typ):
		if a.typ != z.typ && power(a.typ) && power(z.typ) {
			return fleet.MediumPower, "", ""
		}
		return "", ReasonInvalidPowerCable, "a power cable must connect an outlet to an inlet"
	case console(a.typ) || console(z.typ):
		if console(a.typ) && console(z.typ) {
			return fleet.MediumConsole, "", ""
		}
		return "", ReasonMixedEndpoints, "the cable connects a console component to a data component"
	}
	if cable.Type == "" {
		if b.in.Mapping.UntypedCableMedium != "" {
			return string(b.in.Mapping.UntypedCableMedium), "", ""
		}
		return "", ReasonCableMediumUnreviewed, "NetBox records no cable type and the mapping sets no medium for untyped cables"
	}
	for _, rule := range b.in.Mapping.CableMedia {
		for _, t := range rule.CableTypes {
			if t == cable.Type {
				return string(rule.Medium), "", ""
			}
		}
	}
	return "", ReasonCableMediumUnreviewed, fmt.Sprintf("no reviewed medium for NetBox cable type %q", cable.Type)
}

func (b *builder) cables() {
	for _, cable := range b.in.Snapshot.Cables {
		source := TypeCable + "/" + strconv.FormatInt(cable.ID, 10)
		unresolved := func(reason, message string) {
			b.record(source, netboxv1alpha1.RecordStateUnresolved, reason, message)
		}
		if netbox.ChoiceValue(cable.Status) == "planned" {
			b.record(source, netboxv1alpha1.RecordStateExcluded, ReasonNotInstalled, "NetBox status is planned; the cable is not installed")
			continue
		}
		if len(cable.ATerminations) != 1 || len(cable.BTerminations) != 1 {
			b.record(source, netboxv1alpha1.RecordStateUnsupported, ReasonMultiTermination, "a cable with several terminations on one side needs reviewed lanes; none are inferred")
			continue
		}
		a, reason, message := b.cableEnd(cable.ATerminations[0])
		if reason != "" {
			unresolved(reason, "A side: "+message)
			continue
		}
		z, reason, message := b.cableEnd(cable.BTerminations[0])
		if reason != "" {
			unresolved(reason, "B side: "+message)
			continue
		}
		medium, reason, message := b.medium(cable, a, z)
		if reason != "" {
			unresolved(reason, message)
			continue
		}
		ref := b.ref(TypeCable, cable.ID)
		spec := fleet.CableSpec{
			DisplayName: displayName(cable.Label, Name(ref, fleet.KindCable)),
			Medium:      medium,
			Lanes:       []fleet.CableLane{{ID: "lane-1", A: a.end, B: z.end}},
		}
		deps := []string{a.dep}
		if z.dep != a.dep {
			deps = append(deps, z.dep)
		}
		b.add(fleet.KindCable, source, spec, ref, deps...)
	}
}
