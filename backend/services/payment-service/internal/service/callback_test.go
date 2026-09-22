package service

import (
	"context"
	"errors"
	"testing"

	"github.com/panda-dev/panda-v2/backend/services/payment-service/internal/catalog"
	"github.com/panda-dev/panda-v2/backend/services/payment-service/internal/model"
	"github.com/panda-dev/panda-v2/backend/services/payment-service/internal/provider"
	"github.com/panda-dev/panda-v2/backend/services/payment-service/internal/repository"
)

// 这一组用例守的是回调重投的那个分叉：同样一条已经落过库的通知，上一次的处理是「做完了」
// 「被拒了」还是「根本没做完」，对渠道的答复与副作用都不一样。
//
// handleRedelivery 不碰仓储，所以它自己那部分直接调就行；分叉之后**会不会真的去结算**则由
// 下面那条走 HandleNotification 的用例盖住——decide 与 wire 是两件事，只测前者的话，把
// 调用点的 `if handled { return }` 写反了没有任何一条用例会红。

func redeliveryRecord(status string, ageSeconds int64) *repository.NotificationRecord {
	return &repository.NotificationRecord{
		ID: "n-1", ExistingStatus: status, ExistingAgeSeconds: ageSeconds,
	}
}

func redeliveryNotification() provider.Notification {
	return provider.Notification{
		NotificationID: "wx-notify-1", EventType: provider.EventSucceeded,
		PaymentNo: "PAY20260915120000000001", Amount: 1980,
	}
}

// TestHandleRedeliveryDecidesByExistingStatusAndAge 钉住每一种「上次那条通知现在什么状态」
// 各自该得到什么答复。
//
// 表里每一行的 wantHandled / wantErr 都是**行为**而不是实现细节：wantHandled=false 意味着
// 这一次要接手把上次没做完的做完，对调用方来说就是「当成一条新回调继续走」。
func TestHandleRedeliveryDecidesByExistingStatusAndAge(t *testing.T) {
	cases := []struct {
		name        string
		status      string
		ageSeconds  int64
		wantHandled bool
		wantErr     error
		wantDupe    bool
	}{
		// 上次处理完了：回成功应答，一点状态都不碰——防重放靠的就是这条。
		{"上次已处理", model.NotificationProcessed, 0, true, nil, true},
		{"上次判定无需处理", model.NotificationIgnored, 0, true, nil, true},
		// 上次我们拒了它，这次也得拒：改口会让渠道把这条从重投队列里划掉。
		{"上次已拒绝", model.NotificationFailed, 3600, true, ErrNotificationPreviouslyRejected, false},
		// 还在窗口内 = 上一次可能正飞在半路，让渠道稍后再投。
		{"received，刚落下", model.NotificationReceived, 0, true, ErrNotificationInProgress, false},
		{"received，恰好在窗口边界", model.NotificationReceived, 30, true, ErrNotificationInProgress, false},
		// 出了窗口 = 上一次死在了半路。没有任何东西会去动那一行，这次必须接手。
		{"received，刚过窗口", model.NotificationReceived, 31, false, nil, false},
		{"received，很久以前", model.NotificationReceived, 86400, false, nil, false},
		// 读不到状态行 = 另一条投递还没提交，那是并发，不是孤儿。
		{"状态读不到（并发）", "", 0, true, ErrNotificationInProgress, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			svc := &PaymentService{}
			duplicate, handled, err := svc.decideRedelivery(
				context.Background(), redeliveryRecord(tc.status, tc.ageSeconds),
				redeliveryNotification().NotificationID)

			if handled != tc.wantHandled {
				t.Fatalf("handled = %v, want %v（false 表示这次要接手做完）", handled, tc.wantHandled)
			}
			if tc.wantErr == nil {
				if err != nil {
					t.Fatalf("err = %v, want nil", err)
				}
			} else if !errors.Is(err, tc.wantErr) {
				t.Fatalf("err = %v, want %v", err, tc.wantErr)
			}
			if duplicate != tc.wantDupe {
				t.Fatalf("duplicate = %v, want %v", duplicate, tc.wantDupe)
			}
			// 不报错又拦下的那些分支必须回**成功应答**（duplicate），不能既不报错又不认下来：
			// 那样调用方会以为该静默返回，而渠道那边什么都没收到。报错的那几个另说——它们的
			// 答复就是那个错误。
			if handled && tc.wantErr == nil && !duplicate {
				t.Fatal("拦下又不报错的那些分支必须回成功应答（duplicate）")
			}
		})
	}
}

