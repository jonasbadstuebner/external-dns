/*
Copyright 2017 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package hetzner

import (
	"context"
	"fmt"
	"os"
	"strings"

	hdns "github.com/jobstoit/hetzner-dns-go/dns"
	log "github.com/sirupsen/logrus"

	"sigs.k8s.io/external-dns/endpoint"
	"sigs.k8s.io/external-dns/pkg/apis/externaldns"
	"sigs.k8s.io/external-dns/plan"
	"sigs.k8s.io/external-dns/provider"
)

// HetznerProvider is an implementation of Provider for Digital Ocean's DNS.
type HetznerProvider struct {
	provider.BaseProvider
	Client hdns.Client
	// only consider hosted zones managing domains ending in this suffix
	domainFilter endpoint.DomainFilter
	// page size when querying paginated APIs
	apiPageSize      int
	DryRun           bool
	zoneIDNameMapper provider.ZoneIDName
}

type hetznerChangeCreate struct {
	Domain  string
	Options *hdns.RecordCreateOpts
}

type hetznerChangeUpdate struct {
	Domain       string
	DomainRecord hdns.Record
	Options      *hdns.RecordUpdateOpts
}

type hetznerChangeDelete struct {
	Domain   string
	RecordID string
}

// HetznerChange contains all changes to apply to DNS
type hetznerChanges struct {
	Creates []*hetznerChangeCreate
	Updates []*hetznerChangeUpdate
	Deletes []*hetznerChangeDelete
}

func (c *hetznerChanges) Empty() bool {
	return len(c.Creates) == 0 && len(c.Updates) == 0 && len(c.Deletes) == 0
}

// NewHetznerProvider initializes a new Hetzner DNS based Provider.
func NewHetznerProvider(ctx context.Context, domainFilter endpoint.DomainFilter, dryRun bool, apiPageSize int) (*HetznerProvider, error) {
	token, ok := os.LookupEnv("HDNS_TOKEN")
	if !ok {
		return nil, fmt.Errorf("no token found")
	}
	clientOptions := []hdns.ClientOption{hdns.WithToken(token), hdns.WithApplication("ExternalDNS", externaldns.Version)}

	if log.GetLevel() >= log.TraceLevel {
		clientOptions = append(clientOptions, hdns.WithDebugWriter(log.StandardLogger().Out))
	}

	client := hdns.NewClient(clientOptions...)

	_, err := client.NewRequest(ctx, "GET", "/", nil)
	if err != nil {
		return nil, err
	}

	p := &HetznerProvider{
		Client:       *client,
		domainFilter: domainFilter,
		apiPageSize:  apiPageSize,
		DryRun:       dryRun,
	}
	return p, nil
}

// Zones returns the list of hosted zones.
func (p *HetznerProvider) Zones(ctx context.Context) ([]hdns.Zone, error) {
	result := []hdns.Zone{}

	zones, err := p.fetchZones(ctx)
	if err != nil {
		return nil, err
	}

	for _, zone := range zones {
		if p.domainFilter.Match(zone.Name) {
			result = append(result, zone)
		}
	}

	p.ensureZoneIDMappingPresent(zones)

	return result, nil
}

func (p HetznerProvider) AdjustEndpoints(endpoints []*endpoint.Endpoint) []*endpoint.Endpoint {
	adjustedEndpoints := []*endpoint.Endpoint{}

	for _, ep := range endpoints {
		_, zoneName := p.zoneIDNameMapper.FindZone(ep.DNSName)
		adjustedTargets := endpoint.Targets{}
		for _, t := range ep.Targets {
			adjustedTarget, producedValidTarget := p.makeEndpointTarget(zoneName, t, ep.RecordType)
			if producedValidTarget {
				adjustedTargets = append(adjustedTargets, adjustedTarget)
			}
		}

		ep.Targets = adjustedTargets
		adjustedEndpoints = append(adjustedEndpoints, ep)
	}

	return adjustedEndpoints
}

// Merge Endpoints with the same Name and Type into a single endpoint with multiple Targets.
func mergeEndpointsByNameType(endpoints []*endpoint.Endpoint) []*endpoint.Endpoint {
	endpointsByNameType := map[string][]*endpoint.Endpoint{}

	for _, e := range endpoints {
		key := fmt.Sprintf("%s-%s", e.DNSName, e.RecordType)
		endpointsByNameType[key] = append(endpointsByNameType[key], e)
	}

	// If no merge occurred, just return the existing endpoints.
	if len(endpointsByNameType) == len(endpoints) {
		return endpoints
	}

	// Otherwise, construct a new list of endpoints with the endpoints merged.
	var result []*endpoint.Endpoint
	for _, endpoints := range endpointsByNameType {
		dnsName := endpoints[0].DNSName
		recordType := endpoints[0].RecordType

		targets := make([]string, len(endpoints))
		for i, e := range endpoints {
			targets[i] = e.Targets[0]
		}

		e := endpoint.NewEndpoint(dnsName, recordType, targets...)
		result = append(result, e)
	}

	return result
}

// Records returns the list of records in a given zone.
func (p *HetznerProvider) Records(ctx context.Context) ([]*endpoint.Endpoint, error) {
	zones, err := p.Zones(ctx)
	if err != nil {
		return nil, err
	}

	endpoints := []*endpoint.Endpoint{}
	for _, zone := range zones {
		records, err := p.fetchRecords(ctx, zone.ID)
		if err != nil {
			return nil, err
		}

		for _, r := range records {
			if provider.SupportedRecordType(string(r.Type)) {
				name := fmt.Sprintf("%s.%s", r.Name, zone.Name)

				// root name is identified by @ and should be
				// translated to zone name for the endpoint entry.
				if r.Name == "@" {
					name = zone.Name
				}

				ep := endpoint.NewEndpoint(name, string(r.Type), r.Value)
				endpoints = append(endpoints, ep)
			}
		}
	}

	// Merge endpoints with the same name and type (e.g., multiple A records for a single
	// DNS name) into one endpoint with multiple targets.
	endpoints = mergeEndpointsByNameType(endpoints)

	// Log the endpoints that were found.
	log.WithFields(log.Fields{
		"endpoints": endpoints,
	}).Debug("Endpoints generated from Hetzner DNS")

	return endpoints, nil
}

func (p *HetznerProvider) fetchRecords(ctx context.Context, zoneID string) ([]hdns.Record, error) {
	allRecords := []hdns.Record{}
	listOptions := &hdns.RecordListOpts{ListOpts: hdns.ListOpts{PerPage: p.apiPageSize}, ZoneID: zoneID}
	for {
		records, resp, err := p.Client.Record.List(ctx, *listOptions)
		if err != nil {
			return nil, err
		}
		for _, r := range records {
			allRecords = append(allRecords, *r)
		}

		if resp == nil || resp.Meta.Pagination == nil || resp.Meta.Pagination.LastPage <= resp.Meta.Pagination.Page {
			break
		}

		listOptions.Page = resp.Meta.Pagination.Page + 1
	}

	return allRecords, nil
}

func (p *HetznerProvider) fetchZones(ctx context.Context) ([]hdns.Zone, error) {
	allZones := []hdns.Zone{}
	listOptions := &hdns.ZoneListOpts{ListOpts: hdns.ListOpts{PerPage: p.apiPageSize}}
	for {
		zones, resp, err := p.Client.Zone.List(ctx, *listOptions)
		if err != nil {
			return nil, err
		}

		for _, z := range zones {
			allZones = append(allZones, *z)
		}

		if resp == nil || resp.Meta.Pagination == nil || resp.Meta.Pagination.LastPage <= resp.Meta.Pagination.Page {
			break
		}

		listOptions.Page = resp.Meta.Pagination.Page + 1
	}

	return allZones, nil
}

func (p *HetznerProvider) ensureZoneIDMappingPresent(zones []hdns.Zone) {
	zoneIDNameMapper := provider.ZoneIDName{}
	for _, z := range zones {
		zoneIDNameMapper.Add(z.ID, z.Name)
	}
	p.zoneIDNameMapper = zoneIDNameMapper
}

func (p *HetznerProvider) getRecordsByZoneID(ctx context.Context) (map[string][]hdns.Record, provider.ZoneIDName, error) {
	recordsByZoneID := map[string][]hdns.Record{}

	zones, err := p.Zones(ctx)
	if err != nil {
		return nil, nil, err
	}

	// Fetch records for each zone
	for _, zone := range zones {
		records, err := p.fetchRecords(ctx, zone.ID)
		if err != nil {
			return nil, nil, err
		}

		recordsByZoneID[zone.ID] = append(recordsByZoneID[zone.ID], records...)
	}

	return recordsByZoneID, p.zoneIDNameMapper, nil
}

// Make a endpoint name that conforms to Hetzner DNS requirements:
// - Records at root of the zone have `@` as the name
func makeEndpointName(domain, entryName, epType string) string {
	// Trim the domain off the name if present.
	adjustedName := strings.TrimSuffix(entryName, "."+domain)

	// Record at the root should be defined as @ instead of the full domain name.
	if adjustedName == domain {
		adjustedName = "@"
	}

	return adjustedName
}

// Make a endpoint name that conforms to Hetzner DNS requirements:
// - Records at root of the zone have `@` as the name
// - A-Records should respect ignored networks and should only contain IPv4 entries
func (p HetznerProvider) makeEndpointTarget(domain, entryTarget, recordType string) (string, bool) {
	if domain == "" {
		return entryTarget, true
	}
	adjustedTarget := entryTarget

	// Trim the trailing dot
	adjustedTarget = strings.TrimSuffix(entryTarget, ".")
	adjustedTarget = strings.TrimSuffix(adjustedTarget, "."+domain)

	return adjustedTarget, true
}

// submitChanges applies an instance of `hetznerChanges` to the Hetzner API.
func (p *HetznerProvider) submitChanges(ctx context.Context, changes *hetznerChanges) error {
	// return early if there is nothing to change
	if changes.Empty() {
		return nil
	}

	for _, d := range changes.Deletes {
		log.WithFields(log.Fields{
			"domain":   d.Domain,
			"recordID": d.RecordID,
		}).Debug("Deleting domain record")

		if p.DryRun {
			continue
		}

		_, err := p.Client.Record.Delete(ctx, &hdns.Record{ID: d.RecordID})
		if err != nil {
			return err
		}
	}

	for _, c := range changes.Creates {
		ttl := -1
		if c.Options.Ttl != nil {
			ttl = *c.Options.Ttl
		}
		log.WithFields(log.Fields{
			"domain":     c.Domain,
			"zoneID":     c.Options.Zone.ID,
			"dnsName":    c.Options.Name,
			"recordType": c.Options.Type,
			"value":      c.Options.Value,
			"ttl":        ttl,
		}).Debug("Creating domain record")

		if p.DryRun {
			continue
		}

		_, _, err := p.Client.Record.Create(ctx, hdns.RecordCreateOpts{Name: c.Options.Name, Ttl: c.Options.Ttl, Type: c.Options.Type, Value: c.Options.Value, Zone: c.Options.Zone})
		if err != nil {
			return err
		}
	}

	for _, u := range changes.Updates {
		ttl := -1
		if u.Options.Ttl != nil {
			ttl = *u.Options.Ttl
		}
		log.WithFields(log.Fields{
			"domain":     u.Domain,
			"zoneID":     u.Options.Zone.ID,
			"dnsName":    u.Options.Name,
			"recordType": u.Options.Type,
			"value":      u.Options.Value,
			"ttl":        ttl,
		}).Debug("Updating domain record")

		if p.DryRun {
			continue
		}

		_, _, err := p.Client.Record.Update(ctx, &u.DomainRecord, hdns.RecordUpdateOpts{Name: u.Options.Name, Ttl: u.Options.Ttl, Type: u.Options.Type, Value: u.Options.Value, Zone: u.Options.Zone})
		if err != nil {
			return err
		}
	}

	return nil
}

func endpointsByZoneID(zoneIDNameMapper provider.ZoneIDName, endpoints []*endpoint.Endpoint) map[string][]*endpoint.Endpoint {
	endpointsByZoneID := make(map[string][]*endpoint.Endpoint)

	for _, ep := range endpoints {
		zoneID, _ := zoneIDNameMapper.FindZone(ep.DNSName)
		if zoneID == "" {
			log.Debugf("Skipping record %s because no hosted zone matching record DNS Name was detected", ep.DNSName)
			continue
		}
		endpointsByZoneID[zoneID] = append(endpointsByZoneID[zoneID], ep)
	}

	return endpointsByZoneID
}

func getMatchingDomainRecords(records []hdns.Record, zoneName string, ep *endpoint.Endpoint) []hdns.Record {
	var name string
	if ep.DNSName != zoneName {
		name = strings.TrimSuffix(ep.DNSName, "."+zoneName)
	} else {
		name = "@"
	}

	var result []hdns.Record
	for _, r := range records {
		if r.Name == name && string(r.Type) == ep.RecordType {
			result = append(result, r)
		}
	}
	return result
}

func getTTLFromEndpoint(ep *endpoint.Endpoint) (int, bool) {
	if ep.RecordTTL.IsConfigured() {
		return int(ep.RecordTTL), true
	}
	return -1, false
}

func processCreateActions(
	zoneIDNameMapper provider.ZoneIDName,
	recordsByZoneID map[string][]hdns.Record,
	createsByZoneID map[string][]*endpoint.Endpoint,
	changes *hetznerChanges,
) error {
	// Process endpoints that need to be created.
	for zoneID, endpoints := range createsByZoneID {
		zoneName := zoneIDNameMapper[zoneID]
		if len(endpoints) == 0 {
			log.WithFields(log.Fields{
				"zoneName": zoneName,
			}).Debug("Skipping domain, no creates found.")
			continue
		}

		records := recordsByZoneID[zoneName]

		for _, ep := range endpoints {
			// Warn if there are existing records since we expect to create only new records.
			matchingRecords := getMatchingDomainRecords(records, zoneName, ep)
			if len(matchingRecords) > 0 {
				log.WithFields(log.Fields{
					"zoneName":   zoneName,
					"dnsName":    ep.DNSName,
					"recordType": ep.RecordType,
				}).Warn("Preexisting records exist which should not exist for creation actions.")
			}

			var ttl *int = nil
			configuredTTL, ttlIsSet := getTTLFromEndpoint(ep)
			if ttlIsSet {
				ttl = &configuredTTL
			}
			for _, target := range ep.Targets {

				log.WithFields(log.Fields{
					"zoneName":   zoneName,
					"dnsName":    ep.DNSName,
					"recordType": ep.RecordType,
					"target":     target,
				}).Warn("Creating new target")

				changes.Creates = append(changes.Creates, &hetznerChangeCreate{
					Domain: zoneName,
					Options: &hdns.RecordCreateOpts{
						Name:  makeEndpointName(zoneName, ep.DNSName, ep.RecordType),
						Ttl:   ttl,
						Type:  hdns.RecordType(ep.RecordType),
						Value: target,
						Zone: &hdns.Zone{
							ID:   zoneID,
							Name: zoneName,
						},
					},
				})
			}
		}
	}

	return nil
}

func processUpdateActions(
	zoneIDNameMapper provider.ZoneIDName,
	recordsByZoneID map[string][]hdns.Record,
	updatesByZoneID map[string][]*endpoint.Endpoint,
	changes *hetznerChanges,
) error {
	// Generate creates and updates based on existing
	for zoneID, updates := range updatesByZoneID {
		zoneName := zoneIDNameMapper[zoneID]
		if len(updates) == 0 {
			log.WithFields(log.Fields{
				"zoneName": zoneName,
			}).Debug("Skipping Zone, no updates found.")
			continue
		}

		records := recordsByZoneID[zoneID]
		log.WithFields(log.Fields{
			"zoneName": zoneName,
			"records":  records,
		}).Debug("Records for domain")

		for _, ep := range updates {
			matchingRecords := getMatchingDomainRecords(records, zoneName, ep)

			log.WithFields(log.Fields{
				"endpoint":        ep,
				"matchingRecords": matchingRecords,
			}).Debug("matching records")

			if len(matchingRecords) == 0 {
				log.WithFields(log.Fields{
					"zoneName":   zoneName,
					"dnsName":    ep.DNSName,
					"recordType": ep.RecordType,
				}).Warn("Planning an update but no existing records found.")
			}

			matchingRecordsByTarget := map[string]hdns.Record{}
			for _, r := range matchingRecords {
				matchingRecordsByTarget[r.Value] = r
			}

			var ttl *int = nil
			configuredTTL, ttlIsSet := getTTLFromEndpoint(ep)
			if ttlIsSet {
				ttl = &configuredTTL
			}

			// Generate create and delete actions based on existence of a record for each target.
			for _, target := range ep.Targets {
				if record, ok := matchingRecordsByTarget[target]; ok {
					log.WithFields(log.Fields{
						"zoneName":   zoneName,
						"dnsName":    ep.DNSName,
						"recordType": ep.RecordType,
						"target":     target,
					}).Warn("Updating existing target")

					changes.Updates = append(changes.Updates, &hetznerChangeUpdate{
						Domain:       zoneName,
						DomainRecord: record,
						Options: &hdns.RecordUpdateOpts{
							Name:  makeEndpointName(zoneName, ep.DNSName, ep.RecordType),
							Ttl:   ttl,
							Type:  hdns.RecordType(ep.RecordType),
							Value: target,
							Zone: &hdns.Zone{
								ID:   zoneID,
								Name: zoneName,
							},
						},
					})

					delete(matchingRecordsByTarget, target)
				} else {
					// Record did not previously exist, create new 'target'
					log.WithFields(log.Fields{
						"zoneName":   zoneName,
						"dnsName":    ep.DNSName,
						"recordType": ep.RecordType,
						"target":     target,
					}).Warn("No target to update - creating new target")

					changes.Creates = append(changes.Creates, &hetznerChangeCreate{
						Domain: zoneName,
						Options: &hdns.RecordCreateOpts{
							Name:  makeEndpointName(zoneName, ep.DNSName, ep.RecordType),
							Ttl:   ttl,
							Type:  hdns.RecordType(ep.RecordType),
							Value: target,
							Zone: &hdns.Zone{
								ID:   zoneID,
								Name: zoneName,
							},
						},
					})
				}
			}

			// Any remaining records have been removed, delete them
			for _, record := range matchingRecordsByTarget {
				log.WithFields(log.Fields{
					"zoneName":   zoneName,
					"dnsName":    ep.DNSName,
					"recordType": ep.RecordType,
					"target":     record.Value,
				}).Warn("Deleting target")

				changes.Deletes = append(changes.Deletes, &hetznerChangeDelete{
					Domain:   zoneName,
					RecordID: record.ID,
				})
			}
		}
	}

	return nil
}

func processDeleteActions(
	zoneIDNameMapper provider.ZoneIDName,
	recordsByZoneID map[string][]hdns.Record,
	deletesByZoneID map[string][]*endpoint.Endpoint,
	changes *hetznerChanges,
) error {
	// Generate delete actions for each deleted endpoint.
	for zoneID, deletes := range deletesByZoneID {
		zoneName := zoneIDNameMapper[zoneID]
		if len(deletes) == 0 {
			log.WithFields(log.Fields{
				"zoneName": zoneName,
			}).Debug("Skipping Zone, no deletes found.")
			continue
		}

		records := recordsByZoneID[zoneID]

		for _, ep := range deletes {
			matchingRecords := getMatchingDomainRecords(records, zoneName, ep)

			if len(matchingRecords) == 0 {
				log.WithFields(log.Fields{
					"zoneName":   zoneName,
					"dnsName":    ep.DNSName,
					"recordType": ep.RecordType,
				}).Warn("Records to delete not found.")
			}

			for _, record := range matchingRecords {
				doDelete := false
				for _, t := range ep.Targets {
					v1 := t
					v2 := record.Value
					if ep.RecordType == endpoint.RecordTypeCNAME {
						v1 = strings.TrimSuffix(t, ".")
						v2 = strings.TrimSuffix(t, ".")
					}
					if v1 == v2 {
						doDelete = true
					}
				}

				if doDelete {
					changes.Deletes = append(changes.Deletes, &hetznerChangeDelete{
						Domain:   zoneName,
						RecordID: record.ID,
					})
				}
			}
		}
	}

	return nil
}

// ApplyChanges applies the given set of generic changes to the provider.
func (p *HetznerProvider) ApplyChanges(ctx context.Context, planChanges *plan.Changes) error {
	// TODO: This should only retrieve zones affected by the given `planChanges`.
	recordsByZoneID, zoneIDNameMapper, err := p.getRecordsByZoneID(ctx)
	if err != nil {
		return err
	}

	createsByZoneID := endpointsByZoneID(zoneIDNameMapper, planChanges.Create)
	updatesByZoneID := endpointsByZoneID(zoneIDNameMapper, planChanges.UpdateNew)
	deletesByZoneID := endpointsByZoneID(zoneIDNameMapper, planChanges.Delete)

	var changes hetznerChanges

	if err := processCreateActions(zoneIDNameMapper, recordsByZoneID, createsByZoneID, &changes); err != nil {
		return err
	}

	if err := processUpdateActions(zoneIDNameMapper, recordsByZoneID, updatesByZoneID, &changes); err != nil {
		return err
	}

	if err := processDeleteActions(zoneIDNameMapper, recordsByZoneID, deletesByZoneID, &changes); err != nil {
		return err
	}

	return p.submitChanges(ctx, &changes)
}
