// Package service 是支付的业务层：校验、生成单号、编排事务、驱动状态机、分派渠道适配器。
//
// 按业务动作拆文件：create.go 发起支付，callback.go 认渠道回调，state.go 状态机，
// expire.go 超时关单。仓储只负责「一个事务里把这几个事实写进去」，「几个事实分别是什么」
// 由这里决定。
package service

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/panda-dev/panda-v2/backend/services/payment-service/internal/catalog"
	"github.com/panda-dev/panda-v2/backend/services/payment-service/internal/client"
	"github.com/panda-dev/panda-v2/backend/services/payment-service/internal/model"
	"github.com/panda-dev/panda-v2/backend/services/payment-service/internal/provider"
	"github.com/panda-dev/panda-v2/backend/services/payment-service/internal/repository"
)

// Repository 是支付库的数据访问契约。
//
// 定义成接口而不是直接用 *repository.PostgresRepository，是为了让 service 的校验与
// 状态机能在没有数据库的情况下测：这一层里最容易出错的恰恰是不碰数据库的那部分
// （金额该不该建单、这个 action 能不能起支付、密钥读不到时该不该放行），而那些逻辑
// 不该为了测它去起一个 PG。
type Repository interface {
	// —— 读 ——
	//
	// 这里**没有** FindPaymentMethod / FindChannelByCode：支付方式与渠道是代码里的常量
	// （见 internal/catalog），不是表。发起支付与认回调两条路都不再查库来回答「这是哪一种
	// 支付方式、它挂在哪条渠道上」。
	FindPaymentByNo(ctx context.Context, paymentNo string) (*model.Payment, error)
	// FindSettlementRule 按范围精度命中一条启用中的分账规则（没命中返回 nil, nil）。
	// 它是发起支付那条路上的读：任务在那一刻建，规则也在那一刻冻结（见 buildSettlementPlan）。
	FindSettlementRule(ctx context.Context, q repository.SettlementRuleQuery) (*repository.SettlementRule, error)

	// —— 发起支付的三段事务（见 create.go 的流程图）——
	BeginPayment(ctx context.Context, p repository.BeginPaymentParams) (*model.Payment, []byte, bool, error)
	// AbandonPaymentAttempt 把一次没走完的发起尝试退回去，让同一个幂等键能立刻重开
	// （为什么、以及三条约束怎么满足，见 repository 那份实现的长注释）。
	//
	// 它**必须**跟 BeginPayment 成对出现：BeginPayment 占了幂等键，之后每一条失败出口都要
	// 由它把键放开，否则那条幂等行会白占 IdempotencyRecoveryWindow 那么久。
	AbandonPaymentAttempt(ctx context.Context, requestID, paymentID string) error
	MarkPaymentPending(ctx context.Context, p repository.MarkPaymentPendingParams) (*model.Payment, error)
	MarkPaymentFailed(ctx context.Context, p repository.MarkPaymentFailedParams) (*model.Payment, error)
	// SettleAccountPayment 是账户出资那条路的终止事务（见 createAccountPayment）。它与
	// MarkPaymentPending 是**平级的两条路**，不是同一段事务的两种写法：渠道支付会停在
	// pending，账户出资不会。
	SettleAccountPayment(ctx context.Context, p repository.SettleAccountPaymentParams) (*model.Payment, error)
	// RecordAccountDeduction 把「账户域已经扣了这笔豆」登记到支付单上。它**先于结算**，
	// 而且刻意不跟结算共用一个事务：要覆盖的正是结算失败的那个窗口（见 payments.account_entry_id 的列注释）。
	RecordAccountDeduction(ctx context.Context, paymentID, accountEntryID string, fundedAt time.Time) error
	RecordProviderCall(ctx context.Context, p repository.ProviderCallParams) error

	// —— 退款（见 refund.go 的流程图）——
	//
	// 它比发起支付少一段「抢占幂等键」：payment_refunds.after_sale_no 整表唯一，退款单自己
	// 就是那把钥匙，所以这里没有 Begin/Abandon 那一对，只有下面三段。
	//
	// 三个 Mark* 都是在**锁内重判状态**的 CAS（`WHERE status IN (...)`），并发下只有先到
	// 的那一次算数。它们与 MarkPaymentPending / MarkPaymentFailed 是同一种形状，不共用实现
	// 是因为它们改的是另一张表、另一组状态。
	BeginRefund(ctx context.Context, p repository.BeginRefundParams) (*model.Refund, []*model.RefundFunding, bool, error)
	MarkRefundSucceeded(ctx context.Context, p repository.MarkRefundSucceededParams) (*model.Refund, error)
	MarkRefundFailed(ctx context.Context, p repository.MarkRefundFailedParams) (*model.Refund, error)
	MarkRefundProcessing(ctx context.Context, p repository.MarkRefundProcessingParams) (*model.Refund, error)
	FindRefundByNo(ctx context.Context, refundNo string) (*model.Refund, []*model.RefundFunding, error)

	// —— 签约协议（见 agreement.go）——
	//
	// 三个方法而不是「读 + 通用更新」：协议的每一次状态变化都带着判定（终态不复活、已生效
	// 不重写），而判定必须发生在**锁内**——把它放在 service 里就是一次「读出来判断再写回去」
	// 的竞态，两个并发的同步会各自读到同一个旧状态。
	CreateAgreement(ctx context.Context, p repository.CreateAgreementParams) (*model.PaymentAgreement, []byte, bool, error)
	FindAgreementByNo(ctx context.Context, agreementNo string) (*model.PaymentAgreement, error)
	// SettleAgreement 把渠道查回来的结论落到协议上，返回是否真的改了（见它自己的判据）。
	SettleAgreement(ctx context.Context, p repository.SettleAgreementParams) (*model.PaymentAgreement, bool, error)
	// SettleAgreementNotification 与上一条**同源不同入口**：上一条是我们主动去问渠道，这一条是
	// 渠道主动来告诉我们（见 agreement_notify.go）。两条最后都走 applyAgreementTarget 那套
	// 判定，所以它们只差「谁把结论送进来」——这也是为什么它不叫 SettleAgreement 的第二个参数。
	SettleAgreementNotification(ctx context.Context, p repository.AgreementNotificationParams) (*repository.AgreementSettlement, error)

	// —— 代扣（见 agreement_charge.go）——
	//
	// 与上面三个是同一种形状，分开的是**聚合**：一期扣款的成败与协议的生死是两件事
	// （见 model.AggregateCharge），所以它们改的是 payment_agreement_charges 那一张表。
	//
	// CreateOrFindCharge 的第二个返回值是「这次是新建的」。它**不是**「该不该扣」的判据
	// ——那是 service 那张状态分流表的事，这一层只保证「同一期只有一行」。
	CreateOrFindCharge(ctx context.Context, p repository.CreateOrFindChargeParams) (*model.PaymentAgreementCharge, bool, error)
	// MarkChargeAttempt 记一次尝试（受理或当场被拒），并给确定失败的那一次发 charge_failed。
	MarkChargeAttempt(ctx context.Context, p repository.ChargeAttemptParams) (*model.PaymentAgreementCharge, error)
	// SettleChargeNotification 是扣款结果通知的落点，与 SettleAgreementNotification 同源不同入口。
	SettleChargeNotification(ctx context.Context, p repository.ChargeNotificationParams) (*repository.ChargeSettlement, error)
	// ListChargesByAgreementNo 读一份协议下的全部期次（订阅详情那一页要的）。
	//
	// 它**在**这个接口里而不是像后台那几个读一样单开一个（见 admin_query.go）：它与上面三个
	// 写方法读的是同一张表、同一个聚合，而「这一期现在怎样」本来就是发起扣款那条路要回答的
	// 问题。后台那几个单开，是因为它们是另一个消费者的另一张页面，与这一条不是同一种东西。
	ListChargesByAgreementNo(ctx context.Context, agreementNo string) ([]*model.PaymentAgreementCharge, error)

	// —— 回调 ——
	InsertNotification(ctx context.Context, p repository.NotificationParams) (*repository.NotificationRecord, error)
	MarkNotification(ctx context.Context, notificationID, status, reason string) error
	SettlePayment(ctx context.Context, p repository.SettleNotificationParams) (*repository.PaymentSettlement, error)

	// —— 补偿任务 ——
	//
	// 主动查单：找出发起之后一直没结论的支付单，逐个向渠道问一次（见 reconcile.go）。
	// 它读的是支付单自己那几行，不需要回调记录——**这一条路上没有回调**。
	ListStalePendingPayments(ctx context.Context, staleBefore time.Time, limit int) ([]model.Payment, error)
	ExpireOverduePayments(ctx context.Context, limit int) (int, error)
	// FindOverdueAccountFundedPayments 找出「豆已经扣了、到点还没结算」的支付单，
	// 交给 SettleOverdueAccountPayments 结算（见 expire.go）。
	FindOverdueAccountFundedPayments(ctx context.Context, limit int) ([]model.Payment, error)
	// ListStaleProcessingRefunds 找出「向渠道发起之后一直没结论」的退款单（见 refund.go）。
	ListStaleProcessingRefunds(ctx context.Context, staleBefore time.Time, limit int) ([]model.Refund, error)
	// TouchRefund 把一条退款单的 updated_at 推到此刻，用于「问了一轮、还没有结论」。
	TouchRefund(ctx context.Context, refundID string) error
}

