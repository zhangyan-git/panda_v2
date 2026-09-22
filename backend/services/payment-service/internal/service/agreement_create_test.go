package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/panda-dev/panda-v2/backend/services/payment-service/internal/catalog"
	"github.com/panda-dev/panda-v2/backend/services/payment-service/internal/model"
	"github.com/panda-dev/panda-v2/backend/services/payment-service/internal/provider"
)

// 这一组用例守「发起签约」那条路（见 agreement.go 的 CreateAgreement）。它最要紧的三件事：
//
//   - 交给渠道的 contract_code **就是**我们自己的协议号（两个号的话，通知回来对不上）；
//   - 签名算不出来时**什么都不落**（连渠道调用流水都不写）；
//   - 重放回的是**第一次那份参数**（签名是一次性凭据，重算一份给客户端会让用户在微信那边
//     签下一份我们这边没有记录的协议）。

// stubAgreementKeeper 是一个会签约的假适配器。
//
// 与 stubAgreementNotifier 同一个形状（内嵌 stubProvider，只多会一件事）：Name / SecretSlots /
// Ack 那几条契约与别的用例共用同一份行为，不会悄悄跑偏。
type stubAgreementKeeper struct {
	stubProvider
	sign    provider.AgreementSignResult
	signErr error
	// signCalls 记下每一次签名请求。断言「交给渠道的是不是我们自己的协议号」「notify_url
	// 拼的是不是签约那条路」只能靠它——那些值只在这一层看得见。
	signCalls []provider.AgreementSignRequest

	queried    provider.AgreementQueryResult
	queryErr   error
	queryCalls []provider.AgreementQuery

	// —— 代扣与解约（见 agreement_charge.go）——
	//
	// 结果由用例编排、调用逐个记下来：这两条路上「有没有碰渠道」与「交给渠道的是哪个号」
	// 是全部要点（重复扣款与解错约各自都是从这里开始的）。
	charged     provider.AgreementChargeResult
	chargeErr   error
	chargeCalls []provider.AgreementChargeRequest

	terminated     provider.AgreementCallResult
	terminateErr   error
	terminateCalls []provider.AgreementTerminate
}

func (s *stubAgreementKeeper) Sign(_ context.Context, req provider.AgreementSignRequest) (provider.AgreementSignResult, error) {
	s.signCalls = append(s.signCalls, req)
	if s.signErr != nil {
		return provider.AgreementSignResult{}, s.signErr
	}
	result := s.sign
	// 真适配器会把协议号原样回带（见 AgreementSignResult.ContractCode），假的也照做：
	// service 用的是**请求里那个**号，回带字段只是方便调用方。
	if result.ContractCode == "" {
		result.ContractCode = req.ContractCode
	}
	if result.PayParams == nil {
		result.PayParams = map[string]string{testSignParamKey: testSignParamValue, "request_serial": "1"}
	}
	return result, nil
}

func (s *stubAgreementKeeper) QueryAgreement(_ context.Context, req provider.AgreementQuery) (provider.AgreementQueryResult, error) {
	s.queryCalls = append(s.queryCalls, req)
	if s.queryErr != nil {
		return provider.AgreementQueryResult{}, s.queryErr
	}
	return s.queried, nil
}

// TerminateAgreement / Charge 与上面两条同一个形状：编排结果、记下实参。
//
// 零值不是「没实现」而是一句**明确拒绝**（Result 为空串）——那正好是「适配器忘了填 Result」
// 那种编码错误，而两条路对它的处置完全不同：扣款按「结果不明」留在 charging，解约直接报错。
func (s *stubAgreementKeeper) TerminateAgreement(_ context.Context, req provider.AgreementTerminate) (provider.AgreementCallResult, error) {
	s.terminateCalls = append(s.terminateCalls, req)
	if s.terminateErr != nil {
		return provider.AgreementCallResult{}, s.terminateErr
	}
	return s.terminated, nil
}

func (s *stubAgreementKeeper) Charge(_ context.Context, req provider.AgreementChargeRequest) (provider.AgreementChargeResult, error) {
	s.chargeCalls = append(s.chargeCalls, req)
	if s.chargeErr != nil {
		return provider.AgreementChargeResult{}, s.chargeErr
	}
	return s.charged, nil
}

// 假的签约参数里那两个值：键与值都挑成不会在任何真实的键值里出现的形状，好让「参数的值有没有
// 漏进流水」这件事可以用一句 Contains 断言——漏进去的只会是值，不是键。
const (
	testSignParamKey   = "sign"
	testSignParamValue = "SIGNED-PARAMS-MUST-NOT-REACH-THE-LEDGER"
)

