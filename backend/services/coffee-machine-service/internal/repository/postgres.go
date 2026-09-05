package repository

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/panda-dev/panda-v2/backend/services/coffee-machine-service/internal/model"
)

type rowQuerier interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}

type querier interface {
	rowQuerier
	Query(context.Context, string, ...any) (pgx.Rows, error)
}

type PostgreSQL struct {
	pool querier
	exec func(context.Context, string, ...any) (pgconn.CommandTag, error)
}

func NewPostgreSQL(pool *pgxpool.Pool) *PostgreSQL { return &PostgreSQL{pool: pool, exec: pool.Exec} }

const (
	manufacturerColumns = "id, name, contact_name, contact_phone, code, merchant_id, api_base_url, test_api_base_url, status, created_at, updated_at"
	deviceColumns       = "id, manufacturer_id, name, serial_number, location, serial_unique, device_name, manufacturer_code, store_id, store_name, online, version, address, error, last_activity_at, display_config, payment_config, status, created_at, updated_at"
	drinkColumns        = "id, name, description, origin_id, product_num, en_name, price, vip_price, pickup_code_price, image, sort, status, created_at, updated_at"
	deviceDrinkColumns  = "device_id, drink_id, origin_id, enabled, created_at, updated_at"
)

func notFound(err error) error {
	if errors.Is(err, pgx.ErrNoRows) {
		return model.ErrNotFound
	}
	return persistenceError(err)
}

func persistenceError(err error) error {
	if errors.Is(err, pgx.ErrNoRows) {
		return model.ErrNotFound
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		switch pgErr.Code {
		case "23503": // foreign_key_violation
			return model.ErrInvalid
		case "23502", "23505", "23514", "23522": // not-null, unique, check, and domain violations
			return model.ErrInvalid
		}
	}
	return err
}

func nullString(v sql.NullString) string {
	if v.Valid {
		return v.String
	}
	return ""
}

func nullTime(v sql.NullTime) time.Time {
	if v.Valid {
		return v.Time
	}
	return time.Time{}
}

func rawMessage(v []byte) json.RawMessage {
	if v == nil {
		return nil
	}
	return json.RawMessage(v)
}

func scanManufacturer(row pgx.Row) (model.Manufacturer, error) {
	var v model.Manufacturer
	var name, contactName, contactPhone, code, merchantID, apiBaseURL, testAPIBaseURL, status sql.NullString
	var createdAt, updatedAt sql.NullTime
	err := row.Scan(&v.ID, &name, &contactName, &contactPhone, &code, &merchantID, &apiBaseURL, &testAPIBaseURL, &status, &createdAt, &updatedAt)
	v.Name, v.ContactName, v.ContactPhone = nullString(name), nullString(contactName), nullString(contactPhone)
	v.Code, v.MerchantID = nullString(code), nullString(merchantID)
	v.APIBaseURL, v.TestAPIBaseURL, v.Status = nullString(apiBaseURL), nullString(testAPIBaseURL), nullString(status)
	v.CreatedAt, v.UpdatedAt = nullTime(createdAt), nullTime(updatedAt)
	return v, err
}
func scanDevice(row pgx.Row) (model.Device, error) {
	var v model.Device
	var manufacturerID, name, serialNumber, location, serialUnique, deviceName, manufacturerCode, storeID, storeName sql.NullString
	var version, address, deviceError, status sql.NullString
	var lastActivityAt, createdAt, updatedAt sql.NullTime
	var displayConfig, paymentConfig []byte
	err := row.Scan(&v.ID, &manufacturerID, &name, &serialNumber, &location, &serialUnique, &deviceName, &manufacturerCode, &storeID, &storeName, &v.Online, &version, &address, &deviceError, &lastActivityAt, &displayConfig, &paymentConfig, &status, &createdAt, &updatedAt)
	v.ManufacturerID, v.Name, v.SerialNumber = nullString(manufacturerID), nullString(name), nullString(serialNumber)
	v.Location, v.SerialUnique, v.DeviceName = nullString(location), nullString(serialUnique), nullString(deviceName)
	v.ManufacturerCode, v.StoreID, v.StoreName = nullString(manufacturerCode), nullString(storeID), nullString(storeName)
	v.Version, v.Address, v.Error, v.Status = nullString(version), nullString(address), nullString(deviceError), nullString(status)
	v.LastActivityAt, v.CreatedAt, v.UpdatedAt = nullTime(lastActivityAt), nullTime(createdAt), nullTime(updatedAt)
	v.DisplayConfig, v.PaymentConfig = rawMessage(displayConfig), rawMessage(paymentConfig)
	return v, err
}
func scanDrink(row pgx.Row) (model.Drink, error) {
	var v model.Drink
	var name, description, originID, productNum, enName, image, status sql.NullString
	var createdAt, updatedAt sql.NullTime
	err := row.Scan(&v.ID, &name, &description, &originID, &productNum, &enName, &v.Price, &v.VIPPrice, &v.PickupCodePrice, &image, &v.Sort, &status, &createdAt, &updatedAt)
	v.Name, v.Description, v.OriginID = nullString(name), nullString(description), nullString(originID)
	v.ProductNum, v.EnName, v.Image, v.Status = nullString(productNum), nullString(enName), nullString(image), nullString(status)
	v.CreatedAt, v.UpdatedAt = nullTime(createdAt), nullTime(updatedAt)
	return v, err
}
func scanDeviceDrink(row pgx.Row) (model.DeviceDrink, error) {
	var v model.DeviceDrink
	var drinkID, originID sql.NullString
	var createdAt, updatedAt sql.NullTime
	err := row.Scan(&v.DeviceID, &drinkID, &originID, &v.Enabled, &createdAt, &updatedAt)
	v.DrinkID, v.OriginID = nullString(drinkID), nullString(originID)
	v.CreatedAt, v.UpdatedAt = nullTime(createdAt), nullTime(updatedAt)
	return v, err
}

