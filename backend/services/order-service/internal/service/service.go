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
	CreateDeviceOrder(ctx context.Context, p repository.CreateDeviceOrderParams) (*repository.CreateDeviceOrderResult, bool, error)
	// CreateRenewalOrder 落一笔会员续费单（第三条「钱已在别处收过」的路，见 create_renewal.go）。
	//
	// 它与 CreateDeviceOrder 是亲戚但不共用实现：两者的参数结构里有三成字段在各自那一路
	// 无意义（设备单没有用户，续费单没有设备与点位），合并出来的结构会让「这个字段这一路
	// 不用」变成一种心照不宣。理由逐条写在 repository.CreateRenewalOrderParams 上。
	CreateRenewalOrder(ctx context.Context, p repository.CreateRenewalOrderParams) (*repository.CreateRenewalOrderResult, bool, error)
	// FindDeviceOrderByThirdPartyNo 按对方单号查一张已有的设备单。
	//
	// 取货码那条路在**扣款之前**用它一次：那个单号被另一类设备单占着时要在动钱之前拒掉
	// （见 CreatePickupOrder）。它不是幂等的判据——判据在建单那一步的 ON CONFLICT 上。
	FindDeviceOrderByThirdPartyNo(ctx context.Context, thirdPartyOrderNo string) (*repository.DeviceOrderByThirdPartyNo, error)
	SettlePayment(ctx context.Context, p repository.SettlePaymentParams) (*repository.OrderPaymentResult, bool, error)
	CancelOrder(ctx context.Context, p repository.CancelOrderParams) (*repository.OrderPaymentResult, error)
	CompleteOrder(ctx context.Context, p repository.CompleteOrderParams) (*repository.OrderPaymentResult, error)
	ExpireOverdue(ctx context.Context, limit int, traceID string) (int, error)
	FindOrderByID(ctx context.Context, id string) (*model.Order, error)
	FindOrderByNo(ctx context.Context, orderNo string) (*model.Order, error)
	GetOrderDetail(ctx context.Context, id string) (*repository.OrderDetail, error)
	// ListOrderLines 只读行。发起支付那条热路径要的是行类型（分账 biz_type 由它推），
	// 不值得为它把订单头、出资行、流水、售后一起读出来，所以有一条窄查询。
	ListOrderLines(ctx context.Context, orderID string) ([]*model.OrderLine, error)
	ListOrders(ctx context.Context, f repository.OrderFilter) ([]*repository.OrderRow, int, error)
	ApplyAfterSale(ctx context.Context, p repository.ApplyAfterSaleParams) (*repository.AfterSaleRow, bool, error)
	// FortuneCardFreezeGate 是受理之前那次只读的「这一单这次要冻住哪几笔发放、一共几张」。
	// 它不开事务也不加锁，权威的那一判仍在 ApplyAfterSale 的锁内（见 repository 里的说明）。
	FortuneCardFreezeGate(ctx context.Context, orderID, userID, scope string) (repository.FortuneCardFreezePlan, bool, error)
	ReviewAfterSale(ctx context.Context, p repository.ReviewAfterSaleParams) (*repository.AfterSaleRow, error)
	// FindAfterSaleByNo 只服务于退款重试出口：要在调支付域之前看清楚这张单现在长什么样。
	FindAfterSaleByNo(ctx context.Context, afterSaleNo string) (*repository.AfterSaleRow, error)
	// StartRefund 是审核三步里的**事务 B**（approved → refunding），也服务于重试出口。
	// 第二返回值是「这次什么都没写」（重放）。
	StartRefund(ctx context.Context, p repository.StartRefundParams) (*repository.AfterSaleRow, bool, error)
	// AdvanceRefund 按一条退款结果事件推进售后与订单，第二返回值同样是重放。
	AdvanceRefund(ctx context.Context, p repository.AdvanceRefundParams) (*repository.AfterSaleRow, bool, error)
	CancelAfterSale(ctx context.Context, p repository.CancelAfterSaleParams) (*repository.AfterSaleRow, error)
	ListAfterSales(ctx context.Context, f repository.AfterSaleFilter) ([]*repository.AfterSaleRow, int, error)
}

