package repository

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/panda-dev/panda-v2/backend/services/lottery-service/internal/model"
)

// activationColumns 是 lottery_activations 的读取列。
//
// UUID 列一律 ::text：pgx 把 uuid 扫进 string 需要这一步，少了它 Scan 会报类型不匹配。
// 列顺序与 scanActivation 的扫描顺序严格一一对应，两边必须一起改。
const activationColumns = `id::text, location_id::text, location_name, status, remark,
	activated_by::text, activated_at, deactivated_at, created_at, updated_at`

func scanActivation(row scanner) (*model.Activation, error) {
	activation := &model.Activation{}
	err := row.Scan(&activation.ID, &activation.LocationID, &activation.LocationName,
		&activation.Status, &activation.Remark, &activation.ActivatedBy,
		&activation.ActivatedAt, &activation.DeactivatedAt,
		&activation.CreatedAt, &activation.UpdatedAt)
	if err != nil {
		return nil, err
	}
	return activation, nil
}

// ActivateParams 是开通一家门店抽奖时一并建出来的东西。
//
// 默认活动与第一期在**同一个事务**里建：一次「开通」在运营看来是一件事，中途失败留下
// 一个「开通了但没有活动」的门店，会让抽奖中心对着一个空活动列表报错。
type ActivateParams struct {
	LocationID   string
	LocationName string
	Remark       string
	ActivatedBy  *string

	// 默认活动的全部字段，由 service 按内置模板填好（含代码、门槛、窗口、奖品）。
	Campaign DefaultCampaign
}

// DefaultCampaign 是开通时按内置模板创建的那个活动的完整形状。
//
// 它在 service 层组装（模板是业务规则），仓储只负责把它和三行记录一起写下去。
type DefaultCampaign struct {
	Code              string
	Name              string
	Description       string
	ParticipantTarget int32
	StartAt           time.Time
	EndAt             time.Time
	Prize             DefaultPrize
}

// DefaultPrize 是内置模板里的那一行奖池。
//
// 只有一个奖品、一个名额：开箱即用的门店抽奖就是「本期抽一个人送一份礼品」。
// 运营按需要改门槛与奖池——那张表就是为这件事存在的。
type DefaultPrize struct {
	PrizeKind string
	Name      string
	Quantity  int32
}

// ActivationCreated 是开通的结果：开通记录本身 + 默认活动 + 第一期。
type ActivationCreated struct {
	Activation *model.Activation
	Campaign   *model.Campaign
	Round      *model.Round
}

