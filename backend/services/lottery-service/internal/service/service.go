// Package service 是抽奖的业务层：校验、编排三段事务、算种子、驱动期次状态机。
//
// 按业务动作拆文件：activation.go 开通（含默认活动与第一期）、campaign.go 活动与奖池、
// participate.go 用户参与的三段事务、draw.go 开奖与作废、sweep.go 修复 worker 的取件。
// 仓储只负责「一个事务里把这几个事实写进去」，「几个事实分别是什么」由这里决定。
//
// **这个包不持有任何余额。** 福卡张数在 account-service，参与时同步调它的
// DeductFortuneCards；本包只在参与记录上留一个 fortune_entry_id 当值引用
// （方案 §5.7：「Lottery 只拥有抽奖和奖品数据；福卡余额由 Account 负责」）。
//
// # 这条业务线的形状
//
// 订单完成 → account-service 发福卡 → 用户拿福卡参与某一期 → 期次达标或到点 → 开奖 →
// 中奖记录停在 pending。**领取与核销整块延后**（用户拍板）：中奖记录的数据模型与状态机
// 按最终形态建全了，但没有领取入口、没有商户端核销、没有奖品兑付（券服务至今没有 gRPC）。
//
// # 退款追回：规则已写下，实现不做
//
// 「已开奖期次是终局；未开奖期次内的参与在退款成功时冲正 + 回退 participant_count
// （跌破门槛且未到点则把期次开回 open）」——这条规则写在这里而不是写进代码，因为它要消费
// order.after_sale.refunded，而那个事件本身还是个未来事件（payment-service 的退款单没做）。
// **account-service 自己到今天也没有「退款成功后的追回」**，抽奖不该做第一个扣这条线的人。
// 所以本轮 ConsumerInbox / ConsumerHandler / RABBITMQ_QUEUE 全都不配。
package service

import (
	"context"
	"errors"
	"time"

	"github.com/panda-dev/panda-v2/backend/services/lottery-service/internal/client"
	"github.com/panda-dev/panda-v2/backend/services/lottery-service/internal/dto"
	"github.com/panda-dev/panda-v2/backend/services/lottery-service/internal/model"
	"github.com/panda-dev/panda-v2/backend/services/lottery-service/internal/repository"
)

