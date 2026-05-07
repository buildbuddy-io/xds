package snapshot

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"

	"github.com/envoyproxy/go-control-plane/pkg/cache/types"
	"github.com/envoyproxy/go-control-plane/pkg/cache/v3"
	"github.com/envoyproxy/go-control-plane/pkg/resource/v3"
	"google.golang.org/protobuf/proto"
	"k8s.io/klog/v2"

	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	endpointv3 "github.com/envoyproxy/go-control-plane/envoy/config/endpoint/v3"
)

// not a valid character in k8s node label values.
const localityKeySep = "|"

// localityNodeHash partitions xDS clients by (zone, sub_zone).
type localityNodeHash struct{}

func (localityNodeHash) ID(node *corev3.Node) string {
	return node.GetLocality().GetZone() + localityKeySep + node.GetLocality().GetSubZone()
}

func splitLocalityKey(k string) (string, string) {
	zone, subZone, _ := strings.Cut(k, localityKeySep)
	return zone, subZone
}

// endpointsCache wraps a SnapshotCache to produce per-client-locality
// endpoint responses. It stores the current endpoint resources and, when
// a client issues a watch from a previously-unseen locality, synthesizes
// a snapshot for that locality on demand.
type endpointsCache struct {
	inner cache.SnapshotCache

	mu              sync.Mutex
	version         string
	resourcesByType map[string][]types.Resource

	logMu sync.Mutex
	// Set of clusters that have been subscribed to.
	// We log backend changes for any clusters that have been subscribed to
	// at least once.
	subscribed map[string]struct{}
	// Last known backend set for a cluster.
	// We only log when this value changes.
	lastBackends map[string]string
}

func newEndpointsCache() *endpointsCache {
	return &endpointsCache{
		inner:        cache.NewSnapshotCache(false, localityNodeHash{}, Logger),
		subscribed:   make(map[string]struct{}),
		lastBackends: make(map[string]string),
	}
}

// setResources replaces the cached endpoint resources and pushes a new
// snapshot for every locality that already has an active watch, plus the
// default (no-locality) key.
func (c *endpointsCache) setResources(ctx context.Context, version string, resourcesByType map[string][]types.Resource) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.version = version
	c.resourcesByType = resourcesByType

	c.logBackendChanges(resourcesByType)

	keys := c.inner.GetStatusKeys()
	if !slices.Contains(keys, "") {
		keys = append(keys, "")
	}
	for _, k := range keys {
		c.setSnapshotForKey(ctx, k, version, resourcesByType)
	}
}

func (c *endpointsCache) setSnapshotForKey(ctx context.Context, key, version string, resourcesByType map[string][]types.Resource) {
	zone, subZone := splitLocalityKey(key)
	filtered := resourcesForLocality(zone, subZone, resourcesByType)
	snap, err := cache.NewSnapshot(version, filtered)
	if err != nil {
		klog.Errorf("fail to create endpoint snapshot for locality %q: %s", key, err)
		return
	}
	if err := c.inner.SetSnapshot(ctx, key, snap); err != nil {
		klog.Errorf("fail to set endpoint snapshot for locality %q: %s", key, err)
	}
}

// ensureSnapshotForNode makes sure a snapshot exists for the node's locality
// before the inner cache's CreateWatch runs.
func (c *endpointsCache) ensureSnapshotForNode(ctx context.Context, node *corev3.Node) {
	key := localityNodeHash{}.ID(node)
	if _, err := c.inner.GetSnapshot(key); err == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	// Re-check now that we hold the lock.
	// A concurrent setResources or ensureSnapshotForNode may have created the
	// snapshot.
	if _, err := c.inner.GetSnapshot(key); err == nil {
		return
	}
	if c.resourcesByType == nil {
		return
	}
	c.setSnapshotForKey(ctx, key, c.version, c.resourcesByType)
}

