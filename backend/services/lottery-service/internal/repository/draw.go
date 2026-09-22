package repository

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/panda-dev/panda-v2/backend/platform/audit"
	"github.com/panda-dev/panda-v2/backend/services/lottery-service/internal/draw"
	"github.com/panda-dev/panda-v2/backend/services/lottery-service/internal/dto"
	"github.com/panda-dev/panda-v2/backend/services/lottery-service/internal/model"
)

// roundAuditSnapshot 是期次写进审计的那几个字段。
//
// 不用 model.Round：它只有 db tag，快照出来是一串大写的列名；participant_target 也不进
// ——它是开期那一刻从活动冻结下来的，开奖与作废都改不动它。
//
// **不含中奖名单**：名单在 lottery_wins 里、按 draw_id 就能捞出完整的一份，而这一期可能
// 开出几十上百个中奖人，塞进日志只会让后台那个「查看详情」弹窗糊成一片。种子与参与数是
// 复核名单所需要的全部输入（见 docs/architecture.md「开奖可复核但不可证明公平」）。
type roundAuditSnapshot struct {
	RoundNo          string     `json:"roundNo"`
	CampaignID       string     `json:"campaignId"`
	Status           string     `json:"status"`
	ParticipantCount int32      `json:"participantCount"`
	WinnerCount      int32      `json:"winnerCount"`
	DrawnAt          *time.Time `json:"drawnAt,omitempty"`
	CancelledAt      *time.Time `json:"cancelledAt,omitempty"`
	CancelReason     string     `json:"cancelReason,omitempty"`
	// NextRoundNo 只在「作废」那一条审计上有：作废会在同一事务里补开下一期，而
	// 「作废之后活动还有没有在跑的期次」是事后最常被问的一句。为空表示没补开（活动不在
	// enabled，见 rollCampaign）。
	NextRoundNo string `json:"nextRoundNo,omitempty"`
}

func roundSnapshotOf(r *model.Round) roundAuditSnapshot {
	if r == nil {
		return roundAuditSnapshot{}
	}
	return roundAuditSnapshot{
		RoundNo: r.RoundNo, CampaignID: r.CampaignID, Status: r.Status,
		ParticipantCount: r.ParticipantCount, WinnerCount: r.WinnerCount,
		DrawnAt: r.DrawnAt, CancelledAt: r.CancelledAt, CancelReason: r.CancelReason,
	}
}

// drawAuditSnapshot 是一条开奖记录写进审计的那几个字段。
//
// RoundNo 从期次上取：lottery_draws 上没有这一列（它按 round_id 关联），而日志上写
// 「LA1B2C3D4-0003 这一期开了」比写一个 UUID 有用得多。
type drawAuditSnapshot struct {
	RoundNo          string `json:"roundNo"`
	Mode             string `json:"mode"`
	Trigger          string `json:"trigger"`
	Algorithm        string `json:"algorithm"`
	Seed             string `json:"seed"`
	ParticipantCount int32  `json:"participantCount"`
	WinnerCount      int32  `json:"winnerCount"`
	Reason           string `json:"reason,omitempty"`
}

func drawSnapshotOf(d *model.Draw, round *model.Round) drawAuditSnapshot {
	s := drawAuditSnapshot{
		Mode: d.Mode, Trigger: d.Trigger, Algorithm: d.Algorithm,
		Seed: d.Seed, ParticipantCount: d.ParticipantCount, WinnerCount: d.WinnerCount,
		Reason: d.Reason,
	}
	if round != nil {
		s.RoundNo = round.RoundNo
	}
	return s
}

const winColumns = `id::text, draw_id::text, round_id::text, campaign_id::text,
	participation_id::text, user_id::text, round_no, campaign_name, prize_id::text,
	original_prize_name, current_prize_name, claim_no, status,
	testimonial, testimonial_images, source_order_id::text, source_order_no,
	source_machine_id::text, source_location_id::text,
	expires_at, claimed_at, redeemed_at, redeemed_by::text, redeem_location_id::text,
	redeem_location_name, created_at, updated_at`

