package service

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/panda-dev/panda-v2/backend/services/user-service/internal/model"
	"github.com/panda-dev/panda-v2/backend/services/user-service/internal/repository"
)

// fakeOperationLogRepo 记录最后一次查询收到的筛选条件，好断言「service 把参数
// 翻译成了什么」——这个翻译（空串表示不限、时间解析、取值校验）正是本层存在的理由。
type fakeOperationLogRepo struct {
	repository.AdminOperationLogRepository
	lastFilter repository.OperationLogFilter
	lastLimit  int
	lastOffset int
	logs       []*model.AdminOperationLog
	total      int64
	err        error
	facets     *repository.OperationLogFacets
}

func (f *fakeOperationLogRepo) FindPage(
	_ context.Context, filter repository.OperationLogFilter, limit, offset int,
) ([]*model.AdminOperationLog, error) {
	f.lastFilter, f.lastLimit, f.lastOffset = filter, limit, offset
	if f.err != nil {
		return nil, f.err
	}
	return f.logs, nil
}

func (f *fakeOperationLogRepo) Count(context.Context, repository.OperationLogFilter) (int64, error) {
	if f.err != nil {
		return 0, f.err
	}
	return f.total, nil
}

func (f *fakeOperationLogRepo) ListFacets(context.Context) (*repository.OperationLogFacets, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.facets, nil
}

func TestBuildOperationLogFilterRejectsBadInput(t *testing.T) {
	cases := []struct {
		name  string
		query OperationLogQuery
		want  error
	}{
		{"结果取值不在枚举内", OperationLogQuery{Result: "ok"}, ErrOperationLogResultInvalid},
		{"时间不是 RFC3339", OperationLogQuery{StartTime: "2026-09-13"}, ErrOperationLogTimeInvalid},
		{"结束时间不是 RFC3339", OperationLogQuery{EndTime: "昨天"}, ErrOperationLogTimeInvalid},
		{
			"起止颠倒",
			OperationLogQuery{StartTime: "2026-09-13T12:00:00Z", EndTime: "2026-09-13T11:00:00Z"},
			ErrOperationLogTimeRangeInvalid,
		},
		{
			"关键词超长",
			OperationLogQuery{Keyword: strings.Repeat("字", maxOperationLogFilterRunes+1)},
			ErrOperationLogFilterTooLong,
		},
		{
			"操作人超长",
			OperationLogQuery{Operator: strings.Repeat("a", maxOperationLogFilterRunes+1)},
			ErrOperationLogFilterTooLong,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := buildOperationLogFilter(tc.query); !errors.Is(err, tc.want) {
				t.Fatalf("buildOperationLogFilter(%+v) err = %v, want %v", tc.query, err, tc.want)
			}
		})
	}
}

func TestBuildOperationLogFilterNormalizesInput(t *testing.T) {
	start := time.Date(2026, 9, 13, 0, 0, 0, 0, time.UTC)
	end := start.Add(24 * time.Hour)
	got, err := buildOperationLogFilter(OperationLogQuery{
		Module:    "  miniapp_users ",
		Action:    "update_status",
		Result:    model.OperationResultFailure,
		Operator:  " admin ",
		Keyword:   " 咖啡 ",
		StartTime: start.Format(time.RFC3339),
		EndTime:   end.Format(time.RFC3339),
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// 前后空白要去掉：从查询串里原样搬过来的空格会变成 LIKE 的一部分，
	// 「 admin」匹配不到任何用户名，而界面看起来只是搜不到人。
	if got.Module != "miniapp_users" || got.Operator != "admin" || got.Keyword != "咖啡" {
		t.Fatalf("filter not trimmed: %+v", got)
	}
	if got.StartTime == nil || !got.StartTime.Equal(start) {
		t.Fatalf("StartTime = %v, want %v", got.StartTime, start)
	}
	if got.EndTime == nil || !got.EndTime.Equal(end) {
		t.Fatalf("EndTime = %v, want %v", got.EndTime, end)
	}
}

// 空查询必须翻译成零值筛选，而不是「匹配空串」——后者会让默认打开列表时一条都查不到。
func TestBuildOperationLogFilterEmptyQueryMeansUnfiltered(t *testing.T) {
	got, err := buildOperationLogFilter(OperationLogQuery{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.StartTime != nil || got.EndTime != nil {
		t.Fatalf("空时间应为 nil（不限），得到 %+v", got)
	}
	if got.Module != "" || got.Action != "" || got.Result != "" || got.Operator != "" || got.Keyword != "" {
		t.Fatalf("空查询应保持零值，得到 %+v", got)
	}
}

func TestListPassesFilterAndPaginationToRepository(t *testing.T) {
	repo := &fakeOperationLogRepo{
		logs:  []*model.AdminOperationLog{{ID: "log-1", Module: "miniapp_users"}},
		total: 42,
	}
	svc := NewAdminOperationLogService(repo)
	logs, total, err := svc.List(context.Background(), OperationLogQuery{Module: "miniapp_users"}, 3, 20)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if total != 42 || len(logs) != 1 {
		t.Fatalf("logs = %d, total = %d, want 1 / 42", len(logs), total)
	}
	// 第 3 页 20 条 → 偏移 40。偏移算错时第 1 页正常、翻页才开始重复或跳行，
	// 是那种最晚才被发现的错。
	if repo.lastOffset != 40 || repo.lastLimit != 20 {
		t.Fatalf("offset/limit = %d/%d, want 40/20", repo.lastOffset, repo.lastLimit)
	}
	if repo.lastFilter.Module != "miniapp_users" {
		t.Fatalf("筛选条件没传到仓库: %+v", repo.lastFilter)
	}
}

// 仓库出错时必须原样上抛，不能被当成「没有日志」——空列表和「库连不上」在界面上
// 长得一模一样，吞掉错误等于让运维去排查一个不存在的空结果。
func TestListPropagatesRepositoryError(t *testing.T) {
	boom := errors.New("connection refused")
	svc := NewAdminOperationLogService(&fakeOperationLogRepo{err: boom})
	if _, _, err := svc.List(context.Background(), OperationLogQuery{}, 1, 20); !errors.Is(err, boom) {
		t.Fatalf("err = %v, want %v", err, boom)
	}
}

func TestFacetsPropagatesRepositoryError(t *testing.T) {
	boom := errors.New("connection refused")
	svc := NewAdminOperationLogService(&fakeOperationLogRepo{err: boom})
	if _, err := svc.Facets(context.Background()); !errors.Is(err, boom) {
		t.Fatalf("err = %v, want %v", err, boom)
	}
}