// Activate 开通一家门店的抽奖：开通记录 + 默认活动 + 第一期，同一个事务。
//
// 三个 id 由 Go 侧先生成再写库，而不是靠 RETURNING 串起来：第一期要用活动 id 拼
// round_no，而 round_no 是唯一索引的一部分——先有 id 才能拼出一个确定的期次号。
// 这一点与别处「让数据库生成主键」的写法不同，理由就在这里。
func (r *PostgresRepository) Activate(ctx context.Context, p ActivateParams) (*ActivationCreated, error) {
	created := &ActivationCreated{}
	err := r.inTx(ctx, func(tx pgx.Tx) error {
		activationID := newEventID()
		if _, err := tx.Exec(ctx, `INSERT INTO lottery_activations
			(id,location_id,location_name,status,remark,activated_by)
			VALUES($1,$2,$3,'enabled',$4,$5)`,
			activationID, p.LocationID, p.LocationName, p.Remark, p.ActivatedBy); err != nil {
			return err
		}

		campaignID := newEventID()
		if _, err := tx.Exec(ctx, `INSERT INTO lottery_campaigns
			(id,activation_id,code,name,description,is_default,participant_target,start_at,end_at,status,created_by)
			VALUES($1,$2,$3,$4,$5,TRUE,$6,$7,$8,'enabled',$9)`,
			campaignID, activationID, p.Campaign.Code, p.Campaign.Name, p.Campaign.Description,
			p.Campaign.ParticipantTarget, p.Campaign.StartAt, p.Campaign.EndAt, p.ActivatedBy); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO lottery_campaign_prizes
			(campaign_id,sort_order,prize_kind,name,quantity)
			VALUES($1,0,$2,$3,$4)`,
			campaignID, p.Campaign.Prize.PrizeKind, p.Campaign.Prize.Name, p.Campaign.Prize.Quantity); err != nil {
			return err
		}

		round, err := openFirstRound(ctx, tx, campaignID, p.Campaign)
		if err != nil {
			return err
		}

		activation, err := readActivation(ctx, tx, activationID)
		if err != nil {
			return err
		}
		campaign, err := readCampaign(ctx, tx, campaignID)
		if err != nil {
			return err
		}
		created.Activation, created.Campaign, created.Round = activation, campaign, round
		return nil
	})
	if err != nil {
		return nil, mapPGError(err)
	}
	return created, nil
}

// openFirstRound 建活动的第一期（seq=1）。
//
// 开通即开期：一个「开通了抽奖、但抽奖中心没有一期可参与」的门店，在运营看来就是开通
// 失败了。之后的期次由开奖 worker 在开奖的同事务里接着开（见 draw.go）。
func openFirstRound(ctx context.Context, tx pgx.Tx, campaignID string, campaign DefaultCampaign) (*model.Round, error) {
	roundID := newEventID()
	// winner_count 是**名额**，取奖池总和；实际抽出的人数还可能被参与数封顶。
	winnerCount := campaign.Prize.Quantity
	if _, err := tx.Exec(ctx, `INSERT INTO lottery_rounds
		(id,campaign_id,seq,round_no,status,participant_target,participant_count,winner_count,starts_at,ends_at)
		VALUES($1,$2,1,$3,'open',$4,0,$5,$6,$7)`,
		roundID, campaignID, model.RoundNo(campaign.Code, 1),
		campaign.ParticipantTarget, winnerCount, campaign.StartAt, campaign.EndAt); err != nil {
		return nil, err
	}
	return readRound(ctx, tx, roundID)
}

// OpenNextRound 在同事务里开活动的下一期。
//
// 由开奖路径调用（draw.go），所以它收 tx 而不是自己开事务——下一期必须与上一期的开奖
// 一起提交，否则会出现「上一期已 drawn、但没有下一期在跑」的空档，而那期间所有扫码的
// 用户都会看到「暂无进行中的活动」。
func OpenNextRound(ctx context.Context, tx pgx.Tx, campaign *model.Campaign, seq int32) (*model.Round, error) {
	roundID := newEventID()
	if _, err := tx.Exec(ctx, `INSERT INTO lottery_rounds
		(id,campaign_id,seq,round_no,status,participant_target,participant_count,winner_count,starts_at,ends_at)
		SELECT $1,$2,$3,$4,'open',$5,0,COALESCE(SUM(quantity),0),NOW(),$6
		FROM lottery_campaign_prizes WHERE campaign_id=$2`,
		roundID, campaign.ID, seq, model.RoundNo(campaign.Code, seq),
		campaign.ParticipantTarget, campaign.EndAt); err != nil {
		return nil, err
	}
	return readRound(ctx, tx, roundID)
}

// GetActivation 按 id 读一条开通记录。
func (r *PostgresRepository) GetActivation(ctx context.Context, id string) (*model.Activation, error) {
	return readActivation(ctx, r.pool, id)
}

// FindActivationByLocation 按门店读开通记录，没开通时返回 ErrActivationNotFound。
func (r *PostgresRepository) FindActivationByLocation(ctx context.Context, locationID string) (*model.Activation, error) {
	row := r.pool.QueryRow(ctx, `SELECT `+activationColumns+`
		FROM lottery_activations WHERE location_id=$1`, locationID)
	return wrapActivationRow(row)
}

// UpdateActivationStatus 启用 / 停用一家门店的抽奖。
//
// 停用**不动正在跑的那一期**：已经收了 N 个人的参与，作废它们要 N 次跨服务冲正。停用
// 停的是「不再开新期」，与活动的 paused 是同一条规矩（见 model.CampaignPaused）。
//
// 幂等：已经是目标状态时返回当前行，不报错。「停用」是运营的意图，重复表达它不是错误。
func (r *PostgresRepository) UpdateActivationStatus(ctx context.Context, id, status, remark string, actor *string) (*model.Activation, error) {
	err := r.inTx(ctx, func(tx pgx.Tx) error {
		// deactivated_at 由 status 决定，与 CHECK ((status='disabled') = (deactivated_at IS NOT NULL))
		// 绑定：写死一个「停用时间」而状态还是 enabled 会被数据库拒掉，那是好事。
		_, err := tx.Exec(ctx, `UPDATE lottery_activations
			SET status=$2,
			    remark=CASE WHEN $3='' THEN remark ELSE $3 END,
			    deactivated_at=CASE WHEN $2='disabled' THEN NOW() ELSE NULL END,
			    updated_at=NOW()
			WHERE id=$1`, id, status, remark)
		return err
	})
	if err != nil {
		return nil, mapPGError(err)
	}
	activation, err := readActivation(ctx, r.pool, id)
	if err != nil {
		return nil, mapPGError(err)
	}
	return activation, nil
}

// CountCampaigns 数一个开通记录下的活动数（含默认）。列表页显示「3 个活动」。
func (r *PostgresRepository) CountCampaigns(ctx context.Context, activationID string) (int, error) {
	var total int
	err := r.pool.QueryRow(ctx, `SELECT COUNT(*) FROM lottery_campaigns WHERE activation_id=$1`, activationID).Scan(&total)
	return total, err
}

func readActivation(ctx context.Context, q querier, id string) (*model.Activation, error) {
	return wrapActivationRow(q.QueryRow(ctx, `SELECT `+activationColumns+`
		FROM lottery_activations WHERE id=$1`, id))
}

// wrapActivationRow 把「没有这一行」翻成 ErrActivationNotFound，其余错误原样返回。
//
// 抽成函数是因为两条读路径（按 id、按门店）的翻法必须一样：漏掉一条就会让「门店没开通」
// 在上层看起来像一次数据库故障，界面上是 500 而不是「这家店还没开通抽奖」。
func wrapActivationRow(row pgx.Row) (*model.Activation, error) {
	activation, err := scanActivation(row)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrActivationNotFound
		}
		return nil, err
	}
	return activation, nil
}

// DefaultCampaignCode 由门店 id 派生默认活动的短名：'L' + 门店 UUID 的前 8 位十六进制。
//
// 为什么必须派生而不是写死一个好看的常量：code 上有全局唯一索引（它是期次号的前缀），
// 而开通是**每家门店一次**的动作，写死就意味着第二家店开通即撞车。取门店 id 的前 8 位
// 让同一家店永远得到同一个短名（重试安全），碰撞概率是生日界（几千家店约 1e-5），
// 真撞上会被唯一索引拦成一次说得清的 409，而不是悄悄建出一个重复的期次号前缀。
//
// 运营可以在开通后改掉它——「LA1B2C3D4-0001」只适合当默认值。
func DefaultCampaignCode(locationID string) string {
	hex := strings.ReplaceAll(locationID, "-", "")
	if len(hex) > 8 {
		hex = hex[:8]
	}
	return "L" + strings.ToUpper(hex)
}
