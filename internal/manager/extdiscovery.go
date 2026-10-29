package manager

import (
	"context"
	"log"
	"sync"
	"time"

	rpcpb "github.com/glenjbarber/apiary/api/rpc"
	"github.com/glenjbarber/apiary/internal/extdiscovery"
	"github.com/glenjbarber/apiary/internal/proxmoxdiscovery"
	"github.com/glenjbarber/apiary/internal/sylvediscovery"
)

// The external discovery providers of ADR-0191 (Proxmox) and ADR-0192
// (Sylve).
//
// WHAT THIS IS: a read-only observer of OTHER management planes. It
// asks a Proxmox or Sylve installation what it is running, and reports
// what it said.
//
// WHAT THIS IS NOT: a way to manage those installations. There is no
// code path here that creates, changes, or removes anything in
// Proxmox or Sylve. The providers themselves have no mutating method -
// internal/extdiscovery.Provider has only Scan - and this file adds no
// RPC that mutates a provider's world.
//
// The observations are LOCAL to this managerd and are NOT raft-
// replicated. Two reasons, and the second is the important one:
//
//  1. A provider credential is a per-host secret, and a secret does
//     not belong in replicated state.
//
//  2. A DISCOVERY OBSERVATION IS NOT A COLONY FACT. Apiary's
//     replicated state is the Colony's own truth; a Proxmox
//     cluster's node list is another system's truth, and replicating
//     it would give the Colony two sources of truth for the same
//     thing - which is precisely what ADR-0191 rejected when it
//     refused to treat a Proxmox cluster as an independently managed
//     Apiary cluster.
//
// So each managerd polls its own configured providers and serves its
// own observations. A caller that wants the Colony-wide view fans out
// the way the monitoring reports already do.

// DefaultDiscoveryInterval is the poll interval used when a provider is
// configured without one.
//
// Sixty seconds is a starting point rather than a requirement: it is
// frequent enough that a node's disappearance is noticed promptly and
// infrequent enough that a large installation is not hammering its
// provider.
const DefaultDiscoveryInterval = 60 * time.Second

// discoveryProvider couples a provider with the store its observations
// accumulate in.
type discoveryProvider struct {
	provider extdiscovery.Provider
	store    *extdiscovery.Store
	interval time.Duration
}

// discoveryState holds this node's discovery providers.
//
// It is created once at startup and read concurrently by the RPC
// handler, so the provider map is immutable after construction and each
// store has its own lock.
type discoveryState struct {
	mu        sync.RWMutex
	providers map[string]*discoveryProvider
}

// newDiscoveryState builds the provider set from configuration.
//
// A provider that is configured but cannot be CONSTRUCTED is not a
// fatal error: a bad CA bundle or an unreachable URL should not stop
// managerd from serving the rest of its API. The failure is logged, and
// the provider is simply absent - so a caller sees "no such provider"
// rather than a silently broken one.
//
// A provider that fails to construct is NOT given an empty store that
// reports "empty": an empty store reporting StateUnavailable is
// honest, and a caller can tell it apart from a working provider that
// found nothing.
func newDiscoveryState(proxmoxCfg *proxmoxdiscovery.Config, sylveCfg *sylvediscovery.Config, proxmoxInterval, sylveInterval time.Duration) *discoveryState {
	state := &discoveryState{providers: map[string]*discoveryProvider{}}

	if proxmoxCfg != nil {
		provider, err := proxmoxdiscovery.New(*proxmoxCfg)
		if err != nil {
			log.Printf("managerd: Proxmox discovery is configured but unusable, and will not be polled: %v", err)
		} else {
			state.providers[proxmoxdiscovery.ProviderName] = &discoveryProvider{
				provider: provider,
				store:    extdiscovery.NewStore(),
				interval: intervalOrDefault(proxmoxInterval),
			}
		}
	}

	if sylveCfg != nil {
		provider, err := sylvediscovery.New(*sylveCfg)
		if err != nil {
			log.Printf("managerd: Sylve discovery is configured but unusable, and will not be polled: %v", err)
		} else {
			state.providers[sylvediscovery.ProviderName] = &discoveryProvider{
				provider: provider,
				store:    extdiscovery.NewStore(),
				interval: intervalOrDefault(sylveInterval),
			}
		}
	}

	return state
}

// intervalOrDefault returns the configured interval, or the default.
func intervalOrDefault(interval time.Duration) time.Duration {
	if interval > 0 {
		return interval
	}
	return DefaultDiscoveryInterval
}

// Run polls every configured provider until the context is cancelled.
//
// Each provider has its own goroutine and its own interval, so a
// provider that is slow does not delay another. A scan that fails is
// recorded and the loop continues: a provider that is down must not
// stop the poll, and the next successful scan recovers the state
// without any special handling.
func (d *discoveryState) Run(ctx context.Context) {
	if d == nil || len(d.providers) == 0 {
		return
	}

	d.mu.RLock()
	providers := make([]*discoveryProvider, 0, len(d.providers))
	for _, p := range d.providers {
		providers = append(providers, p)
	}
	d.mu.RUnlock()

	for _, p := range providers {
		go d.poll(ctx, p)
	}
}

