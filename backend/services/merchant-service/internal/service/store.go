package service

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/panda-dev/panda-v2/backend/platform/api"
	"github.com/panda-dev/panda-v2/backend/services/merchant-service/internal/model"
	"github.com/panda-dev/panda-v2/backend/services/merchant-service/internal/repository"
)

// StoreInput 门店创建/编辑的可编辑字段
type StoreInput struct {
	MerchantID    string
	BrandID       string
	Name          string
	Logo          string
	Photos        []string
	Province      string
	City          string
	District      string
	ProvinceCode  string
	CityCode      string
	DistrictCode  string
	Address       string
	Longitude     *float64
	Latitude      *float64
	Phone         string
	ContactName   string
	ContactPhone  string
	Detail        string
	BusinessHours string
	Remark        string
	Visible       bool
}

func (i StoreInput) snapshot() string {
	raw, _ := json.Marshal(i)
	return string(raw)
}

// AdminStoreService 平台侧门店管理：CRUD + 状态 + 审核
type AdminStoreService struct {
	stores    repository.StoreRepository
	brands    repository.BrandRepository
	merchants repository.MerchantRepository
	audits    repository.AuditRecordRepository
	users     repository.MerchantUserRepository
}

func NewAdminStoreService(stores repository.StoreRepository, brands repository.BrandRepository, merchants repository.MerchantRepository, audits repository.AuditRecordRepository, users repository.MerchantUserRepository) *AdminStoreService {
	return &AdminStoreService{stores: stores, brands: brands, merchants: merchants, audits: audits, users: users}
}

// List 返回一页门店及其总数，筛选条件由调用方在 f 里给定。
func (s *AdminStoreService) List(ctx context.Context, f repository.StoreFilter, page, pageSize int) ([]*model.Store, int64, error) {
	list, err := s.stores.FindPage(ctx, f, pageSize, api.PageOffset(page, pageSize))
	if err != nil {
		return nil, 0, err
	}
	total, err := s.stores.Count(ctx, f)
	if err != nil {
		return nil, 0, err
	}
	return list, total, nil
}

func (s *AdminStoreService) GetByID(ctx context.Context, id string) (*model.Store, error) {
	return s.stores.FindByID(ctx, id)
}

// checkBrand 门店的品牌必须存在且属于同一商户
func (s *AdminStoreService) checkBrand(ctx context.Context, merchantID, brandID string) error {
	brand, err := s.brands.FindByID(ctx, brandID)
	if err != nil {
		return err
	}
	if brand.MerchantID != merchantID {
		return ErrStoreBrandMismatch
	}
	return nil
}

// Create 平台新建门店：状态 active、审核状态 pending，并落一条待审核记录
func (s *AdminStoreService) Create(ctx context.Context, in StoreInput, operator string) (*model.Store, error) {
	if in.Name == "" {
		return nil, ErrStoreNameRequired
	}
	if _, err := s.merchants.FindByID(ctx, in.MerchantID); err != nil {
		return nil, err
	}
	if err := s.checkBrand(ctx, in.MerchantID, in.BrandID); err != nil {
		return nil, err
	}
	now := time.Now()
	st := &model.Store{
		ID:            uuid.NewString(),
		MerchantID:    in.MerchantID,
		BrandID:       in.BrandID,
		Name:          in.Name,
		Logo:          in.Logo,
		Photos:        in.Photos,
		Province:      in.Province,
		City:          in.City,
		District:      in.District,
		ProvinceCode:  in.ProvinceCode,
		CityCode:      in.CityCode,
		DistrictCode:  in.DistrictCode,
		Address:       in.Address,
		Longitude:     in.Longitude,
		Latitude:      in.Latitude,
		Phone:         in.Phone,
		ContactName:   in.ContactName,
		ContactPhone:  in.ContactPhone,
		Detail:        in.Detail,
		BusinessHours: in.BusinessHours,
		Status:        "active",
		AuditStatus:   "pending",
		Remark:        in.Remark,
		Visible:       in.Visible,
		CreatedBy:     operator,
		CreatedAt:     now,
		UpdatedAt:     now,
	}
	if err := s.stores.Create(ctx, st); err != nil {
		return nil, err
	}
	if err := s.audits.Create(ctx, &model.AuditRecord{
		ID:          uuid.NewString(),
		TargetID:    st.ID,
		Type:        "create",
		Status:      "pending",
		NewData:     in.snapshot(),
		SubmittedBy: operator,
		CreatedAt:   now,
	}); err != nil {
		return nil, err
	}
	return st, nil
}

