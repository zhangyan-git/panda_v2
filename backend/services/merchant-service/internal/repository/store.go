package repository

import (
	"context"
	"errors"
	"strconv"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/panda-dev/panda-v2/backend/platform/audit"
	"github.com/panda-dev/panda-v2/backend/platform/auth"
	"github.com/panda-dev/panda-v2/backend/services/merchant-service/internal/model"
)

// StoreRepository 门店数据访问接口
type StoreRepository interface {
	// FindPage 返回一页门店；总数走 Count，两者共用 storeConds 拼条件。
	FindPage(ctx context.Context, f StoreFilter, limit, offset int) ([]*model.Store, error)
	Count(ctx context.Context, f StoreFilter) (int64, error)
	FindByID(ctx context.Context, id string) (*model.Store, error)
	// CreateInTx 在调用方的事务里建门店：待审核记录必须与它同一次提交，
	// 所以事务由调用方（service）开，不在这里自己 Begin。
	CreateInTx(ctx context.Context, tx pgx.Tx, s *model.Store) error
	Update(ctx context.Context, s *model.Store) error
	UpdateStatus(ctx context.Context, id, status string) error
	// SetAuditInTx 只允许 pending → approved/rejected：待审核记录也在调用方的事务里落章。
	// 受影响行数为 0 说明这行已经被审过，返回 ErrAuditNotPending。
	SetAuditInTx(ctx context.Context, tx pgx.Tx, id, auditStatus, remark, by string) error
	Delete(ctx context.Context, id string) error
	// FindNames is the store-side counterpart of BrandRepository.FindNames.
	FindNames(ctx context.Context, ids []string) (map[string]string, error)
	// FindIDsByScope 把一个商户账号的数据范围展开成一组点位 id。范围的三个档位
	// 只有这张表知道怎么落到 SQL 上，展开放在这里，下游的每个消费方就都能按一组
	// 平板 id 过滤，不必各自重新解释「品牌档意味着什么」。
	//
	// scopeIDs 是目标**集合**：品牌档可以有好几个品牌，门店档可以有好几家店，展开
	// 出来的是它们的并集；商户档为空。
	FindIDsByScope(ctx context.Context, merchantID, scopeType string, scopeIDs []string) ([]string, error)
}

// StoreFilter 列表过滤条件，零值表示不过滤
type StoreFilter struct {
	MerchantID  string
	BrandID     string
	Name        string
	Status      string
	AuditStatus string
	// StoreIDs 是商户数据范围强制加上的点位集合，它**不是**筛选栏上的一项，语义
	// 与上面几个字段相反：
	//
	//   nil      —— 没有数据范围，不过滤（后台那条路走这里）
	//   非 nil   —— 无条件按这个集合过滤；空切片编码成 '{}'，ANY('{}') 恒假，
	//               于是「一个点位都没授权」得到零行
	//
	// 写成「空即不过滤」是这条链路唯一会把越权伪装成正常的写法：那样一个没被授权
	// 任何点位的账号会看到全部点位。所以这里判的是 nil，不是 len。
	StoreIDs []string
}

// storeSnapshot 理由同 brandSnapshot：photos 这类图片列表不进快照。
// 归属（merchant_id/brand_id）必须记——门店改挂品牌会直接改变它属于谁。
// 客户编码与 DMS 编码**进快照**，这是对区划编码那一批的一次有意加码：快照里只放了
// name/city/address，区划编码没进。理由是这两个编码与前缀那些字段不同——它们是
// **对账键**（见 migrations/merchant），改一次就等于这一行指向了另一个真实客户。不进快照的话，
// 一次「客户编码从 A 改成 B」的修改在审计里会记成 before == after，等于查不出来。
// customer_type 跟着一起放：它和这两个编码在同一张表单上，拆开只会让读日志的人少看一列。
//
// **快照是子集，不是整行**：`Update` 会写的 remark / logo / 区划 / 电话 / 联系人 / 营业时间 /
// 经纬度 / photos 都不在里面。所以「改了什么审计里都看得见」这句话不成立——这里只记归属、
// 识别字段与状态。写下这条边界是因为上面那三列的理由（不进快照就等于查不出来）对 remark
// 那些列**同样成立**，只是这个取舍从建快照起就接受了；不写清楚，读日志的人会以为漏记了。
type storeSnapshot struct {
	MerchantID   string `json:"merchant_id"`
	BrandID      string `json:"brand_id,omitempty"`
	Name         string `json:"name"`
	City         string `json:"city,omitempty"`
	Address      string `json:"address,omitempty"`
	CustomerCode string `json:"customer_code,omitempty"`
	DMSCode      string `json:"dms_code,omitempty"`
	CustomerType string `json:"customer_type,omitempty"`
	Status       string `json:"status"`
	AuditStatus  string `json:"audit_status,omitempty"`
	AuditRemark  string `json:"audit_remark,omitempty"`
	Visible      bool   `json:"visible"`
}

