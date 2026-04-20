package test_test

import (
	"context"
	"fmt"
	"net"
	"testing"
	"time"

	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	endpointv3 "github.com/envoyproxy/go-control-plane/envoy/config/endpoint/v3"
	discoveryv3 "github.com/envoyproxy/go-control-plane/envoy/service/discovery/v3"
	"github.com/envoyproxy/go-control-plane/pkg/resource/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	"github.com/stretchr/testify/suite"
	"github.com/wongnai/xds/internal/di"
	"github.com/wongnai/xds/snapshot"
	"github.com/wongnai/xds/snapshot/apigateway"
	"github.com/wongnai/xds/test"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/xds"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	clientfeatures "k8s.io/client-go/features"
	clientfeaturestesting "k8s.io/client-go/features/testing"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/klog/v2"
)

// xdsServerBind is where the xDS Server is listening. Since the port is :0, use s.listener.Addr().String() to get the actual address
const xdsServerBind = "127.2.0.1:0"

type XdsIntegrationTestSuite struct {
	suite.Suite
	di.TestServer

	listener           net.Listener
	kube               *fake.Clientset
	activeFakeServices []*test.FakeService

	fakeServiceIP uint8
}

func (s *XdsIntegrationTestSuite) SetupSuite() {
	lc := net.ListenConfig{}
	listener, err := lc.Listen(s.T().Context(), "tcp", xdsServerBind)
	s.Require().NoError(err)
	s.listener = listener

	go func() {
		err := s.TestServer.GrpcServer.Serve(listener)
		if err != nil {
			s.T().Error(err)
		}
	}()
}

func (s *XdsIntegrationTestSuite) TearDownTest() {
	// TODO: Clear s.kube.Tracker
	s.kube.ClearActions()
	for _, service := range s.activeFakeServices {
		service.AssertExpectations(s.T())
		service.Stop()
	}
	s.activeFakeServices = nil
	s.fakeServiceIP = 0
	klog.Flush()
}

func (s *XdsIntegrationTestSuite) TearDownSuite() {
	s.TestServer.GrpcServer.Stop()
	s.listener.Close()
}

func (s *XdsIntegrationTestSuite) getClient(target string) grpc_health_v1.HealthClient {
	xdsBuilder, err := xds.NewXDSResolverWithConfigForTesting([]byte(fmt.Sprintf(`{
		"xds_servers": [{
			"server_uri": "%s",
			"channel_creds": [{"type": "insecure"}],
            "server_features": ["xds_v3"]
		}],
		"node": {
			"id": "test",
			"locality": {
				"zone" : "test"
			}
		}
	}`, s.listener.Addr().String())))
	s.Require().NoError(err)
	client, err := grpc.NewClient(target, grpc.WithResolvers(xdsBuilder), grpc.WithTransportCredentials(insecure.NewCredentials()))
	s.Require().NoError(err)

	healthClient := grpc_health_v1.NewHealthClient(client)
	return healthClient
}

func (s *XdsIntegrationTestSuite) createFakeService(serviceName string, namespace string, port int32, register bool) *test.FakeService {
	svc, err := test.NewFakeService(fmt.Sprintf("%s:%d", s.getFakeServiceIP(), port))
	s.Require().NoError(err)
	svc.Test(s.T())
	s.activeFakeServices = append(s.activeFakeServices, svc)

	if register {
		s.createKubeService(serviceName, namespace, port)
		s.createKubeEndpoint(serviceName, namespace, svc.Host(), svc.Port())
	}

	return svc
}

func (s *XdsIntegrationTestSuite) createKubeService(serviceName string, namespace string, servicePort int32) {
	svc := &test.K8SService{
		Name:      serviceName,
		Namespace: namespace,
		Ports: []corev1.ServicePort{{
			Name:     "grpc",
			Port:     servicePort,
			Protocol: corev1.ProtocolTCP,
		}},
	}
	err := s.kube.Tracker().Add(svc.AsK8S())
	s.Require().NoError(err)
}

func (s *XdsIntegrationTestSuite) createKubeEndpoint(serviceName string, namespace string, ip string, port int32) {
	endpoint := &test.K8SEndpoint{
		Name:      serviceName,
		Namespace: namespace,
		IP:        []string{ip},
		Ports: []corev1.EndpointPort{{
			Name: "grpc",
			Port: port,
		}},
	}
	err := s.kube.Tracker().Add(endpoint.AsK8S()) //nolint:staticcheck // We use Endpoint to simulate legacy Kube compatibility
	s.Require().NoError(err)
}

