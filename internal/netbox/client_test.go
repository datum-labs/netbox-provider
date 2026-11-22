// SPDX-License-Identifier: AGPL-3.0-only

package netbox_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.miloapis.com/netbox-provider/internal/netbox"
	"go.miloapis.com/netbox-provider/internal/netbox/netboxtest"
)

// newTLSServer starts a local TLS server whose connection-teardown noise is
// discarded.
func newTLSServer(handler http.Handler) *httptest.Server {
	server := httptest.NewUnstartedServer(handler)
	server.Config.ErrorLog = log.New(io.Discard, "", 0)
	server.StartTLS()
	return server
}

func newClient(t *testing.T, server *httptest.Server, base string, limits netbox.Limits) *netbox.Client {
	t.Helper()
	client, err := netbox.New(netbox.Config{BaseURL: base, Token: "synthetic", Scheme: "Bearer", HTTPClient: server.Client(), Limits: limits})
	if err != nil {
		t.Fatal(err)
	}
	return client
}

func TestPaginationPrefixAndConcurrentReads(t *testing.T) {
	for _, scheme := range []string{"Bearer", "Token"} {
		t.Run(scheme, func(t *testing.T) {
			server := newTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet || r.Header.Get("Authorization") != scheme+" synthetic" || r.URL.Path != "/prefix/api/dcim/devices/" {
					t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
				}
				if r.URL.Query().Get("offset") == "" {
					_, _ = fmt.Fprint(w, `{"count":2,"next":"?limit=200&offset=1&ordering=id","results":[{"id":2}]}`)
				} else {
					_, _ = fmt.Fprint(w, `{"count":2,"next":null,"results":[{"id":1}]}`)
				}
			}))
			defer server.Close()
			client, err := netbox.New(netbox.Config{BaseURL: server.URL + "/prefix/", Token: "synthetic", Scheme: scheme, HTTPClient: server.Client()})
			if err != nil {
				t.Fatal(err)
			}
			var group sync.WaitGroup
			for range 8 {
				group.Go(func() {
					records, err := client.List(context.Background(), netbox.CollectionDevices, nil)
					if err != nil || len(records) != 2 || !strings.Contains(string(records[0]), `"id":1`) {
						t.Errorf("records=%s err=%v", records, err)
					}
				})
			}
			group.Wait()
		})
	}
}

func TestUnsafePaginationLinks(t *testing.T) {
	for _, next := range []string{
		"https://untrusted.invalid/api/dcim/devices/", "http://untrusted.invalid/api/dcim/devices/",
		"/api/users/tokens/", "?token=synthetic", "?limit=%zz", "?limit=200&ordering=-id",
		"/api/dcim/devices/#fragment", "?start=100", "/api/dcim/%64evices/", "?limit=200&offset=1&site_id=9",
		"?limit=200&limit=3",
	} {
		t.Run(next, func(t *testing.T) {
			var calls atomic.Int32
			server := newTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				_, _ = fmt.Fprintf(w, `{"count":2,"next":%q,"results":[{"id":1}]}`, next)
			}))
			defer server.Close()
			records, err := newClient(t, server, server.URL, netbox.Limits{}).List(context.Background(), netbox.CollectionDevices, nil)
			if err == nil || records != nil || calls.Load() != 1 {
				t.Fatalf("records=%v err=%v calls=%d", records, err, calls.Load())
			}
		})
	}
}

func TestScopeFilterMustSurvivePagination(t *testing.T) {
	for name, next := range map[string]string{
		"dropped": "?limit=200&offset=1&ordering=id",
		"changed": "?limit=200&offset=1&ordering=id&site_id=1&site_id=3",
		"kept":    "?limit=200&offset=1&ordering=id&site_id=2&site_id=1",
	} {
		t.Run(name, func(t *testing.T) {
			server := newTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if got := r.URL.Query()["site_id"]; len(got) != 2 {
					t.Errorf("filter not sent: %v", got)
				}
				if r.URL.Query().Get("offset") == "" {
					_, _ = fmt.Fprintf(w, `{"count":2,"next":%q,"results":[{"id":1}]}`, next)
				} else {
					_, _ = fmt.Fprint(w, `{"count":2,"next":null,"results":[{"id":2}]}`)
				}
			}))
			defer server.Close()
			filter := url.Values{"site_id": {"1", "2"}}
			records, err := newClient(t, server, server.URL, netbox.Limits{}).List(context.Background(), netbox.CollectionDevices, filter)
			if (name == "kept") != (err == nil) {
				t.Fatalf("records=%d err=%v", len(records), err)
			}
		})
	}
}

