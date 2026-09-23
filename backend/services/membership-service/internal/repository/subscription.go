package repository

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/panda-dev/panda-v2/backend/platform/audit"
	"github.com/panda-dev/panda-v2/backend/services/membership-service/internal/dto"
	"github.com/panda-dev/panda-v2/backend/services/membership-service/internal/model"
)

// 连续包月订阅（membership_subscriptions）的读写。
//
// # 一条订阅怎么来的、怎么走到终态
//
// 这张表建库时就在，但直到微信直连那一刀之前**没有读者也没有写者**：能创建一条订阅的只有
// 小程序端的签约（用户点「开通连续包月」→ 微信委托代扣签约 → 回头写 pending_sign/active），
// 而那条链路依赖微信直连（api.mch.weixin.qq.com/papay/*）。**没有签约，就扣不了款**——开一条
// 永远扣不到钱的订阅比没有订阅更糟，用户会以为续上了（见 cmd/main.go 开头那段）。那一刀落地
// 之后这条路才通，本文件因此长出了写路径：
//
//   - CreateSubscription —— 发起签约：支付服务那边协议建好之后落这一行（`pending_sign`），
//     同一事务里记一条 `subscribe` 流水，那条流水同时是这次点击的幂等凭据。
//   - SettleSubscription —— 收口：签约确认、后台「同步」、协议事件**三条路共用这一个入口**，
//     按目标状态把这一行改成 active 或 cancelled。
//   - CancelSubscription —— 后台人工取消（只改本地状态）。它与 SettleSubscription 的 cancelled
//     不是一回事：那个是我们主动掐掉，这个是**渠道说协议没了**、我们把本地纠正过来。
//
// 这三条写的都是**授权**（协议在不在）。**钱的结论**（某一期扣到了没有）在 charge.go 里
// （SettleCharge），它改的是同一张表的另外几列：计数器、下一次扣款时间、suspended。
//
// 「回渠道查签约状态」这件事本身不在这里，它在服务层（gRPC 调 payment-service），本文件只负责
// 把问回来的结论落成事实。
//
// 老后台自己也建不了订阅——它同样只能由小程序签约产生。所以后台那两个页面**列表为空是正常
// 的**（直到真有人在小程序里签），页面上有一句话说明（不说明的话下一个人会当它是 bug）。

// subscriptionColumns 的列顺序必须与 scanSubscription 逐字对应。
//
// 四个可空列的取舍与 memberships 的 legacy_id 同一条（见 membership.go 上的说明）：
// agreement_id / cancelled_by 是 UUID，用 COALESCE 到空串摊平，省掉每一处读
// 的判空；三个时间戳列保持 *time.Time——「没发生过」与「零值时刻」在那里是两件事，占用一个
// 真值位是省不掉的。
const subscriptionColumns = `id, membership_id, user_id, plan_id, status,
	COALESCE(agreement_id::text, ''), contract_code, price_cents, wechat_plan_id,
	period, period_count, next_charge_at, last_charge_at, charge_count,
	failed_count, consecutive_failed_count, suspended_at, cancel_at, cancel_reason,
	COALESCE(cancelled_by::text, ''), created_at, updated_at,
	COALESCE(order_id::text, ''), COALESCE(coffee_order_id::text, ''),
	COALESCE(campaign_claim_id::text, '')`

// SubscriptionRow 是后台列表要的一行：订阅本身 + 它所属套餐的名字。
//
// 名字来自 join（membership_plans 在**同一个库**里，不跨库），照抄老系统的「会员等级」那一列。
// 用套餐名而不是套餐编码：这一列是给人看的，而 plan_code 是给系统看的稳定标识。
//
// 不把 plan_name 塞进 model.Subscription：那个结构体逐一对应表上的列，一行就是一行。
type SubscriptionRow struct {
	*model.Subscription
	PlanName string
}

// ListSubscriptions 是后台订阅列表。
//
// 排序固定 created_at DESC：这个页面看的是「最近签的那些」，而签约时间是唯一一个所有筛选
// 组合下都说得通的排序键。
//
// **不筛 status 时排除 pending_sign**（老系统就是这么做的）：那条状态是「已下单、等签约
// 结果」的中间态，一屏未完成的单子对运营没有任何用处，而它们卡住的真正原因（用户没在微信
// 里点完）在这一列上也看不出来。
func (r *PostgresRepository) ListSubscriptions(ctx context.Context, q dto.SubscriptionQuery) ([]*SubscriptionRow, int, error) {
	const from = ` FROM membership_subscriptions s`
	where := subscriptionFilter(q)

	total, err := r.countRows(ctx, from, where)
	if err != nil {
		return nil, 0, mapPGError(err)
	}
	args, page := pageClause(where.args, q.Page, q.PageSize)
	rows, err := r.pool.Query(ctx, `SELECT `+prefixColumns(subscriptionColumns, "s")+`, p.name`+from+
		` JOIN membership_plans p ON p.id = s.plan_id`+where.sql()+
		` ORDER BY s.created_at DESC, s.id`+page, args...)
	if err != nil {
		return nil, 0, mapPGError(err)
	}
	defer rows.Close()

	subscriptions := make([]*SubscriptionRow, 0, q.PageSize)
	for rows.Next() {
		row, err := scanSubscriptionRow(rows)
		if err != nil {
			return nil, 0, mapPGError(err)
		}
		subscriptions = append(subscriptions, row)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, mapPGError(err)
	}
	return subscriptions, total, nil
}

