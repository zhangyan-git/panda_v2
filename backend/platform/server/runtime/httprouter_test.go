package runtime

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-kratos/kratos/v2/errors"
	"github.com/go-kratos/kratos/v2/middleware"
	"github.com/go-kratos/kratos/v2/middleware/recovery"
	"github.com/go-kratos/kratos/v2/transport"
	khttp "github.com/go-kratos/kratos/v2/transport/http"
	"github.com/panda-dev/panda-v2/backend/platform/api"
	"google.golang.org/grpc/codes"
)

// recorderMiddleware notes the operation the transport reports, which is only
// reachable once kratos has installed the transport on the request context.
// That is exactly what the tracing and metrics middlewares depend on.
func recorderMiddleware(seen *string, observed *error) middleware.Middleware {
	return func(next middleware.Handler) middleware.Handler {
		return func(ctx context.Context, req any) (any, error) {
			if tr, ok := transport.FromServerContext(ctx); ok {
				*seen = tr.Operation()
			}
			reply, err := next(ctx, req)
			*observed = err
			return reply, err
		}
	}
}

// TestHTTPRouterInstrumentsRegisteredRoutes is the regression test for the reason
// HTTPRouter exists: khttp.Middleware alone never reaches a plain HandleFunc
// route, because kratos only consults that matcher from generated handlers.
func TestHTTPRouterInstrumentsRegisteredRoutes(t *testing.T) {
	var seen string
	var observed error
	srv := khttp.NewServer()
	routes := NewHTTPRouter(srv, recorderMiddleware(&seen, &observed))
	routes.HandleFunc("/v1/thing", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })

	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/thing", nil))

	if rec.Code != http.StatusNoContent {
		t.Fatalf("status=%d want %d", rec.Code, http.StatusNoContent)
	}
	if seen != "/v1/thing" {
		t.Fatalf("middleware operation=%q want %q (middleware did not run, or ran without the transport)", seen, "/v1/thing")
	}
	if observed != nil {
		t.Fatalf("middleware saw error %v for a 204 response", observed)
	}
}

// TestHTTPRouterRoutesBeyondHandleFuncAreStillRegistered keeps the embedding
// honest: the wrapper must not swallow the server's other registration methods.
func TestHTTPRouterRoutesBeyondHandleFuncAreStillRegistered(t *testing.T) {
	var seen string
	var observed error
	srv := khttp.NewServer()
	routes := NewHTTPRouter(srv, recorderMiddleware(&seen, &observed))
	routes.Handle("/v1/raw", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }))

	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/raw", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d want %d", rec.Code, http.StatusOK)
	}
}

// TestHTTPRouterReportsHandlerStatusToTheChain covers the status reconstruction:
// a plain http.HandlerFunc returns no error, so without it every response would
// be counted as a success.
func TestHTTPRouterReportsHandlerStatusToTheChain(t *testing.T) {
	for _, tt := range []struct {
		status int
		want   codes.Code
	}{{http.StatusForbidden, codes.PermissionDenied}, {http.StatusInternalServerError, codes.Internal}} {
		var seen string
		var observed error
		srv := khttp.NewServer()
		routes := NewHTTPRouter(srv, recorderMiddleware(&seen, &observed))
		routes.HandleFunc("/v1/denied", func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, http.StatusText(tt.status), tt.status)
		})

		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/denied", nil))

		if rec.Code != tt.status {
			t.Fatalf("status=%d want %d", rec.Code, tt.status)
		}
		kratosErr := errors.FromError(observed)
		if kratosErr == nil {
			t.Fatalf("status %d reached the chain as a success", tt.status)
		}
		if kratosErr.Code != int32(tt.want) {
			t.Fatalf("status=%d reported code=%d want %d", tt.status, kratosErr.Code, tt.want)
		}
	}
}

// TestHTTPRouterDoesNotLeakTheReconstructedError asserts the error used to carry
// the status to the chain never reaches the client: the handler already wrote the
// response, so a second write would corrupt it.
func TestHTTPRouterDoesNotLeakTheReconstructedError(t *testing.T) {
	srv := khttp.NewServer()
	routes := NewHTTPRouter(srv)
	routes.HandleFunc("/v1/denied", func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "denied", http.StatusForbidden)
	})

	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/denied", nil))

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status=%d want %d", rec.Code, http.StatusForbidden)
	}
	if body := rec.Body.String(); body != "denied\n" {
		t.Fatalf("body=%q want %q", body, "denied\n")
	}
}

// panic 必须落成一个 500，这是 ErrHandlerPanic 存在的全部理由。
//
// recovery 中间件把 panic 收下、把错误交回来之后，**没有人会再写响应**——net/http 见到
// 「处理器正常返回」就发一个空 200。调用方（后台页面、网关、另一个服务）看到的是成功，
// 而实际上什么都没做：这是这个仓库里最不能接受的一种错。
func TestHTTPRouterTurnsAPanicIntoAServerError(t *testing.T) {
	srv := khttp.NewServer()
	routes := NewHTTPRouter(srv, recovery.Recovery(recovery.WithHandler(func(context.Context, any, any) error {
		return ErrHandlerPanic
	})))
	routes.HandleFunc("/v1/boom", func(http.ResponseWriter, *http.Request) { panic("boom") })

	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/boom", nil))

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status=%d want %d（panic 不能变成空 200）", rec.Code, http.StatusInternalServerError)
	}
	var body api.Response
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode %q: %v", rec.Body.String(), err)
	}
	if body.Success || body.ErrorCode != api.CodeInternal {
		t.Fatalf("body=%+v want success=false errorCode=%s", body, api.CodeInternal)
	}
}

// 已经写过响应头的处理器中途 panic：这时候再补一个 500 只会把响应写坏（状态码已经发出去了，
// 多出来的一段 JSON 会跟在半截正文后面）。这条守的是那个「不二次写」的条件。
func TestHTTPRouterDoesNotRewriteAResponseThatAlreadyStarted(t *testing.T) {
	srv := khttp.NewServer()
	routes := NewHTTPRouter(srv, recovery.Recovery(recovery.WithHandler(func(context.Context, any, any) error {
		return ErrHandlerPanic
	})))
	routes.HandleFunc("/v1/partial", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("half"))
		panic("boom")
	})

	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/partial", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d want %d（已经发出去的状态码改不掉）", rec.Code, http.StatusOK)
	}
	if body := rec.Body.String(); body != "half" {
		t.Fatalf("body=%q want %q（不能往半截响应后面再补一段）", body, "half")
	}
}

// 其它错误值不该被当成 panic：这条链上除了 recovery 还有别的中间件，而状态重建那条路
// 也会交回一个错误（400 以上）。认错了就会把一次正常的 404 变成 500。
func TestHTTPRouterDoesNotMistakeOtherErrorsForAPanic(t *testing.T) {
	srv := khttp.NewServer()
	routes := NewHTTPRouter(srv, recovery.Recovery())
	routes.HandleFunc("/v1/missing", func(w http.ResponseWriter, _ *http.Request) {
		api.Error(w, http.StatusNotFound, api.CodeNotFound, "nope")
	})

	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/missing", nil))

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status=%d want %d", rec.Code, http.StatusNotFound)
	}
	var body api.Response
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.ErrorCode != api.CodeNotFound {
		t.Fatalf("errorCode=%q want %q", body.ErrorCode, api.CodeNotFound)
	}
}
