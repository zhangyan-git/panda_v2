package repository

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/panda-dev/panda-v2/backend/platform/audit"
	"github.com/panda-dev/panda-v2/backend/services/coupon-service/internal/dto"
)

// ErrTemplateUnavailable 表示要发的那张券模板不存在、已停用或还没审核通过。
//
// 它只出在**系统发券**（会员价券、活动券）那条路上：模板是运营在后台配的，配错了不会因为
// 重投就变对，所以消费方看见它应当记一条日志然后 ack——一直重投只会把队列堵到死信。
// 后台人工发放那条路不走它：那条路的调用方就在屏幕前面，原始的「查不到」原样回给他更有用。
var ErrTemplateUnavailable = errors.New("coupon template is not available for issuing")

// issueSpec 是一次发券的全部差异点。
//
// 三种发券（后台人工、会员价券、活动券）走的是**同一条链路**：查模板 → 建批次 → 增模板
// 发行数 → 逐张写 user_coupons 与适用范围 → 记库存流水与状态迁移 → （人工的那条才）写审计
// → 写 outbox → 记幂等响应。差别只有下面这些值，所以它们抽成一个结构体、由一个函数执行；
// 抄三份的话，将来中间任何一步加一个判断都要记得改三处，而漏掉的那处不会报错。
type issueSpec struct {
	// scope + key 构成幂等键（coupon_idempotency_keys 的唯一索引是 scope+idempotency_key）。
	scope string
	key   string
	// hash 是请求体摘要，由调用方算好传进来——哈希只能有一个来源（见 IssueCoupons 上那段
	// 说明）。库上 request_hash 非空，系统发券也要给一个真摘要。
	hash string
	// actorID 是操作人，空串落 NULL。系统发券没有操作人。
	actorID string
	// batchPrefix 是批次号前缀（batch_no = 前缀 + 12 位随机串）。运营对账时手里拿的就是这个
	// 批次号，所以它出现在审计的 TargetName 与日志里。
	batchPrefix string
	// source 进 coupon_batches.source（CHECK：platform/admin/merchant/event/purchase）。
	source string
	// claimType 进 user_coupons.claim_type（CHECK：user_claim/admin_assign/daily_gift/
	// event_reward/claim_by_code/purchase）。它答「这张券是怎么来的」。
	claimType string
	// referenceType / referenceID 进 coupon_inventory_ledger，答「这一批是因为什么发出去的」。
	// 系统发券指向业务凭据（领取记录、会员+订单），后台人工发放指向批次自己。
	referenceType string
	referenceID   string
	// campaignClaimID 非空时写进 user_coupons.campaign_claim_id——店铺码活动那条路用它把券与
	// 领取记录对上（老系统同一列、同一用途）。
	campaignClaimID string
	// storeScopeID 非空时，发出的券的适用范围**强制取这一家店**，不抄模板自己的 scope。
	// 店铺码活动那条路用它（老系统同一口径，见 GrantMembershipCoupons 的说明）。
	storeScopeID string
	// eventType 是发完之后写进 outbox 的事件类型。
	eventType string
	// audit 为真才写平台审计。**系统发券不写**：会员价券与活动券每天都在发生，而审计表记的是
	// 后台人工操作，记进去只会把要看的几条淹掉（见 NewPostgresRepository 上那段）。
	audit           bool
	auditAction     string
	auditOperation  string
	reason          string
	templateID      string
	userIDs         []string
	quantityPerUser int
}

// IssueCoupons 直接使用调用方传来的 hash，不自己再算一份。
//
// 这里原本写成 json.Marshal(req) 重新算一遍、把形参丢掉。改 tag 之前两份哈希
// 碰巧相等，所以看不出问题；一旦 service 侧把哈希换成冻结 tag 的渲染，两者就
// 分叉了——service 拿旧哈希做预检、这里拿新哈希写库，同一个 key 重放会被判成
// 冲突。哈希只能有一个来源。
func (r *postgresRepository) IssueCoupons(ctx context.Context, key, actorID, hash string, req dto.IssueCouponsRequest) (*dto.IssueCouponsResponse, error) {
	return r.issue(ctx, issueSpec{
		scope:           "admin.coupons.issue",
		key:             key,
		hash:            hash,
		actorID:         actorID,
		batchPrefix:     "admin-",
		source:          "admin",
		claimType:       "admin_assign",
		referenceType:   "admin_issue",
		eventType:       "coupon.issued",
		audit:           true,
		auditAction:     "issue",
		auditOperation:  "发放优惠券",
		reason:          req.Reason,
		templateID:      req.TemplateID,
		userIDs:         req.UserIDs,
		quantityPerUser: req.QuantityPerUser,
	})
}