// agreementKeeperService 组装一个「wechat_pay 那条渠道指向假 keeper」的业务层。
//
// extra 在需要第二个适配器的用例里用（「这条支付方式压根不会签约」那一条）。
func agreementKeeperService(t *testing.T, repo *fakeRepository, keeper *stubAgreementKeeper,
	extra ...provider.Provider) *PaymentService {
	t.Helper()
	keeper.name = catalog.ChannelCodeWeChatPay
	return newServiceWithCatalog(t, repo, testCatalog(), keeper, catalog.CodeWechatPapay, nil,
		func(*catalog.Channel, string) string { return "secret" }, extra...)
}

// validAgreementRequest 是一次形状正确的发起签约请求。
func validAgreementRequest() CreateAgreementRequest {
	return CreateAgreementRequest{
		UserID:          testUserID,
		PaymentMethod:   catalog.CodeWechatPapay,
		PlanCode:        "monthly_19_9",
		ProviderPlanID:  "214488",
		WalletOpenID:    "oTestOpenId1234567890",
		Subject:         "连续包月",
		MaxChargeAmount: 1990,
		RequestID:       testRequest,
	}
}

// TestCreateAgreementHandsTheProviderOneAgreementNumber 钉住「一个协议号，两个身份」。
//
// 交给渠道的 contract_code 必须**就是**我们落库的那个 agreement_no：两个号的话，渠道推回来的
// 通知按它自己那套标识来，而我们两张表各说各话——老系统的续费回调永远查不到订阅就是这么来的。
//
// 顺带钉住 notify_url 拼的是**协议**那条路（不是支付回调那条）：拼错了的表现是「用户签完了，
// 我们这边什么都没发生」，而那是这条链路上最难查的一种。
func TestCreateAgreementHandsTheProviderOneAgreementNumber(t *testing.T) {
	repo := &fakeRepository{agreement: &model.PaymentAgreement{ID: "agreement-1"}}
	keeper := &stubAgreementKeeper{}
	svc := agreementKeeperService(t, repo, keeper)

	result, err := svc.CreateAgreement(context.Background(), validAgreementRequest())
	if err != nil {
		t.Fatalf("CreateAgreement: %v", err)
	}

	if len(keeper.signCalls) != 1 {
		t.Fatalf("签名调用 = %d 次，想要 1 次", len(keeper.signCalls))
	}
	asked := keeper.signCalls[0]
	if asked.ContractCode == "" || asked.ContractCode != result.AgreementNo {
		t.Errorf("交给渠道的 contract_code = %q，本地协议号 = %q，两者必须是同一个值",
			asked.ContractCode, result.AgreementNo)
	}
	if asked.PlanID != "214488" {
		t.Errorf("签约模板 id = %q，想要调用方给的那个（渠道按它决定这份协议每月扣多少）", asked.PlanID)
	}
	if asked.WalletOpenID != "oTestOpenId1234567890" {
		t.Errorf("openid = %q，想要调用方给的那个（协议上写的是「谁的账户可以被扣」）", asked.WalletOpenID)
	}
	if !strings.HasSuffix(asked.NotifyURL, "/v1/payments/agreement-notify/"+catalog.ChannelCodeWeChatPay) {
		t.Errorf("notify_url = %q，想要签约通知那条路（不是支付回调那条）", asked.NotifyURL)
	}
	// 凭据按**适配器自己声明的槽**解析（见 resolveAgreementRoute 那段注释里漏掉它的后果：
	// 表现是签名里少一个字段，而渠道那边只回一句 SIGN_ERROR）。假适配器声明的是
	// stubSecretSlot，这里就按它查——查 apiV2Key 会查出空，那是假适配器的事，不是签约的。
	if asked.Secrets.Get(stubSecretSlot) == "" {
		t.Error("交给适配器的凭据是空的：签名会算不出来")
	}

	if result.Status != model.AgreementStatusPending {
		t.Errorf("状态 = %q，想要 %q（用户在微信那边还没点同意）", result.Status, model.AgreementStatusPending)
	}
	if result.Action != string(provider.ActionJumpMiniapp) {
		t.Errorf("Action = %q，想要 %q（客户端只按它决定怎么把用户送到签约页）",
			result.Action, provider.ActionJumpMiniapp)
	}
	if result.PayParams[testSignParamKey] != testSignParamValue {
		t.Errorf("交给客户端的参数 = %v，想要适配器算出来的那一份原样带出去", result.PayParams)
	}
	if result.AgreementID != "agreement-1" {
		t.Errorf("协议 id = %q，想要落库那一行的 id", result.AgreementID)
	}
}

