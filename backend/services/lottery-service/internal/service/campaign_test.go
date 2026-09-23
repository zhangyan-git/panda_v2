package service

import (
	"errors"
	"testing"

	"github.com/panda-dev/panda-v2/backend/services/lottery-service/internal/dto"
	"github.com/panda-dev/panda-v2/backend/services/lottery-service/internal/model"
)

// 这一份打的是 campaignParams —— 建单与编辑**共用**的那个构造函数。它不碰数据库，也不碰
// s，所以用例只需要一个零值的 service。
//
// 它值一份测试是因为踩过一次：那个函数里曾经有一条「不能建成暂停中的活动」的规矩，而编辑
// 这条路紧接着就把 Status 覆盖成库里那一份（UpdateCampaign）。于是后台把状态改成「已暂停」
// 并保存时，提交被一个**马上要被丢掉**的字段打成 400，报的还是「当前状态不允许这样切换」，
// 连同一份提交里的名字都没存下去。

// buildableRequest 是一份**除此之外全部合法**的建单请求：只有 Status 是可变的那个输入。
// 其余字段都填满，是为了让用例失败时能咬定「是状态这一条判的」，而不是别的校验先拦下。
func buildableRequest(status string) dto.CampaignRequest {
	return dto.CampaignRequest{
		ActivationID:      "6f1b0f1e-0000-4000-8000-000000000001",
		Code:              "LAKE",
		Name:              "试运行活动",
		ParticipantTarget: 30,
		Status:            status,
		Prize: dto.PrizeRequest{
			Name:       "10 元咖啡兑换券",
			CoverImage: "https://example.com/cover.png",
		},
	}
}

func TestCampaignParamsAcceptsPausedStatus(t *testing.T) {
	// 后台的编辑弹窗把「已暂停」当成一个可选项，提交时原样带在请求体里。这个函数必须放它
	// 过去——拦它的是编辑那条路自己（不读这个字段）。
	params, err := (&LotteryService{}).campaignParams(buildableRequest(model.CampaignPaused), "6f1b0f1e-0000-4000-8000-000000000001", nil)
	if err != nil {
		t.Fatalf("paused 状态被拒了：%v（这条规矩属于 CreateCampaign，不属于共用的构造函数）", err)
	}
	if params.Status != model.CampaignPaused {
		t.Fatalf("params.Status = %q，期望 %q", params.Status, model.CampaignPaused)
	}
}

func TestCampaignParamsStillRejectsUnknownStatus(t *testing.T) {
	// 上面那条放行**不是**把状态校验整个删了：形状仍然要判，否则库里会多出一个 CHECK
	// 拦不住的值（那是一条 23514，翻出来谁都看不懂）。
	_, err := (&LotteryService{}).campaignParams(buildableRequest("running"), "6f1b0f1e-0000-4000-8000-000000000001", nil)
	if !errors.Is(err, ErrStatusInvalid) {
		t.Fatalf("未知状态 err = %v，期望 ErrStatusInvalid", err)
	}
}

func TestCampaignParamsDefaultsToDraftWhenStatusIsEmpty(t *testing.T) {
	// 编辑那条路把请求体里的 Status 清成空串再调这个函数（见 UpdateCampaign），所以这一格
	// 的默认值也一并钉住：它必须是一个合法的建单状态，而不是空串。
	params, err := (&LotteryService{}).campaignParams(buildableRequest(""), "6f1b0f1e-0000-4000-8000-000000000001", nil)
	if err != nil {
		t.Fatalf("空状态 err = %v，期望 nil", err)
	}
	if params.Status != model.CampaignDraft {
		t.Fatalf("params.Status = %q，期望 %q", params.Status, model.CampaignDraft)
	}
}
