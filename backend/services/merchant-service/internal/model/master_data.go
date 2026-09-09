package model

import "time"

type Merchant struct {
	ID           string    `json:"id"`
	Name         string    `json:"name"`
	Status       string    `json:"status"`
	ContactName  string    `json:"contact_name"`
	ContactPhone string    `json:"contact_phone"`
	ContactEmail string    `json:"contact_email"`
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`
}

type Brand struct {
	ID         string    `json:"id"`
	MerchantID string    `json:"merchant_id"`
	Name       string    `json:"name"`
	Status     string    `json:"status"`
	CreatedAt  time.Time `json:"created_at"`
	UpdatedAt  time.Time `json:"updated_at"`
}

type Store struct {
	ID         string    `json:"id"`
	MerchantID string    `json:"merchant_id"`
	BrandID    string    `json:"brand_id,omitempty"`
	Name       string    `json:"name"`
	Status     string    `json:"status"`
	Address    string    `json:"address"`
	CreatedAt  time.Time `json:"created_at"`
	UpdatedAt  time.Time `json:"updated_at"`
}
