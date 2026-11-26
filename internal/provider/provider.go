// Package provider implements the external-dns provider interface on top of
// NodeRings DNS. One external-dns endpoint (a name and a type with its targets)
// is one NodeRings record set.
package provider

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"

	"sigs.k8s.io/external-dns/endpoint"
	"sigs.k8s.io/external-dns/plan"

	"github.com/noderings/external-dns-noderings-webhook/internal/nrdns"
)

// SupportedTypes are the record types the provider reads and writes. The apex
// SOA and NS record sets belong to NodeRings and are never touched.
var SupportedTypes = []string{
	endpoint.RecordTypeA, endpoint.RecordTypeAAAA, endpoint.RecordTypeCNAME, endpoint.RecordTypeTXT,
	endpoint.RecordTypeMX, endpoint.RecordTypeSRV, endpoint.RecordTypeNS, "CAA",
	endpoint.RecordTypePTR, endpoint.RecordTypeNAPTR,
}

// Options configures a Provider.
type Options struct {
	DomainFilter *endpoint.DomainFilter
	// DefaultTTL is used for endpoints without a TTL. Zero means 300 seconds.
	DefaultTTL int64
	// DryRun logs changes without making them.
	DryRun bool
	Logger *slog.Logger
}

// Provider is the NodeRings external-dns provider.
type Provider struct {
	client       *nrdns.Client
	domainFilter *endpoint.DomainFilter
	defaultTTL   int64
	dryRun       bool
	log          *slog.Logger
}

// New returns a Provider.
func New(client *nrdns.Client, opts Options) *Provider {
	p := &Provider{
		client:       client,
		domainFilter: opts.DomainFilter,
		defaultTTL:   opts.DefaultTTL,
		dryRun:       opts.DryRun,
		log:          opts.Logger,
	}
	if p.domainFilter == nil {
		p.domainFilter = endpoint.NewDomainFilter(nil)
	}
	if p.defaultTTL <= 0 {
		p.defaultTTL = 300
	}
	if p.log == nil {
		p.log = slog.New(slog.DiscardHandler)
	}
	return p
}

// GetDomainFilter returns the configured domain filter. external-dns uses it to
// decide which endpoints to send.
func (p *Provider) GetDomainFilter() endpoint.DomainFilterInterface { return p.domainFilter }

// Zones returns the NodeRings zones the domain filter lets the provider manage.
func (p *Provider) Zones(ctx context.Context) ([]nrdns.Zone, error) {
	all, err := p.client.ListZones(ctx)
	if err != nil {
		return nil, fmt.Errorf("listing NodeRings DNS zones: %w", err)
	}
	var out []nrdns.Zone
	for _, z := range all {
		name := nrdns.NormalizeName(z.Name)
		if !p.domainFilter.IsConfigured() || p.domainFilter.Match(name) || p.domainFilter.MatchParent(name) {
			out = append(out, z)
		}
	}
	return out, nil
}

// Records returns every record set in the managed zones as endpoints.
func (p *Provider) Records(ctx context.Context) ([]*endpoint.Endpoint, error) {
	zones, err := p.Zones(ctx)
	if err != nil {
		return nil, err
	}
	var out []*endpoint.Endpoint
	for _, z := range zones {
		recs, err := p.client.ListRecords(ctx, z.ID, "", "")
		if err != nil {
			return nil, fmt.Errorf("listing records of %s: %w", nrdns.NormalizeName(z.Name), err)
		}
		for _, r := range recs {
			rtype := strings.ToUpper(r.Type)
			if r.System || !slices.Contains(SupportedTypes, rtype) {
				continue
			}
			name := nrdns.AbsoluteName(r.Name, z.Name)
			if !p.domainFilter.Match(name) {
				continue
			}
			targets := make([]string, 0, len(r.RData))
			for _, v := range r.RData {
				targets = append(targets, normalizeTarget(rtype, v))
			}
			out = append(out, endpoint.NewEndpointWithTTL(name, rtype, endpoint.TTL(r.TTL), targets...))
		}
	}
	return out, nil
}

// AdjustEndpoints puts desired endpoints into the form Records returns, so that
// external-dns does not plan changes that would make no difference.
func (p *Provider) AdjustEndpoints(endpoints []*endpoint.Endpoint) ([]*endpoint.Endpoint, error) {
	out := make([]*endpoint.Endpoint, 0, len(endpoints))
	for _, ep := range endpoints {
		if !slices.Contains(SupportedTypes, ep.RecordType) {
			p.log.Warn("skipping unsupported record type", "name", ep.DNSName, "type", ep.RecordType)
			continue
		}
		if ep.SetIdentifier != "" {
			p.log.Warn("skipping endpoint with a set identifier; NodeRings has no weighted or latency routing",
				"name", ep.DNSName, "type", ep.RecordType, "setIdentifier", ep.SetIdentifier)
			continue
		}
		for i, t := range ep.Targets {
			ep.Targets[i] = normalizeTarget(ep.RecordType, t)
		}
		out = append(out, ep)
	}
	return out, nil
}