func scanWin(row scanner) (*model.Win, error) {
	win := &model.Win{}
	err := row.Scan(&win.ID, &win.DrawID, &win.RoundID, &win.CampaignID,
		&win.ParticipationID, &win.UserID, &win.RoundNo, &win.CampaignName,
		&win.PrizeID, &win.OriginalPrizeName, &win.CurrentPrizeName,
		&win.ClaimNo, &win.Status, &win.Testimonial, &win.TestimonialImages,
		&win.SourceOrderID, &win.SourceOrderNo, &win.SourceMachineID, &win.SourceLocationID,
		&win.ExpiresAt, &win.ClaimedAt, &win.RedeemedAt, &win.RedeemedBy,
		&win.RedeemLocationID, &win.RedeemLocationName, &win.CreatedAt, &win.UpdatedAt)
	if err != nil {
		return nil, err
	}
	return win, nil
}

// DrawParams 是一次开奖的输入。
type DrawParams struct {
	RoundID string
	// auto / manual，与 lottery_draws.mode 的 CHECK 逐字一致。
	Mode string
	// DrawnBy / Reason 只在人工开奖时有值，且**两者都必填**（数据库 CHECK 也挡）。
	DrawnBy *string
	Reason  string
	// Now 由调用方传（服务层注入的时钟），用于开奖事件里的 DrawnAtUnix。
	Now time.Time
	// TraceID 进 outbox 事件的信封，把一次开奖与它的上游请求串起来。
	TraceID string

	// 下面两个只在人工开奖时有效，是**乐观并发校验**：管理员拿着一个页面点了开奖，而那个
	// 页面可能是三十秒前加载的。对不上就回 409，而不是替一个已经变了的局面决定谁中奖。
	// 为空时跳过该条校验（自动开奖不看这两个）。
	ExpectedStatus           string
	ExpectedParticipantCount *int32
}

// DrawOutcome 是一次开奖的结果。
type DrawOutcome struct {
	// Draw 为 nil 表示这一期**零人参与**，按作废处理（没有开奖记录）。
	Draw *model.Draw
	// Round 是开奖之后的期次（drawn 或 cancelled）。
	Round *model.Round
	// Winners 是实际开出的中奖记录，本轮恒为 pending。
	Winners []*model.Win
	// NextRound 是同事务里开出来的下一期；活动不在 enabled（被暂停/结束/还是草稿）时为
	// nil。周期里不会再有「窗口过了」这个原因——活动的窗口已经删掉了。
	NextRound *model.Round
}

