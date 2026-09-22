package service

import (
	"context"
	"errors"
	"testing"

	"github.com/panda-dev/panda-v2/backend/services/coffee-machine-service/internal/model"
	"github.com/panda-dev/panda-v2/backend/services/coffee-machine-service/internal/repository"
)

// recordingMasterData 只实现被这两个用例盯着的两个方法，其余方法靠嵌入的那个 nil 接口
// 顶着——用例要证明的正是「空参数根本没走到仓储」，一旦真调到了别的办法，它会 panic，
// 而不是安静地返回零值让用例变绿。
type recordingMasterData struct {
	repository.MasterDataRepository
	serial    string
	deviceID  string
	drinkCode string
	drinkID   string
}

func (r *recordingMasterData) GetDeviceBySerial(_ context.Context, serialUnique string) (*model.Device, error) {
	r.serial = serialUnique
	return &model.Device{ID: "device-1", SerialUnique: serialUnique}, nil
}

func (r *recordingMasterData) GetDeviceDrink(_ context.Context, deviceID, drinkCode string) (*model.Drink, error) {
	r.deviceID, r.drinkCode = deviceID, drinkCode
	return &model.Drink{ID: "drink-1", ProductNum: drinkCode}, nil
}

func (r *recordingMasterData) GetDrink(_ context.Context, id string) (*model.Drink, error) {
	r.drinkID = id
	return &model.Drink{ID: id}, nil
}

// TestGetDeviceBySerialRejectsBlankBeforeTheQuery 是设备回调建单（方案 §四）那一侧最容易
// 埋下的一跤：机器报了个空序列号，查询会老实地回一句「查不到」，调用方于是把它读成
// 「这台机器不存在」——而该说的话是「你没给编号」。所以空串必须在这里停下。
func TestGetDeviceBySerialRejectsBlankBeforeTheQuery(t *testing.T) {
	repo := &recordingMasterData{}
	svc := NewMasterDataService(repo)

	for _, blank := range []string{"", "   "} {
		if _, err := svc.GetDeviceBySerial(context.Background(), blank); !errors.Is(err, ErrDeviceSerialRequired) {
			t.Fatalf("serial %q: err = %v, want ErrDeviceSerialRequired", blank, err)
		}
		if repo.serial != "" {
			t.Fatalf("serial %q reached the repository as %q", blank, repo.serial)
		}
	}

	// 合法的序列号按写路径的口径先 trim 再查：库里存的就是 trim 过的值，带着空格去比
	// 只会得到一次「查不到」。
	device, err := svc.GetDeviceBySerial(context.Background(), "  SN-1  ")
	if err != nil {
		t.Fatalf("get device by serial: %v", err)
	}
	if repo.serial != "SN-1" {
		t.Fatalf("repository got %q, want the trimmed serial", repo.serial)
	}
	if device.SerialUnique != "SN-1" {
		t.Fatalf("serial_unique = %q, want SN-1", device.SerialUnique)
	}
}

// TestGetDeviceDrinkRejectsBlankBeforeTheQuery 同一个道理的另一半：设备回调只报两个值，
// 缺哪一个都不该变成「这杯饮品不在库里」——那时钱已经在机器上收过了。
func TestGetDeviceDrinkRejectsBlankBeforeTheQuery(t *testing.T) {
	const deviceID = "11111111-1111-1111-1111-111111111111"

	repo := &recordingMasterData{}
	svc := NewMasterDataService(repo)

	for _, missing := range []struct {
		name           string
		deviceID, code string
		want           error
	}{
		{"设备为空", "", "1001", ErrDrinkLookupDeviceRequired},
		{"设备只有空格", "   ", "1001", ErrDrinkLookupDeviceRequired},
		{"编号为空", deviceID, "", ErrDrinkLookupCodeRequired},
		{"编号只有空格", deviceID, "  ", ErrDrinkLookupCodeRequired},
	} {
		t.Run(missing.name, func(t *testing.T) {
			if _, err := svc.GetDeviceDrink(context.Background(), missing.deviceID, missing.code); !errors.Is(err, missing.want) {
				t.Fatalf("err = %v, want %v", err, missing.want)
			}
			if repo.deviceID != "" || repo.drinkCode != "" {
				t.Fatalf("查询走到了仓储：deviceID = %q, drinkCode = %q", repo.deviceID, repo.drinkCode)
			}
		})
	}

	// 合法入参先 trim 再查：库里的值就是 trim 过的，带着空格去比只会得到一次「查不到」。
	drink, err := svc.GetDeviceDrink(context.Background(), "  "+deviceID+"  ", "  1001 ")
	if err != nil {
		t.Fatalf("get device drink: %v", err)
	}
	if repo.deviceID != deviceID || repo.drinkCode != "1001" {
		t.Fatalf("repository got deviceID = %q, drinkCode = %q, want both trimmed", repo.deviceID, repo.drinkCode)
	}
	if drink.ProductNum != "1001" {
		t.Fatalf("product_num = %q, want 1001", drink.ProductNum)
	}
}

// TestGetDrinkRejectsBlankBeforeTheQuery 是按主键取饮品（小程序下单定价那条路）的同一条规矩。
//
// 这一条比上面那条更贵：调用方是 order-service 下单，它把「没给 id」读成「这杯不在目录里」，
// 用户看到的是一句「该饮品已下架」——一次填漏的参数被报成一次业务拒绝，而那时用户手里
// 明明点着一杯真实的饮品。
func TestGetDrinkRejectsBlankBeforeTheQuery(t *testing.T) {
	const drinkID = "22222222-2222-2222-2222-222222222222"

	repo := &recordingMasterData{}
	svc := NewMasterDataService(repo)

	for _, blank := range []string{"", "   "} {
		if _, err := svc.GetDrink(context.Background(), blank); !errors.Is(err, ErrDrinkIDRequired) {
			t.Fatalf("id %q: err = %v, want ErrDrinkIDRequired", blank, err)
		}
		if repo.drinkID != "" {
			t.Fatalf("id %q reached the repository as %q", blank, repo.drinkID)
		}
	}

	// 合法入参先 trim 再查，与上面两条同一个口径：库里的 uuid 不带空格。
	drink, err := svc.GetDrink(context.Background(), "  "+drinkID+"  ")
	if err != nil {
		t.Fatalf("get drink: %v", err)
	}
	if repo.drinkID != drinkID {
		t.Fatalf("repository got %q, want the trimmed id", repo.drinkID)
	}
	if drink.ID != drinkID {
		t.Fatalf("drink id = %q, want %q", drink.ID, drinkID)
	}
}
