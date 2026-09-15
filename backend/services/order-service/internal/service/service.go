// Package service 是订单的业务层：校验、算钱、生成单号、编排事务、驱动状态机。
//
// 按业务动作拆文件：create.go 下单，pay.go 发起支付，payment.go 支付结果落单，
// cancel.go 取消，query.go 查询，state.go 状态机。仓储只负责「一个事务里把这几个事实
// 写进去」，「几个事实分别是什么」由这里决定。
package service

import (
	"context"
	"errors"
	"time"

	"github.com/panda-dev/panda-v2/backend/services/order-service/internal/client"
	"github.com/panda-dev/panda-v2/backend/services/order-service/internal/dto"
	"github.com/panda-dev/panda-v2/backend/services/order-service/internal/model"
	"github.com/panda-dev/panda-v2/backend/services/order-service/internal/repository"
)

// Repository 是订单库的数据访问契约。
//
// 定义成接口而不是直接用 *repository.PostgresRepository，是为了让 service 的校验与
// 状态机能在没有数据库的情况下测：这一层里最容易出错的恰恰是不碰数据库的那部分
// （算钱、判断状态该不该动），而那些逻辑不该为了测它去起一个 PG。
type Repository interface {
	CreateOrder(ctx context.Context, p repository.CreateOrderParams) (*repository.CreateOrderResult, bool, error)
	SettlePayment(ctx context.Context, p repository.SettlePaymentParams) (*repository.OrderPaymentResult, bool, error)
	CancelOrder(ctx context.Context, p repository.CancelOrderParams) (*repository.OrderPaymentResult, error)
	CompleteOrder(ctx context.Context, p repository.CompleteOrderParams) (*repository.OrderPaymentResult, error)
	ExpireOverdue(ctx context.Context, limit int, traceID string) (int, error)
	FindOrderByID(ctx context.Context, id string) (*model.Order, error)
	FindOrderByNo(ctx context.Context, orderNo string) (*model.Order, error)
	GetOrderDetail(ctx context.Context, id string) (*repository.OrderDetail, error)
	ListOrders(ctx context.Context, f repository.OrderFilter) ([]*repository.OrderRow, int, error)
	ApplyAfterSale(ctx context.Context, p repository.ApplyAfterSaleParams) (*repository.AfterSaleRow, bool, error)
	ReviewAfterSale(ctx context.Context, p repository.ReviewAfterSaleParams) (*repository.AfterSaleRow, error)
	CancelAfterSale(ctx context.Context, p repository.CancelAfterSaleParams) (*repository.AfterSaleRow, error)
	ListAfterSales(ctx context.Context, f repository.AfterSaleFilter) ([]*repository.AfterSaleRow, int, error)
}

// DeviceReader 是下单时校验设备状态的依赖。
type DeviceReader interface {
	Get(ctx context.Context, deviceID string) (*client.Device, bool, error)
}

// PaymentCreator 是发起支付时对支付域的调用。
//
// 定义成接口而不是直接用 *client.PaymentCreator，理由与 Repository 那条一样：发起支付这条
// 路上最容易写错的是那些**不碰网络的部分**（归属、状态、金额、过期），它们必须能在没有
// 支付服务的情况下被测到。
type PaymentCreator interface {
	Create(ctx context.Context, in client.CreatePaymentInput) (*dto.PayAction, error)
}

