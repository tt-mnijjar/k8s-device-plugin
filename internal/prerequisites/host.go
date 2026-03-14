package prerequisites

import (
	"fmt"
	"os"
)

// NewDirectoryCheck verifies that path exists and is a directory.
// Use this for host paths that must be present before discovery
// (e.g. /dev/tenstorrent, /sys/class/tenstorrent).
func NewDirectoryCheck(name, path string, severity Severity) Check {
	return Check{
		Name:     name,
		Severity: severity,
		Run: func() error {
			info, err := os.Stat(path)
			if err != nil {
				if os.IsNotExist(err) {
					return fmt.Errorf("directory %s does not exist", path)
				}
				return fmt.Errorf("cannot access %s: %v", path, err)
			}
			if !info.IsDir() {
				return fmt.Errorf("%s exists but is not a directory", path)
			}
			return nil
		},
	}
}

// NewSocketCheck verifies that path exists and is a Unix socket.
// Use this for sockets that must be present before the plugin can
// register (e.g. the kubelet device-plugin socket).
func NewSocketCheck(name, path string, severity Severity) Check {
	return Check{
		Name:     name,
		Severity: severity,
		Run: func() error {
			info, err := os.Stat(path)
			if err != nil {
				if os.IsNotExist(err) {
					return fmt.Errorf("socket %s does not exist", path)
				}
				return fmt.Errorf("cannot access %s: %v", path, err)
			}
			if info.Mode().Type()&os.ModeSocket == 0 {
				return fmt.Errorf("%s exists but is not a Unix socket", path)
			}
			return nil
		},
	}
}
