// Package rpc implements merchant-service's gRPC surface. The package is named
// rpc rather than grpc so that it does not shadow google.golang.org/grpc.
package rpc

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/panda-dev/panda-v2/backend/platform/auth"
	"github.com/panda-dev/panda-v2/backend/services/merchant-service/internal/model"
	merchantv1 "github.com/panda-dev/panda-v2/contracts/proto/merchant/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// merchantReader is the narrow view of the merchant service the RPCs need.
type merchantReader interface {
	GetByID(ctx context.Context, id string) (*model.Merchant, error)
}

// ownershipReader resolves brand and store references: which merchant owns one,
// what a batch of them is called, and what a store looks like right now.
type ownershipReader interface {
	FindBrandMerchantID(ctx context.Context, id string) (string, error)
	FindStoreMerchantID(ctx context.Context, id string) (string, error)
	FindStore(ctx context.Context, id string) (*model.Store, error)
	ScopeNames(ctx context.Context, brandIDs, storeIDs []string) (map[string]string, map[string]string, error)
	StoreIDsByScope(ctx context.Context, merchantID, scopeType, scopeID string) ([]string, error)
}

// MerchantService serves the internal calls user-service makes: the merchant
// status/name lookup and the brand/store ownership resolution that used to be
// internal HTTP routes.
//
// Only the lookup RPCs are implemented. Merchant CRUD, brand/store listing, and
// the merchant self-service reads stay HTTP-only surfaces; they fall through to
// the embedded Unimplemented server and answer codes.Unimplemented.
type MerchantService struct {
	merchantv1.UnimplementedMerchantServiceServer

	merchants merchantReader
	access    ownershipReader
}

func NewMerchantService(merchants merchantReader, access ownershipReader) *MerchantService {
	return &MerchantService{merchants: merchants, access: access}
}

// GetMerchant carries both facts the retired /merchants/{id}/status and
// /merchants/{id}/name routes returned.
func (s *MerchantService) GetMerchant(ctx context.Context, req *merchantv1.GetMerchantRequest) (*merchantv1.GetMerchantResponse, error) {
	if err := auth.RequireService(ctx); err != nil {
		return nil, err
	}
	m, err := s.merchants.GetByID(ctx, req.GetId())
	if err != nil {
		return nil, resourceError(err)
	}
	return &merchantv1.GetMerchantResponse{Merchant: &merchantv1.Merchant{
		Id:           m.ID,
		Name:         m.Name,
		ContactName:  m.ContactName,
		ContactPhone: m.ContactPhone,
		Status:       m.Status,
		StatusCode:   merchantStatusCode(m.Status),
	}}, nil
}

func (s *MerchantService) GetBrandMerchant(ctx context.Context, req *merchantv1.GetBrandMerchantRequest) (*merchantv1.GetBrandMerchantResponse, error) {
	if err := auth.RequireService(ctx); err != nil {
		return nil, err
	}
	id, err := s.access.FindBrandMerchantID(ctx, req.GetBrandId())
	if err != nil {
		return nil, resourceError(err)
	}
	return &merchantv1.GetBrandMerchantResponse{MerchantId: id}, nil
}

func (s *MerchantService) GetStoreMerchant(ctx context.Context, req *merchantv1.GetStoreMerchantRequest) (*merchantv1.GetStoreMerchantResponse, error) {
	if err := auth.RequireService(ctx); err != nil {
		return nil, err
	}
	id, err := s.access.FindStoreMerchantID(ctx, req.GetStoreId())
	if err != nil {
		return nil, resourceError(err)
	}
	return &merchantv1.GetStoreMerchantResponse{MerchantId: id}, nil
}

// GetStore returns one store, including the status fields that decide whether a
// device may be deployed there. A missing store is NotFound, mapped by the same
// resourceError the other lookups use, so callers get one rule for "this id
// points at nothing".
func (s *MerchantService) GetStore(ctx context.Context, req *merchantv1.GetStoreRequest) (*merchantv1.GetStoreResponse, error) {
	if err := auth.RequireService(ctx); err != nil {
		return nil, err
	}
	store, err := s.access.FindStore(ctx, req.GetStoreId())
	if err != nil {
		return nil, resourceError(err)
	}
	return &merchantv1.GetStoreResponse{Store: storeToProto(store)}, nil
}

// ResolveScopeNames answers the scope column of a merchant account listing. An
// absent id is not an error here — the account stays listable with an empty
// scope name — so this deliberately does not go through resourceError.
func (s *MerchantService) ResolveScopeNames(ctx context.Context, req *merchantv1.ResolveScopeNamesRequest) (*merchantv1.ResolveScopeNamesResponse, error) {
	if err := auth.RequireService(ctx); err != nil {
		return nil, err
	}
	brandNames, storeNames, err := s.access.ScopeNames(ctx, req.GetBrandIds(), req.GetStoreIds())
	if err != nil {
		return nil, status.Error(codes.Internal, "merchant service error")
	}
	return &merchantv1.ResolveScopeNamesResponse{BrandNames: brandNames, StoreNames: storeNames}, nil
}

