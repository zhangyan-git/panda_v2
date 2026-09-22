package repository

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/panda-dev/panda-v2/backend/platform/audit"
	"github.com/panda-dev/panda-v2/backend/services/lottery-service/internal/model"
)

// activationColumns 是 lottery_activations 的读取列。
//
// UUID 列一律 ::text：pgx 把 uuid 扫进 string 需要这一步，少了它 Scan 会报类型不匹配。
// 列顺序与 scanActivation 的扫描顺序严格一一对应，两边必须一起改。
const activationColumns = `id::text, location_id::text, status, remark,
	activated_by::text, activated_at, deactivated_at, created_at, updated_at`

func scanActivation(row scanner) (*model.Activation, error) {
	activation := &model.Activation{}
	err := row.Scan(&activation.ID, &activation.LocationID,
		&activation.Status, &activation.Remark, &activation.ActivatedBy,
		&activation.ActivatedAt, &activation.DeactivatedAt,
		&activation.CreatedAt, &activation.UpdatedAt)
	if err != nil {
		return nil, err
	}
	return activation, nil
}

// activationSnapshot 是开通记录写进审计的那几个字段。
//
// 不用 model.Activation：它只有 db tag，快照出来是一串大写的列名；activated_by / created_at
// 也不进——ActorID 已经在条目上，行的时间戳每一条日志本来就带。
type activationSnapshot struct {
	LocationID string `json:"locationId"`
	Status     string `json:"status"`
	Remark     string `json:"remark,omitempty"`
	// 指针 + omitempty：停用时间为空时这一格直接不出现。写成 time.Time 的话会渲染成
	// 0001-01-01T00:00:00Z，「没停用过」与「停用在公元 1 年」在日志上就分不开了。
	DeactivatedAt *time.Time `json:"deactivatedAt,omitempty"`
	// DefaultCampaignCode / DefaultRoundNo 只在开通那一条上有：运营要知道「这次开通顺带
	// 建出来的是哪个活动、哪一期」，事后拿日志对账时那两个短名比两个 UUID 好认。
	DefaultCampaignCode string `json:"defaultCampaignCode,omitempty"`
	DefaultRoundNo      string `json:"defaultRoundNo,omitempty"`
}

func activationSnapshotOf(a *model.Activation, campaign *model.Campaign, round *model.Round) activationSnapshot {
	s := activationSnapshot{
		LocationID: a.LocationID, Status: a.Status, Remark: a.Remark,
		DeactivatedAt: a.DeactivatedAt,
	}
	if campaign != nil {
		s.DefaultCampaignCode = campaign.Code
	}
	if round != nil {
		s.DefaultRoundNo = round.RoundNo
	}
	return s
}

// selectActivationSnapshot 在事务里锁住这一行并读出待审字段。
//
// 不能复用 readActivation：那个函数不带 FOR UPDATE（它是读路径的函数，不该替调用方决定锁），
// 而这里要的是「读到的那一份在本次提交之前不会被别人改掉」——否则日志上的 before 可能是一份
// 从未存在过的旧状态。
func selectActivationSnapshot(ctx context.Context, tx pgx.Tx, id string) (activationSnapshot, error) {
	var s activationSnapshot
	err := tx.QueryRow(ctx, `SELECT location_id::text, status, remark, deactivated_at
		FROM lottery_activations WHERE id=$1 FOR UPDATE`, id).
		Scan(&s.LocationID, &s.Status, &s.Remark, &s.DeactivatedAt)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return s, ErrActivationNotFound
		}
		return s, err
	}
	return s, nil
}

// ActivateParams 是开通一家门店抽奖时一并建出来的东西。
//
// 默认活动与第一期在**同一个事务**里建：一次「开通」在运营看来是一件事，中途失败留下
// 一个「开通了但没有活动」的门店，会让抽奖中心对着一个空活动列表报错。
type ActivateParams struct {
	LocationID  string
	Remark      string
	ActivatedBy *string

	// 默认活动的全部字段，由 service 按内置模板填好（含代码、门槛、奖品）。
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
	Prize             DefaultPrize
}

