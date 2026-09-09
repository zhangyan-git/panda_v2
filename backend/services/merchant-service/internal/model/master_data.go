package model

import "time"

type Merchant struct {
	ID           string    `db:"id" json:"id"`
	Name         string    `db:"name" json:"name"`
	Status       string    `db:"status" json:"status"`
	ContactName  string    `db:"contact_name" json:"contactName"`
	ContactPhone string    `db:"contact_phone" json:"contactPhone"`
	ContactEmail string    `db:"contact_email" json:"contactEmail"`
	CreatedAt    time.Time `db:"created_at" json:"createdAt"`
	UpdatedAt    time.Time `db:"updated_at" json:"updatedAt"`
}
