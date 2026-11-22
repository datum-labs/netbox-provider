// SPDX-License-Identifier: AGPL-3.0-only

package plan

import (
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	netboxv1alpha1 "go.miloapis.com/netbox-provider/api/v1alpha1"
	"go.miloapis.com/netbox-provider/internal/fleet"
	"go.miloapis.com/netbox-provider/internal/netbox"
)

// Reasons for the hardware catalog.
const (
	ReasonManufacturerAbsent      = "ManufacturerAbsent"
	ReasonCanonicalNameInvalid    = "CanonicalNameInvalid"
	ReasonCanonicalNameCollision  = "CanonicalNameCollision"
	ReasonModelKeyInvalid         = "ModelKeyInvalid"
	ReasonModelKeyCollision       = "ModelKeyCollision"
	ReasonTemplatesInvalid        = "TemplatesInvalid"
	ReasonCatalogDuplicate        = "CatalogDuplicate"
	deviceTypeReboundMessage      = "NetBox moved the device to another device type; Fleet never removes or rebinds a Device's deviceTypeRef"
	deviceTypeManufacturerMessage = "the device type's manufacturer changed in NetBox; Fleet keeps a DeviceType's manufacturerRef immutable"
	deviceTypeModelMessage        = "the device type's model key changed in NetBox; Fleet keeps a DeviceType's model immutable"
)

// Source record types for the catalog.
const (
	TypeManufacturer = netbox.CollectionManufacturers
	TypeDeviceType   = netbox.CollectionDeviceTypes
)

// canonicalName is Fleet's Manufacturer canonicalName rule: printable ASCII,
// starting with a letter or digit and not ending in a space.
var canonicalName = regexp.MustCompile(`^[A-Za-z0-9]([ -~]*[!-~])?$`)

var templateTypes = map[string]string{
	netbox.CollectionInterfaceTemplates:         fleet.ComponentNetworkPort,
	netbox.CollectionPowerPortTemplates:         fleet.ComponentPowerInlet,
	netbox.CollectionPowerOutletTemplates:       fleet.ComponentPowerOutlet,
	netbox.CollectionConsolePortTemplates:       fleet.ComponentConsolePort,
	netbox.CollectionConsoleServerPortTemplates: fleet.ComponentConsoleServerPort,
	netbox.CollectionFrontPortTemplates:         fleet.ComponentFrontPort,
	netbox.CollectionRearPortTemplates:          fleet.ComponentRearPort,
	netbox.CollectionModuleBayTemplates:         fleet.ComponentModule,
	netbox.CollectionDeviceBayTemplates:         fleet.ComponentDeviceBay,
	netbox.CollectionInventoryItemTemplates:     fleet.ComponentInventoryItem,
}

// ModelKey is the normalized model key of a NetBox device type: its slug,
// or its model when it has no slug, lowercased, with every run of characters
// other than ASCII letters and digits replaced by one hyphen, and hyphens
// trimmed from both ends. "Example 1U_Server" and "example-1u-server" both
// become "example-1u-server".
func ModelKey(slug, model string) string {
	source := slug
	if source == "" {
		source = model
	}
	var out strings.Builder
	pending := false
	for _, r := range strings.ToLower(source) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			if pending && out.Len() > 0 {
				out.WriteByte('-')
			}
			pending = false
			out.WriteRune(r)
			continue
		}
		pending = true
	}
	return out.String()
}

// usedDeviceTypes returns the device types of devices the mapping classifies
// as physical. Only those types, and their manufacturers, are imported.
func (b *builder) usedDeviceTypes() map[int64]bool {
	mappings := map[int64]netboxv1alpha1.DeviceMapping{}
	for _, m := range b.in.Mapping.Devices {
		mappings[m.ID] = m
	}
	excludedRoles := map[string]bool{}
	for _, e := range b.in.Mapping.Exclusions {
		for _, slug := range e.RoleSlugs {
			excludedRoles[slug] = true
		}
	}
	used := map[int64]bool{}
	for _, d := range b.in.Snapshot.Devices {
		m, ok := mappings[d.ID]
		if !ok || m.Classification != netboxv1alpha1.DeviceClassificationPhysical || d.DeviceType == nil {
			continue
		}
		if d.Role != nil && excludedRoles[d.Role.Slug] {
			continue
		}
		used[d.DeviceType.ID] = true
	}
	return used
}

