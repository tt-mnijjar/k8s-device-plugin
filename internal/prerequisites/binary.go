package prerequisites

import (
	"fmt"
	"os/exec"
)

// NewBinaryCheck verifies that a named executable is available in PATH.
// Use this for health-check tools or runtime utilities that the plugin
// (or future health probes) will shell out to.
//
// Example — require tt-smi for hardware health checks:
//
//	prerequisites.NewBinaryCheck("tt-smi", prerequisites.Required)
func NewBinaryCheck(binary string, severity Severity) Check {
	return Check{
		Name:     fmt.Sprintf("binary-in-path:%s", binary),
		Severity: severity,
		Run: func() error {
			_, err := exec.LookPath(binary)
			if err != nil {
				return fmt.Errorf("%s not found in PATH", binary)
			}
			return nil
		},
	}
}
