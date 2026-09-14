package rpc

import (
	"testing"
	"time"

	"github.com/panda-dev/panda-v2/backend/services/coffee-machine-service/internal/model"
)

// deviceMessage 是「不知道」与「离线」在内部契约边界上最后一次可能被抹平的地方：
// 只要这里把 nil 写成 false，调用方就再也分不出没同步过的设备和确认离线的设备。
func TestDeviceMessageKeepsUnknownDistinctFromOffline(t *testing.T) {
	// 直接看字段本身而不是 GetVendorOnline()：那个访问器在字段为 nil 时也返回
	// false，零值说明不了有没有 presence。
	neverSynced := deviceMessage(&model.Device{ID: "d1", Status: "active"})
	if neverSynced.VendorOnline != nil {
		t.Fatalf("VendorOnline = %v, want nil for a device that was never synced", *neverSynced.VendorOnline)
	}
	if neverSynced.LastSyncedAtUnix != nil {
		t.Fatalf("LastSyncedAtUnix = %v, want nil when there was no sync", *neverSynced.LastSyncedAtUnix)
	}
	if neverSynced.GetStoreId() != "" {
		t.Fatalf("StoreId = %q, want an empty string for a device with no store", neverSynced.GetStoreId())
	}

	offline := false
	syncedAt := time.Unix(1700000000, 0)
	storeID := "store-1"
	synced := deviceMessage(&model.Device{
		ID: "d2", Status: "disabled",
		VendorOnline: &offline, LastSyncedAt: &syncedAt, StoreID: &storeID,
	})
	if synced.VendorOnline == nil || *synced.VendorOnline {
		t.Fatalf("VendorOnline = %v, want a known false", synced.VendorOnline)
	}
	if synced.LastSyncedAtUnix == nil || *synced.LastSyncedAtUnix != 1700000000 {
		t.Fatalf("LastSyncedAtUnix = %v, want the sync timestamp", synced.LastSyncedAtUnix)
	}
	if synced.GetStoreId() != "store-1" {
		t.Fatalf("StoreId = %q, want store-1", synced.GetStoreId())
	}
}
