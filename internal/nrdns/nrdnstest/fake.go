// Package nrdnstest is an in-memory fake of the NodeRings DNS API for tests.
package nrdnstest

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"

	"github.com/noderings/external-dns-noderings-webhook/internal/nrdns"
)

// Token is the bearer token the fake accepts.
const Token = "test-token"

// API is a fake NodeRings DNS API.
type API struct {
	*httptest.Server

	mu      sync.Mutex
	zones   map[string]*zone // by ID
	nextID  int
	Writes  int // successful record writes (PUT and DELETE)
	FailPut map[string]int
}

type zone struct {
	nrdns.Zone
	records map[string]nrdns.Record // by "name/TYPE"
}

// New starts a fake API. Close it with API.Close.
func New() *API {
	a := &API{zones: map[string]*zone{}, FailPut: map[string]int{}}
	a.Server = httptest.NewServer(http.HandlerFunc(a.serve))
	return a
}

// Client returns a client for the fake.
func (a *API) Client() *nrdns.Client {
	c, err := nrdns.New(nrdns.Config{Endpoint: a.URL, Token: Token, MinBackoff: 1})
	if err != nil {
		panic(err)
	}
	return c
}

// AddZone creates a zone with its apex SOA and NS system records.
func (a *API) AddZone(name string) string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.addZone(name)
}

func (a *API) addZone(name string) string {
	a.nextID++
	id := fmt.Sprintf("zone-%d", a.nextID)
	z := &zone{Zone: nrdns.Zone{ID: id, Name: nrdns.NormalizeName(name) + ".", State: "DNS_ZONE_STATE_ACTIVE"}, records: map[string]nrdns.Record{}}
	z.records["@/SOA"] = nrdns.Record{Name: "@", Type: "SOA", TTL: 3600, RData: []string{"ns1.nrings.io. hostmaster.nrings.io. 1 10800 3600 604800 3600"}, System: true}
	z.records["@/NS"] = nrdns.Record{Name: "@", Type: "NS", TTL: 3600, RData: []string{"ans1.nrings.io.", "ans2.nrings.io."}, System: true}
	a.zones[id] = z
	return id
}

// Records returns a zone's non-system record sets, sorted by name and type.
func (a *API) Records(zoneName string) []nrdns.Record {
	a.mu.Lock()
	defer a.mu.Unlock()
	var out []nrdns.Record
	for _, z := range a.zones {
		if nrdns.NormalizeName(z.Name) != nrdns.NormalizeName(zoneName) {
			continue
		}
		for _, r := range z.records {
			if !r.System {
				out = append(out, r)
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name+out[i].Type < out[j].Name+out[j].Type })
	return out
}

// Put stores a record set directly.
func (a *API) Put(zoneName string, r nrdns.Record) {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, z := range a.zones {
		if nrdns.NormalizeName(z.Name) == nrdns.NormalizeName(zoneName) {
			z.records[r.Name+"/"+r.Type] = r
		}
	}
}

func (a *API) serve(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Authorization") != "Bearer "+Token {
		writeErr(w, http.StatusUnauthorized, 16, "invalid token")
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/") // v1 dns zones [id] [records]
	if len(parts) < 3 || parts[0] != "v1" || parts[1] != "dns" || parts[2] != "zones" {
		writeErr(w, http.StatusNotFound, 5, "no route")
		return
	}
	switch {
	case len(parts) == 3 && r.Method == http.MethodGet:
		zones := []nrdns.Zone{}
		for _, z := range a.zones {
			zones = append(zones, z.Zone)
		}
		sort.Slice(zones, func(i, j int) bool { return zones[i].ID < zones[j].ID })
		writeJSON(w, map[string]any{"zones": zones})
	case len(parts) == 3 && r.Method == http.MethodPost:
		var body struct{ Name string }
		_ = json.NewDecoder(r.Body).Decode(&body)
		id := a.addZone(body.Name)
		writeJSON(w, map[string]any{"zone": a.zones[id].Zone})
	case len(parts) == 4 && r.Method == http.MethodDelete:
		delete(a.zones, parts[3])
		writeJSON(w, map[string]any{})
	case len(parts) == 5 && parts[4] == "records":
		z, ok := a.zones[parts[3]]
		if !ok {
			writeErr(w, http.StatusNotFound, 5, "zone not found")
			return
		}
		a.records(w, r, z)
	default:
		writeErr(w, http.StatusNotFound, 5, "no route")
	}
}

func (a *API) records(w http.ResponseWriter, r *http.Request, z *zone) {
	q := r.URL.Query()
	switch r.Method {
	case http.MethodGet:
		out := []nrdns.Record{}
		for _, rec := range z.records {
			if n := q.Get("name"); n != "" && !strings.Contains(rec.Name, strings.ToLower(n)) {
				continue
			}
			if t := q.Get("type"); t != "" && rec.Type != t {
				continue
			}
			out = append(out, rec)
		}
		sort.Slice(out, func(i, j int) bool { return out[i].Name+out[i].Type < out[j].Name+out[j].Type })
		writeJSON(w, map[string]any{"records": out, "totalCount": len(out)})
	case http.MethodPut:
		var body struct{ Record nrdns.Record }
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeErr(w, http.StatusBadRequest, 3, err.Error())
			return
		}
		rec := body.Record
		if n := a.FailPut[rec.Name]; n > 0 {
			a.FailPut[rec.Name] = n - 1
			writeErr(w, http.StatusServiceUnavailable, 14, "record rejected")
			return
		}
		if rec.Name == "" || rec.Type == "" || len(rec.RData) == 0 {
			writeErr(w, http.StatusBadRequest, 3, "name, type and rdata are required")
			return
		}
		if rec.Type == "CNAME" {
			for k, other := range z.records {
				if other.Name == rec.Name && other.Type != "CNAME" && !other.System {
					writeErr(w, http.StatusBadRequest, 3, "CNAME conflicts with "+k)
					return
				}
			}
		}
		if rec.TTL == 0 {
			rec.TTL = 3600
		}
		if rec.Type == "CNAME" {
			// The API stores hostnames fully qualified.
			rec.RData = []string{strings.TrimSuffix(rec.RData[0], ".") + "."}
		}
		z.records[rec.Name+"/"+rec.Type] = rec
		a.Writes++
		writeJSON(w, rec)
	case http.MethodDelete:
		// Like the API, deleting a missing record set succeeds.
		delete(z.records, q.Get("name")+"/"+q.Get("type"))
		a.Writes++
		writeJSON(w, map[string]any{})
	default:
		writeErr(w, http.StatusMethodNotAllowed, 12, "method not allowed")
	}
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, status, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"code": code, "message": msg})
}
