package main

import (
	"testing"
	"time"
)

func TestDurationEnv(t *testing.T) {
	for _, tc := range []struct {
		name, value string
		want        time.Duration
		wantErr     bool
	}{
		{"default", "", 10 * time.Second, false},
		{"zero uses default", "0", 10 * time.Second, false},
		{"one millisecond", "1", time.Millisecond, false},
		{"configured", "2500", 2500 * time.Millisecond, false},
		{"maximum duration", "9223372036854", 9223372036854 * time.Millisecond, false},
		{"duration overflow", "9223372036855", 0, true},
		{"int64 maximum", "9223372036854775807", 0, true},
		{"integer overflow", "9223372036854775808", 0, true},
		{"negative", "-1", 0, true},
		{"fractional", "1.5", 0, true},
		{"units", "10s", 0, true},
		{"whitespace", " 10 ", 0, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("GATEWAY_REQUEST_TIMEOUT_MS", tc.value)
			got, err := durationEnv("GATEWAY_REQUEST_TIMEOUT_MS", 10*time.Second)
			if (err != nil) != tc.wantErr || got != tc.want {
				t.Fatalf("durationEnv() = %v, %v; want %v, error=%v", got, err, tc.want, tc.wantErr)
			}
		})
	}
}

// 上传路径的缺省值必须比其余路由大一个量级：拿 10 秒当上传预算会让任何一张
// 稍大的图片都超时。默认值写在 cmd 里，所以这条断言也钉在这里。
func TestUploadTimeoutDefault(t *testing.T) {
	t.Setenv("GATEWAY_UPLOAD_TIMEOUT_MS", "")
	got, err := durationEnv("GATEWAY_UPLOAD_TIMEOUT_MS", 120*time.Second)
	if err != nil || got != 120*time.Second {
		t.Fatalf("upload timeout = %v, %v; want 120s", got, err)
	}

	t.Setenv("GATEWAY_UPLOAD_TIMEOUT_MS", "45000")
	got, err = durationEnv("GATEWAY_UPLOAD_TIMEOUT_MS", 120*time.Second)
	if err != nil || got != 45*time.Second {
		t.Fatalf("configured upload timeout = %v, %v; want 45s", got, err)
	}
}
