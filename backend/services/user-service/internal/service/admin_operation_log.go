package service

import (
	"context"
	"errors"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/panda-dev/panda-v2/backend/platform/api"
	"github.com/panda-dev/panda-v2/backend/services/user-service/internal/model"
	"github.com/panda-dev/panda-v2/backend/services/user-service/internal/repository"
)

// 后台日志查询的入参错误，handler 据此回 400。
var (
	// ErrOperationLogResultInvalid 结果筛选不是合法取值。
	ErrOperationLogResultInvalid = errors.New("result 只能是 success 或 failure")
	// ErrOperationLogTimeInvalid 时间参数不是 RFC3339 格式。
	//
	// 特意不接受「随便什么能解析的日期串」：这个接口的调用方只有后台页面，它传来的
	// 就是 ISO 字符串。放宽解析规则只会让「2026-09-13」这种没带时区的值按服务器
	// 本地时区解释，而库里存的是 UTC——差的那几个小时没人会想到去查。
	ErrOperationLogTimeInvalid = errors.New("时间格式不正确，需要 RFC3339（如 2026-09-13T12:00:00Z）")
	// ErrOperationLogTimeRangeInvalid 起止时间颠倒。
	ErrOperationLogTimeRangeInvalid = errors.New("开始时间不能晚于结束时间")
	// ErrOperationLogFilterTooLong 筛选值过长。
	ErrOperationLogFilterTooLong = errors.New("筛选条件过长")
)

// 单个筛选项的长度上限。这些值全都会被拼成匹配模式发到库里，而合法的取值（模块名、
// 动作名、管理员登录名）都在 32 字以内。
const maxOperationLogFilterRunes = 64

// OperationLogQuery 是后台日志列表的查询条件，字段与 HTTP 查询参数一一对应。
//
// 时间用字符串而不是 time.Time：格式校验是「这个请求合不合法」的一部分，得由本层
// 报错、由 handler 回 400，用 time.Time 的话解析失败只会在 handler 里退化成零值，
// 而那正是「不限时间」——一个写错的日期会静默变成查全部。
type OperationLogQuery struct {
	Module   string
	Action   string
	Result   string
	Operator string
	Keyword  string
	// StartTime / EndTime RFC3339，空串表示不限；闭区间。
	StartTime string
	EndTime   string
}

// AdminOperationLogService 是后台对操作日志的只读查询。
//
// 与 OperationLogService 分开：那个是审计事件的消费者（写），这个是后台页面的查询
// 入口（读）。两者共用一张表和同一个仓库，但「怎么把消息落库」和「怎么让人查得到」
// 是两件事，混在一个类型里只会让写入侧的测试不得不先构造一个查询条件。
type AdminOperationLogService struct {
	logs repository.AdminOperationLogRepository
}

func NewAdminOperationLogService(logs repository.AdminOperationLogRepository) *AdminOperationLogService {
	return &AdminOperationLogService{logs: logs}
}

// List 返回一页日志及其总数。
//
// 校验放在这里而不是 handler：这些规则（哪些取值合法、时间怎么解析）是查询语义的
// 一部分，将来后台之外复用同一套时不用再抄一遍。
func (s *AdminOperationLogService) List(
	ctx context.Context, q OperationLogQuery, page, pageSize int,
) ([]*model.AdminOperationLog, int64, error) {
	filter, err := buildOperationLogFilter(q)
	if err != nil {
		return nil, 0, err
	}
	logs, err := s.logs.FindPage(ctx, filter, pageSize, api.PageOffset(page, pageSize))
	if err != nil {
		return nil, 0, err
	}
	total, err := s.logs.Count(ctx, filter)
	if err != nil {
		return nil, 0, err
	}
	return logs, total, nil
}

// Facets 返回模块与动作的可选值，供筛选下拉使用。
func (s *AdminOperationLogService) Facets(ctx context.Context) (*repository.OperationLogFacets, error) {
	return s.logs.ListFacets(ctx)
}

func buildOperationLogFilter(q OperationLogQuery) (repository.OperationLogFilter, error) {
	filter := repository.OperationLogFilter{
		Module:   strings.TrimSpace(q.Module),
		Action:   strings.TrimSpace(q.Action),
		Operator: strings.TrimSpace(q.Operator),
		Keyword:  strings.TrimSpace(q.Keyword),
	}
	// 结果列的取值来自模型的常量而不是库里的 CHECK：这张表没有 CHECK 约束，
	// 所以校验只能在这里做。
	switch result := strings.TrimSpace(q.Result); result {
	case "":
	case model.OperationResultSuccess, model.OperationResultFailure:
		filter.Result = result
	default:
		return repository.OperationLogFilter{}, ErrOperationLogResultInvalid
	}
	for _, value := range []string{filter.Module, filter.Action, filter.Operator, filter.Keyword} {
		if utf8.RuneCountInString(value) > maxOperationLogFilterRunes {
			return repository.OperationLogFilter{}, ErrOperationLogFilterTooLong
		}
	}

	start, err := parseOperationLogTime(q.StartTime)
	if err != nil {
		return repository.OperationLogFilter{}, err
	}
	end, err := parseOperationLogTime(q.EndTime)
	if err != nil {
		return repository.OperationLogFilter{}, err
	}
	// 起止颠倒时明确报错，而不是回一个空列表：空列表和「这段时间真的没人操作过」
	// 长得一模一样，而前者是筛选条件写错了。
	if start != nil && end != nil && start.After(*end) {
		return repository.OperationLogFilter{}, ErrOperationLogTimeRangeInvalid
	}
	filter.StartTime, filter.EndTime = start, end
	return filter, nil
}

// parseOperationLogTime 空串表示不限，非空则必须是 RFC3339。
func parseOperationLogTime(raw string) (*time.Time, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}
	parsed, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		return nil, ErrOperationLogTimeInvalid
	}
	return &parsed, nil
}
