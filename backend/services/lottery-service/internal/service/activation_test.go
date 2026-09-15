package service

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/panda-dev/panda-v2/backend/services/lottery-service/internal/dto"
	"github.com/panda-dev/panda-v2/backend/services/lottery-service/internal/model"
	"github.com/panda-dev/panda-v2/backend/services/lottery-service/internal/repository"
)

// 「开通门店抽奖」在服务层做的只有两件事：把请求补成一份完整的内置模板，和把仓储返回的
// 那三行换成一个统一的读回形状。所以这一份测试打的就是这两件事——**模板的每个字段值**
// 和**回读的是读回值而不是拼出来的第四个版本**。
//
// 这条路径原来一条服务层测试都没有（仓储层的集成测试覆盖了 Activate 的写，但模板是这一层
// 拼的，它看不见）。没有它，后台表单里那句「留空则用内置模板（活动名取门店抽奖、门槛 30 人、
// 窗口从现在起 90 天）」曾经写着另外三个数字而没有任何东西会红——这不是假设，是真的发生过。

// activatedRow 是一条开通后的读回行。服务层只负责把它原样交出去，所以内容本身不重要，
// **指针是不是同一个**才重要（见 TestActivateReturnsTheReadBackRow）。
func activatedRow() *repository.ActivationListRow {
	return &repository.ActivationListRow{
		Activation:          &model.Activation{ID: "aaaa1111-1111-4111-8111-111111111111", LocationID: testLocationID},
		DefaultCampaignID:   testCampaignID,
		DefaultCampaignName: DefaultCampaignName,
		CampaignCount:       1,
		LiveRoundID:         testRoundID,
		LiveRoundNo:         "L66666666-0001",
		LiveRoundSize:       DefaultCampaignTarget,
		LiveRoundDone:       0,
	}
}

// activatableRepo 摆出一次成功的开通：写成功、回读成功。
func activatableRepo() *fakeRepository {
	return &fakeRepository{
		activateResult: &repository.ActivationCreated{
			Activation: &model.Activation{ID: "aaaa1111-1111-4111-8111-111111111111"},
			Campaign:   &model.Campaign{ID: testCampaignID},
			Round:      &model.Round{ID: testRoundID},
		},
		activationView: activatedRow(),
	}
}

// activateRequest 是一次只有必填项的开通请求——也就是「运营什么都不改，点确定」。
func activateRequest() dto.ActivateRequest {
	return dto.ActivateRequest{LocationID: testLocationID, LocationName: "朝阳门店"}
}

func TestActivateFillsTheBuiltInTemplate(t *testing.T) {
	repo := activatableRepo()
	svc := newTestService(repo, nil)

	if _, err := svc.Activate(context.Background(), activateRequest(), nil); err != nil {
		t.Fatalf("Activate: %v", err)
	}
	if repo.activateCalls != 1 {
		t.Fatalf("Activate 交给仓储 %d 次，要的是 1 次——开通记录、默认活动、第一期必须在同一个事务里，拆分调用就没有事务了", repo.activateCalls)
	}
	got := repo.activateParams.Campaign

	// 下面四个值**刻意写字面量而不是引用 Default\* 常量**：拿常量和常量比是句废话，
	// 改掉常量两边的断言会一起跟着变，测试照样绿。而这里要挡的正是「模板被改掉」这件事
	// ——后台表单那句说明（admin-web/src/pages/lottery/activations/index.tsx）逐字复述了
	// 这四个数，它们曾经和这份模板对不上（表单写着「门店名 / 10 人」，后端建的是
	// 「门店抽奖 / 30 人」）而没有任何东西会红。所以这里钉住字面量：改模板就要改这里，
	// 而这条注释会把人领到表单那句话上，逼着两处一起改。
	if got.Name != "门店抽奖" {
		t.Errorf("活动名 = %q，模板是「门店抽奖」", got.Name)
	}
	if got.ParticipantTarget != 30 {
		t.Errorf("门槛 = %d，模板是 30 人", got.ParticipantTarget)
	}
	if got.StartAt != testNow {
		t.Errorf("开始时间 = %v，没给窗口时应当取「现在」%v", got.StartAt, testNow)
	}
	if want := testNow.Add(90 * 24 * time.Hour); !got.EndAt.Equal(want) {
		t.Errorf("结束时间 = %v，模板是现在起 90 天", got.EndAt)
	}
	if got.Prize.Name != "神秘礼品" {
		t.Errorf("奖品名 = %q，模板是「神秘礼品」", got.Prize.Name)
	}
	if got.Prize.Quantity != 1 {
		t.Errorf("奖品名额 = %d，模板是 1——开箱即用的门店抽奖就是抽一个人", got.Prize.Quantity)
	}
	if got.Prize.PrizeKind != model.PrizeKindCustom {
		t.Errorf("奖品类型 = %q，模板是 %q", got.Prize.PrizeKind, model.PrizeKindCustom)
	}
	// 短名由门店 id 派生而不是写死：code 上有全局唯一索引，写死意味着第二家店开通即撞车。
	// 这里断言它确实是派生值（L + 门店 uuid 前 8 位），因为「换成一个好看的常量」是个
	// 看起来更整洁、却会让第二家门店开通失败的改动。
	if want := repository.DefaultCampaignCode(testLocationID); got.Code != want {
		t.Errorf("活动短名 = %q，应当由门店 id 派生得到 %q", got.Code, want)
	}
	// 描述是写死的一句，只断言非空：它不是接口契约，改文案不该让测试红。
	if strings.TrimSpace(got.Description) == "" {
		t.Error("活动描述是空的：活动详情页会显示一句空白说明")
	}
	// 门店名与操作人是照抄请求的，不是回查商户服务得来的（见 dto.ActivateRequest 的说明）。
	if repo.activateParams.LocationName != "朝阳门店" {
		t.Errorf("门店名 = %q", repo.activateParams.LocationName)
	}
	if repo.activateParams.ActivatedBy != nil {
		t.Error("没传 actor 却写进了操作人")
	}
}