// ProviderRegistry 按渠道的 provider 名字查适配器。
//
// 它是个接口而不是直接用 *provider.Registry，理由同 Repository：装配处传真的，
// 测试传一个只有 manual 的注册表。
type ProviderRegistry interface {
	Lookup(name string) (provider.Provider, error)
}

// BeanLedger 是账户域咖啡豆账本的写入口（account-service 的 DeductCoffeeBeans）。
//
// 它是个接口而不是直接收一个 *client.CoffeeBeanClient，理由同 ProviderRegistry：装配处传
// 真的，测试传一个能摆出「余额不足」的假的。**这个接口只有一个方法**——支付域对账户域
// 只有这一个动作，冲正走的是事件（订单域把退款结果转成 order.after_sale.refunded →
// account-service 自己消费），
// 不从这里调。多留一个今天没有调用方的方法，等于让「谁在什么时候冲正」有两处说法。
type BeanLedger interface {
	Deduct(ctx context.Context, req client.DeductRequest) (client.DeductResult, error)
}

// SecretResolver 按渠道 + 槽名解析出一把凭据的明文。
//
// 它是个函数而不是接口：解析就是「拿一个键名换一个值」这一件事，没有状态也没有别的
// 实现维度。返回空串表示读不到——**调用方必须把它当成拒绝的理由，不是默认值**。
//
// 收整个 catalog.Channel 而不是直接收环境变量名，是因为「槽名到变量名的对照表」住在
// Channel.SecretEnv 里（见 catalog）：那是一份**部署决定**，不该由 service 或适配器
// 各自猜一遍。装配处那份实现只做两件事——按槽名查变量名、os.Getenv——所以密钥明文从
// 头到尾不落在任何结构体上，只在这一层与调用栈上活一次。
type SecretResolver func(channel *catalog.Channel, slot string) string

