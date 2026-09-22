package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"testing"

	"github.com/panda-dev/panda-v2/backend/services/payment-service/internal/catalog"
	"github.com/panda-dev/panda-v2/backend/services/payment-service/internal/dto"
	"github.com/panda-dev/panda-v2/backend/services/payment-service/internal/model"
	"github.com/panda-dev/panda-v2/backend/services/payment-service/internal/provider"
	"github.com/panda-dev/panda-v2/backend/services/payment-service/internal/repository"
)

// 这一组用例守签约通知那条路的四步骨架（见 agreement_notify.go 的文件头）：找渠道 → 验签 →
// 落 payment_notifications → 一个事务里改协议 + 写 outbox。第四步在真仓储里，这里能断言的
// 是**交给它的那份结论**（哪份协议、改成什么、通知号是哪个）。
//
// 报文形状与验签本身在 wechatpay 包里验（见 notify_test.go）；这里用假适配器，验的是
// service 拿到的结论怎么处置，以及处置错的时候**有没有碰协议**。

// stubAgreementNotifier 是一个额外认协议通知的假适配器。
//
// 用内嵌而不是另起一个完整实现：它不是这套接口的另一个实现者，只是 stubProvider 多会一件事
// ——这样 Name / SecretSlots / Ack 那几条契约与别的用例共用同一份行为，不会悄悄跑偏。
//
// verifyErr 非零时它像真适配器那样什么都没读出来就拒（零值 provider.AgreementNotification
// 表示「一个字段都没填」，这正是拒绝时调用方该看到的东西）。
type stubAgreementNotifier struct {
	stubProvider
	notification provider.AgreementNotification
	verifyErr    error
	calls        []provider.NotificationRequest
}

func (s *stubAgreementNotifier) VerifyAgreementNotification(_ context.Context, req provider.NotificationRequest) (provider.AgreementNotification, error) {
	s.calls = append(s.calls, req)
	return s.notification, s.verifyErr
}

// agreementNotifier 组装一个「微信那条渠道指向假适配器」的业务层。
//
// 渠道与方式都用目录里真有的那两个 code：签约通知的地址里那一段就是渠道码，而 service 拿它
// 去目录里找渠道、再按渠道的 Provider 找适配器——用例里换一个自造的 code 就等于把这条链
// 绕开了，那样「渠道码写错」这一类错在我们这儿永远是绿的。
func agreementNotifier(t *testing.T, repo *fakeRepository, notifier *stubAgreementNotifier) *PaymentService {
	t.Helper()
	notifier.name = catalog.ChannelCodeWeChatPay
	return newServiceWith(t, repo, notifier, catalog.CodeWechatPapay, nil,
		func(*catalog.Channel, string) string { return "secret" })
}

// signedAgreement 是一份**签约成功**通知里那份协议的样子（微信 ADD 报文的结论）。
func signedAgreement() provider.AgreementNotification {
	return provider.AgreementNotification{
		AgreementNo: "AG2026090001", ContractNo: "1234567890",
		State: provider.AgreementSigned, ProviderState: "0",
		ResponseSummary: map[string]any{"contract_code": "AG2026090001"},
	}
}

func terminatedAgreement() provider.AgreementNotification {
	return provider.AgreementNotification{
		// 解约通知不带我们自己的协议号（微信的 DELETE 报文里只有 contract_id）。
		AgreementNo: "", ContractNo: "1234567890",
		State: provider.AgreementTerminated,
	}
}

