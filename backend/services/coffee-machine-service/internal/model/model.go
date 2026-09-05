package model

import (
	"context"
	"encoding/json"
	"errors"
	"time"
)

var (
	ErrNotFound = errors.New("coffee machine resource not found")
	ErrInvalid  = errors.New("invalid coffee machine resource")
)

const (
	StatusActive   = "active"
	StatusDisabled = "disabled"
)

type Manufacturer struct {
	ID, Name, ContactName, ContactPhone string
	Code, MerchantID                    string
	APIBaseURL, TestAPIBaseURL          string
	Status                              string
	// Credentials are retained for internal integrations only and are never exposed by HTTP APIs.
	Username, Secret, Token string
	CreatedAt, UpdatedAt    time.Time
}
type Device struct {
	ID, ManufacturerID, Name, SerialNumber, Location string
	SerialUnique, DeviceName, ManufacturerCode       string
	StoreID, StoreName                               string
	Online                                           bool
	Version, Address, Error                          string
	LastActivityAt                                   time.Time
	DisplayConfig, PaymentConfig                     json.RawMessage
	Status                                           string
	CreatedAt, UpdatedAt                             time.Time
}
type Drink struct {
	ID, Name, Description string
	OriginID, ProductNum  string
	EnName, Image         string
	Price, VIPPrice       int64
	PickupCodePrice       int64
	Sort                  int
	Status                string
	CreatedAt, UpdatedAt  time.Time
}
type DeviceDrink struct {
	DeviceID, DrinkID, OriginID string
	Enabled                     bool
	CreatedAt, UpdatedAt        time.Time
}

type Repository interface {
	CreateManufacturer(context.Context, Manufacturer) (Manufacturer, error)
	GetManufacturer(context.Context, string) (Manufacturer, error)
	ListManufacturers(context.Context) ([]Manufacturer, error)
	UpdateManufacturer(context.Context, string, Manufacturer) (Manufacturer, error)
	DeleteManufacturer(context.Context, string) error
	CreateDevice(context.Context, Device) (Device, error)
	UpsertDeviceBySerialUnique(context.Context, Device) (Device, error)
	GetDevice(context.Context, string) (Device, error)
	ListDevices(context.Context) ([]Device, error)
	UpdateDevice(context.Context, string, Device) (Device, error)
	DeleteDevice(context.Context, string) error
	CreateDrink(context.Context, Drink) (Drink, error)
	UpsertDrinkByOriginID(context.Context, Drink) (Drink, error)
	GetDrink(context.Context, string) (Drink, error)
	ListDrinks(context.Context) ([]Drink, error)
	UpdateDrink(context.Context, string, Drink) (Drink, error)
	DeleteDrink(context.Context, string) error
	SetDeviceDrink(context.Context, DeviceDrink) (DeviceDrink, error)
	ListDeviceDrinks(context.Context, string) ([]DeviceDrink, error)
	DeleteDeviceDrink(context.Context, string, string) error
}
