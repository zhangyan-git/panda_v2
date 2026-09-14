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

var (
	ErrBrandNameRequired  = errors.New("品牌名称不能为空")
	ErrStoreNameRequired  = errors.New("门店名称不能为空")
	ErrBrandHasStores     = errors.New("品牌下存在门店，无法删除")
	ErrStoreBrandMismatch = errors.New("品牌不属于该商户")
	ErrAuditNotPending    = errors.New("当前审核状态不可审核")
)

// BrandInput 品牌创建/编辑的可编辑字段
type BrandInput struct {
	MerchantID  string
	Name        string
	Logo        string
	Banner      string
	Description string
	Remark      string
	Visible     bool
	Sort        int
}

func (i BrandInput) snapshot() string {
	raw, _ := json.Marshal(i)
	return string(raw)
}

// AdminBrandService 平台侧品牌管理：CRUD + 状态 + 审核
type AdminBrandService struct {
	brands    repository.BrandRepository
	merchants repository.MerchantRepository
	audits    repository.AuditRecordRepository
	users     repository.MerchantUserRepository
}

func NewAdminBrandService(brands repository.BrandRepository, merchants repository.MerchantRepository, audits repository.AuditRecordRepository, users repository.MerchantUserRepository) *AdminBrandService {
	return &AdminBrandService{brands: brands, merchants: merchants, audits: audits, users: users}
}

// List 返回一页品牌及其总数，筛选条件由调用方在 f 里给定。
func (s *AdminBrandService) List(ctx context.Context, f repository.BrandFilter, page, pageSize int) ([]*model.Brand, int64, error) {
	list, err := s.brands.FindPage(ctx, f, pageSize, api.PageOffset(page, pageSize))
	if err != nil {
		return nil, 0, err
	}
	total, err := s.brands.Count(ctx, f)
	if err != nil {
		return nil, 0, err
	}
	return list, total, nil
}

func (s *AdminBrandService) GetByID(ctx context.Context, id string) (*model.Brand, error) {
	return s.brands.FindByID(ctx, id)
}

// Create 平台新建品牌：状态 active、审核状态 pending，并落一条待审核记录
func (s *AdminBrandService) Create(ctx context.Context, in BrandInput, operator string) (*model.Brand, error) {
	if in.Name == "" {
		return nil, ErrBrandNameRequired
	}
	if _, err := s.merchants.FindByID(ctx, in.MerchantID); err != nil {
		return nil, err
	}
	now := time.Now()
	b := &model.Brand{
		ID:          uuid.NewString(),
		MerchantID:  in.MerchantID,
		Name:        in.Name,
		Logo:        in.Logo,
		Banner:      in.Banner,
		Description: in.Description,
		Status:      "active",
		AuditStatus: "pending",
		Remark:      in.Remark,
		Visible:     in.Visible,
		Sort:        in.Sort,
		CreatedBy:   operator,
		CreatedAt:   now,
		UpdatedAt:   now,
	}
	if err := s.brands.Create(ctx, b); err != nil {
		return nil, err
	}
	if err := s.audits.Create(ctx, &model.AuditRecord{
		ID:          uuid.NewString(),
		TargetID:    b.ID,
		Type:        "create",
		Status:      "pending",
		NewData:     in.snapshot(),
		SubmittedBy: operator,
		CreatedAt:   now,
	}); err != nil {
		return nil, err
	}
	return b, nil
}

// Update 平台直接编辑并生效，同时落一条已通过的修改记录留痕
func (s *AdminBrandService) Update(ctx context.Context, id string, in BrandInput, operator string) (*model.Brand, error) {
	if in.Name == "" {
		return nil, ErrBrandNameRequired
	}
	old, err := s.brands.FindByID(ctx, id)
	if err != nil {
		return nil, err
	}
	oldSnapshot := BrandInput{
		MerchantID: old.MerchantID, Name: old.Name, Logo: old.Logo, Banner: old.Banner,
		Description: old.Description, Remark: old.Remark, Visible: old.Visible, Sort: old.Sort,
	}
	old.Name = in.Name
	old.Logo = in.Logo
	old.Banner = in.Banner
	old.Description = in.Description
	old.Remark = in.Remark
	old.Visible = in.Visible
	old.Sort = in.Sort
	old.UpdatedAt = time.Now()
	if err := s.brands.Update(ctx, old); err != nil {
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

func (s *AdminBrandService) UpdateStatus(ctx context.Context, id, status string) error {
	if _, err := s.brands.FindByID(ctx, id); err != nil {
		return err
	}
	return s.brands.UpdateStatus(ctx, id, status)
}

// Audit 审核：仅 pending 可通过或拒绝
func (s *AdminBrandService) Audit(ctx context.Context, id string, approve bool, remark, operator string) error {
	b, err := s.brands.FindByID(ctx, id)
	if err != nil {
		return err
	}
	if b.AuditStatus != "pending" {
		return ErrAuditNotPending
	}
	status := "rejected"
	if approve {
		status = "approved"
	}
	if err := s.brands.SetAudit(ctx, id, status, remark, operator); err != nil {
		return err
	}
	if rec, err := s.audits.FindLatestPending(ctx, id); err == nil && rec != nil {
		return s.audits.SetAudited(ctx, rec.ID, status, remark, operator)
	} else if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		// 审核记录查询失败时应返回错误，避免实体与审核记录状态不一致
		return err
	}
	return nil
}

// Delete 删除品牌；名下有门店时拒绝，指向它的账号范围回收为商户级
func (s *AdminBrandService) Delete(ctx context.Context, id string) error {
	if _, err := s.brands.FindByID(ctx, id); err != nil {
		return err
	}
	hasStores, err := s.brands.HasStores(ctx, id)
	if err != nil {
		return err
	}
	if hasStores {
		return ErrBrandHasStores
	}
	if err := s.users.ResetScopeByTarget(ctx, "brand", id); err != nil {
		return err
	}
	return s.brands.Delete(ctx, id)
}
