package controller

import (
	"github.com/panda-dev/panda-v2/backend/services/account-service/internal/dto"
	"github.com/panda-dev/panda-v2/backend/services/account-service/internal/model"
)

// 咖啡豆那一半的 model → DTO 搬运。规矩与 fortune_card_response.go 逐条相同：model 上
// 只有 db tag，直接序列化会输出 PascalCase，前端按 camelCase 取值会全部落空。

func beanEntryResponse(entry *model.CoffeeBeanEntry) *dto.BeanEntryResponse {
	response := &dto.BeanEntryResponse{
		ID:            entry.ID,
		UserID:        entry.UserID,
		EntryType:     entry.EntryType,
		Amount:        entry.Amount,
		BalanceAfter:  entry.BalanceAfter,
		Title:         entry.Title,
		ReferenceType: entry.ReferenceType,
		ReferenceID:   entry.ReferenceID,
		ReferenceNo:   entry.ReferenceNo,
		OperatorID:    entry.OperatorID,
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

func beanEntryResponses(items []*model.CoffeeBeanEntry) []*dto.BeanEntryResponse {
	// 空切片而不是 nil：nil 会序列化成 null，前端就得多写一层兜底才能判断「没有流水」。
	out := make([]*dto.BeanEntryResponse, 0, len(items))
	for _, item := range items {
		if item != nil {
			out = append(out, beanEntryResponse(item))
		}
	}
	return out
}