// DrawRound 开奖。**整个函数的正确性依赖那把行锁**：它先锁住期次行，锁内重新判定状态，
// 再取参与名单、算种子、写记录。同一期的两个并发调用（两个副本的 worker）里，后到的那个
// 会看到 status 已经是 drawn 并退出——这就是「多副本不需要选主」的全部依据，兜底是
// lottery_draws_round_unique。
//
// 它也与参与确认互斥（那边同样更新期次行），所以不存在「名单取完之后又有人被记进来」
// 的窗口。
func (r *PostgresRepository) DrawRound(ctx context.Context, p DrawParams) (*DrawOutcome, error) {
	outcome := &DrawOutcome{}
	err := r.inTx(ctx, func(tx pgx.Tx) error {
		round, err := lockRound(ctx, tx, p.RoundID)
		if err != nil {
			return err
		}
		campaign, err := readCampaign(ctx, tx, round.CampaignID)
		if err != nil {
			return err
		}

		mode, trigger, err := resolveDrawMode(round, p)
		if err != nil {
			return err
		}

		// 零人参与：直接作废，**不写开奖记录**。
		//
		// 不流局（流局要把 N 张卡沿 N 次跨服务冲正还回去，而那条路没有截止时间；
		// 「参与后不可撤回」也是原型的明文规则），但零人参与连一张卡都没扣过，开一次
		// 没有名单的奖只会在后台留下一条谁也看不懂的记录。拿掉到点开奖之后，这一条只在
		// **人工开奖**一条路上会遇到——自动开奖只会扫到已经收满的期次，那种期次不可能
		// 是零人参与。
		if round.ParticipantCount == 0 {
			cancelledBy, cancelReason := drawActor(mode, p)
			if _, err := tx.Exec(ctx, `UPDATE lottery_rounds
				SET status='cancelled', cancelled_at=NOW(), cancelled_by=$2,
				    cancel_reason=$3, updated_at=NOW()
				WHERE id=$1`, round.ID, cancelledBy, cancelReason); err != nil {
				return err
			}
			if outcome.Round, err = readRound(ctx, tx, round.ID); err != nil {
				return err
			}
			// 这一条也是人工动作的后果（零人参与只有人工开奖这条路会遇到），而它没有开奖
			// 记录——不记审计的话，「这一期为什么没了」在库里查不到任何解释。
			if err := r.recorder.Record(ctx, tx, audit.Entry{
				Module: "lottery_rounds", Action: "cancel", Operation: "零人参与，作废期次",
				TargetType: "lottery_round", TargetID: round.ID, TargetName: round.RoundNo,
				Before: audit.Snapshot(roundSnapshotOf(round)),
				After:  audit.Snapshot(roundSnapshotOf(outcome.Round)),
			}); err != nil {
				return err
			}
			outcome.NextRound, err = rollCampaign(ctx, tx, campaign)
			return err
		}

		ids, err := confirmedParticipationIDs(ctx, tx, round.ID)
		if err != nil {
			return err
		}
		prizes, err := readPrizes(ctx, tx, round.CampaignID)
		if err != nil {
			return err
		}
		// participant_count 是滚动维护的列，这里再数一遍以保证与参与记录一致：种子和开奖
		// 记录里的 participant_count 必须是**参与集合自己的大小**，否则「记录的种子 +
		// 记录的参与集合 = 记录的中奖名单」这条等式在两处不一致时就不成立。
		if int32(len(ids)) != round.ParticipantCount {
			return ErrParticipationCountDrift
		}

		seed := draw.DeriveSeed(draw.SeedInput{
			RoundID:              round.ID,
			FirstParticipationID: ids[0],
			LastParticipationID:  ids[len(ids)-1],
			ParticipantCount:     len(ids),
			Trigger:              trigger,
		})
		prizeInputs := make([]draw.Prize, 0, len(prizes))
		for _, prize := range prizes {
			prizeInputs = append(prizeInputs, draw.Prize{ID: prize.ID, Quantity: int(prize.Quantity)})
		}
		winners := draw.SelectWinners(seed, ids, prizeInputs)

		drawID := newEventID()
		tag, err := tx.Exec(ctx, `INSERT INTO lottery_draws
			(id,round_id,campaign_id,mode,trigger,algorithm,seed,participant_count,winner_count,drawn_by,reason)
			VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)
			ON CONFLICT (round_id) DO NOTHING`,
			drawID, round.ID, round.CampaignID, mode, trigger, draw.SeedAlgorithmFor(),
			seed, len(ids), len(winners), p.DrawnBy, p.Reason)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			// 锁与状态判定已经挡过一次，走到这里说明有人绕开了它们。
			// 宁可不写名单，也不能给同一期开出两份互相矛盾的奖。
			return ErrRoundAlreadyDrawn
		}

		record, err := readDraw(ctx, tx, drawID)
		if err != nil {
			return err
		}
		winList, err := insertWins(ctx, tx, record, round, campaign, prizes, winners, mode)
		if err != nil {
			return err
		}

		if _, err := tx.Exec(ctx, `UPDATE lottery_rounds
			SET status='drawn', drawn_at=NOW(), updated_at=NOW() WHERE id=$1`, round.ID); err != nil {
			return err
		}

		// 事件与开奖记录同事务：一次开奖是终局，下游（将来的退款追回、看板）迟早要知道，
		// 「晚一点」可以接受，「永远不知道」不行。今天没有消费者，照样发——下一轮接线的
		// 人不必回头补一次历史回放。
		payload, err := dto.MarshalRoundDrawn(dto.RoundDrawnPayload{
			DrawID:           record.ID,
			RoundID:          round.ID,
			RoundNo:          round.RoundNo,
			CampaignID:       round.CampaignID,
			Mode:             mode,
			Trigger:          trigger,
			Seed:             seed,
			Algorithm:        draw.SeedAlgorithmFor(),
			ParticipantCount: int32(len(ids)),
			WinnerCount:      int32(len(winners)),
			DrawnAtUnix:      p.Now.Unix(),
			OperatorID:       p.DrawnBy,
		})
		if err != nil {
			return err
		}
		// **只记人工开奖**：自动开奖（trigger=threshold）是 worker 按门槛扫出来的系统动作，
		// 每一期都记会把真正要看的几条淹掉，而它自己的痕迹在 lottery_draws 里（那张表有种子、
		// 参与数与中奖名单，是只增的）。这条界线与会员域一致：系统与用户自己的动作不记审计，
		// 人工干预才记。它也是「审计表里不该出现没有操作人的行」这条规矩的落地。
		if mode == model.DrawModeManual {
			if err := r.recorder.Record(ctx, tx, audit.Entry{
				Module: "lottery_rounds", Action: "draw", Operation: "人工开奖",
				TargetType: "lottery_draw", TargetID: record.ID, TargetName: round.RoundNo,
				After: audit.Snapshot(drawSnapshotOf(record, round)),
			}); err != nil {
				return err
			}
		}
		if err := appendOutbox(ctx, tx, dto.EventRoundDrawn, dto.EventVersion, p.TraceID, payload); err != nil {
			return err
		}

		outcome.Draw, outcome.Winners = record, winList
		if outcome.Round, err = readRound(ctx, tx, round.ID); err != nil {
			return err
		}
		outcome.NextRound, err = rollCampaign(ctx, tx, campaign)
		return err
	})
	if err != nil {
		return nil, mapPGError(err)
	}
	return outcome, nil
}