// TestAgreementNotificationSettlesToTheStateTheChannelReported 钉住第 3、4 步交下去的东西。
//
// 两个状态各一条而不是一条带表：它们的**必填字段不同**（解约那条没有协议号），而「空着也要
// 原样送下去」是这条路上最容易顺手补一个默认值的地方——补了就变成按一个空协议号去查库。
func TestAgreementNotificationSettlesToTheStateTheChannelReported(t *testing.T) {
	cases := []struct {
		name            string
		notification    provider.AgreementNotification
		wantTarget      string
		wantEventType   string
		wantAgreementNo string
	}{
		{
			name: "签约成功", notification: signedAgreement(),
			wantTarget: model.AgreementStatusActive, wantEventType: dto.EventAgreementSigned,
			wantAgreementNo: "AG2026090001",
		},
		{
			name: "解约", notification: terminatedAgreement(),
			wantTarget: model.AgreementStatusTerminated, wantEventType: dto.EventAgreementTerminated,
			// 报文里没有 contract_code，这一格就得是空串：真仓储按 contract_no 兜底去找。
			wantAgreementNo: "",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body := []byte("<xml>the notification body</xml>")
			repo := &fakeRepository{
				// 库上那一份协议（假仓储在它为空时报「查无此约」——与真仓储同一条判据）。
				agreement: &model.PaymentAgreement{
					ID: "agreement-1", AgreementNo: "AG2026090001",
					ContractNo: "1234567890", Status: model.AgreementStatusPending,
				},
				agreementNotificationChanged: true,
			}
			notifier := &stubAgreementNotifier{notification: tc.notification}
			svc := agreementNotifier(t, repo, notifier)

			result, err := svc.HandleAgreementNotification(context.Background(), AgreementNotificationRequest{
				ChannelCode: catalog.ChannelCodeWeChatPay, Body: body,
			})
			if err != nil {
				t.Fatalf("HandleAgreementNotification: %v", err)
			}

			// 第 3 步：落库的那一行。通知号是**报文自身的摘要**（微信的签约通知没有通知号），
			// 这里独立算一遍而不是调被测的那个函数，否则「算法改了」这件事永远测不出来。
			if len(repo.notificationCalls) != 1 {
				t.Fatalf("落库的通知 = %d 条，想要 1 条", len(repo.notificationCalls))
			}
			recorded := repo.notificationCalls[0]
			if want := digest(body); recorded.NotificationID != want {
				t.Errorf("通知号 = %q，想要报文的 sha256 %q", recorded.NotificationID, want)
			}
			if recorded.EventType != tc.wantEventType {
				t.Errorf("落库的事件类型 = %q，想要 %q", recorded.EventType, tc.wantEventType)
			}
			if !recorded.SignatureVerified || recorded.Status != model.NotificationReceived {
				t.Errorf("落库的那一行 = %+v，想要「验签通过、刚收到」", recorded)
			}
			// 协议通知不指向支付单。**这一格是空的不是忘了填**：一份协议上永远不会有支付单号
			// （代扣不建 payments 行），填一个空之外的任何值都是编的。
			if recorded.PaymentNo != "" {
				t.Errorf("支付单号 = %q，协议通知不该指向支付单", recorded.PaymentNo)
			}

			// 第 4 步：交给结算的那份结论。
			if len(repo.agreementNotificationSettles) != 1 {
				t.Fatalf("结算调用 = %d 次，想要 1 次", len(repo.agreementNotificationSettles))
			}
			settled := repo.agreementNotificationSettles[0]
			if settled.Target != tc.wantTarget {
				t.Errorf("目标状态 = %q，想要 %q", settled.Target, tc.wantTarget)
			}
			if settled.Provider != catalog.ChannelCodeWeChatPay {
				t.Errorf("渠道 = %q，想要收到这条通知的那条渠道 %q", settled.Provider, catalog.ChannelCodeWeChatPay)
			}
			if settled.AgreementNo != tc.wantAgreementNo {
				t.Errorf("协议号 = %q，想要 %q", settled.AgreementNo, tc.wantAgreementNo)
			}
			if settled.ContractNo != "1234567890" {
				t.Errorf("渠道侧协议号 = %q，想要报文里的那个", settled.ContractNo)
			}
			if settled.NotificationID != "n-1" {
				t.Errorf("结算带着的通知 id = %q，想要刚落库那一行（同一个事务里要标 processed）", settled.NotificationID)
			}
			// 流水那句话里要有渠道的原话：本地记成 terminated 而渠道说 state=0 时，
			// 这一句是唯一能把两边对上的东西。
			if tc.notification.ProviderState != "" && settled.Reason == "" {
				t.Error("流水的理由是空的，渠道的原话丢了")
			}

			if !result.Settled || result.Duplicate {
				t.Errorf("结果 = %+v，想要「这次真的改了、不是重投」", result)
			}
			if result.EventType != tc.wantEventType {
				t.Errorf("回给调用方的事件类型 = %q，想要与落库那一行同一个串 %q", result.EventType, tc.wantEventType)
			}
		})
	}
}