// 请求本身不合法（controller / rpc 统一回 InvalidArgument）。
var (
	ErrOrderNoRequired     = errors.New("orderNo is required")
	ErrOrderIDRequired     = errors.New("orderId is required")
	ErrOrderIDInvalid      = errors.New("orderId must be a uuid")
	ErrUserIDRequired      = errors.New("userId is required")
	ErrUserIDInvalid       = errors.New("userId must be a uuid")
	ErrAmountNotPositive   = errors.New("amount must be positive")
	ErrMethodRequired      = errors.New("paymentMethod is required")
	ErrRequestIDRequired   = errors.New("requestId is required")
	ErrChannelCodeRequired = errors.New("channel code is required")
	ErrNotificationEmpty   = errors.New("notification body is empty")
	// ErrBizTypeInvalid：bizType 不在 settlement_rules.biz_type 的词表里。
	//
	// 它是这条链路上唯一值得在入口拦下的分账维度：biz_type 是规则命中键的第一段，写错的
	// 表现是「安静地命不中规则、整单归平台」——没有报错、没有异常，只是钱分错了地方。
	// store/device 不校验：它们落的是快照列，写错只影响命中，不会失真（值来源是订单库）。
	ErrBizTypeInvalid = errors.New("bizType is not a known settlement business type")
)

