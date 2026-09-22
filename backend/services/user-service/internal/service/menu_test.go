package service

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/panda-dev/panda-v2/backend/services/user-service/internal/model"
	"github.com/panda-dev/panda-v2/backend/services/user-service/internal/repository"
)

// fakeMenuRepo 只实现 Update 这条路径用到的方法，其余靠内嵌接口兜。
type fakeMenuRepo struct {
	repository.MenuRepository
	menus    []*model.Menu
	findErr  error
	updateNo int
}

func (f *fakeMenuRepo) FindAll(context.Context) ([]*model.Menu, error) {
	return f.menus, f.findErr
}

func (f *fakeMenuRepo) FindByID(_ context.Context, id string) (*model.Menu, error) {
	for _, m := range f.menus {
		if m.ID == id {
			return m, nil
		}
	}
	return nil, errors.New("menu not found")
}

func (f *fakeMenuRepo) Update(_ context.Context, m *model.Menu) error {
	f.updateNo++
	return nil
}

// 环上的菜单：a 的父是 b、b 的父是 a。外键不拦（parent_id 只声明了 ON DELETE CASCADE），
// 服务层的 Update 也拦不住自己——它正是在这份数据上做判断。没有 visited 时这里转不完。
func cyclicMenus() []*model.Menu {
	return []*model.Menu{
		{ID: "a", ParentID: "b", Name: "A"},
		{ID: "b", ParentID: "a", Name: "B"},
		{ID: "c", ParentID: "", Name: "C"},
	}
}

func TestUpdateTerminatesOnCyclicMenus(t *testing.T) {
	repo := &fakeMenuRepo{menus: cyclicMenus()}
	svc := NewAdminMenuService(repo, nil, nil)

	done := make(chan error, 1)
	go func() {
		_, err := svc.Update(context.Background(), "a", "c", "A", "/a", "", 1)
		done <- err
	}()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("环上改菜单应当照常走完（环只影响遍历，不影响这一次改挂载），得到 %v", err)
		}
		if repo.updateNo != 1 {
			t.Fatalf("期望写入一次，实际 %d 次", repo.updateNo)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("2 秒未返回：isDescendant 在成环数据上不终止（visited 没了？）")
	}
}

// 兜环不能把「真的是后代」判成不是：这是防环的那道闸本身。
func TestUpdateRejectsDescendantAsParent(t *testing.T) {
	// x → a → b，把 x 挂到自己的后代 b 下
	menus := []*model.Menu{
		{ID: "x", ParentID: "", Name: "X"},
		{ID: "a", ParentID: "x", Name: "A"},
		{ID: "b", ParentID: "a", Name: "B"},
	}
	repo := &fakeMenuRepo{menus: menus}
	svc := NewAdminMenuService(repo, nil, nil)

	if _, err := svc.Update(context.Background(), "x", "b", "X", "/x", "", 1); !errors.Is(err, ErrMenuParentCycle) {
		t.Fatalf("期望 ErrMenuParentCycle，得到 %v", err)
	}
	if repo.updateNo != 0 {
		t.Fatal("被拒的修改不该落库")
	}
}

// 同一棵树里绕远路也要认出来；顺带钉住 visited 没让「可达」漏判。
func TestIsDescendantFindsDeepAndSiblingSubtrees(t *testing.T) {
	menus := []*model.Menu{
		{ID: "x", ParentID: "", Name: "X"},
		{ID: "a", ParentID: "x", Name: "A"},
		{ID: "b", ParentID: "a", Name: "B"},
		{ID: "c", ParentID: "b", Name: "C"},
		{ID: "z", ParentID: "", Name: "Z"},
		{ID: "y", ParentID: "z", Name: "Y"},
	}
	svc := NewAdminMenuService(&fakeMenuRepo{menus: menus}, nil, nil)

	inSub, err := svc.isDescendant(context.Background(), "x", "c")
	if err != nil {
		t.Fatal(err)
	}
	if !inSub {
		t.Fatal("c 是 x 的第三代，应当判为后代")
	}
	inSub, err = svc.isDescendant(context.Background(), "x", "y")
	if err != nil {
		t.Fatal(err)
	}
	if inSub {
		t.Fatal("y 挂在另一棵树上，不是 x 的后代")
	}
}
