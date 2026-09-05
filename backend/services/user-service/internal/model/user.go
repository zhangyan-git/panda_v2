package model

import (
	"context"
	"errors"
	"time"
)

var ErrInvalidName = errors.New("user name is required")
var ErrNotFound = errors.New("user not found")

type User struct {
	ID         int64     `json:"id"`
	Name       string    `json:"name"`
	Nickname   string    `json:"nickname"`
	AvatarURL  string    `json:"avatar_url"`
	Email      string    `json:"email"`
	Gender     string    `json:"gender"`
	Birthday   string    `json:"birthday"`
	Occupation string    `json:"occupation"`
	Hobbies    []string  `json:"hobbies"`
	RegionCode string    `json:"region_code"`
	RegionName string    `json:"region_name"`
	Status     string    `json:"status"`
	CreatedAt  time.Time `json:"created_at"`
	UpdatedAt  time.Time `json:"updated_at"`
}

type UserUpdate struct {
	Nickname   *string
	AvatarURL  *string
	Email      *string
	Gender     *string
	Birthday   *string
	Occupation *string
	Hobbies    *[]string
	RegionCode *string
	RegionName *string
}

type Repository interface {
	Create(context.Context, string) (User, error)
	GetByID(context.Context, int64) (User, error)
	Update(context.Context, int64, UserUpdate) (User, error)
}