// 状态与配置（controller 各自映射成 404/409/501/503）。
var (
	// ErrPaymentNotFound：支付单不存在。回调指着一个我们没有的单。
	ErrPaymentNotFound = repository.ErrPaymentNotFound
	// ErrChannelNotFound：回调 URL 里那一段不在目录里。**不是 404 给渠道看的那种「没有」**
	// ——它是我们自己收到的 URL 不对，但对回调方一律回失败。
	ErrChannelNotFound = catalog.ErrChannelNotFound
	// ErrPaymentMethodNotFound：调用方给了一个目录里没有的支付方式 code。
	//
	// 它与「这个方式被停用了」不再是一对：停用是运营在后台改一行数据，而那两页已经没了。
	// 今天只可能是调用方传了一个我们不认识的 code（版本回滚、或者客户端拼错了）。
	ErrPaymentMethodNotFound = catalog.ErrMethodNotFound
	// ErrChannelIncomplete：渠道在目录里，但这次部署没配齐（缺的是哪几个环境变量写在
	// catalog.Channel.MissingEnv 里）。它是**部署问题**，不是调用方的错。
	ErrChannelIncomplete = catalog.ErrChannelIncomplete
	// ErrIdempotencyInProgress：同一个幂等键的发起还在处理中，让调用方稍后重试。
	ErrIdempotencyInProgress = repository.ErrIdempotencyInProgress
	// ErrIdempotencyConflict：同一个幂等键换了请求体，重试也没用。
	ErrIdempotencyConflict = repository.ErrIdempotencyConflict
	// ErrPaymentNotPending：支付单已经不在能推进的状态（成功过了、关掉了）。
	ErrPaymentNotPending = repository.ErrPaymentNotPending
	// ErrProviderNotConfigured：渠道行指向一个没注册的适配器实现。
	ErrProviderNotConfigured = provider.ErrProviderNotConfigured
	// ErrSecretNotConfigured：渠道的密钥读不到。回调路径上遇到它就是拒签。
	ErrSecretNotConfigured = provider.ErrSecretNotConfigured
	// ErrSignatureMismatch：回调验签不通过。
	ErrSignatureMismatch = provider.ErrSignatureMismatch
	// ErrInvalidNotification：验签过了但报文缺必需字段。
	ErrInvalidNotification = provider.ErrInvalidNotification
	// ErrNotificationNotActionable：验签过了，但报文说的那件事不该由这条入口处理。
	// 与上一条一样，落进 payment_notifications 时都记「签名验过了」（见 signatureVerified）。
	ErrNotificationNotActionable = provider.ErrNotificationNotActionable
	// ErrUnauthorizedAction：调用方宣称了一个不属于它的支付方式（渠道没有这个 action
	// 能起的支付）。
	ErrUnauthorizedAction = errors.New("payment method action is not supported")
	// ErrInsufficientCoffeeBeans：账户出资时用户的豆不够。**这不是故障**，是一次与
	// 「渠道拒绝」同级的业务结论——它落成 status='failed' 的结果，不是返回给调用方的 error。
	// 判据（哪个 gRPC 码算余额不足）在 internal/client 那边，业务层只认这个名字。
	ErrInsufficientCoffeeBeans = client.ErrInsufficientBeans

	// —— 退款那条路的（见 refund.go）——
	//
	// 下面这四个都是**再导出**而不是新造：判定它们的是仓储与适配器，而 rpc 那一层只该认
	// service 这一个包。多写一层转接，是为了让「错误从哪来」在 grep 时只有一处答案。
	//
	// ErrPaymentNotRefundable：这张支付单不在能退的状态（还没收妥、已经失败、已经关掉）。
	ErrPaymentNotRefundable = repository.ErrPaymentNotRefundable
	// ErrRefundExceedsRefundable：这次要退的钱超过了这张单**剩下的**可退额。
	ErrRefundExceedsRefundable = repository.ErrRefundExceedsRefundable
	// ErrRefundNotAdvanceable：退款单已经被另一个入口推走了（退款查询 worker 快过调用方）。
	ErrRefundNotAdvanceable = repository.ErrRefundNotAdvanceable
	// ErrProviderOperationUnsupported：这条渠道的适配器没有实现这次要做的操作。
	//
	// 它与「渠道没配齐」分开：那条说的是**同一家渠道**这次部署少填了环境变量（改配置就好），
	// 这条说的是这**一族协议**根本没有这个能力（换渠道才行）。
	ErrProviderOperationUnsupported = provider.ErrOperationNotSupported

	// —— 签约那条路的（见 agreement.go）——
	//
	// ErrAgreementNotFound：我们库里没有这份协议。签约通知、后台同步、发起确认三条路都会
	// 撞上它，而它们对「查无此约」的处置是一样的——要么是我们收错了报文，要么是调用方给了
	// 别人的号。与 ErrPaymentNotFound 分开是必须的：**两者都是 404，但说的是不同的东西**，
	// 而调用方（membership-service）要按它决定是「这单没了」还是「这份授权没了」。
	ErrAgreementNotFound = repository.ErrAgreementNotFound
)

