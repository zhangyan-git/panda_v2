package repository

import (
	"context"
	"sort"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/panda-dev/panda-v2/backend/services/coffee-machine-service/internal/model"
)

type Memory struct {
	mu            sync.RWMutex
	manufacturers map[string]model.Manufacturer
	devices       map[string]model.Device
	drinks        map[string]model.Drink
	relations     map[string]model.DeviceDrink
}

func NewMemory() *Memory {
	return &Memory{manufacturers: map[string]model.Manufacturer{}, devices: map[string]model.Device{}, drinks: map[string]model.Drink{}, relations: map[string]model.DeviceDrink{}}
}
func id(s string) string {
	if s == "" {
		return uuid.NewString()
	}
	return s
}
func createdUpdated(created, updated time.Time) (time.Time, time.Time) {
	now := time.Now().UTC()
	if created.IsZero() {
		created = now
	}
	return created, now
}
func (r *Memory) CreateManufacturer(_ context.Context, v model.Manufacturer) (model.Manufacturer, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	v.ID = id(v.ID)
	v.CreatedAt, v.UpdatedAt = createdUpdated(v.CreatedAt, v.UpdatedAt)
	r.manufacturers[v.ID] = v
	return v, nil
}
func (r *Memory) GetManufacturer(_ context.Context, i string) (model.Manufacturer, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	v, ok := r.manufacturers[i]
	if !ok {
		return v, model.ErrNotFound
	}
	return v, nil
}
func (r *Memory) ListManufacturers(context.Context) ([]model.Manufacturer, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	o := make([]model.Manufacturer, 0, len(r.manufacturers))
	for _, v := range r.manufacturers {
		o = append(o, v)
	}
	sort.Slice(o, func(i, j int) bool { return o[i].ID < o[j].ID })
	return o, nil
}
func (r *Memory) UpdateManufacturer(_ context.Context, i string, v model.Manufacturer) (model.Manufacturer, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	old, ok := r.manufacturers[i]
	if !ok {
		return v, model.ErrNotFound
	}
	v.ID = i
	v.CreatedAt, v.UpdatedAt = old.CreatedAt, time.Now().UTC()
	r.manufacturers[i] = v
	return v, nil
}
func (r *Memory) DeleteManufacturer(_ context.Context, i string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.manufacturers[i]; !ok {
		return model.ErrNotFound
	}
	for _, v := range r.devices {
		if v.ManufacturerID == i {
			return model.ErrInvalid
		}
	}
	delete(r.manufacturers, i)
	return nil
}
func (r *Memory) CreateDevice(_ context.Context, v model.Device) (model.Device, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.manufacturers[v.ManufacturerID]; !ok {
		return v, model.ErrNotFound
	}
	v.ID = id(v.ID)
	v.CreatedAt, v.UpdatedAt = createdUpdated(v.CreatedAt, v.UpdatedAt)
	if v.Status == "" {
		v.Status = model.StatusActive
	}
	r.devices[v.ID] = v
	return v, nil
}
func (r *Memory) UpsertDeviceBySerialUnique(_ context.Context, v model.Device) (model.Device, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.manufacturers[v.ManufacturerID]; !ok {
		return v, model.ErrNotFound
	}
	var existingID string
	for k, existing := range r.devices {
		if existing.SerialUnique == v.SerialUnique {
			existingID = k
			v.CreatedAt = existing.CreatedAt
			break
		}
	}
	v.ID = existingID
	v.ID = id(v.ID)
	v.CreatedAt, v.UpdatedAt = createdUpdated(v.CreatedAt, v.UpdatedAt)
	if v.Status == "" {
		v.Status = model.StatusActive
	}
	r.devices[v.ID] = v
	return v, nil
}

