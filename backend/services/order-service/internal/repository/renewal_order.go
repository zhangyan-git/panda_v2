package repository

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// CreateRenewalOrderParams 是一笔**会员续费单**要落进去的全部事实。
//
// 它与 CreateDeviceOrderParams 是同族（都是「钱已在别处收过 → 直接落成已支付」），但**没有**
// 合并成一条 INSERT，理由值得写下来：
//
//   - 参数结构里有三成字段在两边各自无意义：设备单没有 user_id（那一路的 user_id 必须是
//     NULL，不是空串）、没有 membership_id；续费单没有 device_id / device_no / store_id，
//     履约状态恒为 none。合并出来的结构会让「这个字段这一路不用」变成一种心照不宣，而
//     「空串到底是没有还是没填」正是 model.Order.UserID 那段注释专门要避开的东西。
//   - 两条路的**调用方不同、RPC 不同、来源不同**（device vs renewal），唯一的共同点是一张
//     已支付的订单行，而那部分的正确性由库上的约束兜着：NOT NULL、金额恒等式
//     （orders_payable_matches / order_lines_payable_matches / order_lines_discount_breakdown）、
//     以及四条只允许出现在特定行类型上的 CHECK。少写一列是**当场报错**，不是静默漂移
//     ——device_order.go 那段注释里「漏掉 campaign_snapshot 是集成用例先抓出来的」说的就是
//     这件事。加了这一层兜底之后，两个 INSERT 之间的一致性是编译器与数据库一起管着的。
//
// 金额、订单号、履约状态与那唯一一行都由 service 算好——仓储不参与定价。
type CreateRenewalOrderParams struct {
	// ThirdPartyOrderNo 是**渠道流水号**（payment_agreement_charges.provider_transaction_id），
	// 这条路的幂等键。非空由 service 保证。
	//
	// 为什么用渠道流水号而不是「协议号 + 期次」：它是这笔钱在微信那一侧的唯一标识，也正是
	// 「这一期到底扣成没有」的凭据——拿着它去商户平台能查到那一笔。协议号加期次也能拼出一个
	// 唯一的键，但那个键在渠道那边不存在，出事时对不上账。
	ThirdPartyOrderNo string
	OrderNo           string
	// UserID 是下单用户。**非空**——与设备单相反，这一单有主人（那一期的会员费是他的）。
	UserID string
	// MembershipID 是 memberships.id 的值引用，可空。填了后台从会员看订单查得到。
	MembershipID *string
	// Amount 是这一期的实付金额（分）。original = payable = paid = amount，优惠恒为 0
	// ——会员套餐没有「标价」与「成交价」之分（会员价那条优惠是给饮品的，不是给会员本身的），
	// 与 applyMembershipPlan 同一条口径。
	Amount int64
	// PaymentMethod 是钱怎么收的。续费走代扣，取值见 service 里的常量。
	PaymentMethod string
	// PaidAt 是这笔钱在我们这里的记账时刻。
	PaidAt time.Time
	Remark string
	// TransitionReason 是状态流水那一行的原因，由调用方给（同 CreateDeviceOrderParams）。
	TransitionReason string
	// Line 是那唯一一行会员行（line_type=membership，带套餐快照）。
	Line *OrderLineInsert
}

