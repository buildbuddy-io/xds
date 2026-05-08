package snapshot

import (
	"context"
	"sync"
	"time"

	"github.com/envoyproxy/go-control-plane/pkg/cache/types"
	"github.com/envoyproxy/go-control-plane/pkg/cache/v3"
	"github.com/envoyproxy/go-control-plane/pkg/log"
	"github.com/envoyproxy/go-control-plane/pkg/resource/v3"
	"github.com/wongnai/xds/meter"
	"go.opentelemetry.io/otel/metric"
	"golang.org/x/sync/errgroup"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes"
	k8scache "k8s.io/client-go/tools/cache"
	"k8s.io/klog/v2"
)

var Logger log.Logger = &log.LoggerFuncs{
	DebugFunc: func(s string, i ...interface{}) {
		klog.V(4).InfofDepth(1, s, i...)
	},
	InfoFunc: func(s string, i ...interface{}) {
		klog.V(2).InfofDepth(1, s, i...)
	},
	WarnFunc: func(s string, i ...interface{}) {
		klog.WarningfDepth(1, s, i...)
	},
	ErrorFunc: func(s string, i ...interface{}) {
		klog.ErrorfDepth(1, s, i...)
	},
}

func mapTypeURL(typeURL string) string {
	switch typeURL {
	case resource.ListenerType, resource.RouteType, resource.ClusterType:
		return "services"
	case resource.EndpointType:
		return "endpoints"
	default:
		return ""
	}
}

type Snapshotter struct {
	ResyncPeriod time.Duration

	client         kubernetes.Interface
	servicesCache  cache.SnapshotCache
	endpointsCache *endpointsCache
	muxCache       cache.MuxCache

	nodeLocality *nodeLocalityStore

	endpointResourceCache map[string]endpointCacheItem

	resourcesByTypeLock     sync.RWMutex
	serviceResourcesByType  map[string][]types.Resource
	endpointResourcesByType map[string][]types.Resource
	apiGatewayStats         map[string]int
	kubeEventCounter        metric.Int64Counter

	// serviceStore is captured at informer start so other code paths
	// (e.g. per-endpoint locality lookups) can consult Service objects.
	serviceStore k8scache.Store

	emitMu          sync.Mutex
	emitEndpointsFn func()
}

// SubZoneLabel selects the Kubernetes node label used as sub-zone for the
// sub_zone-preference locality mode. Empty = use DefaultSubZoneLabel.
type SubZoneLabel string

func New(client kubernetes.Interface, subZoneLabel SubZoneLabel) *Snapshotter {
	servicesCache := cache.NewSnapshotCache(false, EmptyNodeID{}, Logger)
	endpointsCache := newEndpointsCache()
	muxCache := cache.MuxCache{
		Classify: func(r *cache.Request) string {
			return mapTypeURL(r.TypeUrl)
		},
		ClassifyDelta: func(r *cache.DeltaRequest) string {
			return mapTypeURL(r.TypeUrl)
		},
		Caches: map[string]cache.Cache{
			"services":  servicesCache,
			"endpoints": endpointsCache,
		},
	}

	ss := &Snapshotter{
		ResyncPeriod: 10 * time.Minute,

		client:         client,
		servicesCache:  servicesCache,
		endpointsCache: endpointsCache,
		muxCache:       muxCache,

		nodeLocality: newNodeLocalityStore(string(subZoneLabel)),

		endpointResourceCache: map[string]endpointCacheItem{},
	}

	meter := meter.GetMeter()
	ss.kubeEventCounter, _ = meter.Int64Counter("xds_kube_events")
	meter.Int64ObservableGauge("xds_snapshot_resources", metric.WithInt64Callback(ss.snapshotResourceGaugeCallback))
	meter.Int64ObservableGauge("xds_apigateway_endpoints", metric.WithInt64Callback(ss.apiGatewayEndpointGaugeCallback))

	return ss
}

func (s *Snapshotter) MuxCache() *cache.MuxCache {
	return &s.muxCache
}

func (s *Snapshotter) Start(stopCtx context.Context) error {
	group, groupCtx := errgroup.WithContext(stopCtx)
	group.Go(func() error {
		return s.startServices(groupCtx)
	})
	group.Go(func() error {
		return s.startEndpoints(groupCtx)
	})
	group.Go(func() error {
		return s.startNodes(groupCtx)
	})
	return group.Wait()
}

// triggerEndpointsEmit runs the endpoints emit pipeline under a mutex so
// concurrent triggers from multiple informers don't race on the shared
// endpointResourceCache and the emit closure's snapshot-hash state.
func (s *Snapshotter) triggerEndpointsEmit() {
	s.emitMu.Lock()
	defer s.emitMu.Unlock()
	if s.emitEndpointsFn != nil {
		s.emitEndpointsFn()
	}
}

// localityModeFor reads the Service object behind a namespace/name and
// returns its configured locality mode. Returns LocalityNone if the
// service is not yet known.
func (s *Snapshotter) localityModeFor(namespace, name string) LocalityMode {
	if s.serviceStore == nil {
		return LocalityNone
	}
	obj, exists, err := s.serviceStore.GetByKey(namespace + "/" + name)
	if err != nil || !exists {
		return LocalityNone
	}
	svc, ok := obj.(*corev1.Service)
	if !ok {
		return LocalityNone
	}
	return localityModeFromService(svc)
}

func (s *Snapshotter) snapshotResourceGaugeCallback(_ context.Context, result metric.Int64Observer) error {
	for k, r := range s.getServiceResourcesByType() {
		result.Observe(int64(len(r)), metric.WithAttributes(meter.TypeURLAttrKey.String(k)))
	}
	for k, r := range s.getEndpointResourcesByType() {
		result.Observe(int64(len(r)), metric.WithAttributes(meter.TypeURLAttrKey.String(k)))
	}
	return nil
}

func (s *Snapshotter) apiGatewayEndpointGaugeCallback(_ context.Context, result metric.Int64Observer) error {
	for k, stat := range s.getAPIGatewayStats() {
		result.Observe(int64(stat), metric.WithAttributes(meter.APIGatewayAttrKey.String(k)))
	}
	return nil
}

func (s *Snapshotter) setServiceResourcesByType(serviceResourcesByType map[string][]types.Resource) {
	s.resourcesByTypeLock.Lock()
	defer s.resourcesByTypeLock.Unlock()
	s.serviceResourcesByType = serviceResourcesByType
}

func (s *Snapshotter) getServiceResourcesByType() map[string][]types.Resource {
	s.resourcesByTypeLock.RLock()
	defer s.resourcesByTypeLock.RUnlock()
	return s.serviceResourcesByType
}

func (s *Snapshotter) setEndpointResourcesByType(endpointResourcesByType map[string][]types.Resource) {
	s.resourcesByTypeLock.Lock()
	defer s.resourcesByTypeLock.Unlock()
	s.endpointResourcesByType = endpointResourcesByType
}

func (s *Snapshotter) getEndpointResourcesByType() map[string][]types.Resource {
	s.resourcesByTypeLock.RLock()
	defer s.resourcesByTypeLock.RUnlock()
	return s.endpointResourcesByType
}

func (s *Snapshotter) setAPIGatewayStats(apiGatewayStats map[string]int) {
	s.resourcesByTypeLock.Lock()
	defer s.resourcesByTypeLock.Unlock()
	s.apiGatewayStats = apiGatewayStats
}

func (s *Snapshotter) getAPIGatewayStats() map[string]int {
	s.resourcesByTypeLock.RLock()
	defer s.resourcesByTypeLock.RUnlock()
	return s.apiGatewayStats
}