// RetryableErrors 是「重试可能成功」的那一组。controller 用 503、rpc 用 Unavailable，
// 而不是 400：调用方拿到它应当退避重试，不是去改请求。
var RetryableErrors = []error{
	ErrIdempotencyInProgress,
}

// ValidationErrors 是「请求不合法」这一类错误的全集。放在一处而不是在 controller 里
// 逐个 case：新增一条校验就要在 controller 里同步加一个 case，是必然漏掉的写法。
//
// 分账后台那一批（SettlementValidationErrors）在最后并进来：它们单独列一份只是因为
// controller 的人话表要拿那一份**逐条**核对（支付那几条走别的出口），判定本身只有这一处。
var ValidationErrors = append([]error{
	ErrOrderNoRequired, ErrOrderIDRequired, ErrOrderIDInvalid, ErrUserIDRequired, ErrUserIDInvalid,
	ErrAmountNotPositive, ErrMethodRequired, ErrRequestIDRequired,
	ErrChannelCodeRequired, ErrNotificationEmpty, ErrBizTypeInvalid,
	// 退款那条路的（见 refund.go）。它们与上面那一组共用这个判定，是因为「调用方传错了」
	// 这件事在两端的处置是一样的：gRPC 一律 InvalidArgument，控制器一律 400。
	ErrAfterSaleNoRequired, ErrPaymentNoRequired, ErrRefundAmountNotPositive, ErrOrderLineIDInvalid,
	// 签约那条路（见 agreement.go）。
	ErrProviderPlanIDRequired, ErrWalletOpenIDRequired, ErrMaxChargeAmountInvalid, ErrAgreementNoRequired,
	// 代扣那条路（见 agreement_charge.go）。期次缺了就没有任何东西挡得住同一期被扣两次，
	// 金额不是正数在库上还有一条 CHECK 兜着——两个都该在入口断成一句能读的话。
	ErrBizPeriodRequired, ErrChargeAmountNotPositive,
}, SettlementValidationErrors...)