func (s *XdsIntegrationTestSuite) getFakeServiceIP() string {
	out := s.fakeServiceIP
	s.fakeServiceIP += 1

	return fmt.Sprintf("127.2.1.%d", out)
}

func (s *XdsIntegrationTestSuite) TestValidTarget() {
	svc := s.createFakeService("app", "default", 0, false)
	s.createKubeService("app", "default", 1)
	s.createKubeEndpoint("app", "default", svc.Host(), svc.Port())

	// Test that the client is able to connect
	client := s.getClient("xds:///app.default:1")
	s.T().Run("initial", func(t *testing.T) {
		svc.On("Check", mock.Anything, "test").Return(&grpc_health_v1.HealthCheckResponse{Status: grpc_health_v1.HealthCheckResponse_SERVING}, nil)

		_, err := client.Check(t.Context(), &grpc_health_v1.HealthCheckRequest{Service: "test"})
		assert.NoError(t, err)
	})

	// Test that once the backend IP change, the client connects to the new one
	svc.Stop() // Stop the old one

	// Create unrelated apps to simulate unrelated events
	s.createKubeService("app", "unused", 1)
	s.createKubeEndpoint("app", "unused", "0.0.0.1", 1)

	svc = s.createFakeService("app", "default", 0, false)
	err := s.kube.Tracker().Update(
		schema.GroupVersionResource{Group: "", Version: "v1", Resource: "endpoints"},
		&corev1.Endpoints{ //nolint:staticcheck // We use Endpoint to simulate legacy Kube compatibility
			TypeMeta: metav1.TypeMeta{
				APIVersion: "v1",
				Kind:       "Endpoints",
			},
			ObjectMeta: metav1.ObjectMeta{
				Name:      "app",
				Namespace: "default",
			},
			Subsets: []corev1.EndpointSubset{{ //nolint:staticcheck // See above
				Addresses: []corev1.EndpointAddress{{ //nolint:staticcheck // See above
					IP: svc.Host(),
				}},
				Ports: []corev1.EndpointPort{ //nolint:staticcheck // See above
					{
						Name: "grpc",
						Port: svc.Port(),
					},
					{
						Name: "http",
						Port: 9999,
					},
				},
			}},
		},
		"default",
	)
	s.Require().NoError(err)

	// It doesn't seems that XDS propagation works in test??
	// s.T().Run("updated", func(t *testing.T) {
	//	svc.On("Check", mock.Anything, "test2").Return(&grpc_health_v1.HealthCheckResponse{Status: grpc_health_v1.HealthCheckResponse_SERVING}, nil)
	//
	//	_, err := client.Check(t.Context(), &grpc_health_v1.HealthCheckRequest{Service: "test2"})
	//	assert.NoError(t, err)
	// })
}

func (s *XdsIntegrationTestSuite) TestApiGateway() {
	svc1 := s.createFakeService("apigwbackend1", "default", 50000, false)
	svc1Manifest := &test.K8SService{
		Name:      "apigwbackend1",
		Namespace: "default",
		Annotations: map[string]string{
			apigateway.NameAnnotation:    "apigw1,apigw2",
			apigateway.ServiceAnnotation: "grpc.health.v1.Health,lmwn.inexists.v1.Test",
		},
		Ports: []corev1.ServicePort{{
			Name:     "grpc",
			Port:     50000,
			Protocol: corev1.ProtocolTCP,
		}},
	}
	err := s.kube.Tracker().Add(svc1Manifest.AsK8S())
	s.Require().NoError(err)
	s.createKubeEndpoint("apigwbackend1", "default", svc1.Host(), svc1.Port())

	svc2 := s.createFakeService("apigwbackend2", "default", 50001, false)
	svc2Manifest := &test.K8SService{
		Name:      "apigwbackend2",
		Namespace: "default",
		Annotations: map[string]string{
			apigateway.NameAnnotation:    "apigw1,apigw2",
			apigateway.ServiceAnnotation: "",
		},
		Ports: []corev1.ServicePort{{
			Name:     "grpc",
			Port:     50001,
			Protocol: corev1.ProtocolTCP,
		}},
	}
	err = s.kube.Tracker().Add(svc2Manifest.AsK8S())
	s.Require().NoError(err)
	s.createKubeEndpoint("apigwbackend2", "default", svc2.Host(), svc2.Port())

	svc1.On("Check", mock.Anything, "test").Return(&grpc_health_v1.HealthCheckResponse{Status: grpc_health_v1.HealthCheckResponse_SERVING}, nil)

	client := s.getClient("xds:///apigw1")
	_, err = client.Check(s.T().Context(), &grpc_health_v1.HealthCheckRequest{Service: "test"})
	require.NoError(s.T(), err)
}