// GrantParams 是一次**系统发券**的全部输入：没有操作人，幂等键由业务凭据派生。
//
// 它与 dto.IssueCouponsRequest 分开而不是复用：那个结构体的字段名与顺序**同时是幂等哈希的
// 输入**（见 service 里那份冻结的结构体），往里加字段会动到线上存量 key 的哈希。两者只是
// 长得像，契约不是同一个。
type GrantParams struct {
	// Scope / Key / Hash 是幂等三元组。Key 必须是**从业务凭据派生**的确定性字符串
	// （领取记录 id、会员 id + 订单号），不能是随机的——重投时要靠它命中上一次的响应。
	Scope string
	Key   string
	Hash  string
	// TemplateID / UserIDs / QuantityPerUser 是要发什么、发给谁、发几张。
	TemplateID      string
	UserIDs         []string
	QuantityPerUser int
	// Reason 进 user_coupons.issue_reason，是用户在券上看到的那句话。
	Reason string
	// ClaimType / Source / ReferenceType 见 issueSpec 上那几个字段的说明。
	ClaimType     string
	Source        string
	ReferenceType string
	// ReferenceID 是这次发券指向的业务凭据（一般就是 Key 那一份）。
	ReferenceID string
	// CampaignClaimID / StoreScopeID 只给店铺码活动用，见字段说明。
	CampaignClaimID string
	StoreScopeID    string
	// BatchPrefix 见 issueSpec。系统发券用「member-」「campaign-」这类前缀，运营在批次列表上
	// 一眼能分出这批是谁发的。
	BatchPrefix string
	// EventType 是发完之后写进 outbox 的事件类型。
	EventType string
}