// TestAgreementNotificationRefusesBeforeItTouchesTheAgreement 是「伪造的通知不会把会员的
// 授权改乱」那句话的全部依据。
//
// 每一条都断言**两件事**：报的是哪个错（排查方向），以及**一次结算都没有发生**（协议没被动过）。
// 只断言前者的话，一个「先结算再验签」的写法照样能过——而那正是这条路上唯一不能出的错。
//
// 那几条验签类的失败还会落一条 failed 留痕（recordRejectedNotification）：这是**有意的**，
// 「有人往这个地址打了一条我们处理不了的报文」本身要有人能看见。所以这里断言的是「结算没发生」，
// 不是「什么都没写」。
func TestAgreementNotificationRefusesBeforeItTouchesTheAgreement(t *testing.T) {
	cases := []struct {
		name    string
		request AgreementNotificationRequest
		// adapter 覆盖默认那个认协议的假适配器（「这条渠道根本不认协议通知」那一条要它）。
		adapter provider.Provider
		// verifyErr 是假适配器验签的结果。
		verifyErr error
		// notification 是验签通过时它读出来的结论（造「状态是我们不认识的」那一条）。
		notification provider.AgreementNotification
		wantErr      error
	}{
		{
			name: "渠道码是空的",
			request: AgreementNotificationRequest{
				Body: []byte("<xml/>"),
			},
			wantErr: ErrChannelCodeRequired,
		},
		{
			name: "报文体是空的",
			request: AgreementNotificationRequest{
				ChannelCode: catalog.ChannelCodeWeChatPay,
			},
			wantErr: ErrNotificationEmpty,
		},
		{
			name: "这个渠道码不在目录里",
			request: AgreementNotificationRequest{
				ChannelCode: "wechat_pay_typo", Body: []byte("<xml/>"),
			},
			wantErr: ErrChannelNotFound,
		},
		{
			// 银联商务那条路（它的适配器不实现 AgreementNotifier）：一份支付回调被 POST 到
			// 签约通知的地址上，得到的必须是这句「这条渠道不推协议通知」，而不是一句
			// 「验签失败」——后者会让人去查密钥。
			name: "这条渠道不认协议通知",
			request: AgreementNotificationRequest{
				ChannelCode: catalog.ChannelCodeWeChatPay, Body: []byte("<xml/>"),
			},
			adapter: &stubProvider{name: catalog.ChannelCodeWeChatPay},
			wantErr: ErrAgreementNotifyUnsupported,
		},
		{
			name: "验签不过",
			request: AgreementNotificationRequest{
				ChannelCode: catalog.ChannelCodeWeChatPay, Body: []byte("<xml/>"),
			},
			verifyErr: provider.ErrSignatureMismatch,
			wantErr:   provider.ErrSignatureMismatch,
		},
		{
			name: "密钥没配上",
			request: AgreementNotificationRequest{
				ChannelCode: catalog.ChannelCodeWeChatPay, Body: []byte("<xml/>"),
			},
			verifyErr: provider.ErrSecretNotConfigured,
			wantErr:   provider.ErrSecretNotConfigured,
		},
		{
			// 渠道给了一个我们不认识的协议状态：**我们不知道它是什么意思时不能拿它去改
			// 会员的授权**。就地拒，也正因为如此它不会落一条 event_type 写着那个状态的记录
			// ——那条记录会让人以为我们知道这是什么。
			name: "渠道说的状态我们不认识",
			request: AgreementNotificationRequest{
				ChannelCode: catalog.ChannelCodeWeChatPay, Body: []byte("<xml/>"),
			},
			notification: provider.AgreementNotification{
				AgreementNo: "AG2026090001", ContractNo: "1234567890",
				State: provider.AgreementState("suspended"),
			},
			wantErr: ErrProviderResultUncertain,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			repo := &fakeRepository{agreementNotificationChanged: true}
			// 适配器的注册名必须与渠道声明的 Provider 对得上，否则 service 在注册表里
			// 找不到它——那会在被测的那一步之前就报错，而被测的事一次都没发生。
			adapter := tc.adapter
			if adapter == nil {
				notifier := &stubAgreementNotifier{notification: tc.notification, verifyErr: tc.verifyErr}
				notifier.name = catalog.ChannelCodeWeChatPay
				adapter = notifier
			}
			svc := newServiceWith(t, repo, adapter, catalog.CodeWechatPapay, nil,
				func(*catalog.Channel, string) string { return "secret" })

			_, err := svc.HandleAgreementNotification(context.Background(), tc.request)
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("err = %v，想要 %v", err, tc.wantErr)
			}
			if len(repo.agreementNotificationSettles) != 0 {
				t.Fatalf("被拒的通知却结算了协议：%+v", repo.agreementNotificationSettles)
			}
		})
	}
}