// Repository 是抽奖库的数据访问契约。
//
// 定义成接口而不是直接用 *repository.PostgresRepository，是为了让这一层里最容易出错的
// 那部分——校验、状态机分支、补偿路径的走向——能在没有数据库的情况下测。那些逻辑不该
// 为了测它去起一个 PG。
type Repository interface {
	// —— 开通 ——
	Activate(ctx context.Context, p repository.ActivateParams) (*repository.ActivationCreated, error)
	GetActivationView(ctx context.Context, id string) (*repository.ActivationListRow, error)
	FindActivationByLocation(ctx context.Context, locationID string) (*model.Activation, error)
	UpdateActivationStatus(ctx context.Context, id, status, remark string, actor *string) (*model.Activation, error)
	ListActivations(ctx context.Context, q dto.ActivationQuery) ([]*repository.ActivationListRow, int, error)
	CountCampaigns(ctx context.Context, activationID string) (int, error)

	// —— 活动与奖池 ——
	CreateCampaign(ctx context.Context, p repository.CampaignParams) (*model.Campaign, error)
	UpdateCampaign(ctx context.Context, id string, p repository.CampaignParams) (*model.Campaign, error)
	UpdateCampaignStatus(ctx context.Context, id, status string, actor *string) (*model.Campaign, error)
	GetCampaign(ctx context.Context, id string) (*model.Campaign, error)
	GetCampaignView(ctx context.Context, id string) (*repository.CampaignListRow, error)
	GetDefaultCampaign(ctx context.Context, activationID string) (*model.Campaign, error)
	ListCampaigns(ctx context.Context, q dto.CampaignQuery) ([]*repository.CampaignListRow, int, error)
	ListPrizes(ctx context.Context, campaignID string) ([]*model.CampaignPrize, error)

	// —— 期次 ——
	GetRound(ctx context.Context, id string) (*model.Round, error)
	GetRoundView(ctx context.Context, id string) (*repository.RoundListRow, error)
	LiveRound(ctx context.Context, campaignID string) (*model.Round, error)
	ListRounds(ctx context.Context, q dto.RoundQuery) ([]*repository.RoundListRow, int, error)
	EnsureLiveRound(ctx context.Context, campaignID string, now time.Time) (*model.Round, error)
	CancelRound(ctx context.Context, roundID, reason string, actor *string) (*model.Round, error)

	// —— 参与的三段事务 ——
	Begin(ctx context.Context, p repository.BeginParams) (*repository.BeginResult, error)
	Confirm(ctx context.Context, p repository.ConfirmParams) (*model.Round, bool, error)
	Fail(ctx context.Context, p repository.FailParams) error
	NoteAttempt(ctx context.Context, participationID string, cause error) error
	GetParticipation(ctx context.Context, id string) (*model.Participation, error)
	PendingParticipations(ctx context.Context, repairAfter time.Duration, limit int) ([]string, error)
	CountUserParticipations(ctx context.Context, roundID, userID string) (int32, error)
	ListParticipations(ctx context.Context, q dto.ParticipationQuery) ([]*repository.ParticipationListRow, int, error)

	// —— 开奖与中奖 ——
	DrawRound(ctx context.Context, p repository.DrawParams) (*repository.DrawOutcome, error)
	GetDraw(ctx context.Context, id string) (*model.Draw, error)
	FindDrawByRound(ctx context.Context, roundID string) (*model.Draw, error)
	RoundsAwaitingDraw(ctx context.Context, now time.Time, limit int) ([]string, error)
	GetWin(ctx context.Context, id string) (*model.Win, error)
	ListWins(ctx context.Context, q dto.WinQuery) ([]*model.Win, int, error)
	ListWinEvents(ctx context.Context, winID string) ([]*model.WinEvent, error)
	CountWinsByUser(ctx context.Context, userID string) (total int, pending int, err error)
}

// FortuneCards 是账户域福卡账户的三个动作用到的那一部分。
//
// 它是个接口而不是直接收 *client.FortuneCardClient，理由同 Repository：装配处传真的，
// 测试传一个能摆出「余额不足」与「超时」两种结果的假的。**接口只有三个方法**，与契约
// fortune_card.proto 今天的三个 RPC 一一对应——契约长一个新的 RPC 时，这里要不要跟着长
// 是一个需要想的问题，而不是自动同步。
type FortuneCards interface {
	Deduct(ctx context.Context, req client.DeductRequest) (client.DeductResult, error)
	Reverse(ctx context.Context, entryID, title, remark string) (client.ReverseResult, error)
	// Balance 只给抽奖中心显示用，**不参与任何判定**（见 client.FortuneCardClient.Balance）。
	Balance(ctx context.Context, userID string) (int64, error)
}

