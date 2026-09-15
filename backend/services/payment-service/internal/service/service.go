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
	FindPaymentMethod(ctx context.Context, methodID string) (*repository.PaymentMethodWithChannel, error)
	FindChannelByCode(ctx context.Context, code string) (*repository.ChannelRecord, error)
	FindPaymentByNo(ctx context.Context, paymentNo string) (*model.Payment, error)

	// —— 发起支付的三段事务（见 create.go 的流程图）——
	BeginPayment(ctx context.Context, p repository.BeginPaymentParams) (*model.Payment, []byte, bool, error)
	MarkPaymentPending(ctx context.Context, p repository.MarkPaymentPendingParams) (*model.Payment, error)
	MarkPaymentFailed(ctx context.Context, p repository.MarkPaymentFailedParams) (*model.Payment, error)
	// SettleAccountPayment 是账户出资那条路的终止事务（见 createAccountPayment）。它与
	// MarkPaymentPending 是**平级的两条路**，不是同一段事务的两种写法：渠道支付会停在
	// pending，账户出资不会。
	SettleAccountPayment(ctx context.Context, p repository.SettleAccountPaymentParams) (*model.Payment, error)
	// RecordAccountDeduction 把「账户域已经扣了这笔豆」登记到支付单上。它**先于结算**，
	// 而且刻意不跟结算共用一个事务：要覆盖的正是结算失败的那个窗口（见 005 迁移）。
	RecordAccountDeduction(ctx context.Context, paymentID, accountEntryID string, fundedAt time.Time) error
	RecordProviderCall(ctx context.Context, p repository.ProviderCallParams) error

	// —— 回调 ——
	InsertNotification(ctx context.Context, p repository.NotificationParams) (*repository.NotificationRecord, error)
	MarkNotification(ctx context.Context, notificationID, status, reason string) error
	SettlePayment(ctx context.Context, p repository.SettleNotificationParams) (*repository.PaymentSettlement, error)

	// —— 超时关单 ——
	ExpireOverduePayments(ctx context.Context, limit int) (int, error)
	// FindOverdueAccountFundedPayments 找出「豆已经扣了、到点还没结算」的支付单，
	// 交给 SettleOverdueAccountPayments 结算（见 expire.go）。
	FindOverdueAccountFundedPayments(ctx context.Context, limit int) ([]model.Payment, error)
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
// 只有这一个动作，冲正走的是事件（order.after_sale.reviewed → account-service 自己消费），
// 不从这里调。多留一个今天没有调用方的方法，等于让「谁在什么时候冲正」有两处说法。
type BeanLedger interface {
	Deduct(ctx context.Context, req client.DeductRequest) (client.DeductResult, error)
}

// SecretResolver 把渠道的 secret_ref 解析成密钥本身。
//
// 它是个函数而不是接口：解析就是「拿一个键名换一个值」这一件事，没有状态也没有别的
// 实现维度。返回空串表示读不到——**调用方必须把它当成拒绝的理由，不是默认值**。
type SecretResolver func(ref string) string

// 请求本身不合法（controller / rpc 统一回 InvalidArgument）。
var (
	ErrOrderNoRequired     = errors.New("orderNo is required")
	ErrOrderIDRequired     = errors.New("orderId is required")
	ErrOrderIDInvalid      = errors.New("orderId must be a uuid")
	ErrUserIDRequired      = errors.New("userId is required")
	ErrUserIDInvalid       = errors.New("userId must be a uuid")
	ErrAmountNotPositive   = errors.New("amount must be positive")
	ErrMethodIDRequired    = errors.New("paymentMethodId is required")
	ErrMethodIDInvalid     = errors.New("paymentMethodId must be a uuid")
	ErrRequestIDRequired   = errors.New("requestId is required")
	ErrChannelCodeRequired = errors.New("channel code is required")
	ErrNotificationEmpty   = errors.New("notification body is empty")
)

