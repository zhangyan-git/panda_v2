// Package service 是会员的业务层：校验、把「成交快照」翻成会员该有的样子、决定什么算续费。
//
// 按聚合拆文件：plan.go 是套餐定义、membership.go 是会员资格（读、开通续期的判定、后台人工
// 干预）、entitlement.go 是那个回答「这个人此刻算不算会员价」的判定、event.go 消费别的域发过来
// 的五条事件（order.paid + 支付域的两条协议变更 + 两条某一期代扣的结果）。
//
// # 这一刀的形状
//
// 一个闭环：**买 → 生效 → 到点失效**。
//
//   - 买：order-service 里的一个订单行，付款成功后发 order.paid；本服务消费它，开通或续期。
//   - 生效：memberships 上那一行 + membership_changes 里的一条流水 + 一条 membership.activated
//     / membership.renewed 事件，同一个事务。
//   - 到点失效：ExpireDue 扫描把 status 从 active 改成 expired。
//
// **连续包月这一条链是完整的**：签约（signing.go，微信直连）、发起扣款（charge.go，扫到点的
// 订阅向支付服务要账）、结算（event.go 里那两条扣款事件，续会员 / 记失败 / 连到阈值停扣）三截
// 各在一个文件里。三截缺一个的表现都不同：缺签约是「没有订阅」，缺发起是「签了却不扣」，缺结算
// 是「扣了钱不续会员」。各自不做什么，见那三个文件的头一段。
//
// # 会员价的判定只有一处
//
// 「这个人此刻算不算会员价」是本服务对别的域**唯一**的输出，走 gRPC
// GetMemberPriceEntitlement（见 entitlement.go）。别的服务不该读本库，也不该按
// member_price_mode 自己推——那条规则的完整表述（在有效期内、没被冻结、撤销了没有、这个套餐
// 的会员价是自动给还是要用券）只在这里有一份。
package service

import (
	"context"
	"errors"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/panda-dev/panda-v2/backend/services/membership-service/internal/client"
	"github.com/panda-dev/panda-v2/backend/services/membership-service/internal/dto"
	"github.com/panda-dev/panda-v2/backend/services/membership-service/internal/model"
	"github.com/panda-dev/panda-v2/backend/services/membership-service/internal/repository"
)

