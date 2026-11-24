// SPDX-License-Identifier: AGPL-3.0-only

package netbox

import (
	"encoding/json"
	"errors"
)

// Component, cable and power collections. Each component collection holds
// one class of device component; ComponentCollections lists them in the
// order they are read.
const (
	CollectionInterfaces         = "dcim/interfaces"
	CollectionPowerPorts         = "dcim/power-ports"
	CollectionPowerOutlets       = "dcim/power-outlets"
	CollectionConsolePorts       = "dcim/console-ports"
	CollectionConsoleServerPorts = "dcim/console-server-ports"
	CollectionFrontPorts         = "dcim/front-ports"
	CollectionRearPorts          = "dcim/rear-ports"
	CollectionModules            = "dcim/modules"
	CollectionInventoryItems     = "dcim/inventory-items"
	CollectionDeviceBays         = "dcim/device-bays"
	CollectionCables             = "dcim/cables"
	CollectionPowerPanels        = "dcim/power-panels"
	CollectionPowerFeeds         = "dcim/power-feeds"
	CollectionIPAddresses        = "ipam/ip-addresses"
)

// ComponentCollections are the device component collections, in read order.
var ComponentCollections = []string{
	CollectionInterfaces, CollectionPowerPorts, CollectionPowerOutlets, CollectionConsolePorts,
	CollectionConsoleServerPorts, CollectionFrontPorts, CollectionRearPorts, CollectionModules,
	CollectionInventoryItems, CollectionDeviceBays,
}

// Choice is a NetBox choice field such as a status or port type. Only its
// value is retained.
type Choice struct {
	Value string `json:"value,omitempty"`
}

// ChoiceValue returns the choice's value, or "" when it is absent.
func ChoiceValue(c *Choice) string {
	if c == nil {
		return ""
	}
	return c.Value
}

// Component is one record of a device component collection. Fields that a
// collection does not have stay empty.
type Component struct {
	ID       int64   `json:"id"`
	Device   *Ref    `json:"device,omitempty"`
	Name     string  `json:"name,omitempty"`
	Display  string  `json:"display,omitempty"`
	Type     *Choice `json:"type,omitempty"`
	Serial   string  `json:"serial,omitempty"`
	AssetTag string  `json:"asset_tag,omitempty"`
	// Positions is a rear port's number of positions.
	Positions *int64 `json:"positions,omitempty"`
	// RearPorts maps each front port position to a rear port position
	// (NetBox 4.5 and later). Older NetBox records one mapping in RearPort
	// and RearPortPosition instead, for front port position 1.
	RearPorts        []RearPortMapping `json:"rear_ports,omitempty"`
	RearPort         *Ref              `json:"rear_port,omitempty"`
	RearPortPosition *int64            `json:"rear_port_position,omitempty"`
	// PowerPort maps a power outlet to the inlet that feeds it.
	PowerPort *Ref `json:"power_port,omitempty"`
	// Module is the module that installed the component, if any.
	Module *Ref `json:"module,omitempty"`
	// MACAddress is an interface's MAC as NetBox records it, unnormalized:
	// the primary MAC address object's value (NetBox 4.2 and later), else
	// the legacy mac_address field. It is empty when NetBox records none.
	MACAddress string `json:"macAddress,omitempty"`
}

// UnmarshalJSON reads an interface's MAC from primary_mac_address, falling
// back to mac_address. No other field of the MAC address object is kept.
func (c *Component) UnmarshalJSON(data []byte) error {
	type plain Component
	var decoded struct {
		plain
		PrimaryMAC *struct {
			MACAddress *string `json:"mac_address"`
		} `json:"primary_mac_address"`
		LegacyMAC *string `json:"mac_address"`
	}
	if err := json.Unmarshal(data, &decoded); err != nil {
		return err
	}
	*c = Component(decoded.plain)
	c.MACAddress = deref(decoded.LegacyMAC)
	if decoded.PrimaryMAC != nil && deref(decoded.PrimaryMAC.MACAddress) != "" {
		c.MACAddress = *decoded.PrimaryMAC.MACAddress
	}
	return nil
}

// RearPortMapping maps one front port position to a rear port position.
type RearPortMapping struct {
	Position         *int64 `json:"position,omitempty"`
	RearPort         *ID    `json:"rear_port,omitempty"`
	RearPortPosition *int64 `json:"rear_port_position,omitempty"`
}

// ID is a NetBox object ID that is serialized either as a number or as a
// nested object with an id.
type ID int64

// UnmarshalJSON implements json.Unmarshaler.
func (i *ID) UnmarshalJSON(data []byte) error {
	var n int64
	if err := json.Unmarshal(data, &n); err == nil {
		*i = ID(n)
		return nil
	}
	var ref Ref
	if err := json.Unmarshal(data, &ref); err != nil || ref.ID == 0 {
		return errors.New("invalid object reference")
	}
	*i = ID(ref.ID)
	return nil
}

// Termination is one end of a cable: a component, power feed or circuit
// termination, named by its NetBox object type, such as dcim.interface.
type Termination struct {
	ObjectType string `json:"object_type,omitempty"`
	ObjectID   int64  `json:"object_id,omitempty"`
}

// Cable is a dcim/cables record. Type is NetBox's cable type, such as cat6;
// it is empty when NetBox records none.
type Cable struct {
	ID            int64         `json:"id"`
	Type          string        `json:"type,omitempty"`
	Status        *Choice       `json:"status,omitempty"`
	Label         string        `json:"label,omitempty"`
	ATerminations []Termination `json:"a_terminations,omitempty"`
	BTerminations []Termination `json:"b_terminations,omitempty"`
}

// PowerPanel is a dcim/power-panels record.
type PowerPanel struct {
	ID       int64  `json:"id"`
	Name     string `json:"name,omitempty"`
	Site     *Ref   `json:"site,omitempty"`
	Location *Ref   `json:"location,omitempty"`
}

// PowerFeed is a dcim/power-feeds record.
type PowerFeed struct {
	ID         int64   `json:"id"`
	Name       string  `json:"name,omitempty"`
	PowerPanel *Ref    `json:"power_panel,omitempty"`
	Rack       *Ref    `json:"rack,omitempty"`
	Status     *Choice `json:"status,omitempty"`
	Supply     *Choice `json:"supply,omitempty"`
	Phase      *Choice `json:"phase,omitempty"`
	Voltage    *int64  `json:"voltage,omitempty"`
	Amperage   *int64  `json:"amperage,omitempty"`
}
