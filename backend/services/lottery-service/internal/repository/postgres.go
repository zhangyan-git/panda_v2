// Package repository 是抽奖库的数据访问层。
//
// 按聚合拆文件：activation.go 是开通（含默认活动与第一期的同事务创建）、campaign.go 是
// 活动与奖池、round.go 是期次的读取与开门、participation.go 是参与的三段事务、
// draw.go 是开奖那一把大锁、query.go 是各张列表的读路径。
//
// 这一份的 appendOutbox / recordWinEvent / mapPGError / inTx 与 payment-service、order-service、
// account-service 的同名函数几乎逐行一致——幂等与 outbox 在这个仓库里只有一种语义，
// 换服务时不该重新理解一遍。**唯一的差别是表名**。
package repository

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/panda-dev/panda-v2/backend/platform/audit"
	"github.com/panda-dev/panda-v2/backend/platform/messaging"
)

var (
	// ErrActivationNotFound：开通记录不存在（或那家门店没开通）。
	ErrActivationNotFound = errors.New("lottery activation not found")
	// ErrLocationAlreadyActivated：这家门店已经开通过抽奖。重复点击不是两笔业务，
	// 由 lottery_activations_location_unique 在数据库层挡。
	ErrLocationAlreadyActivated = errors.New("this location already has lottery enabled")
	// ErrCampaignNotFound：活动不存在。
	ErrCampaignNotFound = errors.New("lottery campaign not found")
	// ErrCampaignCodeTaken：活动短名被别的活动占了。它是期次号的前缀，全局唯一。
	ErrCampaignCodeTaken = errors.New("campaign code is already in use")
	// ErrDefaultCampaignExists：这个开通记录已经有默认活动了
	// （lottery_campaigns_one_default_per_activation）。默认活动不能有两个：抽奖中心
	// 得知道该显示哪一个。
	ErrDefaultCampaignExists = errors.New("this activation already has a default campaign")
	// ErrRoundNotFound：期次不存在。
	ErrRoundNotFound = errors.New("lottery round not found")
	// ErrRoundAlreadyLive：这个活动已经有一期在跑（lottery_rounds_one_live_per_campaign）。
	// 期次连开的地基就是这条：开下一期之前上一期必须已经 drawn 或 cancelled。
	ErrRoundAlreadyLive = errors.New("this campaign already has a live round")
	// ErrRoundClosed：期次不在收人窗口内（已达标关闭、已开奖、已作废、或已到点）。
	//
	// **它不是故障**：用户在扣卡通路上被开奖抢先了，什么也没得到，卡要原路退回去。
	ErrRoundClosed = errors.New("lottery round is not accepting participations")
	// ErrRoundChanged：人工开奖时页面上那份状态已经过期。回 409 让管理员刷新，
	// 而不是替一个已经变了的局面决定谁中奖。
	ErrRoundChanged = errors.New("lottery round changed since the page was loaded")
	// ErrRoundAlreadyDrawn：这一期已经开过奖了（lottery_draws_round_unique）。
	// 多副本 worker 不需要选主，全部依据就是这一条。
	ErrRoundAlreadyDrawn = errors.New("this round has already been drawn")
	// ErrRoundCancelled：这一期已经被作废了。与「已开奖」分开是因为对管理员来说这是两句
	// 不同的话：一个说他晚了一步，另一个说这一期根本不作数。
	ErrRoundCancelled = errors.New("this round was cancelled")
	// ErrRoundNotAwaitingDraw：扫描时该开奖、拿到锁时已经不该开了。
	//
	// **它不是故障**：另一个副本先开掉了、或者期次刚被人参与顶到 closed 又在别处被处理。
	// worker 把它当成「跳过这一条」，不记错误、不重试。
	ErrRoundNotAwaitingDraw = errors.New("the round no longer awaits a draw")
	// ErrParticipationCountDrift：期次上滚动维护的 participant_count 与参与记录的实际
	// 条数对不上。
	//
	// 它是**不变式被破坏**的信号，不是一种业务状态：开奖要用「参与集合自己的大小」去算
	// 种子，两个数不一致时算出来的种子不属于任何一份可复核的名单。宁可让这次开奖失败
	// （可查、可告警），也不要留下一份事后重算不出来的开奖记录。
	ErrParticipationCountDrift = errors.New("round participant_count drifted from the participation records")
	// ErrPrizeAllocationBroken：开奖算出来的名单与奖池对不上（空名单，或名额指向了
	// 不存在的档位）。算法是纯函数且被单测钉着，走到这里说明它被改坏了。
	ErrPrizeAllocationBroken = errors.New("prize allocation did not match the prize pool")
	// ErrDrawNotFound：开奖记录不存在。
	ErrDrawNotFound = errors.New("lottery draw not found")
	// ErrRoundHasParticipations：有参与者的期次不能作废。有参与者的作废需要一个 N 次
	// 跨服务冲正循环，属下一轮；今天只有 participant_count = 0 的期次能作废。
	ErrRoundHasParticipations = errors.New("a round with participations cannot be cancelled")
	// ErrIdempotencyKeyConflict：幂等键撞上了另一条参与记录。正常重放不会走到这里
	// （同一个键必然属于同一次参与），撞上说明调用方把号发重了。
	ErrIdempotencyKeyConflict = errors.New("idempotency key belongs to another participation")
	// ErrParticipationNotFound：参与记录不存在。
	ErrParticipationNotFound = errors.New("lottery participation not found")
	// ErrParticipationNotPending：要确认的那条参与已经不在 pending 了。
	//
	// 正常路径不会遇到它——service 在第二段之前会先看回读到的那一行。走到这里说明有人
	// 绕过那一步直接重跑了确认，而那会让期次的计数凭空多一个人。
	ErrParticipationNotPending = errors.New("lottery participation is not pending")
	// ErrMachineMismatch：设备级活动要求参与时报的就是那台设备。
	//
	// 它挡的是「从 A 店的码扫进 B 店的活动」这种真实错配，**挡不住**一个直接构造请求
	// 的客户端（客户端的自述本来就不可信，见 Begin 的注释）。
	ErrMachineMismatch = errors.New("source machine does not match the campaign")
	// ErrWinNotFound：中奖记录不存在。
	ErrWinNotFound = errors.New("lottery win not found")
	// ErrDuplicateWin：一次开奖里同一条参与中了两次（lottery_wins_participation_unique）。
	// 算法本身不会产生这种结果，走到这里说明同一次开奖被写了两遍。
	ErrDuplicateWin = errors.New("this participation already won in this draw")
)

