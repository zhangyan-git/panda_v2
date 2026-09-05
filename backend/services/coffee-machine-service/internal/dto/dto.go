package dto

import (
	"encoding/json"
	"time"
)

type Manufacturer struct {
	ID             string `json:"id,omitempty"`
	Name           string `json:"name"`
	ContactName    string `json:"contact_name,omitempty"`
	ContactPhone   string `json:"contact_phone,omitempty"`
	Code           string `json:"code,omitempty"`
	MerchantID     string `json:"merchant_id,omitempty"`
	APIBaseURL     string `json:"api_base_url,omitempty"`
	TestAPIBaseURL string `json:"test_api_base_url,omitempty"`
	Status         string `json:"status,omitempty"`
}
type Device struct {
	ID               string          `json:"id,omitempty"`
	ManufacturerID   string          `json:"manufacturer_id"`
	Name             string          `json:"name"`
	SerialNumber     string          `json:"serial_number,omitempty"`
	Location         string          `json:"location,omitempty"`
	Status           string          `json:"status"`
	SerialUnique     string          `json:"serial_unique,omitempty"`
	DeviceName       string          `json:"device_name,omitempty"`
	ManufacturerCode string          `json:"manufacturer_code,omitempty"`
	StoreID          string          `json:"store_id,omitempty"`
	StoreName        string          `json:"store_name,omitempty"`
	Online           bool            `json:"online"`
	Version          string          `json:"version,omitempty"`
	Address          string          `json:"address,omitempty"`
	Error            string          `json:"error,omitempty"`
	LastActivityAt   time.Time       `json:"last_activity_at,omitempty"`
	DisplayConfig    json.RawMessage `json:"display_config,omitempty"`
	PaymentConfig    json.RawMessage `json:"payment_config,omitempty"`
}
type Drink struct {
	ID              string `json:"id,omitempty"`
	Name            string `json:"name"`
	Description     string `json:"description,omitempty"`
	Price           int64  `json:"price"`
	Status          string `json:"status"`
	OriginID        string `json:"origin_id,omitempty"`
	ProductNum      string `json:"product_num,omitempty"`
	EnName          string `json:"en_name,omitempty"`
	VIPPrice        int64  `json:"vip_price"`
	PickupCodePrice int64  `json:"pickup_code_price"`
	Image           string `json:"image,omitempty"`
	Sort            int    `json:"sort"`
}
type DeviceDrink struct {
	DeviceID string `json:"device_id"`
	DrinkID  string `json:"drink_id,omitempty"`
	OriginID string `json:"origin_id,omitempty"`
	Enabled  bool   `json:"enabled"`
}