// DeviceReader 是下单时校验设备状态的依赖，也是设备单建单时唯一的设备与饮品来源。
//
// 它还带着**本服务唯一一个写口**：DeductBalance（取货码那条路从设备余额里扣钱）。与
// 咖啡机域只有一条连接、一个客户端，所以它长在这个接口上而不是新加一个依赖——加第六个位置
// 参数会让 New 的每一个调用点都要跟着改，而它们要传的还是同一个对象。
type DeviceReader interface {
	Get(ctx context.Context, deviceID string) (*client.Device, bool, error)
	// DeductBalance 从一台设备的咖啡余额里扣一笔，幂等键是 RequestID（取货码那条路上就是
	// 对方单号）。applied=false 不是失败：那次扣减已经发生过了，调用方要接着把单建出来。
	DeductBalance(ctx context.Context, in client.DeductBalanceInput) (*client.DeductBalanceResult, error)
	// GetBySerial 按**机器自己认识的那个编号**读设备。线下刷卡机的回调里只有序列号（那台
	// 机器不可能知道我们的主键），所以这条是设备单唯一的入口。
	GetBySerial(ctx context.Context, serial string) (*client.Device, bool, error)
	// GetDeviceDrink 取一台设备上的那杯饮品：按设备 uuid + 机器报的编号查一条。
	//
	// 为什么是「查一条」而不是「列出这台机器的饮品再自己匹配」：列表是分页的，机器上饮品一多
	// 就会静默匹配不到，而那时钱已经收过了；而那个编号落在哪一列是饮品库的布局知识，不该抄到
	// 这里来（见契约里 GetDeviceDrink 的说明）。
	GetDeviceDrink(ctx context.Context, deviceID, drinkCode string) (*client.Drink, bool, error)
	// GetDrink 按**我们的 uuid** 取一杯饮品，是下单时给饮品行定价的那一条。
	//
	// 它与上面那条不是便利关系，是两把钥匙：那一条收机器报上来的编号，这一条收主键。
	// 小程序下单的调用方手里只有后者——客户端选中一件商品，给的是我们的 itemId。
	GetDrink(ctx context.Context, drinkID string) (*client.Drink, bool, error)
}

// MembershipPlanReader 是下单时读会员套餐的依赖。
//
// 会员行的价格与时长**只能**从这里来：客户端给一个套餐 ID，服务端拿它去会员域问「这个套餐
// 现在卖的是什么」，再把那份答案冻进订单行。这不是「多校验一道」，而是「价格只有一个来源」
// ——收客户端那份快照等于让客户端决定用户付多少、拿多久。
//
// 定义成接口而不是直接用 *client.MembershipPlanReader，理由与 DeviceReader 那条一样：
// 下单这条路上最容易写错的是**不碰网络的那部分**（金额怎么算、快照怎么拼、什么时候才允许
// 有会员行），它们必须能在没有会员服务的情况下被测到。
type MembershipPlanReader interface {
	Get(ctx context.Context, planID string) (*client.MembershipPlan, bool, error)
	// Entitlement 问「这个人此刻能不能直接按会员价买饮品」。
	//
	// 它长在同一个接口上，是因为答案只能从这个下游来：会员价那一列在饮品目录上，而
	// 「谁有资格按它买」在会员域——本服务两边都不知道，只能问。客户端的 membershipSnapshot
	// 不是这个答案的来源（见 CreateOrderInput.Request 上那段说明）。
	Entitlement(ctx context.Context, userID string) (*client.MemberPriceEntitlement, error)
}

// WalletIdentityReader 是发起支付时取付款人微信身份的依赖。
//
// 定义成接口而不是直接用 *client.WalletIdentityReader，理由与上面那几条一样：发起支付这条
// 路上要能测「取不到身份时这一笔怎么办」，而那不该需要一个真的 user-service。
type WalletIdentityReader interface {
	// MiniappOpenID 返回该用户在小程序下的 openid；found 为 false 表示没有绑定。
	MiniappOpenID(ctx context.Context, userID string) (openID string, found bool, err error)
}

