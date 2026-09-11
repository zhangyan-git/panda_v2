package service

import "context"

// MerchantAccessPort exposes the small set of merchant facts needed by
// merchant accounts. It is implemented by the remote merchant-service client.
type MerchantAccessPort interface {
	FindStatus(ctx context.Context, merchantID string) (string, error)
	FindName(ctx context.Context, merchantID string) (string, error)
}