// prefixColumns 给列清单里**裸列名**加上表别名。
//
// 列表要 join 套餐表，所以裸列名必须带前缀——两张表都有 id / created_at / updated_at，不加
// 前缀时 PostgreSQL 直接报「column reference is ambiguous」。写一份带前缀的列清单常量是更
// 省事，但那就有两份列清单要同步，而漏掉的那一列会以「扫描时少一列」的形式炸出来。
//
// **表达式那一列不能加前缀**：`s.COALESCE(...)` 是语法错误（PostgreSQL 报的是一句
// syntax error at or near "s."，而真正的原因在列清单里）。它们的内层列不加前缀也不会歧义
// ——agreement_id / cancelled_by / order_id / coffee_order_id / campaign_claim_id 这五个
// 名字在 membership_plans 上都不存在（那是 join 的另一张表）。
//
// 列清单是**参数**，不是从订阅那份常量里读的：这个函数一开始写死了 subscriptionColumns，
// 于是活动列表那句 prefixColumns(campaignColumns, "c") 拿到的是一串订阅列名，真库上回的是
// `column c.membership_id does not exist`——一个指向活动表的、完全说不通的报错。
func prefixColumns(columns, alias string) string {
	parts := strings.Split(columns, ",")
	for i, part := range parts {
		column := strings.TrimSpace(part)
		if strings.ContainsAny(column, "()") {
			parts[i] = column
			continue
		}
		parts[i] = alias + "." + column
	}
	return strings.Join(parts, ", ")
}

// subscriptionFilter 拼订阅列表的筛选条件。
func subscriptionFilter(q dto.SubscriptionQuery) whereClause {
	var where whereClause
	if userID := trimOrEmpty(q.UserID); userID != "" {
		where.add("s.user_id = $%d", userID)
	}
	if status := trimOrEmpty(q.Status); status != "" {
		where.add("s.status = $%d", status)
	} else {
		// 不带参数的条件用 addRaw（见 query.go 上那段说明）。
		where.addRaw("s.status <> 'pending_sign'")
	}
	return where
}

// SubscriptionStats 是订阅列表页头上那两张卡。
//
// 两个数都在 SQL 里数，**不把 active 全捞进内存再筛**：老系统就是全表扫描 + 内存排序（那个
// collection 连一个索引都没有），照抄过来等于把它的毛病一起抄过来。
//
// 「待续费」的判据（status='active' AND next_charge_at <= now）与 model.Subscription.IsDue
// 逐字相同，但这里不用它——理由同上。
func (r *PostgresRepository) SubscriptionStats(ctx context.Context, now time.Time) (dto.SubscriptionStats, error) {
	var stats dto.SubscriptionStats
	err := r.pool.QueryRow(ctx, `SELECT
			COUNT(*) FILTER (WHERE status = 'active'),
			COUNT(*) FILTER (WHERE status = 'active' AND next_charge_at <= $1)
		FROM membership_subscriptions`, utc(now)).Scan(&stats.ActiveCount, &stats.DueCount)
	if err != nil {
		return dto.SubscriptionStats{}, mapPGError(err)
	}
	return stats, nil
}

// ListDueSubscriptions 取一批到点该扣的订阅，按到期时间升序。
//
// 判据与 model.Subscription.IsDue 逐字相同（`status='active' AND next_charge_at <= now`），
// 走那条 `membership_subscriptions_charge_idx`（next_charge_at 的部分索引，WHERE
// status='active'）——这条查询正是那条索引存在的理由。
//
// # 为什么没有 FOR UPDATE
//
// 调用方（service.ChargeDue）拿着这一批要去调支付服务，那是一次出网调用。握着几百行的行锁
// 等它回来，会把整个库的写全堵在这一批上（到期扫描那条路是 FOR UPDATE SKIP LOCKED，因为它
// **在同一批里就把活干完了**；这条路干不完）。真正的并发保护也不在这里：同一期只扣一次是
// payment-service 库上那条 `UNIQUE (agreement_id, biz_period)`，本服务多扫一遍、多副本同时
// 扫，后果只是多发几次注定被它挡回去的请求。
//
// 反过来，这里**不能**用 SKIP LOCKED 假装互斥：它挡不住另一个副本（各自的连接看到的是各自
// 的锁），却会让人以为已经挡住了。
//
// limit <= 0 时取一批的默认值，与 ExpireDue 同一个兜底（一个 0 会变成 LIMIT 0，表现是
// 「扫描一直在跑、一条也不处理」）。
func (r *PostgresRepository) ListDueSubscriptions(ctx context.Context, now time.Time, limit int) ([]*SubscriptionRow, error) {
	if limit <= 0 {
		limit = DefaultChargeBatch
	}
	rows, err := r.pool.Query(ctx, `SELECT `+prefixColumns(subscriptionColumns, "s")+`, p.name
		FROM membership_subscriptions s JOIN membership_plans p ON p.id = s.plan_id
		WHERE s.status = $1 AND s.next_charge_at <= $2
		ORDER BY s.next_charge_at, s.id
		LIMIT $3`, model.SubscriptionStatusActive, utc(now), limit)
	if err != nil {
		return nil, mapPGError(err)
	}
	defer rows.Close()

	due := make([]*SubscriptionRow, 0, limit)
	for rows.Next() {
		row, err := scanSubscriptionRow(rows)
		if err != nil {
			return nil, mapPGError(err)
		}
		due = append(due, row)
	}
	if err := rows.Err(); err != nil {
		return nil, mapPGError(err)
	}
	return due, nil
}

// DefaultChargeBatch 是到期扣款扫描的默认批量上限。
//
// 它比到期扫描（200）小一个量级，因为两条路的每一行代价不同：到期扫描每一行是一条本库的
// UPDATE，代扣每一行是一次跨服务的 gRPC（再往下还挂着一次渠道的 mTLS 出网）。一批两百条
// 意味着一个 tick 里的最坏情况是两百次出网调用，而它的超时是分钟级的——积压时该做的是
// 多跑几个 tick，不是让一个 tick 卡在那里。
const DefaultChargeBatch = 50

// GetSubscription 取一条订阅。不存在时回 ErrSubscriptionNotFound（→404），不是一个空对象。
//
// 它同时被列表页的详情抽屉与取消动作的前置读取用；后者读的是同一行，但锁在
// CancelSubscription 里加（这里不加锁，只读不写）。
func (r *PostgresRepository) GetSubscription(ctx context.Context, id string) (*SubscriptionRow, error) {
	return scanSubscriptionRow(r.pool.QueryRow(ctx, `SELECT `+prefixColumns(subscriptionColumns, "s")+`, p.name
		FROM membership_subscriptions s JOIN membership_plans p ON p.id = s.plan_id
		WHERE s.id = $1`, id))
}

