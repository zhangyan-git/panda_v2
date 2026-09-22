package service

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/panda-dev/panda-v2/backend/services/lottery-service/internal/dto"
	"github.com/panda-dev/panda-v2/backend/services/lottery-service/internal/model"
	"github.com/panda-dev/panda-v2/backend/services/lottery-service/internal/repository"
)

// 「开通门店抽奖」在服务层做的只有两件事：把请求补成一份完整的内置模板，和把仓储返回的
// 那三行换成一个统一的读回形状。所以这一份测试打的就是这两件事——**模板的每个字段值**
// 和**回读的是读回值而不是拼出来的第四个版本**。
//
// 这条路径原来一条服务层测试都没有（仓储层的集成测试覆盖了 Activate 的写，但模板是这一层
// 拼的，它看不见）。没有它，后台表单里那句「留空则用内置模板（活动名取门店抽奖、门槛 30 次）」
// 曾经写着另外几个数字而没有任何东西会红——这不是假设，是真的发生过。

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
//
// 只有门店 id：名字是商户域的事实，请求体里没有它（见 dto.ActivateRequest）。
func activateRequest() dto.ActivateRequest {
	return dto.ActivateRequest{LocationID: testLocationID}
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

	// 下面几个值**刻意写字面量而不是引用 Default\* 常量**：拿常量和常量比是句废话，
	// 改掉常量两边的断言会一起跟着变，测试照样绿。而这里要挡的正是「模板被改掉」这件事
	// ——后台表单那句说明（admin-web/src/pages/lottery/activations/index.tsx）逐字复述了
	// 这几个数，它们曾经和这份模板对不上（表单写着「门店名 / 10 人」，后端建的是
	// 「门店抽奖 / 30 人」）而没有任何东西会红。所以这里钉住字面量：改模板就要改这里，
	// 而这条注释会把人领到表单那句话上，逼着两处一起改。
	if got.Name != "门店抽奖" {
		t.Errorf("活动名 = %q，模板是「门店抽奖」", got.Name)
	}
	if got.ParticipantTarget != 30 {
		t.Errorf("门槛 = %d，模板是 30 次", got.ParticipantTarget)
	}
	// 这里原来还有两条断言：开始时间取「现在」、结束时间取「现在起 90 天」。模板不再有窗口
	// （2026-09-15），一期收满门槛才开奖，没满就一直等着。
	if got.Prize.Name != "神秘礼品" {
		t.Errorf("奖品名 = %q，模板是「神秘礼品」", got.Prize.Name)
	}
	if got.Prize.Quantity != 1 {
		t.Errorf("奖品名额 = %d，模板是 1——开箱即用的门店抽奖就是抽一个人", got.Prize.Quantity)
	}
	// 这里原来还有一条断言奖品类型的。prize_kind 那一列 2026-09-15 删了（类型从落地起就
	// 只存不消费），类型这个词在奖品上没有了。模板的封面图**刻意留空**——开通是一键动作，
	// 不该被「先找一张图」挡住；断言一下它确实空着，免得哪天有人顺手给模板塞一个默认图
	// 而没人发现（那会让所有店开出来长得一样）。
	if got.Prize.CoverImage != "" {
		t.Errorf("模板奖品的封面图 = %q，应当是空——开通不该要求传图", got.Prize.CoverImage)
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
	// 进库的**只有门店 id**：名字是商户域的事实，本库不留（见 migrations/lottery/003）。
	// 这条断言挡的是「顺手把名字也存一份」的改动——那一份快照正是被删掉的那一列。
	if repo.activateParams.LocationID != testLocationID {
		t.Errorf("门店 id = %q", repo.activateParams.LocationID)
	}
	if repo.activateParams.ActivatedBy != nil {
		t.Error("没传 actor 却写进了操作人")
	}
}

func TestActivateHonoursWhatTheOperatorGave(t *testing.T) {
	repo := activatableRepo()
	svc := newTestService(repo, nil)

	target := int32(7)
	actor := "admin-1"
	req := dto.ActivateRequest{
		LocationID:        testLocationID,
		CampaignName:      "  周年庆  ",
		ParticipantTarget: &target,
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
	// 备注进库前 trim：首尾空格会让列表页那一列参差不齐。
	if got.Remark != "周年庆专场" {
		t.Errorf("备注 = %q", got.Remark)
	}
	if got.ActivatedBy == nil || *got.ActivatedBy != actor {
		t.Error("操作人没有落到开通记录上——「谁在什么时候开的」就答不出来了")
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

	// 这里原来还有两条「窗口」用例（两端相等、结束早于开始）。窗口删掉之后就没有这条校验了。
	cases := []struct {
		name string
		req  dto.ActivateRequest
		want error
	}{
		{"没有门店 id", dto.ActivateRequest{}, ErrLocationIDRequired},
		{"门店 id 是全空格", dto.ActivateRequest{LocationID: "   "}, ErrLocationIDRequired},
		{"门店 id 不是 uuid", dto.ActivateRequest{LocationID: "store-1"}, ErrLocationIDInvalid},
		{"门槛是 0", dto.ActivateRequest{LocationID: testLocationID, ParticipantTarget: &badTarget}, ErrTargetNotPositive},
		{"门槛是负数", dto.ActivateRequest{LocationID: testLocationID, ParticipantTarget: &negative}, ErrTargetNotPositive},
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

// 商户域说没有这家门店：拒绝开通，且**一个字节都不写**。
//
// 这条是「幽灵门店」的堵口：开通会在本库留下一行永久记录，受理一个查无此店的 id 就等于
// 造一条谁也处理不了的记录——列表页上显示成一家门店，点进去什么都是空的。
func TestActivateRefusesAStoreTheMerchantServiceDoesNotKnow(t *testing.T) {
	repo := activatableRepo()
	stores := existingStores()
	stores.exists = false
	svc := newTestServiceWithStores(repo, nil, stores)

	if _, err := svc.Activate(context.Background(), activateRequest(), nil); !errors.Is(err, ErrStoreNotFound) {
		t.Fatalf("err = %v，要的是 %v", err, ErrStoreNotFound)
	}
	if repo.activateCalls != 0 {
		t.Errorf("门店都不存在却写了 %d 次", repo.activateCalls)
	}
	if stores.existsCalls != 1 || stores.lastStoreID != testLocationID {
		t.Errorf("问商户域 %d 次、问的是 %q，要的是 1 次 %q", stores.existsCalls, stores.lastStoreID, testLocationID)
	}
}

// 商户域**问不到**：拒绝开通，但错误必须是「问不到」而不是「不存在」。
//
// 两者分开是有后果的：把问不到说成不存在（404），商户服务抖一下就会让运营以为门店被删了，
// 于是拿着一个其实合法的开通反复重试；反过来把不存在当成问不到（503），运营只会一直重试。
func TestActivateFailsClosedWhenTheStoreDirectoryIsUnreachable(t *testing.T) {
	repo := activatableRepo()
	stores := existingStores()
	stores.existsErr = errors.New("connection refused")
	svc := newTestServiceWithStores(repo, nil, stores)

	err := func() error { _, err := svc.Activate(context.Background(), activateRequest(), nil); return err }()
	if !errors.Is(err, ErrStoresUnavailable) {
		t.Fatalf("err = %v，要的是 %v", err, ErrStoresUnavailable)
	}
	if errors.Is(err, ErrStoreNotFound) {
		t.Error("问不到被当成了「门店不存在」——那是一个我们证不出来的结论")
	}
	if repo.activateCalls != 0 {
		t.Errorf("没问出结果却写了 %d 次", repo.activateCalls)
	}
}

// 压根没接商户域的部署（stores 为 nil）：同样拒绝开通，不是跳过校验直接写。
func TestActivateWithoutAStoreDirectory(t *testing.T) {
	repo := activatableRepo()
	svc := newTestServiceWithStores(repo, nil, nil)

	if _, err := svc.Activate(context.Background(), activateRequest(), nil); !errors.Is(err, ErrStoresUnavailable) {
		t.Fatalf("err = %v，要的是 %v", err, ErrStoresUnavailable)
	}
	if repo.activateCalls != 0 {
		t.Errorf("没接商户域却写了 %d 次——校验形同虚设", repo.activateCalls)
	}
}

// 本地校验先跑，跨服务往返后跑：一个连门店 id 都不合法的请求不该付一次 gRPC。
func TestActivateValidatesBeforeAskingTheMerchantService(t *testing.T) {
	repo := activatableRepo()
	stores := existingStores()
	svc := newTestServiceWithStores(repo, nil, stores)

	bad := int32(0)
	req := activateRequest()
	req.ParticipantTarget = &bad
	if _, err := svc.Activate(context.Background(), req, nil); !errors.Is(err, ErrTargetNotPositive) {
		t.Fatalf("err = %v，要的是 %v", err, ErrTargetNotPositive)
	}
	if stores.existsCalls != 0 {
		t.Errorf("请求本身就不合法，却问了商户域 %d 次", stores.existsCalls)
	}
}

// 开通后返回的那一行**带着门店名**，而名字是读的时候现解的，不是请求里带上来的。
func TestActivateFillsTheStoreNameFromTheMerchantService(t *testing.T) {
	repo := activatableRepo()
	stores := existingStores()
	svc := newTestServiceWithStores(repo, nil, stores)

	row, err := svc.Activate(context.Background(), activateRequest(), nil)
	if err != nil {
		t.Fatalf("Activate: %v", err)
	}
	if row.LocationName != "朝阳门店" {
		t.Errorf("门店名 = %q，要的是商户域解出来的「朝阳门店」", row.LocationName)
	}
	if stores.namesCalls != 1 {
		t.Errorf("解名字 %d 次，要的是 1 次——一页一次，不是一行一次", stores.namesCalls)
	}
}

// 名字解不出来时**不拖垮整页**：开通记录照常返回，只是名字那一格是空的。
//
// 这与上面「问不到门店就不许开通」是相反的取舍，两边都是有意的：开通会留下不可撤的记录，
// 所以校验失败关闭；而名字只是展示，商户服务抖一下就让后台列表整页打不开（连停用一家店的
// 抽奖都做不了）代价更大。
func TestListActivationsKeepsThePageWhenNamesCannotBeResolved(t *testing.T) {
	repo := &fakeRepository{}
	repo.activationRows = []*repository.ActivationListRow{activatedRow()}
	stores := existingStores()
	stores.namesErr = errors.New("connection refused")
	svc := newTestServiceWithStores(repo, nil, stores)

	rows, total, err := svc.ListActivations(context.Background(), dto.ActivationQuery{Status: model.ActivationEnabled})
	if err != nil {
		t.Fatalf("ListActivations: %v", err)
	}
	if total != 1 || len(rows) != 1 {
		t.Fatalf("拿到 %d 行（total=%d），要的是 1 行——名字解不出来不该让整页变成错误", len(rows), total)
	}
	if rows[0].LocationName != "" {
		t.Errorf("门店名 = %q，解不出来时应当是空串", rows[0].LocationName)
	}
	if rows[0].Activation.LocationID != testLocationID {
		t.Error("门店 id 没了：它是本库的事实，与商户域能不能连上无关")
	}
}

// 一页里去重：同一个门店下的多个活动只该被问一次。
func TestResolveStoreNamesAsksOncePerStore(t *testing.T) {
	stores := existingStores()
	svc := newTestServiceWithStores(&fakeRepository{}, nil, stores)

	rows := []*repository.CampaignListRow{
		{Campaign: &model.Campaign{ID: testCampaignID}, LocationID: testLocationID},
		{Campaign: &model.Campaign{ID: testCampaignID}, LocationID: testLocationID},
		{Campaign: &model.Campaign{ID: testCampaignID}, LocationID: "99999999-9999-4999-8999-999999999999"},
	}
	svc.fillCampaignNames(context.Background(), rows)

	if len(stores.lastStoreIDs) != 2 {
		t.Errorf("问了 %v，要的是去重后的两个 id", stores.lastStoreIDs)
	}
	if rows[0].LocationName != "朝阳门店" || rows[1].LocationName != "朝阳门店" {
		t.Errorf("同一家店的两行名字 = %q / %q", rows[0].LocationName, rows[1].LocationName)
	}
	// 商户域不认识的 id 从返回的 map 里缺席，这里就写空串——不编一个名字出来。
	if rows[2].LocationName != "" {
		t.Errorf("商户域不认识的门店名 = %q，应当是空串", rows[2].LocationName)
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