// catalog plans the Manufacturers and DeviceTypes of the used device types.
func (b *builder) catalog() {
	used := b.usedDeviceTypes()
	manufacturers := map[int64]netbox.Manufacturer{}
	for _, m := range b.in.Snapshot.Manufacturers {
		manufacturers[m.ID] = m
	}
	var types []netbox.DeviceType
	usedManufacturer := map[int64]bool{}
	for _, t := range b.in.Snapshot.DeviceTypes {
		if used[t.ID] {
			types = append(types, t)
			if t.Manufacturer != nil {
				usedManufacturer[t.Manufacturer.ID] = true
			}
		}
	}
	var manufacturerIDs []int64
	for id := range usedManufacturer {
		manufacturerIDs = append(manufacturerIDs, id)
	}
	sort.Slice(manufacturerIDs, func(i, j int) bool { return manufacturerIDs[i] < manufacturerIDs[j] })

	byCanonical := map[string][]*Candidate{}
	for _, id := range manufacturerIDs {
		source := TypeManufacturer + "/" + strconv.FormatInt(id, 10)
		m, ok := manufacturers[id]
		if !ok {
			b.record(source, netboxv1alpha1.RecordStateUnresolved, ReasonManufacturerAbsent, "the manufacturer is not in the snapshot")
			continue
		}
		if len(m.Name) > 253 || !canonicalName.MatchString(m.Name) {
			b.record(source, netboxv1alpha1.RecordStateUnresolved, ReasonCanonicalNameInvalid,
				"the manufacturer's name is not printable ASCII of at most 253 characters, starting with a letter or digit and not ending in a space, as Fleet requires; it is never altered to fit")
			continue
		}
		c := b.add(fleet.KindManufacturer, source, fleet.ManufacturerSpec{DisplayName: m.Name, CanonicalName: m.Name}, b.ref(TypeManufacturer, id))
		b.manufacturerByID[id] = c
		b.canonical[c.Name] = m.Name
		byCanonical[strings.ToLower(m.Name)] = append(byCanonical[strings.ToLower(m.Name)], c)
	}
	for _, group := range byCanonical {
		quarantineGroup(group, ReasonCanonicalNameCollision, "NetBox manufacturers %s have the same canonical name ignoring case; none is written until they are merged or renamed")
	}

	byKey := map[string][]*Candidate{}
	for _, t := range types {
		source := TypeDeviceType + "/" + strconv.FormatInt(t.ID, 10)
		unresolved := func(reason, message string) {
			b.record(source, netboxv1alpha1.RecordStateUnresolved, reason, message)
		}
		if t.Manufacturer == nil {
			unresolved(ReasonManufacturerAbsent, "the device type has no manufacturer")
			continue
		}
		manufacturer, ok := b.manufacturerByID[t.Manufacturer.ID]
		if !ok {
			unresolved(ReasonDependencyBlocked, "the device type's manufacturer is not planned")
			continue
		}
		if t.Model == "" {
			unresolved(ReasonModelAbsent, "the device type has no model")
			continue
		}
		key := ModelKey(t.Slug, t.Model)
		if key == "" || len(key) > 253 {
			unresolved(ReasonModelKeyInvalid, "the device type's slug and model yield no model key of 1 to 253 characters")
			continue
		}
		if len(t.Model) > 253 {
			unresolved(ReasonFieldTooLong, "the device type's model exceeds Fleet's 253 character limit")
			continue
		}
		templates, problem := b.componentTemplates(t.ID)
		if problem != "" {
			unresolved(ReasonTemplatesInvalid, "the device type's component templates cannot be recorded exactly: "+problem)
			continue
		}
		spec := fleet.DeviceTypeSpec{
			DisplayName:        t.Model,
			ManufacturerRef:    fleet.LocalReference{Name: manufacturer.Name},
			Model:              key,
			ComponentTemplates: templates,
		}
		shape := fleet.ModelShape{}
		if t.UHeight != nil && *t.UHeight >= 0 && *t.UHeight <= 100 {
			h := float64(*t.UHeight)
			shape.HeightUnits = &h
		}
		if t.IsFullDepth != nil {
			v := *t.IsFullDepth
			shape.FullDepth = &v
		}
		if shape != (fleet.ModelShape{}) {
			spec.ModelShape = &shape
		}
		c := b.add(fleet.KindDeviceType, source, spec, b.ref(TypeDeviceType, t.ID), manufacturer.Key())
		b.deviceTypeByID[t.ID] = c
		group := manufacturer.Name + "/" + key
		byKey[group] = append(byKey[group], c)
	}
	for _, group := range byKey {
		quarantineGroup(group, ReasonModelKeyCollision, "NetBox device types %s of one manufacturer have the same model key; none is written until they are told apart")
	}
}

