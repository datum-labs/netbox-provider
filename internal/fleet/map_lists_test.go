// SPDX-License-Identifier: AGPL-3.0-only

package fleet_test

import (
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"

	"go.miloapis.com/netbox-provider/internal/fleet"
)

// TestMapListsMatchTheVendoredCRD keeps fleet.MapLists equal to every
// x-kubernetes-list-type: map list in the spec of each written kind, and
// checks none sits inside another list, where the comparison does not look.
func TestMapListsMatchTheVendoredCRD(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "test", "fleet", "crds", "fleet.datumapis.com.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]map[string][]string{}
	for _, doc := range strings.Split(string(data), "\n---") {
		var crd struct {
			Spec struct {
				Names    struct{ Kind string }
				Versions []struct {
					Name   string
					Schema struct {
						OpenAPIV3Schema map[string]any `json:"openAPIV3Schema"`
					}
				}
			}
		}
		if err := yaml.Unmarshal([]byte(doc), &crd); err != nil {
			t.Fatal(err)
		}
		if !slices.Contains(fleet.WriteKinds, crd.Spec.Names.Kind) {
			continue
		}
		for _, version := range crd.Spec.Versions {
			if version.Name != fleet.Version {
				continue
			}
			spec, _ := version.Schema.OpenAPIV3Schema["properties"].(map[string]any)["spec"].(map[string]any)
			walk(t, crd.Spec.Names.Kind, "spec", spec, false, got)
		}
	}
	if !reflect.DeepEqual(got, fleet.MapLists) {
		t.Fatalf("fleet.MapLists = %v, vendored CRD declares %v", fleet.MapLists, got)
	}
}

func walk(t *testing.T, kind, path string, schema map[string]any, inList bool, out map[string]map[string][]string) {
	if schema["type"] == "array" {
		if schema["x-kubernetes-list-type"] == "map" {
			if inList {
				t.Errorf("%s %s is a map-list inside another list", kind, path)
			}
			var keys []string
			for _, key := range schema["x-kubernetes-list-map-keys"].([]any) {
				keys = append(keys, key.(string))
			}
			if out[kind] == nil {
				out[kind] = map[string][]string{}
			}
			out[kind][path] = keys
		}
		if items, ok := schema["items"].(map[string]any); ok {
			walk(t, kind, path+"[]", items, true, out)
		}
		return
	}
	properties, _ := schema["properties"].(map[string]any)
	for name, child := range properties {
		walk(t, kind, path+"."+name, child.(map[string]any), inList, out)
	}
}
