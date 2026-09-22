package controller

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/panda-dev/panda-v2/backend/platform/api"
)

// 这个文件是**请求侧**的解析工具：把 query string 里那几种反复出现的形状翻成 service 认得的
// 类型，翻不动时统一回 400 加一句中文。
//
// 它与 response.go 里那张人话表是两回事：那张表按**错误值**索引，管的是「服务层已经判定了
// 这是哪一条错」；这里是「这个值根本没能变成错误值」——空串、乱码、不是数字。两者混在一起
// 写过一次，结果是筛选项写错时弹出来的是服务层某条风马牛不相及的话。

// parseOptionalBool 读一个三态布尔筛选（true / false / 不筛）。
//
// 空串回 nil 而不是 false：「只看开了自动续费的」与「不看这一项」是两种筛选，合并成一个
// false 会让清空筛选变成「只看没开的」——一个用户从没要求过的结果。
func parseOptionalBool(value string) (*bool, bool) {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return nil, true
	}
	parsed, err := strconv.ParseBool(trimmed)
	if err != nil {
		return nil, false
	}
	return &parsed, true
}

// parseOptionalTime 读一个可选的 RFC3339 时间。空串回 nil（= 没筛这一端）。
//
// 只认 RFC3339：前端用 dayjs 的 toRFC3339 生成的就是这个格式，而 parse 得松一点（认
// "2026-09-15" 之类的简写）会让「按天筛」与「按时刻筛」在后端看起来是同一个东西——运营按
// 到期时间筛人时，那个差别正是「多出来一个人」的来源。
func parseOptionalTime(value string) (*time.Time, bool) {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return nil, true
	}
	parsed, err := time.Parse(time.RFC3339, trimmed)
	if err != nil {
		return nil, false
	}
	parsed = parsed.UTC()
	return &parsed, true
}

// pageParams 读 page / pageSize，不合法时已经把响应写好了（第二个返回值为 false）。
//
// 摊掉的是每个 list handler 都要写的那六行（ParsePage → 判 ok → 拼一句话 → api.Error），
// 而它们每一处都得写对同一句话。
func pageParams(w http.ResponseWriter, r *http.Request, maxPageSize int) (page, pageSize int, ok bool) {
	query := r.URL.Query()
	page, pageSize, ok, message := api.ParsePage(query.Get("page"), query.Get("pageSize"), maxPageSize)
	if !ok {
		// 用 ParsePage 给的那句话（它知道是 page 还是 pageSize 的问题），而不是自己拼一句。
		api.Error(w, http.StatusBadRequest, "INVALID_ARGUMENT", message)
		return 0, 0, false
	}
	return page, pageSize, true
}