// Repository 是会员库的数据访问契约。
//
// 定义成接口而不是直接用 *repository.PostgresRepository，理由与本仓库其余服务相同：让这一层
// 里最容易错的那部分——成交快照的搬运、套餐的配对校验、entitlement 的判定——能在没有数据库的
// 情况下测。那些逻辑不该为了测它去起一个 PG。
//
// **订阅（连续包月）这一段的四个后台方法里三个是只读**：订阅只能由小程序签约产生，后台既建
// 不了也不能改它的状态（只能取消）。微信直连那一刀之后多了三个写方法（CreateSubscription /
// FindSubscriptionByChangeRequest / SettleSubscription），但它们服务的是**签约那条链**，
// 后台的入口一个都没多。见 repository/subscription.go 上的长说明。
type Repository interface {
	// —— 套餐 ——
	CreatePlan(ctx context.Context, code string, p repository.PlanParams) (*model.Plan, error)
	UpdatePlan(ctx context.Context, id string, p repository.PlanParams) (*model.Plan, error)
	SetPlanStatus(ctx context.Context, id, status string) (*model.Plan, error)
	GetPlan(ctx context.Context, id string) (*model.Plan, error)
	GetPlanByCode(ctx context.Context, code string) (*model.Plan, error)
	ListPlans(ctx context.Context, q dto.PlanQuery) ([]*model.Plan, int, error)
	ListActivePlans(ctx context.Context) ([]*model.Plan, error)

	// —— 会员资格 ——
	GetMembership(ctx context.Context, userID string) (*model.Membership, error)
	GetMembershipByID(ctx context.Context, id string) (*model.Membership, error)
	ListMemberships(ctx context.Context, q dto.MembershipQuery) ([]*model.Membership, int, error)
	ListChanges(ctx context.Context, membershipID string) ([]*model.Change, error)

	// —— 由支付驱动（系统路径，不记审计）——
	ApplyPaidOrder(ctx context.Context, p repository.PaidOrderParams) (*model.Membership, error)

	// —— 后台直接开通（人工，记审计）——
	GrantMembership(ctx context.Context, p repository.GrantParams) (*model.Membership, error)

	// —— 后台人工干预（记审计）——
	FreezeMembership(ctx context.Context, id, adminID, reason, traceID, requestID string) (*model.Membership, error)
	UnfreezeMembership(ctx context.Context, id, adminID, reason, traceID, requestID string) (*model.Membership, error)
	RevokeMembership(ctx context.Context, id, adminID, reason, traceID, requestID string) (*model.Membership, error)
	AdjustExpireAt(ctx context.Context, id, adminID string, expireAt time.Time, reason, remark, traceID, requestID string) (*model.Membership, error)

	// —— 用户自己的开关 ——
	SetAutoRenew(ctx context.Context, id string, enabled bool, operatorType, operatorID, reason, traceID, requestID string) (*model.Membership, error)

	// —— 定时扫描 ——
	ExpireDue(ctx context.Context, limit int, traceID string) (int, error)
	// ListDueSubscriptions 取一批到点该扣的订阅（续费扫描用）。判据与 model.Subscription.IsDue
	// 逐字相同，**不加行锁**——见 repository.ListDueSubscriptions 里那段说明。
	ListDueSubscriptions(ctx context.Context, now time.Time, limit int) ([]*repository.SubscriptionRow, error)

	// —— 连续包月订阅（后台只读 + 一个「取消」）——
	ListSubscriptions(ctx context.Context, q dto.SubscriptionQuery) ([]*repository.SubscriptionRow, int, error)
	SubscriptionStats(ctx context.Context, now time.Time) (dto.SubscriptionStats, error)
	GetSubscription(ctx context.Context, id string) (*repository.SubscriptionRow, error)
	CancelSubscription(ctx context.Context, p repository.CancelSubscriptionParams) (*repository.SubscriptionRow, error)

	// —— 连续包月订阅（签约：小程序发起、三条路收口）——
	//
	// 这三个都**没有对应的后台入口**：他们服务的是签约那条链（用户点「开通连续包月」、用户从
	// 微信回来确认、协议事件推过来），后台能做的仍然只有「看」与「取消」。
	CreateSubscription(ctx context.Context, p repository.CreateSubscriptionParams) (*repository.CreateSubscriptionOutcome, error)
	// FindSubscriptionByChangeRequest 是签约那条路上的幂等快路径：同一个 requestId 的重试回来
	// 时，先靠它认出「这就是刚才那一次」，而不是再建一份协议（见 CreateSubscription 的说明）。
	FindSubscriptionByChangeRequest(ctx context.Context, userID, requestID string) (*repository.SubscriptionRow, error)
	// GetLiveSubscription 取这个会员活着的那条订阅（pending_sign / active / suspended）。
	// 没有时回 repository.ErrSubscriptionNotFound。**在调支付服务之前**必须先问它一次，见
	// CreateSubscription。
	GetLiveSubscription(ctx context.Context, membershipID string) (*model.Subscription, error)
	// SettleSubscription 把一条订阅改成它现在实际该处于的状态，三条路共用一个入口。
	// changed 为 false 表示这次核查什么都没改（已经是那个状态了）。
	SettleSubscription(ctx context.Context, p repository.SettleParams) (*repository.SubscriptionRow, bool, error)
	// SettleCharge 把**一期代扣的结果**落到订阅与会员上（成功那支还会续一期会员）。
	//
	// 它与 SettleSubscription 是两件事、不能合并：那一个收口的是**协议**的状态（签约了、
	// 解约了），这个落的是一期**钱**的结论。协议 active 而某一期没扣成，是完全正常的一种组合。
	SettleCharge(ctx context.Context, p repository.ChargeSettleParams) (*repository.ChargeSettleOutcome, error)
	// LoadChargeContext 是**建单之前**那一次读：这一期的订阅，以及它挂在谁的那份会员上。
	//
	// 它服务的是「先建单、再结算」那条顺序（见 service.recordRenewalOrder）：建单要的是签约时
	// 冻结的那份套餐快照，而它在结算之前只能从这两行读出来——结算之后就晚了，一条重投会先撞上
	// 幂等、把建单那一步整个跳过去。
	LoadChargeContext(ctx context.Context, agreementID string) (*repository.ChargeContext, error)

	// —— 扣款成功但账没落成时的待办（见 repository/charge_settlement.go）——
	//
	// 这四个只服务**一条链**：事件入口建单失败时落一行（ParkChargeSettlement），worker 定时取
	// 一批重试（ClaimDueChargeSettlements），落成了就删（DeleteChargeSettlement）、没成就推后
	// （RescheduleChargeSettlement）。它们不是给别的读写路径用的——这张表是一张工作队列，不是
	// 业务数据（见 migrations/membership/009 的文件头）。
	ParkChargeSettlement(ctx context.Context, p repository.ChargeSettleParams) error
	ClaimDueChargeSettlements(ctx context.Context, limit int) ([]*model.ChargeSettlement, error)
	DeleteChargeSettlement(ctx context.Context, providerTransactionID string) error
	RescheduleChargeSettlement(ctx context.Context, providerTransactionID string, nextAttemptAt time.Time, lastError string) error

	// —— 店铺码会员活动（配置侧 CRUD + 领取）——
	//
	// 领取那一条**只写它自己的两张表和会员**：券那一半没做（老系统的领取还发券，而 V2 今天
	// 没有服务间发券的入口——coupon-service 只有一条管理员 JWT 的后台接口），所以活动上也就
	// 没有可配的券。
	CreateCampaign(ctx context.Context, p repository.CampaignParams, adminID string) (*repository.CampaignRow, error)
	UpdateCampaign(ctx context.Context, id string, p repository.CampaignParams, adminID string) (*repository.CampaignRow, error)
	SetCampaignStatus(ctx context.Context, id, status, adminID string) (*repository.CampaignRow, error)
	GetCampaign(ctx context.Context, id string) (*repository.CampaignRow, error)
	ListCampaigns(ctx context.Context, q dto.CampaignQuery) ([]*repository.CampaignRow, int, error)
	ListCampaignClaims(ctx context.Context, campaignID string, q dto.CampaignClaimQuery) ([]*model.CampaignClaim, int, error)
	// ClaimCampaign 返回的是「这次领取的结论」，含「是不是一次重放」——见 ClaimOutcome。
	ClaimCampaign(ctx context.Context, p repository.ClaimParams) (*repository.ClaimOutcome, error)
}

