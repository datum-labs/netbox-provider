// SPDX-License-Identifier: AGPL-3.0-only

package netbox

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"time"
)

// Collections this reader supports. Only fields declared on the types below
// are retained; custom fields, config contexts, comments, URLs and any other
// response content are discarded at decode time.
const (
	CollectionSites       = "dcim/sites"
	CollectionLocations   = "dcim/locations"
	CollectionRacks       = "dcim/racks"
	CollectionDeviceTypes = "dcim/device-types"
	CollectionDevices     = "dcim/devices"
)

// Ref is a nested NetBox reference.
type Ref struct {
	ID int64 `json:"id"`
}

// SlugRef is a nested reference that carries a slug.
type SlugRef struct {
	ID   int64  `json:"id"`
	Slug string `json:"slug,omitempty"`
}

// NamedRef is a nested reference that carries a name.
type NamedRef struct {
	ID   int64  `json:"id"`
	Name string `json:"name,omitempty"`
}

// Decimal decodes a NetBox decimal that may be serialized as a number or a
// numeric string.
type Decimal float64

// UnmarshalJSON implements json.Unmarshaler.
func (d *Decimal) UnmarshalJSON(data []byte) error {
	data = bytes.Trim(data, `"`)
	value, err := strconv.ParseFloat(string(data), 64)
	if err != nil {
		return errors.New("invalid decimal")
	}
	*d = Decimal(value)
	return nil
}

// Site is a dcim/sites record.
type Site struct {
	ID   int64  `json:"id"`
	Name string `json:"name,omitempty"`
}

// Location is a dcim/locations record.
type Location struct {
	ID   int64  `json:"id"`
	Name string `json:"name,omitempty"`
	Site *Ref   `json:"site,omitempty"`
}

// Rack is a dcim/racks record.
type Rack struct {
	ID       int64    `json:"id"`
	Name     string   `json:"name,omitempty"`
	Site     *Ref     `json:"site,omitempty"`
	Location *Ref     `json:"location,omitempty"`
	UHeight  *Decimal `json:"u_height,omitempty"`
}

// DeviceType is a dcim/device-types record.
type DeviceType struct {
	ID           int64     `json:"id"`
	Model        string    `json:"model,omitempty"`
	Slug         string    `json:"slug,omitempty"`
	Manufacturer *NamedRef `json:"manufacturer,omitempty"`
	UHeight      *Decimal  `json:"u_height,omitempty"`
	IsFullDepth  *bool     `json:"is_full_depth,omitempty"`
}

// Device is a dcim/devices record. NetBox 4 names the role "role"; NetBox 3
// names it "device_role". Both decode into Role.
type Device struct {
	ID         int64    `json:"id"`
	Name       string   `json:"name,omitempty"`
	DeviceType *Ref     `json:"device_type,omitempty"`
	Role       *SlugRef `json:"role,omitempty"`
	Site       *Ref     `json:"site,omitempty"`
	Location   *Ref     `json:"location,omitempty"`
	Rack       *Ref     `json:"rack,omitempty"`
	Serial     string   `json:"serial,omitempty"`
	AssetTag   string   `json:"asset_tag,omitempty"`
	// Position is the lowest rack unit the device occupies, and Face the
	// rack face it is mounted on.
	Position *Decimal `json:"position,omitempty"`
	Face     *Choice  `json:"face,omitempty"`
	// OOBIP is the device's out-of-band management address.
	OOBIP *IPAddressRef `json:"oob_ip,omitempty"`
}

// IPAddressRef is a nested ipam/ip-addresses reference. NetBox's nested form
// carries no VRF, so the full record is read from ipam/ip-addresses.
type IPAddressRef struct {
	ID      int64  `json:"id,omitempty"`
	Address string `json:"address,omitempty"`
}

// IPAddress is an ipam/ip-addresses record. VRF is nil in the global table.
type IPAddress struct {
	ID      int64  `json:"id"`
	Address string `json:"address,omitempty"`
	VRF     *Ref   `json:"vrf,omitempty"`
}

