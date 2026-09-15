package service

import (
	"context"
	"errors"
	"strings"

	"github.com/google/uuid"

	"github.com/panda-dev/panda-v2/backend/services/lottery-service/internal/dto"
	"github.com/panda-dev/panda-v2/backend/services/lottery-service/internal/model"
	"github.com/panda-dev/panda-v2/backend/services/lottery-service/internal/repository"
)

// ErrCampaignEnded：活动已经结束，不能再改回别的状态。
//
// ended 是终态：它落下来只有两种原因——窗口走完了，或者运营主动结束了。把它改回 enabled
// 等于让一个已经开过 N 期的活动重新开期，而它的 start_at / end_at 还在过去，开出来的第一期
// 一开门就到点。真要重来就新建一个活动——那留下的是两条清清楚楚的记录，而不是一条被反复
// 改写的。
var ErrCampaignEnded = errors.New("campaign has already ended")

// ErrCampaignWindowOver：活动的窗口已经过了，启用它开不出一期。
//
// 它单独成一条而不是让 EnsureLiveRound 静默返回 nil：管理员点「启用」得到的回应必须是
// 「这活动的时间已经过了」，而不是一个 200 加一个没有期次的活动——那种成功比失败更难查。
var ErrCampaignWindowOver = errors.New("campaign end_at is in the past")

// ErrCampaignStatusTransition：这次状态变化不被允许。
var ErrCampaignStatusTransition = errors.New("campaign status transition is not allowed")

// CampaignDetail 是活动详情：活动本身 + 奖池。
//
// 奖池单独读一次而不是塞进 CampaignListRow 的一个字段：列表接口**不带**奖池（一页 20 个
// 活动、每个 5 个奖品，列表就成了奖池查询），把它变成富行的一个字段会诱使列表也去填它。
type CampaignDetail struct {
	View   *repository.CampaignListRow
	Prizes []*model.CampaignPrize
}

// CreateCampaign 新建一个活动（含奖池）。
//
// 新建时**不自动开期**：只有请求里明确要 enabled 的才紧接着开第一期，其余停在 draft。
// 这与「开通即开期」不同，理由也不同——开通是一个明确的「现在开始运营」动作，而新建活动
// 常常是先配好、过两天再启用。
func (s *LotteryService) CreateCampaign(ctx context.Context, req dto.CampaignRequest, actor *string) (*CampaignDetail, error) {
	activationID := strings.TrimSpace(req.ActivationID)
	if activationID == "" {
		return nil, ErrCampaignIDRequired
	}
	if _, err := uuid.Parse(activationID); err != nil {
		return nil, ErrCampaignIDInvalid
	}
	params, err := s.campaignParams(req, activationID, actor)
	if err != nil {
		return nil, err
	}
	// 开通记录必须存在——否则活动会挂在一个不存在的门店上（外键会拒，但那是一条 23503，
	// 在这里先给一句人话）。
	if _, err := s.repository.GetActivationView(ctx, activationID); err != nil {
		return nil, err
	}

	campaign, err := s.repository.CreateCampaign(ctx, params)
	if err != nil {
		return nil, err
	}
	if campaign.Status == model.CampaignEnabled {
		if _, err := s.repository.EnsureLiveRound(ctx, campaign.ID, s.now()); err != nil {
			return nil, err
		}
	}
	return s.GetCampaign(ctx, campaign.ID)
}

// UpdateCampaign 改一个活动与它的奖池。
//
// **状态不在这个接口里改**：请求体提交的 Status 被忽略，用的永远是库里那一份。改生命周期
// 走 SetCampaignStatus 那一个入口——否则一次「改个名字」的提交顺手把 ended 写回 enabled，
// 而这个动作没有任何地方会拦。
func (s *LotteryService) UpdateCampaign(ctx context.Context, id string, req dto.CampaignRequest, actor *string) (*CampaignDetail, error) {
	if _, err := uuid.Parse(strings.TrimSpace(id)); err != nil {
		return nil, ErrCampaignIDInvalid
	}
	existing, err := s.repository.GetCampaign(ctx, id)
	if err != nil {
		return nil, err
	}
	params, err := s.campaignParams(req, existing.ActivationID, actor)
	if err != nil {
		return nil, err
	}
	params.Status = existing.Status
	// 活动不能换门店（仓储的 UPDATE 里没有 activation_id）。换门店等于新建一个活动。
	if _, err := s.repository.UpdateCampaign(ctx, id, params); err != nil {
		return nil, err
	}
	return s.GetCampaign(ctx, id)
}