// 请求本身不合法（controller 统一回 400）。
//
// ID 类错误**按实体分开**（照 lottery-service 与 payment-service 的写法）：合成一句「ID
// 不合法」的话，运营在后台会员页点错一行时看到的是「ID 不是合法的 UUID」，而他不知道该去改
// 哪一格。
var (
	// —— 套餐 ——
	ErrPlanIDRequired   = errors.New("planId is required")
	ErrPlanIDInvalid    = errors.New("planId must be a uuid")
	ErrPlanCodeRequired = errors.New("code is required")
	// ErrPlanCodeInvalid：编码不是 slug。
	//
	// 编码是下单、签约、代扣都要对接的稳定标识，会出现在事件体、日志与对账表里。限制成
	// 小写字母数字与下划线，是为了让它在这些东西里**永远不需要转义**——一个带空格或斜杠的
	// 编码迟早会在某一条 URL 或某一句日志里被截断，而那时候已经有一批订单挂在它上面了。
	ErrPlanCodeInvalid      = errors.New("code may only contain lowercase letters, digits and underscores")
	ErrPlanNameRequired     = errors.New("plan name is required")
	ErrPlanPriceNegative    = errors.New("priceCents must not be negative")
	ErrPlanPriceTooLarge    = errors.New("priceCents is unreasonably large")
	ErrPlanPeriodInvalid    = errors.New("period is not one of month, year")
	ErrPlanPeriodCountRange = errors.New("periodCount must be positive and at most 120")
	// ErrPlanWechatIDRequired / ErrPlanWechatIDNotAllowed 是一对。
	//
	// 库上两条 CHECK 钉着它们（「要签约就必须有模板 ID」与「coupon 模式必须配齐券」），但库
	// 报出来的是一句 23514（500）。分成两句是因为用户该做的动作不同：一个是去把签约模板 ID
	// 填上，另一个是去配券模板与张数。
	ErrPlanWechatIDRequired   = errors.New("wechatPlanId is required when autoRenew is on")
	ErrPlanWechatIDNotAllowed = errors.New("wechatPlanId is only used when autoRenew is on")
	ErrPlanPriceModeInvalid   = errors.New("memberPriceMode is not one of auto, coupon")
	// 券配置与模式**必须配对**：coupon 全给、auto 全空。库上的 CHECK 是最后一道，
	// 但那道防线报的是 500，而这里能说清是缺了哪一半。
	ErrPlanCouponFieldsRequired   = errors.New("coupon mode needs both a coupon template and a count per period")
	ErrPlanCouponFieldsNotAllowed = errors.New("coupon fields are only used in coupon mode")
	ErrPlanCouponsPerPeriodRange  = errors.New("memberPriceCouponsPerPeriod must be positive")
	ErrPlanStatusInvalid          = errors.New("status is not one of draft, active, disabled")
	// ErrPlanBenefitsTooMany / ErrPlanBenefitTooLong：权益文案的条数与长度。
	//
	// 它只用于展示，但它是**运营手输的、会原样回给 C 端的一段文本**。不设上限的话，一次粘错
	// 就能把一个 JSONB 数组塞成几十万字符，而它每读一次套餐列表都要序列化一遍。
	ErrPlanBenefitsTooMany = errors.New("too many benefit lines")
	ErrPlanBenefitTooLong  = errors.New("a benefit line is too long")

	// —— 会员 ——
	ErrMembershipIDRequired = errors.New("membershipId is required")
	ErrMembershipIDInvalid  = errors.New("membershipId must be a uuid")
	ErrUserIDRequired       = errors.New("userId is required")
	ErrUserIDInvalid        = errors.New("userId must be a uuid")
	ErrFreezeReasonRequired = errors.New("freeze reason is required")
	// ErrUnfreezeReasonRequired 与 ErrFreezeReasonRequired **不共用**。
	//
	// 解冻也是一次人要负责的动作（「为什么给他恢复了」），理由同样必填；共用一条的话，运营在
	// 解冻页面上会看到一句写着「请填写冻结原因」的话，而他要做的是解冻。
	ErrUnfreezeReasonRequired = errors.New("unfreeze reason is required")
	ErrRevokeReasonRequired   = errors.New("revoke reason is required")
	ErrAdjustReasonRequired   = errors.New("adjust reason is required")
	// ErrAdjustExpireRequired：调整有效期时没给时间，或者给的是一个零值时刻。
	//
	// **没有一条「时间格式不对」的错误**：dto.AdjustRequest 上那个字段就是 time.Time，
	// 一个写不成时间的值在 controller 解请求体时就已经失败了（回「请求体格式不正确」），
	// 根本走不到这一层。写一条永远回不出来的错误只会让人以为它在守着什么。
	ErrAdjustExpireRequired = errors.New("expireAt is required")
	ErrAdjustRemarkTooLong  = errors.New("remark is too long")
	// —— 后台直接开通 ——
	ErrGrantReasonRequired = errors.New("grant reason is required")
	// ErrGrantRequestIDRequired：请求体里没带幂等键。
	//
	// 它是**必填**而不是可选的：后台开通的每一次提交都必须能被重试而不开出第二条会员，而
	// 服务端没有任何别的东西能认出「这是同一次点击」。缺了它，一次网络抖动后的重试会让操作员
	// 看到「这个人已经是会员了」——一个在这种场景下纯属假警报的结论。
	ErrGrantRequestIDRequired = errors.New("requestId is required")
	ErrStoreIDInvalid         = errors.New("storeId must be a uuid")
	// ErrStoreNotFound：选的那家门店在商户域查不到。
	//
	// 与「问不到」分开：这一条是操作员在下拉里选错了（或者门店刚被删），重选一个就行；
	// 而商户服务连不上是**我们这边**的问题，那种情况必须让开通失败而不是放一条查无此店的
	// 归属进去（见 service.StoreResolver 的说明）。
	ErrStoreNotFound = errors.New("this store does not exist")
	// ErrGrantExpireTooEarly：手动填的到期日不晚于此刻。
	//
	// 库上有 CHECK (expire_at > start_at) 兜底，但撞上去是一条 23514（500），而它其实是操作员
	// 填错了一个日期——该在写之前就翻成一句能显示给运营的话。
	ErrGrantExpireTooEarly     = errors.New("expireAt must be later than now")
	ErrOccurredRangeInvalid    = errors.New("expireTo must be later than expireFrom")
	ErrMembershipStatusInvalid = errors.New("status is not one of active, frozen, expired, revoked")
	// ErrSubscriptionStatusInvalid：订阅状态不是那五个码之一。
	//
	// 与会员状态那条分开：它们考的是两张表上的 CHECK，共用一个错误值会让提示里那句「状态」
	// 指不清是哪一张表的状态（两个页面长得像，运营两边都可能填错）。
	ErrSubscriptionStatusInvalid = errors.New("status is not one of pending_sign, active, suspended, cancelled, expired")
	// ErrCancelReasonRequired：取消订阅没填原因。
	//
	// 与 ErrAdjustReasonRequired 分开（而不是共用一条「请填写原因」）：这里取消的是**用户的
	// 钱袋子**（他下个月会不会被扣），事后的留痕里「为什么」比「谁」更值得有。
	ErrCancelReasonRequired = errors.New("cancel reason is required")
	// ErrReasonTooLong 是各处「必填理由」共用的长度上限错误。
	//
	// 冻结、撤销、调整三种理由给用户看的话不一样（人话表按错误值索引的是**缺少**那一条），
	// 但「太长了」是同一件事，共用一条不会让任何一句提示变得含糊。
	ErrReasonTooLong = errors.New("reason is too long")
	// **没有一条「这个用户还不是会员」的错误**：那条路走的是 ErrMembershipNotFound——它本来
	// 就是仓储查不到行时回的那一条，再包一个同义的值只会让 controller 的人话表里多一行长得
	// 一样的话。C 端据此显示「你还不是会员」，见 controller.MiniappMembershipController.Me。
)

