package main

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/panda-dev/panda-v2/backend/platform/health"
)

// countingHandler 记录被调用的路径，用来断言「探针没有进代理」。
type countingHandler struct{ paths []string }

func (h *countingHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.paths = append(h.paths, r.URL.Path)
	w.WriteHeader(http.StatusTeapot)
}

func newTestMux(state *health.State, throttle *throttle) (*http.ServeMux, *countingHandler) {
	proxied := &countingHandler{}
	return newMux(state, throttle, proxied), proxied
}

func readyState() *health.State {
	s := health.New()
	s.SetReady(true)
	return s
}

func TestProbesReportHealth(t *testing.T) {
	state := readyState()
	mux, _ := newTestMux(state, &throttle{})

	for _, path := range []string{"/livez", "/readyz"} {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != http.StatusOK {
			t.Errorf("%s = %d, want 200", path, rec.Code)
		}
		if rec.Body.String() != "ok" {
			t.Errorf("%s body = %q, want ok", path, rec.Body.String())
		}
	}
}

// 开始停机后 /readyz 必须立刻变 503——LB 就是靠这个把副本摘掉的。
// /livez 不跟着翻转：进程还活着，让编排层重启它才是错的。
func TestReadyzTurnsNotReadyWhileLivezStaysUp(t *testing.T) {
	state := readyState()
	mux, _ := newTestMux(state, &throttle{})

	state.SetReady(false)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("/readyz = %d, want 503 after readiness is withdrawn", rec.Code)
	}

	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/livez", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("/livez = %d, want 200: 进程还活着，重启它不是正确的反应", rec.Code)
	}
}

// 探针绝不能进代理：这两个路径没有对应的上游，转发过去只会变成 404，
// 而 LB 分不清「探针 404」和「网关坏了」。
func TestProbesAreNotProxied(t *testing.T) {
	mux, proxied := newTestMux(readyState(), &throttle{})

	for _, path := range []string{"/livez", "/readyz"} {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	}
	if len(proxied.paths) != 0 {
		t.Fatalf("proxy saw %v, want nothing: 探针必须被 mux 自己处理", proxied.paths)
	}
}

// 探针不能走限流。LB 的探测频率远高于用户请求，被 429 之后 LB 会把健康的
// 副本判成故障摘掉——这是「限流把系统打挂」最典型的一条路径。
//
// 用一个「什么都拒绝」的外层来断言结构：探针必须是挂在限流之外处理的，
// 而不是靠额度足够大才侥幸通过。
func TestProbesBypassTheThrottle(t *testing.T) {
	// 把两条链都换成一个恒拒的中间件，模拟额度耗尽。wrap 会预先建好两条
	// （每个请求只是选中其中一条），所以两条都得给。
	reject := func(http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusTooManyRequests)
		})
	}
	deny := &throttle{enabled: true, defaultMW: reject, authMW: reject}
	mux, _ := newTestMux(readyState(), deny)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("/readyz = %d, want 200 even when the throttle rejects everything", rec.Code)
	}

	// 同一时刻普通路由确实被拒——否则上面那条断言什么都没证明。
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/admin/roles", nil))
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("proxied route = %d, want the throttle's 429", rec.Code)
	}
}
