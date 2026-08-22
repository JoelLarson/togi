package harness

import (
	"fmt"
	"sync"
)

const (
	linuxPlatformTag     = "@linux"
	unsupportedHostTag   = "@unsupported-host"
	simulatedPlatformTag = "@simulated-platform"
)

var selectedDrivers struct {
	sync.RWMutex
	factories []DriverFactory
}

func selectDriverNames(value string) ([]string, error) {
	switch value {
	case "", "service":
		return []string{"service"}, nil
	case "cli":
		return []string{"cli"}, nil
	case "all":
		return []string{"service", "cli"}, nil
	default:
		return nil, fmt.Errorf("unknown acceptance driver %q (want service, cli, or all)", value)
	}
}

func includesDriver(names []string, name string) bool {
	for _, candidate := range names {
		if candidate == name {
			return true
		}
	}
	return false
}

func setSelectedFactories(factories []DriverFactory) {
	selectedDrivers.Lock()
	defer selectedDrivers.Unlock()
	selectedDrivers.factories = append([]DriverFactory(nil), factories...)
}

func selectedFactories() []DriverFactory {
	selectedDrivers.RLock()
	defer selectedDrivers.RUnlock()
	return append([]DriverFactory(nil), selectedDrivers.factories...)
}
