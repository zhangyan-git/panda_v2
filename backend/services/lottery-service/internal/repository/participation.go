package repository

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/panda-dev/panda-v2/backend/services/lottery-service/internal/model"
)

const participationColumns = `id::text, round_id::text, campaign_id::text, campaign_name,
	round_no, user_id::text, source_order_id::text, source_order_no,
	source_machine_id::text, source_location_id::text, cost, status, failure_code,
	fortune_entry_id::text, reverse_entry_id::text, attempts, last_error,
	idempotency_key, created_at, confirmed_at`

func scanParticipation(row scanner) (*model.Participation, error) {
	participation := &model.Participation{}
	err := row.Scan(&participation.ID, &participation.RoundID, &participation.CampaignID,
		&participation.CampaignName, &participation.RoundNo, &participation.UserID,
		&participation.SourceOrderID, &participation.SourceOrderNo,
		&participation.SourceMachineID, &participation.SourceLocationID,
		&participation.Cost, &participation.Status, &participation.FailureCode,
		&participation.FortuneEntryID, &participation.ReverseEntryID,
		&participation.Attempts, &participation.LastError,
		&participation.IdempotencyKey, &participation.CreatedAt, &participation.ConfirmedAt)
	if err != nil {
		return nil, err
	}
	return participation, nil
}

// BeginParams 是参与的第一段事务的输入。
//
// **没有 Amount**：一次参与扣几张由 service 定死（原型规则是一次一张）。让调用方传这个
// 数，等于让一个被篡改的小程序决定扣几张卡。
type BeginParams struct {
	RoundID          string
	UserID           string
	IdempotencyKey   string
	SourceOrderID    *string
	SourceOrderNo    string
	SourceMachineID  *string
	SourceLocationID *string
	Cost             int32
	// Now 由 service 传进来而不是在 SQL 里取 NOW()：「这一期还收不收人」是一次业务判定，
	// 而业务判定要能被测试决定它看的是哪个时刻。SQL 也在同一个事务里，两者相差微秒级。
	Now time.Time
}

// BeginResult 是第一段事务的结果。
type BeginResult struct {
	// Round 是锁内读到的那一期（拿它与 campaign 一起做入口判定）。
	Round    *model.Round
	Campaign *model.Campaign
	// Participation 是**已经落库的那一行**。Created 为 true 时它是这次新建的；
	// 为 false 时它是幂等键命中的那一行（上一次的），调用方据此回放而不是重新扣卡。
	Participation *model.Participation
	Created       bool
}

