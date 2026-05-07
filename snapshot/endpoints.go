package snapshot

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/ccoveille/go-safecast/v2"
	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	endpointv3 "github.com/envoyproxy/go-control-plane/envoy/config/endpoint/v3"
	"github.com/envoyproxy/go-control-plane/pkg/cache/types"
	"github.com/wongnai/xds/meter"
	"go.opentelemetry.io/otel/metric"
	"google.golang.org/protobuf/types/known/wrapperspb"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/watch"
	k8scache "k8s.io/client-go/tools/cache"
	"k8s.io/klog/v2"
)

type endpointCacheItem struct {
	version     string
	nodeVersion uint64
	mode        LocalityMode
	resources   []types.Resource
}

func (s *Snapshotter) startEndpoints(ctx context.Context) error {
	store := k8scache.NewUndeltaStore(func(v []interface{}) {
		s.triggerEndpointsEmit()
	}, k8scache.DeletionHandlingMetaNamespaceKeyFunc)

	reflector := k8scache.NewReflector(&k8scache.ListWatch{
		ListWithContextFunc: func(ctx context.Context, options metav1.ListOptions) (runtime.Object, error) {
			return s.client.CoreV1().Endpoints("").List(ctx, options)
		},
		WatchFuncWithContext: func(ctx context.Context, options metav1.ListOptions) (watch.Interface, error) {
			return s.client.CoreV1().Endpoints("").Watch(ctx, options)
		},
	}, &corev1.Endpoints{}, store, s.ResyncPeriod) //nolint:staticcheck // We use deprecated API to support legacy Kubernetes

	var lastSnapshotHash uint64

	emit := func() {
		version := reflector.LastSyncResourceVersion()
		s.kubeEventCounter.Add(ctx, 1, metric.WithAttributes(meter.ResourceAttrKey.String("endpoints")))

		endpoints := sliceToEndpoints(store.List())
		endpointsResources := s.kubeEndpointsToResources(endpoints)
		hash, err := resourcesHash(endpointsResources)
		if err == nil {
			if hash == lastSnapshotHash {
				klog.V(5).Info("new endpoints snapshot is equivalent to the previous one")
				return
			}
			lastSnapshotHash = hash
		} else {
			klog.Errorf("fail to hash snapshot: %s", err)
		}

		resourcesByType := resourcesToMap(endpointsResources)
		s.setEndpointResourcesByType(resourcesByType)

		s.endpointsCache.setResources(ctx, version, resourcesByType)
	}
	s.emitMu.Lock()
	s.emitEndpointsFn = emit
	s.emitMu.Unlock()

	reflector.Run(ctx.Done())
	return nil
}

func sliceToEndpoints(s []interface{}) []*corev1.Endpoints { //nolint:staticcheck // We use deprecated API to support legacy Kubernetes
	out := make([]*corev1.Endpoints, len(s)) //nolint:staticcheck
	for i, v := range s {
		out[i] = v.(*corev1.Endpoints) //nolint:staticcheck
	}
	return out
}

// kubeServicesToResources convert list of Kubernetes endpoints to Endpoint
func (s *Snapshotter) kubeEndpointsToResources(endpoints []*corev1.Endpoints) []types.Resource { //nolint:staticcheck // We use deprecated API to support legacy Kubernetes
	var out []types.Resource

	for _, ep := range endpoints {
		out = append(out, s.kubeEndpointToResources(ep)...)
	}

	return out
}

func (s *Snapshotter) kubeEndpointToResources(ep *corev1.Endpoints) []types.Resource { //nolint:staticcheck // We use deprecated API to support legacy Kubernetes
	name, err := k8scache.MetaNamespaceKeyFunc(ep)
	if err != nil {
		klog.Errorf("fail to get object key: %s", err)
		return nil
	}

	mode := s.localityModeFor(ep.Namespace, ep.Name)
	nodeVersion := s.nodeLocality.getVersion()

	if val, ok := s.endpointResourceCache[name]; ok &&
		val.version == ep.ResourceVersion &&
		val.nodeVersion == nodeVersion &&
		val.mode == mode {
		return val.resources
	}

	var out []types.Resource

	for _, subset := range ep.Subsets {
		for _, port := range subset.Ports {
			var clusterName string
			if port.Name == "" {
				clusterName = fmt.Sprintf("%s.%s:%d", ep.Name, ep.Namespace, port.Port)
			} else {
				clusterName = fmt.Sprintf("%s.%s:%s", ep.Name, ep.Namespace, port.Name)
			}

			cla := &endpointv3.ClusterLoadAssignment{
				ClusterName: clusterName,
			}
			out = append(out, cla)

			sortedAddresses := slices.Clone(subset.Addresses)
			slices.SortStableFunc(sortedAddresses, func(a, b corev1.EndpointAddress) int {
				return strings.Compare(a.IP, b.IP)
			})

			portU32 := safecast.MustConvert[uint32](port.Port)
			groups := map[string]*endpointv3.LocalityLbEndpoints{}

			for _, addr := range sortedAddresses {
				hostname := addr.Hostname
				if hostname == "" && addr.TargetRef != nil {
					hostname = fmt.Sprintf("%s.%s", addr.TargetRef.Name, addr.TargetRef.Namespace)
				}
				if hostname == "" && addr.NodeName != nil {
					hostname = *addr.NodeName
				}

				loc := s.localityForAddress(addr, mode)
				key := loc.GetZone() + localityKeySep + loc.GetSubZone()
				g, ok := groups[key]
				if !ok {
					g = &endpointv3.LocalityLbEndpoints{
						Locality:            loc,
						LoadBalancingWeight: wrapperspb.UInt32(1),
					}
					groups[key] = g
				}
				g.LbEndpoints = append(g.LbEndpoints, &endpointv3.LbEndpoint{
					HostIdentifier: &endpointv3.LbEndpoint_Endpoint{
						Endpoint: &endpointv3.Endpoint{
							Address: &corev3.Address{
								Address: &corev3.Address_SocketAddress{
									SocketAddress: &corev3.SocketAddress{
										Protocol: corev3.SocketAddress_TCP,
										Address:  addr.IP,
										PortSpecifier: &corev3.SocketAddress_PortValue{
											PortValue: portU32,
										},
									},
								},
							},
							Hostname: hostname,
						},
					},
				})
			}

			// Emit in sorted locality-key order so hash is stable.
			for _, k := range slices.Sorted(maps.Keys(groups)) {
				cla.Endpoints = append(cla.Endpoints, groups[k])
			}
		}
	}

	s.endpointResourceCache[name] = endpointCacheItem{
		version:     ep.ResourceVersion,
		nodeVersion: nodeVersion,
		mode:        mode,
		resources:   out,
	}

	return out
}

// localityForAddress builds the Locality for a single endpoint
// address according to the service's locality mode.
func (s *Snapshotter) localityForAddress(addr corev1.EndpointAddress, mode LocalityMode) *corev3.Locality {
	if mode == LocalityNone || addr.NodeName == nil {
		return &corev3.Locality{}
	}
	info := s.nodeLocality.get(*addr.NodeName)
	switch mode {
	case LocalityZone:
		return &corev3.Locality{Zone: info.zone}
	case LocalitySubZone:
		return &corev3.Locality{Zone: info.zone, SubZone: info.subZone}
	default:
		return &corev3.Locality{}
	}
}