// resolveDrawMode 在锁内决定这次开奖是自动还是人工、触发是哪一种，并做人工开奖的乐观校验。
//
// 自动那一路**重新判定**一次而不信调用方的 trigger：「为什么现在开这一期」是事实，不是
// 标签。扫描结果可能是过期的——扫描完到拿到锁之间，期次可能已经被人开掉了，所以还要在锁内
// 确认它现在**确实收满了**（status 是 closed）。
func resolveDrawMode(round *model.Round, p DrawParams) (mode, trigger string, err error) {
	if p.Mode == model.DrawModeManual {
		if p.ExpectedStatus != "" && p.ExpectedStatus != round.Status {
			return "", "", ErrRoundChanged
		}
		if p.ExpectedParticipantCount != nil && *p.ExpectedParticipantCount != round.ParticipantCount {
			return "", "", ErrRoundChanged
		}
		switch round.Status {
		case model.RoundDrawn:
			// 人工开奖拦在锁内，而不是靠调用方先读一遍。
			return "", "", ErrRoundAlreadyDrawn
		case model.RoundCancelled:
			return "", "", ErrRoundCancelled
		}
		return model.DrawModeManual, model.TriggerManual, nil
	}

	ok, trigger := round.AwaitingDraw()
	if !ok {
		// 扫描到现在期次已经不满足条件了（刚被别人开掉）。worker 把这一条当成「跳过」，
		// 不是错误。
		return "", "", ErrRoundNotAwaitingDraw
	}
	return model.DrawModeAuto, trigger, nil
}

