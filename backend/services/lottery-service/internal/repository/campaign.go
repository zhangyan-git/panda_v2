package repository

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"

	"github.com/panda-dev/panda-v2/backend/platform/audit"
	"github.com/panda-dev/panda-v2/backend/services/lottery-service/internal/model"
)

const campaignColumns = `id::text, activation_id::text, machine_id::text, code, name,
	participant_target, description, is_default, status,
	created_by::text, updated_by::text, created_at, updated_at`

// prizeColumns 的列序与下面 scanPrize 的 Scan 参数**必须逐位对应**：这是位置扫描，
// 中间插一列而后面不改，字段就会整体串位（而且不报错）。
const prizeColumns = `id::text, campaign_id::text, name, cover_image, poster_image,
	claim_instructions, quantity, created_at, updated_at`

func scanCampaign(row scanner) (*model.Campaign, error) {
	campaign := &model.Campaign{}
	err := row.Scan(&campaign.ID, &campaign.ActivationID, &campaign.MachineID, &campaign.Code,
		&campaign.Name, &campaign.ParticipantTarget, &campaign.Description, &campaign.IsDefault,
		&campaign.Status,
		&campaign.CreatedBy, &campaign.UpdatedBy, &campaign.CreatedAt, &campaign.UpdatedAt)
	if err != nil {
		return nil, err
	}
	return campaign, nil
}

func scanPrize(row scanner) (*model.CampaignPrize, error) {
	prize := &model.CampaignPrize{}
	err := row.Scan(&prize.ID, &prize.CampaignID, &prize.Name, &prize.CoverImage, &prize.PosterImage,
		&prize.ClaimInstructions, &prize.Quantity, &prize.CreatedAt, &prize.UpdatedAt)
	if err != nil {
		return nil, err
	}
	return prize, nil
}

// PrizeInput 是写奖品时要落的那些值。
//
// ID 为空表示新插一行，非空表示保留已有的那一行（原地 UPDATE，id 不变）——这个区别是
// 要紧的：id 被中奖记录引用着，见 replacePrize。
//
// Quantity 不在接口层暴露（名额恒为 1，见 dto.PrizeRequest），但在这一层留着：开通模板
// （repository.DefaultPrize）走的是同一个结构体，activation.openFirstRound 还要读它。
type PrizeInput struct {
	ID                string
	Name              string
	CoverImage        string
	PosterImage       string
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
	Status            string
	Prize             PrizeInput
	Actor             *string
}

// campaignPrizeSnapshot 是奖品那几列。奖品是「开奖时发什么」，改活动最要紧的一格就是它。
type campaignPrizeSnapshot struct {
	ID                string `json:"id"`
	Name              string `json:"name"`
	ClaimInstructions string `json:"claimInstructions,omitempty"`
	Quantity          int32  `json:"quantity"`
}

// campaignSnapshot 是抽奖活动写进审计的那几个字段。
//
// 不用 model.Campaign：它只有 db tag，快照出来是一串大写的列名。is_default 也不进——它是
// 开通时定的，后台改不了；created_by / updated_by 更不进，ActorID 已经在条目上了。
type campaignSnapshot struct {
	ActivationID      string                 `json:"activationId"`
	MachineID         *string                `json:"machineId,omitempty"`
	Code              string                 `json:"code"`
	Name              string                 `json:"name"`
	Description       string                 `json:"description"`
	ParticipantTarget int32                  `json:"participantTarget"`
	Status            string                 `json:"status"`
	Prize             *campaignPrizeSnapshot `json:"prize,omitempty"`
}

