package plugin

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"google.golang.org/grpc"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/connectivity"
	"google.golang.org/grpc/credentials/insecure"
	status "google.golang.org/grpc/status"
	"k8s.io/klog/v2"
	pluginapi "k8s.io/kubelet/pkg/apis/deviceplugin/v1beta1"
)

const (
	resourceDomain = "tenstorrent.com"
	socketName     = "tenstorrent.sock"
)

// DevicePlugin should conform to the DevicePluginServer Interface as seen here:
//
//	https://github.com/kubernetes/kubelet/blob/v0.34.3/pkg/apis/deviceplugin/v1beta1/api_grpc.pb.go#L264
//
// Conceptual documentation for device plugins can be found on the kubernetes docs:
//
//	https://kubernetes.kubernetes.io/docs/concepts/extend-kubernetes/compute-storage-net/device-plugins/#device-plugin-implementation
//
// Lastly, the original design doc can be of benefit when conceptualizing the operational flow of a device plugin:
//	https://github.com/kubernetes/design-proposals-archive/blob/main/resource-management/device-plugin.md
type DevicePlugin struct {
	pluginapi.UnimplementedDevicePluginServer

	ctx context.Context
	// devicesMu guards all access to the devices map.
	devicesMu sync.RWMutex
	// devices is the canonical store of discovered tenstorrent devices, keyed by device ID.
	// All reads (ListAndWatch, Allocate) take a read lock; all writes (health checker) take a write lock.
	devices map[string]*pluginapi.Device
	// resourceName represents the card(s) discovered, eg: n150 or n300
	resourceName string
	// socket represents the device plugin socket the kubelet will communicate with
	socket string
	// socketDir is the directory where sockets are created (default: /var/lib/kubelet/device-plugins)
	socketDir string
}

func NewDevicePlugin(resourceName string, devices []*pluginapi.Device) *DevicePlugin {
	store := make(map[string]*pluginapi.Device, len(devices))
	for _, d := range devices {
		store[d.ID] = d
	}

	return &DevicePlugin{
		ctx:          context.Background(),
		devices:      store,
		resourceName: resourceName,
		socket:       socketName,
		socketDir:    pluginapi.DevicePluginPath,
	}
}

// GetDevicePluginOptions returns options to be communicated with Device Manager.
// TODO: Implement
func (dp *DevicePlugin) GetDevicePluginOptions(context.Context, *pluginapi.Empty) (*pluginapi.DevicePluginOptions, error) {
	return &pluginapi.DevicePluginOptions{}, nil
}

// ListAndWatch returns a stream of List of Devices
// Whenever a Device state change or a Device disappears, ListAndWatch
// returns the new list
func (dp *DevicePlugin) ListAndWatch(e *pluginapi.Empty, stream pluginapi.DevicePlugin_ListAndWatchServer) error {
	for {
		snapshot := dp.deviceSnapshot()

		klog.Infof("ListAndWatch: sending %d device(s)", len(snapshot))
		if err := stream.Send(&pluginapi.ListAndWatchResponse{Devices: snapshot}); err != nil {
			return err
		}
		time.Sleep(5 * time.Second)
	}
}

// deviceSnapshot returns a point-in-time copy of every device in the store.
// The returned slice is safe to pass to gRPC; it will not be mutated by the
// health checker or any other writer.
func (dp *DevicePlugin) deviceSnapshot() []*pluginapi.Device {
	dp.devicesMu.RLock()
	defer dp.devicesMu.RUnlock()

	out := make([]*pluginapi.Device, 0, len(dp.devices))
	for _, d := range dp.devices {
		copy := *d
		out = append(out, &copy)
	}
	return out
}

// GetPreferredAllocation returns a preferred set of devices to allocate
// from a list of available ones. The resulting preferred allocation is not
// guaranteed to be the allocation ultimately performed by the
// devicemanager. It is only designed to help the devicemanager make a more
// informed allocation decision when possible.
func (dp *DevicePlugin) GetPreferredAllocation(context.Context, *pluginapi.PreferredAllocationRequest) (*pluginapi.PreferredAllocationResponse, error) {
	return nil, status.Errorf(codes.Unimplemented, "method GetPreferredAllocation not implemented")
}

// Allocate is called during container creation so that the Device
// Plugin can run device specific operations and instruct Kubelet
// of the steps to make the Device available in the container.
//
// For each container request kubelet sends us the device IDs it has already
// reserved. We validate every ID against the store (must exist and be Healthy),
// mount only the specific /dev/tenstorrent/<id> nodes, and set TT_VISIBLE_DEVICES
// to the comma-separated list of IDs for that container.
func (dp *DevicePlugin) Allocate(ctx context.Context, req *pluginapi.AllocateRequest) (*pluginapi.AllocateResponse, error) {
	if len(req.ContainerRequests) == 0 {
		return nil, status.Errorf(codes.InvalidArgument, "Allocate called with empty ContainerRequests")
	}

	dp.devicesMu.RLock()
	defer dp.devicesMu.RUnlock()

	resp := &pluginapi.AllocateResponse{
		ContainerResponses: make([]*pluginapi.ContainerAllocateResponse, 0, len(req.ContainerRequests)),
	}

	for i, cr := range req.ContainerRequests {
		if len(cr.DevicesIds) == 0 {
			return nil, status.Errorf(codes.InvalidArgument, "ContainerRequests[%d] has empty DevicesIds", i)
		}

		devSpecs := make([]*pluginapi.DeviceSpec, 0, len(cr.DevicesIds))
		for _, id := range cr.DevicesIds {
			dev, ok := dp.devices[id]
			if !ok {
				return nil, status.Errorf(codes.NotFound, "unknown device %q", id)
			}
			if dev.Health != pluginapi.Healthy {
				return nil, status.Errorf(codes.FailedPrecondition, "device %q is %s", id, dev.Health)
			}

			devPath := fmt.Sprintf("/dev/tenstorrent/%s", id)
			devSpecs = append(devSpecs, &pluginapi.DeviceSpec{
				HostPath:      devPath,
				ContainerPath: devPath,
				Permissions:   "rw",
			})
		}

		resp.ContainerResponses = append(resp.ContainerResponses, &pluginapi.ContainerAllocateResponse{
			Envs: map[string]string{
				"TT_VISIBLE_DEVICES": strings.Join(cr.DevicesIds, ","),
			},
			Devices: devSpecs,
		})
	}

	return resp, nil
}