// drawActor 决定这一期是谁作废的、理由写什么。
//
// 人工开奖时用操作员与理由（数据库的 CHECK 要求 manual 时两者都有值）；自动开奖没有操作
// 人，理由由系统写一句实话——「零人参与」是这一期作废的唯一原因。
func drawActor(mode string, p DrawParams) (actor *string, reason string) {
	if mode == model.DrawModeManual {
		return p.DrawnBy, p.Reason
	}
	return nil, "零人参与，未开奖"
}

// confirmedParticipationIDs 取这一期全部已确认的参与，按 id 升序。
//
// 排序是**算法的一部分**：种子取头尾两个 id，名单按分数排但同分按 id 兜底，两者都要求
// 这是一个稳定的全序。ORDER BY id 让同一份数据在重算时得到同一个序列。
func confirmedParticipationIDs(ctx context.Context, tx pgx.Tx, roundID string) ([]string, error) {
	rows, err := tx.Query(ctx, `SELECT id::text FROM lottery_participations
		WHERE round_id=$1 AND status='confirmed' ORDER BY id`, roundID)
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
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(ids) == 0 {
		// 锁内读到的 participant_count 非零却没有一条 confirmed：两个数漂移了。这时开奖
		// 会用一个空集合（种子取不到头尾 id），所以宁可不写，让它变成一次查得出来的失败。
		return nil, ErrParticipationCountDrift
	}
	return ids, nil
}

// insertWins 把中奖名单落库，并为每条写一条 created 流水。
//
// claim_no 由列默认值从 lottery_claim_no_seq 生成，**不在 Go 侧拼**：序号要连续、要能被
// 人念出来，只能有一个发号人。
func insertWins(ctx context.Context, tx pgx.Tx, record *model.Draw, round *model.Round,
	campaign *model.Campaign, prizes []*model.CampaignPrize,
	winners []draw.Winner, mode string) ([]*model.Win, error) {

	if len(winners) == 0 {
		// 参与集合非空，所以这只能是「奖池名额被算成了 0」——那意味着这一期开出一个谁也
		// 没中奖的名单，而活动配置是有名额的。宁可失败也不要留下这种记录。
		return nil, ErrPrizeAllocationBroken
	}

	// 中奖记录要抄一份来源快照（订单、点位），所以得把参与行读出来。用一条 IN 查询而不是
	// 逐条读：一个百人期的中奖名单可能有几十条。
	byID, err := participationsByID(ctx, tx, winnerParticipationIDs(winners))
	if err != nil {
		return nil, err
	}

	actorType := model.ActorSystem
	if mode == model.DrawModeManual {
		actorType = model.ActorAdmin
	}

	created := make([]*model.Win, 0, len(winners))
	for _, winner := range winners {
		if winner.PrizeIndex >= len(prizes) {
			// 算法保证不会发生（名额按奖池分完）。真发生了宁可少发一个奖也不发错档。
			return nil, ErrPrizeAllocationBroken
		}
		participation := byID[winner.ParticipationID]
		if participation == nil {
			return nil, ErrParticipationNotFound
		}
		prize := prizes[winner.PrizeIndex]
		win, err := scanWin(tx.QueryRow(ctx, `INSERT INTO lottery_wins
			(draw_id,round_id,campaign_id,participation_id,user_id,round_no,campaign_name,
			 prize_id,original_prize_name,current_prize_name,
			 source_order_id,source_order_no,source_machine_id,source_location_id)
			VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$9,$10,$11,$12,$13)
			RETURNING `+winColumns,
			record.ID, round.ID, round.CampaignID, participation.ID, participation.UserID,
			round.RoundNo, campaign.Name, prize.ID, prize.Name,
			participation.SourceOrderID, participation.SourceOrderNo,
			participation.SourceMachineID, participation.SourceLocationID))
		if err != nil {
			return nil, err
		}
		if err := recordWinEvent(ctx, tx, win.ID, model.EventWinCreated, "", model.WinPending,
			actorType, record.DrawnBy, "", record.Reason, map[string]any{
				"drawId":  record.ID,
				"roundNo": round.RoundNo,
				"prizeId": prize.ID,
			}); err != nil {
			return nil, err
		}
		created = append(created, win)
	}
	return created, nil
}

