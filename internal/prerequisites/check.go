package prerequisites

import (
	"fmt"
	"strings"

	"k8s.io/klog/v2"
)

// Severity controls whether a failing check prevents startup.
type Severity int

const (
	// Required checks cause a fatal exit on failure.
	Required Severity = iota
	// Warning checks log but allow startup to continue.
	Warning
)

func (s Severity) String() string {
	switch s {
	case Required:
		return "required"
	case Warning:
		return "warning"
	default:
		return "unknown"
	}
}

// Check represents a single prerequisite validation that must pass
// before the device plugin begins device discovery.
type Check struct {
	Name     string
	Severity Severity
	Run      func() error
}

// RunAll executes every check in order. Required checks that fail are
// collected; if any exist, a combined error is returned. Warning checks
// that fail are logged but do not contribute to the returned error.
func RunAll(checks []Check) error {
	var failures []string

	for _, c := range checks {
		klog.Infof("Prerequisite check: %s", c.Name)
		if err := c.Run(); err != nil {
			switch c.Severity {
			case Required:
				klog.Errorf("  FAIL [%s] %s: %v", c.Severity, c.Name, err)
				failures = append(failures, fmt.Sprintf("%s: %v", c.Name, err))
			case Warning:
				klog.Warningf("  WARN [%s] %s: %v", c.Severity, c.Name, err)
			}
		} else {
			klog.Infof("  PASS %s", c.Name)
		}
	}

	if len(failures) > 0 {
		return fmt.Errorf("required prerequisite checks failed:\n  %s", strings.Join(failures, "\n  "))
	}
	return nil
}
