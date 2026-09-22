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
// ended 是终态，**由运营主动结束**（活动已经没有时间窗口，没有「窗口走完自动结束」这条路
// 了）。把它改回 enabled 等于让一个已经开过 N 期的活动重新开期——那一段历史在新开的期次
// 里读不出来，会让人以为第 12 期是第 1 期。真要重来就新建一个活动，留下的是两条清清楚楚
// 的记录，而不是一条被反复改写的。
var ErrCampaignEnded = errors.New("campaign has already ended")

// ErrCampaignStatusTransition：这次状态变化不被允许。
var ErrCampaignStatusTransition = errors.New("campaign status transition is not allowed")

// CampaignDetail 是活动详情：活动本身 + 那个奖品。
//
// 奖品单独读一次而不是塞进 CampaignListRow 的一个字段：列表接口**不带**它（一页 20 个活动、
// 每个带一张图，列表响应会白胖一圈），把它变成富行的一个字段会诱使列表也去填它。
//
// Prize 可以是 nil：奖池表允许一个活动暂时没有奖品行（老数据、或者哪天有人直接改库）。
// 详情页照着「还没有奖品」渲染，而不是给一个零值奖品——那会显示成一个名字为空的奖品。
type CampaignDetail struct {
	View  *repository.CampaignListRow
	Prize *model.CampaignPrize
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
		if _, err := s.repository.EnsureLiveRound(ctx, campaign.ID); err != nil {
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

	// 读-判-写之间那个窗口由 checkCampaignTransition 与这次 UPDATE 之间的**唯一一条**
	// 数据库往返收窄；真正的兜底是「改状态不碰期次」——即使两个人同时按，改出来的也只有
	// 一个状态，而期次的开与不开由 EnsureLiveRound 自己的唯一索引兜（一期不会开两次）。
	if _, err := s.repository.UpdateCampaignStatus(ctx, id, status, actor); err != nil {
		return nil, err
	}
	if status == model.CampaignEnabled {
		if _, err := s.repository.EnsureLiveRound(ctx, id); err != nil {
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
	s.fillCampaignNames(ctx, []*repository.CampaignListRow{view})
	prizes, err := s.repository.ListPrizes(ctx, id)
	if err != nil {
		return nil, err
	}
	detail := &CampaignDetail{View: view}
	if len(prizes) > 0 {
		detail.Prize = prizes[0]
	}
	return detail, nil
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
	rows, total, err := s.repository.ListCampaigns(ctx, q)
	if err != nil {
		return nil, 0, err
	}
	s.fillCampaignNames(ctx, rows)
	return rows, total, nil
}

// ListPrizes 读一个活动的奖品，零个或一个。
//
// 独立成一个动作是因为它有一条自己的路由（GET /campaigns/{id}/prizes），而奖品在修改活动
// 时是整份替换的——没有按行改的接口。后台的活动详情已经不再单独调它（详情响应里带上了
// prize），留着是因为删一条没人调的接口不在这次改动范围里。
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
	// 一个奖品，不是一张清单。名字为空就是「没给奖品」——没有单独的「奖池不能为空」错误：
	// 一个没有奖品的抽奖活动不是「还没配好」，是一个说不通的东西（开期时 winner_count 会是
	// 0，而那一列有 CHECK (> 0)），所以缺名字这一条同时挡住它。
	prizeName := strings.TrimSpace(req.Prize.Name)
	if prizeName == "" {
		return repository.CampaignParams{}, ErrPrizeNameRequired
	}
	// 封面必填。**这一条才是真的闸门**：后台表单上也拦一道，但那个上传组件在本仓没有传
	// rules 的先例（类型上接得到，没被跑过）。开通模板建出的奖品是唯一能绕过它的路径，
	// 那是有意的（见 repository.DefaultPrize）。
	coverImage := strings.TrimSpace(req.Prize.CoverImage)
	if coverImage == "" {
		return repository.CampaignParams{}, ErrPrizeCoverRequired
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

	// 带回来的 id 决定服务端是原地改那一行还是插一行新的（见 repository.replacePrize）。
	// 非 uuid 的 id 在这里就拒掉：它进 SQL 的 uuid 比较会变成一条 22P02，也就是一个 500，
	// 而这只可能是调用方自己拼错了。
	prizeID := strings.TrimSpace(req.Prize.ID)
	if prizeID != "" {
		if _, err := uuid.Parse(prizeID); err != nil {
			return repository.CampaignParams{}, ErrPrizeIDInvalid
		}
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
		Status:            status,
		Prize: repository.PrizeInput{
			ID:         prizeID,
			Name:       prizeName,
			CoverImage: coverImage,
			// 海报选填：留空时前端回落到原型里那块横幅占位。
			PosterImage:       strings.TrimSpace(req.Prize.PosterImage),
			ClaimInstructions: strings.TrimSpace(req.Prize.ClaimInstructions),
			// 名额恒为 1。**这里是「每期开几个人」唯一的决定处**：接口收不到它，开期时
			// 它被求和冻结成期次的 winner_count，之后改这里不影响已经开出的期次。
			Quantity: 1,
		},
		Actor: actor,
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