// winnerParticipationIDs 去重后返回中奖名单里的参与 id（发奖查询用）。
//
// 去重不是多余的防御：现在的算法给每条参与只发一个奖，但那是当前规则不是结构约束；将来
// 变成「一个人在同一次开奖里中两档」时，这里少一条 IN 参数只是白查一次，不会出错。
func winnerParticipationIDs(winners []draw.Winner) []string {
	seen := make(map[string]struct{}, len(winners))
	out := make([]string, 0, len(winners))
	for _, winner := range winners {
		if _, ok := seen[winner.ParticipationID]; ok {
			continue
		}
		seen[winner.ParticipationID] = struct{}{}
		out = append(out, winner.ParticipationID)
	}
	return out
}

func participationsByID(ctx context.Context, tx pgx.Tx, ids []string) (map[string]*model.Participation, error) {
	if len(ids) == 0 {
		return map[string]*model.Participation{}, nil
	}
	rows, err := tx.Query(ctx, `SELECT `+participationColumns+`
		FROM lottery_participations WHERE id = ANY($1::uuid[])`, ids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make(map[string]*model.Participation, len(ids))
	for rows.Next() {
		participation, err := scanParticipation(rows)
		if err != nil {
			return nil, err
		}
		out[participation.ID] = participation
	}
	return out, rows.Err()
}

// rollCampaign 在开奖的同事务里决定这个活动的下一步：接着开下一期，还是什么都不做。
//
// 两条分支：
//   - enabled → 开下一期（期次连开，原型里 LAKE-202608-12 就是这个形状）
//   - paused / draft / ended → 什么都不做。**暂停停的是下一期**：正在跑的那一期照常开奖
//     （已经收了 N 个人的参与，不能因为运营点了暂停就把他们的卡吞掉），而暂停状态下不再
//     自动开新期——恢复时由 EnsureLiveRound 补上。
//
// 这里曾经还有第三条「enabled 但活动窗口已过 → 活动置 ended」。2026-09-15 随活动窗口一起
// 删掉了：活动不再有截止时间，所以它**只会被人为结束**（service.SetCampaignStatus），不会
// 在一期开奖的当口被系统自己停掉。
func rollCampaign(ctx context.Context, tx pgx.Tx, campaign *model.Campaign) (*model.Round, error) {
	if campaign.Status != model.CampaignEnabled {
		return nil, nil
	}
	seq, err := nextSeq(ctx, tx, campaign.ID)
	if err != nil {
		return nil, err
	}
	return OpenNextRound(ctx, tx, campaign, seq)
}

// nextSeq 是活动的下一个期次序号。
//
// MAX+1 而不是 COUNT+1：期次号是给人看的，作废过的期次号不能回收——「第 12 期作废了、下一个
// 又变成第 12 期」会让客服对着两条不同的记录念同一个号。
func nextSeq(ctx context.Context, tx pgx.Tx, campaignID string) (int32, error) {
	var seq int32
	err := tx.QueryRow(ctx, `SELECT COALESCE(MAX(seq),0) + 1 FROM lottery_rounds
		WHERE campaign_id=$1`, campaignID).Scan(&seq)
	return seq, err
}

// EnsureLiveRound 保证一个活动有在跑的一期；没有就开一期。
//
// 它只在**恢复被暂停的活动**时用（service.ResumeCampaign）：暂停期间开奖不开新期，所以
// 一个暂停后恢复的活动会处于「enabled 但没有在跑的期次」——没有这一步，恢复就成了一次
// 什么也不做的点击，抽奖中心会一直显示「暂无进行中的活动」。
//
// 锁活动行：两个管理员同时点恢复不该各开出一期（真开出来会被
// lottery_rounds_one_live_per_campaign 挡住，但那是一次谁也看不懂的 409）。
func (r *PostgresRepository) EnsureLiveRound(ctx context.Context, campaignID string) (*model.Round, error) {
	var round *model.Round
	err := r.inTx(ctx, func(tx pgx.Tx) error {
		var lockedID string
		if err := tx.QueryRow(ctx, `SELECT id::text FROM lottery_campaigns
			WHERE id=$1 FOR UPDATE`, campaignID).Scan(&lockedID); err != nil {
			return err
		}
		campaign, err := readCampaign(ctx, tx, lockedID)
		if err != nil {
			return err
		}
		if campaign.Status != model.CampaignEnabled {
			return nil
		}
		var live bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM lottery_rounds
			WHERE campaign_id=$1 AND status IN ('open','closed'))`, campaignID).Scan(&live); err != nil {
			return err
		}
		if live {
			return nil
		}
		// 期次序号接着 MAX(seq) 走：一个活动被暂停又恢复，不该回头把第 5 期再开一遍。
		seq, err := nextSeq(ctx, tx, campaignID)
		if err != nil {
			return err
		}
		round, err = OpenNextRound(ctx, tx, campaign, seq)
		return err
	})
	if err != nil {
		return nil, mapPGError(err)
	}
	return round, nil
}

// CancelRound 作废一期，并在**同一个事务里开出下一期**。**只允许零人参与的期次**。
//
// 有参与者的作废需要一个 N 次跨服务冲正循环，属下一轮；今天拦在这里，而不是让它写一个
// 「作废了但卡没退」的状态——那正是用户第二天来投诉的东西。
//
// 补开下一期不是锦上添花，是这个动作**说得通的前提**：作废之后活动就没有在跑的期次了，
// 而除了「暂停 → 恢复」再没有任何东西会去开一期（自动开期那一句 rollCampaign 只在开奖时
// 跑），活动会一直显示成「启用中、却没有进行中的期次」。后台那个确认框里写的
// 「作废后会立刻开出下一期」在此之前是**说的和做的不一样**——今天补上。
//
// 注意调用方拿到的是**被作废的那一期**（客户端要看到它变成 cancelled），下一期在
// outcome 里不单独回传：期次列表重取即可。
func (r *PostgresRepository) CancelRound(ctx context.Context, roundID, reason string, actor *string) (*model.Round, error) {
	var round *model.Round
	err := r.inTx(ctx, func(tx pgx.Tx) error {
		locked, err := lockRound(ctx, tx, roundID)
		if err != nil {
			return err
		}
		switch locked.Status {
		case model.RoundDrawn:
			return ErrRoundAlreadyDrawn
		case model.RoundCancelled:
			return ErrRoundCancelled
		}
		if locked.ParticipantCount != 0 {
			return ErrRoundHasParticipations
		}
		if _, err := tx.Exec(ctx, `UPDATE lottery_rounds
			SET status='cancelled', cancelled_at=NOW(), cancelled_by=$2,
			    cancel_reason=$3, updated_at=NOW()
			WHERE id=$1`, roundID, actor, reason); err != nil {
			return err
		}
		campaign, err := readCampaign(ctx, tx, locked.CampaignID)
		if err != nil {
			return err
		}
		next, err := rollCampaign(ctx, tx, campaign)
		if err != nil {
			return err
		}
		if round, err = readRound(ctx, tx, roundID); err != nil {
			return err
		}
		// after 里补上补开的那一期：这个动作在运营眼里是「作废一期，换来新的一期」，
		// 日志上只写被作废的那一期，事后就答不出「作废之后活动还有没有在跑的期次」。
		after := roundSnapshotOf(round)
		if next != nil {
			after.NextRoundNo = next.RoundNo
		}
		return r.recorder.Record(ctx, tx, audit.Entry{
			Module: "lottery_rounds", Action: "cancel", Operation: "作废期次",
			TargetType: "lottery_round", TargetID: roundID, TargetName: round.RoundNo,
			Before: audit.Snapshot(roundSnapshotOf(locked)), After: audit.Snapshot(after),
		})
	})
	if err != nil {
		return nil, mapPGError(err)
	}
	return round, nil
}

// GetDraw 按 id 读一条开奖记录。
func (r *PostgresRepository) GetDraw(ctx context.Context, id string) (*model.Draw, error) {
	return readDraw(ctx, r.pool, id)
}

// FindDrawByRound 按期次读开奖记录，没开过时返回 nil。
//
// 返回 nil 而不是错误：期次详情页对「还没开奖」和「已开奖」显不显示名单是同一段代码的两个
// 分支，把前者做成一个错误会让每个调用方为一种正常状态写 err != nil 的判断。
func (r *PostgresRepository) FindDrawByRound(ctx context.Context, roundID string) (*model.Draw, error) {
	record, err := scanDraw(r.pool.QueryRow(ctx, `SELECT `+drawColumns+`
		FROM lottery_draws WHERE round_id=$1`, roundID))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	return record, nil
}

// RoundsAwaitingDraw 是开奖 worker 的取件：哪些期次现在看起来该开奖。
//
// **只有一条路**：已达门槛（closed）。这里曾经还有第二条「open 且 ends_at 已过」，2026-09-15
// 随到点必开一起删掉了——一个 open 的期次收不满就一直是 open，扫描**永远不会**把它挑出来，
// 这是有意的。它的出路只有人工开奖或作废（见 service.DrawManually / CancelRound）。
//
// **它只是候选集合**——真正的判定在 DrawRound 的锁里重做一遍，因为这里的扫描结果到拿到锁
// 之间可能已经过期（同一期的另一个副本先开掉了，或者又被参与顶了一次）。
//
// ORDER BY created_at 让积压时先开开得最早的那一期（期次不再有 starts_at，开期时刻就是
// created_at）。
func (r *PostgresRepository) RoundsAwaitingDraw(ctx context.Context, limit int) ([]string, error) {
	rows, err := r.pool.Query(ctx, `SELECT id::text FROM lottery_rounds
		WHERE status='closed'
		ORDER BY created_at, id
		LIMIT $1`, limit)
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

const drawColumns = `id::text, round_id::text, campaign_id::text, mode, trigger,
	algorithm, seed, participant_count, winner_count, drawn_by::text, reason, created_at`

func scanDraw(row scanner) (*model.Draw, error) {
	record := &model.Draw{}
	err := row.Scan(&record.ID, &record.RoundID, &record.CampaignID, &record.Mode,
		&record.Trigger, &record.Algorithm, &record.Seed, &record.ParticipantCount,
		&record.WinnerCount, &record.DrawnBy, &record.Reason, &record.CreatedAt)
	if err != nil {
		return nil, err
	}
	return record, nil
}

func readDraw(ctx context.Context, q querier, id string) (*model.Draw, error) {
	record, err := scanDraw(q.QueryRow(ctx, `SELECT `+drawColumns+`
		FROM lottery_draws WHERE id=$1`, id))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrDrawNotFound
		}
		return nil, err
	}
	return record, nil
}

// lockRound 锁住一期并读回它。
//
// 这个函数是整个抽奖域最重要的一个锁：参与确认、开奖、作废三条写路径都以它为起点，而
// 「一期只能开一次」「名单取完之后不会再有新人」这两条不变式全靠它。
//
// 锁的顺序在别处必须是**先期次、后参与**（见 Confirm 的注释）：开奖持有期次锁之后再插入
// lottery_wins，会对参与行取 FOR KEY SHARE；任何一条路径反过来先锁参与再锁期次，就会与它
// 形成环。
func lockRound(ctx context.Context, tx pgx.Tx, id string) (*model.Round, error) {
	return wrapRoundRow(tx.QueryRow(ctx, `SELECT `+roundColumns+`
		FROM lottery_rounds WHERE id=$1 FOR UPDATE`, id))
}
