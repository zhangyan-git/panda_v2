package sync

import (
	"context"

	"github.com/panda-dev/panda-v2/backend/services/coffee-machine-service/internal/model"
)

// Provider retrieves manufacturer-owned device and drink snapshots.
// Implementations must not mutate the management API or expose credentials in errors.
type Provider interface {
	Code() string
	Name() string
	Devices(context.Context, model.Manufacturer) ([]DeviceInfo, error)
	Drinks(context.Context, model.Manufacturer, DeviceInfo) ([]DrinkInfo, error)
}
