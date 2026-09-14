package repository

import (
	"strconv"
	"strings"
)

// whereClause 把 buildFilter 的条件拼成 WHERE 子句；没有条件时返回空串
// （而不是 " WHERE 1=1"），好让调用方能直接把结果接在 FROM 之后。
func whereClause(conds []string) string {
	if len(conds) == 0 {
		return ""
	}
	return ` WHERE ` + strings.Join(conds, " AND ")
}

func buildFilter(merchantID, name, status, auditStatus, alias string) ([]string, []any) {
	conds := make([]string, 0, 4)
	args := make([]any, 0, 4)
	if merchantID != "" {
		args = append(args, merchantID)
		conds = append(conds, alias+"merchant_id = $"+strconv.Itoa(len(args)))
	}
	if name != "" {
		args = append(args, "%"+name+"%")
		conds = append(conds, alias+"name ILIKE $"+strconv.Itoa(len(args)))
	}
	if status != "" {
		args = append(args, status)
		conds = append(conds, alias+"status = $"+strconv.Itoa(len(args)))
	}
	if auditStatus != "" {
		args = append(args, auditStatus)
		conds = append(conds, alias+"audit_status = $"+strconv.Itoa(len(args)))
	}
	return conds, args
}