// PostgresRepository 是抽奖库的数据访问实现。
//
// recorder 只在后台那几条**人工干预**路径上用（人工开奖、期次作废）：方案 11.6 把它们
// 列进必审清单，而审计走平台的 admin.operation.logged → 身份库的 admin_operation_logs，
// **本库不建自己的审计表**（见 [[audit-stays-in-identity-db-by-decision]]）。
type PostgresRepository struct {
	pool *pgxpool.Pool
	// recorder 为 nil 时用 audit.Noop：调用点的审计语句保持无条件执行，不留
	// 「忘了传 recorder 就没有审计」的分支。
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
// readX 这类函数既被写路径调用（在事务里读回刚写的行，否则读不到未提交的数据），也被
// 读路径调用（直接用池）。多写一份只会在两处之间产生漂移——而且漂移的表现是「同一条
// 记录在详情页和列表页字段不一样」，很难查。
type querier interface {
	QueryRow(context.Context, string, ...any) pgx.Row
	Query(context.Context, string, ...any) (pgx.Rows, error)
}

// newEventID 是 outbox 事件的 ID。抽成函数是为了让「一条事件一个 ID」只有一处实现。
func newEventID() string { return uuid.NewString() }

// inTx 跑一个事务，出错即回滚。
//
// 写路径必须整体成功或整体不动：开奖记录追加了而中奖名单没写，或者反过来，都会让
// 「lottery_draws 与 lottery_wins 一一对应」这条不变式当场失效，而那是复核一次开奖的
// 全部依据。
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
// 发出去，提交了的事件也不能因为 broker 当时不通就丢——一次开奖是终局，下游迟早要知道，
// 而「晚一点知道」是可以接受的，「永远不知道」不是。
func appendOutbox(ctx context.Context, tx pgx.Tx, eventType, eventVersion, traceID string, payload []byte) error {
	return messaging.NewPostgreSQLWithQuerier(tx).Append(ctx, messaging.Envelope{
		EventID:      newEventID(),
		EventType:    eventType,
		EventVersion: eventVersion,
		TraceID:      traceID,
		Payload:      payload,
	})
}

// recordWinEvent 追加一条中奖流水。这张表只追加，触发器挡改和挡删。
//
// metadata 为 nil 时写 '{}' 而不是 NULL：那一列是 NOT NULL，而「没有附加信息」与
// 「这一列没填」在查询里长得一样，不如统一成一个空对象。
func recordWinEvent(ctx context.Context, tx pgx.Tx, winID, eventType, fromStatus, toStatus, actorType string, actorID *string, actorName, reason string, metadata any) error {
	var encoded []byte
	if metadata != nil {
		var err error
		encoded, err = json.Marshal(metadata)
		if err != nil {
			return fmt.Errorf("encode win event metadata: %w", err)
		}
	} else {
		encoded = []byte("{}")
	}
	_, err := tx.Exec(ctx, `INSERT INTO lottery_win_events
		(win_id,event_type,from_status,to_status,actor_type,actor_id,actor_name,reason,metadata)
		VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9)`,
		winID, eventType, fromStatus, toStatus, actorType, actorID, actorName, reason, encoded)
	return err
}

// mapPGError 把唯一约束冲突翻成调用方分得清的业务错误。
//
// 没有它，「同一家门店点了两下开通」会变成 500「服务器错误」，而它其实是一个说得清楚的
// 幂等结论。约束名是这里唯一稳定可依赖的东西：靠错误文本匹配会在 PG 换语言或换措辞时
// 静默失效。
//
// 下面这些名字是**从 dev 库里读出来的实际值**（\d lottery_...），不是照 DDL 拼的：
// PG 会自动给 UNIQUE 约束命名，而名字超过 63 字节会被截断。照 DDL 拼会写成永不命中的
// case，只在并发下才走到——正是最不该静默失效的那条路。
func mapPGError(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrRoundNotFound
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		switch pgErr.ConstraintName {
		case "lottery_activations_location_unique":
			return ErrLocationAlreadyActivated
		case "lottery_campaigns_code_unique":
			return ErrCampaignCodeTaken
		case "lottery_campaigns_one_default_per_activation":
			return ErrDefaultCampaignExists
		case "lottery_rounds_one_live_per_campaign", "lottery_rounds_seq_unique", "lottery_rounds_no_unique":
			return ErrRoundAlreadyLive
		case "lottery_participations_key_unique":
			return ErrIdempotencyKeyConflict
		case "lottery_draws_round_unique":
			return ErrRoundAlreadyDrawn
		case "lottery_wins_participation_unique":
			return ErrDuplicateWin
		}
	}
	return err
}
