package upload

import (
	"context"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"
)

// 真实 OSS 的往返测试。默认**跳过**：这条链路需要真凭据、会往真桶里写一个对象，
// 不该在 CI 或任何人的机器上「顺手」跑起来。
//
// 它存在是因为其余用例都走假 Store，唯一没被覆盖的就是 oss.go 里对 SDK 的用法——
// 而那里恰好集中了三个曾经真出过错的地方：SDK 超时上界、Content-Type 有没有设、
// 以及对象能不能被公开读。手抄一遍冒烟步骤容易漏，跑这个不会：
//
//	set -a && . ../../../.env && set +a
//	go test ./platform/upload/ -run TestOSSIntegration -v
//
// 要求 OSS_ACCESS_KEY / OSS_SECRET_KEY / OSS_ENDPOINT / OSS_BUCKET 都在环境里。
// 测试自己新增对象（内容寻址，固定字节 → 固定 key），不读也不删任何已有对象。
func TestOSSIntegration(t *testing.T) {
	cfg := Config{
		AccessKey:   strings.TrimSpace(os.Getenv("OSS_ACCESS_KEY")),
		SecretKey:   strings.TrimSpace(os.Getenv("OSS_SECRET_KEY")),
		Endpoint:    strings.TrimSpace(os.Getenv("OSS_ENDPOINT")),
		Bucket:      strings.TrimSpace(os.Getenv("OSS_BUCKET")),
		CNAME:       strings.TrimSpace(os.Getenv("OSS_CNAME")),
		Prefix:      "panda-v2-integration-test",
		MaxFileSize: 1024 * 1024,
		UseMD5:      true,
	}
	if cfg.AccessKey == "" || cfg.SecretKey == "" || cfg.Endpoint == "" || cfg.Bucket == "" {
		t.Skip("OSS_ACCESS_KEY/OSS_SECRET_KEY/OSS_ENDPOINT/OSS_BUCKET 未全部提供，跳过真实往返测试")
	}

	uploader, err := New(cfg)
	if err != nil {
		t.Fatalf("New with the provided configuration: %v", err)
	}

	// 一份最小的合法 PNG（1x1）。内容固定，所以 key 也固定，重跑不会堆对象。
	data := pngBytes(64)

	first, err := uploader.Upload(context.Background(), "integration.png", data)
	if err != nil {
		t.Fatalf("upload: %v", err)
	}
	if !strings.HasPrefix(first.Key, cfg.Prefix+"/") {
		t.Fatalf("key %q is outside the configured prefix %q", first.Key, cfg.Prefix)
	}
	if !strings.HasSuffix(first.Key, ".png") {
		t.Fatalf("key %q does not carry the sniffed extension", first.Key)
	}
	t.Logf("已写入对象：%s", first.URL)

	// 同一份字节再传一次：内容寻址意味着必须是同一个 key，否则「重试幂等」是空话。
	second, err := uploader.Upload(context.Background(), "换个名字也行.png", data)
	if err != nil {
		t.Fatalf("re-upload: %v", err)
	}
	if second.Key != first.Key {
		t.Fatalf("identical bytes produced different keys: %q vs %q", first.Key, second.Key)
	}

	// 公开可读 + Content-Type 正确。这两条只能对真实对象验：
	// 前者是「返回绝对 URL」这个设计成立的前提（桶不是公共读时它会静默裂图），
	// 后者是旧实现漏设 Content-Type、对象以 octet-stream 存下来会变成下载的坑。
	client := &http.Client{Timeout: 20 * time.Second}
	response, err := client.Get(first.URL)
	if err != nil {
		t.Fatalf("GET %s: %v", first.URL, err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("GET %s: HTTP %d — 桶可能不是公共读，返回的绝对 URL 会渲染不出来", first.URL, response.StatusCode)
	}
	if got := response.Header.Get("Content-Type"); got != "image/png" {
		t.Fatalf("Content-Type = %q, want image/png（存成 octet-stream 时浏览器会下载而不是渲染）", got)
	}
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if len(body) != len(data) {
		t.Fatalf("round-tripped %d bytes, want %d", len(body), len(data))
	}
}