var (
	// —— 请求本身不合法（controller 统一回 400）——
	ErrInvalidSource            = errors.New("source must be miniapp or screen_qr")
	ErrLinesRequired            = errors.New("order must have at least one line")
	ErrTooManyLines             = errors.New("order has too many lines")
	ErrTooManyDrinkLines        = errors.New("an order can contain at most one drink line")
	ErrTooManyMembershipLines   = errors.New("an order can contain at most one membership line")
	ErrInvalidLineType          = errors.New("lineType must be drink, addon or membership")
	ErrInvalidQuantity          = errors.New("quantity must be positive")
	ErrInvalidAmount            = errors.New("amounts must not be negative")
	ErrDiscountExceedsLine      = errors.New("line discount exceeds the original amount")
	ErrCouponOnlyOnDrink        = errors.New("coupons can only discount drink lines")
	ErrCouponNeedsDiscount      = errors.New("couponId and couponDiscountAmount must be given together")
	ErrAddonNeedsCampaign       = errors.New("addon lines must reference a campaign")
	ErrCampaignOnlyOnAddon      = errors.New("campaignId is only allowed on addon lines")
	ErrMembershipPlanOnlyOnPlan = errors.New("membershipPlanSnapshot is only allowed on membership lines")
	ErrMembershipPlanRequired   = errors.New("membership lines must carry a membershipPlanSnapshot")
	ErrMembershipIDRequired     = errors.New("membershipId is required when the order contains a membership line")
	ErrMembershipIDNotAllowed   = errors.New("membershipId is only allowed when the order contains a membership line")
	ErrStoreMismatch            = errors.New("storeId does not match the store the device is deployed at")
	ErrDeviceRequired           = errors.New("deviceId is required when the order contains a drink line")
	ErrIdempotencyKeyRequired   = errors.New("Idempotency-Key is required")
	ErrUserRequired             = errors.New("user id is required")
	ErrCancelReasonRequired     = errors.New("cancellation reason is required")
	ErrPaymentMethodRequired    = errors.New("paymentMethodId is required")

	// —— 售后：请求形状不合法 ——
	//
	// 分界线的另一侧是下面「售后：需要锁内事实」那一组。判据只看得到请求本身的放这里，
	// 要看账上的事实（订单状态、已退金额、有没有别的售后单）的放仓储——那里才有事务。
	ErrAfterSaleScopeInvalid          = errors.New("scope must be all, drink, addon or membership")
	ErrAfterSaleMembershipUnsupported = errors.New("refunding a membership line is not supported yet")
	ErrAfterSaleLineRequired          = errors.New("orderLineId is required unless scope is all")
	ErrAfterSaleLineNotAllowed        = errors.New("orderLineId is only allowed when scope is not all")
	ErrAfterSaleReasonRequired        = errors.New("refund reason is required")
	ErrAfterSaleImagesInvalid         = errors.New("images must be at most 9 http(s) urls")
	ErrAfterSaleRemarkRequired        = errors.New("remark is required when rejecting")
	ErrAfterSaleActionInvalid         = errors.New("after sale action must be approve or reject")

	// —— 状态与资源（controller 各自映射成 404/409/503）——
	ErrDeviceNotFound = errors.New("device not found")
	// ErrDeviceUnavailable：机器存在，但我们不让它用（停用）。这是业务上的拒绝，不是故障。
	ErrDeviceUnavailable = errors.New("device is not active")
	// ErrDeviceLookupUnavailable：问不到设备（读端没配好，或者咖啡机服务这次没答上来）。
	// 与 ErrDeviceUnavailable 分开：前者重发一次可能就好了（503），后者重发一百次也一样（409）。
	// 混成一个会让调用方把一次下游抖动当成「这台机器停用了」，或者反过来无谓地重试。
	ErrDeviceLookupUnavailable = errors.New("device lookup is unavailable")
	ErrOrderNotPending         = errors.New("order is not awaiting payment")
	// ErrOrderNotPayable：这一单现在不该付钱——应付额为 0（券抵完了）。它不是「请求写错了」，
	// 所以不在 ValidationErrors 里：判据是订单上的事实，光看请求看不出来。
	ErrOrderNotPayable = errors.New("order has nothing to pay")
	ErrOrderNotFound   = errors.New("order not found")
	ErrForbidden       = errors.New("operation is not allowed for this caller")

	// —— 事件解码 ——
	ErrInvalidPaymentEvent = errors.New("payment event payload is invalid")
)

