package repository

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// ErrThirdPartyOrderNoTaken 表示这个对方单号已经被**另一类**设备单占了。
//
// 两条设备路（刷卡机与取货码）共用同一个单号空间（orders_third_party_order_no_key 只认
// 单号，不知道它来自哪条路），所以「命中既有那一张」与「这个单号属于你这一类」是两件事。
// 后者不成立时必须报错，不能当成幂等命中——后果见 CreateDeviceOrder 的说明。
var ErrThirdPartyOrderNoTaken = errors.New("third party order no is already used by another kind of device order")

// CreateDeviceOrderParams 是一笔**设备侧订单**要落进去的全部事实。
//
// 它服务两条路：线下刷卡机（service.CreateDeviceOrder）与取货码（service.CreatePickupOrder）。
// 两条的形状完全一样——一张已支付、没有用户、一行饮品的单——差别只有钱怎么收（payment_method
// 与状态流水那句话）与价格谁定（前者设备报，后者我们算）。所以这里只有**一**条 INSERT：
// 抄一份出来的话，库里那些金额恒等式与 NOT NULL 的空对象列就会有两个维护点，而它们改一处
// 漏一处只在出事那天才看得出来。
//
// 与 CreateOrderParams 的区别不是「少了一个幂等键字段」，而是这条路**没有的几件事**：
// 没有用户（user_id 写 NULL）、没有支付单（payment_no 空）、没有待支付期（status 直接是
// paid、expires_at 为 NULL）、没有幂等表（幂等靠 orders.third_party_order_no 的唯一索引，
// 见下面的说明）。金额、订单号、履约状态与那唯一一行都由 service 算好——仓储不参与定价。
type CreateDeviceOrderParams struct {
	// ThirdPartyOrderNo 是对方单号，也是这条路唯一的幂等键。非空由 service 保证。
	ThirdPartyOrderNo string
	OrderNo           string
	Source            string
	// FulfillmentStatus 由设备上报的出饮结果决定：成功是 pending（机器已经出了杯，
	// 但订单这一层的汇总还没被履约事件改过），失败是 failed。
	FulfillmentStatus string
	StoreID           *string
	StoreName         string
	DeviceID          *string
	DeviceNo          string
	OriginalAmount    int64
	DiscountAmount    int64
	PayableAmount     int64
	PaymentMethod     string
	// PaidAt 是这笔钱在我们这里的记账时刻。刷卡机上真正收钱的那一刻我们没有——回调里
	// 没有这个字段，这是本批的已知缺口（见 service.CreateDeviceOrder 的说明）。
	PaidAt time.Time
	Remark string
	// TransitionReason 是状态流水那一行的原因，由调用方给。
	//
	// 它必须是个参数而不是写死一句话：这条 SQL 今天服务两条路（刷卡机与取货码），它们**都**
	// 从无到 paid，而「这一单怎么来的」正是那张流水表要回答的问题——写死「设备刷卡购买」会让
	// 取货码的单在流水里看着像刷了卡。
	TransitionReason string
	Line             *OrderLineInsert
	// 没有 TraceID：这条路不发任何事件（见下），而 trace 唯一的去处本来是事件信封。
	// 真要排查时，手里有的是对方单号——它是这条路上真正的关联键。
}

// CreateDeviceOrderResult 是建单（或幂等命中）的结果。
type CreateDeviceOrderResult struct {
	OrderID string
	OrderNo string
	// Created=false 表示这次命中的是已有的那一张，什么都没写。
	Created bool
}

// DeviceOrderByThirdPartyNo 是「对方单号上已经有的那一张单」的三格事实。
type DeviceOrderByThirdPartyNo struct {
	OrderID string
	OrderNo string
	// PaymentMethod 是那一张单的钱是怎么收的（card_pay / pickup_code）。调用方靠它判断
	// 「这个单号是不是属于我这条路」。
	PaymentMethod string
}

