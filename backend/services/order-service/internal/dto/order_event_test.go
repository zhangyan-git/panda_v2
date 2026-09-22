package dto

import (
	"encoding/json"
	"sort"
	"testing"
)

// TestEventPayloadFieldNamesMatchTheConsumer 是这几个事件体的**契约测试**。
//
// 期望的字段名是手抄进来的，抄的是 backend/services/account-service/internal/dto/order_event.go
// ——那份镜像 struct 的 json tag 就是消费方真正会去读的名字（那边的手工镜像测试在
// internal/service/event.go 里，解码时开着 DisallowUnknownFields）。所以这里断言的是
// 「线上两个服务之间的线上契约」，不是「我们自己的 struct 长什么样」。
//
// 两种改法的后果不同，但都测不出来，所以必须钉在用例里：
//   - 少一个字段（改名、加 omitempty、删掉）：消费方那边**不会报错**，只是永远读到零值。
//     一条 after_sale.applied 少了 fortuneCardEntryKeys 就是「不冻卡」，退款申请照过，
//     福卡照花；这种错在日志里一句话都没有。
//   - 多一个字段：DisallowUnknownFields 当场报错，整条消息进死信——退款申请跟着一起丢。
//
// 用零值序列化：json tag 上没有 omitempty 时零值也会出现在键里，所以键集合相等这一条同时
// 钉住了「不许给这些字段加 omitempty」。加了 omitempty 之后，一个空的 orderLineId（整单退
// 就是空的）会从 JSON 里整条消失，消费方读到的与「字段是空」不再是同一件事。
func TestEventPayloadFieldNamesMatchTheConsumer(t *testing.T) {
	cases := []struct {
		name    string
		payload any
		// want 是消费方那份镜像 struct 的 json tag，**逐字**抄下来的。
		want []string
	}{
		{
			name:    "order.completed",
			payload: OrderCompletedEventPayload{},
			want:    []string{"orderNo", "orderId", "userId", "finishedAtUnix", "fortuneCards"},
		},
		{
			name:    "order.completed 的一笔发放",
			payload: OrderFortuneGrant{},
			want:    []string{"kind", "campaignId", "campaignName", "amount", "entryKey"},
		},
		{
			name:    "order.after_sale.applied",
			payload: AfterSaleAppliedEventPayload{},
			want: []string{"afterSaleId", "afterSaleNo", "orderId", "orderNo", "userId",
				"scope", "orderLineId", "refundAmount", "fortuneCardEntryKeys"},
		},
		{
			name:    "order.after_sale.reviewed",
			payload: AfterSaleReviewedEventPayload{},
			want: []string{"afterSaleId", "afterSaleNo", "orderId", "orderNo", "userId",
				"status", "action", "scope", "orderLineId", "refundAmount"},
		},
		{
			name:    "order.after_sale.cancelled",
			payload: AfterSaleCancelledEventPayload{},
			want:    []string{"afterSaleId", "afterSaleNo", "orderId", "orderNo", "userId", "status", "reason"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			raw, err := json.Marshal(tc.payload)
			if err != nil {
				t.Fatalf("序列化 %s 失败：%v", tc.name, err)
			}
			var decoded map[string]json.RawMessage
			if err := json.Unmarshal(raw, &decoded); err != nil {
				t.Fatalf("序列化出来的不是 JSON 对象：%v", err)
			}

			got := make([]string, 0, len(decoded))
			for key := range decoded {
				got = append(got, key)
			}
			sort.Strings(got)
			want := append([]string(nil), tc.want...)
			sort.Strings(want)

			if len(got) != len(want) {
				t.Fatalf("%s 的字段数是 %d（%v），消费方的镜像 struct 认的是 %d 个（%v）",
					tc.name, len(got), got, len(want), want)
			}
			for i := range want {
				if got[i] != want[i] {
					t.Fatalf("%s 第 %d 个字段是 %q，消费方期望 %q（全量：%v）",
						tc.name, i, got[i], want[i], got)
				}
			}
		})
	}
}