// IsValidationError 判断一个错误是不是「请求不合法」。
func IsValidationError(err error) bool {
	for _, candidate := range ValidationErrors {
		if errors.Is(err, candidate) {
			return true
		}
	}
	return false
}

// IsRetryable 判断一个错误是不是「稍后重试可能成功」。
func IsRetryable(err error) bool {
	for _, candidate := range RetryableErrors {
		if errors.Is(err, candidate) {
			return true
		}
	}
	return false
}

// DefaultPaymentTTL 是支付单的存活时长。
//
// 它必须**不短于** order-service 的 DefaultPaymentTTL（同样是 15 分钟）。两边不一致会
// 出现「订单已经超时关掉，支付单还开着，用户把钱付了」的窗口——那笔钱收进来了却没有单
// 可挂。同值是最省事且安全的取法：两个 worker 各自扫各自的表，谁先扫到都不出问题。
//
// 关单不涉及钱（预支付单会自然过期），所以两边独立、不求同步，只求支付这边不更短。
const DefaultPaymentTTL = 15 * time.Minute

// Options 是构造 PaymentService 的可调项。零值等于用默认值，便于 main 里只写关心的那项。
type Options struct {
	// PaymentTTL 为 0 时用 DefaultPaymentTTL。
	PaymentTTL time.Duration
	// Now 为 nil 时用 time.Now。注入它才能测「过期」与单号里的日期而不必真的等。
	Now func() time.Time
	// NewPaymentNo 为 nil 时用顺序号 + 随机后缀的实现（见 paymentNo）。
	NewPaymentNo func(now time.Time) string
	// NewRefundNo 为 nil 时用 refundNo（REF + 时间戳 + 随机数，**共 23 位**）。
	//
	// 它与 NewPaymentNo 一样是可注入的，唯一的原因是测试要能钉住那个长度：退款单号会被编成
	// 渠道侧的 refundOrderId 发出去，而规范给的总长上限是 28 位（含 4 位来源编号）。这一条
	// 靠一个纯函数测不准——真出问题的地方是「有人往单号里加了一段」，而那正是这个函数。
	NewRefundNo func(now time.Time) string
	// NewAgreementNo 为 nil 时用 agreementNo（AGR + 时间戳 + 随机数，**共 23 位**）。
	//
	// 它可注入的理由与 NewRefundNo 一模一样：这个号会被当作渠道侧的 contract_code 发出去，
	// 而渠道对它有长度与字符集上限（微信 32 位以内的字母数字）。23 位这个数靠一个纯函数
	// 测不准——真出问题的地方是「有人往单号里加了一段」，而那正是这个函数。
	NewAgreementNo func(now time.Time) string
	// NotifyBaseURL 是渠道回调地址的前缀，拼成 `<base>/v1/payments/callback/<渠道码>`。
	//
	// 它不进数据库配置：同一个渠道在 dev / staging / prod 的域名不同，写进
	// payment_channels.config 就得多一套配置，而且改域名要改数据。它跟着部署走。
	NotifyBaseURL string
	// ReturnPageURL 是用户在渠道收银台付完之后，我们把他**再送一程**的那个地址。
	//
	// # 它为什么不从渠道配置读
	//
	// 方案原文写的是「302 到渠道 config 里配的结果页（`return.resultUrl`）」。这里没照办，
	// 因为那会破坏一条既有边界：**service 从不解释渠道 config 的键**（见 create.go 里
	// 「槽名问适配器」那一段）。config 长什么样是适配器独占的知识，service 里写一个
	// `config["return"]["resultUrl"]` 就是「只有 ums 认这个键」的分支——别的协议族进来时
	// 会安静地没有结果页，而那种缺法看不出是漏配还是不支持。
	//
	// 换到部署级的第二层理由与 NotifyBaseURL 逐字相同：结果页在 dev / staging / prod 是
	// 三个不同的地址，而渠道行是**数据**，跟着环境走的东西写进数据里就要维护三份。
	//
	// # 没配怎么办
	//
	// 留空是合法的，回跳仍然验签、仍然回 200，只是不跳转（见 service/return.go）。
	// 它不像 NotifyBaseURL 那样「缺了就是错的」：回跳只影响用户看到什么，不影响钱。
	//
	// 里面的 `{merOrderId}` 会被换成支付单号（见 returnPageURL）。
	ReturnPageURL string
}

