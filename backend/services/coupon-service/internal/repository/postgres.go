package repository

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/panda-dev/panda-v2/backend/platform/audit"
	"github.com/panda-dev/panda-v2/backend/services/coupon-service/internal/dto"
	"github.com/panda-dev/panda-v2/backend/services/coupon-service/internal/model"
)

// ErrInsufficientInventory 表示批次没有足够的可用库存。
var ErrInsufficientInventory = errors.New("coupon inventory is insufficient")

// ErrCouponNotRedeemable 表示用户券当前不能核销。
var ErrCouponNotRedeemable = errors.New("coupon is not redeemable")

// ErrIdempotencyInProgress 表示相同幂等键仍处于处理中，不能将其当作成功响应返回。
var ErrIdempotencyInProgress = errors.New("idempotency operation is still processing")

// ErrIdempotencyConflict 表示同一个幂等键带着**不同的请求体**又来了。
//
// 与 ErrIdempotencyInProgress 分开：那个是「再等等」，这个是「这个键已经用在别的事上了」。
// service 层那个同名的哨兵就是它（见 service.ErrIdempotencyConflict 的别名），两处必须是
// 同一个值——否则并发走到仓储这一层报出来的冲突会被 controller 判成未知错误，回 500 而不是
// 409，调用方看到的是「服务端故障」，实际原因是他自己拿同一个键换了请求体。
var ErrIdempotencyConflict = errors.New("idempotency key request hash conflict")

// BatchRepository 提供批次库存的原子操作。
type BatchRepository interface {
	ReserveInventory(ctx context.Context, batchID string, quantity int64) error
}

// UserCouponRepository 提供用户券的行锁核销操作。
//
// Redeem 收 actorID（操作人，空串 = 没有操作人，落库为 NULL），与 Revoke 的
// 同一个口径：核销与作废都会写一条 coupon_state_transitions，两条都该记操作人。
type UserCouponRepository interface {
	Redeem(ctx context.Context, couponID, requestID, actorID string) (*model.UserCoupon, error)
}

// CouponTypeRepository 提供优惠券类型字典的管理操作。
type CouponTypeRepository interface {
	ListCouponTypes(ctx context.Context) ([]*model.CouponType, error)
	CreateCouponType(ctx context.Context, typ *model.CouponType) (*model.CouponType, error)
	UpdateCouponType(ctx context.Context, typ *model.CouponType) (*model.CouponType, error)
}

// IdempotencyRepository 提供幂等键的基础读写接口。
type IdempotencyRepository interface {
	Create(ctx context.Context, key *model.IdempotencyKey) (bool, error)
	Find(ctx context.Context, scope, idempotencyKey string) (*model.IdempotencyKey, error)
}

// postgresRepository 是优惠券库的数据访问实现。
//
// recorder 只用在**后台的人工操作**上：发券、核销、作废、券类型与券模板的增删改审。领券、
// 到期、系统发券不记——那些每天都在发生，记进审计表只会把要看的几条淹掉，它们的痕迹在
// coupon_state_transitions 里（那张表本来就只增不改）。
//
// 审计走平台的 admin.operation.logged → 身份库的 admin_operation_logs，**本库不建自己的
// 审计表**（见 platform/audit 的包说明）。
type postgresRepository struct {
	pool *pgxpool.Pool
	// recorder 为 nil 时用 audit.Noop：调用点的审计语句保持无条件执行，不留「忘了传 recorder
	// 就没有审计」的分支。
	recorder audit.Recorder
}

// NewPostgresRepository 创建 PostgreSQL 数据访问实现。
func NewPostgresRepository(pool *pgxpool.Pool, recorder audit.Recorder) (BatchRepository, UserCouponRepository, IdempotencyRepository) {
	if recorder == nil {
		recorder = audit.Noop{}
	}
	r := &postgresRepository{pool: pool, recorder: recorder}
	return r, r, r
}

