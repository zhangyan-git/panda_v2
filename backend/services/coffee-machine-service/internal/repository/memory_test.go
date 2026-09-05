package repository

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/panda-dev/panda-v2/backend/services/coffee-machine-service/internal/model"
	"github.com/panda-dev/panda-v2/backend/services/coffee-machine-service/internal/service"
)

func TestMemoryTimestampsAndDeterministicLists(t *testing.T) {
	r := NewMemory()
	created := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	for _, m := range []model.Manufacturer{{ID: "b", Name: "b"}, {ID: "a", Name: "a", CreatedAt: created}} {
		if _, err := r.CreateManufacturer(context.Background(), m); err != nil {
			t.Fatal(err)
		}
	}
	got, err := r.ListManufacturers(context.Background())
	if err != nil || len(got) != 2 || got[0].ID != "a" || got[1].ID != "b" {
		t.Fatalf("got=%+v err=%v", got, err)
	}
	if !got[0].CreatedAt.Equal(created) || got[0].UpdatedAt.IsZero() {
		t.Fatalf("timestamps=%+v", got[0])
	}
}

func TestMemoryRelationsAreIdempotentAndCascade(t *testing.T) {
	r := NewMemory()
	ctx := context.Background()
	m, _ := r.CreateManufacturer(ctx, model.Manufacturer{ID: "m", Name: "maker"})
	d, _ := r.CreateDevice(ctx, model.Device{ID: "d", ManufacturerID: m.ID, Name: "machine"})
	drink, _ := r.CreateDrink(ctx, model.Drink{ID: "r", Name: "coffee"})
	first, err := r.SetDeviceDrink(ctx, model.DeviceDrink{DeviceID: d.ID, DrinkID: drink.ID, Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	second, err := r.SetDeviceDrink(ctx, model.DeviceDrink{DeviceID: d.ID, DrinkID: drink.ID, Enabled: false})
	if err != nil || len(mustRelations(t, r, d.ID)) != 1 || second.CreatedAt != first.CreatedAt || second.Enabled {
		t.Fatalf("first=%+v second=%+v err=%v", first, second, err)
	}
	if err := r.DeleteDevice(ctx, d.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := r.ListDeviceDrinks(ctx, d.ID); err != model.ErrNotFound {
		t.Fatalf("list after device delete=%v", err)
	}
	if err := r.DeleteDrink(ctx, drink.ID); err != nil {
		t.Fatalf("delete drink: %v", err)
	}
}

func mustRelations(t *testing.T, r *Memory, id string) []model.DeviceDrink {
	t.Helper()
	v, err := r.ListDeviceDrinks(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func TestMemoryUpsertsByProviderKeys(t *testing.T) {
	r := NewMemory()
	ctx := context.Background()
	m, _ := r.CreateManufacturer(ctx, model.Manufacturer{ID: "m", Name: "maker"})
	first, err := r.UpsertDeviceBySerialUnique(ctx, model.Device{ManufacturerID: m.ID, Name: "machine", SerialUnique: "serial"})
	if err != nil {
		t.Fatal(err)
	}
	second, err := r.UpsertDeviceBySerialUnique(ctx, model.Device{ManufacturerID: m.ID, Name: "renamed", SerialUnique: "serial"})
	if err != nil || second.ID != first.ID || second.Name != "renamed" || len(mustDevices(t, r)) != 1 {
		t.Fatalf("first=%+v second=%+v err=%v", first, second, err)
	}
	firstDrink, err := r.UpsertDrinkByOriginID(ctx, model.Drink{OriginID: "origin", Name: "coffee", VIPPrice: 12, PickupCodePrice: 8})
	if err != nil {
		t.Fatal(err)
	}
	secondDrink, err := r.UpsertDrinkByOriginID(ctx, model.Drink{OriginID: "origin", Name: "tea"})
	if err != nil || secondDrink.ID != firstDrink.ID || secondDrink.VIPPrice != 12 || secondDrink.PickupCodePrice != 8 {
		t.Fatalf("first=%+v second=%+v err=%v", firstDrink, secondDrink, err)
	}
}

func mustDevices(t *testing.T, r *Memory) []model.Device {
	t.Helper()
	v, err := r.ListDevices(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func TestMemoryRejectsMissingRelationReferences(t *testing.T) {
	r := NewMemory()
	if _, err := r.SetDeviceDrink(context.Background(), model.DeviceDrink{DeviceID: "d", DrinkID: "r"}); err != model.ErrNotFound {
		t.Fatalf("error=%v", err)
	}
	if err := r.DeleteDeviceDrink(context.Background(), "d", "r"); err != model.ErrNotFound {
		t.Fatalf("error=%v", err)
	}
}

func TestServiceValidationAndReferences(t *testing.T) {
	r := NewMemory()
	s := service.NewService(r)
	if _, err := s.CreateDrink(context.Background(), model.Drink{Name: "coffee", Price: -1}); !errors.Is(err, model.ErrInvalid) {
		t.Fatalf("negative price error = %v", err)
	}
	if _, err := s.CreateDrink(context.Background(), model.Drink{Name: "coffee", Status: "unknown"}); !errors.Is(err, model.ErrInvalid) {
		t.Fatalf("invalid status error = %v", err)
	}
	if _, err := s.CreateDevice(context.Background(), model.Device{ManufacturerID: "missing", Name: "machine"}); !errors.Is(err, model.ErrNotFound) {
		t.Fatalf("missing manufacturer error = %v", err)
	}
	m, err := s.CreateManufacturer(context.Background(), model.Manufacturer{Name: "maker"})
	if err != nil {
		t.Fatal(err)
	}
	d, err := s.CreateDevice(context.Background(), model.Device{ManufacturerID: m.ID, Name: "machine"})
	if err != nil || d.Status != model.StatusActive {
		t.Fatalf("device=%+v error=%v", d, err)
	}
	drink, err := s.CreateDrink(context.Background(), model.Drink{Name: "coffee", Price: 10})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.SetDeviceDrink(context.Background(), model.DeviceDrink{DeviceID: d.ID, DrinkID: drink.ID}); err != nil {
		t.Fatal(err)
	}
}

func TestServiceRejectsBlankIDs(t *testing.T) {
	s := service.NewService(NewMemory())
	if _, err := s.GetDrink(context.Background(), " "); !errors.Is(err, model.ErrInvalid) {
		t.Fatalf("error=%v", err)
	}
	if err := s.DeleteDeviceDrink(context.Background(), "d", " "); !errors.Is(err, model.ErrInvalid) {
		t.Fatalf("error=%v", err)
	}
}

func TestNullableScanConversions(t *testing.T) {
	when := time.Date(2026, time.January, 2, 3, 4, 5, 0, time.UTC)
	tests := []struct {
		name string
		got  any
		want any
	}{
		{name: "null string", got: nullString(sql.NullString{}), want: ""},
		{name: "valid string", got: nullString(sql.NullString{String: "value", Valid: true}), want: "value"},
		{name: "null time", got: nullTime(sql.NullTime{}), want: time.Time{}},
		{name: "valid time", got: nullTime(sql.NullTime{Time: when, Valid: true}), want: when},
		{name: "null json", got: rawMessage(nil), want: json.RawMessage(nil)},
		{name: "valid json", got: rawMessage([]byte(`{"enabled":true}`)), want: json.RawMessage(`{"enabled":true}`)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			switch want := tt.want.(type) {
			case time.Time:
				if got := tt.got.(time.Time); !got.Equal(want) {
					t.Fatalf("got %v, want %v", got, want)
				}
			case json.RawMessage:
				if got := tt.got.(json.RawMessage); string(got) != string(want) {
					t.Fatalf("got %s, want %s", got, want)
				}
			default:
				if tt.got != want {
					t.Fatalf("got %v, want %v", tt.got, want)
				}
			}
		})
	}
}
