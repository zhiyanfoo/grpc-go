/*
 *
 * Copyright 2025 gRPC authors.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 *
 */

package xds_test

import (
	"context"
	"fmt"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/internal/envconfig"
	"google.golang.org/grpc/internal/stubserver"
	"google.golang.org/grpc/internal/testutils"
	"google.golang.org/grpc/internal/testutils/xds/e2e"
	e2esetup "google.golang.org/grpc/internal/testutils/xds/e2e/setup"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	v3clusterpb "github.com/envoyproxy/go-control-plane/envoy/config/cluster/v3"
	v3endpointpb "github.com/envoyproxy/go-control-plane/envoy/config/endpoint/v3"
	v3listenerpb "github.com/envoyproxy/go-control-plane/envoy/config/listener/v3"
	v3routepb "github.com/envoyproxy/go-control-plane/envoy/config/route/v3"
	testpb "google.golang.org/grpc/interop/grpc_testing"
)

// TestHostRewriteLiteral tests that if a client receives it's configuration via xDS and it has
// the host rewrite literal value configured, that the authority pseudo-header in the rpc call
// makes has it set
func (s) TestHostRewriteLiteral(t *testing.T) {
	originalEnvVarValue := envconfig.XDSExperimentalAuthoriyLiteralRewrite
	defer func() {
		envconfig.XDSExperimentalAuthoriyLiteralRewrite = originalEnvVarValue
	}()
	envconfig.XDSExperimentalAuthoriyLiteralRewrite = true

	ctx, cancel := context.WithTimeout(context.Background(), defaultTestTimeout)
	defer cancel()

	const hostRewriteValue = "rewritten.host.example.com"

	const dialTargetName = "original.target.example.com" // Used in xds:/// URI and matched by Listener & VirtualHost
	const routeName = "test-route"
	const clusterName = "test-cluster"
	const edsServiceName = "test-eds-service"

	authorityCh := make(chan string, 1) // Channel to receive the authority
	stubServer := &stubserver.StubServer{
		EmptyCallF: func(ctx context.Context, _ *testpb.Empty) (*testpb.Empty, error) {
			md, ok := metadata.FromIncomingContext(ctx)
			if !ok {
				return nil, status.Error(codes.Internal, "could not get metadata from context")
			}
			authorities := md.Get(":authority")
			var authToSend string
			if len(authorities) > 0 {
				authToSend = authorities[0]
			} else {
				authToSend = "[authority not found]"
			}
			authorityCh <- authToSend // Send authority via channel
			return &testpb.Empty{}, nil
		},
	}
	if err := stubServer.StartServer(); err != nil {
		t.Fatalf("Failed to start stub server: %v", err)
	}
	defer stubServer.Stop()
	t.Logf("Stub server listening at: %s", stubServer.Address)

	xdsServer, nodeID, _, xdsResolver := e2esetup.ManagementServerAndResolver(t)

	// EDS: Pointing to our stub server
	endpoints := e2e.EndpointResourceWithOptions(e2e.EndpointOptions{
		ClusterName: edsServiceName,
		Localities: []e2e.LocalityOptions{{
			Backends: []e2e.BackendOptions{{Ports: []uint32{testutils.ParsePort(t, stubServer.Address)}}},
			Weight:   1,
		}},
	})

	// CDS: Defines the cluster
	cluster := e2e.ClusterResourceWithOptions(e2e.ClusterOptions{
		ClusterName: clusterName,
		ServiceName: edsServiceName,
		Policy:      e2e.LoadBalancingPolicyRoundRobin,
	})

	// RDS: Defines the route with HostRewriteLiteral
	routeConfig := e2e.DefaultRouteConfig(routeName, dialTargetName, clusterName)
	if len(routeConfig.VirtualHosts) > 0 && len(routeConfig.VirtualHosts[0].Routes) > 0 {
		routeProto := routeConfig.VirtualHosts[0].Routes[0]
		actionWrapper, ok := routeProto.Action.(*v3routepb.Route_Route)
		if !ok {
			t.Fatalf("Default route action is not of type *v3routepb.Route_Route")
		}
		if actionWrapper.Route == nil {
			actionWrapper.Route = &v3routepb.RouteAction{}
		}
		routeAction := actionWrapper.Route // Explicitly get the *v3routepb.RouteAction
		routeAction.HostRewriteSpecifier = &v3routepb.RouteAction_HostRewriteLiteral{HostRewriteLiteral: hostRewriteValue}
	} else {
		t.Fatalf("DefaultRouteConfig did not generate expected VirtualHost/Route structure")
	}

	// LDS: Defines the listener that client targets
	listener := e2e.DefaultClientListener(dialTargetName, routeName)

	// Update Management Server with resources
	resources := e2e.UpdateOptions{
		NodeID:         nodeID,
		Endpoints:      []*v3endpointpb.ClusterLoadAssignment{endpoints},
		Clusters:       []*v3clusterpb.Cluster{cluster},
		Routes:         []*v3routepb.RouteConfiguration{routeConfig},
		Listeners:      []*v3listenerpb.Listener{listener},
		SkipValidation: true, // Useful if any default values cause issues with specific test setup
	}
	if err := xdsServer.Update(ctx, resources); err != nil {
		t.Fatalf("xDS server update failed: %v", err)
	}

	cc, err := grpc.NewClient(fmt.Sprintf("xds:///%s", dialTargetName),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithResolvers(xdsResolver),
	)
	if err != nil {
		t.Fatalf("Failed to create gRPC client: %v", err)
	}
	defer cc.Close()

	client := testpb.NewTestServiceClient(cc)

	_, err = client.EmptyCall(ctx, &testpb.Empty{})
	if err != nil {
		t.Errorf("expected err to be nil")
	}

	// Verify :authority header
	var finalReceivedAuthority string
	select {
	case finalReceivedAuthority = <-authorityCh:
	case <-ctx.Done():
		t.Fatalf("Context timed out waiting for authority from server: %v", ctx.Err())
	}

	if finalReceivedAuthority == "" {
		t.Errorf("Test inconclusive: :authority header was not captured by the server (or RPC did not reach the intended handler path)")
	} else if finalReceivedAuthority != hostRewriteValue {
		t.Errorf("Expected :authority header to be '%s', but got '%s'", hostRewriteValue, finalReceivedAuthority)
	} else {
		t.Logf("Successfully received :authority '%s' as expected", finalReceivedAuthority)
	}
}