func TestActivateHonoursWhatTheOperatorGave(t *testing.T) {
	repo := activatableRepo()
	svc := newTestService(repo, nil)

	target := int32(7)
	start := testNow.Add(24 * time.Hour)
	end := testNow.Add(48 * time.Hour)
	actor := "admin-1"
	req := dto.ActivateRequest{
		LocationID:        testLocationID,
		LocationName:      "  望京门店  ",
		CampaignName:      "  周年庆  ",
		ParticipantTarget: &target,
		StartAt:           &start,
		EndAt:             &end,
		Remark:            "  周年庆专场  ",
	}
	if _, err := svc.Activate(context.Background(), req, &actor); err != nil {
		t.Fatalf("Activate: %v", err)
	}
	got := repo.activateParams

	if got.Campaign.Name != "周年庆" {
		t.Errorf("活动名 = %q，运营填的应当原样生效（只 trim）", got.Campaign.Name)
	}
	if got.Campaign.ParticipantTarget != 7 {
		t.Errorf("门槛 = %d，运营填的是 7", got.Campaign.ParticipantTarget)
	}
	if !got.Campaign.StartAt.Equal(start) || !got.Campaign.EndAt.Equal(end) {
		t.Errorf("窗口 = [%v, %v]，运营填的是 [%v, %v]", got.Campaign.StartAt, got.Campaign.EndAt, start, end)
	}
	// 门店名与备注是照抄请求的快照，进库前 trim：首尾空格会让列表页那一列参差不齐。
	if got.LocationName != "望京门店" {
		t.Errorf("门店名 = %q", got.LocationName)
	}
	if got.Remark != "周年庆专场" {
		t.Errorf("备注 = %q", got.Remark)
	}
	if got.ActivatedBy == nil || *got.ActivatedBy != actor {
		t.Error("操作人没有落到开通记录上——「谁在什么时候开的」就答不出来了")
	}
}

// 只给一个窗口端点也要成立：运营常常只想改结束时间，把「只填了一个」变成一次校验失败
// 是拿我们自己造出来的规矩挡人。
func TestActivateAcceptsHalfAWindow(t *testing.T) {
	end := testNow.Add(10 * time.Hour)
	repo := activatableRepo()
	svc := newTestService(repo, nil)

	req := activateRequest()
	req.EndAt = &end
	if _, err := svc.Activate(context.Background(), req, nil); err != nil {
		t.Fatalf("Activate: %v", err)
	}
	if !repo.activateParams.Campaign.StartAt.Equal(testNow) {
		t.Errorf("开始时间 = %v，没给时应当回落到现在", repo.activateParams.Campaign.StartAt)
	}
	if !repo.activateParams.Campaign.EndAt.Equal(end) {
		t.Errorf("结束时间 = %v，运营填的是 %v", repo.activateParams.Campaign.EndAt, end)
	}
}

// 空白活动名回落到模板：后台那个输入框留空发上来的可能是 ""，也可能是几个空格，
// 而一个名叫「   」的活动在列表里看起来就是坏掉了。
func TestActivateTreatsABlankNameAsUnset(t *testing.T) {
	repo := activatableRepo()
	svc := newTestService(repo, nil)

	req := activateRequest()
	req.CampaignName = "   "
	if _, err := svc.Activate(context.Background(), req, nil); err != nil {
		t.Fatalf("Activate: %v", err)
	}
	if repo.activateParams.Campaign.Name != DefaultCampaignName {
		t.Errorf("活动名 = %q，空白应当回落到模板的 %q", repo.activateParams.Campaign.Name, DefaultCampaignName)
	}
}