// SetCampaignStatus 启用 / 暂停 / 结束一个活动。
//
// 三条规矩，每一条都对应运营能理解的一句话：
//
//   - 进入 enabled 时**补开一期**（EnsureLiveRound）。暂停停的是下一期，所以一个「暂停后
//     恢复」的活动会处于「enabled 但没有在跑的期次」——没有这一步，恢复就成了一次什么也
//     不做的点击，抽奖中心会一直显示「暂无进行中的活动」。
//   - 暂停 / 结束时**不动正在跑的那一期**：它照常开奖。已经收了 N 个人的参与，不能因为
//     运营点了暂停就把他们手里的卡吞掉（见 model.CampaignPaused）。
//   - ended 是终态，不可逆（见 ErrCampaignEnded）。
func (s *LotteryService) SetCampaignStatus(ctx context.Context, id, rawStatus string, actor *string) (*CampaignDetail, error) {
	if _, err := uuid.Parse(strings.TrimSpace(id)); err != nil {
		return nil, ErrCampaignIDInvalid
	}
	status, err := normaliseCampaignStatus(rawStatus)
	if err != nil {
		return nil, err
	}

	existing, err := s.repository.GetCampaign(ctx, id)
	if err != nil {
		return nil, err
	}
	if err := checkCampaignTransition(existing.Status, status); err != nil {
		return nil, err
	}
	if status == model.CampaignEnabled && !s.now().Before(existing.EndAt) {
		// 窗口已过还去启用：开不出一期，活动会显示成「启用中、0 期」。与其让管理员看到
		// 一个说不通的状态，不如在这里说清楚。
		return nil, ErrCampaignWindowOver
	}

	// 读-判-写之间那个窗口由 checkCampaignTransition 与这次 UPDATE 之间的**唯一一条**
	// 数据库往返收窄；真正的兜底是「改状态不碰期次」——即使两个人同时按，改出来的也只有
	// 一个状态，而期次的开与不开由 EnsureLiveRound 自己的唯一索引兜（一期不会开两次）。
	if _, err := s.repository.UpdateCampaignStatus(ctx, id, status, actor); err != nil {
		return nil, err
	}
	if status == model.CampaignEnabled {
		if _, err := s.repository.EnsureLiveRound(ctx, id, s.now()); err != nil {
			return nil, err
		}
	}
	return s.GetCampaign(ctx, id)
}

// checkCampaignTransition 判定一次状态变化是否允许。
//
// 允许的只有这几条，其余一律拒绝：
//
//	draft   → draft / enabled / ended   配好了就启用；不想要了就结束
//	enabled → draft / enabled / paused / ended
//	paused  → enabled / paused / ended  恢复或结束
//	ended   → （无）                     终态
//
// 同状态到同状态（enabled → enabled）是允许的：重复表达同一个意图不是错误，而且后台的按钮
// 可能被点两下。
//
// 刻意**拦掉**的是两种「回头」：ended 之后的任何变化，以及 draft → paused。后者不是安全
// 考虑——它只是说不通：一个从没开过期、也没人看得见的活动被标成「暂停中」，读到一个从来没有
// 存在过的运营动作。不想要一个 draft 就结束它。
func checkCampaignTransition(from, to string) error {
	if from == to {
		return nil
	}
	switch from {
	case model.CampaignDraft:
		switch to {
		case model.CampaignEnabled, model.CampaignEnded:
			return nil
		}
	case model.CampaignEnabled:
		switch to {
		case model.CampaignDraft, model.CampaignPaused, model.CampaignEnded:
			return nil
		}
	case model.CampaignPaused:
		switch to {
		case model.CampaignEnabled, model.CampaignEnded:
			return nil
		}
	case model.CampaignEnded:
		return ErrCampaignEnded
	}
	return ErrCampaignStatusTransition
}

