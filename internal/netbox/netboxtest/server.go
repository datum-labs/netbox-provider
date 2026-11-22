// SPDX-License-Identifier: AGPL-3.0-only

// Package netboxtest serves synthetic NetBox collections over local TLS for
// tests. It is never used against a real NetBox.
package netboxtest

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
)

// Prefix is the installation path prefix the server uses, to exercise
// prefix handling.
const Prefix = "/netbox"

// Server is a fake NetBox.
type Server struct {
	*httptest.Server

	mu       sync.Mutex
	data     map[string][]map[string]any
	pageSize int
	failWith map[string]int
	requests atomic.Int32
	auth     []string
}

// Collections served, matching the reader's collections.
var Collections = []string{
	"dcim/sites", "dcim/locations", "dcim/racks", "dcim/device-types", "dcim/devices",
	"dcim/interfaces", "dcim/power-ports", "dcim/power-outlets", "dcim/console-ports", "dcim/console-server-ports",
	"dcim/front-ports", "dcim/rear-ports", "dcim/modules", "dcim/inventory-items", "dcim/device-bays",
	"dcim/power-panels", "dcim/power-feeds", "dcim/cables", "ipam/ip-addresses",
}

// siteFiltered are the collections whose records carry a site, which the
// fake filters by site_id. NetBox resolves the filter through the device or
// panel for the others; the fixtures hold only in-scope records for those.
var siteFiltered = map[string]bool{"dcim/locations": true, "dcim/racks": true, "dcim/devices": true, "dcim/power-panels": true}

// FixtureDir is the directory of the synthetic fixture collections.
func FixtureDir() string {
	_, file, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(file), "..", "..", "..", "test", "fixtures", "netbox")
}

// LoadFixture reads the synthetic collections from FixtureDir.
func LoadFixture() (map[string][]map[string]any, error) {
	data := map[string][]map[string]any{}
	for _, collection := range Collections {
		raw, err := os.ReadFile(filepath.Join(FixtureDir(), strings.ReplaceAll(collection, "/", "_")+".json"))
		if err != nil {
			return nil, err
		}
		var records []map[string]any
		if err := json.Unmarshal(raw, &records); err != nil {
			return nil, fmt.Errorf("%s: %w", collection, err)
		}
		data[collection] = records
	}
	return data, nil
}

// New starts a server with the given collections and a small page size so
// pagination is exercised.
func New(data map[string][]map[string]any) *Server {
	s := &Server{data: data, pageSize: 2, failWith: map[string]int{}}
	s.Server = httptest.NewUnstartedServer(http.HandlerFunc(s.serve))
	s.Config.ErrorLog = log.New(io.Discard, "", 0)
	s.StartTLS()
	return s
}

// URL returns the installation root including the prefix.
func (s *Server) RootURL() string { return s.URL + Prefix }

// Requests returns the number of requests served.
func (s *Server) Requests() int { return int(s.requests.Load()) }

// Authorizations returns every Authorization header received.
func (s *Server) Authorizations() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.auth)
}

// Fail makes a collection answer with an HTTP status; 0 clears it.
func (s *Server) Fail(collection string, status int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.failWith[collection] = status
}

// Set changes one field of a record.
func (s *Server) Set(collection string, id int, field string, value any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, r := range s.data[collection] {
		if int(r["id"].(float64)) == id {
			r[field] = value
		}
	}
}

// Remove deletes a record from a collection.
func (s *Server) Remove(collection string, id int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.data[collection] = slices.DeleteFunc(s.data[collection], func(r map[string]any) bool {
		return int(r["id"].(float64)) == id
	})
}

func (s *Server) serve(w http.ResponseWriter, r *http.Request) {
	s.requests.Add(1)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.auth = append(s.auth, r.Header.Get("Authorization"))
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	collection := strings.Trim(strings.TrimPrefix(r.URL.Path, Prefix+"/api/"), "/")
	records, ok := s.data[collection]
	if !ok || !strings.HasPrefix(r.URL.Path, Prefix+"/api/") {
		http.NotFound(w, r)
		return
	}
	if status := s.failWith[collection]; status != 0 {
		w.WriteHeader(status)
		_, _ = fmt.Fprint(w, "synthetic-sensitive-error-body")
		return
	}
	query := r.URL.Query()
	filtered := filter(records, collection, query)
	offset, _ := strconv.Atoi(query.Get("offset"))
	end := min(offset+s.pageSize, len(filtered))
	if offset > len(filtered) {
		offset = len(filtered)
	}
	var next *string
	if end < len(filtered) {
		nextQuery := url.Values{}
		for key, values := range query {
			nextQuery[key] = values
		}
		nextQuery.Set("limit", strconv.Itoa(s.pageSize))
		nextQuery.Set("offset", strconv.Itoa(end))
		link := s.URL + r.URL.Path + "?" + nextQuery.Encode()
		next = &link
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"count": len(filtered), "next": next, "previous": nil, "results": filtered[offset:end]})
}

func filter(records []map[string]any, collection string, query url.Values) []map[string]any {
	key, field := "site_id", "site"
	if collection == "dcim/sites" || collection == "ipam/ip-addresses" {
		key, field = "id", ""
	}
	wanted := query[key]
	if len(wanted) == 0 || (field != "" && !siteFiltered[collection]) {
		return records
	}
	out := []map[string]any{}
	for _, record := range records {
		id := record["id"]
		if field != "" {
			ref, _ := record[field].(map[string]any)
			if ref == nil {
				continue
			}
			id = ref["id"]
		}
		if slices.Contains(wanted, fmt.Sprint(id)) {
			out = append(out, record)
		}
	}
	return out
}