// Begin 记下一次参与的意图：锁住期次、判定还能不能收人、落一条 pending 记录。
//
// 它**不扣卡**。跨服务调用在任何 PG 事务之外（payment-service 的先例：一次跨服务往返
// 不该让数据库事务一直开着锁），所以顺序是「先落本地记录、再扣卡、再确认」三段。
//
// **幂等回放的判定排在期次可参与性判定之前**，这是这个函数的第二条入口：键已经见过就
// 原样回放那一行，根本不看期次还收不收人。反过来写会让一个**卡已经扣了**的用户在重发时
// 拿到 ErrRoundClosed ——他既看不到自己那条参与，也拿不回那张卡。期次校验挡的是**新的**
// 参与，不是一条早就存在的参与的回放。
//
// 锁是 FOR UPDATE 而不是普通的 SELECT：期次的行锁同时被开奖持有（draw.go 里同样
// FOR UPDATE），两者互斥。没有它，一个用户可以在「worker 已经取完参与名单、还没提交」
// 的窗口里被记进来，然后拿到一张不属于任何一期开奖的参与记录。回放那条路**不加锁**：
// 它只有读，没有可以被开奖插队的写入。
func (r *PostgresRepository) Begin(ctx context.Context, p BeginParams) (*BeginResult, error) {
	result := &BeginResult{}
	err := r.inTx(ctx, func(tx pgx.Tx) error {
		// 先按键查一次。这一步不加锁、也不能代替下面那次 ON CONFLICT：两个并发请求会
		// 一起走到 miss，最后仍然只有一个能插进去（唯一索引 + DO NOTHING）。它的作用
		// 是让「已经存在的参与」不必过期次那一关。
		existing, err := readParticipationByKey(ctx, tx, p.IdempotencyKey)
		switch {
		case err == nil:
			// 语义与插入撞唯一索引那条回放路径逐字相同：同一个键换了人仍然是事故。
			if existing.UserID != p.UserID {
				return ErrIdempotencyKeyConflict
			}
			// Round 从这一行自己的 round_id 读，而不是用调用方传的 p.RoundID：回放要
			// 说的是「你当初参与的是哪一期」，不是「你现在请求的是哪一期」。两者不同
			// （客户端拿着旧链接重发）时，回放的答案必须还是原来那一期。
			round, err := readRound(ctx, tx, existing.RoundID)
			if err != nil {
				return err
			}
			campaign, err := readCampaign(ctx, tx, round.CampaignID)
			if err != nil {
				return err
			}
			result.Round, result.Campaign, result.Created = round, campaign, false
			result.Participation = existing
			return nil
		case errors.Is(err, ErrParticipationNotFound):
			// 键没见过，是**新的**参与，往下走正常路径（期次校验 + 落库）。
		default:
			return err
		}

		round, err := lockRound(ctx, tx, p.RoundID)
		if err != nil {
			return err
		}
		if !round.AcceptsParticipation() {
			return ErrRoundClosed
		}
		campaign, err := readCampaign(ctx, tx, round.CampaignID)
		if err != nil {
			return err
		}
		// 设备级活动要求参与时报的就是那台设备。这是**本地能验的唯一一条**：我们不去问
		// 咖啡机服务「这台设备在哪家店」，那会是一次同步跨服务依赖，而它换来的是「客户端
		// 谎报自己在哪台机器前」——客户端的自述本来就不可信，换成服务端查询只是把它变成
		// 「客户端谎报机器 id」，挡不住真正想作弊的人。它挡的是「同一个用户从 A 店的码
		// 扫进 B 店的活动」这种真实的错配。
		if campaign.IsMachineScoped() {
			reported := ""
			if p.SourceMachineID != nil {
				reported = *p.SourceMachineID
			}
			if reported != *campaign.MachineID {
				return ErrMachineMismatch
			}
		}

		participationID := newEventID()
		tag, err := tx.Exec(ctx, `INSERT INTO lottery_participations
			(id,round_id,campaign_id,campaign_name,round_no,user_id,
			 source_order_id,source_order_no,source_machine_id,source_location_id,
			 cost,status,idempotency_key)
			VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,'pending',$12)
			ON CONFLICT (idempotency_key) DO NOTHING`,
			participationID, round.ID, campaign.ID, campaign.Name, round.RoundNo, p.UserID,
			p.SourceOrderID, p.SourceOrderNo, p.SourceMachineID, p.SourceLocationID,
			p.Cost, p.IdempotencyKey)
		if err != nil {
			return err
		}

		if tag.RowsAffected() == 1 {
			result.Round, result.Campaign, result.Created = round, campaign, true
			participation, err := readParticipation(ctx, tx, participationID)
			if err != nil {
				return err
			}
			result.Participation = participation
			return nil
		}

		// 唯一索引上撞了：只有当上面那次「按键先查」与这次插入之间**插进来另一个同样的键**
		// 时才会走到这里（正常路径上键已经见过，第一次查就返回了）。回读那一行，交给
		// service 决定是回放还是接着往下走（扣减本身幂等，所以一条 pending 的行重跑第二段
		// 是安全的）。
		raced, err := readParticipationByKey(ctx, tx, p.IdempotencyKey)
		if err != nil {
			return err
		}
		// 同一个键换了人 —— 只可能是调用方把号发重了（订单号撞车）。这不是幂等命中，
		// 是一次真的事故，必须让人看见，而不是把别人的参与记录回放给这个人。
		if raced.UserID != p.UserID {
			return ErrIdempotencyKeyConflict
		}
		result.Round, result.Campaign, result.Created = round, campaign, false
		result.Participation = raced
		return nil
	})
	if err != nil {
		return nil, mapPGError(err)
	}
	return result, nil
}