// TestAgreementNotificationReplayOfTheSameBodySettlesOnce 守重投那道闸。
//
// 微信会把同一条通知推好几次，而**同 body 算出同一个通知号**正是这套防重放的依据（协议通知
// 没有渠道给的通知号，见 agreementNotificationID）。三件事必须同时成立：这次不结算、回给
// 渠道的是成功应答（否则它会一直推）、以及上一次已经是 processed 时这次什么错都不报。
//
// 三种「上次那条通知现在什么状态」在这里各走一遍：processed / received（还在飞）/
// failed（我们上次拒了它）。判定本身是 decideRedelivery 的（callback_test.go 里列全了），
// 这条用例验的是**签约通知这条路上接对了没有**——两条路各调一次那个函数，接反了不会有人发现。
func TestAgreementNotificationReplayOfTheSameBodySettlesOnce(t *testing.T) {
	cases := []struct {
		name        string
		existing    string
		ageSeconds  int64
		wantErr     error
		wantSettled bool
		wantDupe    bool
	}{
		{name: "上次处理完了", existing: model.NotificationProcessed, wantDupe: true},
		{name: "上次判定无需处理", existing: model.NotificationIgnored, wantDupe: true},
		{name: "上次我们拒了它", existing: model.NotificationFailed, ageSeconds: 3600, wantErr: ErrNotificationPreviouslyRejected},
		{name: "上次那条还在飞", existing: model.NotificationReceived, ageSeconds: 5, wantErr: ErrNotificationInProgress},
		// 上一次死在了半路（进程被 kill）：这一次接手把它做完，而不是一直答「稍后再投」。
		{name: "上次死在半路", existing: model.NotificationReceived, ageSeconds: 3600, wantSettled: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			repo := &fakeRepository{
				agreement: &model.PaymentAgreement{
					ID: "agreement-1", AgreementNo: "AG2026090001",
					ContractNo: "1234567890", Status: model.AgreementStatusPending,
				},
				notification:                 redeliveryRecord(tc.existing, tc.ageSeconds),
				agreementNotificationChanged: true,
			}
			svc := agreementNotifier(t, repo, &stubAgreementNotifier{notification: signedAgreement()})

			result, err := svc.HandleAgreementNotification(context.Background(), AgreementNotificationRequest{
				ChannelCode: catalog.ChannelCodeWeChatPay, Body: []byte("<xml>the notification body</xml>"),
			})

			// 两种「重投」都还是按**同一份报文的摘要**去找那一行：这就是防重放的全部依据
			// （协议通知没有渠道给的通知号）。所以每一次都会去问一次库——被认出来是那一次
			// 查询的结果，而不是「不查也就不会重复」。
			if len(repo.notificationCalls) != 1 {
				t.Fatalf("落库调用 = %d 次，想要 1 次", len(repo.notificationCalls))
			}
			if want := digest([]byte("<xml>the notification body</xml>")); repo.notificationCalls[0].NotificationID != want {
				t.Fatalf("通知号 = %q，想要报文的 sha256 %q（重投认的就是它）",
					repo.notificationCalls[0].NotificationID, want)
			}

			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("err = %v，想要 %v", err, tc.wantErr)
				}
				if len(repo.agreementNotificationSettles) != 0 {
					t.Fatalf("重投被拒却结算了协议：%+v", repo.agreementNotificationSettles)
				}
				return
			}
			if err != nil {
				t.Fatalf("HandleAgreementNotification: %v", err)
			}
			if result.Settled != tc.wantSettled {
				t.Errorf("Settled = %v，想要 %v", result.Settled, tc.wantSettled)
			}
			if result.Duplicate != tc.wantDupe {
				t.Errorf("Duplicate = %v，想要 %v（回给渠道的必须是成功应答）", result.Duplicate, tc.wantDupe)
			}
			// 上次处理完了的：这次不结算、只回成功应答——渠道那边可以划掉了。
			if tc.wantDupe && len(repo.agreementNotificationSettles) != 0 {
				t.Errorf("重投已经处理过的那一条却又结算了一次：%+v", repo.agreementNotificationSettles)
			}
		})
	}
}

