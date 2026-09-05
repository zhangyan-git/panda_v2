package service

import (
	"context"
	"strings"

	"github.com/panda-dev/panda-v2/backend/services/coffee-machine-service/internal/model"
)

type Service struct{ repo model.Repository }

func NewService(r model.Repository) *Service { return &Service{repo: r} }

func validID(id string) bool     { return strings.TrimSpace(id) != "" }
func validName(name string) bool { return strings.TrimSpace(name) != "" }
func validStatus(status string) bool {
	return status == model.StatusActive || status == model.StatusDisabled
}
func valid(id, name string) error {
	if !validID(id) || !validName(name) {
		return model.ErrInvalid
	}
	return nil
}
func (s *Service) CreateManufacturer(c context.Context, v model.Manufacturer) (model.Manufacturer, error) {
	if !validName(v.Name) || (v.Status != "" && !validStatus(v.Status)) {
		return model.Manufacturer{}, model.ErrInvalid
	}
	return s.repo.CreateManufacturer(c, v)
}
func (s *Service) GetManufacturer(c context.Context, id string) (model.Manufacturer, error) {
	if !validID(id) {
		return model.Manufacturer{}, model.ErrInvalid
	}
	return s.repo.GetManufacturer(c, id)
}
func (s *Service) ListManufacturers(c context.Context) ([]model.Manufacturer, error) {
	return s.repo.ListManufacturers(c)
}
func (s *Service) UpdateManufacturer(c context.Context, id string, v model.Manufacturer) (model.Manufacturer, error) {
	if e := valid(id, v.Name); e != nil {
		return model.Manufacturer{}, e
	}
	return s.repo.UpdateManufacturer(c, id, v)
}
func (s *Service) DeleteManufacturer(c context.Context, id string) error {
	if !validID(id) {
		return model.ErrInvalid
	}
	return s.repo.DeleteManufacturer(c, id)
}
func (s *Service) CreateDevice(c context.Context, v model.Device) (model.Device, error) {
	if !validID(v.ManufacturerID) || !validName(v.Name) || (v.Status != "" && !validStatus(v.Status)) {
		return model.Device{}, model.ErrInvalid
	}
	if _, err := s.repo.GetManufacturer(c, v.ManufacturerID); err != nil {
		return model.Device{}, err
	}
	return s.repo.CreateDevice(c, v)
}
func (s *Service) UpsertDeviceBySerialUnique(c context.Context, v model.Device) (model.Device, error) {
	if !validID(v.ManufacturerID) || !validName(v.Name) || !validID(v.SerialUnique) || (v.Status != "" && !validStatus(v.Status)) {
		return model.Device{}, model.ErrInvalid
	}
	if _, err := s.repo.GetManufacturer(c, v.ManufacturerID); err != nil {
		return model.Device{}, err
	}
	return s.repo.UpsertDeviceBySerialUnique(c, v)
}

func (s *Service) GetDevice(c context.Context, id string) (model.Device, error) {
	if !validID(id) {
		return model.Device{}, model.ErrInvalid
	}
	return s.repo.GetDevice(c, id)
}
func (s *Service) ListDevices(c context.Context) ([]model.Device, error) {
	return s.repo.ListDevices(c)
}
func (s *Service) UpdateDevice(c context.Context, id string, v model.Device) (model.Device, error) {
	if !validID(id) || !validName(v.Name) || (v.Status != "" && !validStatus(v.Status)) {
		return model.Device{}, model.ErrInvalid
	}
	if validID(v.ManufacturerID) {
		if _, err := s.repo.GetManufacturer(c, v.ManufacturerID); err != nil {
			return model.Device{}, err
		}
	}
	return s.repo.UpdateDevice(c, id, v)
}
func (s *Service) DeleteDevice(c context.Context, id string) error {
	if !validID(id) {
		return model.ErrInvalid
	}
	return s.repo.DeleteDevice(c, id)
}
func (s *Service) CreateDrink(c context.Context, v model.Drink) (model.Drink, error) {
	if !validName(v.Name) || v.Price < 0 || v.VIPPrice < 0 || v.PickupCodePrice < 0 || (v.Status != "" && !validStatus(v.Status)) {
		return model.Drink{}, model.ErrInvalid
	}
	return s.repo.CreateDrink(c, v)
}
func (s *Service) UpsertDrinkByOriginID(c context.Context, v model.Drink) (model.Drink, error) {
	if !validName(v.Name) || !validID(v.OriginID) || v.Price < 0 || v.VIPPrice < 0 || v.PickupCodePrice < 0 || (v.Status != "" && !validStatus(v.Status)) {
		return model.Drink{}, model.ErrInvalid
	}
	return s.repo.UpsertDrinkByOriginID(c, v)
}

func (s *Service) GetDrink(c context.Context, id string) (model.Drink, error) {
	if !validID(id) {
		return model.Drink{}, model.ErrInvalid
	}
	return s.repo.GetDrink(c, id)
}
func (s *Service) ListDrinks(c context.Context) ([]model.Drink, error) { return s.repo.ListDrinks(c) }
func (s *Service) UpdateDrink(c context.Context, id string, v model.Drink) (model.Drink, error) {
	if !validID(id) || !validName(v.Name) || v.Price < 0 || v.VIPPrice < 0 || v.PickupCodePrice < 0 || (v.Status != "" && !validStatus(v.Status)) {
		return model.Drink{}, model.ErrInvalid
	}
	return s.repo.UpdateDrink(c, id, v)
}
func (s *Service) DeleteDrink(c context.Context, id string) error {
	if !validID(id) {
		return model.ErrInvalid
	}
	return s.repo.DeleteDrink(c, id)
}
func (s *Service) SetDeviceDrink(c context.Context, v model.DeviceDrink) (model.DeviceDrink, error) {
	v.OriginID = strings.TrimSpace(v.OriginID)
	if !validID(v.DeviceID) || !validID(v.DrinkID) {
		return model.DeviceDrink{}, model.ErrInvalid
	}
	if _, err := s.repo.GetDevice(c, v.DeviceID); err != nil {
		return model.DeviceDrink{}, err
	}
	if _, err := s.repo.GetDrink(c, v.DrinkID); err != nil {
		return model.DeviceDrink{}, err
	}
	return s.repo.SetDeviceDrink(c, v)
}
func (s *Service) ListDeviceDrinks(c context.Context, id string) ([]model.DeviceDrink, error) {
	if !validID(id) {
		return nil, model.ErrInvalid
	}
	return s.repo.ListDeviceDrinks(c, id)
}
func (s *Service) DeleteDeviceDrink(c context.Context, d, r string) error {
	if !validID(d) || !validID(r) {
		return model.ErrInvalid
	}
	return s.repo.DeleteDeviceDrink(c, d, r)
}
