package service

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/panda-dev/panda-v2/backend/services/payment-service/internal/model"
	"github.com/panda-dev/panda-v2/backend/services/payment-service/internal/provider"
	"github.com/panda-dev/panda-v2/backend/services/payment-service/internal/repository"
)

// 这一组用例守「查约」那条路（见 agreement.go 的 QueryAgreement）。它是**写**不是读：后台那个
// 「同步」按钮与小程序签完之后的「确认」都走它，纠正本地状态只能发生在这里。
//
// 最要紧的一条是**结果不明时什么都不改**：把一次查约超时读成「已解约」会把一份有效的协议在
// 本地判死，而判死的后果是用户下个月扣不到款、会员静默断掉。

// agreementOnFile 是库上那一份协议（假仓储在它为 nil 时报「查无此约」，与真仓储同一条判据）。
func agreementOnFile(planID string) *model.PaymentAgreement {
	metadata, err := json.Marshal(map[string]any{model.AgreementMetaProviderPlanID: planID})
	if err != nil {
		panic(err)
	}
	return &model.PaymentAgreement{
		ID: "agreement-1", AgreementNo: "AGR20260901120000000001",
		UserID: testUserID, Provider: "wechat_pay",
		PaymentMethod: "wechat_papay", PlanCode: "monthly_19_9",
		Status: model.AgreementStatusPending, Metadata: metadata,
	}
}

// TestQueryAgreementSettlesWhatTheChannelSaid 钉住「渠道的两个字翻成本地状态」这一段。
//
// 三条都在一张表里：它们共用同一份请求与同一段流水，分开写只会把「问的是哪一份协议」断言三遍，
// 而三处真正的差别只有目标状态那一列。
func TestQueryAgreementSettlesWhatTheChannelSaid(t *testing.T) {
	cases := []struct {
		name          string
		state         provider.AgreementState
		wantTarget    string
		wantContractN string
	}{
		{"渠道说已签约", provider.AgreementSigned, model.AgreementStatusActive, "1234567890"},
		{"渠道说已解约", provider.AgreementTerminated, model.AgreementStatusTerminated, ""},
		// 渠道说还在进行中：本地本来就是 pending，那一次核查什么都不改（Changed 为假），
		// 但**结论照样回给调用方**——「渠道那边还等着用户点同意」是一句有用的答复。
		{"渠道说还在进行中", provider.AgreementPending, model.AgreementStatusPending, ""},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			repo := &fakeRepository{
				agreement:              agreementOnFile("214488"),
				agreementSettleChanged: tc.state != provider.AgreementPending,
			}
			keeper := &stubAgreementKeeper{queried: provider.AgreementQueryResult{
				State: tc.state, ProviderContractID: tc.wantContractN,
				Result: provider.ResultSuccess,
			}}
			svc := agreementKeeperService(t, repo, keeper)

			result, err := svc.QueryAgreement(context.Background(), QueryAgreementRequest{
				AgreementNo: "AGR20260901120000000001", RequestID: testRequest,
			})
			if err != nil {
				t.Fatalf("QueryAgreement: %v", err)
			}

			// 问渠道的那一次：plan_id 与 contract_code 都要带（微信按这一对认一份协议），
			// 而 contract_code 就是我们自己的协议号。
			if len(keeper.queryCalls) != 1 {
				t.Fatalf("查约调用 = %d 次，想要 1 次", len(keeper.queryCalls))
			}
			asked := keeper.queryCalls[0]
			if asked.PlanID != "214488" || asked.ContractCode != "AGR20260901120000000001" {
				t.Errorf("问渠道的两个凭据 = (%q, %q)，想要 (plan_id, contract_code)",
					asked.PlanID, asked.ContractCode)
			}
			// 与发起签约那条路同一个槽（验签与签名共用一把密钥），按适配器声明的名字查。
			if asked.Secrets.Get(stubSecretSlot) == "" {
				t.Error("交给适配器的凭据是空的：查约的签名会算不出来")
			}

			// 落回本库的那一次：目标状态是**翻好的**那个（渠道的词不进这一层）。
			if len(repo.agreementSettles) != 1 {
				t.Fatalf("结算调用 = %d 次，想要 1 次", len(repo.agreementSettles))
			}
			settled := repo.agreementSettles[0]
			if settled.Target != tc.wantTarget {
				t.Errorf("目标状态 = %q，想要 %q", settled.Target, tc.wantTarget)
			}
			if settled.ContractNo != tc.wantContractN {
				t.Errorf("渠道侧协议号 = %q，想要 %q（扣款与解约都按它认这份协议）", settled.ContractNo, tc.wantContractN)
			}
			if settled.Reason == "" {
				t.Error("流水的理由是空的：排查时先看的就是它")
			}

			if result.Status != tc.wantTarget {
				t.Errorf("回给调用方的状态 = %q，想要纠正之后的 %q", result.Status, tc.wantTarget)
			}
			if result.ProviderState != string(tc.state) {
				t.Errorf("渠道的原话 = %q，想要 %q（它是排查时唯一能与渠道对上的东西）",
					result.ProviderState, tc.state)
			}
			if want := tc.state != provider.AgreementPending; result.Changed != want {
				t.Errorf("Changed = %v，想要 %v", result.Changed, want)
			}

			// 出网调用先留痕，再管状态：流水记的是「我们对渠道问过什么」，这一次询问已经真的
			// 发生过了，即使后面那一步失败。
			if len(repo.providerCall) != 1 {
				t.Fatalf("渠道调用流水 = %d 条，想要 1 条", len(repo.providerCall))
			}
			call := repo.providerCall[0]
			if call.Operation != model.CallOperationQuery {
				t.Errorf("流水上的操作 = %q，想要 %q", call.Operation, model.CallOperationQuery)
			}
			if call.AgreementNo != "AGR20260901120000000001" {
				t.Errorf("流水挂的协议 = %q，想要被问的那一份", call.AgreementNo)
			}
			if call.RequestSummary["localStatus"] != model.AgreementStatusPending {
				t.Errorf("流水里的本地状态 = %v，想要**核查之前**的那个（它是这次纠正的起点）",
					call.RequestSummary["localStatus"])
			}
		})
	}
}

