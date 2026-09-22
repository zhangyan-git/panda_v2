package repository

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"

	"github.com/panda-dev/panda-v2/backend/services/lottery-service/internal/model"
)

const roundColumns = `id::text, campaign_id::text, seq, round_no, status, participant_target,
	participant_count, winner_count, drawn_at, cancelled_at,
	cancel_reason, cancelled_by::text, created_at, updated_at`

func scanRound(row scanner) (*model.Round, error) {
	round := &model.Round{}
	err := row.Scan(&round.ID, &round.CampaignID, &round.Seq, &round.RoundNo, &round.Status,
		&round.ParticipantTarget, &round.ParticipantCount, &round.WinnerCount,
		&round.DrawnAt, &round.CancelledAt,
		&round.CancelReason, &round.CancelledBy, &round.CreatedAt, &round.UpdatedAt)
	if err != nil {
		return nil, err
	}
	return round, nil
}

// GetRound 按 id 读一期。
func (r *PostgresRepository) GetRound(ctx context.Context, id string) (*model.Round, error) {
	return readRound(ctx, r.pool, id)
}

// LiveRound 读一个活动当前在跑的那一期（open 或 closed），没有则返回 nil。
//
// 「有没有在跑的一期」不是一个可以从活动推导出来的状态：活动 enabled 而两期之间的空档
// 是真实存在的（开奖事务里接着开下一期，中间那一刻）。查询用唯一索引
// lottery_rounds_one_live_per_campaign 的谓词，最多一行。
func (r *PostgresRepository) LiveRound(ctx context.Context, campaignID string) (*model.Round, error) {
	row := r.pool.QueryRow(ctx, `SELECT `+roundColumns+`
		FROM lottery_rounds WHERE campaign_id=$1 AND status IN ('open','closed')`, campaignID)
	round, err := scanRound(row)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	return round, nil
}

func readRound(ctx context.Context, q querier, id string) (*model.Round, error) {
	return wrapRoundRow(q.QueryRow(ctx, `SELECT `+roundColumns+`
		FROM lottery_rounds WHERE id=$1`, id))
}

func wrapRoundRow(row pgx.Row) (*model.Round, error) {
	round, err := scanRound(row)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrRoundNotFound
		}
		return nil, err
	}
	return round, nil
}