// Update 平台直接编辑并生效，同时落一条已通过的修改记录留痕
func (s *AdminStoreService) Update(ctx context.Context, id string, in StoreInput, operator string) (*model.Store, error) {
	if in.Name == "" {
		return nil, ErrStoreNameRequired
	}
	old, err := s.stores.FindByID(ctx, id)
	if err != nil {
		return nil, err
	}
	if err := s.checkBrand(ctx, in.MerchantID, in.BrandID); err != nil {
		return nil, err
	}
	oldSnapshot := StoreInput{
		MerchantID: old.MerchantID, BrandID: old.BrandID, Name: old.Name, Logo: old.Logo,
		Photos: old.Photos, Province: old.Province, City: old.City, District: old.District,
		ProvinceCode: old.ProvinceCode, CityCode: old.CityCode, DistrictCode: old.DistrictCode,
		Address: old.Address, Longitude: old.Longitude, Latitude: old.Latitude,
		Phone: old.Phone, ContactName: old.ContactName, ContactPhone: old.ContactPhone,
		Detail: old.Detail, BusinessHours: old.BusinessHours, Remark: old.Remark, Visible: old.Visible,
	}
	old.BrandID = in.BrandID
	old.Name = in.Name
	old.Logo = in.Logo
	old.Photos = in.Photos
	old.Province = in.Province
	old.City = in.City
	old.District = in.District
	old.ProvinceCode = in.ProvinceCode
	old.CityCode = in.CityCode
	old.DistrictCode = in.DistrictCode
	old.Address = in.Address
	old.Longitude = in.Longitude
	old.Latitude = in.Latitude
	old.Phone = in.Phone
	old.ContactName = in.ContactName
	old.ContactPhone = in.ContactPhone
	old.Detail = in.Detail
	old.BusinessHours = in.BusinessHours
	old.Remark = in.Remark
	old.Visible = in.Visible
	old.UpdatedAt = time.Now()
	if err := s.stores.Update(ctx, old); err != nil {
		return nil, err
	}
	now := time.Now()
	if err := s.audits.Create(ctx, &model.AuditRecord{
		ID:          uuid.NewString(),
		TargetID:    id,
		Type:        "update",
		Status:      "approved",
		OldData:     oldSnapshot.snapshot(),
		NewData:     in.snapshot(),
		AuditRemark: "平台直接编辑",
		AuditBy:     operator,
		AuditAt:     &now,
		SubmittedBy: operator,
		CreatedAt:   now,
	}); err != nil {
		return nil, err
	}
	return old, nil
}

func (s *AdminStoreService) UpdateStatus(ctx context.Context, id, status string) error {
	if _, err := s.stores.FindByID(ctx, id); err != nil {
		return err
	}
	return s.stores.UpdateStatus(ctx, id, status)
}

// Audit 审核：仅 pending 可通过或拒绝
func (s *AdminStoreService) Audit(ctx context.Context, id string, approve bool, remark, operator string) error {
	st, err := s.stores.FindByID(ctx, id)
	if err != nil {
		return err
	}
	if st.AuditStatus != "pending" {
		return ErrAuditNotPending
	}
	status := "rejected"
	if approve {
		status = "approved"
	}
	if err := s.stores.SetAudit(ctx, id, status, remark, operator); err != nil {
		return err
	}
	if rec, err := s.audits.FindLatestPending(ctx, id); err == nil && rec != nil {
		return s.audits.SetAudited(ctx, rec.ID, status, remark, operator)
	} else if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	return nil
}

// Delete 删除门店；指向它的账号范围回收为商户级
func (s *AdminStoreService) Delete(ctx context.Context, id string) error {
	if _, err := s.stores.FindByID(ctx, id); err != nil {
		return err
	}
	if err := s.users.ResetScopeByTarget(ctx, "store", id); err != nil {
		return err
	}
	return s.stores.Delete(ctx, id)
}
