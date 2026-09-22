// Package repository 是会员库的数据访问层。
//
// 按聚合拆文件：plan.go 是套餐、membership.go 是会员资格（含开通与续期的写路径）、
// subscription.go 是连续包月的期次、change.go 是只增的变更流水。
//
// 这一份的 appendOutbox / mapPGError / inTx 与 lottery-service、payment-service、
// order-service、account-service、payment-service 的同名函数几乎逐行一致——幂等与 outbox
// 在这个仓库里只有一种语义，换服务时不该重新理解一遍。**唯一的差别是表名**。
package repository

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/panda-dev/panda-v2/backend/platform/audit"
	"github.com/panda-dev/panda-v2/backend/platform/messaging"
)

var (
	// ErrPlanNotFound：套餐不存在。
	ErrPlanNotFound = errors.New("membership plan not found")
	// ErrPlanCodeTaken：套餐编码被别的套餐占了。
	//
	// 编码是稳定的业务标识（下单、签约、代扣都靠它对接），撞了不是并发问题，是**填重了**
	// ——所以它是一条说得清的冲突（换一个再存），不是 500。
	ErrPlanCodeTaken = errors.New("membership plan code is already in use")

	// ErrMembershipNotFound：这条会员不存在。
	ErrMembershipNotFound = errors.New("membership not found")
	// ErrMembershipExists：这个用户已经有会员了。
	//
	// **一个用户只能有一条会员记录**是硬约束（memberships_user_unique），不是并发产物：
	// 两条并存意味着「以哪条算会员价资格」没有答案，而这正是老库按 VIP 等级存多条的代价。
	//
	// 它有两个来源，都该是 409：
	//
	//   - 后台直接开通（GrantMembership）撞上已有会员——这是**正常答案**，不是异常。开通与
	//     续期是两件事，后者走会员详情页的「调整有效期」。
	//   - 两笔支付同时被处理时，读路径与写路径之间被插了一行（ApplyPaidOrder 的读-写空隙）。
	//     这一条是并发产物，重投一次就走续期分支了。
	ErrMembershipExists = errors.New("this user already has a membership")
	// ErrMembershipRevoked：这条会员已经被撤销，不接受任何自动恢复。
	//
	// 撤销是人工做的决定（风控、争议、退款），一次付款或一次开关不该把它推翻。它**不是**
	// 一条能靠重试解决的错误——所以调用方不该当成「稍后再来」，而该让它显形（见
	// ApplyPaidOrder 里关于死信的说明）。
	ErrMembershipRevoked = errors.New("this membership has been revoked")
	// ErrMembershipExpired：这条会员已经过期。
	//
	// 只有一种用法：给过期会员打开自动续费。过期**不是**「这人不该有任何操作」——他还能
	// 重新买（走 ApplyPaidOrder 的续期分支，续费会把 expired 翻回 active）。
	ErrMembershipExpired = errors.New("this membership has expired")
	// ErrMembershipNotActive：这条会员不是生效状态，这个操作对它没有意义。
	ErrMembershipNotActive = errors.New("this membership is not active")
	// ErrMembershipNotFrozen：这条会员没有被冻结，谈不上解冻。
	ErrMembershipNotFrozen = errors.New("this membership is not frozen")
	// ErrExpireBeforeStart：调整后的有效期不晚于开通时刻。
	//
	// 库上有 CHECK (expire_at > start_at) 兜底，但撞上去是一条 23514（500），而它其实是
	// 后台填错了一个日期——该在写之前就翻成一句能显示给运营的话。
	ErrExpireBeforeStart = errors.New("the new expiry must be later than the membership start")

	// ErrSubscriptionNotFound：这条订阅不存在。
	ErrSubscriptionNotFound = errors.New("membership subscription not found")
	// ErrLiveSubscriptionExists：这个会员已经有活着的订阅了。
	//
	// 撞的是 membership_subscriptions_live_unique。走到这里多半是**并发**：用户连点两次
	// 「开通连续包月」、或者一次事件的两次投递同时进来了。它的反面是签出两份代扣协议、
	// 扣两次钱，所以它必须是一条明确的冲突而不是 500。
	ErrLiveSubscriptionExists = errors.New("this membership already has a live subscription")
	// ErrSubscriptionNotCancellable：这条订阅当前的状态不允许取消。
	//
	// 只有 active 与 suspended 能取消（老后台只放行 active，suspended 是代扣那一刀补上的，见
	// CancelSubscription）：pending_sign 还没签成，谈不上「取消一份授权」；expired / cancelled
	// 已经结束了，再取消一次没有任何意义。
	//
	// **suspended 必须能取消**：它意味着连续扣款失败到停扣（见 model.MaxConsecutiveChargeFailures），
	// 而那一档除了「等下一次扣款成功自己回来」没有任何出路——用户想彻底不续了、运营看着一条
	// 永远不会再动的记录，两个人都需要这个按钮。停扣不是解约，协议还在（那正是它需要一个出口
	// 的理由）。
	ErrSubscriptionNotCancellable = errors.New("this subscription is not active")
	// ErrSubscriptionRequestUsed：这个幂等号之前用过，而它建的那条订阅**已经结束了**。
	//
	// 一次网络重试不可能横跨整个签约与解约，所以走到这里说明客户端把一个旧 requestId 又发了
	// 一次（比如缓存了上一次失败的请求体）。它不是「重放」：照着重放回一条已取消的订阅会让
	// 用户以为签好了，重新建一条又会撞 membership_changes_request_unique——两条都是错的，只有
	// 让他重新发起是对的。
	ErrSubscriptionRequestUsed = errors.New("this request has already been used for a subscription that has ended")
	// ErrSubscriptionNotSettleable：这条订阅的当前状态不允许被收口成 active。
	//
	// 只有 pending_sign 能变 active。一条已经 cancelled / expired 的订阅收到「协议生效了」说明
	// 有人的状态已经乱了（协议在解约之后又活过来、或者两个协议号指向了同一行）——**照做会把一条
	// 已经停掉的订阅重新接上扣款**，所以它必须显形成一条要人来看的冲突，而不是静默照办。
	ErrSubscriptionNotSettleable = errors.New("this subscription can no longer be settled as active")

	// ErrChargeEndedSubscription：一期扣款的结果落到了一条**已经结束**的订阅上。
	//
	// 说「已经结束」的只有 cancelled：那是有谁明确说过不续了（用户在小程序点关、运营在后台取消、
	// 或者协议被渠道作废）。扣款失败落到它上面无所谓——钱没动。**扣款成功落到它上面是一条要人
	// 来看的事**：钱收了，而这份订阅已经不在扣款序列里了。
	//
	// 处置与 ApplyPaidOrder 遇到 revoked 那一条逐字相同：不能默默吞掉这笔钱（用户查账单会查到），
	// 也不能照着「扣到了就续」把一条已经结束的订阅接回扣款序列（那是把一个已经关掉的东西自己
	// 打开）。两条都错，所以让它显形——返回错误让消息进死信，那里是人会来看的地方。
	ErrChargeEndedSubscription = errors.New("a charge is settled on a membership subscription that has ended")

	// ErrAgreementTaken：这份代扣协议已经绑在别的订阅上了。撞的是
	// membership_subscriptions_agreement_unique，反面同样是「钱可能扣两遍」。
	ErrAgreementTaken = errors.New("this payment agreement is already bound to a subscription")
	// ErrAgreementMismatch：渠道/支付那边回来的协议号与本地这一行记着的对不上。
	//
	// 它不是并发产物，也**不是用户能修的东西**：这一行的 contract_code 是签约时写死的，回来的
	// 应该是同一份协议。对不上说明有人拿错了协议号去问、或者支付库那一行被改过——两种都要人看。
	// 照着结论往下写会把别人的协议绑到这个人的订阅上，那比停下来更糟。
	ErrAgreementMismatch = errors.New("the reported agreement does not match this subscription")

	// ErrCampaignNotFound：这场店铺码活动不存在（或这个 scene 没有对应的活动）。
	ErrCampaignNotFound = errors.New("membership campaign not found")
	// ErrCampaignSceneTaken：这个 scene 被别的活动占了。
	//
	// 撞的是 membership_campaigns_scene_unique。扫码进来的请求手里只有 scene，两场活动共用一个
	// scene 就无从判断手里的码是哪一场——这是**填重了**，不是并发问题，所以要一条说得清的冲突
	// （换一个再存），不是 500。
	ErrCampaignSceneTaken = errors.New("this campaign scene is already in use")
	// ErrCampaignNotClaimable：这场活动现在领不了（已停用，或者不在起止窗口里）。
	//
	// 与「活动不存在」分开：前者是运营把活动关了或窗口过了，后者是码本身有问题。合并成一句话
	// 会让用户拿着一个过期的码得到「活动不存在」——而它明明存在，只是结束了。
	ErrCampaignNotClaimable = errors.New("this membership campaign is not claimable now")
	// ErrCampaignAlreadyClaimed：这场活动这个人已经领过了。
	//
	// 撞的是 membership_campaign_claims_user_unique。正常路径上撞不到它——领取在一个事务里
	// 先查了有没有领过（而且活动行被 FOR UPDATE 串行化）。走到这里说明查询与插入之间还是被插了
	// 一行，而它的反面是「一个人领到两份会员天数」，所以必须是一条明确的冲突而不是 500。
	ErrCampaignAlreadyClaimed = errors.New("this membership campaign has already been claimed")
	// ErrCampaignLocked：这场活动正开着，这几个字段不给改。
	//
	// 撞的是 UpdateCampaign 里那条「enabled 时 scene / store_id 必须原样」的判断。**它不是并发
	// 产物**：运营点保存时活动就是开着的，这是正常答案——先停用再改。
	ErrCampaignLocked = errors.New("this campaign must be disabled before these fields can change")
	// ErrDuplicateChange：这一单已经开通过或续期过了。
	//
	// 撞的是 membership_changes_order_unique：order_id 上、限 change_type 为 activate / renew
	// 的部分唯一索引（见 migrations/membership/003）。**按 order_id 而不是 order_id+变更类型**
	// 是有意的：重投一条 order.paid 走的是另一支（首次投递开通、重投续期），只按变更类型去重
	// 拦不住它，那个人会拿到两期会员。
	//
	// 支付成功回调会被重放，老系统同一个回调重试三四次是常事；**它不是故障**，是「这件事已经
	// 做过了」。所以调用方（service）拿到它应当当作成功，而不是回一个 500。
	ErrDuplicateChange = errors.New("this membership change has already been recorded")
)

