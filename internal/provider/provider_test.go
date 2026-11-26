package provider

import (
	"context"
	"net"
	"slices"
	"strings"
	"testing"
	"time"

	"sigs.k8s.io/external-dns/endpoint"
	"sigs.k8s.io/external-dns/pkg/apis/externaldns"
	"sigs.k8s.io/external-dns/plan"
	"sigs.k8s.io/external-dns/provider/webhook"
	"sigs.k8s.io/external-dns/provider/webhook/api"
	"sigs.k8s.io/external-dns/registry/txt"

	"github.com/noderings/external-dns-noderings-webhook/internal/nrdns"
	"github.com/noderings/external-dns-noderings-webhook/internal/nrdns/nrdnstest"
)

func newTest(t *testing.T, filter []string, zones ...string) (*nrdnstest.API, *Provider) {
	t.Helper()
	fake := nrdnstest.New()
	t.Cleanup(fake.Close)
	for _, z := range zones {
		fake.AddZone(z)
	}
	return fake, New(fake.Client(), Options{DomainFilter: endpoint.NewDomainFilter(filter)})
}

func find(eps []*endpoint.Endpoint, name, rtype string) *endpoint.Endpoint {
	for _, e := range eps {
		if e.DNSName == name && e.RecordType == rtype {
			return e
		}
	}
	return nil
}

