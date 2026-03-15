package main

import (
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/goshlanguage/k8s-device-plugin/internal/plugin"
	"github.com/goshlanguage/k8s-device-plugin/internal/prerequisites"
	"k8s.io/klog/v2"
	pluginapi "k8s.io/kubelet/pkg/apis/deviceplugin/v1beta1"
)

func main() {
	runPrerequisitesChecks()
	discoverAndStartPlugins()

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	<-sig
}

func runPrerequisitesChecks() {
	checks := []prerequisites.Check{
		// Downgraded to Warning: nodes without Tenstorrent cards will not have
		// these paths, and the DaemonSet runs on every node. Missing paths mean
		// no devices will be discovered; the plugin will idle rather than crash.
		prerequisites.NewDirectoryCheck(
			"tt-kmd-device-nodes",
			"/dev/tenstorrent",
			prerequisites.Warning,
		),
		prerequisites.NewDirectoryCheck(
			"tt-kmd-sysfs",
			"/sys/class/tenstorrent",
			prerequisites.Warning,
		),
		prerequisites.NewSocketCheck(
			"kubelet-device-plugin-socket",
			pluginapi.KubeletSocket,
			prerequisites.Required,
		),
		// Add future health-check tool requirements here, e.g.:
		// prerequisites.NewBinaryCheck("tt-smi", prerequisites.Required),
	}

	if err := prerequisites.RunAll(checks); err != nil {
		klog.Fatalf("Prerequisite checks failed: %v", err)
	}
}

// discoverAndStartPlugins iterates through the device nodes to discover installed cards.
// The driver is expected to expose tt_card_type for discovering the resource name.
// If /dev/tenstorrent does not exist or is empty (e.g. node has no Tenstorrent cards),
// this function logs and returns without starting any plugin.
func discoverAndStartPlugins() {
	devices := make(map[string][]*pluginapi.Device)

	files, err := filepath.Glob("/dev/tenstorrent/*")
	if err != nil {
		klog.Fatalf("Could not glob /dev/tenstorrent: %v", err)
	}

	for _, devPath := range files {
		deviceID := filepath.Base(devPath)
		cardTypePath := fmt.Sprintf("/sys/class/tenstorrent/tenstorrent!%s/tt_card_type", deviceID)

		cardTypeBytes, err := os.ReadFile(cardTypePath)
		if err != nil {
			klog.Errorf("Failed to read card type from %s, skipping device %s: %v", cardTypePath, deviceID, err)
			continue
		}

		resourceName := strings.TrimSpace(string(cardTypeBytes))
		// FIXME: validate resourceName value ('unknown' means it isn't currently supported)
		devices[resourceName] = append(devices[resourceName], &pluginapi.Device{
			ID:     deviceID,
			Health: pluginapi.Healthy,
		})
	}

	if len(devices) == 0 {
		klog.Info("No Tenstorrent devices discovered on this node; no device plugins will be started")
		return
	}

	klog.Infof("Discovered %d resource type(s): %v", len(devices), devices)

	for resourceName, devs := range devices {
		dp := plugin.NewDevicePlugin(resourceName, devs)

		go func(dp *plugin.DevicePlugin) {
			klog.Infof("Starting device plugin for resource %s", resourceName)

			if err := dp.Start(); err != nil {
				klog.Fatalf("Error starting device plugin: %v", err)
			}
		}(dp)
	}
}
