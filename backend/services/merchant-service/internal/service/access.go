package service

import (
	"context"

	"github.com/panda-dev/panda-v2/backend/services/merchant-service/internal/model"
	"github.com/panda-dev/panda-v2/backend/services/merchant-service/internal/repository"
)

// MerchantAccessService exposes only the master-data facts needed by account services.
type MerchantAccessService struct {
	merchants repository.MerchantRepository
	brands    repository.BrandRepository
	stores    repository.StoreRepository
}

func NewMerchantAccessService(merchants repository.MerchantRepository, brands repository.BrandRepository, stores repository.StoreRepository) *MerchantAccessService {
	return &MerchantAccessService{merchants: merchants, brands: brands, stores: stores}
}

func (s *MerchantAccessService) FindStatus(ctx context.Context, id string) (string, error) {
	merchant, err := s.merchants.FindByID(ctx, id)
	if err != nil {
		return "", err
	}
	return merchant.Status, nil
}

func (s *MerchantAccessService) FindName(ctx context.Context, id string) (string, error) {
	merchant, err := s.merchants.FindByID(ctx, id)
	if err != nil {
		return "", err
	}
	return merchant.Name, nil
}

func (s *MerchantAccessService) FindBrandMerchantID(ctx context.Context, id string) (string, error) {
	brand, err := s.brands.FindByID(ctx, id)
	if err != nil {
		return "", err
	}
	return brand.MerchantID, nil
}

func (s *MerchantAccessService) FindStoreMerchantID(ctx context.Context, id string) (string, error) {
	store, err := s.stores.FindByID(ctx, id)
	if err != nil {
		return "", err
	}
	return store.MerchantID, nil
}

// FindStore 返回门店本身，而不只是它属于谁。
//
// 调用方是 coffee-machine-service：它要在把设备挂到某个点位上之前确认这个点位真实存在
// 且当前可用，而「谁的」回答不了「能不能用」。两者都留着而不是把 FindStoreMerchantID
// 折成 FindStore：user-service 只要归属，让它拖回一整行是替它做了主。
func (s *MerchantAccessService) FindStore(ctx context.Context, id string) (*model.Store, error) {
	return s.stores.FindByID(ctx, id)
}

// StoreIDsByScope 把一个商户账号的数据范围展开成一组点位 id。
//
// 展开放在商户服务而不是各消费方，是因为只有这里知道「品牌档」在这张表上意味着什么。
// 下游的设备表与订单表都只持有 store_id，拿到一组平板 id 就能过滤，不必各自重新
// 解释一次范围语义——那种重复解释正是三处实现慢慢分叉的起点。
func (s *MerchantAccessService) StoreIDsByScope(ctx context.Context, merchantID, scopeType, scopeID string) ([]string, error) {
	return s.stores.FindIDsByScope(ctx, merchantID, scopeType, scopeID)
}

// ScopeNames resolves brand and store ids to display names. It backs the scope
// column of a merchant account listing, which used to come from a join the
// identity database can no longer make. Missing ids are simply absent from the
// two maps: a deleted scope leaves the account readable.
func (s *MerchantAccessService) ScopeNames(ctx context.Context, brandIDs, storeIDs []string) (map[string]string, map[string]string, error) {
	brandNames, err := s.brands.FindNames(ctx, brandIDs)
	if err != nil {
		return nil, nil, err
	}
	storeNames, err := s.stores.FindNames(ctx, storeIDs)
	if err != nil {
		return nil, nil, err
	}
	return brandNames, storeNames, nil
}