// quarantineGroup quarantines every candidate of a group of two or more,
// naming their sources.
func quarantineGroup(group []*Candidate, reason, format string) {
	if len(group) < 2 {
		return
	}
	var sources []string
	for _, c := range group {
		sources = append(sources, c.Source)
	}
	sort.Strings(sources)
	for _, c := range group {
		c.Action, c.Reason, c.Message = ActionQuarantined, reason, fmt.Sprintf(format, strings.Join(sources, ", "))
	}
}

// componentTemplates returns a device type's nominal components, or why they
// cannot be recorded exactly.
func (b *builder) componentTemplates(deviceType int64) ([]fleet.ComponentTemplate, string) {
	var out []fleet.ComponentTemplate
	for _, collection := range netbox.TemplateCollections {
		for _, t := range b.in.Snapshot.Templates[collection] {
			if t.DeviceType == nil || t.DeviceType.ID != deviceType {
				continue
			}
			if collection == netbox.CollectionInterfaceTemplates && logicalInterfaceTypes[netbox.ChoiceValue(t.Type)] {
				continue
			}
			c := fleet.ComponentTemplate{ID: componentID(collection, t.ID), Name: t.Name, Type: templateTypes[collection]}
			if c.Name == "" {
				c.Name = c.ID
			}
			if collection != netbox.CollectionModuleBayTemplates && collection != netbox.CollectionDeviceBayTemplates && collection != netbox.CollectionInventoryItemTemplates {
				c.Connector = netbox.ChoiceValue(t.Type)
			}
			if tooLong(c.Name, c.Connector) {
				return nil, c.ID + " has a field longer than Fleet's 253 character limit"
			}
			if t.Positions != nil || collection == netbox.CollectionRearPortTemplates {
				if t.Positions == nil || *t.Positions < 1 || *t.Positions > 256 {
					return nil, c.ID + " has no position count from 1 to 256; it is never inferred"
				}
				c.Positions = int32(*t.Positions)
			}
			out = append(out, c)
		}
	}
	if len(out) > maxComponents {
		return nil, fmt.Sprintf("%d templates exceed Fleet's limit of %d", len(out), maxComponents)
	}
	return out, ""
}