// storeCodeConflict 把两条「空串不参与」的部分唯一索引违例翻成能说给人听的 sentinel。
//
// 认的是**约束名**而不是错误码：23505 只说明「撞了某个唯一约束」，一律翻成「客户编码被占了」
// 会指错输入框。这张表上目前只有这两条业务唯一索引（另加主键），但这里按名字认、不按
// 「反正只有这两条」认——将来加了约束，认不出的那些照旧走 500。一个没读懂的错误不该被
// 冒充成一句像是用户自己造成的提示。
func storeCodeConflict(err error) error {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "23505" {
		return err
	}
	switch pgErr.ConstraintName {
	case "stores_customer_code_unique":
		return ErrCustomerCodeTaken
	case "stores_dms_code_unique":
		return ErrDMSCodeTaken
	}
	return err
}

// selectStoreSnapshot 读锁定行并组装快照；brand_id 可空，归一化为空字符串。
func selectStoreSnapshot(ctx context.Context, tx pgx.Tx, id string) (storeSnapshot, error) {
	const q = `
		SELECT merchant_id, COALESCE(brand_id::text, ''), name, city, address,
			customer_code, dms_code, customer_type,
			status, audit_status, audit_remark, visible
		FROM stores WHERE id = $1 FOR UPDATE`
	var s storeSnapshot
	err := tx.QueryRow(ctx, q, id).Scan(
		&s.MerchantID, &s.BrandID, &s.Name, &s.City, &s.Address,
		&s.CustomerCode, &s.DMSCode, &s.CustomerType,
		&s.Status, &s.AuditStatus, &s.AuditRemark, &s.Visible,
	)
	return s, err
}

type pgStoreRepo struct {
	pool  *pgxpool.Pool
	audit audit.Recorder
}

func NewStoreRepository(pool *pgxpool.Pool, recorder audit.Recorder) StoreRepository {
	if recorder == nil {
		recorder = audit.Noop{}
	}
	return &pgStoreRepo{pool: pool, audit: recorder}
}

// storeColumns 统一 SELECT 列表；merchant_name/brand_name 为联表计算列，仅展示用
const storeColumns = `s.id, s.merchant_id, s.brand_id, s.name, s.logo, s.photos,
	s.province, s.city, s.district, s.province_code, s.city_code, s.district_code,
	s.address, s.longitude, s.latitude,
	s.phone, s.contact_name, s.contact_phone, s.detail, s.business_hours,
	s.status, s.audit_status, s.audit_remark, s.audit_at, s.audit_by,
	s.remark, s.visible, s.created_by, s.created_at, s.updated_at,
	s.customer_code, s.dms_code, s.customer_type,
	COALESCE(m.name, ''), COALESCE(br.name, '')`

// storeConds 拼门店的筛选条件。brandId 必须接在 buildFilter 之后 append——
// buildFilter 是按 len(args) 递增生成占位符编号的，插队就会把编号错位。
func storeConds(f StoreFilter) ([]string, []any) {
	conds, args := buildFilter(f.MerchantID, f.Name, f.Status, f.AuditStatus, "s.")
	if f.BrandID != "" {
		args = append(args, f.BrandID)
		conds = append(conds, "s.brand_id = $"+strconv.Itoa(len(args)))
	}
	// 判 nil 而不是判 len：非 nil 的空切片必须变成一条恒假的条件，见 StoreFilter.StoreIDs。
	if f.StoreIDs != nil {
		args = append(args, f.StoreIDs)
		conds = append(conds, "s.id::text = ANY($"+strconv.Itoa(len(args))+"::text[])")
	}
	return conds, args
}