// ConfirmParams 是第三段事务的输入。
type ConfirmParams struct {
	ParticipationID string
	// FortuneEntryID 是 account-service 回的账变 ID。对账时从这一笔参与反查那次扣卡，
	// 所以它必须落库，而不是「反正扣成功了」。
	FortuneEntryID string
}

// Confirm 确认一次参与：期次计数 +1，参与记录转 confirmed。同一个事务。
//
// **参与数的 UPDATE 就是并发序列化点**。READ COMMITTED 下 UPDATE 会等行锁、然后重读
// 最新的行版本再算 SET，所以两个并发确认看到的是 9→10 和 10→11，第二个把期次置 closed
// ——没有「先读后写」的窗口。达标即 closed（停止收人），与原型
// soldCount = min(threshold, soldCount+1) 的饱和行为一致。
//
// 影响 0 行 = 期次在扣卡飞行途中关了或开了奖。返回 ErrRoundClosed，调用方走补偿
// （退卡 + 把这条参与标成 failed），**不是**把参与算进去。
func (r *PostgresRepository) Confirm(ctx context.Context, p ConfirmParams) (*model.Round, bool, error) {
	var round *model.Round
	var closed bool
	err := r.inTx(ctx, func(tx pgx.Tx) error {
		// 不加 FOR UPDATE 地读 round_id：这一列写下去就不变了，而给它加行锁会**造成死锁**
		// ——开奖那边持有期次行锁、再插入 lottery_wins 时对参与行取 FOR KEY SHARE，而
		// FOR UPDATE 与 FOR KEY SHARE 互斥。两边锁的顺序一致（都先期次后参与）才没有环。
		var roundID string
		if err := tx.QueryRow(ctx, `SELECT round_id::text FROM lottery_participations
			WHERE id=$1`, p.ParticipationID).Scan(&roundID); err != nil {
			return err
		}

		// 先改参与、后期次。这一句的 WHERE status='pending' 是**重放保护**：修复 worker
		// 可能在上一次「C 已经提交、但返回前进程死了」之后重跑，那时计数已经加过了，
		// 无条件再跑一次会让期次凭空多一个人。
		tag, err := tx.Exec(ctx, `UPDATE lottery_participations
			SET status='confirmed', confirmed_at=NOW(), fortune_entry_id=$2, last_error=NULL
			WHERE id=$1 AND status='pending'`, p.ParticipationID, nullUUID(p.FortuneEntryID))
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return ErrParticipationNotPending
		}

		tag, err = tx.Exec(ctx, `UPDATE lottery_rounds
			SET participant_count = participant_count + 1,
			    status = CASE WHEN participant_count + 1 >= participant_target THEN 'closed' ELSE status END,
			    updated_at = NOW()
			WHERE id=$1 AND status='open'`, roundID)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			// 期次在扣卡飞行途中关了或开了奖。整个事务回滚——上面那一句把参与标成
			// confirmed 的改动**跟着回滚**，所以补偿路径看到的仍然是一条 pending 的行。
			return ErrRoundClosed
		}

		round, err = readRound(ctx, tx, roundID)
		if err != nil {
			return err
		}
		closed = round.Status == model.RoundClosed
		return nil
	})
	if err != nil {
		return nil, false, mapPGError(err)
	}
	return round, closed, nil
}

// FailParams 把一次参与标成失败。
type FailParams struct {
	ParticipationID string
	FailureCode     string
	// ReverseEntryID 只在「已经扣成功了、但要还回去」的补偿路径上有值。为空 = 这一次
	// 扣卡根本没发生（余额不足、请求非法）。
	ReverseEntryID *string
}

// Fail 把一条参与标成 failed。
//
// 状态用 failed 而不是 reversed：这个 CHECK 里的两个词是给**两条不同的路**用的——
//   - failed：这一笔从来有效过（余额不足、期次关了、补偿退卡之后）。它不计入期次，
//     对用户来说是「没参与上」。
//   - reversed：这一笔曾经有效（confirmed 过、进过开奖池），之后因为退款等原因被追回。
//     那是下一轮的事（见 service 包注释里那条已经写下的规则）。
//
// 把补偿路径写成 reversed 会让「这一期有多少人真的参与过」这个数在事后无法回答。
func (r *PostgresRepository) Fail(ctx context.Context, p FailParams) error {
	tag, err := r.pool.Exec(ctx, `UPDATE lottery_participations
		SET status='failed', failure_code=$2, reverse_entry_id=$3
		WHERE id=$1 AND status IN ('pending','confirmed')`, p.ParticipationID, p.FailureCode, p.ReverseEntryID)
	if err != nil {
		return mapPGError(err)
	}
	if tag.RowsAffected() == 0 {
		// 已经是终态（failed / reversed）了。补偿路径会被重跑——修复 worker 不知道
		// 上一次补偿走没走到最后一步，所以这里是幂等的无操作而不是错误。
		return nil
	}
	return nil
}