func TestActivateRejectsInvalidRequests(t *testing.T) {
	badTarget := int32(0)
	negative := int32(-3)
	sameInstant := testNow
	before := testNow.Add(-time.Hour)

	// 窗口两端相等也算不成立：那一期开出来就结束了，参与的人一张卡也花不出去。
	cases := []struct {
		name string
		req  dto.ActivateRequest
		want error
	}{
		{"没有门店 id", dto.ActivateRequest{LocationName: "朝阳门店"}, ErrLocationIDRequired},
		{"门店 id 是全空格", dto.ActivateRequest{LocationID: "   ", LocationName: "朝阳门店"}, ErrLocationIDRequired},
		{"门店 id 不是 uuid", dto.ActivateRequest{LocationID: "store-1", LocationName: "朝阳门店"}, ErrLocationIDInvalid},
		{"没有门店名", dto.ActivateRequest{LocationID: testLocationID}, ErrLocationNameRequired},
		{"门店名是全空格", dto.ActivateRequest{LocationID: testLocationID, LocationName: "  "}, ErrLocationNameRequired},
		{"门槛是 0", dto.ActivateRequest{LocationID: testLocationID, LocationName: "朝阳门店", ParticipantTarget: &badTarget}, ErrTargetNotPositive},
		{"门槛是负数", dto.ActivateRequest{LocationID: testLocationID, LocationName: "朝阳门店", ParticipantTarget: &negative}, ErrTargetNotPositive},
		{"窗口两端相等", dto.ActivateRequest{LocationID: testLocationID, LocationName: "朝阳门店", StartAt: &sameInstant, EndAt: &sameInstant}, ErrCampaignWindowInvalid},
		{"窗口结束早于开始", dto.ActivateRequest{LocationID: testLocationID, LocationName: "朝阳门店", StartAt: &sameInstant, EndAt: &before}, ErrCampaignWindowInvalid},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			repo := activatableRepo()
			svc := newTestService(repo, nil)
			if _, err := svc.Activate(context.Background(), tc.req, nil); !errors.Is(err, tc.want) {
				t.Fatalf("err = %v，要的是 %v", err, tc.want)
			}
			// 校验失败必须在**碰仓储之前**发生：这些请求一个字节都不该写进库。
			if repo.activateCalls != 0 {
				t.Errorf("请求没通过校验却调了仓储 %d 次", repo.activateCalls)
			}
		})
	}
}

// 开通返回的是**回读出来的那一行**，不是把写的结果手工拼成的第四个形状。
//
// 三处（列表页、详情页、开通后的返回）必须是同一份形状，手工拼的那个迟早会少一个字段
// ——比如漏掉 LiveRoundNo，于是开通完的页面上「进行中的期次」那一格是空的，刷新一下又有。
// 所以这里断言拿到的是仓储给的那个指针本身。
func TestActivateReturnsTheReadBackRow(t *testing.T) {
	repo := activatableRepo()
	svc := newTestService(repo, nil)

	row, err := svc.Activate(context.Background(), activateRequest(), nil)
	if err != nil {
		t.Fatalf("Activate: %v", err)
	}
	if row != repo.activationView {
		t.Fatalf("返回的不是回读行（%p ≠ %p）——它多半是被手工拼出来的", row, repo.activationView)
	}
	if repo.viewCalls != 1 {
		t.Errorf("回读了 %d 次，要的是 1 次", repo.viewCalls)
	}
}

// 仓储写失败时把它原样往上抛，不去回读一个并不存在的开通记录。
func TestActivatePropagatesAWriteFailure(t *testing.T) {
	sentinel := errors.New("duplicate key value violates unique constraint")
	repo := activatableRepo()
	repo.activateErr = sentinel
	svc := newTestService(repo, nil)

	// 重复开通同一家门店就是这条路径（有 UNIQUE (location_id)）。回 409 而不是 500 由
	// controller 负责，这一层要保证的是**错误原样传上去**，别被吞成了「开通成功」。
	if _, err := svc.Activate(context.Background(), activateRequest(), nil); !errors.Is(err, sentinel) {
		t.Fatalf("err = %v，要的是仓储那个错", err)
	}
	if repo.viewCalls != 0 {
		t.Errorf("写都失败了还回读了 %d 次", repo.viewCalls)
	}
}