// 事件相关（**这两条都是非 nil 返回值，即让消息重投 / 进死信**）。
var (
	// ErrInvalidEvent：收到的事件体不合法。
	//
	// 与「请求不合法」（400）不是一回事，尽管名字像：那个说的是 HTTP 请求，这个说的是 MQ 上
	// 的一条消息。它**不该被当成可重试的故障**——重投一百次也不会让一个缺了用户 ID 的事件变
	// 合法。回它是为了让消息进死信，那里是人会来看的地方。
	ErrInvalidEvent = errors.New("the event payload is malformed")
	// ErrInvalidUserID：事件里的用户 ID 不是一个 uuid。
	//
	// 单独一条（而不是并进 ErrInvalidEvent）：它指向发出方一个具体的 bug（用户 ID 是谁拼错
	// 的），而其余「事件体不合法」指向的是结构问题。日志里分得清。
	ErrInvalidUserID = errors.New("the event carries an invalid userId")
)

// 身份相关（controller 映射成 401）。
var (
	// ErrActorRequired：这次后台操作没有留下是谁做的。
	//
	// 冻结、撤销、调整有效期都要把操作人写进审计与流水。一条**查不到操作人**的人工调整记录
	// 等于没有记录——而它改的是「这个人还能不能用会员价」。
	ErrActorRequired = errors.New("the request carries no actor identity")
)