// TestCreateAgreementRecordsNoParameterValues 守「签名不进流水」。
//
// 那 8 个参数（含 sign）是一次性凭据：它进流水就等于让任何能看这张表的人拿它去调起一次签约。
// 流水里只该有**键名**与能定位这份协议的那几个业务值。
func TestCreateAgreementRecordsNoParameterValues(t *testing.T) {
	repo := &fakeRepository{agreement: &model.PaymentAgreement{ID: "agreement-1"}}
	keeper := &stubAgreementKeeper{}
	svc := agreementKeeperService(t, repo, keeper)

	if _, err := svc.CreateAgreement(context.Background(), validAgreementRequest()); err != nil {
		t.Fatalf("CreateAgreement: %v", err)
	}

	if len(repo.createAgreementCalls) != 1 {
		t.Fatalf("落库调用 = %d 次，想要 1 次", len(repo.createAgreementCalls))
	}
	recorded := repo.createAgreementCalls[0]

	if got := recorded.CallSummary["payParamKeys"]; !reflect.DeepEqual(got, []string{"request_serial", testSignParamKey}) {
		t.Errorf("payParamKeys = %v，想要参数表里那几个键名（按字典序）", got)
	}
	for key, value := range recorded.CallSummary {
		if strings.Contains(fmt.Sprint(value), testSignParamValue) {
			t.Errorf("流水里 %s 那一格带了参数的值：%v", key, value)
		}
	}
	// 摘要里那几项是这次签约的**身份**，不是参数：排查时要能按它们找到这份协议。
	if recorded.CallSummary["providerPlanId"] != "214488" || recorded.CallSummary["channel"] != catalog.ChannelCodeWeChatPay {
		t.Errorf("流水摘要 = %v，想要能定位这次签约的那几项", recorded.CallSummary)
	}
	if recorded.RequestHash == "" {
		t.Error("幂等键的哈希是空的：「同一个 key 换了请求体」就判不出来了")
	}
	// metadata 是那棵树（service 传 map[string]any 给仓储，真仓储把它序列化成 JSON 列）：
	// 查约要靠它拿 plan_id，缺了那份协议今天查不了（见 ErrAgreementPlanMissing）。
	if recorded.Metadata[model.AgreementMetaProviderPlanID] != "214488" {
		t.Errorf("metadata = %v，查约要用的签约模板 id 没落下去", recorded.Metadata)
	}
}

// TestCreateAgreementReplaysTheRecordedParams 守重放。
//
// 重放回的必须是**第一次那份参数**：签名是一次性凭据，重算一份给客户端意味着用户签下的那份
// 协议与我们记录的那份不是同一份。所以这里断言的是「回的是快照里的号与快照里的参数」。
func TestCreateAgreementReplaysTheRecordedParams(t *testing.T) {
	first := CreateAgreementResult{
		AgreementNo: "AGR20260901120000000001", AgreementID: "agreement-1",
		Status: model.AgreementStatusPending, Action: string(provider.ActionJumpMiniapp),
		PayParams: map[string]string{testSignParamKey: "THE-FIRST-SIGNATURE"},
	}
	snapshot, err := json.Marshal(first)
	if err != nil {
		t.Fatalf("marshal snapshot: %v", err)
	}
	repo := &fakeRepository{createAgreementHit: true, createAgreementSnap: snapshot}
	keeper := &stubAgreementKeeper{}
	svc := agreementKeeperService(t, repo, keeper)

	result, err := svc.CreateAgreement(context.Background(), validAgreementRequest())
	if err != nil {
		t.Fatalf("CreateAgreement: %v", err)
	}

	if result.AgreementNo != first.AgreementNo {
		t.Errorf("重放回的协议号 = %q，想要第一次那个 %q（不是这次新生成的）", result.AgreementNo, first.AgreementNo)
	}
	if result.PayParams[testSignParamKey] != "THE-FIRST-SIGNATURE" {
		t.Errorf("重放回的参数 = %v，想要第一次那份（重算一份签名会让用户签下另一份协议）", result.PayParams)
	}
	if len(repo.createAgreementCalls) != 1 {
		t.Errorf("仓储被调了 %d 次，想要 1 次（重放由那一次的回答决定）", len(repo.createAgreementCalls))
	}
}

// TestCreateAgreementReplayWithoutASnapshotIsAnError：幂等行在、快照是空的，只可能是一行脏数据。
//
// 这时**绝不能**现算一份参数给客户端：那会让用户在微信那边签下一份我们这边没有记录的协议。
// 报错让人来查，是唯一安全的处置。
func TestCreateAgreementReplayWithoutASnapshotIsAnError(t *testing.T) {
	repo := &fakeRepository{createAgreementHit: true}
	svc := agreementKeeperService(t, repo, &stubAgreementKeeper{})

	if _, err := svc.CreateAgreement(context.Background(), validAgreementRequest()); err == nil {
		t.Fatal("快照是空的却成功了：客户端会拿到一份没有记录的签约参数")
	}
}

