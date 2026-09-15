package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/panda-dev/panda-v2/backend/platform/health"
	"github.com/panda-dev/panda-v2/backend/services/gateway-service/internal/proxy"
)

// startupTimeout 限制启动期连 Redis 的等待时间：网关必须在可预期的时间内
// 要么起来要么失败，不能卡在「正在连一个连不上的地址」上。
const startupTimeout = 5 * time.Second

// drainDelay 是「已摘掉自己」到「停止接受连接」之间的等待。
//
// 顺序不能省：LB 靠 /readyz 判断副本可用，而它的探测有间隔。如果收到信号就
// 直接 Shutdown，从进程停到 LB 发现之间会留下几秒窗口——那几秒里 LB 仍把
// 请求发过来，全部变成 502。等一个探测周期，让 LB 先自己把这一台摘掉。
// 取值要大于 LB 的健康检查间隔（常见 2–5s）。
const drainDelay = 5 * time.Second

// shutdownGrace 是优雅停机的兜底余量，加在上传超时之上：
// 正在传大文件的请求不该被滚动发布掐断，而上限又必须存在。
// 这是上限而不是等待时间——连接都空了 Shutdown 立刻返回。
const shutdownGrace = 30 * time.Second

// readHeaderTimeout 防慢速请求头（slowloris）：一个连上就不发完头的客户端
// 本来能一直占着连接。刻意不设 WriteTimeout——上传接口正常的耗时是分钟级，
// 全局写超时会先把大文件上传掐断。单请求的超时由代理按路由各自控制。
const (
	readHeaderTimeout = 10 * time.Second
	idleTimeout       = 120 * time.Second
)

func main() {
	timeout, err := durationEnv("GATEWAY_REQUEST_TIMEOUT_MS", 10*time.Second)
	if err != nil {
		log.Fatalf("gateway-service: %v", err)
	}
	uploadTimeout, err := durationEnv("GATEWAY_UPLOAD_TIMEOUT_MS", 120*time.Second)
	if err != nil {
		log.Fatalf("gateway-service: %v", err)
	}
	h, err := proxy.NewHandler(proxy.Config{
		MerchantServiceURL: requiredEnv("MERCHANT_SERVICE_URL"),
		UserServiceURL:     requiredEnv("USER_SERVICE_URL"),
		// 优惠券服务是后加的，留空时 /v1/admin/coupons 保持 404，不阻塞既有部署。
		CouponServiceURL: os.Getenv("COUPON_SERVICE_URL"),
		// 设备域同理。
		CoffeeMachineServiceURL: os.Getenv("COFFEE_MACHINE_SERVICE_URL"),
		// 订单域同理。
		OrderServiceURL: os.Getenv("ORDER_SERVICE_URL"),
		// 支付域同理。它这条路径是渠道从公网打进来的回调用，所以这个值必须是渠道能到达的
		// 地址（生产是公网域名，本地是网关自己的地址）。
		PaymentServiceURL: os.Getenv("PAYMENT_SERVICE_URL"),
		// 资产账户域（福卡）同理。
		AccountServiceURL: os.Getenv("ACCOUNT_SERVICE_URL"),
		RequestTimeout:    timeout,
		UploadTimeout:     uploadTimeout,
	})
	if err != nil {
		log.Fatalf("gateway-service: configure proxy: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), startupTimeout)
	defer cancel()
	throttle, err := newThrottle(ctx)
	if err != nil {
		log.Fatalf("gateway-service: configure rate limit: %v", err)
	}
	addr := os.Getenv("GATEWAY_ADDR")
	if addr == "" {
		addr = ":8080"
	}

	// 走到这里代理和限流都已经构造成功，可以接流量了。
	state := health.New()
	state.SetReady(true)

	srv := &http.Server{
		Addr:              addr,
		Handler:           newMux(state, throttle, h),
		ReadHeaderTimeout: readHeaderTimeout,
		IdleTimeout:       idleTimeout,
	}

	serveErr := make(chan error, 1)
	go func() {
		err := srv.ListenAndServe()
		if errors.Is(err, http.ErrServerClosed) {
			err = nil
		}
		serveErr <- err
	}()
	log.Printf("gateway-service: listening on %s", addr)

	sigCtx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	select {
	case err := <-serveErr:
		if err != nil {
			log.Fatalf("gateway-service: serve: %v", err)
		}
	case <-sigCtx.Done():
		// 恢复默认信号处理：第二次信号直接退出，不让人卡在排空里等第二轮。
		stop()
		log.Print("gateway-service: shutting down; readiness is off")
		state.SetReady(false)
		time.Sleep(drainDelay)

		shutdownCtx, cancel := context.WithTimeout(context.Background(), uploadTimeout+shutdownGrace)
		defer cancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			log.Printf("gateway-service: shutdown: %v", err)
		}
	}
}

// newMux 把探针、限流、代理接成一条链。抽出来是为了让这段接线能被测到——
// 尤其是「探针在限流之外」这条，它在真实部署里表现为「健康副本被 LB 摘掉」，
// 而不是一个 429。
func newMux(state *health.State, throttle *throttle, proxied http.Handler) *http.ServeMux {
	mux := http.NewServeMux()
	// 探针挂在最外层，既不限流也不进代理：
	//
	// 不限流——探针是 LB 自己发的，频率远高于用户请求，被 429 会被判成故障，
	// 于是健康副本被摘掉；这是「限流把系统打挂」的经典路径。
	// 不进代理——这两个路径不对应任何上游，进去只会变成 404，
	// 而探针的 404 和「上游没这个接口」的 404 在 LB 看来是一回事。
	mux.HandleFunc("/livez", func(w http.ResponseWriter, _ *http.Request) {
		writeHealth(w, state.Status().Live)
	})
	// /readyz 只反映本进程：这里不级联探测上游。级联会让上游抖动把网关自己
	// 从 LB 摘掉——上游挂了本来就该由上游的副本数去扛，网关跟着下线只会
	// 放大故障：流量连一个可以快速失败的地方都没有了。
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, _ *http.Request) {
		writeHealth(w, state.Status().Ready)
	})
	mux.Handle("/", throttle.wrap(proxied))
	return mux
}

// writeHealth 与 platform/server 的探针响应保持一致：200/ok、503/空。
func writeHealth(w http.ResponseWriter, ok bool) {
	if !ok {
		w.WriteHeader(http.StatusServiceUnavailable)
		return
	}
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok"))
}

// durationEnv 读一个以毫秒为单位的超时。空值和 0 都表示「用默认」：0 在 kratos
// 和 context 里的语义是「立刻超时」，不是「不限」，不该给它留第二种解释。
func durationEnv(name string, defaultValue time.Duration) (time.Duration, error) {
	value := os.Getenv(name)
	if value == "" {
		return defaultValue, nil
	}
	const maxTimeoutMS = int64((1<<63 - 1) / time.Millisecond)
	ms, err := strconv.ParseInt(value, 10, 64)
	if err != nil || ms < 0 || ms > maxTimeoutMS {
		return 0, fmt.Errorf("%s must be an integer between 0 and %d", name, maxTimeoutMS)
	}
	if ms == 0 {
		return defaultValue, nil
	}
	return time.Duration(ms) * time.Millisecond, nil
}

func requiredEnv(name string) string {
	value := os.Getenv(name)
	if value == "" {
		log.Fatalf("gateway-service: %s is required", name)
	}
	return value
}
