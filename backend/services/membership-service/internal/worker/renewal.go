package worker

import (
	"context"
	"log/slog"
	"time"

	"github.com/panda-dev/panda-v2/backend/services/membership-service/internal/repository"
	"github.com/panda-dev/panda-v2/backend/services/membership-service/internal/service"
)

// DefaultRenewalInterval 是续费扫描的默认周期。
//
// 与到期扫描那个 DefaultSweepInterval 是**两个旋钮、今天取同一个值**，故意不合成一个常量：
// 它们的代价与后果完全不同（一个只在本库跑 UPDATE，一个每次都要出网调渠道），将来要调也是
// 各调各的。合成一个的话，为了让扣款更及时而把它从十分钟调成两分钟，会顺手把到期扫描也变成
// 五分钟一轮的全表扫。
//
// 十分钟够快：next_charge_at 到的是会员到期的时刻，晚几分钟扣到钱不会让任何人少享一秒权益
// （判定读的是 expire_at）。而渠道那边的重试退避是 24 小时起步，这一层再密也快不过它。
const DefaultRenewalInterval = 10 * time.Minute

// RenewalWorker 扫到点的订阅，向渠道发起这一期的扣款。
//
// 它**只发起**：钱到没到由渠道推回来的事件决定，这里一行订阅都不改（见 service.ChargeDue）。
type RenewalWorker struct {
	memberships *service.MembershipService
	interval    time.Duration
	batch       int
}

// NewRenewalWorker 构造续费扫描任务。interval 或 batch 非正时用默认值。
func NewRenewalWorker(memberships *service.MembershipService, interval time.Duration, batch int) *RenewalWorker {
	if interval <= 0 {
		interval = DefaultRenewalInterval
	}
	if batch <= 0 {
		batch = repository.DefaultChargeBatch
	}
	return &RenewalWorker{memberships: memberships, interval: interval, batch: batch}
}

// Run 先扫一次再按周期扫。先扫一次的理由与到期扫描逐字相同（见 expiry.go）：重启期间到点的
// 订阅堆着，等一个完整周期才处理，会让刚发布完的那几分钟里的续费全部延后。
func (w *RenewalWorker) Run(ctx context.Context) error {
	w.sweep(ctx)

	ticker := time.NewTicker(w.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			w.sweep(ctx)
		}
	}
}

// sweep 发起一批扣款。
//
// # 为什么这里没有到期扫描那个「排空循环」
//
// 到期扫描每处理一条就把那条从待办里移走（status 改成 expired），所以「扫满一批就再扫一批」
// 是在消化积压。代扣**不移走任何东西**——这一期扣成之前 next_charge_at 不动，于是同一个
// LIMIT 每次捞回来的是同一批人。照抄那个循环的后果是一个 tick 里把同一批订阅发十遍
// （maxSweepsPerTick 次），每一遍都是一轮出网调用，换不来任何新进展。
//
// # 那为什么每轮都重发同一批
//
// 因为这正是重试的机制本身：支付侧按 (协议, 期次) 幂等，已经受理的期次原样回（不再碰渠道），
// 被拒的那期按 24h×n 退避、到点才真的再发一次。本服务不需要自己记「这期发过没有」——那个
// 状态在支付库的那一行上，而且它比本服务多知道一件事：钱到底到没到。
//
// 已知的代价：一条**通知丢了**的期次会永远停在 charging，此后每一轮都白发一次 gRPC 并把
// 日志刷一行。这是这一刀接受的缺口（没有查单兜底，见计划 §五），不是可以靠在这里加状态糊过去
// 的——糊过去只会让「这期到底怎么样了」多出一个本服务自己编的答案。
func (w *RenewalWorker) sweep(ctx context.Context) {
	// traceID 传空串：这是一次没有来路的后台扫描（同 expiry.go）。
	charged, err := w.memberships.ChargeDue(ctx, w.batch, "")
	if err != nil {
		// 记下来继续听，不让循环退出。退出会让这个副本从此不再发起任何续费，而且没有任何外部
		// 迹象——订阅还是 active、到期日还是照旧，只是钱再也不扣了。
		slog.Error("membership renewal sweep", "error", err)
		return
	}
	if charged == 0 {
		return
	}
	// 满载说明积压比一批深。**不在这里接着扫**（见函数说明），只留一条日志：下一轮十分钟后
	// 自己会来，而这条日志是「是不是该把批量调大」唯一的依据。
	if charged >= w.batch {
		slog.Info("membership renewal sweep filled its batch; the backlog may be deeper than one batch",
			"batch", w.batch, "charged", charged)
	}
}