// PreStartContainer is called, if indicated by Device Plugin during registration phase,
// before each container start. Device plugin can run device specific operations
// such as resetting the device before making devices available to the container.
func (dp *DevicePlugin) PreStartContainer(context.Context, *pluginapi.PreStartContainerRequest) (*pluginapi.PreStartContainerResponse, error) {
	return &pluginapi.PreStartContainerResponse{}, nil
}

// checkDeviceHealth performs a non-disruptive health probe for a single device
// by verifying its sysfs entry still exists. Heavier diagnostics (e.g. tt-smi)
// are intentionally omitted here because they disrupt running workloads.
//
// Health-check contract (ROADMAP 3.1):
//
//	path: /sys/class/tenstorrent/tenstorrent!<device_id>/tt_card_type
//	present  → Healthy
//	absent   → Unhealthy
func checkDeviceHealth(deviceID string) string {
	sysfsPath := fmt.Sprintf("/sys/class/tenstorrent/tenstorrent!%s/tt_card_type", deviceID)
	if _, err := os.Stat(sysfsPath); err != nil {
		return pluginapi.Unhealthy
	}
	return pluginapi.Healthy
}

// RunStartupHealthChecks evaluates every device in the store exactly once and
// updates its Health field. This is intended to be called synchronously during
// plugin startup, before the gRPC server begins serving, so there are no
// concurrent readers and no risk of disrupting running workloads.
//
// Heavier or disruptive diagnostics (e.g. tt-smi) can safely be used here
// because no pods have been scheduled yet.
func (dp *DevicePlugin) RunStartupHealthChecks() {
	dp.devicesMu.Lock()
	defer dp.devicesMu.Unlock()

	for id, dev := range dp.devices {
		dev.Health = checkDeviceHealth(id)
		klog.Infof("Startup health check: device %s → %s", id, dev.Health)
	}
}

// Start initiates the gRPC server for the device plugin
func (dp *DevicePlugin) Start() error {
	dp.RunStartupHealthChecks()

	fullSocketPath := filepath.Join(dp.socketDir, dp.socket)
  
  // Clean up
  os.Remove(fullSocketPath)

	// Start gRPC server
	sock, err := net.Listen("unix", fullSocketPath)
	if err != nil {
		return fmt.Errorf("failed to listen on socket: %v", err)
	}

	klog.Infof("gRPC server socket established at %s", fullSocketPath)

	grpcServer := grpc.NewServer()
	pluginapi.RegisterDevicePluginServer(grpcServer, dp)

	go func() {
		if err := grpcServer.Serve(sock); err != nil {
			klog.Fatalf("gRPC Serve failed: %v", err)
		}
	}()

	// dummy wait
	//    hoping that the grpc server has acquired the lock in this time before kubelet calls back
	time.Sleep(1 * time.Second)

	return dp.Register(pluginapi.KubeletSocket)
}

func (dp *DevicePlugin) Register(kubeletEndpoint string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5 * time.Second)
	defer cancel()

	conn, err := dp.dial(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()

	client := pluginapi.NewRegistrationClient(conn)

	req := &pluginapi.RegisterRequest{
		Version:      pluginapi.Version,
		Endpoint:     dp.socket,
		ResourceName: fmt.Sprintf("%s/n150", resourceDomain),
	}

	klog.Infof("Registering with kubelet on endpoint %s", req.Endpoint)
	klog.Infof("Registering resource %s", req.ResourceName)
	klog.Infof("Registering with device plugin API version %s", req.Version)

	_, err = client.Register(context.Background(), req)
	if err != nil {
		return fmt.Errorf("failed to register with kubelet: %v", err)
	}

	return nil
}

// dial is a helper function that establishes gRPC communication with the kubelet
func (dp *DevicePlugin) dial(ctx context.Context) (*grpc.ClientConn, error) {
	kubeletSocketEndpoint := fmt.Sprintf("unix://%s", pluginapi.KubeletSocket)
	
	klog.Infof("Dialing kubelet socket: %s", kubeletSocketEndpoint)

	conn, err := grpc.NewClient(
		kubeletSocketEndpoint,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		return nil, err
	}

	// Attempt to connect
	conn.Connect()

	// Explicitly block until READY or context deadline
	for {
		state := conn.GetState()
		if state == connectivity.Ready {
			break
		}

		if !conn.WaitForStateChange(ctx, state) {
			return nil, fmt.Errorf("gRPC connection timeout, last state: %s", state)
		}
	}

	klog.Infof("grpc connection created with endpoint %s", kubeletSocketEndpoint)
	klog.Infof("grpc state %s", conn.GetState().String())

	return conn, nil
}