// GetCampaign 读一个活动（含奖池）。
func (s *LotteryService) GetCampaign(ctx context.Context, id string) (*CampaignDetail, error) {
	if _, err := uuid.Parse(strings.TrimSpace(id)); err != nil {
		return nil, ErrCampaignIDInvalid
	}
	view, err := s.repository.GetCampaignView(ctx, id)
	if err != nil {
		return nil, err
	}
	prizes, err := s.repository.ListPrizes(ctx, id)
	if err != nil {
		return nil, err
	}
	return &CampaignDetail{View: view, Prizes: prizes}, nil
}

// ListCampaigns 分页读活动（不带奖池明细）。
func (s *LotteryService) ListCampaigns(ctx context.Context, q dto.CampaignQuery) ([]*repository.CampaignListRow, int, error) {
	for _, id := range []string{q.ActivationID, q.LocationID, q.MachineID} {
		if id == "" {
			continue
		}
		if _, err := uuid.Parse(id); err != nil {
			return nil, 0, ErrCampaignIDInvalid
		}
	}
	if q.Status != "" {
		if _, err := normaliseCampaignStatus(q.Status); err != nil {
			return nil, 0, err
		}
	}
	return s.repository.ListCampaigns(ctx, q)
}

// ListPrizes 读一个活动的奖池。
//
// 独立成一个动作是因为后台的活动详情页要单独刷新它（改完奖池不等整页重读），而奖池在修改
// 活动时是整体替换的——没有按行读接口。
func (s *LotteryService) ListPrizes(ctx context.Context, campaignID string) ([]*model.CampaignPrize, error) {
	if _, err := uuid.Parse(strings.TrimSpace(campaignID)); err != nil {
		return nil, ErrCampaignIDInvalid
	}
	return s.repository.ListPrizes(ctx, campaignID)
}

// normaliseCampaignStatus 校验并归一化活动状态，三个值都不接受空。
//
// 归一化（Trim + ToLower）而不是原样透传：这些词直接进 SQL 的等值比较，多一个空格会让
// 「筛启用中的」静默变成「筛一个没有的取值」，页面上是一张空表，看起来像「确实没有」。
func normaliseCampaignStatus(raw string) (string, error) {
	status := strings.ToLower(strings.TrimSpace(raw))
	switch status {
	case "":
		return "", ErrStatusRequired
	case model.CampaignDraft, model.CampaignEnabled, model.CampaignPaused, model.CampaignEnded:
		return status, nil
	default:
		return "", ErrStatusInvalid
	}
}

