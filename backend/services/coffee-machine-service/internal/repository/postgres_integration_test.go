package repository

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/panda-dev/panda-v2/backend/services/coffee-machine-service/internal/model"
)

func TestPostgreSQLIntegration(t *testing.T) {
	url := os.Getenv("COFFEE_MACHINE_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("COFFEE_MACHINE_TEST_DATABASE_URL is not set")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	config, err := pgxpool.ParseConfig(url)
	if err != nil {
		t.Fatalf("parse database URL: %v", err)
	}
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		t.Fatalf("connect to PostgreSQL: %v", err)
	}
	defer pool.Close()
	if err := pool.Ping(ctx); err != nil {
		t.Fatalf("ping PostgreSQL: %v", err)
	}

	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin setup transaction: %v", err)
	}
	defer tx.Rollback(ctx)
	migrationPath := migrationFilePath(t)
	migration, err := os.ReadFile(migrationPath)
	if err != nil {
		t.Fatalf("read migration %s: %v", migrationPath, err)
	}
	if _, err := tx.Exec(ctx, string(migration)); err != nil {
		t.Fatalf("execute migration: %v", err)
	}
	repo := &PostgreSQL{pool: tx, exec: tx.Exec}

	manufacturerID := uuid.NewString()
	manufacturer, err := repo.CreateManufacturer(ctx, model.Manufacturer{ID: manufacturerID, Name: "Integration Manufacturer"})
	if err != nil {
		t.Fatalf("create manufacturer: %v", err)
	}
	if manufacturer.Status != model.StatusActive || manufacturer.ID != manufacturerID {
		t.Fatalf("unexpected manufacturer: %+v", manufacturer)
	}

	deviceID := uuid.NewString()
	device, err := repo.CreateDevice(ctx, model.Device{
		ID: deviceID, ManufacturerID: manufacturerID, Name: "Integration Device",
		SerialNumber: "serial-" + deviceID, DisplayConfig: []byte(`{"screen":"bright"}`),
		PaymentConfig: []byte(`{"cash":true}`),
	})
	if err != nil {
		t.Fatalf("create device: %v", err)
	}
	if string(device.DisplayConfig) != `{"screen":"bright"}` || device.PaymentConfig == nil {
		t.Fatalf("JSONB values were not preserved: %+v", device)
	}
	minimalDevice, err := repo.CreateDevice(ctx, model.Device{ManufacturerID: manufacturerID, Name: "Nullable Device"})
	if err != nil {
		t.Fatalf("create nullable device: %v", err)
	}
	if minimalDevice.SerialNumber != "" || minimalDevice.DisplayConfig != nil || !minimalDevice.CreatedAt.After(time.Time{}) {
		t.Fatalf("nullable/default fields were not scanned correctly: %+v", minimalDevice)
	}

	drink, err := repo.CreateDrink(ctx, model.Drink{ID: uuid.NewString(), Name: "Integration Drink", Description: "initial"})
	if err != nil {
		t.Fatalf("create drink: %v", err)
	}
	updated, err := repo.UpdateDrink(ctx, drink.ID, model.Drink{Name: "Updated Drink", Description: "updated", Price: 125})
	if err != nil {
		t.Fatalf("update drink: %v", err)
	}
	if updated.Name != "Updated Drink" || updated.Price != 125 {
		t.Fatalf("unexpected updated drink: %+v", updated)
	}
	if _, err := repo.GetDrink(ctx, drink.ID); err != nil {
		t.Fatalf("get drink: %v", err)
	}

	secondDrink, err := repo.CreateDrink(ctx, model.Drink{ID: uuid.NewString(), Name: "Second Drink"})
	if err != nil {
		t.Fatalf("create second drink: %v", err)
	}
	if _, err := repo.SetDeviceDrink(ctx, model.DeviceDrink{DeviceID: deviceID, DrinkID: drink.ID, OriginID: " origin-1 ", Enabled: true}); err != nil {
		t.Fatalf("create origin relation: %v", err)
	}
	if _, err := repo.SetDeviceDrink(ctx, model.DeviceDrink{DeviceID: deviceID, DrinkID: secondDrink.ID, OriginID: "origin-1", Enabled: false}); err != nil {
		t.Fatalf("upsert origin relation: %v", err)
	}
	if _, err := repo.SetDeviceDrink(ctx, model.DeviceDrink{DeviceID: deviceID, DrinkID: drink.ID, Enabled: true}); err != nil {
		t.Fatalf("create drink relation: %v", err)
	}
	relations, err := repo.ListDeviceDrinks(ctx, deviceID)
	if err != nil {
		t.Fatalf("list relations: %v", err)
	}
	if len(relations) != 2 {
		t.Fatalf("upsert did not preserve generated-key cardinality: got %d relations", len(relations))
	}
	var relationKey string
	if err := tx.QueryRow(ctx, "SELECT relation_key FROM device_drinks WHERE device_id=$1 AND origin_id=$2", deviceID, "origin-1").Scan(&relationKey); err != nil {
		t.Fatalf("read generated relation_key: %v", err)
	}
	if relationKey != "origin:origin-1" {
		t.Fatalf("got relation_key %q, want origin:origin-1", relationKey)
	}

	if _, err := tx.Exec(ctx, "INSERT INTO drinks (id,name,price) VALUES ($1,$2,-1)", uuid.NewString(), "invalid"); err == nil {
		t.Fatal("negative price constraint unexpectedly succeeded")
	}
	if _, err := tx.Exec(ctx, "INSERT INTO devices (id,manufacturer_id,name) VALUES ($1,$2,$3)", uuid.NewString(), "missing", "invalid"); err == nil {
		t.Fatal("foreign-key constraint unexpectedly succeeded")
	}
	if err := repo.DeleteManufacturer(ctx, manufacturerID); err == nil {
		t.Fatal("restrict relationship unexpectedly allowed manufacturer deletion")
	} else if !isForeignKeyError(err) {
		t.Fatalf("manufacturer deletion returned non-FK error: %v", err)
	}
	if err := repo.DeleteDevice(ctx, deviceID); err != nil {
		t.Fatalf("delete device: %v", err)
	}
	var relationCount int
	if err := tx.QueryRow(ctx, "SELECT count(*) FROM device_drinks WHERE device_id=$1", deviceID).Scan(&relationCount); err != nil {
		t.Fatalf("check cascade relation deletion: %v", err)
	}
	if relationCount != 0 {
		t.Fatalf("cascade left %d relations", relationCount)
	}
	if err := repo.DeleteDevice(ctx, minimalDevice.ID); err != nil {
		t.Fatalf("delete nullable device: %v", err)
	}
	if err := repo.DeleteDrink(ctx, drink.ID); err != nil {
		t.Fatalf("delete drink: %v", err)
	}
	if err := repo.DeleteDrink(ctx, secondDrink.ID); err != nil {
		t.Fatalf("delete second drink: %v", err)
	}
	if err := repo.DeleteManufacturer(ctx, manufacturerID); err != nil {
		t.Fatalf("delete manufacturer after dependents: %v", err)
	}
}

func migrationFilePath(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("locate integration test")
	}
	return filepath.Join(filepath.Dir(file), "..", "..", "migrations", "001_create_coffee_machine.sql")
}

func isForeignKeyError(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23503"
}
