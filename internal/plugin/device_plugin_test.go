package plugin_test

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/goshlanguage/k8s-device-plugin/internal/plugin"
	"github.com/goshlanguage/k8s-device-plugin/internal/plugin/mocks"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/mock/gomock"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	pluginapi "k8s.io/kubelet/pkg/apis/deviceplugin/v1beta1"
)

// shortTempDir creates a short temporary directory under /tmp to stay within
// the 104-char Unix socket path limit on macOS.
func shortTempDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "tt")
	require.NoError(t, err)
	t.Cleanup(func() { os.RemoveAll(dir) })
	return dir
}

// startTestServer creates a gRPC server on a temporary Unix socket serving the
// given DevicePlugin, and returns a connected DevicePluginClient. The server
// and connection are cleaned up when the test finishes.
func startTestServer(t *testing.T, dp pluginapi.DevicePluginServer) pluginapi.DevicePluginClient {
	t.Helper()

	sockPath := filepath.Join(shortTempDir(t), "t.sock")
	lis, err := net.Listen("unix", sockPath)
	require.NoError(t, err)

	srv := grpc.NewServer()
	pluginapi.RegisterDevicePluginServer(srv, dp)

	go srv.Serve(lis)
	t.Cleanup(func() { srv.GracefulStop() })

	conn, err := grpc.NewClient(
		fmt.Sprintf("unix://%s", sockPath),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	require.NoError(t, err)
	t.Cleanup(func() { conn.Close() })

	return pluginapi.NewDevicePluginClient(conn)
}

// fakeDevices returns n healthy pluginapi.Device structs with IDs "dev-0", "dev-1", ...
func fakeDevices(n int) []*pluginapi.Device {
	devs := make([]*pluginapi.Device, n)
	for i := range n {
		devs[i] = &pluginapi.Device{
			ID:     fmt.Sprintf("dev-%d", i),
			Health: pluginapi.Healthy,
		}
	}
	return devs
}

// alwaysHealthyChecker returns a mock HealthChecker that returns Healthy for any device.
func alwaysHealthyChecker(ctrl *gomock.Controller) *mocks.MockHealthChecker {
	hc := mocks.NewMockHealthChecker(ctrl)
	hc.EXPECT().Check(gomock.Any()).Return(pluginapi.Healthy).AnyTimes()
	return hc
}

// ---------------------------------------------------------------------------
// Device Discovery Tests
// ---------------------------------------------------------------------------

func TestDeviceDiscovery(t *testing.T) {
	t.Run("initializes with expected devices via ListAndWatch", func(t *testing.T) {
		ctrl := gomock.NewController(t)
		devices := fakeDevices(3)
		dp := plugin.NewDevicePlugin("n150", devices, plugin.WithHealthChecker(alwaysHealthyChecker(ctrl)))

		client := startTestServer(t, dp)

		ctx, cancel := context.WithCancel(context.Background())
		t.Cleanup(cancel)

		stream, err := client.ListAndWatch(ctx, &pluginapi.Empty{})
		require.NoError(t, err)

		resp, err := stream.Recv()
		require.NoError(t, err)
		require.Len(t, resp.Devices, 3)

		ids := make(map[string]string, len(resp.Devices))
		for _, d := range resp.Devices {
			ids[d.ID] = d.Health
		}
		for _, d := range devices {
			assert.Equal(t, pluginapi.Healthy, ids[d.ID], "device %s should be healthy", d.ID)
		}
	})

	t.Run("empty device list streams zero devices", func(t *testing.T) {
		ctrl := gomock.NewController(t)
		dp := plugin.NewDevicePlugin("n150", nil, plugin.WithHealthChecker(alwaysHealthyChecker(ctrl)))

		client := startTestServer(t, dp)

		ctx, cancel := context.WithCancel(context.Background())
		t.Cleanup(cancel)

		stream, err := client.ListAndWatch(ctx, &pluginapi.Empty{})
		require.NoError(t, err)

		resp, err := stream.Recv()
		require.NoError(t, err)
		assert.Empty(t, resp.Devices)
	})
}

// ---------------------------------------------------------------------------
// Allocation Logic Tests
// ---------------------------------------------------------------------------

func TestAllocate(t *testing.T) {
	ctrl := gomock.NewController(t)
	devices := fakeDevices(3)
	dp := plugin.NewDevicePlugin("n150", devices, plugin.WithHealthChecker(alwaysHealthyChecker(ctrl)))

	t.Run("valid single container request", func(t *testing.T) {
		resp, err := dp.Allocate(context.Background(), &pluginapi.AllocateRequest{
			ContainerRequests: []*pluginapi.ContainerAllocateRequest{
				{DevicesIds: []string{"dev-0"}},
			},
		})
		require.NoError(t, err)
		require.Len(t, resp.ContainerResponses, 1)

		cr := resp.ContainerResponses[0]
		assert.Equal(t, "dev-0", cr.Envs["TT_VISIBLE_DEVICES"])
		require.Len(t, cr.Devices, 1)
		assert.Equal(t, "/dev/tenstorrent/dev-0", cr.Devices[0].HostPath)
		assert.Equal(t, "/dev/tenstorrent/dev-0", cr.Devices[0].ContainerPath)
		assert.Equal(t, "rw", cr.Devices[0].Permissions)
	})

	t.Run("valid multi-container request", func(t *testing.T) {
		resp, err := dp.Allocate(context.Background(), &pluginapi.AllocateRequest{
			ContainerRequests: []*pluginapi.ContainerAllocateRequest{
				{DevicesIds: []string{"dev-0"}},
				{DevicesIds: []string{"dev-1", "dev-2"}},
			},
		})
		require.NoError(t, err)
		require.Len(t, resp.ContainerResponses, 2)

		assert.Equal(t, "dev-0", resp.ContainerResponses[0].Envs["TT_VISIBLE_DEVICES"])
		assert.Equal(t, "dev-1,dev-2", resp.ContainerResponses[1].Envs["TT_VISIBLE_DEVICES"])
		assert.Len(t, resp.ContainerResponses[1].Devices, 2)
	})

	t.Run("empty ContainerRequests returns InvalidArgument", func(t *testing.T) {
		_, err := dp.Allocate(context.Background(), &pluginapi.AllocateRequest{
			ContainerRequests: []*pluginapi.ContainerAllocateRequest{},
		})
		require.Error(t, err)
		assert.Equal(t, codes.InvalidArgument, status.Code(err))
	})

	t.Run("empty DevicesIds returns InvalidArgument", func(t *testing.T) {
		_, err := dp.Allocate(context.Background(), &pluginapi.AllocateRequest{
			ContainerRequests: []*pluginapi.ContainerAllocateRequest{
				{DevicesIds: []string{}},
			},
		})
		require.Error(t, err)
		assert.Equal(t, codes.InvalidArgument, status.Code(err))
	})

	t.Run("unknown device ID returns NotFound", func(t *testing.T) {
		_, err := dp.Allocate(context.Background(), &pluginapi.AllocateRequest{
			ContainerRequests: []*pluginapi.ContainerAllocateRequest{
				{DevicesIds: []string{"nonexistent-42"}},
			},
		})
		require.Error(t, err)
		assert.Equal(t, codes.NotFound, status.Code(err))
	})

	t.Run("unhealthy device returns FailedPrecondition", func(t *testing.T) {
		unhealthy := []*pluginapi.Device{
			{ID: "sick-0", Health: pluginapi.Unhealthy},
		}
		dpU := plugin.NewDevicePlugin("n150", unhealthy,
			plugin.WithHealthChecker(alwaysHealthyChecker(ctrl)))

		_, err := dpU.Allocate(context.Background(), &pluginapi.AllocateRequest{
			ContainerRequests: []*pluginapi.ContainerAllocateRequest{
				{DevicesIds: []string{"sick-0"}},
			},
		})
		require.Error(t, err)
		assert.Equal(t, codes.FailedPrecondition, status.Code(err))
	})

	t.Run("concurrent allocations are safe", func(t *testing.T) {
		var wg sync.WaitGroup
		errs := make([]error, 10)
		for i := range 10 {
			wg.Add(1)
			go func(idx int) {
				defer wg.Done()
				_, errs[idx] = dp.Allocate(context.Background(), &pluginapi.AllocateRequest{
					ContainerRequests: []*pluginapi.ContainerAllocateRequest{
						{DevicesIds: []string{"dev-0"}},
					},
				})
			}(i)
		}
		wg.Wait()
		for i, err := range errs {
			assert.NoError(t, err, "concurrent allocation %d should succeed", i)
		}
	})
}

// ---------------------------------------------------------------------------
// Health Check Tests
// ---------------------------------------------------------------------------

func TestHealthCheck(t *testing.T) {
	t.Run("startup health check marks devices via injected checker", func(t *testing.T) {
		ctrl := gomock.NewController(t)
		hc := mocks.NewMockHealthChecker(ctrl)
		hc.EXPECT().Check("dev-0").Return(pluginapi.Healthy)
		hc.EXPECT().Check("dev-1").Return(pluginapi.Unhealthy)

		devices := []*pluginapi.Device{
			{ID: "dev-0", Health: pluginapi.Healthy},
			{ID: "dev-1", Health: pluginapi.Healthy},
		}
		dp := plugin.NewDevicePlugin("n150", devices, plugin.WithHealthChecker(hc))
		dp.RunStartupHealthChecks()

		// dev-0 should still be allocatable
		_, err := dp.Allocate(context.Background(), &pluginapi.AllocateRequest{
			ContainerRequests: []*pluginapi.ContainerAllocateRequest{
				{DevicesIds: []string{"dev-0"}},
			},
		})
		assert.NoError(t, err)

		// dev-1 should now be unhealthy
		_, err = dp.Allocate(context.Background(), &pluginapi.AllocateRequest{
			ContainerRequests: []*pluginapi.ContainerAllocateRequest{
				{DevicesIds: []string{"dev-1"}},
			},
		})
		require.Error(t, err)
		assert.Equal(t, codes.FailedPrecondition, status.Code(err))
	})

	t.Run("health transitions healthy to unhealthy to healthy", func(t *testing.T) {
		ctrl := gomock.NewController(t)
		hc := mocks.NewMockHealthChecker(ctrl)

		devices := []*pluginapi.Device{
			{ID: "dev-0", Health: pluginapi.Healthy},
		}

		// Phase 1: healthy
		hc.EXPECT().Check("dev-0").Return(pluginapi.Healthy)
		dp := plugin.NewDevicePlugin("n150", devices, plugin.WithHealthChecker(hc))
		dp.RunStartupHealthChecks()

		_, err := dp.Allocate(context.Background(), &pluginapi.AllocateRequest{
			ContainerRequests: []*pluginapi.ContainerAllocateRequest{
				{DevicesIds: []string{"dev-0"}},
			},
		})
		assert.NoError(t, err)

		// Phase 2: unhealthy
		hc.EXPECT().Check("dev-0").Return(pluginapi.Unhealthy)
		dp.RunStartupHealthChecks()

		_, err = dp.Allocate(context.Background(), &pluginapi.AllocateRequest{
			ContainerRequests: []*pluginapi.ContainerAllocateRequest{
				{DevicesIds: []string{"dev-0"}},
			},
		})
		require.Error(t, err)
		assert.Equal(t, codes.FailedPrecondition, status.Code(err))

		// Phase 3: back to healthy
		hc.EXPECT().Check("dev-0").Return(pluginapi.Healthy)
		dp.RunStartupHealthChecks()

		_, err = dp.Allocate(context.Background(), &pluginapi.AllocateRequest{
			ContainerRequests: []*pluginapi.ContainerAllocateRequest{
				{DevicesIds: []string{"dev-0"}},
			},
		})
		assert.NoError(t, err)
	})

	t.Run("health check reflected in ListAndWatch stream", func(t *testing.T) {
		ctrl := gomock.NewController(t)
		hc := mocks.NewMockHealthChecker(ctrl)
		hc.EXPECT().Check("dev-0").Return(pluginapi.Unhealthy)

		devices := []*pluginapi.Device{
			{ID: "dev-0", Health: pluginapi.Healthy},
		}
		dp := plugin.NewDevicePlugin("n150", devices, plugin.WithHealthChecker(hc))
		dp.RunStartupHealthChecks()

		client := startTestServer(t, dp)
		ctx, cancel := context.WithCancel(context.Background())
		t.Cleanup(cancel)

		stream, err := client.ListAndWatch(ctx, &pluginapi.Empty{})
		require.NoError(t, err)

		resp, err := stream.Recv()
		require.NoError(t, err)
		require.Len(t, resp.Devices, 1)
		assert.Equal(t, pluginapi.Unhealthy, resp.Devices[0].Health)
	})
}

// ---------------------------------------------------------------------------
// Registration Handling Tests
// ---------------------------------------------------------------------------

// fakeRegistrationServer records the RegisterRequest it receives.
type fakeRegistrationServer struct {
	pluginapi.UnimplementedRegistrationServer
	received *pluginapi.RegisterRequest
	mu       sync.Mutex
}

func (f *fakeRegistrationServer) Register(_ context.Context, req *pluginapi.RegisterRequest) (*pluginapi.Empty, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.received = req
	return &pluginapi.Empty{}, nil
}

func (f *fakeRegistrationServer) getReceived() *pluginapi.RegisterRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.received
}

