package service

import (
	"context"
	"encoding/json"
	"regexp"
	"strings"

	"github.com/panda-dev/panda-v2/backend/services/membership-service/internal/dto"
	"github.com/panda-dev/panda-v2/backend/services/membership-service/internal/model"
	"github.com/panda-dev/panda-v2/backend/services/membership-service/internal/repository"
)

// planCodePattern 是套餐编码允许的形状：小写字母、数字、下划线，至少一个字符。
//
// 不做成「长度最多 64」那种更宽松的规则，是因为这个编码会出现在事件体、RabbitMQ 的路由键、
// 日志行与对账表里。限制在一个不需要转义的字符集里，是为了让它在这些地方**永远不用被转义**
// ——一个带空格或斜杠的编码迟早在某条 URL 或某句日志里被截断，而那时候已经有一批订单挂在
// 它上面了。
var planCodePattern = regexp.MustCompile(`^[a-z0-9_]+$`)

// planCodeMaxLength 是编码的长度上限。
//
// 它跟着库列的 TEXT 走（没有硬上限），但事件体会被投到 broker 上，路由键太长会撞上渠道的
// 限制。64 与 coupon_types.code 是同一个口径。
const planCodeMaxLength = 64

// planParams 把 dto.PlanRequest 校一遍并翻成写库的输入。
//
// 校验集中在这里而不是散在 create 与 update 两处：两者**共用同一套规则**（见 dto.PlanRequest
// 的说明），分两套只会让其中一套先腐烂。code 不在这里——它是不可修改的，只在新建时给。
func (s *MembershipService) planParams(req dto.PlanRequest) (repository.PlanParams, error) {
	name := strings.TrimSpace(req.Name)
	if name == "" {
		return repository.PlanParams{}, ErrPlanNameRequired
	}
	if req.PriceCents < 0 {
		return repository.PlanParams{}, ErrPlanPriceNegative
	}
	if req.PriceCents > maxPlanPriceCents {
		return repository.PlanParams{}, ErrPlanPriceTooLarge
	}

	period, err := parseEnum(req.Period, []string{model.PeriodMonth, model.PeriodYear}, model.PeriodMonth)
	if err != nil {
		return repository.PlanParams{}, ErrPlanPeriodInvalid
	}
	// 上界 120 防的是粘错：期数是一串手输的数字，多打两位就会让一次购买买断十年，而它在下单
	// 页上只显示成「1200 个月」。库上的 CHECK 只要求 > 0。
	periodCount := req.PeriodCount
	if periodCount <= 0 {
		periodCount = 1
	}
	if periodCount > 120 {
		return repository.PlanParams{}, ErrPlanPeriodCountRange
	}

	wechatPlanID := strings.TrimSpace(req.WechatPlanID)
	// 要签约就必须有模板 ID：没有它，payment-service 拿不到 plan_id，代扣协议建不出来，
	// 「开启了连续包月」这句话就是假的。反过来，不签约的套餐留着它会让运营以为配置生效了。
	// 库上两条 CHECK 也钉着，但它们报的是 23514（500），说不到用户能听懂的话。
	switch {
	case req.AutoRenew && wechatPlanID == "":
		return repository.PlanParams{}, ErrPlanWechatIDRequired
	case !req.AutoRenew && wechatPlanID != "":
		return repository.PlanParams{}, ErrPlanWechatIDNotAllowed
	}

	mode, err := parseEnum(req.MemberPriceMode,
		[]string{model.MemberPriceModeAuto, model.MemberPriceModeCoupon}, model.MemberPriceModeAuto)
	if err != nil {
		return repository.PlanParams{}, ErrPlanPriceModeInvalid
	}
	couponTemplate := strings.TrimSpace(req.MemberPriceCouponTemplateID)
	couponsPerPeriod := req.MemberPriceCouponsPerPeriod
	// 券配置与模式必须**配对**：coupon 全给、auto 全空。
	//
	// 这两条库里也有 CHECK（mode 与两列的 NULL 情况一一对应），但落到 23514 就是一条 500
	// ——而它其实是「表单里漏填了一格」。分开说清缺了哪一半，运营才知道去改哪里。
	switch mode {
	case model.MemberPriceModeCoupon:
		if couponTemplate == "" || couponsPerPeriod <= 0 {
			return repository.PlanParams{}, ErrPlanCouponFieldsRequired
		}
		if _, err := optionalUUID(couponTemplate, ErrPlanCouponFieldsRequired); err != nil {
			return repository.PlanParams{}, err
		}
	case model.MemberPriceModeAuto:
		if couponTemplate != "" || couponsPerPeriod != 0 {
			return repository.PlanParams{}, ErrPlanCouponFieldsNotAllowed
		}
	}

	benefits, err := encodeBenefits(req.Benefits)
	if err != nil {
		return repository.PlanParams{}, err
	}

	return repository.PlanParams{
		Name:                        name,
		Description:                 strings.TrimSpace(req.Description),
		Benefits:                    benefits,
		PriceCents:                  req.PriceCents,
		Period:                      period,
		PeriodCount:                 periodCount,
		AutoRenew:                   req.AutoRenew,
		WechatPlanID:                wechatPlanID,
		MemberPriceMode:             mode,
		MemberPriceCouponTemplateID: optionalString(couponTemplate),
		MemberPriceCouponsPerPeriod: optionalInt32(couponsPerPeriod),
		SortOrder:                   req.SortOrder,
		Status:                      model.PlanStatusDraft,
	}, nil
}

