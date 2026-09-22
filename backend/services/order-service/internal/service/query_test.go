package service

import (
	"testing"

	"github.com/panda-dev/panda-v2/backend/services/order-service/internal/repository"
)

// TestUUIDFilterValidationSentinelRegistered 盯的是「仓储拒绝了，但没人认这个错误」这条路。
//
// 判据在仓储（只有那里知道哪一列是 uuid），结论要在 service 这一层被认出来，
// writeOrderError 才会回 400。两处任一漏掉，调用方拿到的都是 500——而 500 提示不出
// 「筛选框里那个字符写错了」。这条用例不用数据库，因为要验的正是这条**登记**关系。
func TestUUIDFilterValidationSentinelRegistered(t *testing.T) {
	if ErrInvalidUUIDFilter != repository.ErrInvalidUUIDFilter {
		t.Fatal("service.ErrInvalidUUIDFilter 与仓储那份不是同一个值，errors.Is 在错的一侧会判不出来")
	}
	if !IsValidationError(ErrInvalidUUIDFilter) {
		t.Fatal("ErrInvalidUUIDFilter 不在 ValidationErrors 里，后台列表填错 uuid 会以 500 回去")
	}
}