// 状态与结论（controller 各自映射成 404 / 409）。
var (
	// ErrPlanNotFound：套餐不存在。
	ErrPlanNotFound = repository.ErrPlanNotFound
	// ErrPlanCodeTaken：套餐编码被别的套餐占了。
	ErrPlanCodeTaken = repository.ErrPlanCodeTaken
	// ErrPlanNotSellable：这个套餐还买不了（不是 active）。
	//
	// 下单那一刻就会撞上它（order-service 会来问），但后台的「预览这个小程序套餐列表」也会。
	ErrPlanNotSellable = errors.New("this plan is not on sale")

	// ErrMembershipNotFound：这条会员不存在。
	ErrMembershipNotFound = repository.ErrMembershipNotFound
	// ErrMembershipRevoked：这条会员已被撤销，不接受任何自动恢复。
	ErrMembershipRevoked = repository.ErrMembershipRevoked
	// ErrMembershipExpired：这条会员已经过期，这个动作做不了。
	//
	// 只有「给过期会员打开自动续费」会回它：过期不是「不该有任何操作」（他还能重新买），
	// 只是「开了也不会扣」。
	ErrMembershipExpired = repository.ErrMembershipExpired
	// ErrMembershipNotActive：这条会员不是 active，冻结做不了。
	ErrMembershipNotActive = repository.ErrMembershipNotActive
	// ErrMembershipNotFrozen：这条会员没被冻结，解冻做不了。
	ErrMembershipNotFrozen = repository.ErrMembershipNotFrozen
	ErrExpireBeforeStart   = repository.ErrExpireBeforeStart
	// ErrDuplicateChange：这一单的这一种变更已经记过了。
	//
	// **它不是故障**：支付回调重放会撞上它，而那时正确的结果是「这件事已经做过了」。
	ErrDuplicateChange = repository.ErrDuplicateChange
	// ErrMembershipExists：这个用户已经有会员了。
	//
	// 两个来源（见仓储上那条的说明）：后台开通撞上已有会员（**正常答案**，不是异常），以及
	// 两笔支付同时被处理时读-写空隙里被插了一行。两条都是 409。
	ErrMembershipExists = repository.ErrMembershipExists
	// ErrAutoRenewNeedsSigning：打开自动续费要先签约，这一步只能从客户端发起。
	//
	// 它**不是**请求不合法（用户填的东西没有错），也不是 404/409 里的任何一类——它是「你得先
	// 去做另一件事」。所以它单独一条，由 controller 映射成一个说得清楚的提示，而不是让用户
	// 看到「操作失败」。见 SetAutoRenewByUser 的说明。
	ErrAutoRenewNeedsSigning = errors.New("opening auto renew requires signing a payment agreement first")

	// ErrSubscriptionNotFound：这条订阅不存在。
	ErrSubscriptionNotFound = repository.ErrSubscriptionNotFound
	// ErrSubscriptionNotCancellable：这条订阅当前的状态不允许取消（只有 active / suspended 能取消）。
	ErrSubscriptionNotCancellable = repository.ErrSubscriptionNotCancellable
	// ErrSubscriptionRequestUsed：这个幂等号之前用过，而它建的那条订阅已经结束了。
	ErrSubscriptionRequestUsed = repository.ErrSubscriptionRequestUsed
	// ErrSubscriptionNotSettleable：这条订阅的当前状态不允许被收口成生效中。
	ErrSubscriptionNotSettleable = repository.ErrSubscriptionNotSettleable
	// ErrLiveSubscriptionExists：这个会员已经有活着的订阅了。
	//
	// **在调支付服务之前**就得判：晚一步就会在微信里多建一份协议，而那一份是用户能点开的
	// ——等于给他留了两份「每月自动续费」的授权。见 CreateSubscription 里的顺序说明。
	ErrLiveSubscriptionExists = repository.ErrLiveSubscriptionExists
	// ErrAgreementTaken：这份代扣协议已经绑在别的订阅上了。
	ErrAgreementTaken = repository.ErrAgreementTaken
	// ErrAgreementMismatch：回来的协议号与本地这一行记着的对不上。不要照着往下写（理由见仓储上
	// 那一条），它是**要人来看**的一类，不是用户能修的东西。
	ErrAgreementMismatch = repository.ErrAgreementMismatch

	// ErrSubscriptionPlanNotSubscription：这个套餐不是连续包月，签不了约。
	//
	// 与「套餐不存在」分开：客户端点错了入口（拿一次性的套餐来签约）该看到「这款套餐不支持自动
	// 续费」，而不是一句含糊的失败。判据是 auto_renew 且 wechat_plan_id 非空——**两个都要**：
	// 前者是「这个套餐设计上就该续」，后者是「续的时候拿什么去扣」，缺任何一个都会签出一份扣不
	// 了款的协议。
	ErrSubscriptionPlanNotSubscription = errors.New("this plan is not a subscription plan")
	// ErrSubscriptionRequestIDRequired：签约请求没带幂等号。
	//
	// 它是 400 而不是「帮客户端生成一个」：签约是有副作用的动作（在渠道建一份授权），幂等号必须
	// 由发起方给，服务端替他编一个等于让每一次重试都签一份新的。
	ErrSubscriptionRequestIDRequired = errors.New("a subscription request must carry a request id")
	// ErrWalletIdentityRequired：这个人没有绑定微信小程序身份，签不了。
	//
	// **不是故障**：他可能只在 H5 登录过。收场是让他先去小程序里登录一次，而这句话说清楚比
	// 「请稍后再试」有用得多。
	ErrWalletIdentityRequired = errors.New("this user has no wechat miniapp identity")
	// ErrChannelUnavailable：渠道那边没答上来（支付服务连不上、超时、应答是坏的）。
	//
	// **它是故障不是结论**，所以是 502 而不是 409：一次超时被读成「没签成」会让用户重新点一遍，
	// 而他手里那份协议其实已经在渠道上建好了。
	//
	// 三条都**直接复用 client 上的值**而不是在这里另立一个再逐处翻译（order-service 的
	// ErrPaymentServiceUnavailable 也是这么接的）：中间多一层映射，就多一处「加了新的失败类
	// 型但忘了在映射里加一条」的位置，而那一处的默认分支会把一次故障说成一个结论。
	ErrChannelUnavailable = client.ErrPaymentUnavailable
	// ErrAgreementConflict：同一把幂等钥匙上的上一笔还在跑。
	//
	// 与前一条的区别是**它是一次结论**：换一把钥匙或者等上一笔跑完。用户看到的应当是「正在处理
	// 中，请稍后再试」，而不是「系统故障」。
	ErrAgreementConflict = client.ErrAgreementConflict
	// ErrAgreementRejected：支付服务不接受这次签约请求（请求不合法，或者这条支付方式压根不是
	// 代扣那一路、这份协议上没有渠道模板 id）。
	//
	// **它故意不在任何分组里**，所以会落到 controller 的默认分支，成为一条 500 与一行日志。
	// 它多半是**我们自己的配置问题**（支付方式没配齐、套餐的签约模板没填），而说得再客气也不会
	// 变成用户能做的事——把它说成「请稍后再试」只会让人去重试一个永远不会成功的东西。
	ErrAgreementRejected = client.ErrAgreementRejected
	// ErrAgreementTerminateRefused：渠道**明确拒绝**了解约（「合同已不存在」这类结论，不是
	// 一次超时）。
	//
	// 它是**结论不是故障**，但也不是用户能修的东西：走到这里时本地一个字节都没改（见
	// terminateForSubscription 的顺序），用户该做的是稍后再试一次，一直不行就找客服；客服手上
	// 的出口是后台的「同步」——它会拿渠道的原话纠正本地这一行。
	//
	// **不能吞掉它继续改本地**：渠道说这份约还在（或者它的话我们读不懂），我们却把开关关掉，
	// 那就是「用户以为关了、下个月照扣」那一格的正面。
	ErrAgreementTerminateRefused = errors.New("the payment channel refused to terminate this agreement")

	// ErrAgreementNotFound：支付服务那边没有这份协议。
	//
	// 本地这一行的 contract_code 是支付服务给的，它说查无此约只有两种可能：**我们这一行被改坏了**，
	// 或者这次请求带的是一个别人的号。两种都不该被当成「没有这件事」悄悄过去。
	ErrAgreementNotFound = client.ErrAgreementNotFound

	// ErrCampaignNotFound：这场店铺码活动不存在（或这个 scene 没有对应的活动）。
	ErrCampaignNotFound = repository.ErrCampaignNotFound
	// ErrCampaignSceneTaken：这个 scene 被别的活动占了。填重了，换一个再存。
	ErrCampaignSceneTaken = repository.ErrCampaignSceneTaken
	// ErrCampaignNotClaimable：这场活动现在领不了（已停用，或者不在起止窗口里）。
	//
	// 与 ErrCampaignNotFound 分开：用户手里的码过期了，页面该说「活动已结束」，而不是「活动
	// 不存在」——它明明存在。
	ErrCampaignNotClaimable = repository.ErrCampaignNotClaimable
	// ErrCampaignAlreadyClaimed：这场活动这个人已经领过了。
	//
	// 正常路径上撞不到它（领取在一个事务里先查了领取记录），它是并发下的兜底。但**它对用户
	// 与重放是同一个结论**：你领过了，结果与第一次相同。
	ErrCampaignAlreadyClaimed = repository.ErrCampaignAlreadyClaimed

	// 活动 id 有一组专门的错误值吗？**没有**：路径上那半个 id 不合法与不存在回同一句话
	// （404 ErrCampaignNotFound），见 campaignID 那段说明。多造两条只会多两条没有调用点的
	// 错误值，而 ValidationErrors 上的每一条都必须有中文说法（response_test 钉着这一点）。
	//
	// ErrCampaignNameRequired：活动名必填。
	ErrCampaignNameRequired = errors.New("campaign name is required")
	// ErrCampaignNameTooLong：活动名超过 60 个字符。
	ErrCampaignNameTooLong = errors.New("campaign name is too long")
	// ErrStoreIDRequired：没选活动门店。
	//
	// 与后台开通（那里门店可留空）分开：活动门店同时是领到的会员的归属门店，一场没有门店的活动
	// 发出去的会员没有归属——那正是这个功能要留下的东西。
	ErrStoreIDRequired = errors.New("campaign store is required")
	// ErrCampaignSceneRequired：scene 必填。
	ErrCampaignSceneRequired = errors.New("campaign scene is required")
	// ErrCampaignSceneInvalid：scene 的格式不对（要么含非法字符，要么超过微信那 32 字节）。
	//
	// 这一条是**替将来把关**：scene 会进小程序码，而码生成走微信的 wxacode.getUnlimited，
	// 那串参数有 32 字节的硬上限，超了是生成失败（一个运营看不懂的报错）。今天没有码，但格式
	// 不对的 scene 一旦存进去就改不掉了（改 scene 会让已经印出去的码失效），所以挡在第一次保存。
	ErrCampaignSceneInvalid = errors.New("campaign scene is invalid")
	// ErrCampaignGiftDaysRange：赠送天数不在 1 到 3650 之间。
	//
	// 上界不是业务规则，是防手滑：多打一位就是送了十年会员。
	ErrCampaignGiftDaysRange = errors.New("campaign gift days is out of range")
	// ErrCampaignWindowInvalid：活动的起止时间不合法（结束不晚于开始）。
	ErrCampaignWindowInvalid = errors.New("campaign end time must be later than its start time")
	// ErrCampaignStatusInvalid：状态值不是 draft / enabled / disabled。
	ErrCampaignStatusInvalid = errors.New("campaign status is invalid")
	// ErrCampaignPlanNotSubscription：这个套餐不是连续包月，不能拿来做店铺码活动。
	//
	// 门槛照抄老系统的「VIP 等级可签约」，在 V2 译作 `membership_plans.auto_renew`——V2 没有
	// VIP 等级这一层，等价物是套餐。**它不是可有可无的洁癖**：活动送出去的是会员天数（不是券），
	// 而 coupon 模式的套餐靠券给会员价、签约这层压根不存在，挑它做活动等于承诺了一个送不出去的
	// 东西。运营该挑 auto 模式的套餐。
	ErrCampaignPlanNotSubscription = errors.New("this plan is not a subscription plan")
	// ErrCampaignLocked：活动正开着，这几个字段不给改，要改先停用。
	//
	// 它**不是请求不合法**（运营填的东西没有错），是「现在这个状态做不了」——所以进 409 那一组，
	// 判据在仓储里（锁着行判，见 UpdateCampaign）。
	ErrCampaignLocked = repository.ErrCampaignLocked
	// ErrCampaignCouponIncomplete：券模板与券张数只填了一个。
	//
	// 两列**同生共死**（库上那条 CHECK 也这么钉着）：只填模板不填张数，等于「该发券」静默
	// 变成「什么都不发」，而用户在小程序上看到的活动写着送券。与套餐上那条
	// member_price_mode='coupon' 必须配齐模板与张数是同一条理由。
	ErrCampaignCouponIncomplete = errors.New("campaign coupon template and count must be provided together")
	// ErrCampaignCouponInvalid：券模板不是一个合法的 id，或券张数不在 1 到 100 之间。
	//
	// **本服务查不出那张模板存不存在**——它在券库，跨库没有外键也没有同步查询（见 007 那段
	// 说明）。这里能管的只有「形状」：形状不对要在保存那一刻就挡住，配错模板则要等发券时由
	// 券服务记日志跳过。
	ErrCampaignCouponInvalid = errors.New("campaign coupon template or count is invalid")
)