// campaignParams 把请求体校验成仓储要的形状。
//
// activationID 由调用方给而不是从请求体读：新建时它来自请求（必填），修改时它来自库里那
// 一份（活动不能换门店，请求体里那个字段被忽略）。
func (s *LotteryService) campaignParams(req dto.CampaignRequest, activationID string, actor *string) (repository.CampaignParams, error) {
	code := strings.TrimSpace(req.Code)
	if code == "" {
		return repository.CampaignParams{}, ErrCampaignCodeRequired
	}
	if !model.ValidCampaignCode(code) {
		// 形状校验放在这里而不是等数据库的 CHECK 拒：23514 翻出来是一句谁都看不懂的话，
		// 而运营需要知道的只是「短名只能是大写字母和数字，最多 16 位」。
		return repository.CampaignParams{}, ErrCampaignCodeInvalid
	}
	name := strings.TrimSpace(req.Name)
	if name == "" {
		return repository.CampaignParams{}, ErrCampaignNameRequired
	}
	if req.ParticipantTarget <= 0 {
		return repository.CampaignParams{}, ErrTargetNotPositive
	}
	if req.StartAt.IsZero() || req.EndAt.IsZero() || !req.EndAt.After(req.StartAt) {
		return repository.CampaignParams{}, ErrCampaignWindowInvalid
	}
	if len(req.Prizes) == 0 {
		// 空的奖池意味着开期时 winner_count 是 0，而那一列有 CHECK (> 0)——一个没有奖品的
		// 抽奖活动不是「还没配好」，是一个说不通的东西。
		return repository.CampaignParams{}, ErrPrizesRequired
	}

	status := model.CampaignDraft
	if raw := strings.TrimSpace(req.Status); raw != "" {
		parsed, err := normaliseCampaignStatus(raw)
		if err != nil {
			return repository.CampaignParams{}, err
		}
		if parsed == model.CampaignPaused {
			// 「新建一个暂停中的活动」：它既没有在跑的期次，也没有人见过它，暂停这个词
			// 描述的是一个没发生过的动作。要留着就建 draft。
			return repository.CampaignParams{}, ErrCampaignStatusTransition
		}
		status = parsed
	}

	prizes := make([]repository.PrizeInput, 0, len(req.Prizes))
	seenOrder := make(map[int32]struct{}, len(req.Prizes))
	for _, prize := range req.Prizes {
		if !model.ValidPrizeKind(prize.PrizeKind) {
			return repository.CampaignParams{}, ErrPrizeKindInvalid
		}
		prizeName := strings.TrimSpace(prize.Name)
		if prizeName == "" {
			return repository.CampaignParams{}, ErrPrizeNameRequired
		}
		if prize.Quantity <= 0 {
			return repository.CampaignParams{}, ErrPrizeQuantityInvalid
		}
		// sort_order 上有 (campaign_id, sort_order) 唯一索引，重复的值会撞成一条 23505。
		// 在这里先拦，是因为给前端的报错要说得出「第几行和第几行重了」，而唯一索引只会说
		// 「有一行重了」。顺带一提：负数 sort_order 也被这一条挡住（前端从 0 起编号）。
		if _, dup := seenOrder[prize.SortOrder]; dup {
			return repository.CampaignParams{}, ErrPrizeSortOrderConflict
		}
		seenOrder[prize.SortOrder] = struct{}{}
		prizes = append(prizes, repository.PrizeInput{
			SortOrder:         prize.SortOrder,
			PrizeKind:         prize.PrizeKind,
			Name:              prizeName,
			CouponTemplateID:  strings.TrimSpace(prize.CouponTemplateID),
			ImageURL:          strings.TrimSpace(prize.ImageURL),
			ClaimInstructions: strings.TrimSpace(prize.ClaimInstructions),
			Quantity:          prize.Quantity,
		})
	}

	machineID, err := optionalUUID(req.MachineID, ErrMachineIDInvalid)
	if err != nil {
		return repository.CampaignParams{}, err
	}

	return repository.CampaignParams{
		ActivationID:      activationID,
		MachineID:         machineID,
		Code:              code,
		Name:              name,
		Description:       strings.TrimSpace(req.Description),
		ParticipantTarget: req.ParticipantTarget,
		StartAt:           req.StartAt,
		EndAt:             req.EndAt,
		Status:            status,
		Prizes:            prizes,
		Actor:             actor,
	}, nil
}

// optionalUUID 把一个「可空、空串等于没值」的 uuid 字段校验成一个指针。
//
// 空串要**收成 nil 而不是原样往下传**：machine_id 列是可空的 uuid，而一个空字符串进
// uuid 列会报 invalid input syntax（它表达的不是「没有值」）。nil 才是「门店级活动」。
func optionalUUID(raw *string, invalid error) (*string, error) {
	if raw == nil {
		return nil, nil
	}
	trimmed := strings.TrimSpace(*raw)
	if trimmed == "" {
		return nil, nil
	}
	if _, err := uuid.Parse(trimmed); err != nil {
		return nil, invalid
	}
	return &trimmed, nil
}
