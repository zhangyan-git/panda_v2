package repository

import (
	"errors"
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
)

// TestBrandNameConflict 覆盖「品牌重名」这条 23505 的翻译：只有认得出的约束名
// 才翻成人话，别的一律原样上抛（走 500），免得把内部故障冒充成用户自己填错了。
func TestBrandNameConflict(t *testing.T) {
	uniqueViolation := func(constraint string) *pgconn.PgError {
		return &pgconn.PgError{Code: "23505", ConstraintName: constraint}
	}
	// 未映射的错误要求「原样」返回，所以期望值就是同一个指针，不能用 errors.Is
	// 比两个内容相同的 PgError（它们不是同一个值）。
	unknownConstraint := uniqueViolation("brands_pkey")
	otherCode := &pgconn.PgError{Code: "23503", ConstraintName: "brands_merchant_id_fkey"}
	plain := errors.New("connection reset")

	for _, tt := range []struct {
		name string
		err  error
		want error
	}{
		{"同商户品牌重名", uniqueViolation("brands_merchant_id_name_key"), ErrBrandNameTaken},
		{"别的唯一约束不认", unknownConstraint, unknownConstraint},
		{"别的错误码不认", otherCode, otherCode},
		{"非 pg 错误原样返回", plain, plain},
		{"包了一层也能认出", fmt.Errorf("create brand: %w", uniqueViolation("brands_merchant_id_name_key")), ErrBrandNameTaken},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got := brandNameConflict(tt.err)
			if !errors.Is(got, tt.want) {
				t.Fatalf("got=%v want %v", got, tt.want)
			}
		})
	}
}