func TestErrorsNeverReturnPartialRecordsOrBodies(t *testing.T) {
	for _, status := range []int{301, 302, 401, 403, 404, 429, 500, 503} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			var calls atomic.Int32
			server := newTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if calls.Add(1) == 1 {
					_, _ = fmt.Fprint(w, `{"count":2,"next":"?offset=1","results":[{"id":1}]}`)
					return
				}
				w.Header().Set("Location", "/secret")
				w.WriteHeader(status)
				_, _ = fmt.Fprint(w, "synthetic-sensitive-response")
			}))
			defer server.Close()
			records, err := newClient(t, server, server.URL, netbox.Limits{}).List(context.Background(), netbox.CollectionDevices, nil)
			if err == nil || records != nil || calls.Load() != 2 || strings.Contains(err.Error(), "sensitive") || strings.Contains(err.Error(), "synthetic") {
				t.Fatalf("records=%v err=%v calls=%d", records, err, calls.Load())
			}
			rejected := status == http.StatusUnauthorized || status == http.StatusForbidden
			if errors.Is(err, netbox.ErrCredentialsRejected) != rejected {
				t.Fatalf("status %d: credentials rejected = %v, want %v", status, !rejected, rejected)
			}
		})
	}
}

func TestCountAndIdentityConsistency(t *testing.T) {
	for name, pages := range map[string][]string{
		"count changes":     {`{"count":2,"next":"?offset=1","results":[{"id":1}]}`, `{"count":3,"next":null,"results":[{"id":2}]}`},
		"short total":       {`{"count":3,"next":"?offset=1","results":[{"id":1}]}`, `{"count":3,"next":null,"results":[{"id":2}]}`},
		"over count":        {`{"count":1,"next":null,"results":[{"id":1},{"id":2}]}`},
		"duplicate id":      {`{"count":2,"next":"?offset=1","results":[{"id":1}]}`, `{"count":2,"next":null,"results":[{"id":1}]}`},
		"empty nonterminal": {`{"count":1,"next":"?offset=1","results":[]}`},
		"string id":         {`{"count":1,"next":null,"results":[{"id":"1"}]}`},
		"zero id":           {`{"count":1,"next":null,"results":[{"id":0}]}`},
		"missing id":        {`{"count":1,"next":null,"results":[{"name":"x"}]}`},
		"cycle":             {`{"count":3,"next":"?offset=1","results":[{"id":1}]}`, `{"count":3,"next":"?offset=1","results":[{"id":2}]}`},
	} {
		t.Run(name, func(t *testing.T) {
			var calls atomic.Int32
			server := newTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = fmt.Fprint(w, pages[min(int(calls.Add(1))-1, len(pages)-1)])
			}))
			defer server.Close()
			if records, err := newClient(t, server, server.URL, netbox.Limits{}).List(context.Background(), netbox.CollectionDevices, nil); err == nil || records != nil {
				t.Fatalf("accepted inconsistent collection: %s", records)
			}
		})
	}
}

func TestDecodePageRejectsMalformedAndOversizedInput(t *testing.T) {
	for _, body := range []string{
		`{}`, `null`, `[]`, `{"count":0,"next":null,"results":null}`, `{"count":0,"next":null,"results":{}}`,
		`{"count":0,"next":"","results":[]}`, `{"count":0,"next":false,"results":[]}`, `{"count":0,"results":[]}`,
		`{"next":null,"results":[]}`, `{"count":-1,"next":null,"results":[]}`, `{"count":"1","next":null,"results":[]}`,
		`{"count":0,"next":null,"results":[]} {}`, strings.Repeat(" ", 1025),
	} {
		if _, err := netbox.DecodePage(strings.NewReader(body), 1024); err == nil {
			t.Fatalf("accepted malformed input %.40q", body)
		}
	}
}