// localityEndpoint describes one backend to register behind a Service, along
// with which Node it runs on.
type localityEndpoint struct {
	IP       string
	NodeName string
}

func (s *XdsIntegrationTestSuite) createKubeNode(name, zone, subZone string) {
	node := &corev1.Node{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Node"},
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
			Labels: map[string]string{
				snapshot.LabelZone:           zone,
				snapshot.DefaultSubZoneLabel: subZone,
			},
		},
	}
	err := s.kube.Tracker().Add(node)
	s.Require().NoError(err)
}

func (s *XdsIntegrationTestSuite) createLocalityService(name, namespace string, port int32, mode string) {
	svc := &test.K8SService{
		Name:      name,
		Namespace: namespace,
		Annotations: map[string]string{
			snapshot.AnnotationLocalityPreference: mode,
		},
		Ports: []corev1.ServicePort{{
			Name:     "grpc",
			Port:     port,
			Protocol: corev1.ProtocolTCP,
		}},
	}
	err := s.kube.Tracker().Add(svc.AsK8S())
	s.Require().NoError(err)
}

func (s *XdsIntegrationTestSuite) createKubeEndpointWithNodes(name, namespace string, addrs []localityEndpoint, port int32) {
	addresses := make([]corev1.EndpointAddress, 0, len(addrs)) //nolint:staticcheck // legacy Endpoints API
	for _, a := range addrs {
		nodeName := a.NodeName
		addresses = append(addresses, corev1.EndpointAddress{ //nolint:staticcheck // See above
			IP:       a.IP,
			NodeName: &nodeName,
		})
	}
	endpoint := &corev1.Endpoints{ //nolint:staticcheck // See above
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Endpoints"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
		Subsets: []corev1.EndpointSubset{{ //nolint:staticcheck // See above
			Addresses: addresses,
			Ports:     []corev1.EndpointPort{{Name: "grpc", Port: port}}, //nolint:staticcheck // See above
		}},
	}
	err := s.kube.Tracker().Add(endpoint)
	s.Require().NoError(err)
}

// fetchEDS opens a one-shot ADS stream with the given client locality and
// returns the first matching ClusterLoadAssignment. The stream is
// re-established each call so snapshot-version state is fresh. Uses Eventually
// because the K8s reflector is asynchronous.
func (s *XdsIntegrationTestSuite) fetchEDS(clientZone, clientSubZone, resourceName string, expectedGroups int) *endpointv3.ClusterLoadAssignment {
	s.T().Helper()
	var cla *endpointv3.ClusterLoadAssignment
	s.Require().Eventually(func() bool {
		conn, err := grpc.NewClient(s.listener.Addr().String(),
			grpc.WithTransportCredentials(insecure.NewCredentials()))
		if err != nil {
			return false
		}
		defer conn.Close()

		ctx, cancel := context.WithTimeout(s.T().Context(), 2*time.Second)
		defer cancel()

		stream, err := discoveryv3.NewAggregatedDiscoveryServiceClient(conn).StreamAggregatedResources(ctx)
		if err != nil {
			return false
		}
		err = stream.Send(&discoveryv3.DiscoveryRequest{
			Node: &corev3.Node{
				Id:       "test-" + clientZone + "-" + clientSubZone,
				Locality: &corev3.Locality{Zone: clientZone, SubZone: clientSubZone},
			},
			ResourceNames: []string{resourceName},
			TypeUrl:       resource.EndpointType,
		})
		if err != nil {
			return false
		}
		resp, err := stream.Recv()
		if err != nil || len(resp.Resources) == 0 {
			return false
		}
		candidate := &endpointv3.ClusterLoadAssignment{}
		if err := resp.Resources[0].UnmarshalTo(candidate); err != nil {
			return false
		}
		if len(candidate.Endpoints) != expectedGroups {
			return false
		}
		cla = candidate
		return true
	}, 5*time.Second, 100*time.Millisecond, "timed out waiting for CLA with %d locality groups", expectedGroups)
	return cla
}