// change is one record set write.
type change struct {
	zone   nrdns.Zone
	name   string // relative to the zone
	rtype  string
	ttl    int64
	values []string // nil means delete
}

func (c change) key() string { return c.zone.ID + "|" + c.name + "|" + c.rtype }

// ApplyChanges writes a plan. Deletes run first, so a record that changes type
// (for example A to CNAME) does not conflict with its old self. A failure is
// logged and the rest of the plan still runs; external-dns retries on its
// next sync.
func (p *Provider) ApplyChanges(ctx context.Context, changes *plan.Changes) error {
	if changes == nil || !changes.HasChanges() {
		return nil
	}
	zones, err := p.Zones(ctx)
	if err != nil {
		return err
	}
	var errs []error
	upserts := map[string]change{}
	var order []string
	add := func(ep *endpoint.Endpoint) {
		c, err := p.toChange(zones, ep)
		if err != nil {
			errs = append(errs, err)
			return
		}
		if c == nil {
			return
		}
		if _, seen := upserts[c.key()]; !seen {
			order = append(order, c.key())
		}
		upserts[c.key()] = *c
	}
	for _, ep := range changes.Create {
		add(ep)
	}
	for _, ep := range changes.UpdateNew {
		add(ep)
	}

	for _, ep := range changes.Delete {
		c, err := p.toChange(zones, ep)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if c == nil {
			continue
		}
		if _, replaced := upserts[c.key()]; replaced {
			continue // the upsert below replaces the whole record set
		}
		errs = append(errs, p.write(ctx, change{zone: c.zone, name: c.name, rtype: c.rtype}))
	}
	for _, k := range order {
		errs = append(errs, p.write(ctx, upserts[k]))
	}
	return errors.Join(errs...)
}

func (p *Provider) toChange(zones []nrdns.Zone, ep *endpoint.Endpoint) (*change, error) {
	zone, ok := nrdns.FindZone(zones, ep.DNSName)
	if !ok {
		p.log.Warn("no managed NodeRings zone for endpoint; skipping", "name", ep.DNSName, "type", ep.RecordType)
		return nil, nil
	}
	rtype := strings.ToUpper(ep.RecordType)
	values := make([]string, 0, len(ep.Targets))
	for _, t := range ep.Targets {
		if rtype == endpoint.RecordTypeTXT {
			t = nrdns.QuoteTXT(t)
		}
		values = append(values, t)
	}
	values = nrdns.SortedUnique(values)
	if err := nrdns.ValidateValues(rtype, values); err != nil {
		return nil, fmt.Errorf("%s %s: %w", ep.DNSName, rtype, err)
	}
	ttl := p.defaultTTL
	if ep.RecordTTL.IsConfigured() {
		ttl = int64(ep.RecordTTL)
	}
	return &change{zone: zone, name: nrdns.RelativeName(ep.DNSName, zone.Name), rtype: rtype, ttl: ttl, values: values}, nil
}

func (p *Provider) write(ctx context.Context, c change) error {
	zoneName := nrdns.NormalizeName(c.zone.Name)
	fields := []any{"zone", zoneName, "name", c.name, "type", c.rtype}
	if c.values == nil {
		if p.dryRun {
			p.log.Info("dry run: would delete record set", fields...)
			return nil
		}
		if err := p.client.DeleteRecord(ctx, c.zone.ID, c.name, c.rtype); err != nil {
			p.log.Error("deleting record set failed", append(fields, "error", err)...)
			return fmt.Errorf("deleting %s %s in %s: %w", c.name, c.rtype, zoneName, err)
		}
		p.log.Info("deleted record set", fields...)
		return nil
	}
	fields = append(fields, "ttl", c.ttl, "values", c.values)
	if p.dryRun {
		p.log.Info("dry run: would write record set", fields...)
		return nil
	}
	rec := nrdns.Record{Name: c.name, Type: c.rtype, TTL: nrdns.Int64(c.ttl), RData: c.values}
	if err := p.client.UpsertRecord(ctx, c.zone.ID, rec); err != nil {
		p.log.Error("writing record set failed", append(fields, "error", err)...)
		return fmt.Errorf("writing %s %s in %s: %w", c.name, c.rtype, zoneName, err)
	}
	p.log.Info("wrote record set", fields...)
	return nil
}

// normalizeTarget gives one spelling to targets that the API and external-dns
// write differently: hostnames lose their trailing dot (the API adds one) and
// TXT values are kept in quoted presentation form.
func normalizeTarget(rtype, v string) string {
	v = strings.TrimSpace(v)
	switch rtype {
	case endpoint.RecordTypeCNAME, endpoint.RecordTypeNS, endpoint.RecordTypePTR:
		return strings.TrimSuffix(strings.ToLower(v), ".")
	case endpoint.RecordTypeMX, endpoint.RecordTypeSRV:
		f := strings.Fields(v)
		if len(f) > 0 {
			f[len(f)-1] = strings.TrimSuffix(strings.ToLower(f[len(f)-1]), ".")
		}
		return strings.Join(f, " ")
	case endpoint.RecordTypeTXT:
		return nrdns.QuoteTXT(v)
	}
	return v
}