// selectCampaignSnapshot 在事务里锁住活动行并读出待审字段（含奖品）。
//
// FOR UPDATE 是为了让 before 与 after 之间不会有别人插进来——否则日志上写着的「从 A 改成 B」
// 可能是从 A' 改成 B。奖品行不单独加锁：它跟着活动行走，写它的两条路都在活动行的锁之下。
func selectCampaignSnapshot(ctx context.Context, tx pgx.Tx, id string) (campaignSnapshot, error) {
	var s campaignSnapshot
	err := tx.QueryRow(ctx, `SELECT activation_id::text, machine_id::text, code, name,
		description, participant_target, status
		FROM lottery_campaigns WHERE id=$1 FOR UPDATE`, id).
		Scan(&s.ActivationID, &s.MachineID, &s.Code, &s.Name, &s.Description,
			&s.ParticipantTarget, &s.Status)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return s, ErrCampaignNotFound
		}
		return s, err
	}
	prize := &campaignPrizeSnapshot{}
	err = tx.QueryRow(ctx, `SELECT id::text, name, claim_instructions, quantity
		FROM lottery_campaign_prizes WHERE campaign_id=$1 ORDER BY created_at LIMIT 1`, id).
		Scan(&prize.ID, &prize.Name, &prize.ClaimInstructions, &prize.Quantity)
	switch {
	case err == nil:
		s.Prize = prize
	case errors.Is(err, pgx.ErrNoRows):
		// 活动可以没有奖品（新建时 Prize 是全零值的情况），日志上就是没有 prize 这一格。
	default:
		return s, err
	}
	return s, nil
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
			 status,created_by,updated_by)
			VALUES($1,$2,$3,$4,$5,$6,FALSE,$7,$8,$9,$9)`,
			campaignID, p.ActivationID, p.MachineID, p.Code, p.Name, p.Description,
			p.ParticipantTarget, p.Status, p.Actor); err != nil {
			return err
		}
		if err := writePrize(ctx, tx, campaignID, p.Prize); err != nil {
			return err
		}
		after, err := selectCampaignSnapshot(ctx, tx, campaignID)
		if err != nil {
			return err
		}
		return r.recorder.Record(ctx, tx, audit.Entry{
			Module: "lottery_campaigns", Action: "create", Operation: "新增抽奖活动",
			TargetType: "lottery_campaign", TargetID: campaignID, TargetName: after.Name,
			After: audit.Snapshot(after),
		})
	})
	if err != nil {
		return nil, mapPGError(err)
	}
	return readCampaign(ctx, r.pool, campaignID)
}

// UpdateCampaign 改一个活动与它的奖品。
//
// 奖品是**整份替换**：调用方提交那一个奖品，这里删掉不是它的行、更新它、没有就插入
// （见 replacePrize）。奖项没有逐行接口——一个活动只有一个奖品，逐行增删只会让前端自己
// 算差集，而差集算错的后果是开奖时发错东西。
//
// **活动不能换门店**：activation_id 不在 UPDATE 里。换门店等于新建一个活动。
func (r *PostgresRepository) UpdateCampaign(ctx context.Context, id string, p CampaignParams) (*model.Campaign, error) {
	err := r.inTx(ctx, func(tx pgx.Tx) error {
		// before 在改之前读，而且是**用 selectCampaignSnapshot 而不是 readCampaign**：后者
		// 不带 FOR UPDATE，并发下读到的可能是别人还没提交的那一份。
		before, err := selectCampaignSnapshot(ctx, tx, id)
		if err != nil {
			return err
		}
		tag, err := tx.Exec(ctx, `UPDATE lottery_campaigns
			SET machine_id=$2, code=$3, name=$4, description=$5, participant_target=$6,
			    status=$7, updated_by=$8, updated_at=NOW()
			WHERE id=$1`,
			id, p.MachineID, p.Code, p.Name, p.Description, p.ParticipantTarget,
			p.Status, p.Actor)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return ErrCampaignNotFound
		}
		if err := replacePrize(ctx, tx, id, p.Prize); err != nil {
			return err
		}
		after, err := selectCampaignSnapshot(ctx, tx, id)
		if err != nil {
			return err
		}
		return r.recorder.Record(ctx, tx, audit.Entry{
			Module: "lottery_campaigns", Action: "update", Operation: "修改抽奖活动",
			TargetType: "lottery_campaign", TargetID: id, TargetName: after.Name,
			Before: audit.Snapshot(before), After: audit.Snapshot(after),
		})
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
	var updated *model.Campaign
	err := r.inTx(ctx, func(tx pgx.Tx) error {
		before, err := selectCampaignSnapshot(ctx, tx, id)
		if err != nil {
			return err
		}
		tag, err := tx.Exec(ctx, `UPDATE lottery_campaigns
			SET status=$2, updated_by=$3, updated_at=NOW() WHERE id=$1`, id, status, actor)
		if err != nil {
			return mapPGError(err)
		}
		if tag.RowsAffected() == 0 {
			return ErrCampaignNotFound
		}
		after, err := selectCampaignSnapshot(ctx, tx, id)
		if err != nil {
			return err
		}
		// 整份快照而不是只记 status：暂停一个活动与改一次门槛挨在一起发生时，日志上只剩
		// 两个 paused，看不出当时跑的是什么配置。
		if err := r.recorder.Record(ctx, tx, audit.Entry{
			Module: "lottery_campaigns", Action: "update_status", Operation: "修改抽奖活动状态",
			TargetType: "lottery_campaign", TargetID: id, TargetName: after.Name,
			Before: audit.Snapshot(before), After: audit.Snapshot(after),
		}); err != nil {
			return err
		}
		updated, err = readCampaign(ctx, tx, id)
		return err
	})
	if err != nil {
		return nil, err
	}
	return updated, nil
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

// ListPrizes 读一个活动的奖品，零个或一个。
//
// 留着切片的形状而不是改成单个指针：开奖那条路（readPrizes → draw.SelectWinners）按切片
// 分配名额，为一行改掉整条签名不值当。调用方想知道「有没有」，看长度。
func (r *PostgresRepository) ListPrizes(ctx context.Context, campaignID string) ([]*model.CampaignPrize, error) {
	return readPrizes(ctx, r.pool, campaignID)
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
		FROM lottery_campaign_prizes WHERE campaign_id=$1`, campaignID)
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

