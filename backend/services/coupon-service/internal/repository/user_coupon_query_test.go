package repository

import (
	"testing"

	"github.com/panda-dev/panda-v2/backend/services/coupon-service/internal/dto"
)

// 用户券列表的筛选条件是逐个追加的，占位符编号跟着参数走；取数语句再把
// LIMIT/OFFSET 接在后面。这条用例钉的是「编号与参数个数一一对应」——
// 错位不会编译失败，只会让翻页翻不动或者筛错列。见 template_query_test.go
// 里 maxPlaceholder 的说明。
func TestUserCouponListWhereNumbersPlaceholdersInOrder(t *testing.T) {
	cases := []struct {
		name     string
		q        dto.UserCouponQuery
		wantSQL  string
		wantArgs []any
	}{
		{
			name:     "没有筛选时只剩恒真条件",
			q:        dto.UserCouponQuery{},
			wantSQL:  "1=1",
			wantArgs: []any{},
		},
		{
			name:     "按券 id 查单张",
			q:        dto.UserCouponQuery{ID: "cb293ce2"},
			wantSQL:  "1=1 AND id::text=$1",
			wantArgs: []any{"cb293ce2"},
		},
		{
			name:     "按类型码筛",
			q:        dto.UserCouponQuery{CouponTypeCode: "COFFEE_CASH"},
			wantSQL:  "1=1 AND coupon_type_code=$1",
			wantArgs: []any{"COFFEE_CASH"},
		},
		{
			name:     "六个条件全给时编号依次递增",
			q:        dto.UserCouponQuery{ID: "c1", CouponTypeCode: "COFFEE_CASH", UserID: "u1", Status: "claimed", BatchID: "b1", TemplateID: "t1"},
			wantSQL:  "1=1 AND id::text=$1 AND user_id::text=$2 AND status=$3 AND batch_id::text=$4 AND template_id::text=$5 AND coupon_type_code=$6",
			wantArgs: []any{"c1", "u1", "claimed", "b1", "t1", "COFFEE_CASH"},
		},
		{
			name:     "空串是不筛，不是筛空字符串",
			q:        dto.UserCouponQuery{Status: "redeemed", ID: ""},
			wantSQL:  "1=1 AND status=$1",
			wantArgs: []any{"redeemed"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			sql, args := userCouponListWhere(c.q)
			if sql != c.wantSQL {
				t.Errorf("where = %q, want %q", sql, c.wantSQL)
			}
			if len(args) != len(c.wantArgs) {
				t.Fatalf("bound %d args, want %d", len(args), len(c.wantArgs))
			}
			for i := range args {
				if args[i] != c.wantArgs[i] {
					t.Errorf("arg %d = %v, want %v", i, args[i], c.wantArgs[i])
				}
			}
			if got := maxPlaceholder(sql); got != len(args) {
				t.Errorf("statement references $%d but %d args are bound", got, len(args))
			}
		})
	}
}