func (s *XdsIntegrationTestSuite) TestLocalityZonePriorities() {
	s.createKubeNode("node-a", "zone-a", "")
	s.createKubeNode("node-b", "zone-b", "")
	s.createKubeNode("node-c", "zone-c", "")

	s.createLocalityService("zsvc", "default", 50100, "zone")
	s.createKubeEndpointWithNodes("zsvc", "default", []localityEndpoint{
		{IP: "10.200.0.1", NodeName: "node-a"},
		{IP: "10.200.0.2", NodeName: "node-b"},
		{IP: "10.200.0.3", NodeName: "node-c"},
	}, 50100)

	// Client in zone-b: zone-b should be priority 0, others contiguous at 1.
	cla := s.fetchEDS("zone-b", "", "zsvc.default:grpc", 3)
	priByZone := map[string]uint32{}
	for _, g := range cla.Endpoints {
		priByZone[g.GetLocality().GetZone()] = g.GetPriority()
	}
	s.Equal(uint32(0), priByZone["zone-b"], "zone-b should be preferred for zone-b client")
	s.Equal(uint32(1), priByZone["zone-a"])
	s.Equal(uint32(1), priByZone["zone-c"])

	// Different client locality flips the priorities.
	cla = s.fetchEDS("zone-a", "", "zsvc.default:grpc", 3)
	priByZone = map[string]uint32{}
	for _, g := range cla.Endpoints {
		priByZone[g.GetLocality().GetZone()] = g.GetPriority()
	}
	s.Equal(uint32(0), priByZone["zone-a"], "zone-a should be preferred for zone-a client")
	s.Equal(uint32(1), priByZone["zone-b"])
	s.Equal(uint32(1), priByZone["zone-c"])
}

func (s *XdsIntegrationTestSuite) TestLocalitySubZonePriorities() {
	s.createKubeNode("n1", "zone-a", "rack-1")
	s.createKubeNode("n2", "zone-a", "rack-2")
	s.createKubeNode("n3", "zone-b", "rack-1")

	s.createLocalityService("ssvc", "default", 50101, "sub_zone")
	s.createKubeEndpointWithNodes("ssvc", "default", []localityEndpoint{
		{IP: "10.201.0.1", NodeName: "n1"},
		{IP: "10.201.0.2", NodeName: "n2"},
		{IP: "10.201.0.3", NodeName: "n3"},
	}, 50101)

	// Client at (zone-a, rack-1):
	//   exact match     → priority 0
	//   same-zone only  → priority 1
	//   different zone  → priority 2
	cla := s.fetchEDS("zone-a", "rack-1", "ssvc.default:grpc", 3)
	priByLoc := map[string]uint32{}
	for _, g := range cla.Endpoints {
		loc := g.GetLocality()
		priByLoc[loc.GetZone()+"/"+loc.GetSubZone()] = g.GetPriority()
	}
	s.Equal(uint32(0), priByLoc["zone-a/rack-1"])
	s.Equal(uint32(1), priByLoc["zone-a/rack-2"])
	s.Equal(uint32(2), priByLoc["zone-b/rack-1"])
}

func (s *XdsIntegrationTestSuite) TestLocalityNoAnnotationNoSplit() {
	// Without the annotation, all endpoints should land in a single
	// empty-locality group regardless of node labels — behavior matches the
	// pre-locality default.
	s.createKubeNode("plain-a", "zone-a", "")
	s.createKubeNode("plain-b", "zone-b", "")

	s.createKubeService("plainsvc", "default", 50102)
	s.createKubeEndpointWithNodes("plainsvc", "default", []localityEndpoint{
		{IP: "10.202.0.1", NodeName: "plain-a"},
		{IP: "10.202.0.2", NodeName: "plain-b"},
	}, 50102)

	cla := s.fetchEDS("zone-a", "", "plainsvc.default:grpc", 1)
	s.Require().Len(cla.Endpoints, 1)
	s.Equal(uint32(0), cla.Endpoints[0].GetPriority())
	s.Empty(cla.Endpoints[0].GetLocality().GetZone())
	s.Len(cla.Endpoints[0].GetLbEndpoints(), 2)
}

func TestXdsIntegration(t *testing.T) {
	// The fake clientset doesn't implement the WatchList bookmark protocol,
	// so disable WatchListClient to use the traditional List+Watch flow.
	clientfeaturestesting.SetFeatureDuringTest(t, clientfeatures.WatchListClient, false)

	kube := fake.NewClientset()

	testServer, stop, err := di.InitializeTestServer(t.Context(), kube, 1, "")
	require.NoError(t, err)
	defer stop()

	suite.Run(t, &XdsIntegrationTestSuite{
		TestServer: testServer,
		kube:       kube,
	})
}