func TestConfiguration(t *testing.T) {
	for _, base := range []string{
		"", "http://example.invalid", "https://user:secret@example.invalid", "https://example.invalid?token=secret",
		"https://example.invalid#fragment", "https://example.invalid/%2e", "https://example.invalid/../", "https://example.invalid/api",
		"https://example.invalid/prefix/api/", "https://", "ftp://example.invalid",
	} {
		if _, err := netbox.New(netbox.Config{BaseURL: base, Token: "synthetic", Scheme: "Bearer"}); err == nil {
			t.Fatalf("accepted unsafe URL %q", base)
		}
	}
	for _, token := range []string{"", "secret\n", "secret token", "\t"} {
		if _, err := netbox.New(netbox.Config{BaseURL: "https://example.invalid", Token: token, Scheme: "Bearer"}); err == nil {
			t.Fatal("accepted invalid token")
		}
	}
	if _, err := netbox.New(netbox.Config{BaseURL: "https://example.invalid", Token: "synthetic", Scheme: "Basic"}); err == nil {
		t.Fatal("accepted unsupported auth")
	}
	client, err := netbox.New(netbox.Config{BaseURL: "https://example.invalid", Token: "synthetic", Scheme: "Bearer"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.List(context.Background(), "users/tokens", nil); err == nil {
		t.Fatal("read an unsupported collection")
	}
	if _, err := client.List(context.Background(), netbox.CollectionDevices, url.Values{"offset": {"9"}}); err == nil {
		t.Fatal("accepted a filter that overrides pagination")
	}
}

func TestRedirectsAreNotFollowed(t *testing.T) {
	var followed atomic.Bool
	server := newTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/elsewhere" {
			followed.Store(true)
		}
		http.Redirect(w, r, "/elsewhere", http.StatusFound)
	}))
	defer server.Close()
	if _, err := newClient(t, server, server.URL, netbox.Limits{}).List(context.Background(), netbox.CollectionSites, nil); err == nil || followed.Load() {
		t.Fatalf("redirect followed=%v err=%v", followed.Load(), err)
	}
}

func TestCancellation(t *testing.T) {
	server := newTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { <-r.Context().Done() }))
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if records, err := newClient(t, server, server.URL, netbox.Limits{}).List(ctx, netbox.CollectionDevices, nil); err == nil || records != nil {
		t.Fatal("cancelled collection returned records")
	}
}

func TestPageAndRecordBounds(t *testing.T) {
	var calls atomic.Int32
	server := newTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := calls.Add(1)
		_, _ = fmt.Fprintf(w, `{"count":100000,"next":"?offset=%d","results":[{"id":%d},{"id":%d}]}`, n, 2*n, 2*n+1)
	}))
	defer server.Close()
	limits := netbox.Limits{PageBytes: 1 << 20, CollectionBytes: 64 << 20, Pages: 5, Records: 1000}
	if _, err := newClient(t, server, server.URL, limits).List(context.Background(), netbox.CollectionDevices, nil); err == nil || calls.Load() != 5 {
		t.Fatalf("page limit not enforced: calls=%d err=%v", calls.Load(), err)
	}
	calls.Store(0)
	limits = netbox.Limits{PageBytes: 1 << 20, CollectionBytes: 64 << 20, Pages: 100, Records: 5}
	if _, err := newClient(t, server, server.URL, limits).List(context.Background(), netbox.CollectionDevices, nil); err == nil || !strings.Contains(err.Error(), "record limit") {
		t.Fatalf("record limit not enforced: %v", err)
	}
}

func TestByteBudgets(t *testing.T) {
	server := newTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := len(r.URL.Query().Get("offset"))
		_, _ = fmt.Fprintf(w, `{"count":3,"next":"?offset=1%s","results":[{"id":%d,"pad":%q}]}`, strings.Repeat("1", n), n+1, strings.Repeat("x", 300))
	}))
	defer server.Close()
	limits := netbox.Limits{PageBytes: 200, CollectionBytes: 1 << 20, Pages: 10, Records: 10}
	if _, err := newClient(t, server, server.URL, limits).List(context.Background(), netbox.CollectionDevices, nil); err == nil || !strings.Contains(err.Error(), "page exceeds") {
		t.Fatalf("page byte limit not enforced: %v", err)
	}
	limits = netbox.Limits{PageBytes: 1 << 20, CollectionBytes: 700, Pages: 10, Records: 10}
	if _, err := newClient(t, server, server.URL, limits).List(context.Background(), netbox.CollectionDevices, nil); err == nil || !strings.Contains(err.Error(), "collection byte limit") {
		t.Fatalf("collection byte limit not enforced: %v", err)
	}
}