// CancelSubscription 取消一条订阅：**只改本地状态**。
//
// 三件事写在同一条 UPDATE 里，不是三步：
//
//   - `status='cancelled'` 与 `cancel_at` 库上有一条 CHECK 钉着（cancelled 必须有 cancel_at），
//     分两次写的话第一次必然撞上它。
//   - `cancelled_by` 记发起人（后台运营/风控），`cancel_reason` 记原因——一条被谁、为什么
//     取消的记录是这个动作唯一的留痕。
//
// **前置状态只能是 active 或 suspended**：expired/cancelled 已经结束了，pending_sign 还没签成。
// 放行 suspended 是因为它需要出口——连续扣款失败到停扣之后，这一行除了「等下一次扣款成功自己
// 回来」没有任何别的可能，而想彻底不续的用户与想收尾的运营都只能靠这个动作。
//
// **它同事务把 memberships.auto_renew 拨灭**（见 applyAutoRenew）：一行订阅都不剩了，开关还亮
// 着就是在骗人。
func (r *PostgresRepository) CancelSubscription(ctx context.Context, p CancelSubscriptionParams) (*SubscriptionRow, error) {
	var result *SubscriptionRow
	err := r.inTx(ctx, func(tx pgx.Tx) error {
		var current *model.Subscription
		var planName string
		row, err := scanSubscriptionRow(tx.QueryRow(ctx, `SELECT `+prefixColumns(subscriptionColumns, "s")+`, p.name
			FROM membership_subscriptions s JOIN membership_plans p ON p.id = s.plan_id
			WHERE s.id = $1 FOR UPDATE OF s`, p.SubscriptionID))
		switch {
		case errors.Is(err, ErrSubscriptionNotFound):
			return err
		case err != nil:
			return err
		}
		current, planName = row.Subscription, row.PlanName

		// 只有 active 与 suspended 能取消。**suspended 是这一刀才放行的**：一条连续失败到停扣
		// 的订阅今天没有任何别的出口（见 model.SubscriptionStatusSuspended），而它是这一组状态里
		// 最需要出口的一个——用户想彻底不续了、运营看着一条永远不会再动的记录，两个人手上都没有
		// 按钮。其余三档（pending_sign 还没签成、cancelled / expired 本来就结束了）放行只会让
		// 「取消」变成一个没有意义的动作。
		if current.Status != model.SubscriptionStatusActive &&
			current.Status != model.SubscriptionStatusSuspended {
			return ErrSubscriptionNotCancellable
		}

		updated, err := scanSubscription(tx.QueryRow(ctx, `UPDATE membership_subscriptions
			SET status = 'cancelled', cancel_at = $2, cancel_reason = $3, cancelled_by = $4,
			    updated_at = NOW()
			WHERE id = $1
			RETURNING `+subscriptionColumns,
			p.SubscriptionID, utc(p.OccurredAt), p.Reason, optionalID(p.CancelledBy)))
		if err != nil {
			return mapPGError(err)
		}

		// 订阅没了，自动续费那个开关就不能还亮着（见 applyAutoRenew 的说明）。同事务，所以不
		// 存在「后台取消成功了、会员中心还显示自动续费：开」那一格。
		if err := applyAutoRenew(ctx, tx, current.MembershipID, false,
			p.CancelledByType, p.CancelledBy, "unsubscribed", p.OccurredAt); err != nil {
			return err
		}

		// 记审计：这是**人工**掐掉一个用户的钱袋子，与冻结/撤销同一档（§11.6「影响用户资产
		// 归属的人工操作」）。取消本身不退款、不改会员有效期，但它决定了这个人下个月还会不会
		// 被扣——事后要能回答「是谁在什么时候把它关了的」。
		if p.CancelledByType == model.OperatorAdmin && p.CancelledBy != "" {
			if err := r.recorder.Record(ctx, tx, audit.Entry{
				ActorID: p.CancelledBy, Module: "membership",
				Action: model.ChangeUnsubscribe, Operation: "取消包月订阅",
				TargetType: "membership_subscription", TargetID: updated.ID, TargetName: planName,
				Before: audit.Snapshot(map[string]any{
					"status": current.Status, "nextChargeAt": current.NextChargeAt,
				}),
				After: audit.Snapshot(map[string]any{
					"status": updated.Status, "cancelAt": updated.CancelAt, "reason": p.Reason,
				}),
			}); err != nil {
				return err
			}
		}

		result = &SubscriptionRow{Subscription: updated, PlanName: planName}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

// CancelSubscriptionParams 是「把一条订阅解掉」要写下来的事实。
//
// 与 SettleParams 分开是**因为发起人不同**，不是因为落库的字段不同：这一条是**我们这边有人
// 明确说不续了**（用户在小程序点、运营在后台点），所以一定有发起人、一定要写 cancelled_by；
// 那一条是渠道的事实（协议被作废、用户在微信里解了），本地只是纠正，没有发起人。
type CancelSubscriptionParams struct {
	SubscriptionID string
	// CancelledBy / CancelledByType 是发起人与他的类型，见 CancelSubscription 的说明。
	CancelledBy     string
	CancelledByType string
	Reason          string
	OccurredAt      time.Time
}

// scanSubscription 把一行读成 model.Subscription。列的顺序必须与 subscriptionColumns 逐字对应。
func scanSubscription(row scanner) (*model.Subscription, error) {
	var s model.Subscription
	if err := row.Scan(
		&s.ID, &s.MembershipID, &s.UserID, &s.PlanID, &s.Status,
		&s.AgreementID, &s.ContractCode, &s.PriceCents, &s.WechatPlanID,
		&s.Period, &s.PeriodCount, &s.NextChargeAt, &s.LastChargeAt, &s.ChargeCount,
		&s.FailedCount, &s.ConsecutiveFailedCount, &s.SuspendedAt, &s.CancelAt, &s.CancelReason,
		&s.CancelledBy, &s.CreatedAt, &s.UpdatedAt,
		&s.OrderID, &s.CoffeeOrderID, &s.CampaignClaimID,
	); err != nil {
		return nil, err
	}
	return &s, nil
}

// ============================================================
// 签约：写路径
// ============================================================

// CreateSubscriptionParams 是「发起一次签约」落库所需的全部输入。
//
// 每个字段都来自**本域的权威来源**（套餐、会员）或**支付服务刚回的那份协议**，没有一个是
// 客户端直接给的钱或时长：客户端能影响的只有「选哪一款套餐」与那个幂等号。
type CreateSubscriptionParams struct {
	UserID string
	PlanID string
	// PlanName 由调用方给：服务层手上就是那份刚校验过的套餐（它要 auto_renew 与
	// wechat_plan_id），这里再 join 一次只是为了拿名字。
	PlanName string
	// AgreementID 是 payment_agreements.id，仅作值引用。
	AgreementID string
	// AgreementNo 是**商户协议号**，同时就是交给渠道的 contract_code，签约这一刻就写进这一行。
	//
	// 它是事后回渠道查协议的唯一钥匙（`papay/querycontract` 的 contract_code），所以**写进来的
	// 时刻比签约成功还早**：一份挂在 pending_sign 上的协议同样要能被查、被纠正。
	AgreementNo  string
	PriceCents   int64
	WechatPlanID string
	Period       string
	PeriodCount  int32
	// OrderID / CoffeeOrderID / CampaignClaimID 是「这次签约从哪来」，都可以为空（见 dto 里
	// 那三个字段的说明）。它们只喂后台那两列，不参与任何判定。
	OrderID         string
	CoffeeOrderID   string
	CampaignClaimID string
	// RequestID 是这次点击的幂等键，必填（服务层校过）。它写进 membership_changes.request_id，
	// 那条部分唯一索引是「重试不会签出两份协议」的最后一道闸门。
	RequestID string
	// OccurredAt 是签约**发起**时刻，取自服务时钟。
	OccurredAt time.Time
}

// CreateSubscriptionOutcome 是一次发起签约的结论。
type CreateSubscriptionOutcome struct {
	Subscription *SubscriptionRow
	// Replayed 为真表示这个 requestId 之前已经落过一条（同一次点击的重试），这一次什么都没写
	// ——回的是**上一次那一条**。见 CreateSubscription 里那段说明。
	Replayed bool
}

// CreateSubscription 落一条订阅：`pending_sign` + 一条 `subscribe` 流水，同一个事务。
//
// # 它为什么能建，而在这之前为什么不能
//
// 建这条行之前，支付服务那边**已经建好了一份协议**（服务层先调 payment 的 CreateAgreement，
// 拿回协议号才走到这里）。所以这一行不是「一个意向」，它指着渠道上一份真实存在的待签协议——
// 用户拿着跳转参数过去点一次「同意」，它就生效。反过来先建行再建协议会让一个建协议失败的
// 点击在库里留下一条永远签不成的订阅，而它是 live 的，会把这个人之后每一次签约都挡住。
//
// # 一个会员只能有一条活着的订阅
//
// 两道防线，判据是同一条（`membership_subscriptions_live_unique`）：这里的先查是为了给出一句
// 说得清的 409，库上那条唯一索引是并发下的最后一道——两个请求同时读到「没有活得订阅」时，
// 只有一条能插进去。**它的反面是扣两次钱**，所以判据必须在锁内。
//
// # 同一次点击重试 = 回上一次那一条
//
// 事务的第一件事是锁住这个用户的会员行，之后才去判「这次点击是不是已经落过了」。顺序反过来
// 就会漏：两个并发请求各自读到「没有流水」、各自建一条，其中一条撞上库上的索引报 500——而
// 它们本该是「同一次点击」，第二次的结果必须与第一次一模一样。
//
// 反查用的是 membership_changes 上那条 `membership_changes_request_unique`
// ——`(user_id, change_type, request_id)` 部分唯一索引（它建表时就在，这一刀第一次真用上）。
// **它同时是「这个 requestId 用过没有」的唯一凭据**
// ——订阅表上没有这一列，而那条流水与这条订阅是同一个事务写的。
func (r *PostgresRepository) CreateSubscription(ctx context.Context, p CreateSubscriptionParams) (*CreateSubscriptionOutcome, error) {
	var outcome *CreateSubscriptionOutcome
	err := r.inTx(ctx, func(tx pgx.Tx) error {
		occurredAt := utc(p.OccurredAt)
		// 会员行必须先存在：membership_id 是 NOT NULL 且带外键，而本域的规矩是「先有会员，
		// 才谈得上续费」（见 service.CreateSubscription）。锁住它同时把同一次点击的两个并发
		// 请求排成一队。
		membership, err := membershipForUpdate(ctx, tx, p.UserID)
		if err != nil {
			return err
		}

		previous, err := subscriptionByChangeRequest(ctx, tx, p.UserID, p.RequestID)
		switch {
		case err == nil:
			if previous.IsLive() {
				outcome = &CreateSubscriptionOutcome{Subscription: previous, Replayed: true}
				return nil
			}
			// 这个 requestId 确实用过，但那条订阅已经结束了（被取消 / 解约）。一次网络重试
			// 不可能横跨整个签约与解约，所以走到这里说明客户端把一个旧 requestId 又发了一次。
			// 照着重放回一条已经取消的订阅会让用户以为「签好了」，重新建一条又会撞流水上的
			// 唯一索引，所以停下来让他重新发起。
			return ErrSubscriptionRequestUsed
		case !errors.Is(err, ErrSubscriptionNotFound):
			return err
		}

		if live, err := liveSubscription(ctx, tx, membership.ID); err != nil {
			return err
		} else if live != nil {
			return ErrLiveSubscriptionExists
		}

		subscription, err := scanSubscription(tx.QueryRow(ctx, `INSERT INTO membership_subscriptions
			(membership_id, user_id, plan_id, status, agreement_id, contract_code, price_cents,
			 wechat_plan_id, period, period_count, order_id, coffee_order_id, campaign_claim_id)
			VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13)
			RETURNING `+subscriptionColumns,
			membership.ID, p.UserID, p.PlanID, model.SubscriptionStatusPendingSign,
			optionalID(p.AgreementID), trimOrEmpty(p.AgreementNo), p.PriceCents,
			trimOrEmpty(p.WechatPlanID), p.Period, p.PeriodCount,
			optionalID(p.OrderID), optionalID(p.CoffeeOrderID), optionalID(p.CampaignClaimID)))
		if err != nil {
			return mapPGError(err)
		}

		// 流水记的是**会员**身上发生的事，所以挂 membership_id 而不是订阅 id——这条流水同时
		// 是时间线上那句「他签了连续包月」，也是这次点击的幂等凭据（见函数说明）。
		//
		// from/to status 留空：签约起止都不改会员的状态（签约生效也不改，那个人的会员是从另一
		// 条路来的），这一行只是「发生过签约」。
		if err := insertChange(ctx, tx, model.Change{
			MembershipID: membership.ID,
			UserID:       p.UserID,
			ChangeType:   model.ChangeSubscribe,
			PlanID:       optionalText(p.PlanID),
			// 系统记的：这一次是**用户点了一下**，但真正写下这一行的是服务端的编排（客户端没有
			// 直接写库的能力）。老的四个后台动作里 user 那一档指的是「用户自己按的开关」，与这里
			// 同一类，只是没有 user_id 可填（签约的人就是这一行的 user_id 本身）。
			OperatorType: model.OperatorSystem,
			Reason:       "sign_requested",
			Metadata:     mustJSON(map[string]any{"agreementId": p.AgreementID, "agreementNo": p.AgreementNo}),
			RequestID:    p.RequestID,
			OccurredAt:   occurredAt,
		}); err != nil {
			return err
		}

		outcome = &CreateSubscriptionOutcome{
			Subscription: &SubscriptionRow{Subscription: subscription, PlanName: p.PlanName},
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return outcome, nil
}

// FindSubscriptionByChangeRequest 是「这次点击之前是不是已经落过一条订阅」的读路径。
//
// 服务层在调支付服务**之前**先问它一次：同一个 requestId 的重试不该再建一份协议，也不该被
// 「你已经有一条活着的订阅了」挡回去——那条活着的订阅正是这次点击自己建的。
//
// 查不到时回 ErrSubscriptionNotFound（调用方按 errors.Is 判，这是本包读路径的惯例）。
func (r *PostgresRepository) FindSubscriptionByChangeRequest(ctx context.Context, userID, requestID string) (*SubscriptionRow, error) {
	return subscriptionByChangeRequest(ctx, r.pool, userID, requestID)
}

// subscriptionByChangeRequest 按 (user_id, change_type='subscribe', request_id) 反查那条订阅。
//
// **只看活着的订阅**（pending_sign / active / suspended）：一条会员理论上可以先后有过好几条
// 订阅（取消一条、再签一条），而 join 走的是 membership_id，不筛的话会把历史那几条一起捞出来。
// `membership_subscriptions_live_unique` 保证活着的最多一条，所以这个查询是确定的，不需要
// ORDER BY。
//
// 代价是「这个 requestId 用过、且那条订阅已经结束」时这里回查不到（回 ErrSubscriptionNotFound）
// ——那个分叉由调用方判，见 CreateSubscription 里那段说明。
func subscriptionByChangeRequest(ctx context.Context, q querier, userID, requestID string) (*SubscriptionRow, error) {
	if strings.TrimSpace(requestID) == "" {
		// 空 key 不可能命中：membership_changes_request_unique 的 WHERE 也把空串排除在外。
		// 直接回「没有」，
		// 不去跑一条注定扫全表的查询。
		return nil, ErrSubscriptionNotFound
	}
	return scanSubscriptionRow(q.QueryRow(ctx, `SELECT `+prefixColumns(subscriptionColumns, "s")+`, p.name
		FROM membership_subscriptions s
		JOIN membership_plans p ON p.id = s.plan_id
		WHERE s.status IN ($3, $4, $5)
		  AND s.membership_id = (SELECT membership_id FROM membership_changes
		                          WHERE user_id = $1 AND change_type = $2 AND request_id = $6)`,
		userID, model.ChangeSubscribe,
		model.SubscriptionStatusPendingSign, model.SubscriptionStatusActive, model.SubscriptionStatusSuspended,
		requestID))
}

// GetLiveSubscription 取这个会员活着的那条订阅。没有时回 ErrSubscriptionNotFound。
//
// 「活着」的判据在 model.Subscription.IsLive 上，不在这里写 `== active`：pending_sign 也算活着，
// 否则用户连点两次「开通连续包月」就会签出两份协议。
func (r *PostgresRepository) GetLiveSubscription(ctx context.Context, membershipID string) (*model.Subscription, error) {
	subscription, err := liveSubscription(ctx, r.pool, membershipID)
	if err != nil {
		return nil, err
	}
	if subscription == nil {
		return nil, ErrSubscriptionNotFound
	}
	return subscription, nil
}

// liveSubscription 是上面那个判断的裸查询版：没有时回 (nil, nil) 而不是错误。
//
// 两个形状都要有不是重复：读路径（GetLiveSubscription）要的是「没有就是 404」，而写路径内部
// 拿它当**一块要判的空缺**（没有才继续建），在那里回一个错误只会让每一处都写一句
// `if !errors.Is(err, ErrSubscriptionNotFound)`。
func liveSubscription(ctx context.Context, q querier, membershipID string) (*model.Subscription, error) {
	subscription, err := scanSubscription(q.QueryRow(ctx, `SELECT `+subscriptionColumns+`
		FROM membership_subscriptions
		WHERE membership_id = $1 AND status IN ($2, $3, $4)`,
		membershipID,
		model.SubscriptionStatusPendingSign, model.SubscriptionStatusActive, model.SubscriptionStatusSuspended))
	if err != nil {
		if isNoRows(err) {
			return nil, nil
		}
		return nil, mapPGError(err)
	}
	return subscription, nil
}

// SettleParams 是一次「收口」要的全部输入：把一条订阅改成它**现在实际该处于**的状态。
//
// 三个调用方共用它——小程序的「确认」（用户从微信回来）、后台的「同步」（运营点一下）、
// 协议事件（payment-service 推过来的 signed / terminated）。共用一个入口是因为**判据只有
// 一套**：签约状态是渠道的事实，我们这边无论从哪条路得知它，落库的规则都该一模一样。
type SettleParams struct {
	// SubscriptionID 与 AgreementID 二选一：前两个调用方手上是订阅 id，事件消费手上只有协议 id
	// （事件体里没有订阅 id）。两个都给时以 SubscriptionID 为准。
	SubscriptionID string
	AgreementID    string
	// Target 是这一行最终该处于的状态，只有两个取值：active / cancelled。
	Target string
	// AgreementNo 是支付服务回给我们的**商户协议号**，Target=active 时用来核对「回来的就是这一行
	// 记着的那份协议」（见 ErrAgreementMismatch）。另外两个目标下不读它。
	AgreementNo string
	// CancelReason 是解约原因码，Target=cancelled 时写进 cancel_reason。
	CancelReason string
	// CancelledBy 是**解约发起人**：小程序里点「关闭自动续费」的那个人（userID），或后台点
	// 「同步」发现微信侧已解约时留空。
	//
	// 留空与填上是有区别的：它回答的是「这次解约是我们这边谁主动做的，还是渠道那边的事实」。
	// 渠道作废、后台同步这两种情况下本地只是**纠正**，没有发起人；照填一个运营的 id 进去会让
	// 事后看到这一列的人以为是那个运营解掉了用户的约。
	CancelledBy string
	// CancelledByType 是发起人的类型（model.OperatorUser / OperatorAdmin / OperatorSystem），
	// 决定自动续费那条流水上的 operator_type。与 CancelledBy 成对给：一个 id 分不出「用户自己
	// 点的」与「运营替他点的」，而这两件事在时间线上必须看得出区别。
	CancelledByType string
	// ProviderState 是渠道的原话（signed / terminated / pending），**不翻译**：它只进审计的
	// after 快照，排查时先看的那一眼就是它。
	ProviderState string
	OccurredAt    time.Time
	// AdminID 非空表示这次是**人**点的「同步」：写一条审计。另外两条路（用户确认、事件消费）
	// 留空——那不是人工操作，痕迹在 payment_notifications 与 outbox 里。
	AdminID string
	TraceID string
}

// SettleSubscription 按目标状态收口一条订阅，返回收口后的那一行与「这次有没有真的改」。
//
// # 它不判断「该不该改」，只执行结论
//
// 「该不该」是渠道说了算的（签约查询的结果、协议事件的目标状态），这个函数把那个结论落成事实。
// 唯一的例外是**不变式的守卫**：不允许把一条已经结束的订阅改回 active（见下）。
//
// # 幂等靠「已经是这个状态了」
//
// 三条路都会重复调用它（用户连点确认、运营点两次同步、事件重投），而它们的答案必须是同一个。
// 所以第一件事就是比对当前状态：已经等于 Target 就短路，一个字节都不写——特别注意
// **不能顺手再写一遍 next_charge_at**，那会让「重投一条三个月前的旧事件」把下一次扣款时间
// 往后推三个月（base 取的是 max(会员到期日, 这一次的时刻)）。
//
// # 已结束的订阅不会被协议事件叫回来
//
// active 只能从 pending_sign 来。一条已经 cancelled / expired 的订阅收到「协议生效了」时报错
// 而不是照做：那说明有人的状态已经乱了（协议在解约之后又活过来，或者两个不同的协议号指向了
// 同一行），而那正是要人来看的事——照做会把一条已经停掉的订阅重新接上扣款。
func (r *PostgresRepository) SettleSubscription(ctx context.Context, p SettleParams) (*SubscriptionRow, bool, error) {
	var (
		result  *SubscriptionRow
		changed bool
	)
	err := r.inTx(ctx, func(tx pgx.Tx) error {
		current, err := lockSubscription(ctx, tx, p.SubscriptionID, p.AgreementID)
		if err != nil {
			return err
		}
		if current.Status == p.Target {
			// 已经是这个状态：这次核查什么都没改（见函数说明）。
			result = current
			return nil
		}

		switch p.Target {
		case model.SubscriptionStatusActive:
			if err := guardSettleToActive(current, p.AgreementNo); err != nil {
				return err
			}
		case model.SubscriptionStatusCancelled:
			// 取消**不接受 expired**：一条走完的订阅（会员到期不再续）是个正常的终点，把它改成
			// cancelled 等于说「有人解约了」，而那是两件事。今天没有代码写 expired，这条判据是
			// 留给代扣那一刀的（连续扣款失败到阈值之后怎么收场）。
			if current.Status == model.SubscriptionStatusExpired {
				result = current
				return nil
			}
		default:
			// 目标状态是调用方给的常量之一，走到这里是我们自己写错了（比如想置 suspended）——
			// 让它当场炸，而不是静默不改。
			return fmt.Errorf("membership: unsupported settle target %q", p.Target)
		}

		updated, err := applySettle(ctx, tx, current, p)
		if err != nil {
			return err
		}

		// 审计只对**人**点的那一下记（后台的「同步」）：它改的是「这个人下个月还会不会被扣款」，
		// 而方案 §11.6 把影响用户资产归属的人工操作列进必审清单。用户自己确认签约、事件推过来
		// 的系统动作不记——它们的痕迹在渠道通知与 outbox 里。
		if p.AdminID != "" {
			if err := r.recorder.Record(ctx, tx, audit.Entry{
				ActorID: p.AdminID, Module: "membership",
				Action: p.Target, Operation: "同步包月签约状态",
				TargetType: "membership_subscription", TargetID: updated.ID, TargetName: updated.PlanName,
				Before: audit.Snapshot(map[string]any{"status": current.Status, "contractCode": current.ContractCode}),
				After: audit.Snapshot(map[string]any{
					"status": updated.Status, "providerState": p.ProviderState,
					"contractCode": updated.ContractCode, "cancelReason": updated.CancelReason,
				}),
			}); err != nil {
				return err
			}
		}

		result, changed = updated, true
		return nil
	})
	if err != nil {
		return nil, false, err
	}
	return result, changed, nil
}

// guardSettleToActive 是「把这一行改成生效中」之前的两条守卫。
//
// 一、**只有 pending_sign 能变 active**（理由见 SettleSubscription 的说明）。
//
// 二、回来的协议号必须与这一行记着的那一份**对得上**。这一行的 contract_code 是签约那一刻写死
// 的，回来的应该是同一份协议；对不上说明有人拿错了协议号去问、或者支付库那一行被改过——两种都
// 要人看。照着结论往下写会把**别人的协议**绑到这个人的订阅上，那比停下来更糟（那意味着这个人
// 以后被扣的钱进了另一个人的账）。
func guardSettleToActive(current *SubscriptionRow, agreementNo string) error {
	if current.Status != model.SubscriptionStatusPendingSign {
		return ErrSubscriptionNotSettleable
	}
	reported := strings.TrimSpace(agreementNo)
	if reported == "" || reported != current.ContractCode {
		return ErrAgreementMismatch
	}
	return nil
}

// applySettle 写状态，并把 active 那一支需要的 next_charge_at 算出来。
//
// # 下一次扣款时间的那条规则
//
// **next_charge_at 就是「这一段权益的结束时刻」**——这个列的含义在代扣那一刀上是同一个
// （见 charge.go「下一次扣款 = 新的会员到期日」），所以这里只有两种取值：
//
//   - **会员还在有效期内：取那个到期日**，不加任何东西。签约本身不发放权益（这条流水起止
//     都不改会员，见 repository.CreateSubscription），所以它不产生新的一段——加了就变成
//     「权益结束之后再过一整期才来扣第一次」。用户那一期持币却没有会员，界面上自动续费还
//     亮着，而库里没有一行会去纠正它。
//   - **会员已过期（或没有剩余）：这一刻 + 一个周期**。没有权益可接，这一期从第一次扣款的
//     那一刻起算。
//
// 判据与 renewMembership 里决定叠加基点的那条逐字相同（`expire_at` 在不在这一刻之后），
// 区别只在 renewMembership 会在那个基点上真加一个周期——那一次确实发了权益。两处共用一条
// 判据的理由：它们回答的是同一个问题「这一段权益从哪儿接着算」，而两套判据迟早在某次调价或
// 某次补签上走偏，走偏的表现是「用户签完当月被扣了两次」。
//
// 会员行在**同一个事务里锁着读**：它是这条规则唯一的输入，读到别人正在改的中间值会让
// next_charge_at 差出一整个周期。
func applySettle(ctx context.Context, tx pgx.Tx, current *SubscriptionRow, p SettleParams) (*SubscriptionRow, error) {
	occurredAt := utc(p.OccurredAt)
	switch p.Target {
	case model.SubscriptionStatusActive:
		membership, err := membershipByIDForUpdate(ctx, tx, current.MembershipID)
		if err != nil {
			return nil, err
		}
		// 先按「没有剩余」算，会员还没过期再改成它的到期日。**顺序不能反**：写成「先取基点、
		// 再加一个周期」会在签约时多加一整期（见上面那段规则），而那是这一条出过的错。
		nextChargeAt := model.AddPeriod(occurredAt, current.Period, current.PeriodCount)
		if membership.ExpireAt.After(occurredAt) {
			nextChargeAt = membership.ExpireAt
		}
		updated, err := scanSubscription(tx.QueryRow(ctx, `UPDATE membership_subscriptions
			SET status = $2, contract_code = $3, next_charge_at = $4, updated_at = NOW()
			WHERE id = $1
			RETURNING `+subscriptionColumns,
			current.ID, model.SubscriptionStatusActive, trimOrEmpty(p.AgreementNo), utc(nextChargeAt)))
		if err != nil {
			return nil, mapPGError(err)
		}
		// 签约生效即开自动续费：这两件事本来就是一件事的两半（见 applyAutoRenew）。同事务，
		// 所以不存在「刚签完约、会员中心显示自动续费：关」那一格中间状态。
		if err := applyAutoRenew(ctx, tx, current.MembershipID, true,
			model.OperatorSystem, "", "signed", occurredAt); err != nil {
			return nil, err
		}
		return &SubscriptionRow{Subscription: updated, PlanName: current.PlanName}, nil

	case model.SubscriptionStatusCancelled:
		updated, err := scanSubscription(tx.QueryRow(ctx, `UPDATE membership_subscriptions
			SET status = $2, cancel_at = $3, cancel_reason = $4, cancelled_by = $5, updated_at = NOW()
			WHERE id = $1
			RETURNING `+subscriptionColumns,
			current.ID, model.SubscriptionStatusCancelled, occurredAt, trimOrEmpty(p.CancelReason),
			optionalID(p.CancelledBy)))
		if err != nil {
			return nil, mapPGError(err)
		}
		// cancelled_by 为空是**渠道那边**发生的解约（用户去微信里解了，或者协议被作废），我们
		// 只是把本地纠正过来——点「同步」的那个运营不是解约发起人，他是发现它的那个人，留痕在
		// 审计里。有值就是小程序里那个人自己关的（见 SettleParams.CancelledBy）。
		//
		// 不管哪种，自动续费都要跟着关：一条订阅都不剩了，那个开关还亮着就是在骗人。
		if err := applyAutoRenew(ctx, tx, current.MembershipID, false,
			orSystem(p.CancelledByType), p.CancelledBy, "unsubscribed", occurredAt); err != nil {
			return nil, err
		}
		return &SubscriptionRow{Subscription: updated, PlanName: current.PlanName}, nil

	default:
		return nil, fmt.Errorf("membership: unsupported settle target %q", p.Target)
	}
}

// orSystem 把空的操作人类型补成 system：能走到「没人给类型」的只有渠道那条路（协议被作废、
// 后台同步发现微信侧已解约），那两次都是系统在纠正本地，不是谁按的。
func orSystem(operatorType string) string {
	if operatorType == "" {
		return model.OperatorSystem
	}
	return operatorType
}

// applyAutoRenew 把 memberships.auto_renew 拨到与订阅一致的位置，真拨动了才写一条流水。
//
// # 这个开关是订阅的投影，不是第二份事实
//
// `memberships.auto_renew` 是**用户唯一看得见**的那一半（会员中心那个「自动续费」开关），
// 而权威的那一半是 `membership_subscriptions` 里的行：存在一条活着的订阅（active / suspended）
// 就是真，一条都没有就是假。两处分开写迟早在某次失败的半步上错开，而错开之后**没有任何自动
// 修复的路径**——用户看到「自动续费：已关闭」而代扣照跑，或者反过来看到「开启」而名下什么都
// 没有。所以凡是订阅状态变了的地方，都在**同一个事务里**把它拨过来。
//
// # 它只拨真变了的那一次
//
// 用 `auto_renew <> $2` 做条件而不是无条件 UPDATE：协议事件会被重投（见 WithInbox 的去重，
// 它挡的是同一份报文的重复投递，挡不住两条不同报文说同一件事），而这条时间线上多出来的每一行
// 都在回答「谁在什么时候拨了它」——重复的一行会让那个答案变得不可信。
//
// **suspended 不在这里拨到 false**：停扣只是「这一期先不扣了」，协议还在、用户也没说过不续，
// 那不是他关掉了自动续费（见 model.SubscriptionStatusSuspended 的说明）。
func applyAutoRenew(ctx context.Context, tx pgx.Tx, membershipID string, enabled bool,
	operatorType, operatorID, reason string, occurredAt time.Time) error {

	changeType := model.ChangeAutoRenewOff
	if enabled {
		changeType = model.ChangeAutoRenewOn
	}
	var (
		userID   string
		status   string
		expireAt time.Time
		planID   *string
	)
	err := tx.QueryRow(ctx, `UPDATE memberships
		SET auto_renew = $2,
		    -- 这个 ::timestamptz 转换**不能省**：CASE 的结果类型是从两个分支推出来的，而 $3 在
		    -- ELSE 那一支里没有别的线索，PostgreSQL 会把它当成 text——于是整条语句变成「把 text
		    -- 写进 timestamptz 列」，42804。开关拨到 true 时那一支根本不会被取到，但类型是静态
		    -- 解析的，一样躲不过去，表现是**签约生效那一步整个失败**。
		    auto_renew_off_at = CASE WHEN $2 THEN NULL ELSE $3::timestamptz END,
		    updated_at = NOW()
		WHERE id = $1 AND auto_renew <> $2
		RETURNING user_id, status, expire_at, plan_id`,
		membershipID, enabled, utc(occurredAt)).Scan(&userID, &status, &expireAt, &planID)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		// 已经是这个位置了：这次切换在开关上什么都没改，也就不该在这条时间线上留下一行。
		return nil
	case err != nil:
		return mapPGError(err)
	}

	// from/to 的会员状态与到期日**是同一个值**：自动续费开关不改会员的状态也不改有效期，它
	// 改的是「下个月还会不会被扣」。这一行交代的是发生过的动作与它是谁做的，那两列在这里只是
	// 「当时的会员长什么样」的快照。与 SetAutoRenew 走 mutateMembership 时写下的行同一个形状。
	return insertChange(ctx, tx, model.Change{
		MembershipID: membershipID,
		UserID:       userID,
		ChangeType:   changeType,
		FromStatus:   optionalText(status),
		ToStatus:     optionalText(status),
		FromExpireAt: &expireAt,
		ToExpireAt:   &expireAt,
		PlanID:       planID,
		OperatorType: operatorType,
		OperatorID:   optionalID(operatorID),
		Reason:       reason,
		OccurredAt:   utc(occurredAt),
	})
}

// lockSubscription 按订阅 id 或协议 id 取这一行并锁住它。
//
// 两个入口合一个是因为它们是同一件事的两种说法（「这一行」与「这份协议绑着的那一行」），
// 而 membership_subscriptions_agreement_unique 保证后者最多一行。
func lockSubscription(ctx context.Context, tx pgx.Tx, subscriptionID, agreementID string) (*SubscriptionRow, error) {
	where := "s.id = $1"
	key := strings.TrimSpace(subscriptionID)
	if key == "" {
		where = "s.agreement_id = $1"
		key = strings.TrimSpace(agreementID)
	}
	if key == "" {
		// 两个都没给：调用方不知道自己在收口哪一行。当「不存在」处理而不是报 500，因为
		// 事件里缺 agreementId 与「这份协议在我们这儿没有订阅」对调用方是同一个结论。
		return nil, ErrSubscriptionNotFound
	}
	// FOR UPDATE OF s：锁订阅那一行，不锁 join 过来的套餐行——套餐是全表共用的配置，
	// 锁它等于让所有签约互相排队。
	return scanSubscriptionRow(tx.QueryRow(ctx, `SELECT `+prefixColumns(subscriptionColumns, "s")+`, p.name
		FROM membership_subscriptions s JOIN membership_plans p ON p.id = s.plan_id
		WHERE `+where+` FOR UPDATE OF s`, key))
}

// scanSubscriptionRow 是 scanSubscription 加 join 出来的套餐名。
func scanSubscriptionRow(row scanner) (*SubscriptionRow, error) {
	var s model.Subscription
	var planName string
	if err := row.Scan(
		&s.ID, &s.MembershipID, &s.UserID, &s.PlanID, &s.Status,
		&s.AgreementID, &s.ContractCode, &s.PriceCents, &s.WechatPlanID,
		&s.Period, &s.PeriodCount, &s.NextChargeAt, &s.LastChargeAt, &s.ChargeCount,
		&s.FailedCount, &s.ConsecutiveFailedCount, &s.SuspendedAt, &s.CancelAt, &s.CancelReason,
		&s.CancelledBy, &s.CreatedAt, &s.UpdatedAt,
		&s.OrderID, &s.CoffeeOrderID, &s.CampaignClaimID,
		&planName,
	); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrSubscriptionNotFound
		}
		return nil, err
	}
	return &SubscriptionRow{Subscription: &s, PlanName: planName}, nil
}
