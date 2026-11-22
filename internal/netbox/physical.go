// SPDX-License-Identifier: AGPL-3.0-only

package netbox

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
	// RearPort and RearPortPosition map a front port to its rear port.
	RearPort         *Ref   `json:"rear_port,omitempty"`
	RearPortPosition *int64 `json:"rear_port_position,omitempty"`
	// PowerPort maps a power outlet to the inlet that feeds it.
	PowerPort *Ref `json:"power_port,omitempty"`
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