// PostgresRepository 是会员库的数据访问实现。
//
// recorder 只用在**人工干预**路径上：后台直接调整有效期、冻结、撤销。方案 §11.6 把「影响用户
// 资产归属的人工操作」列进必审清单，而审计走平台的 admin.operation.logged → 身份库的
// admin_operation_logs，**本库不建自己的审计表**（见 platform/audit 的包说明）。
//
// 开通、续费、到期**不记审计**：那是系统的正常动作，每一次都记只会把真正要看的那几条淹掉，
// 它们的痕迹在 membership_changes 里（那张表本来就只增不改）。
type PostgresRepository struct {
	pool *pgxpool.Pool
	// recorder 为 nil 时用 audit.Noop：调用点的审计语句保持无条件执行，不留「忘了传 recorder
	// 就没有审计」的分支。
	recorder audit.Recorder
}

func NewPostgresRepository(pool *pgxpool.Pool, recorder audit.Recorder) *PostgresRepository {
	if recorder == nil {
		recorder = audit.Noop{}
	}
	return &PostgresRepository{pool: pool, recorder: recorder}
}

type scanner interface{ Scan(...any) error }

// querier 让同一段读逻辑既能用连接池也能用事务。
//
// readX 这类函数既被写路径调用（在事务里读回刚写的行，否则读不到未提交的数据），也被读路径
// 调用（直接用池）。多写一份只会在两处之间产生漂移——而漂移的表现是「同一条会员在详情页和
// 列表页字段不一样」，很难查。
type querier interface {
	QueryRow(context.Context, string, ...any) pgx.Row
	Query(context.Context, string, ...any) (pgx.Rows, error)
}