func (r *Memory) GetDevice(_ context.Context, i string) (model.Device, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	v, ok := r.devices[i]
	if !ok {
		return v, model.ErrNotFound
	}
	return v, nil
}
func (r *Memory) ListDevices(context.Context) ([]model.Device, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	o := make([]model.Device, 0, len(r.devices))
	for _, v := range r.devices {
		o = append(o, v)
	}
	sort.Slice(o, func(i, j int) bool { return o[i].ID < o[j].ID })
	return o, nil
}
func (r *Memory) UpdateDevice(_ context.Context, i string, v model.Device) (model.Device, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	old, ok := r.devices[i]
	if !ok {
		return v, model.ErrNotFound
	}
	if v.ManufacturerID == "" {
		v.ManufacturerID = old.ManufacturerID
	}
	if _, ok := r.manufacturers[v.ManufacturerID]; !ok {
		return v, model.ErrNotFound
	}
	if v.Status == "" {
		v.Status = old.Status
	}
	v.ID = i
	v.CreatedAt, v.UpdatedAt = old.CreatedAt, time.Now().UTC()
	r.devices[i] = v
	return v, nil
}
func (r *Memory) DeleteDevice(_ context.Context, i string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.devices[i]; !ok {
		return model.ErrNotFound
	}
	delete(r.devices, i)
	for k, v := range r.relations {
		if v.DeviceID == i {
			delete(r.relations, k)
		}
	}
	return nil
}
func (r *Memory) CreateDrink(_ context.Context, v model.Drink) (model.Drink, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	v.ID = id(v.ID)
	v.CreatedAt, v.UpdatedAt = createdUpdated(v.CreatedAt, v.UpdatedAt)
	if v.Status == "" {
		v.Status = model.StatusActive
	}
	r.drinks[v.ID] = v
	return v, nil
}
func (r *Memory) UpsertDrinkByOriginID(_ context.Context, v model.Drink) (model.Drink, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var existingID string
	for k, existing := range r.drinks {
		if existing.OriginID == v.OriginID {
			existingID = k
			v.CreatedAt = existing.CreatedAt
			v.VIPPrice = existing.VIPPrice
			v.PickupCodePrice = existing.PickupCodePrice
			break
		}
	}
	v.ID = id(existingID)
	v.CreatedAt, v.UpdatedAt = createdUpdated(v.CreatedAt, v.UpdatedAt)
	if v.Status == "" {
		v.Status = model.StatusActive
	}
	r.drinks[v.ID] = v
	return v, nil
}

func (r *Memory) GetDrink(_ context.Context, i string) (model.Drink, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	v, ok := r.drinks[i]
	if !ok {
		return v, model.ErrNotFound
	}
	return v, nil
}
func (r *Memory) ListDrinks(context.Context) ([]model.Drink, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	o := make([]model.Drink, 0, len(r.drinks))
	for _, v := range r.drinks {
		o = append(o, v)
	}
	sort.Slice(o, func(i, j int) bool { return o[i].ID < o[j].ID })
	return o, nil
}
func (r *Memory) UpdateDrink(_ context.Context, i string, v model.Drink) (model.Drink, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	old, ok := r.drinks[i]
	if !ok {
		return v, model.ErrNotFound
	}
	if v.Status == "" {
		v.Status = old.Status
	}
	v.ID = i
	v.CreatedAt, v.UpdatedAt = old.CreatedAt, time.Now().UTC()
	r.drinks[i] = v
	return v, nil
}
func (r *Memory) DeleteDrink(_ context.Context, i string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.drinks[i]; !ok {
		return model.ErrNotFound
	}
	delete(r.drinks, i)
	for k, v := range r.relations {
		if v.DrinkID == i {
			delete(r.relations, k)
		}
	}
	return nil
}
func key(d, r string) string { return d + ":" + r }
func relationKey(v model.DeviceDrink) string {
	if v.OriginID != "" {
		return key(v.DeviceID, "origin:"+v.OriginID)
	}
	return key(v.DeviceID, "drink:"+v.DrinkID)
}
func (r *Memory) SetDeviceDrink(_ context.Context, v model.DeviceDrink) (model.DeviceDrink, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.devices[v.DeviceID]; !ok {
		return v, model.ErrNotFound
	}
	if _, ok := r.drinks[v.DrinkID]; !ok {
		return v, model.ErrNotFound
	}
	k := relationKey(v)
	old, exists := r.relations[k]
	if exists {
		v.CreatedAt = old.CreatedAt
	}
	v.CreatedAt, v.UpdatedAt = createdUpdated(v.CreatedAt, v.UpdatedAt)
	r.relations[k] = v
	return v, nil
}
func (r *Memory) ListDeviceDrinks(_ context.Context, d string) ([]model.DeviceDrink, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if _, ok := r.devices[d]; !ok {
		return nil, model.ErrNotFound
	}
	o := []model.DeviceDrink{}
	for _, v := range r.relations {
		if v.DeviceID == d {
			o = append(o, v)
		}
	}
	sort.Slice(o, func(i, j int) bool { return o[i].DrinkID < o[j].DrinkID })
	return o, nil
}
func (r *Memory) DeleteDeviceDrink(_ context.Context, d, x string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	for k, v := range r.relations {
		if v.DeviceID == d && (v.DrinkID == x || v.OriginID == x) {
			delete(r.relations, k)
			return nil
		}
	}
	return model.ErrNotFound
}

var _ model.Repository = (*Memory)(nil)