// UnmarshalJSON accepts both role field names and null strings.
func (d *Device) UnmarshalJSON(data []byte) error {
	type plain Device
	var decoded struct {
		plain
		Name       *string  `json:"name"`
		AssetTag   *string  `json:"asset_tag"`
		Serial     *string  `json:"serial"`
		DeviceRole *SlugRef `json:"device_role"`
	}
	if err := json.Unmarshal(data, &decoded); err != nil {
		return err
	}
	*d = Device(decoded.plain)
	d.Name, d.AssetTag, d.Serial = deref(decoded.Name), deref(decoded.AssetTag), deref(decoded.Serial)
	if d.Role == nil {
		d.Role = decoded.DeviceRole
	}
	return nil
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// Snapshot is one complete, sequential read of the supported collections. It
// is not a transactional database snapshot: counts are checked per
// collection, but content can change between collections.
type Snapshot struct {
	ObservedAt  time.Time    `json:"-"`
	Sites       []Site       `json:"sites"`
	Locations   []Location   `json:"locations"`
	Racks       []Rack       `json:"racks"`
	DeviceTypes []DeviceType `json:"deviceTypes"`
	Devices     []Device     `json:"devices"`
	// Components holds each component collection's records, keyed by
	// collection.
	Components  map[string][]Component `json:"components"`
	Cables      []Cable                `json:"cables"`
	PowerPanels []PowerPanel           `json:"powerPanels"`
	PowerFeeds  []PowerFeed            `json:"powerFeeds"`
	// IPAddresses are the devices' out-of-band IP records.
	IPAddresses []IPAddress `json:"ipAddresses"`
	// Manufacturers and Templates are read only for the device types the
	// scoped devices use. Templates are keyed by collection.
	Manufacturers []Manufacturer                 `json:"manufacturers"`
	Templates     map[string][]ComponentTemplate `json:"templates"`
}

// Digest is the sha256 of the canonical JSON of the retained fields.
func (s *Snapshot) Digest() string {
	data, _ := json.Marshal(s)
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// Counts returns the number of records per collection.
func (s *Snapshot) Counts() map[string]int32 {
	counts := map[string]int32{
		CollectionSites:         int32(len(s.Sites)),
		CollectionLocations:     int32(len(s.Locations)),
		CollectionRacks:         int32(len(s.Racks)),
		CollectionDeviceTypes:   int32(len(s.DeviceTypes)),
		CollectionDevices:       int32(len(s.Devices)),
		CollectionCables:        int32(len(s.Cables)),
		CollectionPowerPanels:   int32(len(s.PowerPanels)),
		CollectionPowerFeeds:    int32(len(s.PowerFeeds)),
		CollectionIPAddresses:   int32(len(s.IPAddresses)),
		CollectionManufacturers: int32(len(s.Manufacturers)),
	}
	for _, collection := range TemplateCollections {
		counts[collection] = int32(len(s.Templates[collection]))
	}
	for _, collection := range ComponentCollections {
		counts[collection] = int32(len(s.Components[collection]))
	}
	return counts
}

// Scope narrows a snapshot to NetBox sites.
type Scope struct {
	SiteIDs []int64
}

func (s Scope) filter(key string) url.Values {
	if len(s.SiteIDs) == 0 {
		return nil
	}
	values := url.Values{}
	for _, id := range s.SiteIDs {
		values.Add(key, strconv.FormatInt(id, 10))
	}
	return values
}

// Reader reads snapshots. *Client implements it.
type Reader interface {
	Snapshot(ctx context.Context, scope Scope, now time.Time) (*Snapshot, error)
}

// Snapshot reads every supported collection, sequentially. Any failure
// returns an error and no snapshot.
func (c *Client) Snapshot(ctx context.Context, scope Scope, now time.Time) (*Snapshot, error) {
	snapshot := &Snapshot{ObservedAt: now.UTC().Truncate(time.Second), Components: map[string][]Component{}, Templates: map[string][]ComponentTemplate{}}
	steps := []struct {
		collection string
		filter     url.Values
		decode     func([]json.RawMessage) error
	}{
		{CollectionSites, scope.filter("id"), func(raw []json.RawMessage) (err error) { snapshot.Sites, err = decodeAll[Site](raw); return }},
		{CollectionLocations, scope.filter("site_id"), func(raw []json.RawMessage) (err error) {
			snapshot.Locations, err = decodeAll[Location](raw)
			return
		}},
		{CollectionRacks, scope.filter("site_id"), func(raw []json.RawMessage) (err error) { snapshot.Racks, err = decodeAll[Rack](raw); return }},
		{CollectionDeviceTypes, nil, func(raw []json.RawMessage) (err error) {
			snapshot.DeviceTypes, err = decodeAll[DeviceType](raw)
			return
		}},
		{CollectionDevices, scope.filter("site_id"), func(raw []json.RawMessage) (err error) {
			snapshot.Devices, err = decodeAll[Device](raw)
			return
		}},
	}
	for _, collection := range ComponentCollections {
		steps = append(steps, struct {
			collection string
			filter     url.Values
			decode     func([]json.RawMessage) error
		}{collection, scope.filter("site_id"), func(raw []json.RawMessage) (err error) {
			snapshot.Components[collection], err = decodeAll[Component](raw)
			return
		}})
	}
	steps = append(steps, []struct {
		collection string
		filter     url.Values
		decode     func([]json.RawMessage) error
	}{
		{CollectionPowerPanels, scope.filter("site_id"), func(raw []json.RawMessage) (err error) {
			snapshot.PowerPanels, err = decodeAll[PowerPanel](raw)
			return
		}},
		{CollectionPowerFeeds, scope.filter("site_id"), func(raw []json.RawMessage) (err error) {
			snapshot.PowerFeeds, err = decodeAll[PowerFeed](raw)
			return
		}},
		{CollectionCables, scope.filter("site_id"), func(raw []json.RawMessage) (err error) { snapshot.Cables, err = decodeAll[Cable](raw); return }},
	}...)
	for _, step := range steps {
		raw, err := c.List(ctx, step.collection, step.filter)
		if err != nil {
			return nil, err
		}
		if err := step.decode(raw); err != nil {
			return nil, fmt.Errorf("%s: %w", step.collection, err)
		}
	}
	// Records the scoped devices refer to are read by ID, a bounded number
	// per request: their out-of-band IPs, and the manufacturers and component
	// templates of the device types they use.
	var ipIDs, typeIDs, manufacturerIDs []string
	usedType, usedManufacturer := map[int64]bool{}, map[int64]bool{}
	for _, d := range snapshot.Devices {
		if d.OOBIP != nil && d.OOBIP.ID > 0 {
			ipIDs = append(ipIDs, strconv.FormatInt(d.OOBIP.ID, 10))
		}
		if d.DeviceType != nil && !usedType[d.DeviceType.ID] {
			usedType[d.DeviceType.ID] = true
			typeIDs = append(typeIDs, strconv.FormatInt(d.DeviceType.ID, 10))
		}
	}
	for _, t := range snapshot.DeviceTypes {
		if usedType[t.ID] && t.Manufacturer != nil && !usedManufacturer[t.Manufacturer.ID] {
			usedManufacturer[t.Manufacturer.ID] = true
			manufacturerIDs = append(manufacturerIDs, strconv.FormatInt(t.Manufacturer.ID, 10))
		}
	}
	var err error
	if snapshot.IPAddresses, err = listByID[IPAddress](ctx, c, CollectionIPAddresses, "id", ipIDs); err != nil {
		return nil, err
	}
	if snapshot.Manufacturers, err = listByID[Manufacturer](ctx, c, CollectionManufacturers, "id", manufacturerIDs); err != nil {
		return nil, err
	}
	for _, collection := range TemplateCollections {
		if snapshot.Templates[collection], err = listByID[ComponentTemplate](ctx, c, collection, "device_type_id", typeIDs); err != nil {
			return nil, err
		}
	}
	return snapshot, nil
}

// idBatch bounds the IDs in one request.
const idBatch = 100

// listByID reads the records of a collection whose key matches one of ids,
// idBatch IDs per read. No IDs reads nothing.
func listByID[T any](ctx context.Context, c *Client, collection, key string, ids []string) ([]T, error) {
	out := []T{}
	for start := 0; start < len(ids); start += idBatch {
		raw, err := c.List(ctx, collection, url.Values{key: ids[start:min(start+idBatch, len(ids))]})
		if err != nil {
			return nil, err
		}
		batch, err := decodeAll[T](raw)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", collection, err)
		}
		out = append(out, batch...)
	}
	return out, nil
}

func decodeAll[T any](raw []json.RawMessage) ([]T, error) {
	out := make([]T, 0, len(raw))
	for _, item := range raw {
		var record T
		if err := json.Unmarshal(item, &record); err != nil {
			return nil, errors.New("record has an unexpected field type")
		}
		out = append(out, record)
	}
	return out, nil
}
