package registry

import (
	"context"
	"net/url"
	"os"
	"testing"
	"time"
)

// 注册/注销走的是真 etcd，没有可用的假实现能替代：这个用例存在的意义，
// 正是被替身掩盖住的那一类缺陷。
//
// TEST_ETCD_ENDPOINT 未设置时跳过，与仓库里其它集成用例一致
// （见 user-service 的 TEST_DATABASE_URL）。
func etcdRegistry(t *testing.T) (*Etcd, context.Context) {
	t.Helper()
	endpoint := os.Getenv("TEST_ETCD_ENDPOINT")
	if endpoint == "" {
		t.Skip("TEST_ETCD_ENDPOINT is required for etcd integration tests")
	}
	reg, ok := New(endpoint).(*Etcd)
	if !ok {
		t.Fatalf("New(%q) did not return an *Etcd", endpoint)
	}
	t.Cleanup(func() { _ = reg.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	return reg, ctx
}

// 持有租约时的注销曾经必崩：Unregister 连续调了两次 Txn.If，而 clientv3 的 If
// 不是可累加的构造器，第二次直接 panic("cannot call If twice!")。这个组合只在
// 「注册过、租约还在、然后优雅下线」时出现——也就是正常停服的路径。
func TestEtcdUnregisterWithLeaseReleasesInstance(t *testing.T) {
	reg, ctx := etcdRegistry(t)

	// 服务名带上时间戳，避免与并发跑的其它用例或真实的 dev 栈互相踩到。
	service := "registry-test-" + time.Now().Format("150405.000000")
	instance, err := NewInstance(service, &url.URL{Scheme: "grpc", Host: "127.0.0.1:19099"}, "test", "test")
	if err != nil {
		t.Fatal(err)
	}
	if err := reg.Register(ctx, instance); err != nil {
		t.Fatalf("Register: %v", err)
	}
	instances, err := reg.Resolve(ctx, service)
	if err != nil {
		t.Fatalf("Resolve after Register: %v", err)
	}
	if len(instances) != 1 {
		t.Fatalf("after Register: got %d instances want 1", len(instances))
	}

	if err := reg.Unregister(ctx, instance); err != nil {
		t.Fatalf("Unregister: %v", err)
	}
	instances, err = reg.Resolve(ctx, service)
	if err != nil {
		t.Fatalf("Resolve after Unregister: %v", err)
	}
	if len(instances) != 0 {
		t.Fatalf("after Unregister: got %d instances want 0", len(instances))
	}
}