// PaymentCreator 是对支付域的调用，今天有两个动作：发起支付、发起退款。
//
// 定义成接口而不是直接用 *client.PaymentCreator，理由与 Repository 那条一样：这两条路上
// 最容易写错的是那些**不碰网络的部分**（归属、状态、金额、过期、可退额），它们必须能在
// 没有支付服务的情况下被测到。
//
// 两个动作长在同一个接口上，是因为它们本来就是同一个下游的同一个客户端（见 client 里
// CreateRefund 的说明）。拆成两个接口只会让 New 多一个参数，而两个参数在 main 里永远是
// 同一个对象。
type PaymentCreator interface {
	Create(ctx context.Context, in client.CreatePaymentInput) (*dto.PayAction, error)
	// CreateRefund 把一笔已审核通过的售后单交给支付域去退钱。
	//
	// 它的幂等键是**售后单号本身**（AfterSaleNo），不是这里另给的一把钥匙：一张售后单
	// 最多落一张退款单，所以重发同一张单不会退两次钱。审核那条路上的重试出口正是靠它
	// 才敢直接重发（见 service.StartRefund）。
	CreateRefund(ctx context.Context, in client.CreateRefundInput) (*client.RefundOutcome, error)
}

// FortuneCardQuoter 是受理退款申请前问账户域的那一句：这一单赠送的福卡现在还冻得上吗。
//
// 定义成接口而不是直接用 *client.FortuneCardQuoter，理由与上面几条一样：这条规则的判据
// （什么时候该拒、什么时候该放行）是**不碰网络的那部分**，它必须能在没有账户服务的情况下
// 被测到——而它恰好是这条路上最容易写错的地方（把「还没发卡」读成「用过了」）。
type FortuneCardQuoter interface {
	// FreezeQuote 只算不写。它不会拒绝谁：冻不上几张是一个数，不是一次失败。
	FreezeQuote(ctx context.Context, in client.FreezeQuoteInput) (*client.FortuneCardFreezeQuote, error)
}