// encodeBenefits 把权益文案序列化成 JSONB 要写的那串字节。
//
// 它把「空列表」也写成 `[]` 而不是 NULL：库上有 NOT NULL DEFAULT，但直接改库能把列写成
// NULL，而读路径会在解析处炸——炸的是「所有套餐都列不出来」。这里统一成合法 JSON，
// 读路径那边再 COALESCE 一层，两头都不会给出一个解析不了的值。
func encodeBenefits(lines []string) ([]byte, error) {
	cleaned := make([]string, 0, len(lines))
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			// 空行直接丢掉，而不是报错：后台那个「权益说明」多半是个可以加行的输入框，
			// 用户加了一行没填就保存是常态，为它弹一次错只会让人以为保存失败了。
			continue
		}
		if len([]rune(trimmed)) > MaxBenefitLength {
			return nil, ErrPlanBenefitTooLong
		}
		cleaned = append(cleaned, trimmed)
	}
	if len(cleaned) > MaxBenefitLines {
		return nil, ErrPlanBenefitsTooMany
	}
	encoded, err := json.Marshal(cleaned)
	if err != nil {
		// 入参全是 []string，唯一的失败可能是内存不够。包成一条校验错误是错的——那不是用户
		// 的问题。原样往上抛，由 controller 记 500。
		return nil, err
	}
	return encoded, nil
}

// optionalString / optionalInt32 把零值翻成 nil，供库上那两列可空的券配置使用。
func optionalString(value string) *string {
	if value == "" {
		return nil
	}
	return &value
}

func optionalInt32(value int32) *int32 {
	if value == 0 {
		return nil
	}
	return &value
}

// ============================================================
// 套餐
// ============================================================

// CreatePlan 新建一个套餐，落在 draft。
//
// **新建永远是 draft**，请求体里没有 status 这个字段：一个还没配完的套餐直接上架，用户会在
// 小程序上看到它并买下来，而它的签约模板可能还是空的。上架是**另一个动作**（SetPlanStatus），
// 要人来点第二下。
func (s *MembershipService) CreatePlan(ctx context.Context, req dto.CreatePlanRequest, adminID string) (*model.Plan, error) {
	code := strings.TrimSpace(req.Code)
	if code == "" {
		return nil, ErrPlanCodeRequired
	}
	if len(code) > planCodeMaxLength || !planCodePattern.MatchString(code) {
		return nil, ErrPlanCodeInvalid
	}
	params, err := s.planParams(req.PlanRequest)
	if err != nil {
		return nil, err
	}
	params.CreatedBy = optionalString(strings.TrimSpace(adminID))
	return s.repository.CreatePlan(ctx, code, params)
}

