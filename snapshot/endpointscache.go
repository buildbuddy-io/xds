package snapshot

import (
	"context"
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
}

func newEndpointsCache() *endpointsCache {
	return &endpointsCache{
		inner: cache.NewSnapshotCache(false, localityNodeHash{}, Logger),
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
	c.ensureSnapshotForNode(context.Background(), request.GetNode())
	return c.inner.CreateWatch(request, sub, value)
}

// CreateDeltaWatch implements cache.ConfigWatcher.
func (c *endpointsCache) CreateDeltaWatch(request *cache.DeltaRequest, sub cache.Subscription, value chan cache.DeltaResponse) (func(), error) {
	c.ensureSnapshotForNode(context.Background(), request.GetNode())
	return c.inner.CreateDeltaWatch(request, sub, value)
}

// Fetch implements cache.ConfigFetcher.
func (c *endpointsCache) Fetch(ctx context.Context, request *cache.Request) (cache.Response, error) {
	c.ensureSnapshotForNode(ctx, request.GetNode())
	return c.inner.Fetch(ctx, request)
}