// ListStoreIDs expands a merchant account's data scope into the set of stores it
// authorizes. user-service calls it while resolving GetMerchantAccess, and the
// answer becomes the boundary every downstream service filters on.
//
// The request is validated rather than trusted: an unknown scope type or a
// brand/store level without a scope id is InvalidArgument, not a widened or
// empty answer. Both mistakes would otherwise surface downstream as "this
// account can see nothing", which is indistinguishable from a legitimate empty
// scope and would be debugged in the wrong service.
func (s *MerchantService) ListStoreIDs(ctx context.Context, req *merchantv1.ListStoreIDsRequest) (*merchantv1.ListStoreIDsResponse, error) {
	if err := auth.RequireService(ctx); err != nil {
		return nil, err
	}
	merchantID := req.GetMerchantId()
	if merchantID == "" {
		return nil, status.Error(codes.InvalidArgument, "merchant_id is required")
	}
	scopeType := req.GetScopeType()
	scopeID := req.GetScopeId()
	switch scopeType {
	case auth.ScopeTypeMerchant:
		// 商户档的 scope_id 应当为空。带上一个却没被用上，说明调用方对范围的
		// 理解和这里不一致，宁可报错也不要静默按商户全量返回。
		if scopeID != "" {
			return nil, status.Error(codes.InvalidArgument, "scope_id must be empty for merchant scope")
		}
	case auth.ScopeTypeBrand, auth.ScopeTypeStore:
		if scopeID == "" {
			return nil, status.Error(codes.InvalidArgument, "scope_id is required for brand and store scope")
		}
	default:
		return nil, status.Error(codes.InvalidArgument, "unknown scope_type")
	}
	ids, err := s.access.StoreIDsByScope(ctx, merchantID, scopeType, scopeID)
	if err != nil {
		// 认不出的档位在上面已经挡掉，走到这里的只可能是存储故障。
		return nil, status.Error(codes.Internal, "merchant service error")
	}
	if ids == nil {
		// 空集是一个正常答案（这个品牌名下确实没有点位），这里把它写成明确的空切片，
		// 让进程内调用方拿到的是一个良构的答案。
		//
		// 但这道归一化**不构成**下游的安全边界：proto3 的 repeated 字段零元素不
		// 上线，走到 gRPC 对端又会被解码回 nil。真正承担 nil→空切片的是
		// auth.WithStoreScope（见 platform/auth/store_scope.go），它在中间件里
		// 把范围放上 context 时收口，任何消费者读到的 StoreIDs 都非 nil。
		ids = []string{}
	}
	return &merchantv1.ListStoreIDsResponse{StoreIds: ids}, nil
}

// resourceError maps a missing target to NotFound: user-service's client keys
// on exactly that code to keep the pgx.ErrNoRows semantics it had while reading
// merchant-service over HTTP. Everything else stays opaque so storage error
// text never crosses the service boundary.
func resourceError(err error) error {
	if errors.Is(err, pgx.ErrNoRows) {
		return status.Error(codes.NotFound, "merchant resource not found")
	}
	return status.Error(codes.Internal, "merchant service error")
}

// storeToProto carries the store columns the internal RPCs expose. Both the
// string and the enum form of each status travel together, for the same reason
// they do on Merchant: the string is what the columns hold and what older
// readers switch on, the enum is what new readers should use.
func storeToProto(store *model.Store) *merchantv1.Store {
	if store == nil {
		return nil
	}
	return &merchantv1.Store{
		Id:              store.ID,
		MerchantId:      store.MerchantID,
		BrandId:         store.BrandID,
		Name:            store.Name,
		Logo:            store.Logo,
		Phone:           store.Phone,
		Province:        store.Province,
		City:            store.City,
		District:        store.District,
		Address:         store.Address,
		BusinessHours:   store.BusinessHours,
		Longitude:       store.Longitude,
		Latitude:        store.Latitude,
		Status:          store.Status,
		StatusCode:      storeStatusCode(store.Status),
		Visible:         store.Visible,
		AuditStatus:     store.AuditStatus,
		AuditStatusCode: storeAuditStatusCode(store.AuditStatus),
	}
}

// storeStatusCode mirrors the additive enum form of Store.status.
func storeStatusCode(status string) merchantv1.ResourceStatus {
	switch status {
	case "active":
		return merchantv1.ResourceStatus_RESOURCE_STATUS_ACTIVE
	case "disabled":
		return merchantv1.ResourceStatus_RESOURCE_STATUS_DISABLED
	default:
		return merchantv1.ResourceStatus_RESOURCE_STATUS_UNSPECIFIED
	}
}

// storeAuditStatusCode mirrors the additive enum form of Store.audit_status.
func storeAuditStatusCode(auditStatus string) merchantv1.AuditStatus {
	switch auditStatus {
	case "pending":
		return merchantv1.AuditStatus_AUDIT_STATUS_PENDING
	case "approved":
		return merchantv1.AuditStatus_AUDIT_STATUS_APPROVED
	case "rejected":
		return merchantv1.AuditStatus_AUDIT_STATUS_REJECTED
	default:
		return merchantv1.AuditStatus_AUDIT_STATUS_UNSPECIFIED
	}
}

// merchantStatusCode mirrors the additive enum form of Merchant.status.
func merchantStatusCode(status string) merchantv1.MerchantStatus {
	switch status {
	case "pending":
		return merchantv1.MerchantStatus_MERCHANT_STATUS_PENDING
	case "active":
		return merchantv1.MerchantStatus_MERCHANT_STATUS_ACTIVE
	case "suspended":
		return merchantv1.MerchantStatus_MERCHANT_STATUS_SUSPENDED
	default:
		return merchantv1.MerchantStatus_MERCHANT_STATUS_UNSPECIFIED
	}
}