// ValidationErrors 是「请求不合法」这一类错误的全集，controller 拿它决定回 400
// 还是回别的。放在一处而不是在 controller 里逐个 case：新增一条校验就要在
// controller 里同步加一个 case，是必然漏掉的写法。
var ValidationErrors = []error{
	ErrInvalidSource, ErrLinesRequired, ErrTooManyLines, ErrTooManyDrinkLines,
	ErrTooManyMembershipLines, ErrInvalidLineType, ErrInvalidQuantity, ErrInvalidAmount,
	ErrDiscountExceedsLine, ErrCouponOnlyOnDrink, ErrCouponNeedsDiscount, ErrAddonNeedsCampaign,
	ErrCampaignOnlyOnAddon, ErrMembershipPlanOnlyOnPlan, ErrMembershipPlanRequired,
	ErrMembershipIDRequired, ErrMembershipIDNotAllowed, ErrStoreMismatch, ErrDeviceRequired,
	ErrIdempotencyKeyRequired, ErrUserRequired, ErrCancelReasonRequired, ErrPaymentMethodRequired,
	ErrAfterSaleScopeInvalid, ErrAfterSaleMembershipUnsupported, ErrAfterSaleLineRequired,
	ErrAfterSaleLineNotAllowed, ErrAfterSaleReasonRequired, ErrAfterSaleImagesInvalid,
	ErrAfterSaleRemarkRequired, ErrAfterSaleActionInvalid,
}

// 幂等与金额相关的哨兵从仓储再导出一次。
//
// 这样 controller 只需要认 service 这一个包的错误：同一个语义若有两个可比的值
// （service.ErrX 和 repository.ErrX），迟早有人只比了其中一个，而错误会从另一个漏出去
// 变成 500。导出的是同一份值，errors.Is 两侧都成立。
var (
	// ErrIdempotencyInProgress：同一个幂等键的操作还在处理中，让调用方稍后重试（409）。
	ErrIdempotencyInProgress = repository.ErrIdempotencyInProgress
	// ErrIdempotencyConflict：同一个幂等键换了请求体（409），重试也没用。
	ErrIdempotencyConflict = repository.ErrIdempotencyConflict
	// ErrPaymentAmountMismatch：支付事件里的金额与订单应付金额对不上。
	ErrPaymentAmountMismatch = repository.ErrPaymentAmountMismatch
	// ErrOrderNotCompletable：订单不在可完成的状态（还没付钱、已经退过、关过了）。
	ErrOrderNotCompletable = repository.ErrOrderNotCompletable

	// —— 售后：判据在仓储的事务里（注释见 repository/after_sale.go）——
	// ErrOrderNotRefundable：订单不在可退的状态（没付钱、已取消、已经在退）。
	ErrOrderNotRefundable = repository.ErrOrderNotRefundable
	// ErrAfterSaleNotFound：售后单不存在，或者不属于这个用户。
	ErrAfterSaleNotFound = repository.ErrAfterSaleNotFound
	// ErrAfterSaleNotPending：这张售后单已经不在待处理状态（审过了、撤销过了）。
	ErrAfterSaleNotPending = repository.ErrAfterSaleNotPending
	// ErrAfterSaleAlreadyOpen：这一行（或整张单）已经有一张还没结束的售后单。
	ErrAfterSaleAlreadyOpen = repository.ErrAfterSaleAlreadyOpen
	// ErrAfterSaleAlreadyRefunded：这一行已经退过款了。
	ErrAfterSaleAlreadyRefunded = repository.ErrAfterSaleAlreadyRefunded
	// ErrAfterSaleLineMismatch：指的那一行不属于这张订单。
	ErrAfterSaleLineMismatch = repository.ErrAfterSaleLineMismatch
	// ErrAfterSaleNothingToRefund：算下来没有可退的金额。
	ErrAfterSaleNothingToRefund = repository.ErrAfterSaleNothingToRefund
	// ErrAfterSaleExceedsRefundable：按行退的金额超过了这张订单的剩余可退额。
	ErrAfterSaleExceedsRefundable = repository.ErrAfterSaleExceedsRefundable
	// ErrFortuneCardConfirmationRequired：这一单承诺过福卡，审核通过前必须显式确认
	// 「赠送的福卡没有参与过抽奖」。
	ErrFortuneCardConfirmationRequired = repository.ErrFortuneCardConfirmationRequired
)

