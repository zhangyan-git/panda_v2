package ratelimit

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func requestWith(remoteAddr string, forwarded ...string) *http.Request {
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.RemoteAddr = remoteAddr
	for _, value := range forwarded {
		// Add 而不是 Set：HTTP 允许同一个头出现多次，语义上等于逗号连接。
		r.Header.Add("X-Forwarded-For", value)
	}
	return r
}

func mustProxyTrust(t *testing.T, cidrs ...string) *ProxyTrust {
	t.Helper()
	trust, err := NewProxyTrust(cidrs)
	if err != nil {
		t.Fatalf("NewProxyTrust(%v) = %v, want nil", cidrs, err)
	}
	return trust
}

// 不配可信代理就是原来的行为：RemoteAddr 说了算，XFF 一个字节都不看。
// 这条是默认路径，也是升级不该放松的那条安全性质。
func TestProxyTrustWithoutConfigurationIgnoresForwardedFor(t *testing.T) {
	for name, trust := range map[string]*ProxyTrust{
		"empty list": mustProxyTrust(t),
		"nil":        nil,
	} {
		r := requestWith("203.0.113.9:5555", "1.2.3.4")
		if got := trust.ClientIP(r); got != "203.0.113.9" {
			t.Errorf("%s: ClientIP = %q, want the peer address", name, got)
		}
	}
}

// 对端不在可信网段时，它就是客户端自己（或一台我们不认识的机器），
// 它写的 XFF 同样是不可信输入。
func TestProxyTrustIgnoresForwardedForFromUntrustedPeer(t *testing.T) {
	trust := mustProxyTrust(t, "10.0.0.0/8")
	r := requestWith("203.0.113.9:5555", "1.2.3.4")
	if got := trust.ClientIP(r); got != "203.0.113.9" {
		t.Fatalf("ClientIP = %q, want the peer address: 伪造的 XFF 必须被忽略", got)
	}
}

// 可信代理解析 XFF：一跳就是客户端地址，限流因此按人计数而不是按 LB 计数。
func TestProxyTrustReadsForwardedForFromTrustedPeer(t *testing.T) {
	trust := mustProxyTrust(t, "10.0.0.0/8")
	r := requestWith("10.0.0.5:44321", "203.0.113.9")
	if got := trust.ClientIP(r); got != "203.0.113.9" {
		t.Fatalf("ClientIP = %q, want 203.0.113.9", got)
	}
}

// 核心安全性质：客户端自己塞在 XFF 左边的前缀必须无效。
// LB 会把「它看到的对端」追加在右边，从右往左走先撞到真实地址就停下，
// 伪造的那段在更左边，永远走不到。
func TestProxyTrustIgnoresForgedForwardedForPrefix(t *testing.T) {
	trust := mustProxyTrust(t, "10.0.0.0/8")
	// 客户端发 X-Forwarded-For: 1.2.3.4，LB 追加真实来源 203.0.113.9。
	r := requestWith("10.0.0.5:44321", "1.2.3.4, 203.0.113.9")
	if got := trust.ClientIP(r); got != "203.0.113.9" {
		t.Fatalf("ClientIP = %q, want 203.0.113.9（不是客户端伪造的 1.2.3.4）", got)
	}
}

// 多层代理：从右往左跳过所有可信跳，第一个不可信的就是客户端。
func TestProxyTrustWalksPastTrustedHops(t *testing.T) {
	trust := mustProxyTrust(t, "10.0.0.0/8", "192.168.0.0/16")
	r := requestWith("10.0.0.5:44321", "203.0.113.9, 192.168.1.7, 10.0.0.9")
	if got := trust.ClientIP(r); got != "203.0.113.9" {
		t.Fatalf("ClientIP = %q, want 203.0.113.9", got)
	}
}

// 整条链都可信时退回 peer，而不是最左边那个值。链上没有任何一跳能证明
// 更左边的地址是真的，宁可退化成「按代理地址计数」这个共享桶。
func TestProxyTrustFallsBackToPeerWhenEveryHopIsTrusted(t *testing.T) {
	trust := mustProxyTrust(t, "10.0.0.0/8")
	r := requestWith("10.0.0.5:44321", "10.0.0.6, 10.0.0.7")
	if got := trust.ClientIP(r); got != "10.0.0.5" {
		t.Fatalf("ClientIP = %q, want the peer address", got)
	}
}

