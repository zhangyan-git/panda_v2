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
	"github.com/panda-dev/panda-v2/backend/services/coupon-service/internal/dto"
	"github.com/panda-dev/panda-v2/backend/services/coupon-service/internal/model"
)

// ErrInsufficientInventory 表示批次没有足够的可用库存。
var ErrInsufficientInventory = errors.New("coupon inventory is insufficient")

// ErrCouponNotRedeemable 表示用户券当前不能核销。
var ErrCouponNotRedeemable = errors.New("coupon is not redeemable")

// ErrIdempotencyInProgress 表示相同幂等键仍处于处理中，不能将其当作成功响应返回。
var ErrIdempotencyInProgress = errors.New("idempotency operation is still processing")

// BatchRepository 提供批次库存的原子操作。
type BatchRepository interface {
	ReserveInventory(ctx context.Context, batchID string, quantity int64) error
}

// UserCouponRepository 提供用户券的行锁核销操作。
type UserCouponRepository interface {
	Redeem(ctx context.Context, couponID, requestID string) (*model.UserCoupon, error)
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

type postgresRepository struct{ pool *pgxpool.Pool }

// NewPostgresRepository 创建 PostgreSQL 数据访问实现。
func NewPostgresRepository(pool *pgxpool.Pool) (BatchRepository, UserCouponRepository, IdempotencyRepository) {
	r := &postgresRepository{pool: pool}
	return r, r, r
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

func (r *postgresRepository) CreateCouponType(ctx context.Context, typ *model.CouponType) (*model.CouponType, error) {
	result := &model.CouponType{}
	err := r.pool.QueryRow(ctx, `INSERT INTO coupon_types(code,name,description,status) VALUES($1,$2,$3,$4) RETURNING id::text,code,name,description,status,created_at,updated_at`, typ.Code, typ.Name, typ.Description, typ.Status).Scan(&result.ID, &result.Code, &result.Name, &result.Description, &result.Status, &result.CreatedAt, &result.UpdatedAt)
	return result, err
}

func (r *postgresRepository) UpdateCouponType(ctx context.Context, typ *model.CouponType) (*model.CouponType, error) {
	result := &model.CouponType{}
	err := r.pool.QueryRow(ctx, `UPDATE coupon_types SET name=$2,description=$3,status=$4,updated_at=NOW() WHERE id=$1 RETURNING id::text,code,name,description,status,created_at,updated_at`, typ.ID, typ.Name, typ.Description, typ.Status).Scan(&result.ID, &result.Code, &result.Name, &result.Description, &result.Status, &result.CreatedAt, &result.UpdatedAt)
	return result, err
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
func (r *postgresRepository) Redeem(ctx context.Context, couponID, requestID string) (*model.UserCoupon, error) {
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
	if _, err := tx.Exec(ctx, `INSERT INTO coupon_state_transitions(aggregate_type,aggregate_id,from_status,to_status,reason,request_id) VALUES('user_coupon',$1,'claimed','redeemed','',$2)`, coupon.ID, requestID); err != nil {
		return nil, err
	}
	coupon.Status = "redeemed"
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
	result, err := tx.Exec(ctx, `INSERT INTO coupon_idempotency_keys(scope,idempotency_key,request_hash,resource_type,resource_id,status) VALUES($1,$2,$3,$4,$5,'processing') ON CONFLICT (scope,idempotency_key) DO NOTHING`, scope, key, hash, resourceType, resourceID)
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
		return nil, false, errors.New("idempotency key request hash conflict")
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
		if _, err := tx.Exec(ctx, `INSERT INTO coupon_idempotency_keys(scope,idempotency_key,request_hash,resource_type,resource_id,status) VALUES($1,$2,$3,$4,$5,'processing')`, scope, key, hash, resourceType, resourceID); err != nil {
			return nil, false, err
		}
		return nil, false, nil
	}
	return nil, false, fmt.Errorf("idempotency operation has unsupported status %q", status)
}

const userCouponColumns = `id::text,template_id::text,batch_id::text,user_id::text,coupon_type_code,claim_type,issue_reason,status,face_value,min_purchase_amount,redemption_type,valid_from,expired_at,claimed_at,held_at,redeemed_at,refunded_at,invalidated_at,created_at,updated_at`

func scanUserCoupon(row interface{ Scan(...any) error }) (*model.UserCoupon, error) {
	c := &model.UserCoupon{}
	err := row.Scan(&c.ID, &c.TemplateID, &c.BatchID, &c.UserID, &c.CouponTypeCode, &c.ClaimType, &c.IssueReason, &c.Status, &c.FaceValue, &c.MinPurchaseAmount, &c.RedemptionType, &c.ValidFrom, &c.ExpiredAt, &c.ClaimedAt, &c.HeldAt, &c.RedeemedAt, &c.RefundedAt, &c.InvalidatedAt, &c.CreatedAt, &c.UpdatedAt)
	return c, err
}
func (r *postgresRepository) GetUserCoupon(ctx context.Context, id string) (*model.UserCoupon, error) {
	return scanUserCoupon(r.pool.QueryRow(ctx, `SELECT `+userCouponColumns+` FROM user_coupons WHERE id=$1`, id))
}
func (r *postgresRepository) ListUserCoupons(ctx context.Context, q dto.UserCouponQuery) ([]*model.UserCoupon, int64, error) {
	if q.Page < 1 {
		q.Page = 1
	}
	if q.PageSize < 1 || q.PageSize > 100 {
		q.PageSize = 20
	}
	// uuid 列必须转成 text 再比：PostgreSQL 会把参数按列类型解析，空串在
	// uuid 列上直接报 invalid input syntax，即便 OR 左边永远为真。
	w := ` WHERE ($1='' OR user_id::text=$1) AND ($2='' OR status=$2) AND ($3='' OR batch_id::text=$3) AND ($4='' OR template_id::text=$4)`
	var total int64
	if err := r.pool.QueryRow(ctx, `SELECT count(*) FROM user_coupons`+w, q.UserID, q.Status, q.BatchID, q.TemplateID).Scan(&total); err != nil {
		return nil, 0, err
	}
	rows, err := r.pool.Query(ctx, `SELECT `+userCouponColumns+` FROM user_coupons`+w+` ORDER BY created_at DESC,id LIMIT $5 OFFSET $6`, q.UserID, q.Status, q.BatchID, q.TemplateID, q.PageSize, (q.Page-1)*q.PageSize)
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
	if e = tx.QueryRow(ctx, `UPDATE user_coupons SET status='invalidated',invalidated_at=NOW(),updated_at=NOW() WHERE id=$1 RETURNING invalidated_at`, id).Scan(&c.InvalidatedAt); e != nil {
		return nil, e
	}
	if _, e = tx.Exec(ctx, `INSERT INTO coupon_state_transitions(aggregate_type,aggregate_id,from_status,to_status,reason,request_id,actor_id) VALUES('user_coupon',$1,$2,'invalidated',$3,$4,NULLIF($5,'')::uuid)`, id, c.Status, reason, requestID, actorID); e != nil {
		return nil, e
	}
	c.Status = "invalidated"
	response, e := json.Marshal(c)
	if e != nil {
		return nil, e
	}
	if _, e = tx.Exec(ctx, `UPDATE coupon_idempotency_keys SET resource_id=$2,response=$3,status='succeeded' WHERE scope=$1 AND idempotency_key=$4`, "coupon.revoke", id, response, requestID); e != nil {
		return nil, e
	}
	return c, tx.Commit(ctx)
}

// IssueCoupons 直接使用调用方传来的 hash，不自己再算一份。
//
// 这里原本写成 json.Marshal(req) 重新算一遍、把形参丢掉。改 tag 之前两份哈希
// 碰巧相等，所以看不出问题；一旦 service 侧把哈希换成冻结 tag 的渲染，两者就
// 分叉了——service 拿旧哈希做预检、这里拿新哈希写库，同一个 key 重放会被判成
// 冲突。哈希只能有一个来源。
func (r *postgresRepository) IssueCoupons(ctx context.Context, key, actorID, hash string, req dto.IssueCouponsRequest) (*dto.IssueCouponsResponse, error) {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	var existingHash string
	var existingResponse []byte
	err = tx.QueryRow(ctx, `SELECT request_hash,response FROM coupon_idempotency_keys WHERE scope='admin.coupons.issue' AND idempotency_key=$1 FOR UPDATE`, key).Scan(&existingHash, &existingResponse)
	if err == nil {
		if existingHash != hash {
			return nil, errors.New("idempotency key request hash conflict")
		}
		var out dto.IssueCouponsResponse
		if err := json.Unmarshal(existingResponse, &out); err != nil {
			return nil, err
		}
		return &out, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return nil, err
	}
	if _, err = tx.Exec(ctx, `INSERT INTO coupon_idempotency_keys(scope,idempotency_key,request_hash,resource_type,status) VALUES('admin.coupons.issue',$1,$2,'coupon_batch','processing')`, key, hash); err != nil {
		return nil, err
	}
	var templateID, typeCode string
	var face, minPurchase int64
	var redemptionType, validityMode string
	var validFrom, validTo *time.Time
	var validDays *int
	var total int64
	err = tx.QueryRow(ctx, `SELECT t.id, ct.code, t.face_value,t.min_purchase_amount,t.redemption_type,t.validity_mode,t.valid_from,t.valid_to,t.valid_days,t.total_quantity FROM coupon_templates t JOIN coupon_types ct ON ct.id=t.coupon_type_id WHERE t.id=$1 AND t.status='active' AND t.audit_status='approved' FOR UPDATE`, req.TemplateID).Scan(&templateID, &typeCode, &face, &minPurchase, &redemptionType, &validityMode, &validFrom, &validTo, &validDays, &total)
	if err != nil {
		return nil, err
	}
	quantity := int64(len(req.UserIDs) * req.QuantityPerUser)
	if quantity > total {
		return nil, ErrInsufficientInventory
	}
	batchID := ""
	err = tx.QueryRow(ctx, `INSERT INTO coupon_batches(template_id,batch_no,source,total_quantity,reserved_quantity,issued_quantity,status,request_id,created_by) VALUES($1,'admin-'||substr(gen_random_uuid()::text,1,12),'admin',$2,0,$2,'exhausted',$3,$4) RETURNING id::text`, templateID, quantity, key, actorID).Scan(&batchID)
	if err != nil {
		return nil, err
	}
	result, err := tx.Exec(ctx, `UPDATE coupon_templates SET issued_quantity=issued_quantity+$2,updated_at=NOW() WHERE id=$1 AND issued_quantity+reserved_quantity+$2<=total_quantity`, templateID, quantity)
	if err != nil {
		return nil, err
	}
	if result.RowsAffected() != 1 {
		return nil, ErrInsufficientInventory
	}
	var scopeRows []struct{ typ, id string }
	rows, err := tx.Query(ctx, `SELECT scope_type,scope_id::text FROM coupon_template_scopes WHERE template_id=$1`, templateID)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var x struct{ typ, id string }
		if err := rows.Scan(&x.typ, &x.id); err != nil {
			rows.Close()
			return nil, err
		}
		scopeRows = append(scopeRows, x)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	rows.Close()
	ids := make([]string, 0, quantity)
	for _, userID := range req.UserIDs {
		for j := 0; j < req.QuantityPerUser; j++ {
			var id string
			err = tx.QueryRow(ctx, `INSERT INTO user_coupons(template_id,batch_id,user_id,coupon_type_code,claim_type,issue_reason,face_value,min_purchase_amount,redemption_type,valid_from,expired_at) VALUES($1,$2,$3,$4,'admin_assign',$5,$6,$7,$8,COALESCE($9,NOW()),COALESCE($10,NOW()+make_interval(days=>COALESCE($11,30)))) RETURNING id::text`, templateID, batchID, userID, typeCode, req.Reason, face, minPurchase, redemptionType, validFrom, validTo, validDays).Scan(&id)
			if err != nil {
				return nil, err
			}
			ids = append(ids, id)
			for _, sc := range scopeRows {
				if _, err = tx.Exec(ctx, `INSERT INTO user_coupon_scopes(user_coupon_id,scope_type,scope_id) VALUES($1,$2,$3)`, id, sc.typ, sc.id); err != nil {
					return nil, err
				}
			}
		}
	}
	payload, _ := json.Marshal(map[string]any{"batch_id": batchID, "user_coupon_ids": ids, "actor_id": actorID})
	if _, err = tx.Exec(ctx, `INSERT INTO coupon_inventory_ledger(template_id,batch_id,reference_type,reference_id,quantity,operation,request_id) VALUES($1,$2,'admin_issue',$2,$3,'issue',$4)`, templateID, batchID, quantity, key); err != nil {
		return nil, err
	}
	if _, err = tx.Exec(ctx, `INSERT INTO coupon_state_transitions(aggregate_type,aggregate_id,from_status,to_status,reason,request_id,metadata) VALUES('batch',$1,'','active',$2,$3,$4)`, batchID, req.Reason, key, payload); err != nil {
		return nil, err
	}
	if _, err = tx.Exec(ctx, `INSERT INTO message_outbox(event_id,event_type,event_version,trace_id,payload) VALUES(gen_random_uuid()::text,'coupon.issued','v1',$1,$2)`, key, payload); err != nil {
		return nil, err
	}
	out := &dto.IssueCouponsResponse{BatchID: batchID, IssuedQuantity: int(quantity), UserCouponIDs: ids}
	encoded, _ := json.Marshal(out)
	if _, err = tx.Exec(ctx, `UPDATE coupon_idempotency_keys SET resource_id=$2,response=$3,status='succeeded' WHERE scope='admin.coupons.issue' AND idempotency_key=$1`, key, batchID, encoded); err != nil {
		return nil, err
	}
	if err = tx.Commit(ctx); err != nil {
		return nil, err
	}
	return out, nil
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