// 状态与配置（controller 各自映射成 404/409/501/503）。
var (
	// ErrPaymentNotFound：支付单不存在。回调指着一个我们没有的单。
	ErrPaymentNotFound = repository.ErrPaymentNotFound
	// ErrChannelNotFound：回调的渠道代码在库里没有对应的行。**不是 404 给渠道看的那种
	// 「没有」**，是我们自己的配置问题，但对回调方一律回失败。
	ErrChannelNotFound = repository.ErrChannelNotFound
	// ErrPaymentMethodNotFound：调用方给了一个库里没有的支付方式 id。
	ErrPaymentMethodNotFound = repository.ErrPaymentMethodNotFound
	// ErrPaymentMethodInactive：支付方式存在但被运营停用了。
	ErrPaymentMethodInactive = repository.ErrPaymentMethodInactive
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
	// ErrUnauthorizedAction：调用方宣称了一个不属于它的支付方式（渠道没有这个 action
	// 能起的支付）。
	ErrUnauthorizedAction = errors.New("payment method action is not supported")
	// ErrInsufficientCoffeeBeans：账户出资时用户的豆不够。**这不是故障**，是一次与
	// 「渠道拒绝」同级的业务结论——它落成 status='failed' 的结果，不是返回给调用方的 error。
	// 判据（哪个 gRPC 码算余额不足）在 internal/client 那边，业务层只认这个名字。
	ErrInsufficientCoffeeBeans = client.ErrInsufficientBeans
)

// RetryableErrors 是「重试可能成功」的那一组。controller 用 503、rpc 用 Unavailable，
// 而不是 400：调用方拿到它应当退避重试，不是去改请求。
var RetryableErrors = []error{
	ErrIdempotencyInProgress,
}

// ValidationErrors 是「请求不合法」这一类错误的全集。放在一处而不是在 controller 里
// 逐个 case：新增一条校验就要在 controller 里同步加一个 case，是必然漏掉的写法。
var ValidationErrors = []error{
	ErrOrderNoRequired, ErrOrderIDRequired, ErrOrderIDInvalid, ErrUserIDRequired, ErrUserIDInvalid,
	ErrAmountNotPositive, ErrMethodIDRequired, ErrMethodIDInvalid, ErrRequestIDRequired,
	ErrChannelCodeRequired, ErrNotificationEmpty,
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
	// NotifyBaseURL 是渠道回调地址的前缀，拼成 `<base>/v1/payments/callback/<渠道码>`。
	//
	// 它不进数据库配置：同一个渠道在 dev / staging / prod 的域名不同，写进
	// payment_channels.config 就得多一套配置，而且改域名要改数据。它跟着部署走。
	NotifyBaseURL string
}

// PaymentService 是支付业务层。
type PaymentService struct {
	repository Repository
	providers  ProviderRegistry
	secrets    SecretResolver
	// beans 是账户出资那条路的账本入口。允许为 nil：没接账户域的部署（今天只有测试会这样）
	// 选了豆支付会**明确失败**（见 createAccountPayment 的第一道检查），不会静默扣不到豆
	// 却把支付单推成 succeeded。
	beans BeanLedger

	paymentTTL    time.Duration
	now           func() time.Time
	newPaymentNo  func(time.Time) string
	notifyBaseURL string
}

// New 构造业务层。
//
// providers 与 secrets 允许为 nil：只有「按渠道发起支付」和「认渠道回调」两条路需要它们，
// 而这两条路上遇到 nil 会明确失败，不会静默跳过验签或猜一个适配器。beans 同理——只有
// action=account 那条路需要它。
func New(r Repository, providers ProviderRegistry, secrets SecretResolver, beans BeanLedger, options Options) *PaymentService {
	if options.PaymentTTL <= 0 {
		options.PaymentTTL = DefaultPaymentTTL
	}
	if options.Now == nil {
		options.Now = time.Now
	}
	if options.NewPaymentNo == nil {
		options.NewPaymentNo = paymentNo
	}
	return &PaymentService{
		repository:    r,
		providers:     providers,
		secrets:       secrets,
		beans:         beans,
		paymentTTL:    options.PaymentTTL,
		now:           options.Now,
		newPaymentNo:  options.NewPaymentNo,
		notifyBaseURL: strings.TrimRight(options.NotifyBaseURL, "/"),
	}
}

// PaymentTTL 暴露给调用方（超时关单的扫描周期要参考它）。
func (s *PaymentService) PaymentTTL() time.Duration { return s.paymentTTL }

// resolveSecret 按渠道读密钥。resolver 没配时返回空串——调用方会把它当成「读不到」，
// 于是验签拒绝。**绝不默认放行**，这是这个函数的全部要点。
func (s *PaymentService) resolveSecret(channel *model.PaymentChannel) string {
	if s.secrets == nil || channel == nil {
		return ""
	}
	ref := strings.TrimSpace(channel.SecretRef)
	if ref == "" {
		return ""
	}
	return strings.TrimSpace(s.secrets(ref))
}

// notifyURL 拼出这个渠道的回调地址。
func (s *PaymentService) notifyURL(channelCode string) string {
	if s.notifyBaseURL == "" {
		return ""
	}
	return s.notifyBaseURL + "/v1/payments/callback/" + channelCode
}
