package service

import (
	"context"
	"strings"

	"github.com/google/uuid"

	"github.com/panda-dev/panda-v2/backend/services/lottery-service/internal/dto"
	"github.com/panda-dev/panda-v2/backend/services/lottery-service/internal/model"
	"github.com/panda-dev/panda-v2/backend/services/lottery-service/internal/repository"
)

// Activate 开通一家门店的抽奖：开通记录 + 默认活动 + 第一期，同一个事务。
//
// 开通是一个**动作**（有操作人、有时间），不是「有没有启用中的活动」派生出来的。派生会让
// 门店在两次开奖之间的空档里显示成「没开通」，抽奖中心会对着一个其实正常的状态亮空页；
// 派生也回答不了「谁在什么时候开的」。
//
// 默认活动由内置模板生成，运营可以在开通的同一个动作里改掉名字与门槛——但不改也能用，
// 这正是模板存在的理由：开箱即用的门店抽奖是「本期攒够 30 个人抽一份礼品」。
//
// 短名（code）**由门店 ID 派生**，不是写死一个好看的常量：code 上有全局唯一索引（它是
// 期次号的前缀），而开通是每家门店一次的动作，写死意味着第二家店开通即撞车
// （见 repository.DefaultCampaignCode）。
//
// 门店名**不由请求体带上来**（原来带，2026-09-15 去掉）：名字是商户域的事实，本库只存 id
// （见 migrations/lottery）。开通前先问一次商户域「这家店存在吗」，问不出来就不受理
// ——那一次调用顺带就把「幽灵门店」堵掉了，而名字在每次读的时候现解。
func (s *LotteryService) Activate(ctx context.Context, req dto.ActivateRequest, actor *string) (*repository.ActivationListRow, error) {
	locationID := strings.TrimSpace(req.LocationID)
	if locationID == "" {
		return nil, ErrLocationIDRequired
	}
	if _, err := uuid.Parse(locationID); err != nil {
		return nil, ErrLocationIDInvalid
	}
	target := int32(DefaultCampaignTarget)
	if req.ParticipantTarget != nil {
		if *req.ParticipantTarget <= 0 {
			return nil, ErrTargetNotPositive
		}
		target = *req.ParticipantTarget
	}
	name := strings.TrimSpace(req.CampaignName)
	if name == "" {
		name = DefaultCampaignName
	}
	// 门店存在性放在**本地校验之后**：一个连门店 id 都不合法的请求不该付一次跨服务往返，
	// 而且先报出来的是「你填错了」而不是「商户域不可达」——后者会让人去查一个没坏的东西。
	if err := s.requireStore(ctx, locationID); err != nil {
		return nil, err
	}

	created, err := s.repository.Activate(ctx, repository.ActivateParams{
		LocationID:  locationID,
		Remark:      strings.TrimSpace(req.Remark),
		ActivatedBy: actor,
		Campaign: repository.DefaultCampaign{
			Code:              repository.DefaultCampaignCode(locationID),
			Name:              name,
			Description:       "开通抽奖时自动创建，可在活动里修改或停用。",
			ParticipantTarget: target,
			// 两张图都留空。封面在后台表单上是必填，但开通**不**受那条约束：逼运营先找
			// 一张图才能开通，等于把整个抽奖域的入口抬高。空封面由前端落到占位块上，等
			// 真要办活动了，编辑这个默认活动时那条校验会要他传图（见 repository.DefaultPrize）。
			Prize: repository.DefaultPrize{
				Name:     DefaultPrizeName,
				Quantity: 1,
			},
		},
	})
	if err != nil {
		return nil, err
	}

	// 回读一次富行而不是手工拼响应：列表页、详情页、开通返回的这三份必须是同一份形状，
	// 手工拼出来的第四个版本迟早会少一个字段（比如漏掉 LiveRoundNo，于是开通完的页面上
	// 那一格是空的，而刷新一下又有了）。这一次读在开完通之后，代价是毫秒级。
	row, err := s.repository.GetActivationView(ctx, created.Activation.ID)
	if err != nil {
		return nil, err
	}
	s.fillActivationNames(ctx, []*repository.ActivationListRow{row})
	return row, nil
}

// UpdateActivationStatus 启用 / 停用一家门店的抽奖。
//
// 停用**不动正在跑的那一期**：已经收了 N 个人的参与，作废它们要 N 次跨服务冲正。停用停的
// 是「不再开新期」，与活动的 paused 是同一条规矩（见 model.CampaignPaused）。正在跑的那
// 一期照常开奖——不能因为运营点了停用就把参与者的卡吞掉。
func (s *LotteryService) UpdateActivationStatus(ctx context.Context, id string, req dto.UpdateActivationRequest, actor *string) (*repository.ActivationListRow, error) {
	if _, err := uuid.Parse(strings.TrimSpace(id)); err != nil {
		return nil, ErrLocationIDInvalid
	}
	status, err := normaliseActivationStatus(req.Status)
	if err != nil {
		return nil, err
	}
	if _, err := s.repository.UpdateActivationStatus(ctx, id, status, strings.TrimSpace(req.Remark), actor); err != nil {
		return nil, err
	}
	return s.GetActivation(ctx, id)
}

// GetActivation 读一条开通记录的富行。
func (s *LotteryService) GetActivation(ctx context.Context, id string) (*repository.ActivationListRow, error) {
	if _, err := uuid.Parse(strings.TrimSpace(id)); err != nil {
		return nil, ErrLocationIDInvalid
	}
	row, err := s.repository.GetActivationView(ctx, id)
	if err != nil {
		return nil, err
	}
	s.fillActivationNames(ctx, []*repository.ActivationListRow{row})
	return row, nil
}

// FindActivationByLocation 按门店读一条开通记录（「这家店开通了没有」）。
func (s *LotteryService) FindActivationByLocation(ctx context.Context, locationID string) (*model.Activation, error) {
	if _, err := uuid.Parse(strings.TrimSpace(locationID)); err != nil {
		return nil, ErrLocationIDInvalid
	}
	return s.repository.FindActivationByLocation(ctx, locationID)
}

// ListActivations 分页读开通记录。
func (s *LotteryService) ListActivations(ctx context.Context, q dto.ActivationQuery) ([]*repository.ActivationListRow, int, error) {
	if q.LocationID != "" {
		if _, err := uuid.Parse(q.LocationID); err != nil {
			return nil, 0, ErrLocationIDInvalid
		}
	}
	if q.Status != "" {
		if _, err := normaliseActivationStatus(q.Status); err != nil {
			return nil, 0, err
		}
	}
	rows, total, err := s.repository.ListActivations(ctx, q)
	if err != nil {
		return nil, 0, err
	}
	s.fillActivationNames(ctx, rows)
	return rows, total, nil
}

// normaliseActivationStatus 校验并归一化开通状态。
//
// 归一化（Trim + ToLower）而不是原样透传：这两个词直接进 SQL 的等值比较，多一个空格会让
// 「筛启用中的」静默变成「筛一个没有的取值」，页面上是一张空表，看起来像「确实没有」。
func normaliseActivationStatus(raw string) (string, error) {
	status := strings.ToLower(strings.TrimSpace(raw))
	switch status {
	case "":
		return "", ErrStatusRequired
	case model.ActivationEnabled, model.ActivationDisabled:
		return status, nil
	default:
		return "", ErrStatusInvalid
	}
}
