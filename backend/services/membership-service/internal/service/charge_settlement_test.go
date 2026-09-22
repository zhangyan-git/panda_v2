package service

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/panda-dev/panda-v2/backend/services/membership-service/internal/client"
	"github.com/panda-dev/panda-v2/backend/services/membership-service/internal/repository"
)

// 待办那两条纯规则：怎么退避、什么算「重试有用」。
//
// 它们都没有库可依赖，所以这一份**不挂在集成测试上**（`go test ./...` 不给库地址时那些用例是
// t.Skip，只有这一份照常跑）。这两条偏偏又是最该被钉住的——它们错了不会报错：
//
//	退避错了          这张表要么永远不重试（订单域恢复了也补不上），要么一分钟一轮地吵到天荒地老
//	可重试判反了      判宽了是一条吵闹的待办（人来了能删），判严了是**这笔钱从此没有人管**

// TestSettlementBackoff 钉住退避的档位、封顶与两端。
//
// 值本身不重要，重要的是**它的形状**：前几档必须短于 worker 的一分钟扫描周期（订单域重启两三分钟
// 这种最常见的抖动，靠的就是「下一轮再试」），之后单调不降、最终停在一小时（一张永远修不好的待办
// 不能一秒一条 ERROR 地响下去）。
func TestSettlementBackoff(t *testing.T) {
	cases := []struct {
		attempts int
		want     time.Duration
		why      string
	}{
		{1, time.Second, "第一次失败：下一轮就再试"},
		{6, 32 * time.Second, "第六次仍在扫描周期以内"},
		{7, 64 * time.Second, "第七次开始超过一分钟的扫描周期"},
		{12, 2048 * time.Second, "第十二次约半小时"},
		{13, time.Hour, "第十三次（2^12 秒）越过封顶，被削平"},
		{40, time.Hour, "再往后一直是封顶值，不再翻倍"},
		{0, time.Second, "没认领过（不该出现）时按第一次算，不该退成零"},
		{-3, time.Second, "负数同理，且不能让它移位溢出"},
	}

	for _, tc := range cases {
		if got := settlementBackoff(tc.attempts); got != tc.want {
			t.Errorf("attempts=%d 时退避 %v，想要 %v（%s）", tc.attempts, got, tc.want, tc.why)
		}
	}

	// 单调不降：认领时 attempts 只增不减，退避却缩回去的话，一张修不好的待办会重新变得吵闹。
	previous := time.Duration(0)
	for attempts := 1; attempts <= 40; attempts++ {
		got := settlementBackoff(attempts)
		if got < previous {
			t.Fatalf("attempts=%d 的退避 %v 比上一档 %v 还短", attempts, got, previous)
		}
		if got > settlementBackoffCap {
			t.Fatalf("attempts=%d 的退避 %v 超过封顶 %v", attempts, got, settlementBackoffCap)
		}
		previous = got
	}
}

// TestOrderFailureCanBeRetried 钉住「哪些建单失败值得落成待办」这条分界。
//
// 分两半看：
//
//	不能重试的（返回 false → 进死信）  重投一百次也是同一个答案的那些：事件体自己坏了、这个进程
//	                                  压根没接订单域、订阅挂在坏数据上、订单域**明确答了「不建」**
//	能重试的（返回 true → 落成待办）    订单域没答上来（连不上、超时、应答是坏的）——过一会儿就好了
//
// 包装过的错误也要认出来：建单那条路上错误是层层往上裹的（`fmt.Errorf("...: %w", err)`），
// 只在最外层做 errors.Is 会全部落进 default 那一支。
func TestOrderFailureCanBeRetried(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
		why  string
	}{
		{
			name: "事件体坏了",
			err:  fmt.Errorf("build renewal order: %w", ErrInvalidEvent),
			want: false,
			why:  "重投不会让那个缺失的字段出现",
		},
		{
			name: "进程没接订单域",
			err:  ErrChannelUnavailable,
			want: false,
			why:  "重投不会把这个依赖装上，那是改部署的事",
		},
		{
			name: "订阅挂在不存在的会员上",
			err:  fmt.Errorf("load renewal context: %w", repository.ErrMembershipNotFound),
			want: false,
			why:  "一行坏数据，重试不会变好",
		},
		{
			name: "订单域拒收",
			err:  fmt.Errorf("create renewal order: %w", client.ErrRenewalOrderRejected),
			want: false,
			why:  "对面**答了**，答案是「这单我不建」",
		},
		{
			name: "流水号已被别种单占用",
			err:  client.ErrRenewalOrderConflict,
			want: false,
			why:  "同上，是结论不是抖动",
		},
		{
			name: "订单域说查无此单",
			err:  client.ErrOrderNotFound,
			want: false,
			why:  "同上",
		},
		{
			name: "订单域连不上",
			err:  client.ErrOrderUnavailable,
			want: true,
			why:  "过一会儿就好了，这正是待办表存在的理由",
		},
		{
			name: "订单域超时",
			err:  fmt.Errorf("create renewal order: %w", context.DeadlineExceeded),
			want: true,
			why:  "没问到结论，默认按能重试算",
		},
		{
			name: "认不出来的错误",
			err:  errors.New("something new"),
			want: true,
			why:  "判错的代价两侧不对称：多一条吵闹的待办 远轻于 这笔钱从此没人管",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := orderFailureCanBeRetried(tc.err); got != tc.want {
				t.Errorf("orderFailureCanBeRetried(%v) = %v，想要 %v（%s）", tc.err, got, tc.want, tc.why)
			}
		})
	}
}
