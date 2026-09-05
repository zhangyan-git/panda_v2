package sync

import (
	"context"
	"fmt"

	"github.com/panda-dev/panda-v2/backend/services/coffee-machine-service/internal/model"
	"github.com/panda-dev/panda-v2/backend/services/coffee-machine-service/internal/service"
)

// SyncReport describes the successful writes and item-level failures from a sync.
// Device retrieval failures are returned directly because they prevent the sync
// from determining the set of devices to process.
type SyncReport struct {
	Devices   int
	Drinks    int
	Relations int
	Errors    []error
}

// Service synchronizes snapshots from one manufacturer provider into the domain
// service. It deliberately has no transport or scheduling responsibilities.
type Service struct {
	provider Provider
	domain   *service.Service
}

func NewService(provider Provider, svc *service.Service) *Service {
	return &Service{provider: provider, domain: svc}
}

// SyncManufacturer imports all currently available devices and their drinks.
// An invalid snapshot or item-level write failure is recorded and processing
// continues with the remaining items.
func (s *Service) SyncManufacturer(ctx context.Context, manufacturer model.Manufacturer) (SyncReport, error) {
	devices, err := s.provider.Devices(ctx, manufacturer)
	if err != nil {
		return SyncReport{}, err
	}

	report := SyncReport{}
	for _, info := range devices {
		device, err := info.DomainDevice(manufacturer)
		if err != nil {
			report.addError("device", info.SerialUnique, err)
			continue
		}
		device, err = s.domain.UpsertDeviceBySerialUnique(ctx, device)
		if err != nil {
			report.addError("device", info.SerialUnique, err)
			continue
		}
		report.Devices++

		drinks, err := s.provider.Drinks(ctx, manufacturer, info)
		if err != nil {
			report.addError("drinks", device.SerialUnique, err)
			continue
		}
		for _, drinkInfo := range drinks {
			drink, err := drinkInfo.DomainDrink()
			if err != nil {
				report.addError("drink", drinkInfo.OriginID, err)
				continue
			}
			drink, err = s.domain.UpsertDrinkByOriginID(ctx, drink)
			if err != nil {
				report.addError("drink", drinkInfo.OriginID, err)
				continue
			}
			report.Drinks++

			if _, err = s.domain.SetDeviceDrink(ctx, model.DeviceDrink{
				DeviceID: device.ID,
				DrinkID:  drink.ID,
				OriginID: drink.OriginID,
				Enabled:  true,
			}); err != nil {
				report.addError("relation", device.SerialUnique+"/"+drink.OriginID, err)
				continue
			}
			report.Relations++
		}
	}
	return report, nil
}

func (r *SyncReport) addError(kind, identity string, err error) {
	r.Errors = append(r.Errors, fmt.Errorf("sync %s %q: %w", kind, identity, err))
}