// 发起支付那条路上的四种结论同样从 client 再导出一次，理由与上面那一组相同：controller
// 只认 service 这一个包的错误。
var (
	// ErrPaymentRejected：支付侧不接受这次请求（支付方式非法/不存在/停用/依赖的服务没建）。
	ErrPaymentRejected = client.ErrPaymentRejected
	// ErrPaymentConflict：同一个幂等号的上一笔还在跑，或者那把钥匙被换过请求体。
	ErrPaymentConflict = client.ErrPaymentConflict
	// ErrPaymentUncertain：支付侧没给出确定的结论（渠道超时/结果不明，或我们这条调用的
	// deadline 先到了）。**这次发起不算失败**，稍后可以带着同一把幂等号重试。
	ErrPaymentUncertain = client.ErrPaymentUncertain
	// ErrPaymentServiceUnavailable：支付服务没答上来。是故障，不是业务结论。
	ErrPaymentServiceUnavailable = client.ErrPaymentServiceUnavailable
)

// IsValidationError 判断一个错误是不是「请求不合法」。
func IsValidationError(err error) bool {
	for _, candidate := range ValidationErrors {
		if errors.Is(err, candidate) {
			return true
		}
	}
	return false
}

// maxOrderLines 是单笔订单的行数上限。
//
// 不是业务规则，是防呆：合法的组合只有「一杯饮品 + 若干加购品 + 一个会员套餐」，杯套类
// 加购品再多也不会到两位数。设一个上限是为了让「客户端 bug 循环塞了一万行」在进库前
// 就停住，而不是变成一次万行插入的慢查询。
const maxOrderLines = 20

// DefaultPaymentTTL 是待支付订单的存活时长。
//
// 与银联商务的订单有效期对齐：短于渠道的超时，才能保证我们关掉的每一单在渠道那边也
// 已经失效。它同时是 orders.expires_at 的取值来源和超时关单扫描的判断依据，两边用
// 同一个值——分别配一次就会出现「界面还在倒计时，后台已经把它关了」。
const DefaultPaymentTTL = 15 * time.Minute

// Options 是构造 OrderService 的可调项。零值等于用默认值，便于 main 里只写关心的那项。
type Options struct {
	// PaymentTTL 为 0 时用 DefaultPaymentTTL。
	PaymentTTL time.Duration
	// Now 为 nil 时用 time.Now。注入它才能测「过期」而不必真的等 15 分钟。
	Now func() time.Time
}

// OrderService 是订单业务层。
type OrderService struct {
	repository Repository
	devices    DeviceReader
	payments   PaymentCreator
	paymentTTL time.Duration
	now        func() time.Time
}

// New 构造业务层。devices 与 payments 都允许为 nil，但**不是同一种允许**：
//
//   - devices 为 nil 只是「没有设备可查」：纯会员订单不下发设备校验，测试里也没有设备。
//     一旦真的有饮品行而 devices 为 nil，createOrder 会明确失败而不是跳过校验。
//   - payments 为 nil 是「这个部署根本没接支付域」，发起支付会明确回 503。它和「支付服务
//     这次没答上来」是同一个结论、同一个出口，所以不必让每个调用点都判一次空。
func New(r Repository, devices DeviceReader, payments PaymentCreator, options Options) *OrderService {
	if options.PaymentTTL <= 0 {
		options.PaymentTTL = DefaultPaymentTTL
	}
	if options.Now == nil {
		options.Now = time.Now
	}
	return &OrderService{repository: r, devices: devices, payments: payments,
		paymentTTL: options.PaymentTTL, now: options.Now}
}

// PaymentTTL 暴露给调用方（超时关单的扫描周期要参考它）。
func (s *OrderService) PaymentTTL() time.Duration { return s.paymentTTL }

// Clock 暴露给调用方，让「同一时刻」在 service 和调用方之间是同一个值。
func (s *OrderService) Clock() time.Time { return s.now() }
