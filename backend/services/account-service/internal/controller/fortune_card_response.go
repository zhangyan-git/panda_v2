package controller

import (
	"github.com/panda-dev/panda-v2/backend/services/account-service/internal/dto"
	"github.com/panda-dev/panda-v2/backend/services/account-service/internal/model"
)

// 这一层只做 model → DTO 的搬运：model 上只有 db tag，直接序列化会输出 PascalCase，
// 前端按 camelCase 取值会全部落空。所有对外响应都必须经过这里。

func entryResponse(entry *model.FortuneCardEntry) *dto.EntryResponse {
	response := &dto.EntryResponse{
		ID:            entry.ID,
		UserID:        entry.UserID,
		EntryType:     entry.EntryType,
		Amount:        entry.Amount,
		BalanceAfter:  entry.BalanceAfter,
		Title:         entry.Title,
		ReferenceType: entry.ReferenceType,
		ReferenceID:   entry.ReferenceID,
		ReferenceNo:   entry.ReferenceNo,
		Remark:        entry.Remark,
		OccurredAt:    entry.OccurredAt,
		CreatedAt:     entry.CreatedAt,
	}
	// 空指针换成空串：冲正那一列在界面上是「冲的哪一笔」，绝大多数行没有它。
	// 回 null 会让前端多写一层兜底才能渲染同一列。
	if entry.ReversesEntryID != nil {
		response.ReversesEntryID = *entry.ReversesEntryID
	}
	return response
}

func entryResponses(items []*model.FortuneCardEntry) []*dto.EntryResponse {
	// 空切片而不是 nil：nil 会序列化成 null，前端就得多写一层兜底才能判断「没有流水」。
	out := make([]*dto.EntryResponse, 0, len(items))
	for _, item := range items {
		if item != nil {
			out = append(out, entryResponse(item))
		}
	}
	return out
}

func freezeResponse(freeze *model.FortuneCardFreeze) *dto.FreezeResponse {
	response := &dto.FreezeResponse{
		ID:          freeze.ID,
		UserID:      freeze.UserID,
		AfterSaleNo: freeze.AfterSaleNo,
		OrderID:     freeze.OrderID,
		OrderNo:     freeze.OrderNo,
		EntryKeys:   freeze.EntryKeys,
		Amount:      freeze.Amount,
		Status:      freeze.Status,
		Reason:      freeze.Reason,
		OccurredAt:  freeze.OccurredAt,
		ReleasedAt:  freeze.ReleasedAt,
		RecoveredAt: freeze.RecoveredAt,
		CreatedAt:   freeze.CreatedAt,
		UpdatedAt:   freeze.UpdatedAt,
	}
	if response.EntryKeys == nil {
		// 与 entryResponses 同一条：数据库里的空数组经 pgx 读回来可能是 nil，
		// 序列化成 null 会让前端在「冻了什么」这一列上多写一层兜底。
		response.EntryKeys = []string{}
	}
	return response
}

func freezeResponses(items []*model.FortuneCardFreeze) []*dto.FreezeResponse {
	out := make([]*dto.FreezeResponse, 0, len(items))
	for _, item := range items {
		if item != nil {
			out = append(out, freezeResponse(item))
		}
	}
	return out
}