// resourcesForLocality returns a copy of resourcesByType where each
// ClusterLoadAssignment's LocalityLbEndpoints are re-prioritized relative
// to the client at (clientZone, clientSubZone). Non-endpoint types pass
// through untouched. Priorities are compacted so they always start at 0
// and are contiguous (gRPC's priority LB requires that).
func resourcesForLocality(clientZone, clientSubZone string, resourcesByType map[string][]types.Resource) map[string][]types.Resource {
	eps, ok := resourcesByType[resource.EndpointType]
	if !ok {
		return resourcesByType
	}
	out := make(map[string][]types.Resource, len(resourcesByType))
	for k, v := range resourcesByType {
		if k != resource.EndpointType {
			out[k] = v
		}
	}
	newEps := make([]types.Resource, 0, len(eps))
	for _, r := range eps {
		cla, ok := r.(*endpointv3.ClusterLoadAssignment)
		if !ok {
			newEps = append(newEps, r)
			continue
		}
		newEps = append(newEps, assignmentWithPriorities(cla, clientZone, clientSubZone))
	}
	out[resource.EndpointType] = newEps
	return out
}

// assignmentWithPriorities clones the input and assigns a priority to each
// LocalityLbEndpoints group based on how closely it matches the client's
// locality. xDS priorities start at 0 (most preferred):
//
//	0: group.zone == client.zone && group.sub_zone == client.sub_zone
//	1: group.zone == client.zone
//	2: no match (or client has no locality info)
//
// Empty buckets are skipped so priorities stay contiguous.
func assignmentWithPriorities(in *endpointv3.ClusterLoadAssignment, clientZone, clientSubZone string) *endpointv3.ClusterLoadAssignment {
	out := proto.Clone(in).(*endpointv3.ClusterLoadAssignment)
	if len(out.Endpoints) == 0 {
		return out
	}
	// matchPriority returns 0, 1, or 2; index buckets by priority directly.
	var byPriority [3][]*endpointv3.LocalityLbEndpoints
	for _, g := range out.Endpoints {
		p := matchPriority(g.GetLocality(), clientZone, clientSubZone)
		byPriority[p] = append(byPriority[p], g)
	}

	priority := uint32(0)
	regrouped := make([]*endpointv3.LocalityLbEndpoints, 0, len(out.Endpoints))
	for _, bucket := range byPriority {
		if len(bucket) == 0 {
			continue
		}
		for _, g := range bucket {
			g.Priority = priority
			regrouped = append(regrouped, g)
		}
		priority++
	}
	out.Endpoints = regrouped
	return out
}

// matchPriority returns the match priority between the client and locality.
// The returned value is between 0 and 2 with 0 being the highest priority
// (most specific match) and 2 being the lowest priority (least specific match).
func matchPriority(loc *corev3.Locality, clientZone, clientSubZone string) int {
	if loc == nil || clientZone == "" {
		return 2
	}
	if loc.GetZone() == "" || loc.GetZone() != clientZone {
		return 2
	}
	if loc.GetSubZone() != "" && clientSubZone != "" && loc.GetSubZone() == clientSubZone {
		return 0
	}
	return 1
}

// CreateWatch implements cache.ConfigWatcher.
func (c *endpointsCache) CreateWatch(request *cache.Request, sub cache.Subscription, value chan cache.Response) (func(), error) {
	c.markSubscribed(request.GetResourceNames())
	c.ensureSnapshotForNode(context.Background(), request.GetNode())
	return c.inner.CreateWatch(request, sub, value)
}

// CreateDeltaWatch implements cache.ConfigWatcher.
func (c *endpointsCache) CreateDeltaWatch(request *cache.DeltaRequest, sub cache.Subscription, value chan cache.DeltaResponse) (func(), error) {
	c.markSubscribed(request.GetResourceNamesSubscribe())
	c.ensureSnapshotForNode(context.Background(), request.GetNode())
	return c.inner.CreateDeltaWatch(request, sub, value)
}

