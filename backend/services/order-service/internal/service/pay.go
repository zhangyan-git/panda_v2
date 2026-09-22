package service

import (
	"context"
	"fmt"
	"strings"

	"github.com/panda-dev/panda-v2/backend/services/order-service/internal/client"
	"github.com/panda-dev/panda-v2/backend/services/order-service/internal/dto"
	"github.com/panda-dev/panda-v2/backend/services/order-service/internal/model"
)

// InitiatePaymentInput 是一次发起支付的输入。
type InitiatePaymentInput struct {
	// OrderNo 是订单号，不是内部 id：发起支付这条链路的两端（小程序、支付服务）手上都
	// 只有它（理由见 repository.FindOrderByNo）。
	OrderNo string
	// UserID 是调用方的身份，由 controller 从 access token 解出来。**归属判定用它**，
	// 不用请求里的任何字段。
	UserID string
	// PaymentMethod 是用户选的支付方式 **code**（支付服务目录里的常量）。
	//
	// 这一层不判 code 认不认识、也不看它有没有被停用：目录在支付服务那一侧，把它的取值
	// 抄一份到这里就有了两个真相，而版本错配（客户端拼错、支付服务回滚到旧版本）本来就
	// 该由支付服务明确拒掉（见那边 resolveMethod 的注释）。
	PaymentMethod string
	// RequestID 是这次发起的幂等号，原样交给支付服务做支付单的幂等键。
	RequestID string
	TraceID   string
}

// InitiatePayment 为一张订单发起一次支付。
//
// 这一段**只读订单、不改订单**：校验一遍「这单现在能不能付、该付多少」，然后把权威金额交给
// 支付域去建支付单。订单变 paid 是支付结果事件回来之后的事（HandlePaymentEvent），不在这
// 次请求里——所以这里不需要幂等事务，重发的幂等由支付单的 request_id 保证：同一把钥匙重发
// 拿到的是同一张支付单，不是第二张。
//
// 判据的顺序是**从便宜到贵、从能拒到不能拒**：形状 → 读单 → 归属 → 状态 → 金额 → 期限 →
// 下游。归属放在状态前面：别人的单必须回「不存在」，如果先判状态，一个已支付的他人订单会
// 回 409 而泄露「这个单号存在」。
func (s *OrderService) InitiatePayment(ctx context.Context, in InitiatePaymentInput) (*dto.PayAction, error) {
	orderNo := strings.TrimSpace(in.OrderNo)
	if orderNo == "" {
		return nil, ErrOrderNotFound
	}
	if strings.TrimSpace(in.PaymentMethod) == "" {
		return nil, ErrPaymentMethodRequired
	}
	if strings.TrimSpace(in.RequestID) == "" {
		return nil, ErrIdempotencyKeyRequired
	}

	order, err := s.repository.FindOrderByNo(ctx, orderNo)
	if err != nil {
		return nil, mapWriteError(err)
	}
	if !userMatches(order.UserID, in.UserID) {
		// 同 GetOrderDetail 与 CancelOrder：别人的单回「不存在」。回 403 等于确认了
		// 「这个单号是存在的」，那本身就是一次可以枚举的信息泄露。
		//
		// 设备单（没有用户）也落在这一支：它不是「无主的单谁都能付」，而是「这张单根本不
		// 该出现在收银台上」——钱已经在刷卡机上收过了。
		return nil, ErrOrderNotFound
	}
	if order.Status != model.OrderStatusPendingPayment {
		return nil, ErrOrderNotPending
	}
	if order.PayableAmount <= 0 {
		// 应付额为 0 的单（券抵完了）不该走到收银台。放它过去，支付侧会拿 amount<=0 拒掉，
		// 而那句报错是从「钱」的角度说的；这里说得更准：这一单本来就不用付钱。
		return nil, ErrOrderNotPayable
	}
	if order.ExpiresAt != nil && !s.now().Before(*order.ExpiresAt) {
		// 过了付款期限、但超时关单还没扫到它：行上还是待支付，事实上已经不是了。放它过去会
		// 造出「钱收了、单被关掉」——支付侧那张预支付单的期限是**从发起支付算起**的，可以
		// 远晚于订单的 expires_at，于是用户能在一张即将作废的单上把钱付掉。
		// 宁可让用户重新下一单；这一单马上就真的被关掉了。
		return nil, ErrOrderNotPending
	}
	if s.payments == nil {
		// 这个部署没接支付域。与「支付服务这次没答上来」是同一个结论：稍后可能成。
		return nil, ErrPaymentServiceUnavailable
	}

	// 分账规则按 (biz_type, 范围) 命中，而这一单的 biz_type 由**订单行**决定——它是订单域的
	// 事实，支付域猜不出来（猜错的表现是安静地命不中规则、整单归平台，没有任何报错）。
	// 所以这里读一次行，与门店/设备一起送过去。
	//
	// 读行失败**不让这次发起继续**：少了 biz_type 就等于少了一次分账，而钱是真的收了。
	// 代价是这一单付不成——但读不动订单行的库，读不动支付库多半也就是前后脚的事。
	lines, err := s.repository.ListOrderLines(ctx, order.ID)
	if err != nil {
		return nil, mapWriteError(err)
	}

	// 微信身份在下游调用**之前**取，不是在建支付单失败之后再补：openid 是渠道报文的一部分，
	// 它必须在支付服务着手发起时就在手上。取不到时的处理见 WalletIdentityReader。
	if s.wallets == nil {
		// 这个部署问不到身份域。见 New 里 wallets 那一段：不能退化成「发一个空 openid 出去」，
		// 所以在这里 fail-closed，而不是让支付侧去渠道那里碰壁。
		return nil, ErrWalletIdentityUnavailable
	}
	// 走到这里 order.UserID 一定非空：上面那一支已经把没有用户（设备单）与不是本人的单
	// 都挡掉了，而付钱这件事必须有一个付钱的人。
	openID, bound, err := s.wallets.MiniappOpenID(ctx, derefString(order.UserID))
	if err != nil {
		// 问不到身份服务是「得不出结论」，稍后重发可能就好。**它不能被当成「他没有绑定」**：
		// 那会让用户去重新登录微信，而真正的问题是我们这边没问到。
		return nil, fmt.Errorf("%w: %v", ErrWalletIdentityUnavailable, err)
	}
	if !bound {
		// 没有绑定不是这里的终点：用户选的可能是扫码、H5 或者咖啡豆——那些方式不需要 openid，
		// 一刀切拒掉会让没绑微信的人连豆都花不出去。**这一个事实由支付侧按 action 收场**：
		// 需要用户身份的渠道会把这笔落成 failed + failure_code，而不是拿空 openid 去试。
		openID = ""
	}

	// 金额只从这里出去：order.PayableAmount 是下单时算好并冻结在订单上的权威值。
	// 请求体里没有任何能影响它的字段——这是「客户端改不了付款金额」的全部依据。
	return s.payments.Create(ctx, client.CreatePaymentInput{
		OrderID:       order.ID,
		OrderNo:       order.OrderNo,
		UserID:        derefString(order.UserID),
		Amount:        order.PayableAmount,
		PaymentMethod: strings.TrimSpace(in.PaymentMethod),
		RequestID:     strings.TrimSpace(in.RequestID),
		WalletOpenID:  openID,
		// 分账的维度：范围（门店/设备）与业务分类。两者都在订单这一侧，支付域拿不到。
		// 门店与设备**可以是空的**（纯会员订单没有点位），空值只会让那一档规则命不中；
		// biz_type 不是可选项——它由下面的函数从订单行推出来，订单至少有一行。
		StoreID:  derefString(order.StoreID),
		DeviceID: derefString(order.DeviceID),
		BizType:  settlementBizType(lines),
	})
}

