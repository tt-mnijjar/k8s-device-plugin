package integration

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/goshlanguage/k8s-device-plugin/internal/plugin"
	"github.com/goshlanguage/k8s-device-plugin/internal/plugin/mocks"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/stretchr/testify/suite"
	"go.uber.org/mock/gomock"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	pluginapi "k8s.io/kubelet/pkg/apis/deviceplugin/v1beta1"
)

// PluginIntegrationSuite exercises the DevicePlugin through its gRPC API,
// testing the plugin as a whole with no real hardware.
type PluginIntegrationSuite struct {
	suite.Suite

	ctrl   *gomock.Controller
	hc     *mocks.MockHealthChecker
	server *grpc.Server
	conn   *grpc.ClientConn
	client pluginapi.DevicePluginClient
	dp     *plugin.DevicePlugin
	tmpDir string
}

func TestPluginIntegration(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration tests in -short mode")
	}
	suite.Run(t, new(PluginIntegrationSuite))
}

// shortTmpDir creates a short temp dir under /tmp to stay within the
// 104-char macOS Unix socket path limit.
func shortTmpDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "tt")
	require.NoError(t, err)
	return dir
}

func (s *PluginIntegrationSuite) SetupTest() {
	t := s.T()
	s.ctrl = gomock.NewController(t)

	s.hc = mocks.NewMockHealthChecker(s.ctrl)
	s.hc.EXPECT().Check(gomock.Any()).Return(pluginapi.Healthy).AnyTimes()

	devices := []*pluginapi.Device{
		{ID: "dev-0", Health: pluginapi.Healthy},
		{ID: "dev-1", Health: pluginapi.Healthy},
		{ID: "dev-2", Health: pluginapi.Healthy},
	}

	s.tmpDir = shortTmpDir(t)

	s.dp = plugin.NewDevicePlugin("n150", devices,
		plugin.WithHealthChecker(s.hc),
		plugin.WithSocketDir(s.tmpDir),
	)

	sockPath := filepath.Join(s.tmpDir, "int.sock")
	lis, err := net.Listen("unix", sockPath)
	s.Require().NoError(err)

	s.server = grpc.NewServer()
	pluginapi.RegisterDevicePluginServer(s.server, s.dp)
	go s.server.Serve(lis)

	s.conn, err = grpc.NewClient(
		fmt.Sprintf("unix://%s", sockPath),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	s.Require().NoError(err)

	s.client = pluginapi.NewDevicePluginClient(s.conn)
}

func (s *PluginIntegrationSuite) TearDownTest() {
	if s.conn != nil {
		s.conn.Close()
	}
	if s.server != nil {
		s.server.GracefulStop()
	}
	if s.tmpDir != "" {
		os.RemoveAll(s.tmpDir)
	}
	if s.ctrl != nil {
		s.ctrl.Finish()
	}
}

// ---------------------------------------------------------------------------
// Test: Plugin gRPC API — all protocol endpoints served correctly
// ---------------------------------------------------------------------------

func (s *PluginIntegrationSuite) TestGRPCAPI() {
	t := s.T()
	ctx := context.Background()

	// GetDevicePluginOptions
	opts, err := s.client.GetDevicePluginOptions(ctx, &pluginapi.Empty{})
	require.NoError(t, err)
	assert.NotNil(t, opts)

	// ListAndWatch — consume the first message
	streamCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	stream, err := s.client.ListAndWatch(streamCtx, &pluginapi.Empty{})
	require.NoError(t, err)

	resp, err := stream.Recv()
	require.NoError(t, err)
	assert.Len(t, resp.Devices, 3)
	cancel()

	// Allocate
	allocResp, err := s.client.Allocate(ctx, &pluginapi.AllocateRequest{
		ContainerRequests: []*pluginapi.ContainerAllocateRequest{
			{DevicesIds: []string{"dev-0"}},
		},
	})
	require.NoError(t, err)
	require.Len(t, allocResp.ContainerResponses, 1)
	assert.Equal(t, "dev-0", allocResp.ContainerResponses[0].Envs["TT_VISIBLE_DEVICES"])

	// PreStartContainer
	preStart, err := s.client.PreStartContainer(ctx, &pluginapi.PreStartContainerRequest{})
	require.NoError(t, err)
	assert.NotNil(t, preStart)
}

// ---------------------------------------------------------------------------
// Test: Device lifecycle — initial device list via ListAndWatch, then
// health transitions observed after RunStartupHealthChecks
// ---------------------------------------------------------------------------

func (s *PluginIntegrationSuite) TestDeviceLifecycle() {
	t := s.T()

	// Open ListAndWatch stream and read initial device list
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	stream, err := s.client.ListAndWatch(ctx, &pluginapi.Empty{})
	require.NoError(t, err)

	resp, err := stream.Recv()
	require.NoError(t, err)
	require.Len(t, resp.Devices, 3)

	for _, d := range resp.Devices {
		assert.Equal(t, pluginapi.Healthy, d.Health, "device %s", d.ID)
	}
}

// ---------------------------------------------------------------------------
// Test: Allocation across pods — correct distribution and exhaustion handling
// ---------------------------------------------------------------------------

func (s *PluginIntegrationSuite) TestAllocationAcrossPods() {
	t := s.T()
	ctx := context.Background()

	// Pod 1: requests dev-0
	resp1, err := s.client.Allocate(ctx, &pluginapi.AllocateRequest{
		ContainerRequests: []*pluginapi.ContainerAllocateRequest{
			{DevicesIds: []string{"dev-0"}},
		},
	})
	require.NoError(t, err)
	require.Len(t, resp1.ContainerResponses, 1)
	assert.Equal(t, "dev-0", resp1.ContainerResponses[0].Envs["TT_VISIBLE_DEVICES"])

	// Pod 2: requests dev-1 and dev-2
	resp2, err := s.client.Allocate(ctx, &pluginapi.AllocateRequest{
		ContainerRequests: []*pluginapi.ContainerAllocateRequest{
			{DevicesIds: []string{"dev-1", "dev-2"}},
		},
	})
	require.NoError(t, err)
	require.Len(t, resp2.ContainerResponses, 1)
	assert.Equal(t, "dev-1,dev-2", resp2.ContainerResponses[0].Envs["TT_VISIBLE_DEVICES"])
	assert.Len(t, resp2.ContainerResponses[0].Devices, 2)

	// Pod 3: requests a device that doesn't exist → exhausted/NotFound
	_, err = s.client.Allocate(ctx, &pluginapi.AllocateRequest{
		ContainerRequests: []*pluginapi.ContainerAllocateRequest{
			{DevicesIds: []string{"dev-99"}},
		},
	})
	require.Error(t, err)
	assert.Equal(t, codes.NotFound, status.Code(err))
}

// ---------------------------------------------------------------------------
// Test: Health change propagation — unhealthy device excluded from allocation
// and reflected in ListAndWatch
// ---------------------------------------------------------------------------

func (s *PluginIntegrationSuite) TestHealthChangePropagation() {
	t := s.T()

	// Re-create the plugin with a controllable health checker.
	// We need a fresh mock that we can re-program mid-test.
	ctrl := gomock.NewController(t)
	hc := mocks.NewMockHealthChecker(ctrl)

	// Initially all healthy
	hc.EXPECT().Check("dev-0").Return(pluginapi.Healthy)
	hc.EXPECT().Check("dev-1").Return(pluginapi.Unhealthy)
	hc.EXPECT().Check("dev-2").Return(pluginapi.Healthy)

	devices := []*pluginapi.Device{
		{ID: "dev-0", Health: pluginapi.Healthy},
		{ID: "dev-1", Health: pluginapi.Healthy},
		{ID: "dev-2", Health: pluginapi.Healthy},
	}

	tmpDir := shortTmpDir(t)
	defer os.RemoveAll(tmpDir)

	dp := plugin.NewDevicePlugin("n150", devices,
		plugin.WithHealthChecker(hc),
		plugin.WithSocketDir(tmpDir),
	)
	dp.RunStartupHealthChecks()

	// Start a dedicated gRPC server for this test
	sockPath := filepath.Join(tmpDir, "hp.sock")
	lis, err := net.Listen("unix", sockPath)
	require.NoError(t, err)

	srv := grpc.NewServer()
	pluginapi.RegisterDevicePluginServer(srv, dp)
	go srv.Serve(lis)
	defer srv.GracefulStop()

	conn, err := grpc.NewClient(
		fmt.Sprintf("unix://%s", sockPath),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	require.NoError(t, err)
	defer conn.Close()

	client := pluginapi.NewDevicePluginClient(conn)
	ctx := context.Background()

	// Allocate dev-1 should fail — it's unhealthy
	_, err = client.Allocate(ctx, &pluginapi.AllocateRequest{
		ContainerRequests: []*pluginapi.ContainerAllocateRequest{
			{DevicesIds: []string{"dev-1"}},
		},
	})
	require.Error(t, err)
	assert.Equal(t, codes.FailedPrecondition, status.Code(err))

	// Allocate dev-0 should succeed — it's healthy
	resp, err := client.Allocate(ctx, &pluginapi.AllocateRequest{
		ContainerRequests: []*pluginapi.ContainerAllocateRequest{
			{DevicesIds: []string{"dev-0"}},
		},
	})
	require.NoError(t, err)
	assert.Len(t, resp.ContainerResponses, 1)

	// ListAndWatch should show dev-1 as unhealthy
	streamCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	stream, err := client.ListAndWatch(streamCtx, &pluginapi.Empty{})
	require.NoError(t, err)

	lwResp, err := stream.Recv()
	require.NoError(t, err)

	healthByID := make(map[string]string)
	for _, d := range lwResp.Devices {
		healthByID[d.ID] = d.Health
	}
	assert.Equal(t, pluginapi.Healthy, healthByID["dev-0"])
	assert.Equal(t, pluginapi.Unhealthy, healthByID["dev-1"])
	assert.Equal(t, pluginapi.Healthy, healthByID["dev-2"])
}

// ---------------------------------------------------------------------------
// Test: Error recovery — invalid requests and stream reconnection
// ---------------------------------------------------------------------------

func (s *PluginIntegrationSuite) TestErrorRecovery() {
	t := s.T()
	ctx := context.Background()

	// Empty ContainerRequests
	_, err := s.client.Allocate(ctx, &pluginapi.AllocateRequest{
		ContainerRequests: []*pluginapi.ContainerAllocateRequest{},
	})
	require.Error(t, err)
	assert.Equal(t, codes.InvalidArgument, status.Code(err))

	// Empty DevicesIds in a container request
	_, err = s.client.Allocate(ctx, &pluginapi.AllocateRequest{
		ContainerRequests: []*pluginapi.ContainerAllocateRequest{
			{DevicesIds: []string{}},
		},
	})
	require.Error(t, err)
	assert.Equal(t, codes.InvalidArgument, status.Code(err))

	// Unknown device
	_, err = s.client.Allocate(ctx, &pluginapi.AllocateRequest{
		ContainerRequests: []*pluginapi.ContainerAllocateRequest{
			{DevicesIds: []string{"does-not-exist"}},
		},
	})
	require.Error(t, err)
	assert.Equal(t, codes.NotFound, status.Code(err))

	// After errors, valid requests should still succeed (state is consistent)
	resp, err := s.client.Allocate(ctx, &pluginapi.AllocateRequest{
		ContainerRequests: []*pluginapi.ContainerAllocateRequest{
			{DevicesIds: []string{"dev-0"}},
		},
	})
	require.NoError(t, err)
	assert.Len(t, resp.ContainerResponses, 1)

	// Close and reconnect ListAndWatch stream
	streamCtx1, cancel1 := context.WithCancel(ctx)
	stream1, err := s.client.ListAndWatch(streamCtx1, &pluginapi.Empty{})
	require.NoError(t, err)

	_, err = stream1.Recv()
	require.NoError(t, err)
	cancel1()

	// Second stream should work
	streamCtx2, cancel2 := context.WithCancel(ctx)
	defer cancel2()

	stream2, err := s.client.ListAndWatch(streamCtx2, &pluginapi.Empty{})
	require.NoError(t, err)

	resp2, err := stream2.Recv()
	require.NoError(t, err)
	assert.Len(t, resp2.Devices, 3)
}

// ---------------------------------------------------------------------------
// Test: Concurrent allocations — verify thread safety under race detector
// ---------------------------------------------------------------------------

func (s *PluginIntegrationSuite) TestConcurrentAllocations() {
	t := s.T()
	ctx := context.Background()

	const goroutines = 20
	var wg sync.WaitGroup
	errs := make([]error, goroutines)
	resps := make([]*pluginapi.AllocateResponse, goroutines)

	wg.Add(goroutines)
	for i := range goroutines {
		go func(idx int) {
			defer wg.Done()
			devID := fmt.Sprintf("dev-%d", idx%3)
			resps[idx], errs[idx] = s.client.Allocate(ctx, &pluginapi.AllocateRequest{
				ContainerRequests: []*pluginapi.ContainerAllocateRequest{
					{DevicesIds: []string{devID}},
				},
			})
		}(i)
	}
	wg.Wait()

	for i := range goroutines {
		assert.NoError(t, errs[i], "goroutine %d", i)
		if errs[i] == nil {
			assert.Len(t, resps[i].ContainerResponses, 1, "goroutine %d", i)
		}
	}
}

// ---------------------------------------------------------------------------
// Test: Full Start → Register lifecycle with mock kubelet
// ---------------------------------------------------------------------------

func (s *PluginIntegrationSuite) TestStartAndRegister() {
	t := s.T()

	ctrl := gomock.NewController(t)
	hc := mocks.NewMockHealthChecker(ctrl)
	hc.EXPECT().Check(gomock.Any()).Return(pluginapi.Healthy).AnyTimes()

	tmpDir := shortTmpDir(t)
	defer os.RemoveAll(tmpDir)

	devices := []*pluginapi.Device{
		{ID: "dev-0", Health: pluginapi.Healthy},
	}
	dp := plugin.NewDevicePlugin("n150", devices,
		plugin.WithHealthChecker(hc),
		plugin.WithSocketDir(tmpDir),
	)

	// Set up a fake kubelet registration server
	kubeletSock := filepath.Join(tmpDir, "kubelet.sock")
	lis, err := net.Listen("unix", kubeletSock)
	require.NoError(t, err)

	var received *pluginapi.RegisterRequest
	var mu sync.Mutex

	regSrv := grpc.NewServer()
	pluginapi.RegisterRegistrationServer(regSrv, &captureRegistrationServer{
		onRegister: func(req *pluginapi.RegisterRequest) {
			mu.Lock()
			received = req
			mu.Unlock()
		},
	})
	go regSrv.Serve(lis)
	defer regSrv.GracefulStop()

	err = dp.Register(kubeletSock)
	require.NoError(t, err)

	// Allow a moment for the async registration to be captured
	time.Sleep(100 * time.Millisecond)

	mu.Lock()
	defer mu.Unlock()
	require.NotNil(t, received)
	assert.Equal(t, pluginapi.Version, received.Version)
	assert.Equal(t, "tenstorrent.com/n150", received.ResourceName)
	assert.Equal(t, "tenstorrent-n150.sock", received.Endpoint)
}

// captureRegistrationServer is a minimal Registration server that records requests.
type captureRegistrationServer struct {
	pluginapi.UnimplementedRegistrationServer
	onRegister func(*pluginapi.RegisterRequest)
}

func (c *captureRegistrationServer) Register(_ context.Context, req *pluginapi.RegisterRequest) (*pluginapi.Empty, error) {
	if c.onRegister != nil {
		c.onRegister(req)
	}
	return &pluginapi.Empty{}, nil
}
