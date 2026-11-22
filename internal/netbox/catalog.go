// SPDX-License-Identifier: AGPL-3.0-only

package netbox

// Catalog collections. Template collections hold a device type's nominal
// components; TemplateCollections lists them in the order they are read.
const (
	CollectionManufacturers              = "dcim/manufacturers"
	CollectionInterfaceTemplates         = "dcim/interface-templates"
	CollectionPowerPortTemplates         = "dcim/power-port-templates"
	CollectionPowerOutletTemplates       = "dcim/power-outlet-templates"
	CollectionConsolePortTemplates       = "dcim/console-port-templates"
	CollectionConsoleServerPortTemplates = "dcim/console-server-port-templates"
	CollectionFrontPortTemplates         = "dcim/front-port-templates"
	CollectionRearPortTemplates          = "dcim/rear-port-templates"
	CollectionModuleBayTemplates         = "dcim/module-bay-templates"
	CollectionDeviceBayTemplates         = "dcim/device-bay-templates"
	CollectionInventoryItemTemplates     = "dcim/inventory-item-templates"
)

// TemplateCollections are the component template collections, in read order.
var TemplateCollections = []string{
	CollectionInterfaceTemplates, CollectionPowerPortTemplates, CollectionPowerOutletTemplates,
	CollectionConsolePortTemplates, CollectionConsoleServerPortTemplates, CollectionFrontPortTemplates,
	CollectionRearPortTemplates, CollectionModuleBayTemplates, CollectionDeviceBayTemplates,
	CollectionInventoryItemTemplates,
}

// Manufacturer is a dcim/manufacturers record.
type Manufacturer struct {
	ID   int64  `json:"id"`
	Name string `json:"name,omitempty"`
	Slug string `json:"slug,omitempty"`
}

// ComponentTemplate is one record of a template collection. A template that
// belongs to a module type rather than a device type has no DeviceType.
type ComponentTemplate struct {
	ID         int64   `json:"id"`
	DeviceType *Ref    `json:"device_type,omitempty"`
	Name       string  `json:"name,omitempty"`
	Type       *Choice `json:"type,omitempty"`
	Positions  *int64  `json:"positions,omitempty"`
}
