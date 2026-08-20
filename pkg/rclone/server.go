package rclone

import (
	"context"
	"fmt"
	"net"
	"os"
	"strings"
	"sync"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"k8s.io/klog/v2"
)

// This file replaces github.com/kubernetes-csi/drivers/pkg/csi-common.
//
// That package was archived upstream at v1.0.2 (2019) and has no successor, so
// it could never be updated - it pinned the whole build to the 2019 dependency
// tree (k8s.io/kubernetes v1.13.2, client-go v10.0.0+incompatible, CSI spec
// v1.0.0). Modern CSI drivers implement the generated gRPC interfaces directly,
// which is all this file does. Behaviour is deliberately identical to the
// csi-common defaults we relied on.

// ParseEndpoint splits a CSI endpoint into protocol and address.
//
// NOTE the leading-slash behaviour is load bearing and matches csi-common
// exactly: the deployment passes CSI_ENDPOINT="unix://plugin/csi.sock", which
// parses to addr "plugin/csi.sock" and is then made absolute as
// "/plugin/csi.sock". Dropping that step would move the socket and the
// node-driver-registrar would no longer find the driver.
func ParseEndpoint(ep string) (string, string, error) {
	if strings.HasPrefix(strings.ToLower(ep), "unix://") || strings.HasPrefix(strings.ToLower(ep), "tcp://") {
		s := strings.SplitN(ep, "://", 2)
		if s[1] != "" {
			return s[0], s[1], nil
		}
	}
	return "", "", fmt.Errorf("invalid endpoint: %v", ep)
}

type nonBlockingGRPCServer struct {
	wg     sync.WaitGroup
	server *grpc.Server
}

func newNonBlockingGRPCServer() *nonBlockingGRPCServer {
	return &nonBlockingGRPCServer{}
}

func (s *nonBlockingGRPCServer) Start(endpoint string, ids csi.IdentityServer, cs csi.ControllerServer, ns csi.NodeServer) {
	s.wg.Add(1)
	go s.serve(endpoint, ids, cs, ns)
}

func (s *nonBlockingGRPCServer) Wait() {
	s.wg.Wait()
}

func (s *nonBlockingGRPCServer) serve(endpoint string, ids csi.IdentityServer, cs csi.ControllerServer, ns csi.NodeServer) {
	defer s.wg.Done()

	proto, addr, err := ParseEndpoint(endpoint)
	if err != nil {
		klog.Fatal(err.Error())
	}

	if proto == "unix" {
		addr = "/" + addr
		if err := os.Remove(addr); err != nil && !os.IsNotExist(err) {
			klog.Fatalf("failed to remove %s, error: %s", addr, err.Error())
		}
	}

	listener, err := net.Listen(proto, addr)
	if err != nil {
		klog.Fatalf("failed to listen: %v", err)
	}

	server := grpc.NewServer(grpc.UnaryInterceptor(logGRPC))
	s.server = server

	if ids != nil {
		csi.RegisterIdentityServer(server, ids)
	}
	if cs != nil {
		csi.RegisterControllerServer(server, cs)
	}
	if ns != nil {
		csi.RegisterNodeServer(server, ns)
	}

	klog.Infof("listening for connections on address: %#v", listener.Addr())

	if err := server.Serve(listener); err != nil {
		klog.Fatalf("grpc server stopped serving: %v", err)
	}
}

// logGRPC mirrors csi-common's interceptor: log every RPC and its error. The
// request/response bodies are deliberately NOT logged - volume context carries
// rclone configData, which contains OAuth tokens and crypt passwords.
func logGRPC(ctx context.Context, req interface{}, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (interface{}, error) {
	klog.V(3).Infof("GRPC call: %s", info.FullMethod)
	resp, err := handler(ctx, req)
	if err != nil {
		klog.Errorf("GRPC error for %s: %v", info.FullMethod, err)
	}
	return resp, err
}

// identityServer answers the plugin identity RPCs the kubelet and the
// node-driver-registrar depend on.
type identityServer struct {
	csi.UnimplementedIdentityServer
	name    string
	version string
}

func (ids *identityServer) GetPluginInfo(ctx context.Context, req *csi.GetPluginInfoRequest) (*csi.GetPluginInfoResponse, error) {
	if ids.name == "" {
		return nil, status.Error(codes.Unavailable, "driver name not configured")
	}
	if ids.version == "" {
		return nil, status.Error(codes.Unavailable, "driver is missing version")
	}
	return &csi.GetPluginInfoResponse{
		Name:          ids.name,
		VendorVersion: ids.version,
	}, nil
}

// Probe is what the liveness-probe sidecar calls to decide whether this driver
// is healthy, so it must stay cheap and must not touch the remote.
func (ids *identityServer) Probe(ctx context.Context, req *csi.ProbeRequest) (*csi.ProbeResponse, error) {
	return &csi.ProbeResponse{}, nil
}

func (ids *identityServer) GetPluginCapabilities(ctx context.Context, req *csi.GetPluginCapabilitiesRequest) (*csi.GetPluginCapabilitiesResponse, error) {
	return &csi.GetPluginCapabilitiesResponse{
		Capabilities: []*csi.PluginCapability{
			{
				Type: &csi.PluginCapability_Service_{
					Service: &csi.PluginCapability_Service{
						Type: csi.PluginCapability_Service_CONTROLLER_SERVICE,
					},
				},
			},
		},
	}, nil
}

// controllerServer exists only so the controller service is registered; this
// driver does no dynamic provisioning (volumes are static PVs). Every RPC we do
// not implement returns Unimplemented via the embedded struct, which is exactly
// what csi-common's default controller did for this driver's capability set
// (RPC_UNKNOWN, i.e. none).
type controllerServer struct {
	csi.UnimplementedControllerServer
}

func (cs *controllerServer) ControllerGetCapabilities(ctx context.Context, req *csi.ControllerGetCapabilitiesRequest) (*csi.ControllerGetCapabilitiesResponse, error) {
	return &csi.ControllerGetCapabilitiesResponse{}, nil
}

func (cs *controllerServer) ValidateVolumeCapabilities(ctx context.Context, req *csi.ValidateVolumeCapabilitiesRequest) (*csi.ValidateVolumeCapabilitiesResponse, error) {
	if len(req.GetVolumeId()) == 0 {
		return nil, status.Error(codes.InvalidArgument, "volume ID must be provided")
	}
	if len(req.GetVolumeCapabilities()) == 0 {
		return nil, status.Error(codes.InvalidArgument, "volume capabilities must be provided")
	}
	for _, c := range req.GetVolumeCapabilities() {
		if c.GetAccessMode() != nil && c.GetAccessMode().GetMode() != csi.VolumeCapability_AccessMode_MULTI_NODE_MULTI_WRITER {
			return &csi.ValidateVolumeCapabilitiesResponse{}, nil
		}
	}
	return &csi.ValidateVolumeCapabilitiesResponse{
		Confirmed: &csi.ValidateVolumeCapabilitiesResponse_Confirmed{
			VolumeCapabilities: req.GetVolumeCapabilities(),
		},
	}, nil
}
