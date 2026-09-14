package handler

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/panda-dev/panda-v2/backend/platform/api"
	"github.com/panda-dev/panda-v2/backend/services/user-service/internal/model"
	"github.com/panda-dev/panda-v2/backend/services/user-service/internal/repository"
	"github.com/panda-dev/panda-v2/backend/services/user-service/internal/service"
)

type operationLogRepoStub struct {
	repository.AdminOperationLogRepository
	logs []*model.AdminOperationLog
	// lastFilter 记下 handler 搬运过来的筛选条件，用来验「查询串里的参数真的到了仓库」。
	lastFilter repository.OperationLogFilter
	total      int64
	facets     *repository.OperationLogFacets
	err        error
}

func (s *operationLogRepoStub) FindPage(
	_ context.Context, filter repository.OperationLogFilter, _, _ int,
) ([]*model.AdminOperationLog, error) {
	s.lastFilter = filter
	if s.err != nil {
		return nil, s.err
	}
	return s.logs, nil
}

func (s *operationLogRepoStub) Count(context.Context, repository.OperationLogFilter) (int64, error) {
	if s.err != nil {
		return 0, s.err
	}
	return s.total, nil
}

func (s *operationLogRepoStub) ListFacets(context.Context) (*repository.OperationLogFacets, error) {
	if s.err != nil {
		return nil, s.err
	}
	return s.facets, nil
}

func newOperationLogHandler(stub *operationLogRepoStub) *AdminOperationLogHandler {
	return NewAdminOperationLogHandler(service.NewAdminOperationLogService(stub))
}

// doOperationLogRequest 发一个 GET 并把响应解成统一信封。target 带查询串，
// 因为筛选条件本身就是被测对象。
func doOperationLogRequest(
	t *testing.T, h *AdminOperationLogHandler, target string,
) (*httptest.ResponseRecorder, api.Response) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, target, nil)
	rec := httptest.NewRecorder()
	if strings.HasSuffix(target, "/facets") {
		h.Facets(rec, req)
	} else {
		h.List(rec, req)
	}
	var resp api.Response
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	return rec, resp
}

func TestOperationLogListReturnsSnapshotAndPaging(t *testing.T) {
	occurredAt := time.Date(2026, 9, 13, 4, 11, 43, 0, time.UTC)
	stub := &operationLogRepoStub{
		logs: []*model.AdminOperationLog{{
			ID: "log-1", AdminUsername: "admin", AdminName: "超级管理员",
			Module: "miniapp_users", Action: "update_status", Operation: "修改小程序用户状态",
			TargetType: "miniapp_user", TargetID: "584a640d-00ae-4777-abfa-b644ae2fdf3e",
			TargetName: "咖啡用户", Result: model.OperationResultSuccess,
			BeforeData: json.RawMessage(`{"status":"active","phone":"139******12"}`),
			AfterData:  json.RawMessage(`{"status":"disabled","phone":"139******12"}`),
			OccurredAt: occurredAt,
		}},
		total: 137,
	}
	_, resp := doOperationLogRequest(t, newOperationLogHandler(stub), "/v1/admin/operation-logs?page=1&pageSize=20")
	if !resp.Success {
		t.Fatalf("expected success, got %+v", resp)
	}
	page, ok := resp.Data.(map[string]any)
	if !ok {
		t.Fatalf("data is %T, want page object", resp.Data)
	}
	// total 必须是满足条件的总数，不是本页条数——给了本页条数分页器就只剩一页。
	if page["total"] != float64(137) || page["pageSize"] != float64(20) {
		t.Fatalf("paging不匹配: %+v", page)
	}
	items, ok := page["items"].([]any)
	if !ok || len(items) != 1 {
		t.Fatalf("items = %#v, want 1 条", page["items"])
	}
	item := items[0].(map[string]any)
	// 快照以原始 JSON 下发，界面上才能按字段展开；被摊平成字符串的话前端只能整段显示。
	before, ok := item["beforeData"].(map[string]any)
	if !ok || before["status"] != "active" {
		t.Fatalf("beforeData = %#v, want 原始对象", item["beforeData"])
	}
	if item["occurredAt"] != occurredAt.Format(time.RFC3339) {
		t.Fatalf("occurredAt = %v, want %v", item["occurredAt"], occurredAt.Format(time.RFC3339))
	}
}

// 设备详情页的「操作日志」那一屏就是这两个参数的用户：它只发 targetType=device 与
// 这台设备的 id。参数名写错（比如写成 target_id）不会报错，只会安静地不过滤——
// 界面上会是「这台设备的操作记录」里混着别的设备的操作，比空表更难发现。
func TestOperationLogListPlumbsTargetFilter(t *testing.T) {
	const targetID = "2f8a1f1e-0f4a-4a5e-9c3b-1d2e3f4a5b6c"
	stub := &operationLogRepoStub{}
	rec, resp := doOperationLogRequest(t, newOperationLogHandler(stub),
		"/v1/admin/operation-logs?targetType=device&targetId="+targetID)
	if rec.Code != http.StatusOK || !resp.Success {
		t.Fatalf("status = %d, resp = %+v, want 200", rec.Code, resp)
	}
	if stub.lastFilter.TargetType != "device" || stub.lastFilter.TargetID != targetID {
		t.Fatalf("筛选条件 = %+v，want device / %s", stub.lastFilter, targetID)
	}
}

