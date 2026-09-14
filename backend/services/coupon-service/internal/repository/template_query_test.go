package repository

import (
	"regexp"
	"strconv"
	"testing"

	"github.com/panda-dev/panda-v2/backend/services/coupon-service/internal/model"
)

var placeholderPattern = regexp.MustCompile(`\$(\d+)`)

// maxPlaceholder 返回语句里出现的最大占位符编号。pgx 走扩展协议，绑定值只要
// 比语句引用的占位符多（或少），Postgres 就在 Bind 阶段报错，编译期看不出来。
func maxPlaceholder(query string) int {
	max := 0
	for _, m := range placeholderPattern.FindAllStringSubmatch(query, -1) {
		n, err := strconv.Atoi(m[1])
		if err != nil {
			continue
		}
		if n > max {
			max = n
		}
	}
	return max
}

func TestTemplateStatementsMatchBindArgs(t *testing.T) {
	args := len(templateArgs(&model.CouponTemplate{}))
	cases := []struct {
		name  string
		query string
		want  int
	}{
		// 全量插入：templateArgs 每一项都有对应占位符。
		{"insert", templateInsertQuery, args},
		// 更新语句是 $1 = WHERE id，加上 templateArgs 去掉 created_by 后的每一项，
		// 总数仍然等于 len(templateArgs)。回归：UpdateTemplate 曾把 templateArgs
		// 全量绑进去（多一个 created_by），模板编辑恒返回 500。
		{"update", templateUpdateQuery, args},
	}
	for _, c := range cases {
		if got := maxPlaceholder(c.query); got != c.want {
			t.Errorf("%s statement references $%d, want $%d", c.name, got, c.want)
		}
	}
}

// templateColumns 被拼进 RETURNING，里面不能出现占位符，否则编号会错位。
func TestTemplateColumnsHasNoPlaceholders(t *testing.T) {
	if got := maxPlaceholder(templateColumns); got != 0 {
		t.Fatalf("templateColumns must not contain placeholders, found $%d", got)
	}
}