// CreateRenewalOrder 在一个事务里落订单主表与那唯一一行会员行。
//
// # 幂等靠唯一索引，不靠先查后插
//
// 与 CreateDeviceOrder 同一条：直接插，靠 orders_third_party_order_no_key 兜，撞上就
// ON CONFLICT DO NOTHING 返回 0 行。这条路上重投是**常态而不是异常**——扣款结果通知是
// MQ 消息，投递语义是至少一次，而消费失败还会重投（上限见 platform/messaging）。
//
// # 命中的那一张必须也是续费单
//
// 三条路（刷卡机 card_pay / 取货码 pickup_code / 续费 wechat_papay）共用同一个单号空间，
// 那个索引只认 third_party_order_no。所以命中时校一次 payment_method，不是同一类就回
// ErrThirdPartyOrderNoTaken。
//
// 这条检查在这一路上比在设备那两路上更要紧，因为**单号的来源不同**：设备那两条的键是合作方
// 生成的商户单号，而这一条用的是**微信的流水号**——两个空间的取值风格完全不同，撞车意味着
// 有人把别的单号填进了这条 RPC（或者反过来），那是一次要立刻查的串线，而不是一次重投。
//
// # 没有事件
//
// 与设备单同一条理由：这一单的后果（会员延期、发券）在调用方那边**已经做完了**
// （membership-service 是先建单、再在自己的事务里落续费与发券）。再发一条 order.paid
// 会让下游以为「有人刚付了钱、该去开通会员」，而那条路会按订单快照再开一次会员、
// 于是同一期扣款被兑现两次。见 order.proto 里 CreateRenewalOrder 的说明。
func (r *PostgresRepository) CreateRenewalOrder(ctx context.Context, p CreateRenewalOrderParams) (*CreateRenewalOrderResult, bool, error) {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return nil, false, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	// 与设备单那一条逐列对齐，差别就是上面参数结构里说的那几列：
	//   - user_id / membership_id 有值（设备单是 NULL / NULL）
	//   - device_id / device_no / store_id / store_name / scene_token 一律为空
	//     （纯会员订单没有点位、没有设备，见 proto 与 model.Order.SourceRenewal）
	//   - expires_at 为 NULL：这一单建出来就是终态，没有待支付期
	//   - membership_snapshot 与 fortune_card_snapshot 写空对象：这一单没有会员资格快照
	//     （那是下单当时会员状态的副本，而续费的会员事实在 membership 库），也不承诺福卡
	//     （福卡只能抽奖赠送，见模型里的说明）。空对象列显式写值而不是靠 DEFAULT
	//     ——INSERT 里每一列都写了值，不写的列才会走 DEFAULT。
	var id string
	var createdAt time.Time
	err = tx.QueryRow(ctx, `INSERT INTO orders
		(order_no, user_id, source, status, fulfillment_status, store_id, store_name, device_id,
		 device_no, scene_token, original_amount, discount_amount, payable_amount, paid_amount,
		 refunded_amount, membership_id, membership_snapshot, fortune_cards_expected,
		 fortune_card_snapshot, payment_method, payment_no, paid_at, request_id, remark,
		 expires_at, third_party_order_no)
		VALUES ($1, $2, 'renewal', 'paid', 'none', NULL, '', NULL, '', '', $3, 0, $3, $3, 0,
		 $4, '{}', 0, '{}', $5, '', $6, '', $7, NULL, $8)
		ON CONFLICT (third_party_order_no) WHERE third_party_order_no <> '' DO NOTHING
		RETURNING id::text, created_at`,
		p.OrderNo, p.UserID, p.Amount, p.MembershipID, p.PaymentMethod, p.PaidAt, p.Remark,
		p.ThirdPartyOrderNo).Scan(&id, &createdAt)
	if errors.Is(err, pgx.ErrNoRows) {
		// 幂等命中。不报错：重投是正常的，报错会让平台一直重投下去。
		var existingID, existingNo, existingMethod string
		if err := tx.QueryRow(ctx, `SELECT id::text, order_no, payment_method FROM orders
			WHERE third_party_order_no = $1`, p.ThirdPartyOrderNo).
			Scan(&existingID, &existingNo, &existingMethod); err != nil {
			return nil, false, mapPGError(err)
		}
		if existingMethod != p.PaymentMethod {
			return nil, false, fmt.Errorf("%w: third party order no %q belongs to a %s order",
				ErrThirdPartyOrderNoTaken, p.ThirdPartyOrderNo, existingMethod)
		}
		return &CreateRenewalOrderResult{OrderID: existingID, OrderNo: existingNo}, false, tx.Commit(ctx)
	}
	if err != nil {
		return nil, false, mapPGError(err)
	}

	if err := insertOrderLine(ctx, tx, id, p.Line); err != nil {
		return nil, false, err
	}

	// 状态流水照写：这一单的第一步就是从无到 paid。actor 是 system——推动它的是到期扫描
	// 与渠道的扣款结果通知，不是人在点。
	if err := recordTransition(ctx, tx, "order", id, "", "paid", p.TransitionReason, "", "system", nil, nil); err != nil {
		return nil, false, err
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, false, err
	}
	return &CreateRenewalOrderResult{OrderID: id, OrderNo: p.OrderNo, Created: true}, true, nil
}

// CreateRenewalOrderResult 是建单（或幂等命中）的结果。
type CreateRenewalOrderResult struct {
	OrderID string
	OrderNo string
	// Created=false 表示这次命中的是已有的那一张，什么都没写。调用方要把它带进自己的
	// 续费流水里（两边都记 order_id），所以这一位必须与订单号一起回。
	Created bool
}