// 没有快照时下发 null，而不是空对象：空对象在界面上会渲染成一个空表格，
// 看起来像「快照丢了」；null 才能让前端显示「无」。
func TestOperationLogListKeepsMissingSnapshotAsNull(t *testing.T) {
	stub := &operationLogRepoStub{logs: []*model.AdminOperationLog{{ID: "log-2", Result: "success"}}}
	_, resp := doOperationLogRequest(t, newOperationLogHandler(stub), "/v1/admin/operation-logs")
	page := resp.Data.(map[string]any)
	item := page["items"].([]any)[0].(map[string]any)
	if item["beforeData"] != nil || item["afterData"] != nil {
		t.Fatalf("空快照应为 null, got %#v / %#v", item["beforeData"], item["afterData"])
	}
}

func TestOperationLogListBadFiltersReturn400(t *testing.T) {
	cases := []struct {
		name   string
		query  string
		expect string
	}{
		{"结果取值不在枚举内", "?result=ok", service.ErrOperationLogResultInvalid.Error()},
		{"时间不是 RFC3339", "?startTime=2026-09-13", service.ErrOperationLogTimeInvalid.Error()},
		{"起止颠倒", "?startTime=2026-09-13T12:00:00Z&endTime=2026-09-13T11:00:00Z", service.ErrOperationLogTimeRangeInvalid.Error()},
		// 设备详情页的日志 tab 全靠这两个参数；打错的 id 必须是 400，不能是一张空表。
		{"目标 id 不是 UUID", "?targetId=devic-1", service.ErrOperationLogTargetIDInvalid.Error()},
		{
			"筛选值超长",
			"?keyword=" + strings.Repeat("a", 65),
			service.ErrOperationLogFilterTooLong.Error(),
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec, resp := doOperationLogRequest(t, newOperationLogHandler(&operationLogRepoStub{}), "/v1/admin/operation-logs"+tc.query)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400", rec.Code)
			}
			if resp.Success || resp.ErrorMessage != tc.expect {
				t.Fatalf("resp = %+v, want 400 %q", resp, tc.expect)
			}
		})
	}
}

func TestOperationLogListRepositoryFailureIs500WithoutLeakingDetail(t *testing.T) {
	stub := &operationLogRepoStub{err: errors.New(`pq: relation "admin_operation_logs" does not exist`)}
	rec, resp := doOperationLogRequest(t, newOperationLogHandler(stub), "/v1/admin/operation-logs")
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
	if resp.Success || resp.ErrorCode != api.CodeInternal {
		t.Fatalf("resp = %+v, want %s", resp, api.CodeInternal)
	}
	if strings.Contains(resp.ErrorMessage, "admin_operation_logs") {
		t.Fatalf("错误信息泄露了库内细节: %q", resp.ErrorMessage)
	}
}

// pageSize 超上限由 api.ParsePage 拒绝，不该走到 service。
func TestOperationLogListRejectsPageSizeOverLimit(t *testing.T) {
	rec, resp := doOperationLogRequest(t, newOperationLogHandler(&operationLogRepoStub{}), "/v1/admin/operation-logs?pageSize=1000")
	if rec.Code != http.StatusBadRequest || resp.Success {
		t.Fatalf("status = %d, resp = %+v, want 400", rec.Code, resp)
	}
}

func TestOperationLogFacetsReturnsVocabulary(t *testing.T) {
	stub := &operationLogRepoStub{facets: &repository.OperationLogFacets{
		Modules: []string{"miniapp_users", "roles"},
		Actions: []string{"create", "update_status"},
	}}
	_, resp := doOperationLogRequest(t, newOperationLogHandler(stub), "/v1/admin/operation-logs/facets")
	if !resp.Success {
		t.Fatalf("expected success, got %+v", resp)
	}
	data := resp.Data.(map[string]any)
	if len(data["modules"].([]any)) != 2 || len(data["actions"].([]any)) != 2 {
		t.Fatalf("facets = %+v", data)
	}
}

func TestOperationLogFacetsRepositoryFailureIs500(t *testing.T) {
	rec, resp := doOperationLogRequest(t,
		newOperationLogHandler(&operationLogRepoStub{err: errors.New("boom")}), "/v1/admin/operation-logs/facets")
	if rec.Code != http.StatusInternalServerError || resp.Success {
		t.Fatalf("status = %d, resp = %+v, want 500", rec.Code, resp)
	}
}