// inTx 跑一个事务，出错即回滚。
//
// 「改了券」与「记下改了什么」必须整体成功或整体不动：券被发出去而日志没写，事后就没有任何
// 东西能解释这批券是谁发的；日志写了而操作回滚，那是一条凭空捏造的记录。所以审计条目走
// platform/audit，由它在**调用方的事务**里追加一条 outbox（它拒绝 nil tx，见那个包）。
func (r *postgresRepository) inTx(ctx context.Context, fn func(pgx.Tx) error) error {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	// 提交后这次 Rollback 是无操作（返回 ErrTxClosed），所以不必判断返回值。
	defer func() { _ = tx.Rollback(ctx) }()
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (r *postgresRepository) ListCouponTypes(ctx context.Context) ([]*model.CouponType, error) {
	rows, err := r.pool.Query(ctx, `SELECT id::text, code, name, description, status, created_at, updated_at FROM coupon_types ORDER BY created_at DESC, id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []*model.CouponType
	for rows.Next() {
		typ := &model.CouponType{}
		if err := rows.Scan(&typ.ID, &typ.Code, &typ.Name, &typ.Description, &typ.Status, &typ.CreatedAt, &typ.UpdatedAt); err != nil {
			return nil, err
		}
		result = append(result, typ)
	}
	return result, rows.Err()
}

// couponTypeSnapshot 是券类型写进审计的那几个字段。
//
// 不用 model.CouponType：它只有 db tag，直接快照出来是一串大写的列名。字段与后台表单一一
// 对应，id / created_at 不进——它们不可改，记下来只是把日志撑长。
type couponTypeSnapshot struct {
	Code        string `json:"code"`
	Name        string `json:"name"`
	Description string `json:"description"`
	Status      string `json:"status"`
}

func couponTypeSnapshotOf(t *model.CouponType) couponTypeSnapshot {
	return couponTypeSnapshot{Code: t.Code, Name: t.Name, Description: t.Description, Status: t.Status}
}

const couponTypeColumns = `id::text,code,name,description,status,created_at,updated_at`

// selectCouponTypeSnapshot 锁住这一行并读出待审字段。FOR UPDATE 的理由见 template.go 的
// selectTemplateSnapshot：它保证 before 与 after 之间不会有别人插进来。
func selectCouponTypeSnapshot(ctx context.Context, tx pgx.Tx, id string) (couponTypeSnapshot, error) {
	var s couponTypeSnapshot
	err := tx.QueryRow(ctx, `SELECT code,name,description,status FROM coupon_types WHERE id=$1 FOR UPDATE`, id).
		Scan(&s.Code, &s.Name, &s.Description, &s.Status)
	return s, err
}

// CreateCouponType 新增一个券类型。
//
// 写与审计同一个事务：券类型是**别的域的引用目标**（会员套餐的会员价券模板就挂在
// MEMBERSHIP_PRICE_EXPERIENCE 这个 code 上），谁在什么时候加了一个、改了一个，只有审计能回答。
func (r *postgresRepository) CreateCouponType(ctx context.Context, typ *model.CouponType) (*model.CouponType, error) {
	result := &model.CouponType{}
	err := r.inTx(ctx, func(tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `INSERT INTO coupon_types(code,name,description,status) VALUES($1,$2,$3,$4) RETURNING `+couponTypeColumns, typ.Code, typ.Name, typ.Description, typ.Status).
			Scan(&result.ID, &result.Code, &result.Name, &result.Description, &result.Status, &result.CreatedAt, &result.UpdatedAt); err != nil {
			return err
		}
		return r.recorder.Record(ctx, tx, audit.Entry{
			Module: "coupon_types", Action: "create", Operation: "新增优惠券类型",
			TargetType: "coupon_type", TargetID: result.ID, TargetName: result.Name,
			After: audit.Snapshot(couponTypeSnapshotOf(result)),
		})
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

// UpdateCouponType 改一个券类型的展示与状态。**code 不可改**：它是跨域的引用键（会员套餐按
// MEMBERSHIP_PRICE_EXPERIENCE 找券模板），改了它上面那句 UPDATE 也不会写——它压根不在 SET 里。
func (r *postgresRepository) UpdateCouponType(ctx context.Context, typ *model.CouponType) (*model.CouponType, error) {
	result := &model.CouponType{}
	err := r.inTx(ctx, func(tx pgx.Tx) error {
		before, err := selectCouponTypeSnapshot(ctx, tx, typ.ID)
		if err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, `UPDATE coupon_types SET name=$2,description=$3,status=$4,updated_at=NOW() WHERE id=$1 RETURNING `+couponTypeColumns, typ.ID, typ.Name, typ.Description, typ.Status).
			Scan(&result.ID, &result.Code, &result.Name, &result.Description, &result.Status, &result.CreatedAt, &result.UpdatedAt); err != nil {
			return err
		}
		return r.recorder.Record(ctx, tx, audit.Entry{
			Module: "coupon_types", Action: "update", Operation: "修改优惠券类型",
			TargetType: "coupon_type", TargetID: result.ID, TargetName: result.Name,
			Before: audit.Snapshot(before), After: audit.Snapshot(couponTypeSnapshotOf(result)),
		})
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

// ReserveInventory 使用带条件的 UPDATE，确保并发请求不会超卖库存。
func (r *postgresRepository) ReserveInventory(ctx context.Context, batchID string, quantity int64) error {
	if quantity <= 0 {
		return fmt.Errorf("quantity must be positive")
	}
	const q = `UPDATE coupon_batches
		SET reserved_quantity = reserved_quantity + $2, updated_at = NOW()
		WHERE id = $1 AND status = 'active'
		  AND reserved_quantity + issued_quantity + $2 <= total_quantity`
	result, err := r.pool.Exec(ctx, q, batchID, quantity)
	if err != nil {
		return err
	}
	if result.RowsAffected() == 0 {
		return ErrInsufficientInventory
	}
	return nil
}

// Redeem 在事务中锁定用户券行，校验状态和有效期后完成核销。
//
// actorID 与 Revoke 的同一个含义：后台账号的 subject，写进 coupon_state_transitions
// 的 actor_id。它只是留痕，不参与任何判断，所以允许为空串（内部调用方没有操作人时
// 落成 NULL），与 reason/request_id 那几个「必须有值」的参数不同。
func (r *postgresRepository) Redeem(ctx context.Context, couponID, requestID, actorID string) (*model.UserCoupon, error) {
	requestID = strings.TrimSpace(requestID)
	if requestID == "" {
		return nil, errors.New("request id is required")
	}
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	hash := requestHash(map[string]string{"coupon_id": couponID})
	if response, done, err := beginIdempotentOperation(ctx, tx, "coupon.redeem", requestID, hash, "user_coupon", couponID); err != nil {
		return nil, err
	} else if done {
		coupon := &model.UserCoupon{}
		if err := json.Unmarshal(response, coupon); err != nil {
			return nil, err
		}
		return coupon, tx.Commit(ctx)
	}

	coupon := &model.UserCoupon{}
	const selectQ = `SELECT id, template_id, batch_id, user_id, coupon_type_code,
		status, face_value, valid_from, expired_at, redeemed_at
		FROM user_coupons WHERE id = $1 FOR UPDATE`
	if err := tx.QueryRow(ctx, selectQ, couponID).Scan(
		&coupon.ID, &coupon.TemplateID, &coupon.BatchID, &coupon.UserID,
		&coupon.CouponTypeCode, &coupon.Status, &coupon.FaceValue,
		&coupon.ValidFrom, &coupon.ExpiredAt, &coupon.RedeemedAt,
	); err != nil {
		return nil, err
	}
	if coupon.Status != "claimed" {
		return nil, ErrCouponNotRedeemable
	}
	// 在改之前留一份 before：下面几行会把这个结构体的 Status 就地改成 redeemed，
	// 改完再拼快照，两边的 status 就都是 redeeemed 了——一条「从 redeemed 改成 redeemed」
	// 的日志比没有日志更坏，它会让人以为核销没生效。
	before := userCouponSnapshotOf(coupon)
	var valid bool
	if err := tx.QueryRow(ctx, `SELECT NOW() >= $1 AND NOW() < $2`, coupon.ValidFrom, coupon.ExpiredAt).Scan(&valid); err != nil {
		return nil, err
	}
	if !valid {
		return nil, ErrCouponNotRedeemable
	}
	if err := tx.QueryRow(ctx, `UPDATE user_coupons SET status = 'redeemed', redeemed_at = NOW(), updated_at = NOW() WHERE id = $1 AND status = 'claimed' RETURNING redeemed_at`, couponID).Scan(&coupon.RedeemedAt); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrCouponNotRedeemable
		}
		return nil, err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO coupon_redemptions (user_coupon_id, template_id, request_id, redemption_method, status, completed_at) VALUES ($1, $2, $3, 'platform', 'succeeded', NOW())`, coupon.ID, coupon.TemplateID, requestID); err != nil {
		return nil, err
	}
	// actor_id 的写法与 Revoke 那条逐字一致（NULLIF($n,'')::uuid）：核销也是人工动作，
	// 「谁核销的」与「谁作废的」在同一条时间线上，缺一个就得去后台审计里对时间戳。
	if _, err := tx.Exec(ctx, `INSERT INTO coupon_state_transitions(aggregate_type,aggregate_id,from_status,to_status,reason,request_id,actor_id) VALUES('user_coupon',$1,'claimed','redeemed','',$2,NULLIF($3,'')::uuid)`, coupon.ID, requestID, actorID); err != nil {
		return nil, err
	}
	coupon.Status = "redeemed"
	// 审计只在这条路上记：上面那个 `done` 分支是同一个 request_id 的重放，第一次已经记过了，
	// 再记一条会让日志上长出一串看起来像重复核销的记录。
	if err := r.recorder.Record(ctx, tx, audit.Entry{
		Module: "coupons", Action: "redeem", Operation: "核销优惠券",
		TargetType: "user_coupon", TargetID: coupon.ID, TargetName: coupon.CouponTypeCode,
		Before: audit.Snapshot(before), After: audit.Snapshot(userCouponSnapshotOf(coupon)),
	}); err != nil {
		return nil, err
	}
	response, err := json.Marshal(coupon)
	if err != nil {
		return nil, err
	}
	if _, err = tx.Exec(ctx, `UPDATE coupon_idempotency_keys SET resource_id=$2,response=$3,status='succeeded' WHERE scope=$1 AND idempotency_key=$4`, "coupon.redeem", couponID, response, requestID); err != nil {
		return nil, err
	}
	return coupon, tx.Commit(ctx)
}

func requestHash(value any) string {
	raw, _ := json.Marshal(value)
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

func beginIdempotentOperation(ctx context.Context, tx pgx.Tx, scope, key, hash, resourceType, resourceID string) ([]byte, bool, error) {
	// resource_id 用 NULLIF 落空串：发券那条路要等批次建出来才知道它指向谁（末尾那条 UPDATE
	// 补上），调用时只能给空串，而空串不是合法的 uuid。
	result, err := tx.Exec(ctx, `INSERT INTO coupon_idempotency_keys(scope,idempotency_key,request_hash,resource_type,resource_id,status) VALUES($1,$2,$3,$4,NULLIF($5,'')::uuid,'processing') ON CONFLICT (scope,idempotency_key) DO NOTHING`, scope, key, hash, resourceType, resourceID)
	if err != nil {
		return nil, false, err
	}
	if result.RowsAffected() == 1 {
		return nil, false, nil
	}
	var existingHash, status string
	var response []byte
	var createdAt time.Time
	err = tx.QueryRow(ctx, `SELECT request_hash,status,response,created_at FROM coupon_idempotency_keys WHERE scope=$1 AND idempotency_key=$2 FOR UPDATE`, scope, key).Scan(&existingHash, &status, &response, &createdAt)
	if err != nil {
		return nil, false, err
	}
	if existingHash != hash {
		return nil, false, ErrIdempotencyConflict
	}
	if status == "succeeded" {
		return response, true, nil
	}
	if status == "processing" {
		if createdAt.After(time.Now().Add(-15 * time.Minute)) {
			return nil, false, ErrIdempotencyInProgress
		}
		// A committed processing row older than the recovery window cannot be
		// trusted to represent a live transaction. Reclaim it before retrying.
		if _, err := tx.Exec(ctx, `DELETE FROM coupon_idempotency_keys WHERE scope=$1 AND idempotency_key=$2`, scope, key); err != nil {
			return nil, false, err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO coupon_idempotency_keys(scope,idempotency_key,request_hash,resource_type,resource_id,status) VALUES($1,$2,$3,$4,NULLIF($5,'')::uuid,'processing')`, scope, key, hash, resourceType, resourceID); err != nil {
			return nil, false, err
		}
		return nil, false, nil
	}
	return nil, false, fmt.Errorf("idempotency operation has unsupported status %q", status)
}

const userCouponColumns = `id::text,template_id::text,batch_id::text,user_id::text,coupon_type_code,claim_type,issue_reason,status,face_value,min_purchase_amount,redemption_type,valid_from,expired_at,claimed_at,held_at,redeemed_at,refunded_at,invalidated_at,created_at,updated_at`

// userCouponAuditSnapshot 是用户券写进审计的那几个字段。
//
// 一张券的其余列（有效期、门槛、核销方式）都是发券那一刻从模板快照过来的**历史事实**，
// 后台这两条路改不动它们，记下来只会让日志里全是两份一模一样的长文本。要看的只有「这张券
// 现在是什么状态、什么时候变的、为什么」。
type userCouponAuditSnapshot struct {
	UserID         string     `json:"userId"`
	CouponTypeCode string     `json:"couponTypeCode"`
	FaceValue      int64      `json:"faceValue"`
	Status         string     `json:"status"`
	RedeemedAt     *time.Time `json:"redeemedAt,omitempty"`
	InvalidatedAt  *time.Time `json:"invalidatedAt,omitempty"`
	Reason         string     `json:"reason,omitempty"`
}

func userCouponSnapshotOf(c *model.UserCoupon) userCouponAuditSnapshot {
	return userCouponAuditSnapshot{
		UserID: c.UserID, CouponTypeCode: c.CouponTypeCode, FaceValue: c.FaceValue,
		Status: c.Status, RedeemedAt: c.RedeemedAt, InvalidatedAt: c.InvalidatedAt,
	}
}

func scanUserCoupon(row interface{ Scan(...any) error }) (*model.UserCoupon, error) {
	c := &model.UserCoupon{}
	err := row.Scan(&c.ID, &c.TemplateID, &c.BatchID, &c.UserID, &c.CouponTypeCode, &c.ClaimType, &c.IssueReason, &c.Status, &c.FaceValue, &c.MinPurchaseAmount, &c.RedemptionType, &c.ValidFrom, &c.ExpiredAt, &c.ClaimedAt, &c.HeldAt, &c.RedeemedAt, &c.RefundedAt, &c.InvalidatedAt, &c.CreatedAt, &c.UpdatedAt)
	return c, err
}
func (r *postgresRepository) GetUserCoupon(ctx context.Context, id string) (*model.UserCoupon, error) {
	return scanUserCoupon(r.pool.QueryRow(ctx, `SELECT `+userCouponColumns+` FROM user_coupons WHERE id=$1`, id))
}

// userCouponListWhere 拼用户券列表的筛选子句与绑定参数，写法与 templateListWhere 一致。
//
// uuid 列必须转成 text 再比：PostgreSQL 会把参数按列类型解析，空串在 uuid 列上
// 直接报 invalid input syntax，即便条件本意是「不筛」。
func userCouponListWhere(q dto.UserCouponQuery) (string, []any) {
	where := []string{"1=1"}
	args := []any{}
	add := func(clause string, value any) {
		args = append(args, value)
		where = append(where, fmt.Sprintf(clause, len(args)))
	}
	if q.ID != "" {
		add("id::text=$%d", q.ID)
	}
	if q.UserID != "" {
		add("user_id::text=$%d", q.UserID)
	}
	if q.Status != "" {
		add("status=$%d", q.Status)
	}
	if q.BatchID != "" {
		add("batch_id::text=$%d", q.BatchID)
	}
	if q.TemplateID != "" {
		add("template_id::text=$%d", q.TemplateID)
	}
	if q.CouponTypeCode != "" {
		add("coupon_type_code=$%d", q.CouponTypeCode)
	}
	return strings.Join(where, " AND "), args
}

func (r *postgresRepository) ListUserCoupons(ctx context.Context, q dto.UserCouponQuery) ([]*model.UserCoupon, int64, error) {
	if q.Page < 1 {
		q.Page = 1
	}
	// 兜底：HTTP 层已由 api.ParsePage 按 dto.MaxPageSize 挡过一道。上限必须和那一处
	// 一致，否则越界值会被**静默**改成 20 —— 请求页码 200 却只回 20 行，比报错更难查。
	if q.PageSize < 1 || q.PageSize > dto.MaxPageSize {
		q.PageSize = 20
	}
	where, args := userCouponListWhere(q)
	var total int64
	if err := r.pool.QueryRow(ctx, `SELECT count(*) FROM user_coupons WHERE `+where, args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	args = append(args, q.PageSize, (q.Page-1)*q.PageSize)
	rows, err := r.pool.Query(ctx, `SELECT `+userCouponColumns+` FROM user_coupons WHERE `+where+
		fmt.Sprintf(` ORDER BY created_at DESC,id LIMIT $%d OFFSET $%d`, len(args)-1, len(args)), args...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	var out []*model.UserCoupon
	for rows.Next() {
		c, e := scanUserCoupon(rows)
		if e != nil {
			return nil, 0, e
		}
		out = append(out, c)
	}
	return out, total, rows.Err()
}
func (r *postgresRepository) UserCouponStats(ctx context.Context, userID string) (*dto.UserCouponStats, error) {
	s := &dto.UserCouponStats{}
	e := r.pool.QueryRow(ctx, `SELECT count(*),count(*) FILTER(WHERE status='claimed'),count(*) FILTER(WHERE status='held'),count(*) FILTER(WHERE status='redeemed'),count(*) FILTER(WHERE status='expired'),count(*) FILTER(WHERE status='refunded'),count(*) FILTER(WHERE status='invalidated') FROM user_coupons WHERE user_id=$1`, userID).Scan(&s.Total, &s.Claimed, &s.Held, &s.Redeemed, &s.Expired, &s.Refunded, &s.Invalidated)
	return s, e
}
func (r *postgresRepository) Revoke(ctx context.Context, id, requestID, actorID, reason string) (*model.UserCoupon, error) {
	requestID = strings.TrimSpace(requestID)
	if requestID == "" {
		return nil, errors.New("request id is required")
	}
	tx, e := r.pool.Begin(ctx)
	if e != nil {
		return nil, e
	}
	defer tx.Rollback(ctx)
	hash := requestHash(map[string]string{"coupon_id": id})
	if response, done, err := beginIdempotentOperation(ctx, tx, "coupon.revoke", requestID, hash, "user_coupon", id); err != nil {
		return nil, err
	} else if done {
		c := &model.UserCoupon{}
		if err := json.Unmarshal(response, c); err != nil {
			return nil, err
		}
		return c, tx.Commit(ctx)
	}
	c, e := scanUserCoupon(tx.QueryRow(ctx, `SELECT `+userCouponColumns+` FROM user_coupons WHERE id=$1 FOR UPDATE`, id))
	if e != nil {
		return nil, e
	}
	if c.Status != "claimed" && c.Status != "held" {
		return nil, ErrCouponNotRedeemable
	}
	// 与 Redeem 同一个理由：下面那行只回写 invalidated_at，c.Status 靠手工赋值翻新，
	// before 必须在这一行之前取。
	before := userCouponSnapshotOf(c)
	if e = tx.QueryRow(ctx, `UPDATE user_coupons SET status='invalidated',invalidated_at=NOW(),updated_at=NOW() WHERE id=$1 RETURNING invalidated_at`, id).Scan(&c.InvalidatedAt); e != nil {
		return nil, e
	}
	if _, e = tx.Exec(ctx, `INSERT INTO coupon_state_transitions(aggregate_type,aggregate_id,from_status,to_status,reason,request_id,actor_id) VALUES('user_coupon',$1,$2,'invalidated',$3,$4,NULLIF($5,'')::uuid)`, id, c.Status, reason, requestID, actorID); e != nil {
		return nil, e
	}
	c.Status = "invalidated"
	after := userCouponSnapshotOf(c)
	// 作废的原因跟着记：这张券为什么没了，是事后最常被问的那一句，而 user_coupons 上
	// 只有一个状态列，reason 只落在状态迁移表里。
	after.Reason = reason
	if e := r.recorder.Record(ctx, tx, audit.Entry{
		Module: "coupons", Action: "revoke", Operation: "作废优惠券",
		TargetType: "user_coupon", TargetID: c.ID, TargetName: c.CouponTypeCode,
		Before: audit.Snapshot(before), After: audit.Snapshot(after),
	}); e != nil {
		return nil, e
	}
	response, e := json.Marshal(c)
	if e != nil {
		return nil, e
	}
	if _, e = tx.Exec(ctx, `UPDATE coupon_idempotency_keys SET resource_id=$2,response=$3,status='succeeded' WHERE scope=$1 AND idempotency_key=$4`, "coupon.revoke", id, response, requestID); e != nil {
		return nil, e
	}
	return c, tx.Commit(ctx)
}

func (r *postgresRepository) Create(ctx context.Context, key *model.IdempotencyKey) (bool, error) {
	const q = `INSERT INTO coupon_idempotency_keys
		(id, scope, idempotency_key, request_hash, resource_type, resource_id, response, status, expires_at)
		VALUES ($1, $2, $3, $4, $5, $6, COALESCE($7, '{}'::jsonb), $8, $9)
		ON CONFLICT (scope, idempotency_key) DO NOTHING`
	result, err := r.pool.Exec(ctx, q, key.ID, key.Scope, key.Key, key.RequestHash,
		key.ResourceType, key.ResourceID, json.RawMessage(key.Response), key.Status, key.ExpiresAt)
	if err != nil {
		return false, err
	}
	return result.RowsAffected() == 1, nil
}

func (r *postgresRepository) Find(ctx context.Context, scope, idempotencyKey string) (*model.IdempotencyKey, error) {
	const q = `SELECT id, scope, idempotency_key, request_hash, resource_type,
		resource_id, response, status, expires_at, created_at
		FROM coupon_idempotency_keys WHERE scope = $1 AND idempotency_key = $2`
	key := &model.IdempotencyKey{}
	if err := r.pool.QueryRow(ctx, q, scope, idempotencyKey).Scan(
		&key.ID, &key.Scope, &key.Key, &key.RequestHash, &key.ResourceType,
		&key.ResourceID, &key.Response, &key.Status, &key.ExpiresAt, &key.CreatedAt,
	); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	return key, nil
}