// FindPage 排序带上 s.id 作决胜位，理由同 brandRepo.FindPage。
func (r *pgStoreRepo) FindPage(ctx context.Context, f StoreFilter, limit, offset int) ([]*model.Store, error) {
	conds, args := storeConds(f)
	q := `SELECT ` + storeColumns + `
		FROM stores s
		LEFT JOIN merchants m ON m.id = s.merchant_id
		LEFT JOIN brands br ON br.id = s.brand_id` + whereClause(conds) +
		` ORDER BY s.created_at DESC, s.id LIMIT $` + strconv.Itoa(len(args)+1) + ` OFFSET $` + strconv.Itoa(len(args)+2)
	rows, err := r.pool.Query(ctx, q, append(args, limit, offset)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var list []*model.Store
	for rows.Next() {
		s, err := scanStore(rows)
		if err != nil {
			return nil, err
		}
		list = append(list, s)
	}
	return list, rows.Err()
}

// Count 不 JOIN merchants/brands：筛选条件全部落在 s.* 上，理由同 brandRepo.Count。
func (r *pgStoreRepo) Count(ctx context.Context, f StoreFilter) (int64, error) {
	conds, args := storeConds(f)
	var n int64
	err := r.pool.QueryRow(ctx, `SELECT count(*) FROM stores s`+whereClause(conds), args...).Scan(&n)
	return n, err
}

func (r *pgStoreRepo) FindByID(ctx context.Context, id string) (*model.Store, error) {
	q := `SELECT ` + storeColumns + `
		FROM stores s
		LEFT JOIN merchants m ON m.id = s.merchant_id
		LEFT JOIN brands br ON br.id = s.brand_id
		WHERE s.id = $1 LIMIT 1`
	return scanStore(r.pool.QueryRow(ctx, q, id))
}

func (r *pgStoreRepo) FindNames(ctx context.Context, ids []string) (map[string]string, error) {
	return findNames(ctx, r.pool, "stores", ids)
}

// ErrScopeTypeUnknown 是数据范围里认不出的档位。
//
// 它必须是错误而不是空集：空集是一个**正常答案**（这个品牌确实没有门店），
// 让认不出的档位混进同一个答案里，调用方会把一次配置错误读成「这个账号名下没有点位」
// 并照常放行。
var ErrScopeTypeUnknown = errors.New("unknown store scope type")

// FindIDsByScope 把一个商户账号的数据范围展开成一组点位 id。
//
// merchant_id 是**每一条分支的锚点**，不是可选的附加条件：品牌档拿到的是调用方给来的
// scope_ids，而那些是值引用。少了这个锚点，A 商户的账号只要拿到 B 商户的一个品牌 id，
// 就能把自己看到点位扩到 B 家去——「伪造资源 ID 不能扩大权限」这条验收标准说的正是这个。
// 锚点管的是「哪些点位能被选中」，筛选条件管的是「选中哪几个」，两者不能互相替代：
// 一组 id 里混进一个别家的，那一支查不出点位（它不属于这个商户），而不是把 B 家的
// 点位带进来。
//
// 一组目标取并集：同一个品牌下的点位只会出现一次（store 只有一行），所以不需要去重；
// 空集合是一个正常答案（这个账号确实没有点位），不是错误。
func (r *pgStoreRepo) FindIDsByScope(ctx context.Context, merchantID, scopeType string, scopeIDs []string) ([]string, error) {
	// 三个档位都转成 text 再比，与 FindNames 同理：参数会被 PostgreSQL 按列类型解析，
	// 非 uuid 的输入会在 uuid 列上直接报 22P02 而不是返回空集。
	q := `SELECT s.id::text FROM stores s WHERE s.merchant_id = $1`
	args := []any{merchantID}
	switch scopeType {
	case auth.ScopeTypeMerchant:
		// 本商户全部点位，不加条件。空集合的 scope_ids 在这一档是正常的。
	case auth.ScopeTypeBrand:
		q += ` AND s.brand_id::text = ANY($2::text[])`
		args = append(args, scopeIDs)
	case auth.ScopeTypeStore:
		q += ` AND s.id::text = ANY($2::text[])`
		args = append(args, scopeIDs)
	default:
		return nil, ErrScopeTypeUnknown
	}
	// 排序只为让结果稳定，调用方把它当集合用。
	q += ` ORDER BY s.id`
	rows, err := r.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	// 非 nil 的空切片，不是 nil：这个结果会被原样当代入传给下游的 ANY()，nil 在那里
	// 编码成 NULL 并被读成「不过滤」——那正是「没授权任何点位」与「看全平台」的分界。
	ids := make([]string, 0, 8)
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// findNames reads id/name pairs for one table in a single round trip. table is
// interpolated into the statement, so every caller passes a literal and never
// anything that reached the process from outside.
func findNames(ctx context.Context, pool *pgxpool.Pool, table string, ids []string) (map[string]string, error) {
	names := map[string]string{}
	if len(ids) == 0 {
		return names, nil
	}
	rows, err := pool.Query(ctx, `SELECT id::text, name FROM `+table+` WHERE id = ANY($1::uuid[])`, ids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id, name string
		if err := rows.Scan(&id, &name); err != nil {
			return nil, err
		}
		names[id] = name
	}
	return names, rows.Err()
}

// CreateInTx 写门店、读回快照、记平台审计，全部落在调用方给的事务里；
// Commit 由调用方负责，失败时它把门店与待审核记录一起回滚。
func (r *pgStoreRepo) CreateInTx(ctx context.Context, tx pgx.Tx, s *model.Store) error {
	const q = `
		INSERT INTO stores (id, merchant_id, brand_id, name, logo, photos,
			province, city, district, province_code, city_code, district_code,
			address, longitude, latitude,
			phone, contact_name, contact_phone, detail, business_hours,
			customer_code, dms_code, customer_type,
			status, audit_status, audit_remark, audit_by, remark, visible, created_by, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, COALESCE($6, '{}'::text[]), $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19, $20, $21, $22, $23, $24, $25, $26, $27, $28, $29, $30, $31, $32)`
	if _, err := tx.Exec(ctx, q,
		s.ID, s.MerchantID, s.BrandID, s.Name, s.Logo, s.Photos,
		s.Province, s.City, s.District, s.ProvinceCode, s.CityCode, s.DistrictCode,
		s.Address, s.Longitude, s.Latitude,
		s.Phone, s.ContactName, s.ContactPhone, s.Detail, s.BusinessHours,
		s.CustomerCode, s.DMSCode, s.CustomerType,
		s.Status, s.AuditStatus, s.AuditRemark, s.AuditBy, s.Remark, s.Visible, s.CreatedBy,
		s.CreatedAt, s.UpdatedAt,
	); err != nil {
		return storeCodeConflict(err)
	}
	after, err := selectStoreSnapshot(ctx, tx, s.ID)
	if err != nil {
		return err
	}
	if err := r.audit.Record(ctx, tx, audit.Entry{
		Module: "stores", Action: "create", Operation: "新增门店",
		TargetType: "store", TargetID: s.ID, TargetName: after.Name,
		MerchantID: after.MerchantID,
		After:      audit.Snapshot(after),
	}); err != nil {
		return err
	}
	return nil
}

// Update 平台直接编辑，不碰状态与审核字段
func (r *pgStoreRepo) Update(ctx context.Context, s *model.Store) error {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	before, err := selectStoreSnapshot(ctx, tx, s.ID)
	if err != nil {
		return err
	}
	const q = `
		UPDATE stores
		SET brand_id = $2, name = $3, logo = $4, photos = COALESCE($5, '{}'::text[]),
			province = $6, city = $7, district = $8,
			province_code = $9, city_code = $10, district_code = $11,
			address = $12, longitude = $13, latitude = $14,
			phone = $15, contact_name = $16, contact_phone = $17, detail = $18, business_hours = $19,
			customer_code = $20, dms_code = $21, customer_type = $22,
			remark = $23, visible = $24, updated_at = $25
		WHERE id = $1`
	if _, err := tx.Exec(ctx, q,
		s.ID, s.BrandID, s.Name, s.Logo, s.Photos,
		s.Province, s.City, s.District, s.ProvinceCode, s.CityCode, s.DistrictCode,
		s.Address, s.Longitude, s.Latitude,
		s.Phone, s.ContactName, s.ContactPhone, s.Detail, s.BusinessHours,
		s.CustomerCode, s.DMSCode, s.CustomerType,
		s.Remark, s.Visible, s.UpdatedAt,
	); err != nil {
		return storeCodeConflict(err)
	}
	after, err := selectStoreSnapshot(ctx, tx, s.ID)
	if err != nil {
		return err
	}
	if err := r.audit.Record(ctx, tx, audit.Entry{
		Module: "stores", Action: "update", Operation: "修改门店",
		TargetType: "store", TargetID: s.ID, TargetName: after.Name,
		MerchantID: after.MerchantID,
		Before:     audit.Snapshot(before), After: audit.Snapshot(after),
	}); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (r *pgStoreRepo) UpdateStatus(ctx context.Context, id, status string) error {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	before, err := selectStoreSnapshot(ctx, tx, id)
	if err != nil {
		return err
	}
	const q = `UPDATE stores SET status = $1 WHERE id = $2`
	if _, err := tx.Exec(ctx, q, status, id); err != nil {
		return err
	}
	after, err := selectStoreSnapshot(ctx, tx, id)
	if err != nil {
		return err
	}
	if err := r.audit.Record(ctx, tx, audit.Entry{
		Module: "stores", Action: "update_status", Operation: "修改门店状态",
		TargetType: "store", TargetID: id, TargetName: after.Name,
		MerchantID: after.MerchantID,
		Before:     audit.Snapshot(before), After: audit.Snapshot(after),
	}); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// SetAuditInTx 审核门店：实体、待审核记录、平台审计同属调用方的一次提交。
func (r *pgStoreRepo) SetAuditInTx(ctx context.Context, tx pgx.Tx, id, auditStatus, remark, by string) error {
	// 前后快照都走这个 SELECT，它带 FOR UPDATE：并发的第二次审核会在这里等锁，
	// 拿到锁时读到的已经是「已审核」的新版本。
	before, err := selectStoreSnapshot(ctx, tx, id)
	if err != nil {
		return err
	}
	// 谓词里的 audit_status = 'pending' 才是并发下的唯一权威：两次审核都可能
	// 先读到 pending，但只有先拿到行锁的那次能改成 1 行，另一次落在 RowsAffected == 0，
	// 返回 ErrAuditNotPending（而不是两个方向都报成功）。
	//
	// 驳回必须同时离开 active：消费方（coffee-machine-service 的 checkStore）只认
	// status，光改 audit_status 的话被驳回的门店照样能接设备。status 的词表只有
	// active/disabled（stores.status 的列注释，无 CHECK 约束），所以驳回落到 disabled。
	// 通过不动 status——新建门店本来就是 active，而「审核通过」不该覆盖管理员手工设的
	// disabled（那是一个有意为之的停用）。
	const q = `
		UPDATE stores
		SET audit_status = $2, audit_remark = $3, audit_by = $4, audit_at = NOW(), updated_at = NOW(),
			status = CASE WHEN $2 = 'rejected' AND status = 'active' THEN 'disabled' ELSE status END
		WHERE id = $1 AND audit_status = 'pending'`
	tag, err := tx.Exec(ctx, q, id, auditStatus, remark, by)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrAuditNotPending
	}
	after, err := selectStoreSnapshot(ctx, tx, id)
	if err != nil {
		return err
	}
	return r.audit.Record(ctx, tx, audit.Entry{
		Module: "stores", Action: "audit", Operation: "审核门店",
		TargetType: "store", TargetID: id, TargetName: after.Name,
		MerchantID: after.MerchantID,
		Before:     audit.Snapshot(before), After: audit.Snapshot(after),
	})
}

func (r *pgStoreRepo) Delete(ctx context.Context, id string) error {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	before, err := selectStoreSnapshot(ctx, tx, id)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		return err
	}
	const q = `DELETE FROM stores WHERE id = $1`
	if _, err := tx.Exec(ctx, q, id); err != nil {
		return err
	}
	if err := r.audit.Record(ctx, tx, audit.Entry{
		Module: "stores", Action: "delete", Operation: "删除门店",
		TargetType: "store", TargetID: id, TargetName: before.Name,
		MerchantID: before.MerchantID,
		Before:     audit.Snapshot(before),
	}); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func scanStore(row pgx.Row) (*model.Store, error) {
	s := &model.Store{}
	err := row.Scan(
		&s.ID, &s.MerchantID, &s.BrandID, &s.Name, &s.Logo, &s.Photos,
		&s.Province, &s.City, &s.District, &s.ProvinceCode, &s.CityCode, &s.DistrictCode,
		&s.Address, &s.Longitude, &s.Latitude,
		&s.Phone, &s.ContactName, &s.ContactPhone, &s.Detail, &s.BusinessHours,
		&s.Status, &s.AuditStatus, &s.AuditRemark, &s.AuditAt, &s.AuditBy,
		&s.Remark, &s.Visible, &s.CreatedBy, &s.CreatedAt, &s.UpdatedAt,
		&s.CustomerCode, &s.DMSCode, &s.CustomerType,
		&s.MerchantName, &s.BrandName,
	)
	if err != nil {
		return nil, err
	}
	return s, nil
}

// buildFilter 商户/名称模糊/状态/审核状态 四个通用过滤条件；
// alias 为主表别名前缀（联表后 status 等列名会歧义，必须限定）
