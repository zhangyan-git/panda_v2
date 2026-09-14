package upload

import (
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/aliyun/aliyun-oss-go-sdk/oss"
)

// 这是整个包唯一 import 对象存储 SDK 的文件。守住这条边界不只是为了
// 让 upload.go 好读：backend 是被各服务 replace 掉的模块，SDK 出现在哪里，
// 谁的 go.sum 就会跟着变。
//
// 两个超时是给 SDK 自己的限时器，单位是秒。SDK 的 PutObject 不接受 context，
// 一次 PUT 的上界只能由它给：建连 10 秒，单次读或写 60 秒（SDK 另外把
// readWrite*10 当作两次操作之间的空闲上限）。
//
// 必须显式设置。默认是 10/20，和服务端超时不成比例；超时一旦错配，结果就是
// 自相矛盾的状态——服务端早就告诉客户端失败了，连接还在往里写。
// 选定 60 秒是因为它落在服务端 90 秒之内：OSS 卡住时先由 SDK 超时失败，
// 处理器能给出干净的 503，而不是被 kratos 的整请求 deadline 打断成 i/o timeout。
const (
	ossConnectTimeoutSec   = 10
	ossReadWriteTimeoutSec = 60
)

// ossStore 是 Store 的 OSS 实现。
type ossStore struct {
	bucket *oss.Bucket
}

// newOSSStore 建立到 OSS 的连接。这里只是拿到 bucket 的 handle，
// 真正验证权限要等第一次写入——所以「配置全对但 bucket 不可写」只会在
// 第一次上传时暴露，这一点写在 merchant-service 的 README 里。
func newOSSStore(cfg Config) (Store, error) {
	client, err := oss.New(strings.TrimSpace(cfg.Endpoint), cfg.AccessKey, cfg.SecretKey,
		oss.Timeout(ossConnectTimeoutSec, ossReadWriteTimeoutSec))
	if err != nil {
		return nil, fmt.Errorf("upload: connect OSS: %w", err)
	}
	bucket, err := client.Bucket(cfg.Bucket)
	if err != nil {
		return nil, fmt.Errorf("upload: use OSS bucket %q: %w", cfg.Bucket, err)
	}
	return &ossStore{bucket: bucket}, nil
}

// Put 写入一个对象。
//
// size 没有转发给 SDK：PutObject 自己会从可寻址的 reader 上量出长度并设置
// Content-Length，显式再传一次只会和它算出来的值争一个头。
func (s *ossStore) Put(ctx context.Context, key string, r io.Reader, size int64, contentType string) error {
	_ = size
	// PutObject 不认 context，这里只能做一次「已经取消了就别开始」的检查，
	// 真正的上界是上面那组 SDK 超时。刻意不把它丢进 goroutine 里 select
	// ctx.Done()：提前返回会报出一次失败，而对象很可能已经写成——md5 键让
	// 重试无害，但一次假失败会污染日志和告警，比让调用方多等几十秒更糟。
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("upload: %w", err)
	}
	if err := s.bucket.PutObject(key, r,
		// Content-Type 用嗅探出来的，不用客户端声明的。
		oss.ContentType(contentType),
		// 键由内容摘要决定，内容永不变，这个缓存头是白送的。
		oss.CacheControl("public, max-age=31536000, immutable"),
	); err != nil {
		return fmt.Errorf("upload: put object %q: %w", key, err)
	}
	return nil
}
