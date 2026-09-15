package service

import (
	"context"
	"errors"
	"log/slog"

	"github.com/panda-dev/panda-v2/backend/services/lottery-service/internal/model"
)

// DefaultSweepBatch 是修复 worker 每轮取多少条。
//
// 200 而不是「一次全捞」：卡住的记录在正常运行时应该是**零**，出现一条就是一次事故。批量
// 存在的意义是「万一积压了一堆，别把一轮拖成一个事务」，不是为了吞吐。
const DefaultSweepBatch = 200

// SweepResult 是一轮修复的结果。
//
// 三个数分开而不是一个「处理了几条」：运维要知道的是**还剩几条没落地**，而 StillPending
// 正是那个数。把它与 Settled 合成一个计数，会让「一直在重试、一条也没成」看起来与
// 「都收尾了」一模一样。
type SweepResult struct {
	// Scanned 是这一轮扫描到的条数（已经老过 RepairAfter 的那些）。
	Scanned int
	// Settled 是这一轮**得出了结论**的条数：成交、余额不足、期次已结束（含退卡）。
	Settled int
	// StillPending 是这一轮跑完之后仍然未决的条数。它不为零说明账户域在出问题。
	StillPending int
}

// SweepParticipations 是修复 worker 的入口：重跑卡在 pending 的参与记录。
//
// # 为什么需要它
//
// 参与是「先落本地记录 → 扣卡 → 再确认」三段，跨服务调用夹在中间，所以有两处会留下一条
// 永远不动的 pending：
//
//   - 进程在第二段中途死了；
//   - 扣卡调用超时（我们不知道卡扣没扣）。
//
// 这两种情况下用户的卡**可能已经被扣了**，而参与没有进期次。没有这个 worker，那张卡就
// 悬在那里——用户查余额少了一张，抽奖中心里却查不到任何一笔参与。
//
// # 为什么它安全
//
// 重跑走的是 settle，与第一次尝试逐字相同的那段代码；扣减请求里的 request_id 就是参与
// 记录的 id，账户域拿它派生幂等键。所以无论重跑多少次，卡最多扣一张，计数最多加一次。
//
// # 为什么不设重试上限
//
// 传输层一直失败的行是「**真的不知道扣没扣**」。到点判它 failed 是在撒谎：用户可能已经
// 被扣了卡却什么也得不到，而我们连一行需要人工去退的痕迹都没留下。所以这里没有上限，
// attempts 与 last_error 只用于观测——**超过一小时仍未决就该告警**（worker 那边按
// StillPending 与 attempts 判）。
//
// limit 传 0 时用 DefaultSweepBatch。
func (s *LotteryService) SweepParticipations(ctx context.Context, limit int) (SweepResult, error) {
	if limit <= 0 {
		limit = DefaultSweepBatch
	}
	ids, err := s.repository.PendingParticipations(ctx, s.repairAfter, limit)
	if err != nil {
		return SweepResult{}, err
	}

	result := SweepResult{Scanned: len(ids)}
	for _, id := range ids {
		// 每一条记录重新读一遍当前状态，而不是信扫描那一刻的快照：这一轮里前面几条的处理
		// 可能耗掉几秒，而扫描之后、处理之前这中间可能有别的路径（用户自己重试、另一个
		// 副本的 worker）已经把这条收尾了。
		participation, err := s.repository.GetParticipation(ctx, id)
		if err != nil {
			if errors.Is(err, ErrParticipationNotFound) {
				continue
			}
			// 读不出来不影响这一轮继续——剩下的记录还等着收尾。记下来，下一轮再来。
			slog.ErrorContext(ctx, "reading a pending lottery participation failed",
				"participationId", id, "error", err)
			result.StillPending++
			continue
		}
		if participation.Status != model.ParticipationPending {
			continue
		}

		if _, err := s.settle(ctx, participation); err != nil {
			if errors.Is(err, ErrParticipationPending) {
				// 这一轮还是没得出结论（账户域仍然不可达）。这不是错误，是「再等等」——
				// 但它是**唯一**需要被看见的那种「没做成」，所以单独计数。
				result.StillPending++
				continue
			}
			if IsValidationError(err) {
				// 走不到：settle 的输入是从库里读出来的，不经过请求校验。真出现说明有个
				// 我们没想到的分支，记下来别静默。
				slog.ErrorContext(ctx, "sweeping a lottery participation hit an unexpected validation error",
					"participationId", id, "error", err)
				result.StillPending++
				continue
			}
			// 余额不足、期次已结束（含退卡被拒那条警情）——都是**结论**，不是故障。
			// 记录已经落到终态了，这条不再需要重跑。
			slog.InfoContext(ctx, "a pending lottery participation reached a conclusion",
				"participationId", id, "error", err)
			result.Settled++
			continue
		}
		result.Settled++
	}
	return result, nil
}