// TestQueryAgreementLeavesTheAgreementAloneWhenTheAnswerIsUnclear 是这条路上唯一不能出的错。
//
// 渠道超时、报错、或者报文读不懂时，**一个字段都不能改**：那份协议在本地还是 pending 或
// active，用户只是下个月再被核查一次。把它读成「已解约」等于单方面替用户解约。
//
// 但流水照样要写：这次询问真的发生过，排查时那条流水是「我们问过、没问出结果」的唯一证据。
func TestQueryAgreementLeavesTheAgreementAloneWhenTheAnswerIsUnclear(t *testing.T) {
	cases := []struct {
		name     string
		queried  provider.AgreementQueryResult
		queryErr error
	}{
		{
			name: "渠道回了一句失败",
			queried: provider.AgreementQueryResult{
				Result: provider.ResultFailed, FailureCode: "SYSTEMERROR",
			},
		},
		{
			// 超时：适配器连状态都没填。**空状态绝不能被当成「没签约」**。
			name:     "出网调用挂了",
			queried:  provider.AgreementQueryResult{Result: provider.ResultUnknown},
			queryErr: context.DeadlineExceeded,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			repo := &fakeRepository{agreement: agreementOnFile("214488"), agreementSettleChanged: true}
			keeper := &stubAgreementKeeper{queried: tc.queried, queryErr: tc.queryErr}
			svc := agreementKeeperService(t, repo, keeper)

			result, err := svc.QueryAgreement(context.Background(), QueryAgreementRequest{
				AgreementNo: "AGR20260901120000000001",
			})
			if !errors.Is(err, ErrProviderResultUncertain) {
				t.Fatalf("err = %v，想要 ErrProviderResultUncertain", err)
			}
			if result != nil {
				t.Errorf("结果 = %+v，想要 nil（什么都没问出来）", result)
			}
			if len(repo.agreementSettles) != 0 {
				t.Fatalf("结果不明却改了协议状态：%+v", repo.agreementSettles)
			}
			if len(repo.providerCall) != 1 {
				t.Errorf("渠道调用流水 = %d 条，想要 1 条（问过就要有人说得出问过）", len(repo.providerCall))
			}
		})
	}
}

