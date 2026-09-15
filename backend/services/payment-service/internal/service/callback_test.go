package service

import (
	"context"
	"errors"
	"testing"

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
			result, handled, err := svc.handleRedelivery(
				context.Background(), redeliveryRecord(tc.status, tc.ageSeconds), redeliveryNotification())

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
			// 接手的那些分支必须给出应答形状——除了报错的那几个，它们的结果就是错误本身。
			if handled && tc.wantErr == nil && result == nil {
				t.Fatal("接手的那些分支必须给出应答形状")
			}
			if result != nil && result.Duplicate != tc.wantDupe {
				t.Fatalf("Duplicate = %v, want %v", result.Duplicate, tc.wantDupe)
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
			ChannelCode: "stub_dev", Body: []byte(`{"notification":"body"}`),
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
				// 回调这条路第一步就是按 URL 里那段查渠道；不摆这一行，HandleNotification
				// 会在读渠道字段时直接空指针。
				channel: &repository.ChannelRecord{
					Channel: &model.PaymentChannel{
						ID: testChannel, Code: "stub_dev", Provider: "stub",
						Mode: "sandbox", Status: model.ChannelEnabled, SecretRef: "TEST_SECRET",
					},
					Config: map[string]string{},
				},
				notification: redeliveryRecord(model.NotificationReceived, tc.age),
				settle: &repository.PaymentSettlement{
					Payment: &model.Payment{PaymentNo: "PAY20260915120000000001", Status: model.PaymentSucceeded},
				},
			}
			stub := &stubProvider{verifyOK: true, notification: redeliveryNotification()}
			svc := newTestService(t, repo, stub, model.ActionNativePay)

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
