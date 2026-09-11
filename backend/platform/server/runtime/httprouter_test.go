package runtime

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-kratos/kratos/v2/errors"
	"github.com/go-kratos/kratos/v2/middleware"
	"github.com/go-kratos/kratos/v2/transport"
	khttp "github.com/go-kratos/kratos/v2/transport/http"
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