func TestRecordsSkipsSystemAndMapsNames(t *testing.T) {
	fake, p := newTest(t, nil, "example.com")
	fake.Put("example.com", nrdns.Record{Name: "@", Type: "A", TTL: 60, RData: []string{"192.0.2.1"}})
	fake.Put("example.com", nrdns.Record{Name: "www", Type: "CNAME", TTL: 300, RData: []string{"Example.COM."}})
	fake.Put("example.com", nrdns.Record{Name: "mail", Type: "MX", TTL: 300, RData: []string{"10 mx.example.net."}})
	fake.Put("example.com", nrdns.Record{Name: "_x", Type: "TXT", TTL: 300, RData: []string{`"hello"`}})
	fake.Put("example.com", nrdns.Record{Name: "k", Type: "SSHFP", TTL: 300, RData: []string{"1 1 abc"}})

	eps, err := p.Records(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(eps) != 4 {
		t.Fatalf("want 4 endpoints (no SOA, apex NS or SSHFP), got %d: %v", len(eps), eps)
	}
	if e := find(eps, "example.com", "A"); e == nil || e.Targets[0] != "192.0.2.1" || e.RecordTTL != 60 {
		t.Errorf("apex A: %v", e)
	}
	if e := find(eps, "www.example.com", "CNAME"); e == nil || e.Targets[0] != "example.com" {
		t.Errorf("CNAME target should lose case and trailing dot: %v", e)
	}
	if e := find(eps, "mail.example.com", "MX"); e == nil || e.Targets[0] != "10 mx.example.net" {
		t.Errorf("MX: %v", e)
	}
	if e := find(eps, "_x.example.com", "TXT"); e == nil || e.Targets[0] != `"hello"` {
		t.Errorf("TXT stays quoted: %v", e)
	}
}

func TestDomainFilterLimitsZonesAndRecords(t *testing.T) {
	fake, p := newTest(t, []string{"app.example.com"}, "example.com", "other.org")
	fake.Put("example.com", nrdns.Record{Name: "web.app", Type: "A", TTL: 60, RData: []string{"192.0.2.1"}})
	fake.Put("example.com", nrdns.Record{Name: "web", Type: "A", TTL: 60, RData: []string{"192.0.2.2"}})
	fake.Put("other.org", nrdns.Record{Name: "web", Type: "A", TTL: 60, RData: []string{"192.0.2.3"}})

	eps, err := p.Records(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(eps) != 1 || eps[0].DNSName != "web.app.example.com" {
		t.Fatalf("want only web.app.example.com, got %v", eps)
	}
}

func TestApplyChangesCreateUpdateDelete(t *testing.T) {
	fake, p := newTest(t, nil, "example.com", "sub.example.com")
	ctx := context.Background()
	err := p.ApplyChanges(ctx, &plan.Changes{Create: []*endpoint.Endpoint{
		endpoint.NewEndpoint("web.example.com", "A", "192.0.2.2", "192.0.2.1"),
		endpoint.NewEndpointWithTTL("api.sub.example.com", "CNAME", 120, "web.example.com"),
		endpoint.NewEndpoint("_owner.example.com", "TXT", "heritage=external-dns"),
	}})
	if err != nil {
		t.Fatal(err)
	}
	recs := fake.Records("example.com")
	if len(recs) != 2 {
		t.Fatalf("example.com: %v", recs)
	}
	if recs[1].Name != "web" || !slices.Equal(recs[1].RData, []string{"192.0.2.1", "192.0.2.2"}) || recs[1].TTL != 300 {
		t.Errorf("A record set with sorted values and default TTL: %+v", recs[1])
	}
	if recs[0].Name != "_owner" || recs[0].RData[0] != `"heritage=external-dns"` {
		t.Errorf("TXT is quoted for the API: %+v", recs[0])
	}
	sub := fake.Records("sub.example.com")
	if len(sub) != 1 || sub[0].Name != "api" || sub[0].TTL != 120 {
		t.Errorf("CNAME goes to the most specific zone: %+v", sub)
	}

	err = p.ApplyChanges(ctx, &plan.Changes{
		UpdateOld: []*endpoint.Endpoint{endpoint.NewEndpoint("web.example.com", "A", "192.0.2.1", "192.0.2.2")},
		UpdateNew: []*endpoint.Endpoint{endpoint.NewEndpoint("web.example.com", "A", "192.0.2.9")},
		Delete:    []*endpoint.Endpoint{endpoint.NewEndpoint("_owner.example.com", "TXT", `"heritage=external-dns"`)},
	})
	if err != nil {
		t.Fatal(err)
	}
	recs = fake.Records("example.com")
	if len(recs) != 1 || !slices.Equal(recs[0].RData, []string{"192.0.2.9"}) {
		t.Errorf("after update and delete: %+v", recs)
	}
}

func TestTypeChangeDeletesFirst(t *testing.T) {
	fake, p := newTest(t, nil, "example.com")
	fake.Put("example.com", nrdns.Record{Name: "web", Type: "A", TTL: 300, RData: []string{"192.0.2.1"}})
	err := p.ApplyChanges(context.Background(), &plan.Changes{
		Delete: []*endpoint.Endpoint{endpoint.NewEndpoint("web.example.com", "A", "192.0.2.1")},
		Create: []*endpoint.Endpoint{endpoint.NewEndpoint("web.example.com", "CNAME", "lb.example.net")},
	})
	if err != nil {
		t.Fatal(err) // the fake rejects a CNAME next to an A record
	}
	recs := fake.Records("example.com")
	if len(recs) != 1 || recs[0].Type != "CNAME" {
		t.Fatalf("want only the CNAME: %+v", recs)
	}
}

func TestApplyChangesKeepsGoingAfterAFailure(t *testing.T) {
	fake, p := newTest(t, nil, "example.com")
	fake.FailPut["bad"] = 10
	err := p.ApplyChanges(context.Background(), &plan.Changes{Create: []*endpoint.Endpoint{
		endpoint.NewEndpoint("bad.example.com", "A", "192.0.2.1"),
		endpoint.NewEndpoint("good.example.com", "A", "192.0.2.2"),
		endpoint.NewEndpoint("invalid.example.com", "A", "not-an-ip"),
		endpoint.NewEndpoint("elsewhere.org", "A", "192.0.2.3"),
	}})
	if err == nil || !strings.Contains(err.Error(), "bad") || !strings.Contains(err.Error(), "not-an-ip") {
		t.Fatalf("want errors for bad and invalid, got %v", err)
	}
	recs := fake.Records("example.com")
	if len(recs) != 1 || recs[0].Name != "good" {
		t.Errorf("good record still written: %+v", recs)
	}
}

func TestDryRunWritesNothing(t *testing.T) {
	fake, _ := newTest(t, nil, "example.com")
	p := New(fake.Client(), Options{DryRun: true})
	err := p.ApplyChanges(context.Background(), &plan.Changes{Create: []*endpoint.Endpoint{
		endpoint.NewEndpoint("web.example.com", "A", "192.0.2.1"),
	}})
	if err != nil || fake.Writes != 0 {
		t.Fatalf("dry run: err=%v writes=%d", err, fake.Writes)
	}
}

func TestAdjustEndpoints(t *testing.T) {
	_, p := newTest(t, nil, "example.com")
	weighted := endpoint.NewEndpoint("w.example.com", "A", "192.0.2.1")
	weighted.SetIdentifier = "eu"
	out, err := p.AdjustEndpoints([]*endpoint.Endpoint{
		endpoint.NewEndpoint("www.example.com", "CNAME", "LB.Example.net."),
		endpoint.NewEndpoint("t.example.com", "TXT", "plain text"),
		endpoint.NewEndpoint("x.example.com", "DS", "1 2 3 abc"),
		weighted,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 2 || out[0].Targets[0] != "lb.example.net" || out[1].Targets[0] != `"plain text"` {
		t.Fatalf("adjusted: %v", out)
	}
}

// TestExternalDNSSyncLoop runs external-dns's own webhook client, TXT registry and
// planner against the webhook server, the way the external-dns controller does:
// the first sync creates the records with ownership TXT records, a second sync
// with the same input changes nothing, and removing the source deletes them.
func TestExternalDNSSyncLoop(t *testing.T) {
	fake, p := newTest(t, []string{"example.com"}, "example.com")
	fake.Put("example.com", nrdns.Record{Name: "manual", Type: "A", TTL: 300, RData: []string{"198.51.100.7"}})

	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	_ = l.Close()
	started := make(chan struct{}, 1)
	go api.StartHTTPApi(api.ServerOptions{
		Provider: p, StartedChan: started, ProviderPort: addr,
		ReadTimeout: 10 * time.Second, WriteTimeout: 10 * time.Second,
	})
	<-started

	ctx := context.Background()
	cfg := externaldns.NewConfig()
	cfg.WebhookProviderURL = "http://" + addr
	cfg.WebhookProviderReadTimeout, cfg.WebhookProviderWriteTimeout = 5*time.Second, 10*time.Second
	cfg.TXTOwnerID = "test-cluster"
	cfg.ManagedDNSRecordTypes = []string{"A", "AAAA", "CNAME"} // the external-dns default
	client, err := webhook.New(ctx, cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	reg, err := txt.New(cfg, client)
	if err != nil {
		t.Fatal(err)
	}

	sync := func(source []*endpoint.Endpoint) *plan.Changes {
		t.Helper()
		current, err := reg.Records(ctx)
		if err != nil {
			t.Fatal(err)
		}
		desired, err := reg.AdjustEndpoints(source)
		if err != nil {
			t.Fatal(err)
		}
		pl := (&plan.Plan{
			Policies: []plan.Policy{&plan.SyncPolicy{}}, Current: current, Desired: desired,
			DomainFilter:   endpoint.MatchAllDomainFilters{client.GetDomainFilter(), reg.GetDomainFilter()},
			ManagedRecords: cfg.ManagedDNSRecordTypes, OwnerID: cfg.TXTOwnerID,
		}).Calculate()
		if pl.Changes.HasChanges() {
			if err := reg.ApplyChanges(ctx, pl.Changes); err != nil {
				t.Fatal(err)
			}
		}
		return pl.Changes
	}
	source := func() []*endpoint.Endpoint {
		web := endpoint.NewEndpoint("web.example.com", "A", "192.0.2.10")
		web.Labels = endpoint.Labels{endpoint.ResourceLabelKey: "ingress/default/web"}
		alias := endpoint.NewEndpoint("www.example.com", "CNAME", "web.example.com")
		alias.Labels = endpoint.Labels{endpoint.ResourceLabelKey: "ingress/default/web"}
		return []*endpoint.Endpoint{web, alias}
	}

	if ch := sync(source()); len(ch.Create) == 0 {
		t.Fatalf("first sync should create records: %+v", ch)
	}
	names := map[string]bool{}
	for _, r := range fake.Records("example.com") {
		names[r.Name+"/"+r.Type] = true
	}
	for _, want := range []string{"web/A", "www/CNAME", "manual/A"} {
		if !names[want] {
			t.Errorf("missing %s after first sync: %v", want, names)
		}
	}
	txtCount := 0
	for k := range names {
		if strings.HasSuffix(k, "/TXT") {
			txtCount++
		}
	}
	if txtCount == 0 {
		t.Errorf("no ownership TXT records: %v", names)
	}

	if ch := sync(source()); ch.HasChanges() {
		t.Fatalf("second sync with the same input must be a no-op, got create=%v updateNew=%v delete=%v",
			ch.Create, ch.UpdateNew, ch.Delete)
	}
	writes := fake.Writes

	moved := source()
	moved[0].Targets = endpoint.Targets{"192.0.2.11"}
	if ch := sync(moved); len(ch.UpdateNew) == 0 {
		t.Fatalf("changed target should update: %+v", ch)
	}
	if fake.Writes == writes {
		t.Fatal("update wrote nothing")
	}

	sync(nil)
	left := fake.Records("example.com")
	if len(left) != 1 || left[0].Name != "manual" {
		t.Fatalf("removing the source should delete only records external-dns owns, left: %+v", left)
	}
}