// settlementBizType 由订单行推出这笔支付的分账业务分类（settlement_rules.biz_type 的词表）。
//
// 一笔支付只有一条分账任务、整单一次分完（见 008 的 settlement_tasks），而一张订单可以有
// 多种行（咖啡 + 会员 + 加购），所以这里必须收口成一个值。优先序是**饮品 > 会员 > 加购**：
// 有饮品行的单是咖啡单，哪怕它顺手带了一个杯套或者一年会员——门店分成最常挂的就是饮品。
//
// 这是这条链路里唯一一处「把多值压成一个值」的判断，写在一个函数里，改起来只改这里。
// 真要按商品分开分，改的是 settlement_tasks 的唯一键（payment_id → (payment_id, seq)），
// 不是这里的优先级。
//
// 空行返回 coffee：订单至少有一行，走到这里说明行读了却读不出类型——那不是能靠猜解决的
// 事，但也不是能让用户付不了款的事。归到最常用的那一档，与「没配规则的门店整单归平台」
// 同一个兜底方向。
func settlementBizType(lines []*model.OrderLine) string {
	var hasAddon, hasMembership bool
	for _, line := range lines {
		switch line.LineType {
		case model.LineTypeDrink:
			return model.SettlementBizCoffee
		case model.LineTypeMembership:
			hasMembership = true
		case model.LineTypeAddon:
			hasAddon = true
		}
	}
	switch {
	case hasMembership:
		return model.SettlementBizMembership
	case hasAddon:
		return model.SettlementBizAddonProduct
	default:
		return model.SettlementBizCoffee
	}
}
