package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestResponseSuccessAndError(t *testing.T) {
	for _, test := range []struct {
		name        string
		write       func(http.ResponseWriter)
		httpStatus  int
		wantSuccess bool
		wantCode    string
		dataPresent bool
	}{
		{
			"success",
			func(w http.ResponseWriter) { Success(w, map[string]bool{"ok": true}) },
			http.StatusOK, true, "", true,
		},
		{
			"error",
			func(w http.ResponseWriter) { Error(w, http.StatusBadRequest, CodeInvalidRequest, "bad") },
			http.StatusBadRequest, false, CodeInvalidRequest, false,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			test.write(w)
			if w.Code != test.httpStatus {
				t.Fatalf("http status: got %d, want %d", w.Code, test.httpStatus)
			}
			var got Response
			if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
				t.Fatal(err)
			}
			if got.Success != test.wantSuccess {
				t.Errorf("success: got %v, want %v", got.Success, test.wantSuccess)
			}
			if test.wantCode != "" && got.ErrorCode != test.wantCode {
				t.Errorf("errorCode: got %q, want %q", got.ErrorCode, test.wantCode)
			}
			if test.dataPresent && got.Data == nil {
				t.Error("expected data to be present")
			}
			if !test.dataPresent && got.Data != nil {
				t.Errorf("expected no data, got %v", got.Data)
			}
		})
	}
}

func TestWriteNoContentHasEmptyBody(t *testing.T) {
	w := httptest.NewRecorder()
	WriteNoContent(w)
	if w.Code != http.StatusNoContent || w.Body.Len() != 0 {
		t.Fatalf("status=%d body=%q", w.Code, w.Body.String())
	}
}

func TestPageResponseShape(t *testing.T) {
	// 分页包裹的字段名是前后端契约：items/total/page/pageSize。
	// 前端 ProTable 的 request 按这三个名字取值，改名不会报错，页面只是空白，
	// 所以这里把键名钉成字面量。
	w := httptest.NewRecorder()
	Success(w, PageResponse{Items: []string{"a", "b"}, Total: 7, Page: 2, PageSize: 3})
	if w.Code != http.StatusOK {
		t.Fatalf("http status: got %d", w.Code)
	}
	var got struct {
		Success bool `json:"success"`
		Data    struct {
			Items    []string `json:"items"`
			Total    int64    `json:"total"`
			Page     int      `json:"page"`
			PageSize int      `json:"pageSize"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if !got.Success {
		t.Error("expected success=true")
	}
	if len(got.Data.Items) != 2 || got.Data.Total != 7 || got.Data.Page != 2 || got.Data.PageSize != 3 {
		t.Fatalf("page payload: %+v", got.Data)
	}
	// 只认 camelCase：出现 page_size 就说明有人又加回 snake_case 的 tag。
	if strings.Contains(w.Body.String(), "page_size") {
		t.Fatalf("分页响应里出现了 snake_case 键: %s", w.Body.String())
	}
}

func TestParsePage(t *testing.T) {
	for _, test := range []struct {
		name                   string
		rawPage, rawSize       string
		wantPage, wantPageSize int
		wantOK                 bool
	}{
		{"缺省", "", "", 1, DefaultPageSize, true},
		{"显式", "3", "50", 3, 50, true},
		{"pageSize 到上限", "", "200", 1, 200, true},
		{"page=0", "0", "", 0, 0, false},
		{"page 非数字", "abc", "", 0, 0, false},
		{"pageSize=0", "", "0", 0, 0, false},
		{"pageSize 超上限", "", "201", 0, 0, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			page, size, ok, msg := ParsePage(test.rawPage, test.rawSize, MaxPageSize)
			if ok != test.wantOK {
				t.Fatalf("ok: got %v (%s), want %v", ok, msg, test.wantOK)
			}
			if ok && (page != test.wantPage || size != test.wantPageSize) {
				t.Fatalf("got page=%d pageSize=%d, want %d/%d", page, size, test.wantPage, test.wantPageSize)
			}
			if !ok && msg == "" {
				t.Fatal("拒绝时必须给出可回给调用方的说明")
			}
		})
	}
}

func TestPageOffset(t *testing.T) {
	// 第一页必须落在 OFFSET 0。写成 (page-1)*pageSize 时少个 -1 就会变成
	// OFFSET pageSize，症状是第一页凭空少一整页数据且不报错——
	// 调用方只会觉得「刚建的记录不见了」。所以第一页单独钉一条。
	for _, test := range []struct {
		page, pageSize, want int
	}{
		{1, 20, 0},
		{2, 20, 20},
		{3, 50, 100},
		{1, 1, 0},
		{4, 1, 3},
	} {
		if got := PageOffset(test.page, test.pageSize); got != test.want {
			t.Errorf("PageOffset(%d, %d) = %d, want %d", test.page, test.pageSize, got, test.want)
		}
	}
}