// UpdatePlan 改一个套餐的展示与销售参数。**不改 status**，也不改 code。
func (s *MembershipService) UpdatePlan(ctx context.Context, id string, req dto.PlanRequest) (*model.Plan, error) {
	planID, err := requiredID(id, ErrPlanIDRequired, ErrPlanIDInvalid)
	if err != nil {
		return nil, err
	}
	params, err := s.planParams(req)
	if err != nil {
		return nil, err
	}
	return s.repository.UpdatePlan(ctx, planID, params)
}

// SetPlanStatus 上下架 / 转草稿。
//
// 下架（disabled）**不影响已经买过的会员**：memberships 上存着成交快照，权益跟着那份快照走。
// 这一条是「改价、下架都不能追溯改写历史会员」那组不变式里最容易被人怀疑的一环，所以它在库
// 结构上就成立，不靠这里的任何判断。
func (s *MembershipService) SetPlanStatus(ctx context.Context, id, status string) (*model.Plan, error) {
	planID, err := requiredID(id, ErrPlanIDRequired, ErrPlanIDInvalid)
	if err != nil {
		return nil, err
	}
	target, err := parseEnum(status, []string{
		model.PlanStatusDraft, model.PlanStatusActive, model.PlanStatusDisabled,
	}, "")
	if err != nil || target == "" {
		// 空串也报错：上下架这个动作**必须**说明切到哪个状态，没有「默认状态」可言。
		return nil, ErrPlanStatusInvalid
	}
	return s.repository.SetPlanStatus(ctx, planID, target)
}

// GetPlan 按 ID 取套餐（后台详情/编辑页）。
func (s *MembershipService) GetPlan(ctx context.Context, id string) (*model.Plan, error) {
	planID, err := requiredID(id, ErrPlanIDRequired, ErrPlanIDInvalid)
	if err != nil {
		return nil, err
	}
	return s.repository.GetPlan(ctx, planID)
}

// GetSellablePlan 按 ID 取一个**可以卖**的套餐：order-service 下单定价走这一条。
//
// 与 GetPlan 分开是有意的：「存在」与「能买」是两件事。后台要能打开草稿套餐的编辑页（那正是
// 草稿存在的意义），而下单只能买上架的那一个——把这条判断塞进 GetPlan，后台的编辑页就再也
// 打不开还没配完的套餐了。
//
// 返回的两条错误分得很清：ErrPlanNotFound 是客户端拿了一个不存在的 id，ErrPlanNotSellable
// 是这个套餐此刻不卖（draft / disabled）。调用方该做的处理不同——换一个 id，还是等它上架。
func (s *MembershipService) GetSellablePlan(ctx context.Context, id string) (*model.Plan, error) {
	plan, err := s.GetPlan(ctx, id)
	if err != nil {
		return nil, err
	}
	if plan.Status != model.PlanStatusActive {
		return nil, ErrPlanNotSellable
	}
	return plan, nil
}

// ListPlans 是后台套餐列表。
func (s *MembershipService) ListPlans(ctx context.Context, query dto.PlanQuery) ([]*model.Plan, int, error) {
	normalizePlanQuery(&query)
	return s.repository.ListPlans(ctx, query)
}

// ListActivePlans 是小程序的套餐列表。
//
// 它**不接受任何筛选**：C 端要的是「现在能买的全部」，多一个参数就多一种「用户看不到某个
// 套餐」的可能，而那种事在界面上是看不出来的。
func (s *MembershipService) ListActivePlans(ctx context.Context) ([]*model.Plan, error) {
	return s.repository.ListActivePlans(ctx)
}

// normalizePlanQuery 把分页参数收进合法范围。
//
// pageSize 越界时**截断而不是报错**：分页参数是前端组件自己拼的，为一个它顺手写大的数字弹一次
// 400，用户看到的是「加载失败」，而他什么也没做错。这与「筛选条件写错了要报错」是两回事
// ——那些是用户手输的。
func normalizePlanQuery(query *dto.PlanQuery) {
	if query.Page < 1 {
		query.Page = 1
	}
	switch {
	case query.PageSize <= 0:
		query.PageSize = dto.DefaultPageSize
	case query.PageSize > MaxPageSize:
		query.PageSize = MaxPageSize
	}
}