// 请求本身不合法（controller 统一回 400）。
var (
	ErrLocationIDRequired   = errors.New("locationId is required")
	ErrLocationIDInvalid    = errors.New("locationId must be a uuid")
	ErrLocationNameRequired = errors.New("locationName is required")
	ErrStatusRequired       = errors.New("status is required")
	ErrStatusInvalid        = errors.New("status is not one of the allowed values")

	ErrCampaignIDRequired     = errors.New("campaignId is required")
	ErrCampaignIDInvalid      = errors.New("campaignId must be a uuid")
	ErrMachineIDInvalid       = errors.New("machineId must be a uuid when present")
	ErrCampaignCodeRequired   = errors.New("campaign code is required")
	ErrCampaignCodeInvalid    = errors.New("campaign code must be 1-16 uppercase letters or digits")
	ErrCampaignNameRequired   = errors.New("campaign name is required")
	ErrTargetNotPositive      = errors.New("participantTarget must be positive")
	ErrCampaignWindowInvalid  = errors.New("endAt must be after startAt")
	ErrPrizesRequired         = errors.New("at least one prize is required")
	ErrPrizeKindInvalid       = errors.New("prizeKind is not one of the allowed values")
	ErrPrizeNameRequired      = errors.New("prize name is required")
	ErrPrizeQuantityInvalid   = errors.New("prize quantity must be positive")
	ErrPrizeSortOrderConflict = errors.New("prize sortOrder is duplicated")

	ErrOrderIDInvalid = errors.New("sourceOrderId must be a uuid")
	// ErrUserIDRequired：这次请求没有身份。
	//
	// 它不是「请求体里少填了一个字段」——userId 从来不在请求体里，它来自令牌。走到这里
	// 说明授权链配错了（路由没挂认证中间件），所以它**不进 ValidationErrors**：那不是
	// 400 该回的东西。
	ErrUserIDRequired = errors.New("the request carries no user identity")

	ErrRoundIDRequired   = errors.New("roundId is required")
	ErrRoundIDInvalid    = errors.New("roundId must be a uuid")
	ErrReasonRequired    = errors.New("reason is required")
	ErrReasonTooLong     = errors.New("reason must be at most 200 characters")
	ErrIdempotencyNeeded = errors.New("either sourceOrderId or the Idempotency-Key header is required")
	// ErrExpectedCountRequired：人工开奖必须报出「我看到的这一期有几个人」。
	//
	// 它是**必填**而不是可选：只校验状态不校验人数，会漏掉「状态没变、人又进来了几个」那一
	// 类变化——而开奖名单正是按人数取的，人数变了名单就变了。
	ErrExpectedCountRequired = errors.New("expectedParticipantCount is required for a manual draw")
)

// 身份相关（controller 映射成 401）。
var (
	// ErrActorRequired：这次操作没有留下是谁做的。
	//
	// 人工开奖 / 作废的 drawn_by 是必填的（数据库 CHECK 也挡）：这两个是全系统少数几个能
	// 直接决定谁中奖的动作，一条查不到操作人的记录等于没有记录。
	ErrActorRequired = errors.New("the request carries no actor identity")

	// ErrRoundNotAwaitingDraw 见 repository 同名错误：扫描之后期次变了，锁内重判发现不该开。
	ErrRoundNotAwaitingDraw = repository.ErrRoundNotAwaitingDraw
)

