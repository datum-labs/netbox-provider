// SPDX-License-Identifier: AGPL-3.0-only

package plan

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// This controller's correlation metadata on the Fleet records it creates.
// Fleet ignores metadata, so none of this is a Fleet API concept. Labels
// carry what the controller selects and matches on; annotations carry the
// observation a record was created from.
const (
	// LabelSource is the NetBoxSource's sourceID.
	LabelSource = "netbox.miloapis.com/source"
	// LabelType is the NetBox collection, with "/" written as "." so it is
	// a valid label value, for example dcim.devices.
	LabelType = "netbox.miloapis.com/type"
	// LabelID is the NetBox record ID.
	LabelID = "netbox.miloapis.com/id"

	AnnotationSnapshotDigest  = "netbox.miloapis.com/snapshot-digest"
	AnnotationMappingRevision = "netbox.miloapis.com/mapping-revision"
	AnnotationObservedAt      = "netbox.miloapis.com/observed-at"
)

// SourceKey identifies one NetBox record of one source.
type SourceKey struct {
	// Source is the sourceID.
	Source string
	// Type is the NetBox collection, such as dcim/devices.
	Type string
	// ID is the NetBox record ID.
	ID string
}

// String is the record's key within the source, such as dcim/devices/12.
func (k SourceKey) String() string { return k.Type + "/" + k.ID }

// qualified includes the source, for indexing records across sources.
func (k SourceKey) qualified() string { return k.Source + "/" + k.Type + "/" + k.ID }

// Labels returns the correlation labels.
func (k SourceKey) Labels() map[string]string {
	return map[string]string{LabelSource: k.Source, LabelType: strings.ReplaceAll(k.Type, "/", "."), LabelID: k.ID}
}

// KeyOf returns the correlation key of a Fleet record, and false when the
// record does not carry this controller's labels. Records without them were
// not written by this controller and are never adopted.
func KeyOf(obj *unstructured.Unstructured) (SourceKey, bool) {
	labels := obj.GetLabels()
	key := SourceKey{Source: labels[LabelSource], Type: strings.ReplaceAll(labels[LabelType], ".", "/"), ID: labels[LabelID]}
	return key, key.Source != "" && key.Type != "" && key.ID != ""
}

// NamePrefix starts every name this controller generates.
const NamePrefix = "netbox-"

// Name is this controller's naming rule: "netbox-" and the first 24 hex digits
// of the sha256 of the versioned, newline-separated tuple (sourceID,
// collection, id, Fleet kind). It depends only on the source record and the
// kind, never on mutable NetBox names, so NetBox renames never rename Fleet
// records, and a claim and device from one NetBox device get distinct names.
// Changing the sourceID changes every name, which is why it is immutable.
// This is not a Fleet convention.
func Name(key SourceKey, kind string) string {
	sum := sha256.Sum256([]byte(fmt.Sprintf("netbox-provider-name/v1\n%s\n%s\n%s\n%s", key.Source, key.Type, key.ID, kind)))
	return NamePrefix + hex.EncodeToString(sum[:])[:24]
}