// ValidationErrors 是「请求不合法」这一类错误的全集。放在一处而不是在 controller 里逐个
// case：新增一条校验就要在 controller 里同步加一个 case，是必然漏掉的写法。
var ValidationErrors = []error{
	ErrPlanIDRequired, ErrPlanIDInvalid, ErrPlanCodeRequired, ErrPlanCodeInvalid,
	ErrPlanNameRequired, ErrPlanPriceNegative, ErrPlanPriceTooLarge,
	ErrPlanPeriodInvalid, ErrPlanPeriodCountRange,
	ErrPlanWechatIDRequired, ErrPlanWechatIDNotAllowed,
	ErrPlanPriceModeInvalid, ErrPlanCouponFieldsRequired, ErrPlanCouponFieldsNotAllowed,
	ErrPlanCouponsPerPeriodRange, ErrPlanStatusInvalid,
	ErrPlanBenefitsTooMany, ErrPlanBenefitTooLong,
	ErrMembershipIDRequired, ErrMembershipIDInvalid, ErrUserIDRequired, ErrUserIDInvalid,
	ErrFreezeReasonRequired, ErrUnfreezeReasonRequired, ErrRevokeReasonRequired,
	ErrAdjustReasonRequired, ErrAdjustExpireRequired, ErrAdjustRemarkTooLong,
	ErrOccurredRangeInvalid, ErrMembershipStatusInvalid, ErrReasonTooLong,
	ErrGrantReasonRequired, ErrGrantRequestIDRequired, ErrStoreIDInvalid,
	ErrStoreNotFound, ErrGrantExpireTooEarly,
	ErrSubscriptionStatusInvalid, ErrCancelReasonRequired,
	ErrCampaignNameRequired, ErrCampaignNameTooLong, ErrStoreIDRequired,
	ErrCampaignSceneRequired, ErrCampaignSceneInvalid, ErrCampaignGiftDaysRange,
	ErrCampaignWindowInvalid, ErrCampaignStatusInvalid, ErrCampaignPlanNotSubscription,
	ErrCampaignCouponIncomplete, ErrCampaignCouponInvalid,
	ErrSubscriptionRequestIDRequired, ErrSubscriptionPlanNotSubscription,
}

// IsValidationError 判断一个错误是不是「请求不合法」。
func IsValidationError(err error) bool {
	for _, candidate := range ValidationErrors {
		if errors.Is(err, candidate) {
			return true
		}
	}
	return false
}

const (
	// MaxReasonLength 是冻结 / 撤销 / 调整理由的长度上限，按**字符**算（不是字节）。
	//
	// 与 lottery-service、payment-service 同一个取值、同一条道理：中文一个字符三字节，
	// 按字节限 200 会让运营在写了一百来个字的时候被莫名其妙地拒绝。
	MaxReasonLength = 200
	// MaxRemarkLength 是后台备注的上限。
	//
	// 比理由宽松：备注是给自己人看的补充说明（工单号、聊天记录摘要），不是给用户看的结论。
	MaxRemarkLength = 500
	// MaxBenefitLines / MaxBenefitLength 是权益文案的条数与单条长度上限。
	//
	// 原型上就是一行「每月 20 次咖啡享会员价」。给到 20 行是留足余量，不是鼓励写满。
	MaxBenefitLines  = 20
	MaxBenefitLength = 100
	// maxPlanPriceCents 是套餐价格的上界（一百万元）。
	//
	// 它不防「有人真的想卖一百万」——它防**粘错**：价格是一串手输的数字，多打四位就会变成一笔
	// 荒谬的订单，而支付渠道那一侧会直接拒掉，用户看到的是「支付失败」而不是「价格填错了」。
	maxPlanPriceCents = 100_000_000
	// MaxPageSize / DefaultPageSize 与 dto 上的同名常量共用取值，见那里的说明。
	MaxPageSize     = dto.MaxPageSize
	DefaultPageSize = dto.DefaultPageSize

	// MaxCampaignNameLength 是活动名的长度上限，按**字符**算。
	//
	// 比 MaxBenefitLength 松一点：活动名是运营写给运营看的（「五一·望京店开业」），但也不该
	// 是一段话——它要挤进列表的一列里。
	MaxCampaignNameLength = 60
	// MaxCampaignGiftDays 是赠送天数的上界（十年）。
	MaxCampaignGiftDays = 3650
	// MaxCampaignCouponCount 是店铺码活动每次领取送的券张数上限。
	//
	// 100 是**券服务那边的既有上限**（`POST /v1/admin/coupons/issue` 的 quantityPerUser 校验
	// 就是 1..100）。两边取同一个数不是巧合：活动券走的是同一个发券实现，这边放行一个那边
	// 不收的张数，用户就会领到会员却一张券都没有——而那时候已经没人能回头改了。
	MaxCampaignCouponCount = 100
	// maxCampaignSceneBytes 是小程序码 scene 的长度上限，**按字节**算。
	//
	// 32 是微信 wxacode.getUnlimited 的硬上限，不是我们挑的数——所以这里必须按字节而不是字符，
	// 与同文件里那些按字符算的上限是**两种规矩**，别照抄。
	maxCampaignSceneBytes = 32
)