// TestHandleNotificationTakesOverAnAbandonedNotification 守调用点的接线：
// 一条躺着很久的 received 必须走到结算，而一条刚落下的 received 不许走。
//
// 两条对照缺一不可。只测「孤儿会被结算」的话，把窗口取成 0 也能过——而那会让每一次正常的
// 并发重投都变成一次重复结算；只测「新鲜的会被拦住」的话，把 default 分支写成无条件拦下
// 就是原来那个 bug 本身了。
func TestHandleNotificationTakesOverAnAbandonedNotification(t *testing.T) {
	settled := func(t *testing.T, svc *PaymentService) (*CallbackResult, error) {
		t.Helper()
		return svc.HandleNotification(context.Background(), CallbackRequest{
			ChannelCode: catalog.ChannelCodeUMS, Body: []byte(`{"notification":"body"}`),
		})
	}

	cases := []struct {
		name string
		age  int64
		// wantSettled 为真表示这次确实走到了结算（副作用发生了）。
		wantSettled bool
		wantErr     error
	}{
		{
			// 上一次投递死在半路留下的孤儿行：这次接手，真的去结算。
			name: "孤儿行由这一次接手结算", age: 3600,
			wantSettled: true,
		},
		{
			// 上一次投递可能正飞在半路：不许替它做决定，也不许产生任何副作用。
			name: "窗口内不接手", age: 5,
			wantErr: ErrNotificationInProgress,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			repo := &fakeRepository{
				notification: redeliveryRecord(model.NotificationReceived, tc.age),
				settle: &repository.PaymentSettlement{
					Payment: &model.Payment{PaymentNo: "PAY20260915120000000001", Status: model.PaymentSucceeded},
				},
			}
			stub := &stubProvider{verifyOK: true, notification: redeliveryNotification()}
			svc := newTestService(t, repo, stub, catalog.CodeUMSMiniappWechat)

			result, err := settled(t, svc)

			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("err = %v, want %v", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("err = %v, want nil", err)
			}
			// Settled 只可能由「真的走完了结算」得到：上面那个错误分支根本到不了那里。
			if result.Settled != tc.wantSettled {
				t.Fatalf("Settled = %v, want %v", result.Settled, tc.wantSettled)
			}
		})
	}
}

// TestCallbackAsksForExactlyTheDeclaredSlots 与下单那条同形，但这条路上取错槽的**表现**不一样，
// 所以值得自己一条：下单取错槽是签名被对方拒（有一句明确的错误），回调取错槽是「这条回调验不过」
// ——渠道会照着重投，而我们的日志上只有签名不匹配，看不出根因是配置。
//
// 断言的是集合与顺序，不是「至少问过一次」：一个「声明的槽解析成空串」的渠道必须在名单上
// 就看得出来（见 resolveSecrets）——今天没有兜底那一把了，落空就是落空。
func TestCallbackAsksForExactlyTheDeclaredSlots(t *testing.T) {
	stub := &stubProvider{
		verifyOK:     true,
		notification: redeliveryNotification(),
		slots:        []string{"platformCertificate", "apiV3Key"},
	}
	secrets := &stubSecretResolver{values: map[string]string{
		"platformCertificate": "cert", "apiV3Key": "key",
	}}
	repo := &fakeRepository{
		// 孤儿行（落在窗口外），这一次接手把它做完——与上面那条用例里的同一档，好让这条
		// 用例只在「问了哪些槽」上有所断言，不掺别的分叉。
		notification: redeliveryRecord(model.NotificationReceived, 3600),
		settle: &repository.PaymentSettlement{
			Payment: &model.Payment{PaymentNo: "PAY20260915120000000001", Status: model.PaymentSucceeded},
		},
	}
	svc := newServiceWith(t, repo, stub, catalog.CodeUMSMiniappWechat, nil, secrets.resolve)

	if _, err := svc.HandleNotification(context.Background(), CallbackRequest{
		ChannelCode: catalog.ChannelCodeUMS, Body: []byte(`{"notification":"body"}`),
	}); err != nil {
		t.Fatalf("HandleNotification: %v", err)
	}

	secrets.askedOnly(t, "platformCertificate", "apiV3Key")
}
