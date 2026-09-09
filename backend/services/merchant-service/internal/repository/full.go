package repository

import "context"

type FullRepository interface {
	Ping(context.Context) error
}