// 状态与归属（controller 各自映射成 404 / 409 / 429 / 503）。
var (
	// ErrActivationNotFound：这家门店没有开通过抽奖。
	ErrActivationNotFound = repository.ErrActivationNotFound
	// ErrLocationAlreadyActivated：这家门店已经开通过。重复点击不是两笔业务。
	ErrLocationAlreadyActivated = repository.ErrLocationAlreadyActivated
	// ErrCampaignNotFound：活动不存在。
	ErrCampaignNotFound = repository.ErrCampaignNotFound
	// ErrCampaignCodeTaken：活动短名被别的活动占了（它是期次号的前缀，全局唯一）。
	ErrCampaignCodeTaken = repository.ErrCampaignCodeTaken
	// ErrDefaultCampaignExists：这个开通记录已经有默认活动了。
	ErrDefaultCampaignExists = repository.ErrDefaultCampaignExists
	// ErrRoundNotFound：期次不存在。
	ErrRoundNotFound = repository.ErrRoundNotFound
	// ErrRoundAlreadyLive：这个活动已经有一期在跑。
	ErrRoundAlreadyLive = repository.ErrRoundAlreadyLive
	// ErrRoundClosed：期次不在收人窗口内。
	//
	// **它不是故障**：用户在扣卡通路上被开奖抢先了，什么也没得到，卡要原路退回去。
	ErrRoundClosed = repository.ErrRoundClosed
	// ErrRoundChanged：人工开奖时页面上那份状态已经过期。
	ErrRoundChanged = repository.ErrRoundChanged
	// ErrRoundAlreadyDrawn：这一期已经开过奖了。
	ErrRoundAlreadyDrawn = repository.ErrRoundAlreadyDrawn
	// ErrRoundCancelled：这一期已经被作废了。
	ErrRoundCancelled = repository.ErrRoundCancelled
	// ErrRoundHasParticipations：有参与者的期次不能作废。
	ErrRoundHasParticipations = repository.ErrRoundHasParticipations
	// ErrIdempotencyKeyConflict：幂等键撞上了别人的参与记录。
	ErrIdempotencyKeyConflict = repository.ErrIdempotencyKeyConflict
	// ErrParticipationNotFound：参与记录不存在。
	ErrParticipationNotFound = repository.ErrParticipationNotFound
	// ErrMachineMismatch：设备级活动要求参与时报的就是那台设备。
	ErrMachineMismatch = repository.ErrMachineMismatch
	// ErrWinNotFound：中奖记录不存在。
	ErrWinNotFound = repository.ErrWinNotFound
	// ErrDrawNotFound：开奖记录不存在。
	ErrDrawNotFound = repository.ErrDrawNotFound

	// ErrInsufficientFortuneCards：福卡不够。**不是故障**，是一次说得清楚的业务结论
	// （400 + 业务码），用户看到的是「福卡不够了」而不是「系统繁忙」。
	ErrInsufficientFortuneCards = client.ErrInsufficientFortuneCards
	// ErrParticipationPending：扣减没有得出结论（超时、账户域不可达），参与停在 pending。
	//
	// 这是**唯一一个「还不知道结果」的返回**，controller 把它映成 202 而不是 4xx/5xx：
	// 用户的卡可能已经被扣了，我们不能说失败；也可能没扣，我们也不能说成功。修复 worker
	// 会用同一个 request_id 重跑，那一次会得到确切的答案。
	ErrParticipationPending = errors.New("participation is awaiting confirmation")
	// ErrInvalidDeductRequest：账户域说这次扣减的**参数**不对（负数张数、空 user_id）。
	//
	// 那是本服务自己的 bug，不是用户的错，所以要大声记日志。参与会标成
	// failed/invalid_request——重试一万次也是同样的结果，没有重试的价值。
	//
	// 与 ErrInsufficientFortuneCards 一样，结论名在业务层、判据在传输层
	// （client.Deduct 把 codes.InvalidArgument 翻成它）。
	ErrInvalidDeductRequest = client.ErrInvalidDeductRequest
	// ErrFortuneCardsUnavailable：这次部署根本没有接上账户域。
	//
	// 它不是「余额不足」，也不该被当成一次业务结论——没有账户域就没有任何一笔参与能被
	// 扣卡，参与**明确失败**而不是记成一条 confirmed 却没扣卡的记录。今天只有测试会走到
	// （New 的 cards 允许为 nil）。
	ErrFortuneCardsUnavailable = errors.New("fortune card account is not configured")
	// ErrParticipationFailed 是一条已有结论的失败记录被回放，而它的 failure_code 是本服务
	// 还不认识的那一个（只可能来自将来新增的取值，比如退款追回）。
	//
	// 兜底而不是「默认成功」：把一条看不懂原因的失败当成成功回给用户，是我们能犯的最贵的
	// 那类错误。真走到这里，日志里有 failure_code 可以查。
	ErrParticipationFailed = errors.New("participation has already failed")
)