func TestRegistration(t *testing.T) {
	t.Run("register sends correct request to kubelet", func(t *testing.T) {
		ctrl := gomock.NewController(t)

		kubeletSock := filepath.Join(shortTempDir(t), "k.sock")
		lis, err := net.Listen("unix", kubeletSock)
		require.NoError(t, err)

		fakeSrv := &fakeRegistrationServer{}
		grpcSrv := grpc.NewServer()
		pluginapi.RegisterRegistrationServer(grpcSrv, fakeSrv)

		go grpcSrv.Serve(lis)
		t.Cleanup(func() { grpcSrv.GracefulStop() })

		devices := fakeDevices(2)
		dp := plugin.NewDevicePlugin("n150", devices,
			plugin.WithHealthChecker(alwaysHealthyChecker(ctrl)),
		)

		err = dp.Register(kubeletSock)
		require.NoError(t, err)

		req := fakeSrv.getReceived()
		require.NotNil(t, req)
		assert.Equal(t, pluginapi.Version, req.Version)
		assert.Equal(t, "tenstorrent-n150.sock", req.Endpoint)
		assert.Equal(t, "tenstorrent.com/n150", req.ResourceName)
	})

	t.Run("register returns error for unreachable kubelet", func(t *testing.T) {
		ctrl := gomock.NewController(t)
		dp := plugin.NewDevicePlugin("n150", fakeDevices(1),
			plugin.WithHealthChecker(alwaysHealthyChecker(ctrl)),
		)

		err := dp.Register(filepath.Join(shortTempDir(t), "x.sock"))
		assert.Error(t, err)
	})
}

