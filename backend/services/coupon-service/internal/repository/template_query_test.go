package repository

import (
	"regexp"
	"strconv"
	"testing"

	"github.com/panda-dev/panda-v2/backend/services/coupon-service/internal/dto"
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

// 筛选条件的占位符必须严格按 args 的出现顺序编号：ListTemplates 的取数语句把
// LIMIT/OFFSET 接在筛选参数后面，编号一旦错位，错的是「第几页」而不是报错——
// 症状是翻页翻不动或者少拿一行，很难往 SQL 上想。
func TestTemplateListWhereNumbersPlaceholdersInOrder(t *testing.T) {
	cases := []struct {
		name     string
		q        dto.CouponTemplateQuery
		wantSQL  string
		wantArgs []any
	}{
		{
			name:     "没有筛选时只剩恒真条件",
			q:        dto.CouponTemplateQuery{},
			wantSQL:  "1=1",
			wantArgs: []any{},
		},
		{
			name:     "只按名称模糊筛",
			q:        dto.CouponTemplateQuery{Name: "拿铁"},
			wantSQL:  "1=1 AND name ILIKE '%' || $1 || '%'",
			wantArgs: []any{"拿铁"},
		},
		{
			name:     "只按状态筛",
			q:        dto.CouponTemplateQuery{Status: "active"},
			wantSQL:  "1=1 AND status=$1",
			wantArgs: []any{"active"},
		},
		{
			name:     "三个都筛时编号依次是 1/2/3",
			q:        dto.CouponTemplateQuery{Name: "拿铁", Status: "active", AuditStatus: "approved"},
			wantSQL:  "1=1 AND name ILIKE '%' || $1 || '%' AND status=$2 AND audit_status=$3",
			wantArgs: []any{"拿铁", "active", "approved"},
		},
		{
			name:     "空串不等于筛空字符串，是不筛",
			q:        dto.CouponTemplateQuery{Name: "", Status: "draft"},
			wantSQL:  "1=1 AND status=$1",
			wantArgs: []any{"draft"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			sql, args := templateListWhere(c.q)
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
			// 占位符个数必须与参数个数一致，否则 pgx 在 Bind 阶段才会报错。
			if got := maxPlaceholder(sql); got != len(args) {
				t.Errorf("statement references $%d but %d args are bound", got, len(args))
			}
		})
	}
}