var (
	// —— 请求本身不合法（controller 统一回 400）——
	ErrInvalidSource          = errors.New("source must be miniapp or screen_qr")
	ErrLinesRequired          = errors.New("order must have at least one line")
	ErrTooManyLines           = errors.New("order has too many lines")
	ErrTooManyDrinkLines      = errors.New("an order can contain at most one drink line")
	ErrTooManyMembershipLines = errors.New("an order can contain at most one membership line")
	ErrInvalidLineType        = errors.New("lineType must be drink, addon or membership")
	ErrInvalidQuantity        = errors.New("quantity must be positive")
	ErrInvalidAmount          = errors.New("amounts must not be negative")
	ErrDiscountExceedsLine    = errors.New("line discount exceeds the original amount")
	ErrCouponOnlyOnDrink      = errors.New("coupons can only discount drink lines")
	ErrCouponNeedsDiscount    = errors.New("couponId and couponDiscountAmount must be given together")
	ErrAddonNeedsCampaign     = errors.New("addon lines must reference a campaign")
	ErrCampaignOnlyOnAddon    = errors.New("campaignId is only allowed on addon lines")
	// 会员行的形状。价格、时长、快照都不在这里判——它们由服务端从会员域取（见
	// service.applyMembershipPlan），请求里根本没有那些字段可判。
	ErrMembershipPlanIDRequired   = errors.New("membershipPlanId is required on membership lines")
	ErrMembershipPlanIDNotAllowed = errors.New("membershipPlanId is only allowed on membership lines")
	ErrMembershipPlanIDInvalid    = errors.New("membershipPlanId must be a uuid")
	// ErrMembershipQuantityInvalid：会员行只能是一份。
	//
	// 数量不是「买几期」的意思——买几期由套餐的 period/period_count 决定，而金额是
	// 「套餐价 × 数量」。放过 quantity=3 就等于让这一次付款收三份钱、只开一期会员。
	ErrMembershipQuantityInvalid = errors.New("a membership line must have quantity 1")
	ErrStoreMismatch             = errors.New("storeId does not match the store the device is deployed at")
	ErrDeviceRequired            = errors.New("deviceId is required when the order contains a drink line")
	ErrIdempotencyKeyRequired    = errors.New("Idempotency-Key is required")
	ErrUserRequired              = errors.New("user id is required")
	ErrCancelReasonRequired      = errors.New("cancellation reason is required")
	ErrPaymentMethodRequired     = errors.New("paymentMethod is required")

	// —— 设备单：请求形状不合法 ——
	//
	// 这三个都是「没给」而不是「给了但不对」：对方单号是幂等键（没有它这条路根本不成立，
	// 见 CreateDeviceOrder 的说明），序列号与机器报的饮品编号是仅有的两个定位依据。
	ErrThirdPartyOrderNoRequired = errors.New("thirdPartyOrderNo is required")
	ErrDeviceSerialRequired      = errors.New("deviceSerial is required")
	ErrDrinkCodeRequired         = errors.New("drinkCode is required")

	// —— 会员续费单：请求形状不合法 ——
	//
	// 只有两个。这一条路上的金额与套餐快照都**有意不校验**（调用方填的就是签约时冻结的那
	// 一份，而这次调用发生在钱已经收走之后，拒收只会让一笔真实发生过的交易进死信），
	// 所以这里没有「金额对不上」「快照不全」那一类。剩下这两个是「这一单根本立不起来」：
	// 用户为空则这一期扣款无处可归，套餐为空则会员行成了无主的一行。
	//
	// 对方单号为空复用上面那个 ErrThirdPartyOrderNoRequired——它在两条路上的含义完全一样。
	ErrRenewalAmountInvalid = errors.New("renewal amount must not be negative")
	ErrRenewalPlanRequired  = errors.New("renewal must carry the membership plan snapshot")

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

	// —— 状态与资源（controller 各自映射成 404/409/503；rpc 那边是 404/400/503，见
	// rpc.createDeviceOrderError 的分档表——同一个哨兵在两个出口上的档可以不同）——
	ErrDeviceNotFound = errors.New("device not found")
	// ErrDeviceUnavailable：机器存在，但我们不让它用（停用）。这是业务上的拒绝，不是故障。
	ErrDeviceUnavailable = errors.New("device is not active")
	// ErrDeviceLookupUnavailable：问不到设备（读端没配好，或者咖啡机服务这次没答上来）。
	// 与 ErrDeviceUnavailable 分开：前者重发一次可能就好了（503），后者重发一百次也一样（409）。
	// 混成一个会让调用方把一次下游抖动当成「这台机器停用了」，或者反过来无谓地重试。
	ErrDeviceLookupUnavailable = errors.New("device lookup is unavailable")
	// ErrDrinkNotFound：这台机器上没有这个编号的饮品。它与 ErrDeviceNotFound 是**两条不同的
	// 对外结论**，而且在 partner-service 那里落在**两个不同的档**上（见 rpc.createDeviceOrderError
	// 的分档表）：设备找不到是 NotFound→404「这台机器没在我们这儿登记」，饮品找不到是
	// InvalidArgument→400「这份报文要改（换个编号）」。
	//
	// 所以它**不是**「我们不卖这一杯」那种业务拒绝，而是「你报的这个编号在这台机器上不存在」：
	// 合作方该做的是改报文重投，不是去重新同步设备。合并成一句等于让它去猜，而这两种情况下
	// 它该做的事完全不同。
	//
	// 小程序下单那条路也用它，读法换一个：**目录里没有这一杯**（客户端拿着一个过期的
	// itemId）。两处的共同点正是它要表达的那一件事——「你给的这个标识在我这儿找不到对应的
	// 那一杯」，而调用方该做的都是换一个（改报文 / 刷新菜单让用户重挑），不是重试。
	ErrDrinkNotFound = errors.New("drink not found")
	// ErrDrinkLookupUnavailable：问不到饮品（饮品库没答上来）。与 ErrDeviceLookupUnavailable
	// 同理，是「我们这边没问到」（503），不是「这个标识不存在」（400）。两条路共用。
	ErrDrinkLookupUnavailable = errors.New("drink lookup is unavailable")
	// ErrDrinkOffShelf：这一杯还在目录里，但已经下架。
	//
	// 与 ErrDrinkNotFound 分开，是因为**这两句话对用户完全不同**，而它们只差一条 status：
	// 「这件商品不存在」是让客户端刷新菜单，「这件商品已下架」是告诉用户换一杯。合并成一个
	// 就等于把「运营下架了」说成「你手里的链接是坏的」。
	//
	// 它不进 ValidationErrors：请求一个字都没写错，判据是目录里的那一列（与 ErrDeviceUnavailable
	// 同一档，409）。
	ErrDrinkOffShelf = errors.New("drink is not on sale")
	// ErrDrinkDeviceMismatch：请求里的这一杯不挂在请求里那台设备上。
	//
	// 饮品行就是「某台设备上的一杯」，所以「用 A 店的饮品配 B 店的设备」不是一次普通的参数
	// 填错——它会让订单看起来完全合法，而价格、点位、机器全都对不上。空 device_id 的饮品行
	// （库里真有没挂设备的遗留行）不触发它：没有可比的设备，不是「挂错了设备」。
	ErrDrinkDeviceMismatch = errors.New("drink does not belong to the device on this order")
	// ErrDrinkItemIDRequired / ErrDrinkItemIDInvalid 是饮品行的形状：它只收一个 itemId。
	//
	// 前者是「没给」，后者是「给了个不是 uuid 的」。与会员行那两个同一条规矩——一条
	// drink 行没有 itemId 就没有任何定价依据，服务端无处可查。
	ErrDrinkItemIDRequired = errors.New("itemId is required on drink lines")
	ErrDrinkItemIDInvalid  = errors.New("itemId must be a uuid")
	// ErrDrinkNotPickupPriced：这一杯不能走取货码——取货码价与目录价都是 0，而取货码那条路上
	// 定价权在我们（报文里没有金额），两个价都拿不到就没法定这一单。
	//
	// 它**不在 ValidationErrors 里**：报文一个字段都没写错，错的是饮品库那一行还没定价。
	// 放进那份「请求不合法」的清单里会让 controller 的 400 判据说谎（那份清单是给 HTTP 用的），
	// 而对合作方这一档要单独说一句（400，见 rpc.createPickupOrderError 与 partner-service）。
	ErrDrinkNotPickupPriced = errors.New("drink has no pickup code price")
	ErrOrderNotPending      = errors.New("order is not awaiting payment")
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
	ErrCampaignOnlyOnAddon, ErrMembershipPlanIDRequired, ErrMembershipPlanIDNotAllowed,
	ErrMembershipPlanIDInvalid, ErrMembershipQuantityInvalid, ErrStoreMismatch, ErrDeviceRequired,
	ErrDrinkItemIDRequired, ErrDrinkItemIDInvalid, ErrDrinkDeviceMismatch,
	ErrIdempotencyKeyRequired, ErrUserRequired, ErrCancelReasonRequired, ErrPaymentMethodRequired,
	ErrAfterSaleScopeInvalid, ErrAfterSaleMembershipUnsupported, ErrAfterSaleLineRequired,
	ErrAfterSaleLineNotAllowed, ErrAfterSaleReasonRequired, ErrAfterSaleImagesInvalid,
	ErrAfterSaleRemarkRequired, ErrAfterSaleActionInvalid,
	ErrThirdPartyOrderNoRequired, ErrDeviceSerialRequired, ErrDrinkCodeRequired,
	// 续费单那两条。它**不在**这一组的常见位置上（上面那一行是设备单的），但归的类一样：
	// 「请求不合法」——调用方把值改对就行，不是我们这边的故障。
	ErrRenewalAmountInvalid, ErrRenewalPlanRequired,
	// ErrInvalidUUIDFilter：后台列表的 userId / storeId / deviceId 不是一个 uuid。它是
	// 「请求不合法」而不是「服务器错误」：调用方把筛选值改对就行。
	ErrInvalidUUIDFilter,
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
	// ErrThirdPartyOrderNoTaken：这个对方单号已经被**另一类**设备单占了（刷卡机那条与取货码
	// 那条共用一个单号空间）。判据在仓储（那边才看得到既有订单的 payment_method），见
	// repository.ErrThirdPartyOrderNoTaken 的说明。
	ErrThirdPartyOrderNoTaken = repository.ErrThirdPartyOrderNoTaken
	// ErrInvalidUUIDFilter：后台列表的 uuid 筛选值形状不对。判据在仓储（那里才知道哪一列是
	// uuid），但「请求不合法」这条结论要从 service 出去，controller 才能回 400——与上面那几条
	// 同一个理由：controller 只认 service 这一个包的错误。
	ErrInvalidUUIDFilter = repository.ErrInvalidUUIDFilter

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
	// ErrAfterSaleFortuneCardsUsed：这一单赠送的福卡已经用过了，**不受理**这次退款申请。
	//
	// 与上面那条是同一条规则的两个时刻：那条是审核时请人确认「没抽过奖」，这一条是申请时
	// 系统自己判——判得出来就不该轮到人。判据是账户域答的两个数（见 client.FortuneCardFreezeQuote），
	// 因为福卡流水是一口池子（抽奖那一笔只记 draw:{requestId}，从不指向它消耗的是哪一次
	// 发放），「这一单那几张还在不在」在库里没有直接答案。
	ErrAfterSaleFortuneCardsUsed = errors.New("fortune cards of this order have been used")
	// ErrOrderHasNoPayment：这一单没有支付单（设备单），退款这条链装不下它。
	ErrOrderHasNoPayment = repository.ErrOrderHasNoPayment
	// ErrAfterSaleNotApproved：这张售后单还没审核通过，不能发起退款。
	ErrAfterSaleNotApproved = repository.ErrAfterSaleNotApproved
	// ErrAfterSaleNotRefunding：这张售后单不在退款推进中。
	ErrAfterSaleNotRefunding = repository.ErrAfterSaleNotRefunding
	// ErrAfterSaleRefundMismatch：收到的退款单号与这张售后单上记的对不上，要人来看。
	ErrAfterSaleRefundMismatch = repository.ErrAfterSaleRefundMismatch
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
	// ErrFortuneCardQuoteUnavailable：问不到账户域「这一单的福卡现在还冻得上吗」。
	//
	// 它是**故障**，不是业务结论，两个方向都不能去：当成「没用过」放行，那条规则就会在
	// 账户域抖动时静默失效（它挡的正是「卡已经抽掉了还想退钱」）；当成「用过了」则是在毫无
	// 证据的情况下冤枉用户。所以它自己一个结论，回 503 让人重试。
	ErrFortuneCardQuoteUnavailable = client.ErrFortuneCardServiceUnavailable

	// —— 退款：退的那一侧同理，四种结论与上面四个一一对应 ——
	//
	// 分开命名（而不是复用 ErrPaymentRejected 那几个）是为了让后台的提示分得清「这次退款
	// 被拒了」与「这次支付被拒了」——两句话对客服的含义完全不同。见 client/refund.go。
	//
	// ErrRefundRejected：这笔钱现在退不了（支付单没收妥、超过可退余额、渠道不支持）。
	ErrRefundRejected = client.ErrRefundRejected
	// ErrRefundConflict：上一笔退款还在推进，或者这把售后单号被换过请求体。
	ErrRefundConflict = client.ErrRefundConflict
	// ErrRefundUncertain：**得不出结论**。退款单停在支付侧的 pending / processing，重发
	// 同一张售后单是安全的——后台那个重试按钮就是为它准备的。
	ErrRefundUncertain = client.ErrRefundUncertain
	// ErrRefundServiceUnavailable：支付服务没答上来。是故障，不是业务结论。
	ErrRefundServiceUnavailable = client.ErrRefundServiceUnavailable

	// ErrRefundNotStarted：**审核已经落库了，但退款单没建起来**。
	//
	// 它不在这两组里的任何一组：不是「请求不合法」（请求一个字都没错），也不是上面那四个
	// 之一（那四个说的是「退款单建没建成」，而这个说的是「已经批了但钱还没上路」）。
	// 后台必须把它与「审核失败」分开说，否则同一个人会再点一次「通过」，而第二次只会
	// 得到「这张单已经不在待审核状态」——一个真正的死胡同。正确的下一步是重试退款
	// （见 StartRefund 与后台那个「发起退款」按钮）。
	//
	// 它包着上面四个之一（用 %w 串起来），所以判完这一条还能继续往下判是哪种原因。
	ErrRefundNotStarted = errors.New("the approval was saved but the refund was not started")

	// —— 下单：结论来自会员域 ——
	//
	// ErrMembershipPlanNotFound：这一单点的那个套餐买不了（不存在，或者已经下架）。
	// 它与「会员服务没答上来」分开的理由，与设备那两条完全一样：前者让用户重新挑一个套餐
	// （400），后者是我们暂时答不上来（503），把后者说成前者会让一次下游抖动看起来像
	// 「你选的套餐买不了」。
	ErrMembershipPlanNotFound = client.ErrMembershipPlanNotFound
	// ErrMembershipPlanUnavailable：问不到会员域（读端没配好，或者会员服务这次没答上来）。
	ErrMembershipPlanUnavailable = client.ErrMembershipPlanUnavailable
	// ErrMemberPriceUnavailable：问不到「这个人此刻算不算会员价」。
	//
	// 它**不是**「会员服务挂了」的同义词，而是饮品行定价缺了一块拼不上：这一杯该按原价还是
	// 会员价卖，只有会员域答得上来。所以问不到时不能退回原价继续下单——那会把一次下游抖动
	// 变成「悄悄按原价卖给了会员」，用户不会知道，我们也不会。回 503 让他重试。
	ErrMemberPriceUnavailable = client.ErrMemberPriceUnavailable

	// —— 发起支付：结论来自身份域 ——
	//
	// ErrWalletIdentityUnavailable：问不到付款人的微信身份（读端没配好，或者身份服务这次
	// 没答上来）。它**不**包含「这个人没绑微信」——那是一个事实、不是一个错误：要不要为它
	// 停下来由支付侧按用户选的那个支付方式决定。
	ErrWalletIdentityUnavailable = client.ErrWalletIdentityUnavailable

	// —— 取货码：结论来自咖啡机域的扣减 ——
	//
	// 五条各自的处置完全不同，所以一条都不能省（见 client 那边每一条的说明）：
	// 码不对与余额不够要变成机器前面两句不同的话，而「没问到」是唯一一条可以重投的。
	ErrDeviceBalanceRejected           = client.ErrDeviceBalanceRejected
	ErrDeviceBalanceDeviceMissing      = client.ErrDeviceBalanceDeviceMissing
	ErrDeviceBalancePasswordRejected   = client.ErrDeviceBalancePasswordRejected
	ErrDeviceBalanceNotEnough          = client.ErrDeviceBalanceNotEnough
	ErrDeviceBalanceServiceUnavailable = client.ErrDeviceBalanceServiceUnavailable
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
	plans      MembershipPlanReader
	wallets    WalletIdentityReader
	// fortuneCards 是受理退款申请前问账户域的那一句。与上面几个不同，它是**必填**的：
	// 为空时不放行（见 assertFortuneCardsUnused）。把「没接上」当成「那就放行」等于这条
	// 规则可以在没人察觉的情况下失效。
	fortuneCards FortuneCardQuoter
	paymentTTL   time.Duration
	now          func() time.Time
}

