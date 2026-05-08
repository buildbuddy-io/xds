package report

import (
	"context"
	"sync"

	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	loadReportingService "github.com/envoyproxy/go-control-plane/envoy/service/load_stats/v3"
	"github.com/wongnai/xds/meter"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/durationpb"
	"k8s.io/klog/v2"
)

type server struct {
	loadReportingService.UnimplementedLoadReportingServiceServer

	lock           sync.Mutex
	nodesConnected map[string]bool

	statsIntervalInSeconds int64
	statsUpdateCounter     metric.Int64Counter
	nodeGauge              metric.Int64UpDownCounter

	successCounter metric.Int64Counter
	errorCounter   metric.Int64Counter
	issuedCounter  metric.Int64Counter
	droppedCounter metric.Int64Counter
}

type Option func(s *server)

func NewServer(opts ...Option) loadReportingService.LoadReportingServiceServer {
	m := meter.GetMeter()
	lrsUpdatesCounter, _ := m.Int64Counter("lrs_updates")
	lrsNodesCounter, _ := m.Int64UpDownCounter("lrs_nodes")
	successCounter, _ := m.Int64Counter("lrs_upstream_requests_success")
	errorCounter, _ := m.Int64Counter("lrs_upstream_requests_error")
	issuedCounter, _ := m.Int64Counter("lrs_upstream_requests_issued")
	droppedCounter, _ := m.Int64Counter("lrs_upstream_requests_dropped")
	s := &server{
		nodesConnected:         make(map[string]bool),
		statsIntervalInSeconds: 300,
		statsUpdateCounter:     lrsUpdatesCounter,
		nodeGauge:              lrsNodesCounter,
		successCounter:         successCounter,
		errorCounter:           errorCounter,
		issuedCounter:          issuedCounter,
		droppedCounter:         droppedCounter,
	}

	for _, o := range opts {
		o(s)
	}

	return s
}

func (s *server) StreamLoadStats(stream loadReportingService.LoadReportingService_StreamLoadStatsServer) error {
	// Node is only included in the first request on the stream.
	var node *corev3.Node
	for {
		req, err := stream.Recv()
		if err != nil {
			if node != nil {
				s.removeNode(stream.Context(), node)
			}
			return err
		}
		if node == nil {
			node = req.Node
		}
		if node == nil {
			klog.Warning("LRS stream received initial request without Node; closing")
			return status.Error(codes.InvalidArgument, "first LoadStatsRequest must include node")
		}

		s.HandleRequest(stream, node, req)
	}
}

func (s *server) HandleRequest(stream loadReportingService.LoadReportingService_StreamLoadStatsServer, node *corev3.Node, request *loadReportingService.LoadStatsRequest) {
	nodeID := node.Id

	s.statsUpdateCounter.Add(stream.Context(), 1)

	s.lock.Lock()
	_, alreadyConnected := s.nodesConnected[nodeID]
	if !alreadyConnected {
		klog.V(4).InfoS("New node connected", "node_id", nodeID, "cluster_str", node.Cluster)
		s.nodesConnected[nodeID] = true
		s.nodeGauge.Add(stream.Context(), 1)
	}
	s.lock.Unlock()

	if !alreadyConnected {
		err := stream.Send(&loadReportingService.LoadStatsResponse{
			SendAllClusters:           true,
			LoadReportingInterval:     &durationpb.Duration{Seconds: s.statsIntervalInSeconds},
			ReportEndpointGranularity: true,
		})
		if err != nil {
			klog.Errorf("Unable to send response to node %s due to err: %s", nodeID, err)
			// Cleanup happens in StreamLoadStats when Recv fails on this stream.
		}
		return
	}

	s.recordStats(stream.Context(), node, request)
}

func (s *server) recordStats(ctx context.Context, node *corev3.Node, request *loadReportingService.LoadStatsRequest) {
	srcLocality := node.GetLocality()
	sourceZone := srcLocality.GetZone()
	sourceSubZone := srcLocality.GetSubZone()

	for _, clusterStats := range request.ClusterStats {
		clusterName := clusterStats.GetClusterName()

		for _, locStats := range clusterStats.UpstreamLocalityStats {
			dst := locStats.GetLocality()
			dstZone := dst.GetZone()
			dstSubZone := dst.GetSubZone()
			attrs := metric.WithAttributes(
				attribute.String("cluster", clusterName),
				attribute.String("source_zone", sourceZone),
				attribute.String("source_sub_zone", sourceSubZone),
				attribute.String("destination_zone", dstZone),
				attribute.String("destination_sub_zone", dstSubZone),
				attribute.String("cross_zone", crossLocality(sourceZone, dstZone)),
				attribute.String("cross_sub_zone", crossLocality(sourceSubZone, dstSubZone)),
			)

			if locStats.TotalSuccessfulRequests > 0 {
				s.successCounter.Add(ctx, int64(locStats.TotalSuccessfulRequests), attrs)
			}
			if locStats.TotalErrorRequests > 0 {
				s.errorCounter.Add(ctx, int64(locStats.TotalErrorRequests), attrs)
			}
			if locStats.TotalIssuedRequests > 0 {
				s.issuedCounter.Add(ctx, int64(locStats.TotalIssuedRequests), attrs)
			}

			klog.V(4).InfoS("Got locality stats",
				"node_id", node.Id,
				"cluster", clusterName,
				"source_zone", sourceZone,
				"source_sub_zone", sourceSubZone,
				"destination_zone", dstZone,
				"destination_sub_zone", dstSubZone,
				"successful", locStats.TotalSuccessfulRequests,
				"error", locStats.TotalErrorRequests,
				"in_progress", locStats.TotalRequestsInProgress,
				"issued", locStats.TotalIssuedRequests,
			)
		}

		// Cluster-level dropped requests have no destination locality.
		baseDropAttrs := []attribute.KeyValue{
			attribute.String("cluster", clusterName),
			attribute.String("source_zone", sourceZone),
			attribute.String("source_sub_zone", sourceSubZone),
		}
		for _, dropped := range clusterStats.DroppedRequests {
			s.droppedCounter.Add(ctx, int64(dropped.DroppedCount), metric.WithAttributes(
				append(baseDropAttrs, attribute.String("category", dropped.Category))...,
			))
		}
	}
}

func (s *server) removeNode(ctx context.Context, node *corev3.Node) {
	s.lock.Lock()
	defer s.lock.Unlock()

	if _, exist := s.nodesConnected[node.Id]; !exist {
		return
	}
	delete(s.nodesConnected, node.Id)

	klog.V(4).InfoS("Node disconnected", "node_id", node.Id, "cluster_str", node.Cluster)

	s.nodeGauge.Add(ctx, -1)
}

func WithStatsIntervalInSeconds(statsIntervalInSeconds int64) Option {
	return func(s *server) {
		s.statsIntervalInSeconds = statsIntervalInSeconds
	}
}

// crossLocality returns a metric value to indicate whether src and dst match.
// If either src or dst is not populated, we can't know whether the traffic
// is crossing localities. In that case we populate the value with "unknown" to
// avoid incorrectly flagging that traffic as cross-locality.
func crossLocality(src, dst string) string {
	if src == "" || dst == "" {
		return "unknown"
	}
	if src != dst {
		return "true"
	}
	return "false"
}
