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
	// ErrStoreMerchantImmutable 编辑门店的请求带了一个与库上不同的商户 ID。门店换商户
	// 不是这个接口支持的动作（后台 UI 把商户选择框禁掉了），静默忽略会让调用方以为
	// 改成功了，所以直接拒绝。
	ErrStoreMerchantImmutable = errors.New("门店所属商户不可变更")
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
	// tx 让「品牌写入 + 待审核记录」落在同一次提交里，见 Create / Audit。
	tx repository.Transactor
}

func NewAdminBrandService(brands repository.BrandRepository, merchants repository.MerchantRepository, audits repository.AuditRecordRepository, users repository.MerchantUserRepository, tx repository.Transactor) *AdminBrandService {
	return &AdminBrandService{brands: brands, merchants: merchants, audits: audits, users: users, tx: tx}
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
	// 品牌与它的待审核记录必须一次提交：分开提交时进程死在中间，列表里就多出一个
	// 永远不进待审队列的品牌——它看着正常，却再也不会被审。
	rec := &model.AuditRecord{
		ID:          uuid.NewString(),
		TargetID:    b.ID,
		Type:        "create",
		Status:      "pending",
		NewData:     in.snapshot(),
		SubmittedBy: operator,
		CreatedAt:   now,
	}
	if err := s.tx.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		if err := s.brands.CreateInTx(ctx, tx, b); err != nil {
			return err
		}
		return s.audits.CreateInTx(ctx, tx, rec)
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
	// 这一次读没有锁，只是把绝大多数重复点击挡在事务之外；并发下的权威判定在
	// SetAuditInTx 的 WHERE audit_status = 'pending' 上（0 行 → ErrAuditNotPending）。
	if b.AuditStatus != "pending" {
		return repository.ErrAuditNotPending
	}
	status := "rejected"
	if approve {
		status = "approved"
	}
	// 品牌与审核记录一起提交：分开提交时，第一次提交失败会留下一个「审核状态已改、
	// 待审记录还挂着」的品牌，列表上永远显示一条待办，点进去再审核又会被谓词拦下。
	return s.tx.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		if err := s.brands.SetAuditInTx(ctx, tx, id, status, remark, operator); err != nil {
			return err
		}
		rec, err := s.audits.FindLatestPendingInTx(ctx, tx, id)
		if err != nil {
			// 没有待审记录（历史数据）：实体已经落章，不作为失败。
			if errors.Is(err, pgx.ErrNoRows) {
				return nil
			}
			return err
		}
		return s.audits.SetAuditedInTx(ctx, tx, rec.ID, status, remark, operator)
	})
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