// GrantSystemCoupons 执行一次系统发券。
//
// 它与后台人工发放共用同一个事务体（见 issueSpec 上那段）：库存、幂等、状态迁移、outbox
// 一字不差，差别只在批次来源、券的来源、有没有操作人与审计。
//
// # 模板查不到 ⇒ ErrTemplateUnavailable
//
// 原始错误是 pgx 的 ErrNoRows，它与「这个事务里别的查询没查到行」长得一模一样。这里当场
// 把它翻成一个说得清的错误值，消费方才判得出「这是配错了、别再重投」。
func (r *postgresRepository) GrantSystemCoupons(ctx context.Context, p GrantParams) (*dto.IssueCouponsResponse, error) {
	out, err := r.issue(ctx, issueSpec{
		scope:           p.Scope,
		key:             p.Key,
		hash:            p.Hash,
		batchPrefix:     p.BatchPrefix,
		source:          p.Source,
		claimType:       p.ClaimType,
		referenceType:   p.ReferenceType,
		referenceID:     p.ReferenceID,
		campaignClaimID: p.CampaignClaimID,
		storeScopeID:    p.StoreScopeID,
		eventType:       p.EventType,
		reason:          p.Reason,
		templateID:      p.TemplateID,
		userIDs:         p.UserIDs,
		quantityPerUser: p.QuantityPerUser,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		// 唯一可能回 ErrNoRows 的就是那次模板查询：幂等那次的 ErrNoRows 是被判掉的分支，
		// 两条 INSERT ... RETURNING 一定回行。所以这里可以直接翻。
		return nil, ErrTemplateUnavailable
	}
	return out, err
}

// issue 是发券的事务体。三种发券都从这里出去，差异全在 spec 里。
func (r *postgresRepository) issue(ctx context.Context, spec issueSpec) (*dto.IssueCouponsResponse, error) {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	var existingHash string
	var existingResponse []byte
	err = tx.QueryRow(ctx, `SELECT request_hash,response FROM coupon_idempotency_keys WHERE scope=$1 AND idempotency_key=$2 FOR UPDATE`, spec.scope, spec.key).Scan(&existingHash, &existingResponse)
	if err == nil {
		if existingHash != spec.hash {
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
	if _, err = tx.Exec(ctx, `INSERT INTO coupon_idempotency_keys(scope,idempotency_key,request_hash,resource_type,status) VALUES($1,$2,$3,'coupon_batch','processing')`, spec.scope, spec.key, spec.hash); err != nil {
		return nil, err
	}
	var templateID, typeCode string
	var face, minPurchase int64
	var redemptionType, validityMode string
	var validFrom, validTo *time.Time
	var validDays *int
	var total int64
	// 模板必须是 active + approved 的：还没审过的模板发出去，等于绕过审核那道闸。
	err = tx.QueryRow(ctx, `SELECT t.id, ct.code, t.face_value,t.min_purchase_amount,t.redemption_type,t.validity_mode,t.valid_from,t.valid_to,t.valid_days,t.total_quantity FROM coupon_templates t JOIN coupon_types ct ON ct.id=t.coupon_type_id WHERE t.id=$1 AND t.status='active' AND t.audit_status='approved' FOR UPDATE`, spec.templateID).Scan(&templateID, &typeCode, &face, &minPurchase, &redemptionType, &validityMode, &validFrom, &validTo, &validDays, &total)
	if err != nil {
		return nil, err
	}
	quantity := int64(len(spec.userIDs) * spec.quantityPerUser)
	if quantity > total {
		return nil, ErrInsufficientInventory
	}
	batchID := ""
	// batch_no 一起读回来：它是审计条目上的 TargetName。只留一个 UUID 的话，日志里那一行
	// 在后台界面上是一串看不懂的 id，而运营手里的批次号就是这个前缀 + 随机串。
	batchNo := ""
	err = tx.QueryRow(ctx, `INSERT INTO coupon_batches(template_id,batch_no,source,total_quantity,reserved_quantity,issued_quantity,status,request_id,created_by) VALUES($1,$2||substr(gen_random_uuid()::text,1,12),$3,$4,0,$4,'exhausted',$5,NULLIF($6,'')::uuid) RETURNING id::text, batch_no`, templateID, spec.batchPrefix, spec.source, quantity, spec.key, spec.actorID).Scan(&batchID, &batchNo)
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
	// 适用范围：默认抄模板自己的 scope（品牌/门店两层），**系统发券可以整份换掉**——店铺码
	// 活动那条路把券锁在活动门店上，见 GrantSystemCoupons 的调用方。
	var scopeRows []struct{ typ, id string }
	if spec.storeScopeID != "" {
		scopeRows = append(scopeRows, struct{ typ, id string }{"store", spec.storeScopeID})
	} else {
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
			rows.Close()
			return nil, err
		}
		rows.Close()
	}
	ids := make([]string, 0, quantity)
	for _, userID := range spec.userIDs {
		for j := 0; j < spec.quantityPerUser; j++ {
			var id string
			err = tx.QueryRow(ctx, `INSERT INTO user_coupons(template_id,batch_id,user_id,coupon_type_code,claim_type,issue_reason,campaign_claim_id,face_value,min_purchase_amount,redemption_type,valid_from,expired_at) VALUES($1,$2,$3,$4,$5,$6,NULLIF($7,'')::uuid,$8,$9,$10,COALESCE($11,NOW()),COALESCE($12,NOW()+make_interval(days=>COALESCE($13,30)))) RETURNING id::text`, templateID, batchID, userID, typeCode, spec.claimType, spec.reason, spec.campaignClaimID, face, minPurchase, redemptionType, validFrom, validTo, validDays).Scan(&id)
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
	payload, _ := json.Marshal(map[string]any{"batch_id": batchID, "user_coupon_ids": ids, "actor_id": spec.actorID})
	referenceID := spec.referenceID
	if referenceID == "" {
		referenceID = batchID
	}
	if _, err = tx.Exec(ctx, `INSERT INTO coupon_inventory_ledger(template_id,batch_id,reference_type,reference_id,quantity,operation,request_id) VALUES($1,$2,$3,$4,$5,'issue',$6)`, templateID, batchID, spec.referenceType, referenceID, quantity, spec.key); err != nil {
		return nil, err
	}
	if _, err = tx.Exec(ctx, `INSERT INTO coupon_state_transitions(aggregate_type,aggregate_id,from_status,to_status,reason,request_id,metadata) VALUES('batch',$1,'','active',$2,$3,$4)`, batchID, spec.reason, spec.key, payload); err != nil {
		return nil, err
	}
	// 审计只给后台人工发放写：发券是这个服务里最不可逆的一步（券一旦落到用户账上，收回来只能
	// 靠逐张作废），它是那条链路上最该留下痕迹的一环，先记它。系统发券由批次、库存流水与状态
	// 迁移三条只增不改的记录交代，见 NewPostgresRepository 上那段。
	if spec.audit {
		if err = r.recorder.Record(ctx, tx, audit.Entry{
			Module: "coupons", Action: spec.auditAction, Operation: spec.auditOperation,
			TargetType: "coupon_batch", TargetID: batchID, TargetName: batchNo,
			After: audit.Snapshot(map[string]any{
				"templateId": templateID, "couponTypeCode": typeCode, "faceValue": face,
				"quantity": quantity, "userCount": len(spec.userIDs),
				"quantityPerUser": spec.quantityPerUser, "reason": spec.reason,
			}),
		}); err != nil {
			return nil, err
		}
	}
	if _, err = tx.Exec(ctx, `INSERT INTO message_outbox(event_id,event_type,event_version,trace_id,payload) VALUES(gen_random_uuid()::text,$1,$2,$3,$4)`, spec.eventType, dto.EventVersion, spec.key, payload); err != nil {
		return nil, err
	}
	out := &dto.IssueCouponsResponse{BatchID: batchID, IssuedQuantity: int(quantity), UserCouponIDs: ids}
	encoded, _ := json.Marshal(out)
	if _, err = tx.Exec(ctx, `UPDATE coupon_idempotency_keys SET resource_id=$2,response=$3,status='succeeded' WHERE scope=$1 AND idempotency_key=$4`, spec.scope, batchID, encoded, spec.key); err != nil {
		return nil, err
	}
	if err = tx.Commit(ctx); err != nil {
		return nil, err
	}
	return out, nil
}
