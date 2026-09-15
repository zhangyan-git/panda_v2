package repository

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/panda-dev/panda-v2/backend/services/lottery-service/internal/model"
)

const campaignColumns = `id::text, activation_id::text, machine_id::text, code, name,
	participant_target, description, is_default, start_at, end_at, status,
	created_by::text, updated_by::text, created_at, updated_at`

const prizeColumns = `id::text, campaign_id::text, sort_order, prize_kind, name,
	coupon_template_id, image_url, claim_instructions, quantity, created_at, updated_at`

func scanCampaign(row scanner) (*model.Campaign, error) {
	campaign := &model.Campaign{}
	err := row.Scan(&campaign.ID, &campaign.ActivationID, &campaign.MachineID, &campaign.Code,
		&campaign.Name, &campaign.ParticipantTarget, &campaign.Description, &campaign.IsDefault,
		&campaign.StartAt, &campaign.EndAt, &campaign.Status,
		&campaign.CreatedBy, &campaign.UpdatedBy, &campaign.CreatedAt, &campaign.UpdatedAt)
	if err != nil {
		return nil, err
	}
	return campaign, nil
}

func scanPrize(row scanner) (*model.CampaignPrize, error) {
	prize := &model.CampaignPrize{}
	err := row.Scan(&prize.ID, &prize.CampaignID, &prize.SortOrder, &prize.PrizeKind, &prize.Name,
		&prize.CouponTemplateID, &prize.ImageURL, &prize.ClaimInstructions, &prize.Quantity,
		&prize.CreatedAt, &prize.UpdatedAt)
	if err != nil {
		return nil, err
	}
	return prize, nil
}

// PrizeInput 是写奖池时的一行。ID 为空表示新加的行，非空表示保留已有的那一行。
type PrizeInput struct {
	ID                string
	SortOrder         int32
	PrizeKind         string
	Name              string
	CouponTemplateID  string
	ImageURL          string
	ClaimInstructions string
	Quantity          int32
}

// CampaignParams 是新建 / 修改活动的全部可写字段。
type CampaignParams struct {
	ActivationID      string
	MachineID         *string
	Code              string
	Name              string
	Description       string
	ParticipantTarget int32
	StartAt           time.Time
	EndAt             time.Time
	Status            string
	Prizes            []PrizeInput
	Actor             *string
}

// CreateCampaign 新建一个活动（含奖池）。
//
// **不开期**：新活动的初始状态由 Status 决定，只有 enabled 的活动才由调用方紧接着开
// 第一期（service 层做）。这条分工与「开通即开期」不同，理由也不同——开通是一个明确的
// 「现在开始运营」动作，而新建活动常常是先配好、过两天再启用。
func (r *PostgresRepository) CreateCampaign(ctx context.Context, p CampaignParams) (*model.Campaign, error) {
	var campaignID string
	err := r.inTx(ctx, func(tx pgx.Tx) error {
		campaignID = newEventID()
		if _, err := tx.Exec(ctx, `INSERT INTO lottery_campaigns
			(id,activation_id,machine_id,code,name,description,is_default,participant_target,
			 start_at,end_at,status,created_by,updated_by)
			VALUES($1,$2,$3,$4,$5,$6,FALSE,$7,$8,$9,$10,$11,$11)`,
			campaignID, p.ActivationID, p.MachineID, p.Code, p.Name, p.Description,
			p.ParticipantTarget, p.StartAt, p.EndAt, p.Status, p.Actor); err != nil {
			return err
		}
		return writePrizes(ctx, tx, campaignID, p.Prizes)
	})
	if err != nil {
		return nil, mapPGError(err)
	}
	return readCampaign(ctx, r.pool, campaignID)
}