// Fetch implements cache.ConfigFetcher.
func (c *endpointsCache) Fetch(ctx context.Context, request *cache.Request) (cache.Response, error) {
	c.markSubscribed(request.GetResourceNames())
	c.ensureSnapshotForNode(ctx, request.GetNode())
	return c.inner.Fetch(ctx, request)
}

func (c *endpointsCache) GetSnapshot(node string) (cache.ResourceSnapshot, error) {
	return c.inner.GetSnapshot(node)
}

func (c *endpointsCache) GetStatusKeys() []string {
	return c.inner.GetStatusKeys()
}

func (c *endpointsCache) markSubscribed(names []string) {
	if len(names) == 0 {
		return
	}
	c.logMu.Lock()
	newSubscription := false
	for _, n := range names {
		if _, exists := c.subscribed[n]; !exists {
			c.subscribed[n] = struct{}{}
			newSubscription = true
		}
	}
	c.logMu.Unlock()

	// If we already have a subscription then we've already logged the backends.
	if !newSubscription {
		return
	}

	// Otherwise dump the current backend set.

	c.mu.Lock()
	resources := c.resourcesByType
	c.mu.Unlock()
	if resources != nil {
		c.logBackendChanges(resources)
	}
}

// logBackendChanges logs backend changes for subscribed clusters.
// Logs are only emitted if the backend set has changed.
func (c *endpointsCache) logBackendChanges(resourcesByType map[string][]types.Resource) {
	eps := resourcesByType[resource.EndpointType]
	byName := make(map[string]*endpointv3.ClusterLoadAssignment, len(eps))
	for _, r := range eps {
		if cla, ok := r.(*endpointv3.ClusterLoadAssignment); ok {
			byName[cla.GetClusterName()] = cla
		}
	}

	c.logMu.Lock()
	defer c.logMu.Unlock()

	for name := range c.subscribed {
		desc := "(no endpoints)"
		if cla, ok := byName[name]; ok {
			desc = backendDescription(cla)
		}
		if prev := c.lastBackends[name]; prev == desc {
			continue
		}
		c.lastBackends[name] = desc
		klog.InfoS("endpoint set changed", "cluster", name, "backends", desc)
	}
}

// backendDescription returns a deterministic one-line summary of a CLA's
// endpoint set, grouped by locality:
//
//	"[zone/sub_zone] addr:port(hostname),addr:port(hostname) [zone2] addr:port"
func backendDescription(cla *endpointv3.ClusterLoadAssignment) string {
	byLoc := map[string][]string{}
	for _, g := range cla.GetEndpoints() {
		loc := formatLocality(g.GetLocality())
		for _, lbe := range g.GetLbEndpoints() {
			ep := lbe.GetEndpoint()
			sock := ep.GetAddress().GetSocketAddress()
			entry := fmt.Sprintf("%s:%d", sock.GetAddress(), sock.GetPortValue())
			if host := ep.GetHostname(); host != "" {
				entry = fmt.Sprintf("%s(%s)", entry, host)
			}
			byLoc[loc] = append(byLoc[loc], entry)
		}
	}
	locs := make([]string, 0, len(byLoc))
	for loc := range byLoc {
		locs = append(locs, loc)
	}
	slices.Sort(locs)
	parts := make([]string, 0, len(locs))
	for _, loc := range locs {
		addrs := byLoc[loc]
		slices.Sort(addrs)
		parts = append(parts, fmt.Sprintf("[%s] %s", loc, strings.Join(addrs, ",")))
	}
	return strings.Join(parts, " ")
}

func formatLocality(loc *corev3.Locality) string {
	if loc == nil {
		return "-"
	}
	z, sz := loc.GetZone(), loc.GetSubZone()
	switch {
	case z == "" && sz == "":
		return "-"
	case sz == "":
		return z
	default:
		return z + "/" + sz
	}
}