// PaymentService 是支付业务层。
type PaymentService struct {
	repository Repository
	// catalog 是这份代码认识的**全部**支付方式与渠道（见 internal/catalog）。它从库里
	// 搬到了代码里，所以这里持有的是一个常量表，而不是一次查询。
	catalog   *catalog.Catalog
	providers ProviderRegistry
	secrets   SecretResolver
	// beans 是账户出资那条路的账本入口。允许为 nil：没接账户域的部署（今天只有测试会这样）
	// 选了豆支付会**明确失败**（见 createAccountPayment 的第一道检查），不会静默扣不到豆
	// 却把支付单推成 succeeded。
	beans BeanLedger

	paymentTTL     time.Duration
	now            func() time.Time
	newPaymentNo   func(time.Time) string
	newRefundNo    func(time.Time) string
	newAgreementNo func(time.Time) string
	notifyBaseURL  string
	// returnPageTemplate 是结果页地址的模板（见 Options.ReturnPageURL），**不是**拼好的地址：
	// 每一单的结果页都带自己的支付单号，所以存模板、每次现填（见 returnPageURL）。
	returnPageTemplate string
}

// New 构造业务层。
//
// directory 是那份常量表，**不允许为 nil**：它回答的是「这个 code 是哪一种支付方式」，
// 而那正是发起支付的第一个问题。装配处传 catalog.FromConfig(cfg)，测试传
// catalog.FromConfig(catalog.Config{})（一个没接银联商务的部署，只剩豆支付）。
//
// providers 与 secrets 允许为 nil：只有「按渠道发起支付」和「认渠道回调」两条路需要它们，
// 而这两条路上遇到 nil 会明确失败，不会静默跳过验签或猜一个适配器。beans 同理——只有
// action=account 那条路需要它。
func New(r Repository, directory *catalog.Catalog, providers ProviderRegistry, secrets SecretResolver, beans BeanLedger, options Options) *PaymentService {
	if options.PaymentTTL <= 0 {
		options.PaymentTTL = DefaultPaymentTTL
	}
	if options.Now == nil {
		options.Now = time.Now
	}
	if options.NewPaymentNo == nil {
		options.NewPaymentNo = paymentNo
	}
	if options.NewRefundNo == nil {
		options.NewRefundNo = refundNo
	}
	if options.NewAgreementNo == nil {
		options.NewAgreementNo = agreementNo
	}
	return &PaymentService{
		repository:         r,
		catalog:            directory,
		providers:          providers,
		secrets:            secrets,
		beans:              beans,
		paymentTTL:         options.PaymentTTL,
		now:                options.Now,
		newPaymentNo:       options.NewPaymentNo,
		newRefundNo:        options.NewRefundNo,
		newAgreementNo:     options.NewAgreementNo,
		notifyBaseURL:      strings.TrimRight(options.NotifyBaseURL, "/"),
		returnPageTemplate: strings.TrimSpace(options.ReturnPageURL),
	}
}

// PaymentTTL 暴露给调用方（超时关单的扫描周期要参考它）。
func (s *PaymentService) PaymentTTL() time.Duration { return s.paymentTTL }

