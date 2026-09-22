package service

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/panda-dev/panda-v2/backend/services/merchant-service/internal/model"
	"github.com/panda-dev/panda-v2/backend/services/merchant-service/internal/repository"
)

type merchantRepoFake struct {
	repository.MerchantRepository
	events      *[]string
	merchantID  string
	hasChildren bool
	childrenErr error
	deleteErr   error
	deleted     bool
}

func (f *merchantRepoFake) FindByID(_ context.Context, id string) (*model.Merchant, error) {
	*f.events = append(*f.events, "find_merchant")
	f.merchantID = id
	return &model.Merchant{ID: id}, nil
}

func (f *merchantRepoFake) HasBrandsOrStores(context.Context, string) (bool, error) {
	*f.events = append(*f.events, "has_children")
	return f.hasChildren, f.childrenErr
}

func (f *merchantRepoFake) Delete(context.Context, string) error {
	*f.events = append(*f.events, "delete_merchant")
	f.deleted = true
	return f.deleteErr
}

type accountPresenceFake struct {
	has bool
	err error
}

func (f accountPresenceFake) HasUsers(context.Context, string) (bool, error) {
	return f.has, f.err
}

// TestMerchantDeleteRefusesWhileBrandsOrStoresExist 覆盖第 6 条：brands / stores 对
// merchants 是 ON DELETE CASCADE，名下还有它们时删除必须被拦在 DELETE 之前，
// 否则品牌、门店与两张审核表的历史会跟着静默消失。
func TestMerchantDeleteRefusesWhileBrandsOrStoresExist(t *testing.T) {
	failure := errors.New("children query failed")
	for _, tt := range []struct {
		name        string
		hasUsers    bool
		hasChildren bool
		childrenErr error
		deleteErr   error
		want        error
		wantEvents  []string
		wantDeleted bool
	}{
		{
			name:        "名下有品牌或门店",
			hasChildren: true,
			want:        ErrMerchantHasBrandsOrStores,
			wantEvents:  []string{"find_merchant", "has_children"},
		},
		{
			name:        "名下没有品牌与门店",
			wantEvents:  []string{"find_merchant", "has_children", "delete_merchant"},
			wantDeleted: true,
		},
		{
			name:        "检查失败时不删除",
			childrenErr: failure,
			want:        failure,
			wantEvents:  []string{"find_merchant", "has_children"},
		},
		{
			name:        "删除失败照旧上抛",
			deleteErr:   failure,
			want:        failure,
			wantEvents:  []string{"find_merchant", "has_children", "delete_merchant"},
			wantDeleted: true,
		},
		{
			name:       "名下有账号时先说账号（原有行为不变）",
			hasUsers:   true,
			want:       ErrMerchantHasUsers,
			wantEvents: []string{"find_merchant"},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var events []string
			merchants := &merchantRepoFake{events: &events, hasChildren: tt.hasChildren, childrenErr: tt.childrenErr, deleteErr: tt.deleteErr}
			svc := NewAdminMerchantService(merchants, accountPresenceFake{has: tt.hasUsers})

			if err := svc.Delete(context.Background(), "m1"); !errors.Is(err, tt.want) {
				t.Fatalf("err=%v want %v", err, tt.want)
			}
			if !reflect.DeepEqual(events, tt.wantEvents) {
				t.Fatalf("events=%v want %v", events, tt.wantEvents)
			}
			if merchants.deleted != tt.wantDeleted {
				t.Fatalf("deleted=%v want %v", merchants.deleted, tt.wantDeleted)
			}
		})
	}
}