// NoteAttempt 记下一次「扣减没得出结论」的尝试。只用于观测，不改状态。
//
// **不设重试上限**：传输层一直失败的行是「真的不知道扣没扣」，判 failed 就是撒谎——
// 用户可能已经被扣了卡却什么也得不到。attempts 与 last_error 是给人看的（超过一小时
// 仍未决就该告警），不是判据。
func (r *PostgresRepository) NoteAttempt(ctx context.Context, participationID string, cause error) error {
	message := ""
	if cause != nil {
		message = cause.Error()
	}
	_, err := r.pool.Exec(ctx, `UPDATE lottery_participations
		SET attempts = attempts + 1, last_error=$2 WHERE id=$1`, participationID, message)
	return err
}

// PendingParticipations 取卡在 pending 且已经老过 repairAfter 的参与记录 id。
//
// 60 秒的下限不是「等它一下」：一段正常的三段事务是毫秒级的，一分钟还停在 pending 只
// 可能来自「进程在第二段中途死了」或「扣减调用挂住了」。等待时间越长，用户的卡被扣了
// 却没进期次的窗口越大。
//
// 上界由 service 给（超时未决要告警而不是无限重试）。
func (r *PostgresRepository) PendingParticipations(ctx context.Context, repairAfter time.Duration, limit int) ([]string, error) {
	rows, err := r.pool.Query(ctx, `SELECT id::text FROM lottery_participations
		WHERE status='pending' AND created_at < NOW() - $1::interval
		ORDER BY created_at
		LIMIT $2`, repairAfter.String(), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// GetParticipation 按 id 读一条参与记录。
func (r *PostgresRepository) GetParticipation(ctx context.Context, id string) (*model.Participation, error) {
	return readParticipation(ctx, r.pool, id)
}

// CountUserParticipations 数一个用户在某一期参与了几次（含 pending）。
//
// 抽奖中心的「我已参与 3 次」用它。**含 pending**：用户刚点完那一下就该看到计数涨了，
// 而不是等扣卡往返回来才涨——那一瞬间的等待不该显示成一个没生效的点击。
func (r *PostgresRepository) CountUserParticipations(ctx context.Context, roundID, userID string) (int32, error) {
	var count int32
	err := r.pool.QueryRow(ctx, `SELECT COUNT(*)::int FROM lottery_participations
		WHERE round_id=$1 AND user_id=$2 AND status IN ('pending','confirmed')`, roundID, userID).Scan(&count)
	return count, err
}

func readParticipation(ctx context.Context, q querier, id string) (*model.Participation, error) {
	return wrapParticipationRow(q.QueryRow(ctx, `SELECT `+participationColumns+`
		FROM lottery_participations WHERE id=$1`, id))
}

func readParticipationByKey(ctx context.Context, q querier, key string) (*model.Participation, error) {
	return wrapParticipationRow(q.QueryRow(ctx, `SELECT `+participationColumns+`
		FROM lottery_participations WHERE idempotency_key=$1`, key))
}

func wrapParticipationRow(row pgx.Row) (*model.Participation, error) {
	participation, err := scanParticipation(row)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrParticipationNotFound
		}
		return nil, err
	}
	return participation, nil
}

// nullUUID 把一个可能为空的 uuid 字符串转成 SQL 里可用的值。
//
// 空串不能直接进 uuid 列（会报 invalid input syntax），而调用方手里是「有值 / 没值」的
// 字符串而不是指针。NULLIF 在 SQL 里做这件事比在 Go 里来回转指针清楚。
func nullUUID(value string) any {
	if value == "" {
		return nil
	}
	return value
}