// poll runs one provider's scan loop.
func (d *discoveryState) poll(ctx context.Context, p *discoveryProvider) {
	scan := func() {
		result := p.provider.Scan(ctx)
		p.store.Apply(result)

		switch result.State {
		case extdiscovery.StateFresh, extdiscovery.StateEmpty:
			// The ordinary case. Nothing is logged, because a poll
			// that succeeds is not news.
		default:
			log.Printf("managerd: %s discovery scan state=%q complete=%t: %v",
				p.provider.Name(), result.State, result.Complete, result.Err)
		}

		for _, conflict := range p.store.Conflicts() {
			log.Printf("managerd: %s discovery conflict (%s %s): %s",
				p.provider.Name(), conflict.Kind, conflict.Key, conflict.Reason)
		}
	}

	// The first scan runs immediately, so a caller that starts
	// querying right after startup is not met with an empty report for
	// a whole interval.
	scan()

	ticker := time.NewTicker(p.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			scan()
		}
	}
}

// report builds the RPC response for the requested providers.
//
// An empty provider name means every configured provider. A named
// provider that is not configured produces an EMPTY provider entry with
// the state "unavailable" and an error naming it - NOT a silent empty
// result, because "that provider does not exist here" and "that
// provider found nothing" are different answers.
func (d *discoveryState) report(providerName string) *rpcpb.GetExternalDiscoveryResponse {
	resp := &rpcpb.GetExternalDiscoveryResponse{}

	if d == nil {
		return resp
	}

	d.mu.RLock()
	defer d.mu.RUnlock()

	names := make([]string, 0, len(d.providers))
	for name := range d.providers {
		names = append(names, name)
	}
	sortStrings(names)

	if providerName != "" {
		if _, ok := d.providers[providerName]; !ok {
			resp.Providers = []*rpcpb.ExternalDiscoveryProviderReport{{
				Provider: providerName,
				State:    string(extdiscovery.StateUnavailable),
				Error:    "no discovery provider with that name is configured on this node",
			}}
			return resp
		}
		names = []string{providerName}
	}

	for _, name := range names {
		p := d.providers[name]
		store := p.store

		report := &rpcpb.ExternalDiscoveryProviderReport{
			Provider: name,
			SourceId: store.SourceID(),
			State:    string(store.State()),
			Error:    discoveryErrorString(store.LastError()),
		}
		if at := store.LastScan(); !at.IsZero() {
			report.LastScan = at.Unix()
		}

		for _, node := range store.Nodes() {
			report.Nodes = append(report.Nodes, &rpcpb.ExternalDiscoveryNode{
				Provider:    node.Provider,
				SourceId:    node.SourceID,
				Name:        node.Name,
				Fingerprint: node.Fingerprint,
				Status:      node.Status,
				ObservedAt:  node.ObservedAt.Unix(),
			})
		}

		for _, resource := range store.Resources() {
			report.Resources = append(report.Resources, &rpcpb.ExternalDiscoveryResource{
				Provider:         resource.Provider,
				SourceId:         resource.SourceID,
				NodeName:         resource.NodeName,
				NodeId:           resource.NodeID,
				Kind:             string(resource.Kind),
				SourceResourceId: resource.SourceResourceID,
				Name:             resource.Name,
				State:            resource.State,
				ObservedAt:       resource.ObservedAt.Unix(),
			})
		}

		for _, absent := range store.AbsentNodes() {
			report.AbsentNodes = append(report.AbsentNodes, absent.String())
		}

		for _, conflict := range store.Conflicts() {
			report.Conflicts = append(report.Conflicts, &rpcpb.ExternalDiscoveryConflict{
				Kind:                conflict.Kind,
				Key:                 conflict.Key,
				Reason:              conflict.Reason,
				ObservedFingerprint: conflict.ObservedFingerprint,
				KnownFingerprint:    conflict.KnownFingerprint,
			})
		}

		resp.Providers = append(resp.Providers, report)
	}

	return resp
}

// discoveryErrorString renders a scan error for the wire.
//
// The error is the provider's own message. It never contains a
// credential: the HTTP client builds its errors without the request
// body or the Authorization header, and the token is never logged.
func discoveryErrorString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// sortStrings sorts a string slice in place.
func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}

// GetExternalDiscovery implements rpcpb.ManagerServiceServer: the
// read-only external discovery report.
//
// Viewer-tier. It reads Apiary's own record of what other management
// planes said, and changes nothing.
//
// The returned report is a SNAPSHOT of what was last observed, not a
// live query of the provider: a caller does not trigger a scan, and a
// provider that is unreachable shows up as its last known state marked
// stale, not as an error at call time. That is deliberate - making
// every read trigger an outbound request would turn a UI page load
// into a load on somebody else's API.
func (s *Server) GetExternalDiscovery(ctx context.Context, req *rpcpb.GetExternalDiscoveryRequest) (*rpcpb.GetExternalDiscoveryResponse, error) {
	if s.discovery == nil {
		return &rpcpb.GetExternalDiscoveryResponse{}, nil
	}
	return s.discovery.report(req.GetProvider()), nil
}