// FindDeviceOrderByThirdPartyNo 按对方单号查一张已有的设备单，没有就回 (nil, nil)。
//
// 它是**扣款之前**那一次检查用的：取货码那条路的顺序是先扣钱后建单，等建单那一步才发现
// 单号被另一类单占了，钱已经扣出去了——那正是这条路最不想要的形状。所以调用方要在动钱
// 之前先问一次。
//
// 它**不是**幂等的判据：判据在 CreateDeviceOrder 的 ON CONFLICT 那一条上（先查后插在并发
// 下是错的，见那里的说明）。这一次读只判「这个单号是不是属于我这条路」，判错了也只是把一次
// 本该被拒的请求放到扣款之后才拒——所以它与写那一步的判据可以各用各的方式，不冲突。
//
// 回 (nil, nil) 而不是 ErrNoRows：**「没有」是正常情形**（绝大多数请求都是第一次），把它
// 做成错误会让每个调用点都要先翻译一遍。
func (r *PostgresRepository) FindDeviceOrderByThirdPartyNo(ctx context.Context, thirdPartyOrderNo string) (*DeviceOrderByThirdPartyNo, error) {
	existing := &DeviceOrderByThirdPartyNo{}
	err := r.pool.QueryRow(ctx, `SELECT id::text, order_no, payment_method FROM orders
		WHERE third_party_order_no = $1`, thirdPartyOrderNo).
		Scan(&existing.OrderID, &existing.OrderNo, &existing.PaymentMethod)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return existing, nil
}