// resolveSecret 按渠道读一把凭据。resolver 没配时返回空串——调用方会把它当成「读不到」，
// 于是验签拒绝。**绝不默认放行**，这是这个函数的全部要点。
//
// 只做「trim + 判空」这一层，槽名到环境变量名的对照在装配处那份实现里（见 catalog）。
func (s *PaymentService) resolveSecret(channel *catalog.Channel, slot string) string {
	if s.secrets == nil || channel == nil {
		return ""
	}
	return strings.TrimSpace(s.secrets(channel, slot))
}

// resolveSecrets 按**适配器自己声明的槽**，解析出这一次调用可用的全部凭据明文。
//
// 这是「槽名只有一处定义」那句话在装配处的落点：service 不知道微信要三样、银联商务要两样，
// 它只问适配器要那几个名字，然后照着逐个解析。协议族再多一个槽，这里一行都不用改——要改的
// 只有那个协议的适配器（它的 SecretSlots 与 Slots 是同一批常量）。
//
// **没有兜底那一把了**：从前它对应渠道行的 secret_ref，而那张表已经不在。今天每一个适配器
// 都实现 SecretSlotter（唯一一个不实现的 manual 已随它的协议族删除），所以「一把都没声明」
// 这件事在代码里不可能发生；解析出来是空表的话，适配器自己会因为取不到密钥而拒绝签名。
func resolveSecrets(lookup func(*catalog.Channel, string) string, channel *catalog.Channel, adapter provider.Provider, method provider.Method) provider.Credentials {
	slots := provider.SecretSlotsFor(adapter, method)
	secrets := make(provider.Credentials, len(slots))
	for _, slot := range slots {
		secrets[slot] = lookup(channel, slot)
	}
	return secrets
}

// notifyURL 拼出这个渠道的回调地址。
func (s *PaymentService) notifyURL(channelCode string) string {
	if s.notifyBaseURL == "" {
		return ""
	}
	return s.notifyBaseURL + "/v1/payments/callback/" + channelCode
}

// returnURL 拼出这个渠道的结果页回跳地址。
//
// 与 notifyURL **共用同一个公开基址**：两条路打的是同一个服务的同一个端口，域名没有理由
// 不同，多一个配置项只会多一种「回调到了、回跳没到」的配错法。
//
// `/v1/payments/return` 这一段与 controller.ReturnPath 是同一个值，这里是**硬编码的第二份**
// ——与 notifyURL 那边一模一样（那里硬编码的 callback 前缀也在 controller.CallbackPath）。
// 为什么不去共享常量：controller 已经 import service，反过来引就是环。两处各留一句注释，
// 改前缀时知道要去哪儿找另一处。
func (s *PaymentService) returnURL(channelCode string) string {
	if s.notifyBaseURL == "" {
		return ""
	}
	return s.notifyBaseURL + "/v1/payments/return/" + channelCode
}

// returnPageURL 把配置里的结果页模板填成这一单的地址。
//
// `{merOrderId}` 被换成**我们自己的支付单号**（不是渠道报文里的任何字段）：这个值来自
// 验签之后的 provider.Notification，但它同时是我们生成的、字符集只有数字与大写字母
// （见 paymentNo），所以这里直接替换、不做转义。换成渠道给的自由文本才需要转义，而那一份
// 从来不进这个函数。
//
// 占位符不在模板里时**照原样返回**（运营可以配一个不带参数的静态页），这不算配错。
// 没配结果页时返回空串，调用方据此回 200 而不是 302。
func (s *PaymentService) returnPageURL(paymentNo string) string {
	if s.returnPageTemplate == "" {
		return ""
	}
	return strings.ReplaceAll(s.returnPageTemplate, placeholderMerOrderID, paymentNo)
}

// placeholderMerOrderID 是结果页模板里的支付单号占位符。
const placeholderMerOrderID = "{merOrderId}"