// writePrize 写下新建活动的那个奖品。
func writePrize(ctx context.Context, tx pgx.Tx, campaignID string, prize PrizeInput) error {
	_, err := tx.Exec(ctx, `INSERT INTO lottery_campaign_prizes
		(campaign_id,name,cover_image,poster_image,claim_instructions,quantity)
		VALUES($1,$2,$3,$4,$5,$6)`,
		campaignID, prize.Name, prize.CoverImage, prize.PosterImage,
		prize.ClaimInstructions, prize.Quantity)
	return err
}

// replacePrize 把提交上来的那一个奖品换成活动当前的奖品。
//
// **带 id 就原地 UPDATE，不是删了重插。** 这个 id 被已开出的中奖记录引用着
// （lottery_wins.prize_id 是 ON DELETE RESTRICT 的外键），删了重插会被外键拒绝，而且拒绝
// 得很难懂；原地改还让「同一个奖品历年的中奖记录」连得起来。中奖记录上那两列名字快照
// （original_ / current_prize_name）就是为「奖品改过之后还看得见原来发的是什么」而存在的。
//
// 删除按 id 的反集（`id <> ALL(...)`）而不是「先全删再全插」，理由同上；keep 至多一个 id。
// 提交上来的 id 为空时 keep 是空切片，`id <> ALL('{}')` 为真——即把旧行删掉，正是「换了个
// 新奖品」的意思（那条 DELETE 会被外键挡住，如果这个奖品已经发出过奖——这是对的）。
func replacePrize(ctx context.Context, tx pgx.Tx, campaignID string, prize PrizeInput) error {
	keep := make([]string, 0, 1)
	if prize.ID != "" {
		keep = append(keep, prize.ID)
	}
	if _, err := tx.Exec(ctx, `DELETE FROM lottery_campaign_prizes
		WHERE campaign_id=$1 AND id <> ALL($2::uuid[])`, campaignID, keep); err != nil {
		return err
	}

	if prize.ID != "" {
		tag, err := tx.Exec(ctx, `UPDATE lottery_campaign_prizes
			SET name=$3, cover_image=$4, poster_image=$5, claim_instructions=$6,
			    quantity=$7, updated_at=NOW()
			WHERE id=$1 AND campaign_id=$2`,
			prize.ID, campaignID, prize.Name, prize.CoverImage, prize.PosterImage,
			prize.ClaimInstructions, prize.Quantity)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 1 {
			return nil
		}
		// 带着一个不属于这个活动的 id 进来。当成「新加一个」而不是报错：前端从别的活动
		// 复制一份奖品再改，是很正常的一次操作。
	}
	_, err := tx.Exec(ctx, `INSERT INTO lottery_campaign_prizes
		(campaign_id,name,cover_image,poster_image,claim_instructions,quantity)
		VALUES($1,$2,$3,$4,$5,$6)`,
		campaignID, prize.Name, prize.CoverImage, prize.PosterImage,
		prize.ClaimInstructions, prize.Quantity)
	return err
}