// TestCreateAgreementRefusesWhenItCannotSign：签名算不出来时**什么都不落**，包括渠道调用流水
// ——那时我们对渠道什么也没做（连报文都没有），一条没有协议行可挂的流水只会让人去找一份不
// 存在的协议。
func TestCreateAgreementRefusesWhenItCannotSign(t *testing.T) {
	repo := &fakeRepository{agreement: &model.PaymentAgreement{ID: "agreement-1"}}
	keeper := &stubAgreementKeeper{signErr: provider.ErrSecretNotConfigured}
	svc := agreementKeeperService(t, repo, keeper)

	if _, err := svc.CreateAgreement(context.Background(), validAgreementRequest()); !errors.Is(err, provider.ErrSecretNotConfigured) {
		t.Fatalf("err = %v，想要 ErrSecretNotConfigured 原样透出来", err)
	}
	if len(repo.createAgreementCalls) != 0 {
		t.Errorf("签名都没算出来却落了库：%+v", repo.createAgreementCalls)
	}
	if len(repo.providerCall) != 0 {
		t.Errorf("签名都没算出来却写了渠道调用流水：%+v", repo.providerCall)
	}
}

// TestCreateAgreementValidatesItsInput 是形状层面的那几道闸。
//
// 「没走到渠道、也没落库」是断言的一部分：一条不合法的签约请求在库上留下任何痕迹都是错的，
// 而更糟的是它可能带着一份**缺字段的**协议参数跑到客户端去。
func TestCreateAgreementValidatesItsInput(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*CreateAgreementRequest)
		want   error
	}{
		{"没有用户", func(r *CreateAgreementRequest) { r.UserID = "" }, ErrUserIDRequired},
		{"用户 id 不是 uuid", func(r *CreateAgreementRequest) { r.UserID = "u_1" }, ErrUserIDInvalid},
		{"没有支付方式", func(r *CreateAgreementRequest) { r.PaymentMethod = "" }, ErrMethodRequired},
		{"没有幂等键", func(r *CreateAgreementRequest) { r.RequestID = "" }, ErrRequestIDRequired},
		// 签约模板 id 在渠道那边决定「这份协议每月扣多少」，缺了它签出来的东西在渠道那边
		// 不存在——所以它在入口就断，而不是等用户点了「开通」之后。
		{"没有签约模板", func(r *CreateAgreementRequest) { r.ProviderPlanID = "" }, ErrProviderPlanIDRequired},
		{"没有 openid", func(r *CreateAgreementRequest) { r.WalletOpenID = "" }, ErrWalletOpenIDRequired},
		{"扣款上限是负的", func(r *CreateAgreementRequest) { r.MaxChargeAmount = -1 }, ErrMaxChargeAmountInvalid},
		{"支付方式不在目录里", func(r *CreateAgreementRequest) { r.PaymentMethod = "no_such_method" }, catalog.ErrMethodNotFound},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			request := validAgreementRequest()
			tc.mutate(&request)

			repo := &fakeRepository{agreement: &model.PaymentAgreement{ID: "agreement-1"}}
			keeper := &stubAgreementKeeper{}
			svc := agreementKeeperService(t, repo, keeper)

			if _, err := svc.CreateAgreement(context.Background(), request); !errors.Is(err, tc.want) {
				t.Fatalf("err = %v，想要 %v", err, tc.want)
			}
			if len(keeper.signCalls) != 0 {
				t.Error("被拦下的签约不该去算签名")
			}
			if len(repo.createAgreementCalls) != 0 {
				t.Error("被拦下的签约不该落库")
			}
		})
	}
}

// TestCreateAgreementRefusesAMethodThatCannotSign 把「这条方式压根不是代扣那一路」与别的拒绝
// 分开。
//
// 它要的是 ErrAgreementUnsupported（调用方该做的是别拿这条方式签约），而不是「适配器没注册」
// 或者「渠道没配齐」——那两句会把人引去改配置。所以这条用例里**两个适配器都注册着**：银联商务
// 那条路是好的，它只是不会签约。
func TestCreateAgreementRefusesAMethodThatCannotSign(t *testing.T) {
	repo := &fakeRepository{}
	keeper := &stubAgreementKeeper{}
	svc := agreementKeeperService(t, repo, keeper, &stubProvider{})

	request := validAgreementRequest()
	request.PaymentMethod = catalog.CodeUMSMiniappWechat

	if _, err := svc.CreateAgreement(context.Background(), request); !errors.Is(err, ErrAgreementUnsupported) {
		t.Fatalf("err = %v，想要 ErrAgreementUnsupported", err)
	}
	if len(repo.createAgreementCalls) != 0 {
		t.Error("不会签约的支付方式不该落库")
	}
}