func (r *PostgreSQL) CreateManufacturer(ctx context.Context, v model.Manufacturer) (model.Manufacturer, error) {
	if v.ID == "" {
		v.ID = uuid.NewString()
	}
	q := `INSERT INTO manufacturers (id,name,contact_name,contact_phone,code,merchant_id,api_base_url,test_api_base_url,status) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,COALESCE(NULLIF($9,''),'active')) RETURNING ` + manufacturerColumns
	x, err := scanManufacturer(r.pool.QueryRow(ctx, q, v.ID, v.Name, v.ContactName, v.ContactPhone, v.Code, v.MerchantID, v.APIBaseURL, v.TestAPIBaseURL, v.Status))
	return x, persistenceError(err)
}
func (r *PostgreSQL) GetManufacturer(ctx context.Context, id string) (model.Manufacturer, error) {
	x, err := scanManufacturer(r.pool.QueryRow(ctx, `SELECT `+manufacturerColumns+` FROM manufacturers WHERE id=$1`, id))
	return x, notFound(err)
}
func (r *PostgreSQL) ListManufacturers(ctx context.Context) ([]model.Manufacturer, error) {
	rows, err := r.pool.Query(ctx, `SELECT `+manufacturerColumns+` FROM manufacturers ORDER BY created_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []model.Manufacturer{}
	for rows.Next() {
		x, e := scanManufacturer(rows)
		if e != nil {
			return nil, e
		}
		out = append(out, x)
	}
	return out, rows.Err()
}
func (r *PostgreSQL) UpdateManufacturer(ctx context.Context, id string, v model.Manufacturer) (model.Manufacturer, error) {
	q := `UPDATE manufacturers SET name=$2,contact_name=$3,contact_phone=$4,code=$5,merchant_id=$6,api_base_url=$7,test_api_base_url=$8,updated_at=NOW() WHERE id=$1 RETURNING ` + manufacturerColumns
	x, err := scanManufacturer(r.pool.QueryRow(ctx, q, id, v.Name, v.ContactName, v.ContactPhone, v.Code, v.MerchantID, v.APIBaseURL, v.TestAPIBaseURL))
	return x, persistenceError(err)
}
func (r *PostgreSQL) DeleteManufacturer(ctx context.Context, id string) error {
	tag, err := r.exec(ctx, `DELETE FROM manufacturers WHERE id=$1`, id)
	if err != nil {
		return persistenceError(err)
	}
	if tag.RowsAffected() == 0 {
		return model.ErrNotFound
	}
	return nil
}

func (r *PostgreSQL) CreateDevice(ctx context.Context, v model.Device) (model.Device, error) {
	if v.ID == "" {
		v.ID = uuid.NewString()
	}
	q := `INSERT INTO devices (id,manufacturer_id,name,serial_number,location,serial_unique,device_name,manufacturer_code,store_id,store_name,online,version,address,error,last_activity_at,display_config,payment_config,status) VALUES ($1,$2,$3,NULLIF($4,''),$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,COALESCE(NULLIF($18,''),'active')) RETURNING ` + deviceColumns
	x, err := scanDevice(r.pool.QueryRow(ctx, q, v.ID, v.ManufacturerID, v.Name, v.SerialNumber, v.Location, v.SerialUnique, v.DeviceName, v.ManufacturerCode, v.StoreID, v.StoreName, v.Online, v.Version, v.Address, v.Error, v.LastActivityAt, v.DisplayConfig, v.PaymentConfig, v.Status))
	return x, persistenceError(err)
}
func (r *PostgreSQL) UpsertDeviceBySerialUnique(ctx context.Context, v model.Device) (model.Device, error) {
	if v.ID == "" {
		v.ID = uuid.NewString()
	}
	q := `INSERT INTO devices (id,manufacturer_id,name,serial_number,location,serial_unique,device_name,manufacturer_code,store_id,store_name,online,version,address,error,last_activity_at,display_config,payment_config,status) VALUES ($1,$2,$3,NULLIF($4,''),$5,NULLIF($6,''),$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,COALESCE(NULLIF($18,''),'active')) ON CONFLICT (serial_unique) WHERE serial_unique IS NOT NULL DO UPDATE SET manufacturer_id=EXCLUDED.manufacturer_id,name=EXCLUDED.name,serial_number=EXCLUDED.serial_number,location=EXCLUDED.location,device_name=EXCLUDED.device_name,manufacturer_code=EXCLUDED.manufacturer_code,store_id=EXCLUDED.store_id,store_name=EXCLUDED.store_name,online=EXCLUDED.online,version=EXCLUDED.version,address=EXCLUDED.address,error=EXCLUDED.error,last_activity_at=EXCLUDED.last_activity_at,display_config=EXCLUDED.display_config,payment_config=EXCLUDED.payment_config,status=EXCLUDED.status,updated_at=NOW() RETURNING ` + deviceColumns
	x, err := scanDevice(r.pool.QueryRow(ctx, q, v.ID, v.ManufacturerID, v.Name, v.SerialNumber, v.Location, v.SerialUnique, v.DeviceName, v.ManufacturerCode, v.StoreID, v.StoreName, v.Online, v.Version, v.Address, v.Error, v.LastActivityAt, v.DisplayConfig, v.PaymentConfig, v.Status))
	return x, persistenceError(err)
}

func (r *PostgreSQL) GetDevice(ctx context.Context, id string) (model.Device, error) {
	x, err := scanDevice(r.pool.QueryRow(ctx, `SELECT `+deviceColumns+` FROM devices WHERE id=$1`, id))
	return x, notFound(err)
}
func (r *PostgreSQL) ListDevices(ctx context.Context) ([]model.Device, error) {
	rows, err := r.pool.Query(ctx, `SELECT `+deviceColumns+` FROM devices ORDER BY created_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []model.Device{}
	for rows.Next() {
		x, e := scanDevice(rows)
		if e != nil {
			return nil, e
		}
		out = append(out, x)
	}
	return out, rows.Err()
}
func (r *PostgreSQL) UpdateDevice(ctx context.Context, id string, v model.Device) (model.Device, error) {
	q := `UPDATE devices SET manufacturer_id=COALESCE(NULLIF(trim($2),''),manufacturer_id),name=$3,serial_number=NULLIF($4,''),location=$5,serial_unique=$6,device_name=$7,manufacturer_code=$8,store_id=$9,store_name=$10,online=$11,version=$12,address=$13,error=$14,last_activity_at=$15,display_config=$16,payment_config=$17,status=COALESCE(NULLIF($18,''),status),updated_at=NOW() WHERE id=$1 RETURNING ` + deviceColumns
	x, err := scanDevice(r.pool.QueryRow(ctx, q, id, v.ManufacturerID, v.Name, v.SerialNumber, v.Location, v.SerialUnique, v.DeviceName, v.ManufacturerCode, v.StoreID, v.StoreName, v.Online, v.Version, v.Address, v.Error, v.LastActivityAt, v.DisplayConfig, v.PaymentConfig, v.Status))
	return x, persistenceError(err)
}
func (r *PostgreSQL) DeleteDevice(ctx context.Context, id string) error {
	tag, err := r.exec(ctx, `DELETE FROM devices WHERE id=$1`, id)
	if err != nil {
		return persistenceError(err)
	}
	if tag.RowsAffected() == 0 {
		return model.ErrNotFound
	}
	return nil
}

func (r *PostgreSQL) CreateDrink(ctx context.Context, v model.Drink) (model.Drink, error) {
	if v.ID == "" {
		v.ID = uuid.NewString()
	}
	q := `INSERT INTO drinks (id,name,description,origin_id,product_num,en_name,price,vip_price,pickup_code_price,image,sort,status) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,COALESCE(NULLIF($12,''),'active')) RETURNING ` + drinkColumns
	x, err := scanDrink(r.pool.QueryRow(ctx, q, v.ID, v.Name, v.Description, v.OriginID, v.ProductNum, v.EnName, v.Price, v.VIPPrice, v.PickupCodePrice, v.Image, v.Sort, v.Status))
	return x, persistenceError(err)
}
func (r *PostgreSQL) UpsertDrinkByOriginID(ctx context.Context, v model.Drink) (model.Drink, error) {
	if v.ID == "" {
		v.ID = uuid.NewString()
	}
	q := `INSERT INTO drinks (id,name,description,origin_id,product_num,en_name,price,vip_price,pickup_code_price,image,sort,status) VALUES ($1,$2,$3,NULLIF($4,''),$5,$6,$7,$8,$9,$10,$11,COALESCE(NULLIF($12,''),'active')) ON CONFLICT (origin_id) WHERE origin_id IS NOT NULL DO UPDATE SET name=EXCLUDED.name,description=EXCLUDED.description,product_num=EXCLUDED.product_num,en_name=EXCLUDED.en_name,price=EXCLUDED.price,image=EXCLUDED.image,sort=EXCLUDED.sort,status=EXCLUDED.status,updated_at=NOW() RETURNING ` + drinkColumns
	x, err := scanDrink(r.pool.QueryRow(ctx, q, v.ID, v.Name, v.Description, v.OriginID, v.ProductNum, v.EnName, v.Price, v.VIPPrice, v.PickupCodePrice, v.Image, v.Sort, v.Status))
	return x, persistenceError(err)
}

func (r *PostgreSQL) GetDrink(ctx context.Context, id string) (model.Drink, error) {
	x, err := scanDrink(r.pool.QueryRow(ctx, `SELECT `+drinkColumns+` FROM drinks WHERE id=$1`, id))
	return x, notFound(err)
}
func (r *PostgreSQL) ListDrinks(ctx context.Context) ([]model.Drink, error) {
	rows, err := r.pool.Query(ctx, `SELECT `+drinkColumns+` FROM drinks ORDER BY created_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []model.Drink{}
	for rows.Next() {
		x, e := scanDrink(rows)
		if e != nil {
			return nil, e
		}
		out = append(out, x)
	}
	return out, rows.Err()
}
func (r *PostgreSQL) UpdateDrink(ctx context.Context, id string, v model.Drink) (model.Drink, error) {
	q := `UPDATE drinks SET name=$2,description=$3,origin_id=$4,product_num=$5,en_name=$6,price=$7,vip_price=$8,pickup_code_price=$9,image=$10,sort=$11,status=COALESCE(NULLIF($12,''),status),updated_at=NOW() WHERE id=$1 RETURNING ` + drinkColumns
	x, err := scanDrink(r.pool.QueryRow(ctx, q, id, v.Name, v.Description, v.OriginID, v.ProductNum, v.EnName, v.Price, v.VIPPrice, v.PickupCodePrice, v.Image, v.Sort, v.Status))
	return x, notFound(err)
}
func (r *PostgreSQL) DeleteDrink(ctx context.Context, id string) error {
	tag, err := r.exec(ctx, `DELETE FROM drinks WHERE id=$1`, id)
	if err != nil {
		return persistenceError(err)
	}
	if tag.RowsAffected() == 0 {
		return model.ErrNotFound
	}
	return nil
}

func (r *PostgreSQL) SetDeviceDrink(ctx context.Context, v model.DeviceDrink) (model.DeviceDrink, error) {
	q := `INSERT INTO device_drinks (device_id,drink_id,origin_id,enabled) VALUES ($1,NULLIF($2,''),NULLIF($3,''),$4) ON CONFLICT (device_id,relation_key) DO UPDATE SET drink_id=EXCLUDED.drink_id,origin_id=EXCLUDED.origin_id,enabled=EXCLUDED.enabled,updated_at=NOW() RETURNING ` + deviceDrinkColumns
	x, err := scanDeviceDrink(r.pool.QueryRow(ctx, q, v.DeviceID, strings.TrimSpace(v.DrinkID), strings.TrimSpace(v.OriginID), v.Enabled))
	return x, persistenceError(err)
}
func (r *PostgreSQL) ListDeviceDrinks(ctx context.Context, id string) ([]model.DeviceDrink, error) {
	rows, err := r.pool.Query(ctx, `SELECT `+deviceDrinkColumns+` FROM device_drinks WHERE device_id=$1 ORDER BY drink_id`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []model.DeviceDrink{}
	for rows.Next() {
		x, e := scanDeviceDrink(rows)
		if e != nil {
			return nil, e
		}
		out = append(out, x)
	}
	return out, rows.Err()
}
func (r *PostgreSQL) DeleteDeviceDrink(ctx context.Context, deviceID, drinkID string) error {
	tag, err := r.exec(ctx, `DELETE FROM device_drinks WHERE device_id=$1 AND (drink_id=$2 OR origin_id=$2)`, deviceID, drinkID)
	if err != nil {
		return persistenceError(err)
	}
	if tag.RowsAffected() == 0 {
		return model.ErrNotFound
	}
	return nil
}

var _ model.Repository = (*PostgreSQL)(nil)
