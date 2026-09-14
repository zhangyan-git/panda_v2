package dto_test

import (
	"testing"

	"github.com/panda-dev/panda-v2/backend/platform/api"
	"github.com/panda-dev/panda-v2/backend/services/coupon-service/internal/dto"
)

// 这个常量不能再偷偷收紧。前端「要全集」的统一参数
// （admin-web/src/services/pagination.ts 的 FULL_PAGE_PARAMS）发的是
// api.MaxPageSize，任何比它小的上限都会让那些请求 400，而调用方写的是
// `catch {}` 只为了「取不到字典就退回显示 id」——于是页面安静地铺一串 uuid，
// 没有任何报错。2026-09 用户券/发券批次的模板名、批次号就是这么丢的。
func TestMaxPageSizeMatchesPlatform(t *testing.T) {
	if dto.MaxPageSize != api.MaxPageSize {
		t.Fatalf("dto.MaxPageSize=%d 与 platform/api.MaxPageSize=%d 不一致：前端的 FULL_PAGE_PARAMS 会在这一层被拒",
			dto.MaxPageSize, api.MaxPageSize)
	}
}