// campaignScenePattern 是 scene 的格式：`smc_` 前缀 + 字母数字下划线连字符。
//
// 逐字沿用老系统的正则（`^smc_[A-Za-z0-9_-]+$`），包括那个前缀。前缀是给码本身用的：微信开发者
// 工具里一眼能把店铺码的 scene 与别的业务参数分开看。改变它会让老码与新码认不出一套来。
var campaignScenePattern = regexp.MustCompile(`^smc_[A-Za-z0-9_-]+$`)

// StoreResolver 回答两个关于商户域门店的问题：这家店**存在吗**、这几家店**叫什么**。
//
// 与 lottery-service 的 client.StoreClient 是同两个方法、同一套语义（那是这个仓库里最近的
// 同形先例），定义成接口是为了让这一层能脱离 gRPC 测——门店不存在与商户服务连不上是两条
// 必须分开的分支，而它们的区别只有在能替换实现时才测得出来。
type StoreResolver interface {
	// Exists：「不存在」是 (false, nil)，「问不到」是 (false, err)，**不合并**。
	Exists(ctx context.Context, storeID string) (bool, error)
	// Names：一次解一页，解不出来的 id 从返回的 map 里缺席（调用方按空串处理）。
	Names(ctx context.Context, storeIDs []string) (map[string]string, error)
}

// AgreementGateway 是协议这条路上本服务要支付域做的四件事：签约、查约、扣款、解约。
//
// 与 StoreResolver 同一个位置、同一条理由：这条路上最容易错的那部分（重放的是不是同一次点击、
// 什么状态才该改本地、渠道没答上来时能不能动、受理与扣到钱的区别）必须能脱离 gRPC 测——而
// 「没答上来」与「答了，是不行」这两条分支的区别，只有在能替换实现时才测得出来。
//
// **它不代表用户**：这一次调用代表会员域去告诉支付域一个事实。用户是谁由令牌解出来的 userID
// 决定，openid 来自身份域，两者都不是客户端在请求体里能声称的。
type AgreementGateway interface {
	// Create 发起一次签约，拿回给客户端的跳转参数。**它不等到签约完成**（返回的 status 恒为
	// pending）：用户拿参数去微信里点「同意」才算签成。
	Create(ctx context.Context, in dto.CreateAgreementParams) (*dto.AgreementSigningResult, error)
	// Query 回渠道核一份协议，返回**纠正之后**的状态——名字像查询，做的事是写（支付库那边会
	// 按渠道的原话改自己那一行）。
	Query(ctx context.Context, agreementNo, requestID string) (*dto.AgreementState, error)
	// Charge 发起一期扣款。**返回的不是「扣到钱了没有」**：受理只说明请求被收下了（status 为
	// charging），钱的结论在 payment.agreement.charge_succeeded / _failed 两条事件里。
	Charge(ctx context.Context, in dto.ChargeAgreementParams) (*dto.AgreementChargeResult, error)
	// Terminate 解一份协议。渠道明确拒绝**不是 error**（走结果的 FailureCode），只有「没问出
	// 结论」才是 error——这个区别是「先解约、成功后才改本地」那条顺序的依据。
	Terminate(ctx context.Context, in dto.TerminateAgreementParams) (*dto.AgreementTermination, error)
}

// WalletOpenIDReader 取用户绑在微信小程序下的身份。
//
// 与 AgreementGateway 分开成两个接口，是因为它问的是**另一个服务**（身份域）：测签约那条链时
// 想替换的往往只有一个，合并成一个接口会让每一次替换都要把另一半也写一遍。
type WalletOpenIDReader interface {
	// MiniappOpenID：「这个人没绑小程序」是 ("", false, nil)，「问不到」是 err。**不合并**：
	// 前者要告诉用户先去小程序登录一次，后者要让他稍后再试。
	MiniappOpenID(ctx context.Context, userID string) (string, bool, error)
}

// OrderGateway 是本服务要订单域做的两件事：把一期续费记成一张订单，以及回读一张订单的摘要。
//
// 与 AgreementGateway 分开成两个接口，是因为它问的是**另一个域**：测扣款那条链时想替换的往往
// 只有一个，合并会让每一次替换都要把另一半也写一遍。
//
// 它与 AgreementGateway 有一处**结构上的不同**，值得写下来：AgreementGateway 的四个动作全是
// 写（连名字像读的 Query 也是写），而这里的 GetOrder 是真的读——它服务的是后台一个页面，不是
// 任何判定。所以它在服务层的失败处置也不同：建单失败要**让整条消息重投**（钱收了、账记不上），
// 而读订单失败只让那一块降级（见 subscriptionDetail）。
type OrderGateway interface {
	// CreateRenewal 把一期扣款记成一张订单。**幂等键是渠道流水号**，所以调用方在拿不准时原样
	// 重发是安全的——这正是它必须排在结算之前的前提（见 recordRenewalOrder）。
	CreateRenewal(ctx context.Context, in dto.RenewalOrderParams) (*dto.RenewalOrder, error)
	// GetOrder 按订单 id 读一张订单的摘要。查无此单回 ErrOrderNotFound——那是一次结论，不是
	// 「问不到」。
	GetOrder(ctx context.Context, orderID string) (*dto.OrderSummary, error)
}