func TestSnapshotAllowlistsFieldsAndIsStable(t *testing.T) {
	data, err := netboxtest.LoadFixture()
	if err != nil {
		t.Fatal(err)
	}
	server := netboxtest.New(data)
	defer server.Close()
	client := newClient(t, server.Server, server.RootURL(), netbox.Limits{})
	now := time.Date(2026, 1, 2, 3, 4, 5, 600, time.UTC)
	first, err := client.Snapshot(context.Background(), netbox.Scope{}, now)
	if err != nil {
		t.Fatal(err)
	}
	second, err := client.Snapshot(context.Background(), netbox.Scope{}, now.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if first.Digest() != second.Digest() || !strings.HasPrefix(first.Digest(), "sha256:") || len(first.Digest()) != 71 {
		t.Fatalf("digest unstable or malformed: %s %s", first.Digest(), second.Digest())
	}
	if !first.ObservedAt.Equal(now.Truncate(time.Second)) {
		t.Fatalf("observedAt %v", first.ObservedAt)
	}
	encoded, _ := json.Marshal(first)
	for _, forbidden := range []string{"must-not-be-retained", "synthetic comment", "https://", "1000BASE-T", "\"Front\"", "mac_address", "comments"} {
		if strings.Contains(string(encoded), forbidden) {
			t.Errorf("snapshot retained non-allowlisted content %q", forbidden)
		}
	}
	counts := first.Counts()
	if counts[netbox.CollectionDevices] != 9 || counts[netbox.CollectionRacks] != 2 || counts[netbox.CollectionDeviceTypes] != 5 ||
		counts[netbox.CollectionInterfaces] != 9 || counts[netbox.CollectionPowerOutlets] != 2 || counts[netbox.CollectionCables] != 8 ||
		counts[netbox.CollectionPowerFeeds] != 3 || counts[netbox.CollectionPowerPanels] != 1 || counts[netbox.CollectionRearPorts] != 0 {
		t.Fatalf("counts %v", counts)
	}
	outlet := first.Components[netbox.CollectionPowerOutlets][0]
	if outlet.Device == nil || outlet.Device.ID != 1008 || outlet.PowerPort == nil || outlet.PowerPort.ID != 3010 || netbox.ChoiceValue(outlet.Type) != "iec-60320-c13" {
		t.Errorf("power outlet decoded as %+v", outlet)
	}
	cable := first.Cables[0]
	if cable.Type != "dac-passive" || netbox.ChoiceValue(cable.Status) != "connected" || len(cable.ATerminations) != 1 ||
		cable.ATerminations[0] != (netbox.Termination{ObjectType: "dcim.interface", ObjectID: 2001}) {
		t.Errorf("cable decoded as %+v", cable)
	}
	if first.Cables[1].Type != "" {
		t.Error("a null cable type was not decoded as empty")
	}
	feed := first.PowerFeeds[0]
	if feed.PowerPanel == nil || feed.PowerPanel.ID != 7000 || netbox.ChoiceValue(feed.Supply) != "ac" || feed.Voltage == nil || *feed.Voltage != 208 {
		t.Errorf("power feed decoded as %+v", feed)
	}
	byID := map[int64]netbox.Device{}
	for _, d := range first.Devices {
		byID[d.ID] = d
	}
	if byID[1002].Role == nil || byID[1002].Role.Slug != "server" {
		t.Error("NetBox 3 device_role was not decoded as the role")
	}
	if byID[1001].AssetTag != "" || byID[1000].AssetTag != "SYNTH-ASSET-0001" {
		t.Error("asset tags decoded incorrectly")
	}
	for _, dt := range first.DeviceTypes {
		if dt.ID == 501 && (dt.UHeight == nil || *dt.UHeight != 2) {
			t.Error("string decimal was not decoded")
		}
	}

	scoped, err := client.Snapshot(context.Background(), netbox.Scope{SiteIDs: []int64{2}}, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(scoped.Sites) != 1 || len(scoped.Devices) != 1 || len(scoped.Racks) != 1 || len(scoped.DeviceTypes) != 5 {
		t.Fatalf("scope not applied: sites=%d devices=%d racks=%d", len(scoped.Sites), len(scoped.Devices), len(scoped.Racks))
	}
	for _, auth := range server.Authorizations() {
		if auth != "Bearer synthetic" {
			t.Fatalf("unexpected authorization %q", auth)
		}
	}
}

func TestSnapshotFailsWhollyOnAnyCollectionError(t *testing.T) {
	data, err := netboxtest.LoadFixture()
	if err != nil {
		t.Fatal(err)
	}
	server := netboxtest.New(data)
	defer server.Close()
	server.Fail(netbox.CollectionDevices, http.StatusBadGateway)
	snapshot, err := newClient(t, server.Server, server.RootURL(), netbox.Limits{}).Snapshot(context.Background(), netbox.Scope{}, time.Now())
	if err == nil || snapshot != nil || strings.Contains(err.Error(), "sensitive") {
		t.Fatalf("snapshot=%v err=%v", snapshot, err)
	}
}