// 链的长度由上游决定，不设上限就等于把解析成本交给对方。
// 超出预算就退回 peer——保守，但不给出一条能被无限拉长的循环。
func TestProxyTrustBoundsForwardedChainLength(t *testing.T) {
	trust := mustProxyTrust(t, "10.0.0.0/8")
	chain := make([]string, 0, maxForwardedHops+5)
	chain = append(chain, "203.0.113.9") // 最左边：够不着
	for range maxForwardedHops + 4 {
		chain = append(chain, "10.0.0.1")
	}
	r := requestWith("10.0.0.5:44321", strings.Join(chain, ", "))
	if got := trust.ClientIP(r); got != "10.0.0.5" {
		t.Fatalf("ClientIP = %q, want the peer address once the chain exceeds the budget", got)
	}
}

// 真实部署里的写法不统一，都要认得；畸形项跳过而不是让整条链作废。
func TestProxyTrustParsesHeterogeneousHops(t *testing.T) {
	trust := mustProxyTrust(t, "10.0.0.0/8", "::1/128")
	for name, tc := range map[string]struct {
		remote    string
		forwarded []string
		want      string
	}{
		"ipv6 with brackets and port": {"10.0.0.5:44321", []string{"[2001:db8::1]:5555"}, "2001:db8::1"},
		"ipv4 with port":              {"10.0.0.5:44321", []string{"203.0.113.9:1234"}, "203.0.113.9"},
		"ipv4-mapped ipv6":            {"10.0.0.5:44321", []string{"::ffff:203.0.113.9"}, "203.0.113.9"},
		"garbage hop is skipped":      {"10.0.0.5:44321", []string{"203.0.113.9, unknown, 10.0.0.1"}, "203.0.113.9"},
		"blank hops are skipped":      {"10.0.0.5:44321", []string{" , 203.0.113.9 , "}, "203.0.113.9"},
		"multiple headers are joined": {"10.0.0.5:44321", []string{"203.0.113.9", "10.0.0.1"}, "203.0.113.9"},
	} {
		if got := trust.ClientIP(requestWith(tc.remote, tc.forwarded...)); got != tc.want {
			t.Errorf("%s: ClientIP = %q, want %q", name, got, tc.want)
		}
	}
}

// 可信代理没写 XFF（或写空）时退回 peer，不能返回空串——空串会让所有
// 这种请求共用一个桶，也会让「没拿到地址」看起来像「拿到了一个地址」。
func TestProxyTrustFallsBackToPeerWithoutForwardedFor(t *testing.T) {
	trust := mustProxyTrust(t, "10.0.0.0/8")
	if got := trust.ClientIP(requestWith("10.0.0.5:44321")); got != "10.0.0.5" {
		t.Fatalf("ClientIP = %q, want the peer address", got)
	}
	if got := trust.ClientIP(requestWith("10.0.0.5:44321", "")); got != "10.0.0.5" {
		t.Fatalf("ClientIP = %q, want the peer address", got)
	}
}

// 漏写掩码是最容易犯的错，而且错得隐蔽：那台代理会被判成不可信，
// 限流悄悄退回全站一个桶。所以裸地址按单主机接受。
func TestNewProxyTrustAcceptsBareAddress(t *testing.T) {
	for _, cidr := range []string{"10.0.0.5", "10.0.0.5/32", "::1", "::1/128"} {
		trust, err := NewProxyTrust([]string{cidr})
		if err != nil {
			t.Fatalf("NewProxyTrust(%q) = %v, want nil", cidr, err)
		}
		if len(trust.nets) != 1 {
			t.Fatalf("NewProxyTrust(%q) kept %d networks, want 1", cidr, len(trust.nets))
		}
	}
}

// 写错的网段必须报错：静默忽略等于让限流在没人察觉的情况下退回全站一个桶。
func TestNewProxyTrustRejectsInvalidCIDR(t *testing.T) {
	for _, cidr := range []string{"10.0.0.0/33", "not-an-ip", "lb.internal", "10.0.0.0/8/8"} {
		if _, err := NewProxyTrust([]string{cidr}); err == nil {
			t.Errorf("NewProxyTrust(%q) = nil, want an error", cidr)
		}
	}
}