// ChargeReader 是后台「包月订阅」详情要的两块**支付域**的读：这一份协议的扣款期次，以及一张
// 支付单的摘要。
//
// # 为什么由本服务代转，而不让后台直连支付域
//
// 因为那一页的权限码是 membership:read：它读的是「谁签了连续包月、这一期扣了没」，而支付域的
// 后台读接口要求 payment:read（见 migrations/identity/033 的说明）。让前端直连就等于把一次
// 「看订阅」变成一次「看支付数据」，运营得同时拿到两个权限。所以这两块经本服务出去。
//
// # 两个方法都是纯读，而且都**不允许**把整页打成 500
//
// 调用方是 subscriptionDetail，它对这两个方法的失败一律降级（那一块空着并记日志）。理由很实在：
// 支付服务抖一下不该让运营打不开订阅详情——他要看的「谁签的、下一次什么时候扣」在本库里，
// 一个字都没毛病。
type ChargeReader interface {
	// ListAgreementCharges 读一份协议下的全部期次，按期次升序。协议不存在回
	// ErrAgreementNotFound，协议存在但没有期次回空列表。
	ListAgreementCharges(ctx context.Context, agreementNo string) ([]dto.AgreementCharge, error)
	// GetPayment 读一张支付单的摘要（后台要的是里面的渠道流水号）。支付单号为空是请求不合法，
	// 查无此单回 ErrPaymentNotFound。
	GetPayment(ctx context.Context, paymentNo string) (*dto.PaymentSummary, error)
}

// Options 是构造 MembershipService 的可调项。零值等于用默认值。
type Options struct {
	// Now 为 nil 时用 time.Now。
	//
	// 它是**唯一**的「现在」来源：entitlement 判定、到期扫描的判断、请求体里日期的完整性
	// 校验都取它。各处自己调一次 time.Now()，迟早会差出一个让人解释不了的时间戳。
	Now func() time.Time
	// Stores 为 nil 时归属门店的**校验与解析都退化成空操作**：开通时不再问门店存不存在，
	// 列表里也不显示门店名。这条退化路径是给单测用的——生产装配必须传（见 cmd/main.go）。
	Stores StoreResolver
	// Agreements 为 nil 时**签约那条路整条不可用**（回 ErrChannelUnavailable）。与上面两个不同，
	// 这里没有「退化成空操作」这种中间态可退：签约不在渠道上建一份协议就是没签，所以宁可直接
	// 说「渠道不可用」，也不能在没接渠道的进程里假装签成了。生产装配必须传。
	Agreements AgreementGateway
	// Wallets 为 nil 时同上：签约要素（openid）拿不到就不该往下走。
	Wallets WalletOpenIDReader
	// Orders 为 nil 时**扣款成功那条路会失败**（回 ErrOrderUnavailable）：钱已经收了，而这一期
	// 记不成账——返回错误让消息重投，比静默地少一张订单好。与 Agreements 同一条：这里没有
	// 「退化成空操作」的中间态可退，生产装配必须传。
	Orders OrderGateway
	// Charges 为 nil 时后台订阅详情那两块**降级成空**（不报错）。它与上面几个不同：这是一条
	// 只读的展示路径，没接支付域时那一页其余部分照常能看——让运营因为一个附带信息块打不开
	// 「谁签了连续包月」是不划算的。
	Charges ChargeReader
}

// MembershipService 是会员业务层。
type MembershipService struct {
	repository Repository
	now        func() time.Time
	stores     StoreResolver
	agreements AgreementGateway
	wallets    WalletOpenIDReader
	orders     OrderGateway
	charges    ChargeReader
}

// New 构造业务层。
func New(r Repository, options Options) *MembershipService {
	if options.Now == nil {
		options.Now = time.Now
	}
	return &MembershipService{
		repository: r,
		now:        options.Now,
		stores:     options.Stores,
		agreements: options.Agreements,
		wallets:    options.Wallets,
		orders:     options.Orders,
		charges:    options.Charges,
	}
}

// —— 共用的小工具 ——

// requiredID 校验一个必填的 uuid 参数，空串与「不是 uuid」用**调用方给的那一对**错误报出去。
//
// 两者必须分开：前者是漏传（「请选择套餐」），后者是传错了东西（「套餐 ID 不是合法的
// UUID」）。合成一条的话，前端那个下拉框没选东西时会收到「不是合法的 UUID」，而它其实是
// 空的——用户会去检查一个根本没填过的字段。
func requiredID(value string, required, invalid error) (string, error) {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return "", required
	}
	if _, err := uuid.Parse(trimmed); err != nil {
		return "", invalid
	}
	return trimmed, nil
}

// optionalUUID 校验一个可选的 uuid 参数，空串原样回空串（= 不筛这一项）。
func optionalUUID(value string, invalid error) (string, error) {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return "", nil
	}
	if _, err := uuid.Parse(trimmed); err != nil {
		return "", invalid
	}
	return trimmed, nil
}

// parseEnum 校验一个枚举字段，空串取默认值。
//
// 空串取默认而不是报错：这些字段在库上都有 DEFAULT，前端不传时本来就该落到那个默认值上。
// 传了一个不认识的取值才是错——那时候不报，用户会得到一个「保存成功了但设置没生效」。
func parseEnum(value string, allowed []string, fallback string) (string, error) {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return fallback, nil
	}
	for _, candidate := range allowed {
		if trimmed == candidate {
			return trimmed, nil
		}
	}
	return "", ErrPlanStatusInvalid
}

// checkReason 校验一条必填的理由。
//
// required 由调用方给（冻结 / 撤销 / 调整各一条，人话表按它索引「请填写原因」那句）。
// 太长了共用 ErrReasonTooLong：三种理由给用户看的话不一样，但「太长了」是同一件事。
func checkReason(value string, required error) (string, error) {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return "", required
	}
	if len([]rune(trimmed)) > MaxReasonLength {
		return "", ErrReasonTooLong
	}
	return trimmed, nil
}