// TestAgreementNotificationMarksAFailedSettlement：结算失败时那条通知要留成 failed。
//
// 它值一条用例，是因为这一步**必须走独立连接**（业务事务已经回滚了，用同一个事务写等于把
// 「我们拒了这条通知」的留痕一起带走）。假仓储看不出连接，能验的是「有没有去标、标成什么、
// 理由是不是那个错」——真仓储那部分是仓储单测的事。
//
// 同时钉住：结算失败时协议状态改没改不是这一层能决定的（真仓储里那个事务会回滚），
// 但**调用方拿到的是那个原始错误**，不许被改写成另一个。
func TestAgreementNotificationMarksAFailedSettlement(t *testing.T) {
	repo := &fakeRepository{agreementNotificationErr: repository.ErrAgreementNotFound}
	svc := agreementNotifier(t, repo, &stubAgreementNotifier{notification: signedAgreement()})

	_, err := svc.HandleAgreementNotification(context.Background(), AgreementNotificationRequest{
		ChannelCode: catalog.ChannelCodeWeChatPay, Body: []byte("<xml>the notification body</xml>"),
	})
	if !errors.Is(err, repository.ErrAgreementNotFound) {
		t.Fatalf("err = %v，想要结算那条错误原样透出来", err)
	}
	if len(repo.markedNotifications) != 1 {
		t.Fatalf("标记通知 = %d 次，想要 1 次（这一次失败要留痕）", len(repo.markedNotifications))
	}
	marked := repo.markedNotifications[0]
	if marked.id != "n-1" || marked.status != model.NotificationFailed {
		t.Errorf("标记 = %+v，想要把刚落的 n-1 标成 %q", marked, model.NotificationFailed)
	}
	if marked.reason == "" {
		t.Error("失败理由是空的：查这条记录的人只能看到「失败了」，看不出为什么")
	}
}

// TestAgreementNotificationWithoutAChannelIsRefused 单拎一条：装配漏了密钥解析器这一类
// 「我们的配置坏了」的错，不能表现成一次成功的处理。
//
// s.secrets 为 nil 时 resolveSecret 回空串，适配器因此取不到密钥而拒——这条链是故意的
// （见 resolveSecret 的注释：绝不默认放行）。这里钉的是它的**端到端表现**：拒绝、不结算。
func TestAgreementNotificationWithoutAChannelIsRefused(t *testing.T) {
	repo := &fakeRepository{agreementNotificationChanged: true}
	notifier := &stubAgreementNotifier{
		stubProvider: stubProvider{name: catalog.ChannelCodeWeChatPay},
		verifyErr:    provider.ErrSecretNotConfigured,
	}
	svc := newServiceWith(t, repo, notifier, catalog.CodeWechatPapay, nil, nil)

	if _, err := svc.HandleAgreementNotification(context.Background(), AgreementNotificationRequest{
		ChannelCode: catalog.ChannelCodeWeChatPay, Body: []byte("<xml/>"),
	}); !errors.Is(err, provider.ErrSecretNotConfigured) {
		t.Fatalf("err = %v，想要 ErrSecretNotConfigured", err)
	}
	if len(repo.agreementNotificationSettles) != 0 {
		t.Fatalf("取不到密钥却结算了协议：%+v", repo.agreementNotificationSettles)
	}
}

// digest 独立算一遍报文的 sha256（不调被测的那个函数，否则「算法改了」测不出来）。
func digest(body []byte) string {
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:])
}
