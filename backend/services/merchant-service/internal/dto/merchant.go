package dto

import "github.com/panda-dev/panda-v2/backend/services/merchant-service/internal/model"

// MerchantRequest and StoreRequest provide the public JSON naming contract.
type MerchantRequest struct {
	ID              string       `json:"id"`
	Name            string       `json:"name"`
	ContactName     string       `json:"contact_name"`
	ContactPhone    string       `json:"contact_phone"`
	BusinessLicense string       `json:"business_license"`
	Address         string       `json:"address"`
	Status          model.Status `json:"status"`
}
type StoreRequest struct {
	ID            string       `json:"id"`
	MerchantID    string       `json:"merchant_id"`
	BrandID       string       `json:"brand_id"`
	Name          string       `json:"name"`
	Logo          string       `json:"logo"`
	Phone         string       `json:"phone"`
	Province      string       `json:"province"`
	City          string       `json:"city"`
	District      string       `json:"district"`
	Address       string       `json:"address"`
	BusinessHours string       `json:"business_hours"`
	Longitude     float64      `json:"longitude"`
	Latitude      float64      `json:"latitude"`
	Status        model.Status `json:"status"`
	Visible       bool         `json:"visible"`
}

func (v MerchantRequest) Merchant() model.Merchant {
	return model.Merchant{ID: v.ID, Name: v.Name, ContactName: v.ContactName, ContactPhone: v.ContactPhone, BusinessLicense: v.BusinessLicense, Address: v.Address, Status: v.Status}
}
func Store(v StoreRequest) model.Store {
	return model.Store{ID: v.ID, MerchantID: v.MerchantID, BrandID: v.BrandID, Name: v.Name, Logo: v.Logo, Phone: v.Phone, Province: v.Province, City: v.City, District: v.District, Address: v.Address, BusinessHours: v.BusinessHours, Longitude: v.Longitude, Latitude: v.Latitude, Status: v.Status, Visible: v.Visible}
}