// TestQueryAgreementRefusesBeforeAskingTheChannel：三件「这一步走不到」的事各自要有自己的错，
// 而且都在**碰渠道之前**断掉。
//
// 三句错指向三个不同的排查方向——调用方传错了参数、这份协议不在库里、这份协议没记签约模板。
// 混成一句「查约失败」的话，运维得从数据库开始翻。
func TestQueryAgreementRefusesBeforeAskingTheChannel(t *testing.T) {
	cases := []struct {
		name      string
		request   QueryAgreementRequest
		agreement *model.PaymentAgreement
		wantErr   error
	}{
		{
			name:    "没给协议号",
			request: QueryAgreementRequest{},
			// 连库都不该查：这一条在仓储之前就断了。
			agreement: agreementOnFile("214488"),
			wantErr:   ErrAgreementNoRequired,
		},
		{
			name:      "这份协议不在库里",
			request:   QueryAgreementRequest{AgreementNo: "AGR20260901999999999999"},
			agreement: nil,
			wantErr:   repository.ErrAgreementNotFound,
		},
		{
			// 没记签约模板：微信按 (plan_id, contract_code) 认一份协议，拿一个猜的模板去问
			// 会得到「查无此约」，而那条结论会把一份有效的协议在本地判死。宁可不问。
			name:      "这份协议没记签约模板",
			request:   QueryAgreementRequest{AgreementNo: "AGR20260901120000000001"},
			agreement: agreementOnFile(""),
			wantErr:   ErrAgreementPlanMissing,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			repo := &fakeRepository{agreement: tc.agreement, agreementSettleChanged: true}
			keeper := &stubAgreementKeeper{queried: provider.AgreementQueryResult{
				State: provider.AgreementSigned, Result: provider.ResultSuccess,
			}}
			svc := agreementKeeperService(t, repo, keeper)

			_, err := svc.QueryAgreement(context.Background(), tc.request)
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("err = %v，想要 %v", err, tc.wantErr)
			}
			if len(keeper.queryCalls) != 0 {
				t.Error("被拦下的查约不该去问渠道")
			}
			if len(repo.agreementSettles) != 0 {
				t.Error("被拦下的查约不该改状态")
			}
		})
	}
}

// TestQueryAgreementRefusesAnUnknownState：渠道给了一个我们不认识的协议状态。
//
// 它走的是 agreementTarget 的 default 分支（与签约通知那条路**同一个函数**）。这里钉的是
// service 这一侧的处置：报错、不改状态、不写结算——**不知道它是什么意思时不能拿它去改会员的
// 授权**。
func TestQueryAgreementRefusesAnUnknownState(t *testing.T) {
	repo := &fakeRepository{agreement: agreementOnFile("214488"), agreementSettleChanged: true}
	keeper := &stubAgreementKeeper{queried: provider.AgreementQueryResult{
		State: provider.AgreementState("suspended"), Result: provider.ResultSuccess,
	}}
	svc := agreementKeeperService(t, repo, keeper)

	_, err := svc.QueryAgreement(context.Background(), QueryAgreementRequest{
		AgreementNo: "AGR20260901120000000001",
	})
	if !errors.Is(err, ErrProviderResultUncertain) {
		t.Fatalf("err = %v，想要 ErrProviderResultUncertain", err)
	}
	if !strings.Contains(err.Error(), "suspended") {
		t.Errorf("错误串 = %v，想要带上渠道说的那个原话（那是唯一的线索）", err)
	}
	if len(repo.agreementSettles) != 0 {
		t.Fatalf("不认识的状态却改了协议：%+v", repo.agreementSettles)
	}
}
