package client

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/panda-dev/panda-v2/backend/platform/auth"
	merchantv1 "github.com/panda-dev/panda-v2/contracts/proto/merchant/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

var ErrInvalidResponse = errors.New("invalid merchant service response")

// MerchantGRPCClient reads the merchant facts merchant accounts depend on over
// the internal gRPC transport. It replaces the former HTTP client without
// changing the interfaces the services consume.
type MerchantGRPCClient struct {
	merchants merchantv1.MerchantServiceClient
	token     string
	timeout   time.Duration
}

// NewMerchantGRPCClient reuses the caller's connection: main dials once and
// shares the *grpc.ClientConn instead of letting each call site open its own.
func NewMerchantGRPCClient(conn *grpc.ClientConn, token string, timeout time.Duration) (*MerchantGRPCClient, error) {
	if conn == nil {
		return nil, errors.New("merchant service connection is required")
	}
	if strings.TrimSpace(token) == "" {
		return nil, errors.New("merchant service token is required")
	}
	if timeout <= 0 {
		return nil, errors.New("merchant service timeout must be positive")
	}
	return &MerchantGRPCClient{merchants: merchantv1.NewMerchantServiceClient(conn), token: token, timeout: timeout}, nil
}

func (c *MerchantGRPCClient) call(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(auth.WithServiceToken(ctx, c.token), c.timeout)
}

// notFound restores the sentinel the call sites branch on. A resource that does
// not exist must stay distinguishable from a dependency failure: callers turn
// the former into "scope out of merchant" and fail closed on the latter, so a
// transport error must never be dressed up as pgx.ErrNoRows.
func notFound(err error) error {
	if status.Code(err) == codes.NotFound {
		return fmt.Errorf("merchant resource not found: %w", pgx.ErrNoRows)
	}
	return err
}

// merchantStatus returns the merchant lifecycle state as the string the former
// HTTP transport produced. The proto keeps that string for wire compatibility
// and adds status_code for new readers; preferring the string, then falling back
// to the enum, keeps today's values even if a peer populates only one of them.
func merchantStatus(m *merchantv1.Merchant) string {
	if m == nil {
		return ""
	}
	if m.GetStatus() != "" {
		return m.GetStatus()
	}
	switch m.GetStatusCode() {
	case merchantv1.MerchantStatus_MERCHANT_STATUS_ACTIVE:
		return "active"
	case merchantv1.MerchantStatus_MERCHANT_STATUS_PENDING:
		return "pending"
	case merchantv1.MerchantStatus_MERCHANT_STATUS_SUSPENDED:
		return "suspended"
	default:
		return ""
	}
}

// validateID rejects empty, padded, path-traversal and control-character IDs
// before any RPC is sent, so a malformed identifier cannot reach the peer.
func validateID(id string) error {
	if strings.TrimSpace(id) == "" {
		return errors.New("merchant resource ID is required")
	}
	if id != strings.TrimSpace(id) || id == "." || id == ".." || strings.ContainsAny(id, "/\\") || !utf8.ValidString(id) || strings.IndexFunc(id, unicode.IsControl) >= 0 {
		return errors.New("invalid merchant resource ID")
	}
	return nil
}

func (c *MerchantGRPCClient) FindStatus(ctx context.Context, id string) (string, error) {
	if err := validateID(id); err != nil {
		return "", err
	}
	ctx, cancel := c.call(ctx)
	defer cancel()
	resp, err := c.merchants.GetMerchant(ctx, &merchantv1.GetMerchantRequest{Id: id})
	if err != nil {
		return "", notFound(err)
	}
	switch value := merchantStatus(resp.GetMerchant()); value {
	case "active", "pending", "suspended":
		return value, nil
	default:
		return "", ErrInvalidResponse
	}
}

func (c *MerchantGRPCClient) FindName(ctx context.Context, id string) (string, error) {
	if err := validateID(id); err != nil {
		return "", err
	}
	ctx, cancel := c.call(ctx)
	defer cancel()
	resp, err := c.merchants.GetMerchant(ctx, &merchantv1.GetMerchantRequest{Id: id})
	if err != nil {
		return "", notFound(err)
	}
	name := resp.GetMerchant().GetName()
	if strings.TrimSpace(name) == "" {
		return "", ErrInvalidResponse
	}
	return name, nil
}

func (c *MerchantGRPCClient) FindBrandMerchantID(ctx context.Context, id string) (string, error) {
	if err := validateID(id); err != nil {
		return "", err
	}
	ctx, cancel := c.call(ctx)
	defer cancel()
	resp, err := c.merchants.GetBrandMerchant(ctx, &merchantv1.GetBrandMerchantRequest{BrandId: id})
	if err != nil {
		return "", notFound(err)
	}
	merchantID := resp.GetMerchantId()
	if strings.TrimSpace(merchantID) == "" {
		return "", ErrInvalidResponse
	}
	return merchantID, nil
}

// ScopeNames resolves the display names of brand and store scopes in one round
// trip. Ids the peer does not know are absent from the returned maps: a scope
// deleted after it was referenced must not fail a listing. An id that never
// could have existed is a caller bug, so it is rejected before the call.
func (c *MerchantGRPCClient) ScopeNames(ctx context.Context, brandIDs, storeIDs []string) (map[string]string, map[string]string, error) {
	for _, id := range append(append([]string{}, brandIDs...), storeIDs...) {
		if err := validateID(id); err != nil {
			return nil, nil, err
		}
	}
	ctx, cancel := c.call(ctx)
	defer cancel()
	resp, err := c.merchants.ResolveScopeNames(ctx, &merchantv1.ResolveScopeNamesRequest{BrandIds: brandIDs, StoreIds: storeIDs})
	if err != nil {
		return nil, nil, err
	}
	return resp.GetBrandNames(), resp.GetStoreNames(), nil
}

// ListStoreIDs expands a merchant account's data scope into its authorized
// store set. A bad scope type is a caller bug and the peer says so with
// InvalidArgument; it travels up unchanged rather than being flattened into an
// empty set, because "this account may see nothing" and "this request cannot be
// answered" must not look the same at the call site.
func (c *MerchantGRPCClient) ListStoreIDs(ctx context.Context, merchantID, scopeType string, scopeIDs []string) ([]string, error) {
	if err := validateID(merchantID); err != nil {
		return nil, err
	}
	// 每一个目标都要过这一关，不是只查第一个：一个不合法的 id 混在中间会让对端拿它去
	// 查库，而这一层要挡的正是「本就不可能存在的 id」。
	for _, id := range scopeIDs {
		if err := validateID(id); err != nil {
			return nil, err
		}
	}
	ctx, cancel := c.call(ctx)
	defer cancel()
	resp, err := c.merchants.ListStoreIDs(ctx, &merchantv1.ListStoreIDsRequest{
		MerchantId: merchantID,
		ScopeType:  scopeType,
		ScopeIds:   scopeIDs,
	})
	if err != nil {
		return nil, err
	}
	return resp.GetStoreIds(), nil
}

func (c *MerchantGRPCClient) FindStoreMerchantID(ctx context.Context, id string) (string, error) {
	if err := validateID(id); err != nil {
		return "", err
	}
	ctx, cancel := c.call(ctx)
	defer cancel()
	resp, err := c.merchants.GetStoreMerchant(ctx, &merchantv1.GetStoreMerchantRequest{StoreId: id})
	if err != nil {
		return "", notFound(err)
	}
	merchantID := resp.GetMerchantId()
	if strings.TrimSpace(merchantID) == "" {
		return "", ErrInvalidResponse
	}
	return merchantID, nil
}