// New 构造业务层。devices、payments、plans、wallets、fortuneCards 都允许为 nil，但
// **不是同一种允许**：
//
//   - devices 为 nil 只是「没有设备可查」：纯会员订单不下发设备校验，测试里也没有设备。
//     一旦真的有饮品行而 devices 为 nil，createOrder 会明确失败而不是跳过校验。
//   - plans 为 nil 同理：不带会员行的订单不问会员域，测试里也没有会员服务。一旦真的有
//     会员行而 plans 为 nil，createOrder 会明确失败——**绝不退化成「那就用请求里那份快照」**，
//     因为那份快照决定了用户付多少钱、拿多久。
//   - payments 为 nil 是「这个部署根本没接支付域」，发起支付会明确回 503。它和「支付服务
//     这次没答上来」是同一个结论、同一个出口，所以不必让每个调用点都判一次空。
//   - fortuneCards 为 nil 与上面几条**都不同**：它不是「这次用不到」，而是「这条部署少接
//     了一个账户域」。受理退款申请时会明确失败（503），而不是跳过那道判断——跳过等于这条
//     规则在账户域缺席时静默失效，而它挡的是「卡已经抽掉了还想退钱」。测试里造它是为了一根
//     桩，不是为了绕过。
//   - wallets 为 nil 是「这个部署问不到用户身份」。它与 devices 那种「只有在需要时才成
//     必需」不同：用户选的方式是不是需要 openid 由支付侧才知道，所以缺了它没有任何一条
//     支付能安全地发起——空 openid 发出去等于让用户去渠道那里碰一次壁。发起支付会明确
//     回 503（见 InitiatePayment），而不是发一次没有身份的请求。
func New(r Repository, devices DeviceReader, payments PaymentCreator, plans MembershipPlanReader, wallets WalletIdentityReader, fortuneCards FortuneCardQuoter, options Options) *OrderService {
	if options.PaymentTTL <= 0 {
		options.PaymentTTL = DefaultPaymentTTL
	}
	if options.Now == nil {
		options.Now = time.Now
	}
	return &OrderService{repository: r, devices: devices, payments: payments, plans: plans, wallets: wallets,
		fortuneCards: fortuneCards, paymentTTL: options.PaymentTTL, now: options.Now}
}

// PaymentTTL 暴露给调用方（超时关单的扫描周期要参考它）。
func (s *OrderService) PaymentTTL() time.Duration { return s.paymentTTL }

// Clock 暴露给调用方，让「同一时刻」在 service 和调用方之间是同一个值。
func (s *OrderService) Clock() time.Time { return s.now() }
