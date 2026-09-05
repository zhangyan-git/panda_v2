package sync

import (
	"encoding/json"
	"strings"
	"time"

	"github.com/panda-dev/panda-v2/backend/services/coffee-machine-service/internal/model"
)

// DeviceInfo is the provider-neutral device snapshot returned by a manufacturer.
type DeviceInfo struct {
	SerialUnique   string
	DeviceName     string
	Name           string
	SerialNumber   string
	Location       string
	Online         bool
	Status         string
	Version        string
	Address        string
	Error          string
	LastActivityAt time.Time
	DisplayConfig  json.RawMessage
	PaymentConfig  json.RawMessage
}

// DrinkInfo is the provider-neutral drink snapshot returned by a manufacturer.
type DrinkInfo struct {
	OriginID    string
	Name        string
	Description string
	ProductNum  string
	EnName      string
	Price       int64
	Image       string
	Sort        int
	Status      string
}

func (v DeviceInfo) DomainDevice(manufacturer model.Manufacturer) (model.Device, error) {
	serialUnique := strings.TrimSpace(v.SerialUnique)
	name := strings.TrimSpace(v.Name)
	if name == "" {
		name = strings.TrimSpace(v.DeviceName)
	}
	if serialUnique == "" || name == "" || strings.TrimSpace(manufacturer.ID) == "" {
		return model.Device{}, model.ErrInvalid
	}
	status := v.Status
	if status == "" {
		status = model.StatusActive
	}
	if status != model.StatusActive && status != model.StatusDisabled {
		return model.Device{}, model.ErrInvalid
	}
	return model.Device{
		ManufacturerID:   manufacturer.ID,
		Name:             name,
		SerialNumber:     strings.TrimSpace(v.SerialNumber),
		Location:         strings.TrimSpace(v.Location),
		SerialUnique:     serialUnique,
		DeviceName:       strings.TrimSpace(v.DeviceName),
		ManufacturerCode: strings.TrimSpace(manufacturer.Code),
		Online:           v.Online,
		Version:          strings.TrimSpace(v.Version),
		Address:          strings.TrimSpace(v.Address),
		Error:            strings.TrimSpace(v.Error),
		LastActivityAt:   v.LastActivityAt,
		DisplayConfig:    v.DisplayConfig,
		PaymentConfig:    v.PaymentConfig,
		Status:           status,
	}, nil
}

func (v DrinkInfo) DomainDrink() (model.Drink, error) {
	originID := strings.TrimSpace(v.OriginID)
	name := strings.TrimSpace(v.Name)
	if name == "" {
		name = strings.TrimSpace(v.EnName)
	}
	if name == "" {
		name = strings.TrimSpace(v.ProductNum)
	}
	if originID == "" || name == "" || v.Price < 0 {
		return model.Drink{}, model.ErrInvalid
	}
	status := v.Status
	if status == "" {
		status = model.StatusActive
	}
	if status != model.StatusActive && status != model.StatusDisabled {
		return model.Drink{}, model.ErrInvalid
	}
	return model.Drink{
		Name:        name,
		Description: strings.TrimSpace(v.Description),
		OriginID:    originID,
		ProductNum:  strings.TrimSpace(v.ProductNum),
		EnName:      strings.TrimSpace(v.EnName),
		Price:       v.Price,
		Image:       strings.TrimSpace(v.Image),
		Sort:        v.Sort,
		Status:      status,
	}, nil
}
