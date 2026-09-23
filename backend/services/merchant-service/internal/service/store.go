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
	MerchantID   string
	BrandID      string
	Name         string
	Logo         string
	Photos       []string
	Province     string
	City         string
	District     string
	ProvinceCode string
	CityCode     string
	DistrictCode string
	// 订货系统 xlsx「客户」页的三列（migrations/merchant）；编码是对账键，两个编码各有
	// 「空串不参与」的部分唯一索引，冲突由库拦下、经 repository.storeCodeConflict 翻成人话
	// （**本服务没有 mapPGError**，那是 order/payment 那几个服务的写法，别 grep 错）。
	CustomerCode  string
	DMSCode       string
	CustomerType  string
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
	// tx 让「门店写入 + 待审核记录」落在同一次提交里，见 Create / Audit。
	tx repository.Transactor
}

func NewAdminStoreService(stores repository.StoreRepository, brands repository.BrandRepository, merchants repository.MerchantRepository, audits repository.AuditRecordRepository, users repository.MerchantUserRepository, tx repository.Transactor) *AdminStoreService {
	return &AdminStoreService{stores: stores, brands: brands, merchants: merchants, audits: audits, users: users, tx: tx}
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
		CustomerCode:  in.CustomerCode,
		DMSCode:       in.DMSCode,
		CustomerType:  in.CustomerType,
		Status:        "active",
		AuditStatus:   "pending",
		Remark:        in.Remark,
		Visible:       in.Visible,
		CreatedBy:     operator,
		CreatedAt:     now,
		UpdatedAt:     now,
	}
	// 门店与它的待审核记录必须一次提交：分开提交时进程死在中间，列表里就多出一个
	// 永远不进待审队列的门店——它看着正常，却再也不会被审。
	rec := &model.AuditRecord{
		ID:          uuid.NewString(),
		TargetID:    st.ID,
		Type:        "create",
		Status:      "pending",
		NewData:     in.snapshot(),
		SubmittedBy: operator,
		CreatedAt:   now,
	}
	if err := s.tx.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		if err := s.stores.CreateInTx(ctx, tx, st); err != nil {
			return err
		}
		return s.audits.CreateInTx(ctx, tx, rec)
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
	// 归属只认库上现有的 merchant_id，品牌则按请求里的 brand_id 校验——两者必须对齐，
	// 而且证明只能来自「门店现在属于谁」，不能来自请求带上来的那个商户 ID。
	//
	// 修之前这里拿 in.MerchantID 去核对品牌：请求带 M2 的 merchantId 与 M2 的品牌，
	// 校验自然通过，而 repository 的 UPDATE 从来不写 merchant_id，落库后
	// stores.merchant_id 仍是 M1、指向的品牌却属于 M2。库上只有 merchant_id 与
	// brand_id 两个**单列**外键，各自都成立，没有复合约束兜底，任何查询都不会报错。
	//
	// 现在 UPDATE 依然不写 merchant_id，而 checkBrand 只认 old.MerchantID，于是
	// 「门店归属」与「品牌归属」在写入前后只能是同一个值：Create 也是先 checkBrand
	// (in.MerchantID, in.BrandID) 再把两者一起写进去，两条写路径都无法让它们分叉。
	// in.MerchantID 与库上不一致时直接拒绝而不是静默忽略——静默忽略会让调用方以为
	// 改成功了，而这个服务并不支持换商户（后台 UI 把商户选择框禁掉了）。
	if in.MerchantID != "" && in.MerchantID != old.MerchantID {
		return nil, ErrStoreMerchantImmutable
	}
	if err := s.checkBrand(ctx, old.MerchantID, in.BrandID); err != nil {
		return nil, err
	}
	oldSnapshot := StoreInput{
		MerchantID: old.MerchantID, BrandID: old.BrandID, Name: old.Name, Logo: old.Logo,
		Photos: old.Photos, Province: old.Province, City: old.City, District: old.District,
		ProvinceCode: old.ProvinceCode, CityCode: old.CityCode, DistrictCode: old.DistrictCode,
		Address: old.Address, Longitude: old.Longitude, Latitude: old.Latitude,
		Phone: old.Phone, ContactName: old.ContactName, ContactPhone: old.ContactPhone,
		Detail: old.Detail, BusinessHours: old.BusinessHours, Remark: old.Remark, Visible: old.Visible,
		CustomerCode: old.CustomerCode, DMSCode: old.DMSCode, CustomerType: old.CustomerType,
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
	old.CustomerCode = in.CustomerCode
	old.DMSCode = in.DMSCode
	old.CustomerType = in.CustomerType
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
	// 这一次读没有锁，只是把绝大多数重复点击挡在事务之外；并发下的权威判定在
	// SetAuditInTx 的 WHERE audit_status = 'pending' 上（0 行 → ErrAuditNotPending）。
	if st.AuditStatus != "pending" {
		return repository.ErrAuditNotPending
	}
	status := "rejected"
	if approve {
		status = "approved"
	}
	// 门店与审核记录一起提交：分开提交时，第一次提交失败会留下一个「审核状态已改、
	// 待审记录还挂着」的门店，列表上永远显示一条待办，点进去再审核又会被谓词拦下。
	return s.tx.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		if err := s.stores.SetAuditInTx(ctx, tx, id, status, remark, operator); err != nil {
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