// UpdateCampaign 改一个活动与它的奖池。
//
// 奖池是**整体替换**：调用方提交完整清单，这里删掉不在清单里的行、更新留下来的、插入
// 新的。逐行增删接口会让前端自己算差集，而差集算错的后果是名额总数对不上——那正是开奖
// 要用的数（见 dto.PrizeRequest 的说明）。
//
// **活动不能换门店**：activation_id 不在 UPDATE 里。换门店等于新建一个活动。
func (r *PostgresRepository) UpdateCampaign(ctx context.Context, id string, p CampaignParams) (*model.Campaign, error) {
	err := r.inTx(ctx, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `UPDATE lottery_campaigns
			SET machine_id=$2, code=$3, name=$4, description=$5, participant_target=$6,
			    start_at=$7, end_at=$8, status=$9, updated_by=$10, updated_at=NOW()
			WHERE id=$1`,
			id, p.MachineID, p.Code, p.Name, p.Description, p.ParticipantTarget,
			p.StartAt, p.EndAt, p.Status, p.Actor)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return ErrCampaignNotFound
		}
		return replacePrizes(ctx, tx, id, p.Prizes)
	})
	if err != nil {
		return nil, mapPGError(err)
	}
	return readCampaign(ctx, r.pool, id)
}

// UpdateCampaignStatus 只改状态。暂停 / 恢复 / 结束走它，不重写整个活动。
//
// 单独一个方法而不是让调用方先读再写：那个写法在并发下会拿一份过期的活动去覆盖别处刚
// 改过的门槛与奖池（读-改-写的经典丢更新）。这里 UPDATE 的只有 status 一列。
func (r *PostgresRepository) UpdateCampaignStatus(ctx context.Context, id, status string, actor *string) (*model.Campaign, error) {
	tag, err := r.pool.Exec(ctx, `UPDATE lottery_campaigns
		SET status=$2, updated_by=$3, updated_at=NOW() WHERE id=$1`, id, status, actor)
	if err != nil {
		return nil, mapPGError(err)
	}
	if tag.RowsAffected() == 0 {
		return nil, ErrCampaignNotFound
	}
	return readCampaign(ctx, r.pool, id)
}

// GetCampaign 按 id 读一个活动（不含奖池）。
func (r *PostgresRepository) GetCampaign(ctx context.Context, id string) (*model.Campaign, error) {
	return readCampaign(ctx, r.pool, id)
}

// GetDefaultCampaign 读一个开通记录的默认活动。
func (r *PostgresRepository) GetDefaultCampaign(ctx context.Context, activationID string) (*model.Campaign, error) {
	row := r.pool.QueryRow(ctx, `SELECT `+campaignColumns+`
		FROM lottery_campaigns WHERE activation_id=$1 AND is_default`, activationID)
	return wrapCampaignRow(row)
}

// ListPrizes 读一个活动的奖池，按 sort_order 升序。
//
// 顺序在这里定死（ORDER BY sort_order）：开奖的名字额分配依赖它，调用方不该有机会传一个
// 别的顺序进来——「哪一档先给」是业务规则，而它在建奖池时就已经由运营排好了。
func (r *PostgresRepository) ListPrizes(ctx context.Context, campaignID string) ([]*model.CampaignPrize, error) {
	return readPrizes(ctx, r.pool, campaignID)
}

// PrizeTotal 是一个奖池的名额总和，也就是下一期的 winner_count。
func (r *PostgresRepository) PrizeTotal(ctx context.Context, campaignID string) (int32, error) {
	var total int32
	err := r.pool.QueryRow(ctx, `SELECT COALESCE(SUM(quantity),0)::int
		FROM lottery_campaign_prizes WHERE campaign_id=$1`, campaignID).Scan(&total)
	return total, err
}

func readCampaign(ctx context.Context, q querier, id string) (*model.Campaign, error) {
	return wrapCampaignRow(q.QueryRow(ctx, `SELECT `+campaignColumns+`
		FROM lottery_campaigns WHERE id=$1`, id))
}

func wrapCampaignRow(row pgx.Row) (*model.Campaign, error) {
	campaign, err := scanCampaign(row)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrCampaignNotFound
		}
		return nil, err
	}
	return campaign, nil
}