// ValidationErrors 是「请求不合法」这一类错误的全集。放在一处而不是在 controller 里逐个
// case：新增一条校验就要在 controller 里同步加一个 case，是必然漏掉的写法。
var ValidationErrors = []error{
	ErrLocationIDRequired, ErrLocationIDInvalid, ErrLocationNameRequired,
	ErrStatusRequired, ErrStatusInvalid,
	ErrCampaignIDRequired, ErrCampaignIDInvalid, ErrMachineIDInvalid,
	ErrCampaignCodeRequired, ErrCampaignCodeInvalid,
	ErrCampaignNameRequired, ErrTargetNotPositive, ErrCampaignWindowInvalid,
	ErrPrizesRequired, ErrPrizeKindInvalid, ErrPrizeNameRequired, ErrPrizeQuantityInvalid,
	ErrPrizeSortOrderConflict,
	ErrOrderIDInvalid,
	ErrRoundIDRequired, ErrRoundIDInvalid, ErrReasonRequired, ErrReasonTooLong, ErrIdempotencyNeeded,
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

// DefaultCampaignTarget 是开通时内置模板的参与门槛。
//
// 与 DefaultCampaignName / DefaultPrizeName 一样，它的存在是为了让「开通」这一个动作就能
// 产出一个能跑的抽奖：填 30 而不是要求运营每次想一个数——想不出来的人会填 1，而一个
// 门槛为 1 的活动会在第一个人参与时就开奖。
const (
	DefaultCampaignTarget = 30
	DefaultCampaignName   = "门店抽奖"
	DefaultPrizeName      = "神秘礼品"
)

// DefaultCampaignWindow 是内置模板的活动窗口长度。
//
// 九十个工作日不是一个有讲究的数，它回答的是一个具体的问题：开通一个门店抽奖而它当场就
// 结束显然是错的，所以必须有一个兜底的窗口。运营可以在开通后改。
const DefaultCampaignWindow = 90 * 24 * time.Hour

// MaxReasonLength 是人工开奖 / 作废理由的长度上限，按**字符**算（不是字节）。
//
// 中文一个字符三字节，按字节限 200 会让运营在写了一百来个字的时候被莫名其妙地拒绝。
const MaxReasonLength = 200

// Options 是构造 LotteryService 的可调项。零值等于用默认值。
type Options struct {
	// Now 为 nil 时用 time.Now。注入它才能测「到点开奖」、「窗口已过」这类判定而不必真的等。
	//
	// **它流进仓储的那些 Params，也被 SQL 用 NOW() 读**——两者在同一个事务里，相差微秒级。
	// 之所以不把 SQL 全换成 $n：NOW() 是数据库侧的事实，而调用方手里那个是业务判定的输入，
	// 混成一个字段会让「测试里假装现在是明天」这件事在两种语义之间摇摆。
	Now func() time.Time
	// DeductTimeout 是调用账户域扣减卡片的硬超时。
	//
	// 3 秒而不是默认的无限等：这是一条**用户点了按钮在等**的同步调用，挂住的后果是用户
	// 看到一个转不完的圈。超时不等于失败——参与停在 pending，修复 worker 会拿着同一个
	// request_id 重跑。
	DeductTimeout time.Duration
	// RepairAfter 是修复 worker 认为一条 pending 记录「该被重跑」的最小年龄。
	//
	// 60 秒的下限不是「等它一下」：一段正常的三段事务是毫秒级的，一分钟还停在 pending 只
	// 可能来自「进程在第二段中途死了」或「扣减调用挂住了」。等待时间越长，用户的卡被扣了
	// 却没进期次的窗口越大。
	RepairAfter time.Duration
}

// DefaultDeductTimeout 见 Options.DeductTimeout。
const DefaultDeductTimeout = 3 * time.Second

// DefaultRepairAfter 见 Options.RepairAfter。
const DefaultRepairAfter = 60 * time.Second

// LotteryService 是抽奖业务层。
type LotteryService struct {
	repository Repository
	// cards 允许为 nil：没接账户域的部署（今天只有测试会这样）参与会**明确失败**
	// （见 Participate 的第一道检查），不会静默扣不到卡却把参与记成 confirmed。
	cards FortuneCards

	now           func() time.Time
	deductTimeout time.Duration
	repairAfter   time.Duration
}

// New 构造业务层。
func New(r Repository, cards FortuneCards, options Options) *LotteryService {
	if options.Now == nil {
		options.Now = time.Now
	}
	if options.DeductTimeout <= 0 {
		options.DeductTimeout = DefaultDeductTimeout
	}
	if options.RepairAfter <= 0 {
		options.RepairAfter = DefaultRepairAfter
	}
	return &LotteryService{
		repository:    r,
		cards:         cards,
		now:           options.Now,
		deductTimeout: options.DeductTimeout,
		repairAfter:   options.RepairAfter,
	}
}

// RepairAfter 暴露给调用方（修复 worker 的扫描周期要参考它）。
func (s *LotteryService) RepairAfter() time.Duration { return s.repairAfter }

// ParticipationCost 是参与一次扣几张福卡。
//
// **写死在服务端**，不让请求体传：那等于让一个被篡改的小程序决定扣几张卡。原型的规则是
// 一次一张，将来如果变成「一次参与消耗 N 张」，改的是这一个常量，而不是一个请求字段的
// 校验宽度。
const ParticipationCost int64 = 1
