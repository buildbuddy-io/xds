package snapshot

import (
	"context"
	"sync"

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/klog/v2"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8scache "k8s.io/client-go/tools/cache"
)

type nodeLocality struct {
	zone    string
	subZone string
}

// nodeLocalityStore holds a snapshot of node to {zone, sub-zone} mapping.
// The endpoints builder consults this store when assigning locality to each
// LbEndpoint.
// Version is bumped on every observed change so callers can invalidate
// per-endpoint caches.
type nodeLocalityStore struct {
	subZoneLabel string

	mu      sync.RWMutex
	nodes   map[string]nodeLocality
	version uint64
}

func newNodeLocalityStore(subZoneLabel string) *nodeLocalityStore {
	if subZoneLabel == "" {
		subZoneLabel = DefaultSubZoneLabel
	}
	return &nodeLocalityStore{
		subZoneLabel: subZoneLabel,
		nodes:        map[string]nodeLocality{},
	}
}

func (s *nodeLocalityStore) get(name string) nodeLocality {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.nodes[name]
}

func (s *nodeLocalityStore) getVersion() uint64 {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.version
}

// apply replaces the store with the nodes. Returns true if anything changed.
func (s *nodeLocalityStore) apply(nodes []*corev1.Node) bool {
	next := make(map[string]nodeLocality, len(nodes))
	for _, n := range nodes {
		next[n.Name] = nodeLocality{
			zone:    n.Labels[LabelZone],
			subZone: n.Labels[s.subZoneLabel],
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if nodeMapsEqual(s.nodes, next) {
		return false
	}
	s.nodes = next
	s.version++
	return true
}

func nodeMapsEqual(a, b map[string]nodeLocality) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

func (s *Snapshotter) startNodes(ctx context.Context) error {
	store := k8scache.NewUndeltaStore(func(v []interface{}) {
		nodes := make([]*corev1.Node, 0, len(v))
		for _, obj := range v {
			if n, ok := obj.(*corev1.Node); ok {
				nodes = append(nodes, n)
			}
		}
		if s.nodeLocality.apply(nodes) {
			klog.V(2).Infof("node locality updated to version %d (%d nodes)", s.nodeLocality.getVersion(), len(nodes))
			s.triggerEndpointsEmit()
		}
	}, k8scache.DeletionHandlingMetaNamespaceKeyFunc)

	reflector := k8scache.NewReflector(&k8scache.ListWatch{
		ListWithContextFunc: func(ctx context.Context, options metav1.ListOptions) (runtime.Object, error) {
			return s.client.CoreV1().Nodes().List(ctx, options)
		},
		WatchFuncWithContext: func(ctx context.Context, options metav1.ListOptions) (watch.Interface, error) {
			return s.client.CoreV1().Nodes().Watch(ctx, options)
		},
	}, &corev1.Node{}, store, s.ResyncPeriod)

	reflector.Run(ctx.Done())
	return nil
}