func readPrizes(ctx context.Context, q querier, campaignID string) ([]*model.CampaignPrize, error) {
	rows, err := q.Query(ctx, `SELECT `+prizeColumns+`
		FROM lottery_campaign_prizes WHERE campaign_id=$1 ORDER BY sort_order`, campaignID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var prizes []*model.CampaignPrize
	for rows.Next() {
		prize, err := scanPrize(rows)
		if err != nil {
			return nil, err
		}
		prizes = append(prizes, prize)
	}
	return prizes, rows.Err()
}

// writePrizes 一次写完一个新建活动的奖池。
func writePrizes(ctx context.Context, tx pgx.Tx, campaignID string, prizes []PrizeInput) error {
	for _, prize := range prizes {
		if _, err := tx.Exec(ctx, `INSERT INTO lottery_campaign_prizes
			(campaign_id,sort_order,prize_kind,name,coupon_template_id,image_url,claim_instructions,quantity)
			VALUES($1,$2,$3,$4,$5,$6,$7,$8)`,
			campaignID, prize.SortOrder, prize.PrizeKind, prize.Name,
			prize.CouponTemplateID, prize.ImageURL, prize.ClaimInstructions, prize.Quantity); err != nil {
			return err
		}
	}
	return nil
}

// replacePrizes 用提交上来的清单整体替换一个活动的奖池。
//
// 删除按 id 的反集（`id <> ALL(...)`）而不是「先全删再全插」：先全删会换掉每一行的 id，
// 而这个 id 被已开出的中奖记录引用着（lottery_wins.prize_id 是 ON DELETE RESTRICT 的
// 外键）——全删会被外键拒绝，而且拒绝得很难懂。保留 id 还让「这一档奖的历年中奖记录」
// 能连起来看。
//
// sort_order 上有 (campaign_id, sort_order) 唯一索引，所以更新顺序而不能先插入后删除：
// 交换两档顺序时中间态会撞唯一键。两段更新（先挪到负数区间再落到目标值）不值当，这里
// 直接「先删后改」——删除先做，腾出来的名额不会有冲突。
func replacePrizes(ctx context.Context, tx pgx.Tx, campaignID string, prizes []PrizeInput) error {
	keep := make([]string, 0, len(prizes))
	for _, prize := range prizes {
		if prize.ID != "" {
			keep = append(keep, prize.ID)
		}
	}
	// 没有任何一行带 id 时 keep 是空切片，`id <> ALL('{}')` 为真——即全部删掉，
	// 正是「换了一整套奖池」的意思。
	if _, err := tx.Exec(ctx, `DELETE FROM lottery_campaign_prizes
		WHERE campaign_id=$1 AND id <> ALL($2::uuid[])`, campaignID, keep); err != nil {
		return err
	}

	for _, prize := range prizes {
		if prize.ID != "" {
			tag, err := tx.Exec(ctx, `UPDATE lottery_campaign_prizes
				SET sort_order=$3, prize_kind=$4, name=$5, coupon_template_id=$6,
				    image_url=$7, claim_instructions=$8, quantity=$9, updated_at=NOW()
				WHERE id=$1 AND campaign_id=$2`,
				prize.ID, campaignID, prize.SortOrder, prize.PrizeKind, prize.Name,
				prize.CouponTemplateID, prize.ImageURL, prize.ClaimInstructions, prize.Quantity)
			if err != nil {
				return err
			}
			if tag.RowsAffected() == 1 {
				continue
			}
			// 带着一个不属于这个活动的 id 进来。当成「新加一行」而不是报错：
			// 前端从别的活动复制一份奖池再改，是很正常的一次操作。
		}
		if _, err := tx.Exec(ctx, `INSERT INTO lottery_campaign_prizes
			(campaign_id,sort_order,prize_kind,name,coupon_template_id,image_url,claim_instructions,quantity)
			VALUES($1,$2,$3,$4,$5,$6,$7,$8)`,
			campaignID, prize.SortOrder, prize.PrizeKind, prize.Name,
			prize.CouponTemplateID, prize.ImageURL, prize.ClaimInstructions, prize.Quantity); err != nil {
			return err
		}
	}
	return nil
}