// CreateDeviceOrder 在一个事务里落订单主表与那唯一一行饮品行。
//
// # 幂等靠唯一索引，不靠先查后插
//
// 先查后插在并发下是错的：同一台机器重投时两个请求会各自查到「没有」、各自插一张。
// 所以这里是**直接插**，靠 orders_third_party_order_no_key 兜：撞上就 ON CONFLICT DO
// NOTHING 返回 0 行，那一行就是「已经有人建过了」。对方重投是这条路的常态（网络重放、
// 机器自己重试），所以命中不是错误——返回既有那张、created=false，让它 ack 掉。
//
// 并发下这一条也成立：ON CONFLICT DO NOTHING 会等另一笔事务结束后才返回，所以冲突之后
// 紧接着的 SELECT 一定读得到那一行（READ COMMITTED 下新语句看得到已提交的数据）。
//
// # 命中的那一张必须**是同一类设备单**
//
// 这两条设备路（刷卡机 card_pay 与取货码 pickup_code）**共用同一个单号空间**：那个索引
// 只认 third_party_order_no，不知道单号是从哪条路来的。所以命中时还要校一次 payment_method
// ——不是同一类就回 ErrThirdPartyOrderNoTaken，而不是把那张单当成「你这一次的幂等命中」。
//
// 不校的后果分两条路说清楚：
//   - 取货码这条：对方单号属于一张刷卡机单时，扣款在**建单之前**已经发生过（那条路的
//     顺序是先扣后建），于是钱扣了、而返回给对方的是另一张单的单号——一笔扣款从此挂在一
//     张与它无关的订单上，谁也查不出来。
//   - 刷卡机这条：钱不动，但对方拿到的是另一类单的单号，他账上那一笔从此对错了单。
//
// 合作方把一个单号用两次是他那侧的错（两条接口的对接文档都写明单号要唯一），所以这是一次
// **确定的拒绝**，不是重试能好的一档。
//
// # 没有事件
//
// 这条路**不发 order.created / order.paid**，这是有意的：出杯已经在机器上发生过了，
// 而 order.paid 是「有人付了钱、该去做这一杯」的触发点（履约接上来时它会消费它）——
// 对着一条已经出过杯的订单发它，等于让机器再出一杯。设备单也没有别的下游：没有券、
// 没有会员行、福卡承诺恒为 0。真要有人消费设备单时，这里要显式决定发什么事件，而不是
// 顺手复用支付那条路上的那一个。
//
// # 对冲账的那一格
//
// payment_no 是空的：我们手里没有支付凭据（钱是刷卡机收的）。这条缺口与 amount 不校验
// 是同一件事的两面，见 service.CreateDeviceOrder 的说明。
func (r *PostgresRepository) CreateDeviceOrder(ctx context.Context, p CreateDeviceOrderParams) (*CreateDeviceOrderResult, bool, error) {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return nil, false, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	// user_id 显式写 NULL：设备单没有用户（migrations/order）。
	// scene_token / membership_id / membership_snapshot / fortune_cards_* 都写这一档的
	// 空值：设备单上没有屏幕选品会话，也没有会员与福卡承诺。写出来而不是靠 DEFAULT，
	// 因为 INSERT 里每一列都写了值——不写的列才会走 DEFAULT。
	var id string
	var createdAt time.Time
	err = tx.QueryRow(ctx, `INSERT INTO orders
		(order_no, user_id, source, status, fulfillment_status, store_id, store_name, device_id,
		 device_no, scene_token, original_amount, discount_amount, payable_amount, paid_amount,
		 refunded_amount, membership_id, membership_snapshot, fortune_cards_expected,
		 fortune_card_snapshot, payment_method, payment_no, paid_at, request_id, remark,
		 expires_at, third_party_order_no)
		VALUES ($1, NULL, $2, 'paid', $3, $4, $5, $6, $7, '', $8, $9, $10, $10, 0, NULL,
		 '{}', 0, '{}', $11, '', $12, '', $13, NULL, $14)
		ON CONFLICT (third_party_order_no) WHERE third_party_order_no <> '' DO NOTHING
		RETURNING id::text, created_at`,
		p.OrderNo, p.Source, p.FulfillmentStatus, p.StoreID, p.StoreName, p.DeviceID,
		p.DeviceNo, p.OriginalAmount, p.DiscountAmount, p.PayableAmount, p.PaymentMethod,
		p.PaidAt, p.Remark, p.ThirdPartyOrderNo).Scan(&id, &createdAt)
	if errors.Is(err, pgx.ErrNoRows) {
		// 幂等命中。这里**不报错**：对方重投是正常的，报错会让它一直重投下去。
		var existingID, existingNo, existingMethod string
		if err := tx.QueryRow(ctx, `SELECT id::text, order_no, payment_method FROM orders
			WHERE third_party_order_no = $1`, p.ThirdPartyOrderNo).
			Scan(&existingID, &existingNo, &existingMethod); err != nil {
			return nil, false, mapPGError(err)
		}
		// 不是同一类设备单就不是「你这一次的幂等命中」，见上面的说明。这一格用 payment_method
		// 而不是 source 判：两条路的 source 都是 device，单号空间共享的也正是这两条路之间的
		// 事（source 分不开它们，payment_method 正是为此而分的，
		// 见 migrations/order 里 orders.payment_method 的列注释）。
		if existingMethod != p.PaymentMethod {
			return nil, false, fmt.Errorf("%w: third party order no %q belongs to a %s order",
				ErrThirdPartyOrderNoTaken, p.ThirdPartyOrderNo, existingMethod)
		}
		return &CreateDeviceOrderResult{OrderID: existingID, OrderNo: existingNo}, false, tx.Commit(ctx)
	}
	if err != nil {
		return nil, false, mapPGError(err)
	}

	if err := insertOrderLine(ctx, tx, id, p.Line); err != nil {
		return nil, false, err
	}

	// 状态流水照写：这张表讲的是「这一单怎么走到今天的」，而设备单的第一步就是从无到
	// paid。actor 是 system——推动它的是机器，不是人。原因由调用方给（刷卡机与取货码各自
	// 一句，见 CreateDeviceOrderParams.TransitionReason）。
	if err := recordTransition(ctx, tx, "order", id, "", "paid", p.TransitionReason, "", "system", nil, nil); err != nil {
		return nil, false, err
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, false, err
	}
	return &CreateDeviceOrderResult{OrderID: id, OrderNo: p.OrderNo, Created: true}, true, nil
}