// ---------------------------------------------------------------------------
// GetDevicePluginOptions / GetPreferredAllocation / PreStartContainer Tests
// ---------------------------------------------------------------------------

func TestGetDevicePluginOptions(t *testing.T) {
	ctrl := gomock.NewController(t)
	dp := plugin.NewDevicePlugin("n150", fakeDevices(1),
		plugin.WithHealthChecker(alwaysHealthyChecker(ctrl)))

	opts, err := dp.GetDevicePluginOptions(context.Background(), &pluginapi.Empty{})
	require.NoError(t, err)
	assert.NotNil(t, opts)
}

func TestGetPreferredAllocation(t *testing.T) {
	ctrl := gomock.NewController(t)
	dp := plugin.NewDevicePlugin("n150", fakeDevices(1),
		plugin.WithHealthChecker(alwaysHealthyChecker(ctrl)))

	_, err := dp.GetPreferredAllocation(context.Background(), &pluginapi.PreferredAllocationRequest{})
	require.Error(t, err)
	assert.Equal(t, codes.Unimplemented, status.Code(err))
}

func TestPreStartContainer(t *testing.T) {
	ctrl := gomock.NewController(t)
	dp := plugin.NewDevicePlugin("n150", fakeDevices(1),
		plugin.WithHealthChecker(alwaysHealthyChecker(ctrl)))

	resp, err := dp.PreStartContainer(context.Background(), &pluginapi.PreStartContainerRequest{})
	require.NoError(t, err)
	assert.NotNil(t, resp)
}