// DefaultPrize 是内置模板里的那个奖品。
//
// 一个奖品、一个名额：开箱即用的门店抽奖就是「本期抽一个人送一份礼品」。运营再按需要改
// 门槛与奖品。
//
// **两张图都是空的**，而封面在后台表单上是必填的。这是有意的：开通是一个一键动作，逼运营
// 先找一张图才能开通，等于把整个域的入口抬高。空封面由前端回落到原型里那块占位；等他们真
// 要办活动了，编辑那个默认活动时再传图——那条路上的校验会要求它。
type DefaultPrize struct {
	Name              string
	CoverImage        string
	PosterImage       string
	ClaimInstructions string
	Quantity          int32
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
			(id,location_id,status,remark,activated_by)
			VALUES($1,$2,'enabled',$3,$4)`,
			activationID, p.LocationID, p.Remark, p.ActivatedBy); err != nil {
			return err
		}

		campaignID := newEventID()
		if _, err := tx.Exec(ctx, `INSERT INTO lottery_campaigns
			(id,activation_id,code,name,description,is_default,participant_target,status,created_by)
			VALUES($1,$2,$3,$4,$5,TRUE,$6,'enabled',$7)`,
			campaignID, activationID, p.Campaign.Code, p.Campaign.Name, p.Campaign.Description,
			p.Campaign.ParticipantTarget, p.ActivatedBy); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO lottery_campaign_prizes
			(campaign_id,name,cover_image,poster_image,claim_instructions,quantity)
			VALUES($1,$2,$3,$4,$5,$6)`,
			campaignID, p.Campaign.Prize.Name, p.Campaign.Prize.CoverImage,
			p.Campaign.Prize.PosterImage, p.Campaign.Prize.ClaimInstructions,
			p.Campaign.Prize.Quantity); err != nil {
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
		if err := r.recorder.Record(ctx, tx, audit.Entry{
			Module: "lottery_activations", Action: "activate", Operation: "开通门店抽奖",
			TargetType: "lottery_activation", TargetID: activationID,
			// TargetName 里放的是**门店 id 不是店名**：本服务不存店名（后台列表上的
			// locationName 是读那一刻向商户域现解的，见 admin-web/src/services/lottery.ts）。
			// 这里的目的是让日志能对回是哪家店，一个 id 就够，为它去调一次跨服务查询不值得。
			TargetName: p.LocationID,
			After:      audit.Snapshot(activationSnapshotOf(activation, campaign, round)),
		}); err != nil {
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
		(id,campaign_id,seq,round_no,status,participant_target,participant_count,winner_count)
		VALUES($1,$2,1,$3,'open',$4,0,$5)`,
		roundID, campaignID, model.RoundNo(campaign.Code, 1),
		campaign.ParticipantTarget, winnerCount); err != nil {
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
		(id,campaign_id,seq,round_no,status,participant_target,participant_count,winner_count)
		SELECT $1,$2,$3,$4,'open',$5,0,COALESCE(SUM(quantity),0)
		FROM lottery_campaign_prizes WHERE campaign_id=$2`,
		roundID, campaign.ID, seq, model.RoundNo(campaign.Code, seq),
		campaign.ParticipantTarget); err != nil {
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
		before, err := selectActivationSnapshot(ctx, tx, id)
		if err != nil {
			return err
		}
		// deactivated_at 由 status 决定，与 CHECK ((status='disabled') = (deactivated_at IS NOT NULL))
		// 绑定：写死一个「停用时间」而状态还是 enabled 会被数据库拒掉，那是好事。
		if _, err := tx.Exec(ctx, `UPDATE lottery_activations
			SET status=$2,
			    remark=CASE WHEN $3='' THEN remark ELSE $3 END,
			    deactivated_at=CASE WHEN $2='disabled' THEN NOW() ELSE NULL END,
			    updated_at=NOW()
			WHERE id=$1`, id, status, remark); err != nil {
			return err
		}
		// after 用 readActivation 读回**同一事务里**刚写的那一行：写这一条的 SQL 里有 CASE，
		// 用入参拼快照会把「remark 传空 = 保留原值」记成「备注被清空了」。
		updated, err := readActivation(ctx, tx, id)
		if err != nil {
			return err
		}
		if err := r.recorder.Record(ctx, tx, audit.Entry{
			Module: "lottery_activations", Action: "update_status", Operation: "修改门店抽奖状态",
			TargetType: "lottery_activation", TargetID: id, TargetName: updated.LocationID,
			Before: audit.Snapshot(before), After: audit.Snapshot(activationSnapshotOf(updated, nil, nil)),
		}); err != nil {
			return err
		}
		return nil
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