// newEventID 是 outbox 事件的 ID。抽成函数是为了让「一条事件一个 ID」只有一处实现。
func newEventID() string { return uuid.NewString() }

// inTx 跑一个事务，出错即回滚。
//
// 「改了会员」与「记下改了什么」「告诉别人改了」必须整体成功或整体不动：会员行动了而流水没
// 写，事后就没有任何东西能解释这个人为什么是会员；流水写了而事件没落，下游永远算不出会员价。
// 而 membership_changes 是只增的（触发器挡着 UPDATE / DELETE），写错了撤不回来。
func (r *PostgresRepository) inTx(ctx context.Context, fn func(pgx.Tx) error) error {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	// 提交后这次 Rollback 是无操作（返回 ErrTxClosed），所以不必判断返回值。
	defer func() { _ = tx.Rollback(ctx) }()
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// appendOutbox 在业务事务里追加一条领域事件。
//
// 为什么必须走 outbox 而不是直接投递：事件与它描述的事实得一起落地。事务回滚的事件不能
// 发出去，提交了的事件也不能因为 broker 当时不通就丢。
func appendOutbox(ctx context.Context, tx pgx.Tx, eventType, eventVersion, traceID string, payload []byte) error {
	return messaging.NewPostgreSQLWithQuerier(tx).Append(ctx, messaging.Envelope{
		EventID:      newEventID(),
		EventType:    eventType,
		EventVersion: eventVersion,
		TraceID:      traceID,
		Payload:      payload,
	})
}

// mapPGError 把唯一约束冲突翻成调用方分得清的业务错误。
//
// 没有它，「同一张订单的开通事件投了两次」会变成 500「服务器错误」，而它其实是一个说得清楚
// 的幂等结论。约束名是这里唯一稳定可依赖的东西：靠错误文本匹配会在 PG 换语言或换措辞时
// 静默失效。
//
// 下面这些名字是**从库里读出来的实际值**（\d memberships、\d membership_subscriptions），
// 不是照 DDL 拼的：PG 会给 UNIQUE 索引自动命名，而名字超过 63 字节会被截断。
//
// **它不认 pgx.ErrNoRows**：本包的读路径各自把「没这一行」翻成自己的错误值
// （ErrPlanNotFound / ErrMembershipNotFound …）——一个全局的「没有行就是套餐不存在」在这里
// 只会把别的说成别的。
func mapPGError(err error) error {
	if err == nil {
		return nil
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		switch pgErr.Code {
		case "23505": // unique_violation
			switch pgErr.ConstraintName {
			case "membership_plans_code_key":
				return ErrPlanCodeTaken
			case "memberships_user_unique":
				return ErrMembershipExists
			case "membership_subscriptions_live_unique":
				return ErrLiveSubscriptionExists
			case "membership_subscriptions_agreement_unique":
				return ErrAgreementTaken
			case "membership_changes_order_unique":
				return ErrDuplicateChange
			case "membership_changes_request_unique":
				// 同一次点击的第二次落库（004 那条 `(user_id, change_type, request_id)` 部分唯一
				// 索引）。正常路径上撞不到它：签约在事务里先锁了会员行、再反查过这个 requestId。
				// 撞上说明那次反查与这次插入之间还是被插了一行，而它的反面是**同一份协议签出两条
				// 订阅**——所以先当「已经记过了」，由调用方再查一次那条订阅是好是坏。
				return ErrDuplicateChange
			case "membership_campaigns_scene_unique":
				return ErrCampaignSceneTaken
			case "membership_campaign_claims_user_unique":
				return ErrCampaignAlreadyClaimed
			}
		case "23514": // check_violation
			// 走到这里说明**本服务自己**写出了一条违反不变式的行（不是用户输错了，那样的
			// 输入早在 service 层就被挡下来了）。原样往上抛，由 controller 记 500——包一层
			// 好听的业务错误只会把「我们的 bug」说成「你的请求不对」。
			return fmt.Errorf("invariant violated (%s): %w", pgErr.ConstraintName, err)
		}
	}
	return err
}

// isNoRows 判断「查不到这一行」。
//
// 单独包一层是为了让读路径的每一个调用点都写成同一句 `if isNoRows(err)`，而不是各自
// errors.Is(err, pgx.ErrNoRows)：后者散在十几处时，漏掉一处就是把「不存在」报成 500。
func isNoRows(err error) bool { return errors.Is(err, pgx.ErrNoRows) }

// optionalText 把空串变成 NULL，供可空列使用。
//
// VARCHAR 列传空串是合法的（它就是空串），所以这里的意义不在「能不能写进去」，而在**读出来
// 的东西能不能区分**：membership_changes.from_status 为空表示「之前没有这条会员」（开通那一次），
// 存成空串之后，「从空状态变成 active」与「有一个叫空字符串的状态」在下游看起来一模一样。
func optionalText(value string) *string {
	if value == "" {
		return nil
	}
	return &value
}

// optionalID 把空串变成 NULL，供 UUID 列使用。
//
// 与 optionalText 分开是必须的：空串对 UUID 列**不是一个合法的值**，传进去会报
// invalid input syntax for type uuid（一条 500），而不是被当成 NULL。operator_id、order_id
// 这些列在系统动作与老数据上经常是空的，所以这个转换在每个调用点上都不能漏。
func optionalID(id string) *string {
	if strings.TrimSpace(id) == "" {
		return nil
	}
	return &id
}

// trimOrEmpty 是各处在写库前对文本字段的统一处理：去掉首尾空白。
//
// 不 trim 的后果不是「多了几个空格」：`CHECK (char_length(trim(name)) > 0)` 会让一个全是
// 空格的输入在数据库层报 23514（一条 500），而它本该是一次 400「请填写名称」。
func trimOrEmpty(value string) string { return strings.TrimSpace(value) }

// jsonOrEmpty 是 JSONB 列的统一处理：空值写成 `{}`，不写成 NULL。
//
// 库上那些列都是 `JSONB NOT NULL DEFAULT '{}'`，但**显式写一个 NULL 会覆盖掉默认值**——
// 默认值只在「这一列压根不在 INSERT 的列清单里」时才生效。所以一条没带 metadata 的流水
// （开通那一次就没带）会撞一条 23502，而它报出来的是「metadata 不能为空」这种跟业务毫不相干
// 的话，真实原因是调用方没填。归一化放在这里而不是各调用方：写这一列的地方只有一处。
func jsonOrEmpty(value []byte) []byte {
	if len(value) == 0 {
		return []byte("{}")
	}
	return value
}

// mustJSON 把事件体序列化成 outbox 的 payload。
//
// 序列化不了就 panic 而不是回错误：这里的入参全是本包自己拼出来的结构体（没有 map、没有
// channel、没有循环引用），编不出来只可能是**代码写错了**。把它当成一次可重试的失败会让错误
// 在重试里循环，而当掉一次更早暴露。
func mustJSON(value any) []byte {
	payload, err := json.Marshal(value)
	if err != nil {
		panic("membership: outbox payload is not serializable: " + err.Error())
	}
	return payload
}

// utc 把时间统一成 UTC 再入库。
//
// 列是 TIMESTAMPTZ，PG 自己会存绝对时刻，所以这一步不影响存进去的值；它影响的是**读回来的
// 东西在各处的比较**——一个带本地时区的 time.Time 与一个 UTC 的相减结果是对的，但 fmt 出来
// 的字面量不同，排查时对不上眼。
func utc(value time.Time) time.Time { return value.UTC() }