// catalogDuplicates quarantines a planned Manufacturer or DeviceType that a
// record from another writer already describes. Fleet cannot enforce that
// uniqueness across objects, so a human decides which record to keep.
func (b *builder) catalogDuplicates() {
	ours := func(obj *unstructured.Unstructured) bool {
		ref, ok := KeyOf(obj)
		return ok && ref.Source == b.in.System
	}
	otherCanonical := map[string]string{}
	allCanonical := map[string]string{}
	for i := range b.in.Existing[fleet.KindManufacturer] {
		obj := &b.in.Existing[fleet.KindManufacturer][i]
		name, _, _ := unstructured.NestedString(obj.Object, "spec", "canonicalName")
		allCanonical[obj.GetName()] = name
		if !ours(obj) {
			otherCanonical[strings.ToLower(name)] = obj.GetName()
		}
	}
	for _, c := range b.plan.Candidates {
		if c.Kind != fleet.KindManufacturer || c.Action != ActionCreate {
			continue
		}
		if other, ok := otherCanonical[strings.ToLower(b.canonical[c.Name])]; ok {
			c.Action, c.Reason = ActionQuarantined, ReasonCatalogDuplicate
			c.Message = fmt.Sprintf("Manufacturer %q from another writer already has the canonical name %q; this one is not written until someone decides which to keep", other, b.canonical[c.Name])
		}
	}
	otherTypes := map[string]string{}
	for i := range b.in.Existing[fleet.KindDeviceType] {
		obj := &b.in.Existing[fleet.KindDeviceType][i]
		if ours(obj) {
			continue
		}
		manufacturer, _, _ := unstructured.NestedString(obj.Object, "spec", "manufacturerRef", "name")
		model, _, _ := unstructured.NestedString(obj.Object, "spec", "model")
		if canonical, ok := allCanonical[manufacturer]; ok {
			otherTypes[strings.ToLower(canonical)+"/"+model] = obj.GetName()
		}
	}
	for _, c := range b.plan.Candidates {
		if c.Kind != fleet.KindDeviceType || c.Action != ActionCreate {
			continue
		}
		manufacturer, _, _ := unstructured.NestedString(c.Object.Object, "spec", "manufacturerRef", "name")
		model, _, _ := unstructured.NestedString(c.Object.Object, "spec", "model")
		if other, ok := otherTypes[strings.ToLower(b.canonical[manufacturer])+"/"+model]; ok {
			c.Action, c.Reason = ActionQuarantined, ReasonCatalogDuplicate
			c.Message = fmt.Sprintf("DeviceType %q from another writer already describes %s %s; this one is not written until someone decides which to keep", other, b.canonical[manufacturer], model)
		}
	}
}

// attachDeviceTypes sets each planned Device's deviceTypeRef. A Device whose
// type is not written in this plan keeps the reference it already has, since
// Fleet never lets one be removed, or is created without one. A Device whose
// NetBox type changed keeps its reference and is quarantined after
// reconciliation: Fleet never rebinds it.
func (b *builder) attachDeviceTypes() {
	usable := func(c *Candidate) bool {
		return c != nil && (c.Action == ActionCreate || c.Action == ActionUpdate || c.Action == ActionUnchanged)
	}
	existing := map[string]*unstructured.Unstructured{}
	for i := range b.in.Existing[fleet.KindDevice] {
		obj := &b.in.Existing[fleet.KindDevice][i]
		existing[obj.GetName()] = obj
	}
	for _, c := range b.plan.Candidates {
		typeID, ok := b.deviceTypeOf[c]
		if c.Kind != fleet.KindDevice || !ok {
			continue
		}
		want := ""
		tc := b.deviceTypeByID[typeID]
		if usable(tc) {
			manufacturer, _, _ := unstructured.NestedString(tc.Object.Object, "spec", "manufacturerRef", "name")
			if usable(b.byKey[fleet.KindManufacturer+"/"+manufacturer]) {
				want = tc.Name
			}
		}
		current := ""
		if obj := existing[c.Name]; obj != nil {
			current, _, _ = unstructured.NestedString(obj.Object, "spec", "deviceTypeRef", "name")
		}
		ref := want
		switch {
		case current != "" && want != "" && current != want:
			ref = current
			b.rebound[c] = true
		case current != "":
			ref = current
		case want != "":
			c.DependsOn = append(c.DependsOn, tc.Key())
		}
		if ref == "" {
			continue
		}
		_ = unstructured.SetNestedField(c.Object.Object, map[string]any{"name": ref}, "spec", "deviceTypeRef")
		annotations := c.Object.GetAnnotations()
		annotations[AnnotationAppliedDigest] = AppliedDigest(c.Object)
		c.Object.SetAnnotations(annotations)
	}
}
