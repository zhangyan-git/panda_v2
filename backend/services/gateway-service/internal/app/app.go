package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"time"

	khttp "github.com/go-kratos/kratos/v2/transport/http"
	"github.com/panda-dev/panda-v2/backend/platform/auth"
	"github.com/panda-dev/panda-v2/backend/platform/config"
	"github.com/panda-dev/panda-v2/backend/platform/server"
	runtime "github.com/panda-dev/panda-v2/backend/platform/server/runtime"
)

// Run starts the gateway HTTP and gRPC servers.
func Run(service string) error {
	cfg, err := config.Load(service)
	if err != nil {
		return err
	}
	if cfg.JWTSecret == "" || cfg.JWTIssuer == "" {
		return errors.New("JWT_SECRET and JWT_ISSUER are required")
	}
	authorizer, err := auth.NewService([]byte(cfg.JWTSecret), cfg.JWTIssuer, 15*time.Minute, 7*24*time.Hour)
	if err != nil {
		return err
	}
	return server.RunWithOptions(cfg, runtime.Options{HTTPRoutes: func(s *khttp.Server) {
		s.HandlePrefix("/v1/", NewHTTPHandler(authorizer, WithAccountServiceURL(cfg.AccountServiceURL), WithUserServiceURL(cfg.UserServiceURL)))
	}})
}

type handlerOptions struct {
	accountServiceURL, userServiceURL string
	httpClient                        *http.Client
}
type HTTPHandlerOption func(*handlerOptions)

func WithAccountServiceURL(value string) HTTPHandlerOption {
	return func(o *handlerOptions) { o.accountServiceURL = strings.TrimRight(value, "/") }
}
func WithUserServiceURL(value string) HTTPHandlerOption {
	return func(o *handlerOptions) { o.userServiceURL = strings.TrimRight(value, "/") }
}
func WithHTTPClient(client *http.Client) HTTPHandlerOption {
	return func(o *handlerOptions) { o.httpClient = client }
}

// NewHTTPHandler returns the gateway HTTP entrypoint. Login remains public;
// all other routes under /v1/ require a bearer token.
func NewHTTPHandler(authorizer *auth.Service, options ...HTTPHandlerOption) http.Handler {
	settings := handlerOptions{httpClient: &http.Client{Timeout: 10 * time.Second}}
	for _, option := range options {
		option(&settings)
	}
	if settings.httpClient == nil {
		settings.httpClient = &http.Client{Timeout: 10 * time.Second}
	}
	mux := http.NewServeMux()
	mux.Handle("/v1/", auth.Bearer(authorizer)(http.HandlerFunc(notImplemented)))
	for _, path := range []string{"/v1/auth/login", "/v1/auth/refresh", "/v1/auth/revoke"} {
		mux.HandleFunc(path, accountAuthForwarder(settings.accountServiceURL, path, settings.httpClient))
	}
	mux.Handle("/v1/users/", auth.Bearer(authorizer)(userProxy(settings.userServiceURL, settings.httpClient)))
	return mux
}

func userProxy(baseURL string, client *http.Client) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodPatch {
			writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
			return
		}
		if baseURL == "" {
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "user service unavailable"})
			return
		}
		target := baseURL + r.URL.Path
		if r.URL.RawQuery != "" {
			target += "?" + r.URL.RawQuery
		}
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request body"})
			return
		}
		req, err := http.NewRequestWithContext(r.Context(), r.Method, target, bytes.NewReader(body))
		if err != nil {
			writeJSON(w, http.StatusBadGateway, map[string]string{"error": "user service unavailable"})
			return
		}
		for _, h := range []string{"Authorization", "Content-Type", "Accept"} {
			if v := r.Header.Get(h); v != "" {
				req.Header.Set(h, v)
			}
		}
		resp, err := client.Do(req)
		if err != nil {
			status, msg := http.StatusBadGateway, "user service unavailable"
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() || errors.Is(err, context.DeadlineExceeded) {
				status, msg = http.StatusGatewayTimeout, "user service timeout"
			}
			writeJSON(w, status, map[string]string{"error": msg})
			return
		}
		defer resp.Body.Close()
		for h, values := range resp.Header {
			if strings.EqualFold(h, "Connection") || strings.EqualFold(h, "Transfer-Encoding") {
				continue
			}
			for _, v := range values {
				w.Header().Add(h, v)
			}
		}
		w.WriteHeader(resp.StatusCode)
		_, _ = io.Copy(w, resp.Body)
	})
}

func accountAuthForwarder(accountServiceURL, endpoint string, client *http.Client) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
			return
		}
		if accountServiceURL == "" {
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "account service unavailable"})
			return
		}
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request body"})
			return
		}
		target := accountServiceURL
		for _, path := range []string{"/v1/auth/login", "/v1/auth/refresh", "/v1/auth/revoke"} {
			if strings.HasSuffix(target, path) {
				target = strings.TrimSuffix(target, path)
				break
			}
		}
		target += endpoint
		req, err := http.NewRequestWithContext(r.Context(), http.MethodPost, target, bytes.NewReader(body))
		if err != nil {
			writeJSON(w, http.StatusBadGateway, map[string]string{"error": "account service unavailable"})
			return
		}
		req.Header.Set("Content-Type", r.Header.Get("Content-Type"))
		if req.Header.Get("Content-Type") == "" {
			req.Header.Set("Content-Type", "application/json")
		}
		resp, err := client.Do(req)
		if err != nil {
			status, msg := http.StatusBadGateway, "account service unavailable"
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() || errors.Is(err, context.DeadlineExceeded) || errors.Is(r.Context().Err(), context.DeadlineExceeded) {
				status, msg = http.StatusGatewayTimeout, "account service timeout"
			}
			writeJSON(w, status, map[string]string{"error": msg})
			return
		}
		defer resp.Body.Close()
		for h, values := range resp.Header {
			if strings.EqualFold(h, "Connection") || strings.EqualFold(h, "Transfer-Encoding") {
				continue
			}
			for _, v := range values {
				w.Header().Add(h, v)
			}
		}
		w.WriteHeader(resp.StatusCode)
		_, _ = io.Copy(w, resp.Body)
	}
}
func notImplemented(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusNotImplemented, map[string]string{"error": "not implemented", "message": "gateway downstream proxy is not configured"})
}
func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
