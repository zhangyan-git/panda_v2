package service

import (
	"context"
	"log/slog"

	"github.com/panda-dev/panda-v2/backend/services/lottery-service/internal/repository"
)

// 门店名不落库（见 migrations/lottery），所以每一次要显示名字的读都要向商户域解一次。
// 这一层管的正是那一次解析，以及开通前的那一次存在性检查——两件事都只在这一个文件里，
// 因为它们的取舍是相反的（见下）。

// requireStore 确认门店在商户域里真实存在，不存在或问不到都返回错误。
//
// 它跑在**开通之前**：开通会在本库留下一行永久记录，而受理一个查无此店的门店 id 就等于
// 造一条永远没人能处理的记录（列表页上那种「幽灵门店」）。
//
// 两种失败分开返回：门店不存在是 ErrStoreNotFound（404），问不到是 ErrStoresUnavailable
// （503）。这不是措辞上的讲究——把它们合并，商户服务抖一下就会让运营以为门店被删了。
func (s *LotteryService) requireStore(ctx context.Context, storeID string) error {
	if s.stores == nil {
		return ErrStoresUnavailable
	}
	exists, err := s.stores.Exists(ctx, storeID)
	if err != nil {
		slog.ErrorContext(ctx, "checking a store before activating lottery failed",
			"store_id", storeID, "error", err)
		return ErrStoresUnavailable
	}
	if !exists {
		return ErrStoreNotFound
	}
	return nil
}

// resolveStoreNames 一次解出一页门店的名字，返回 id → 名字。
//
// **失败不返回错误，只记日志**：这个返回值只用来显示。与上面那条取舍相反是有意的——开通
// 一旦受理就留下不可撤的记录，所以校验必须失败关闭；而名字是展示，商户服务抖一下就让后台
// 的开通列表整页打不开（连停用一家店的抽奖都做不了）代价更大。名字缺了不构成谎言：门店的
// 身份是那一列 id，它来自本库。
func (s *LotteryService) resolveStoreNames(ctx context.Context, ids []string) map[string]string {
	if len(ids) == 0 || s.stores == nil {
		return nil
	}
	names, err := s.stores.Names(ctx, dedupeIDs(ids))
	if err != nil {
		slog.WarnContext(ctx, "resolving store names for lottery failed",
			"store_count", len(ids), "error", err)
		return nil
	}
	return names
}

// fillActivationNames 给一页开通记录补上门店名。
func (s *LotteryService) fillActivationNames(ctx context.Context, rows []*repository.ActivationListRow) {
	if len(rows) == 0 {
		return
	}
	ids := make([]string, 0, len(rows))
	for _, row := range rows {
		ids = append(ids, row.Activation.LocationID)
	}
	names := s.resolveStoreNames(ctx, ids)
	for _, row := range rows {
		// nil map 取下标得空串，所以解析失败时这里天然写成「没有名字」。
		row.LocationName = names[row.Activation.LocationID]
	}
}

// fillCampaignNames 给一页活动补上门店名。
//
// 同一个门店下的多个活动会带上同一个 id，dedupeIDs 把它们收成一个——一页 20 个活动很
// 可能只对应两三家店。
func (s *LotteryService) fillCampaignNames(ctx context.Context, rows []*repository.CampaignListRow) {
	if len(rows) == 0 {
		return
	}
	ids := make([]string, 0, len(rows))
	for _, row := range rows {
		ids = append(ids, row.LocationID)
	}
	names := s.resolveStoreNames(ctx, ids)
	for _, row := range rows {
		row.LocationName = names[row.LocationID]
	}
}

// dedupeIDs 去重并保持首次出现的顺序，顺带丢掉空串。
//
// 顺序不是必需的（返回值是 map），但稳定的顺序让日志和测试的输出可读，也让将来换成
// 逐条调用时不会变成随机顺序。
func dedupeIDs(ids []string) []string {
	seen := make(map[string]struct{}, len(ids))
	unique := make([]string, 0, len(ids))
	for _, id := range ids {
		if id == "" {
			continue
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		unique = append(unique, id)
	}
	return unique
}
