package service

import (
	"context"
	"testing"

	"github.com/panda-dev/panda-v2/backend/services/lottery-service/internal/dto"
	"github.com/panda-dev/panda-v2/backend/services/lottery-service/internal/model"
)

// fakeDrawRepo 只实现 GetDraw 走得到的那两个方法，其余由内嵌的 nil 接口兜着——真调到就 panic，
// 这正是想要的：这条用例只该走这两步。
type fakeDrawRepo struct {
	Repository
	gotQuery dto.WinQuery
}

func (f *fakeDrawRepo) GetDraw(context.Context, string) (*model.Draw, error) {
	return &model.Draw{ID: "6b1f4a26-2c3e-4b0d-9a77-3d5e8f0c1a44", RoundID: "0c9d1e77-8a2b-4f3c-9d10-2b6e4a8f7c55"}, nil
}

func (f *fakeDrawRepo) ListWins(_ context.Context, q dto.WinQuery) ([]*model.Win, int, error) {
	f.gotQuery = q
	return []*model.Win{{ID: "aa"}}, 1, nil
}

// TestGetDrawAsksForAFullFirstPage 钉住「看开奖」弹窗那个 500 的成因。
//
// 这条读路径不是分页读，是「这一期的全部中奖记录」，所以只需要一个足够大的 PageSize——
// 但 **Page 不能省**。WinQuery 的零值是 0，repository 把 page 原样交给 api.PageOffset 算
// (page-1)*pageSize，0 会算出负偏移，PostgreSQL 拒绝整条查询（SQLSTATE 2201X），
// 弹窗里永远只看到「操作失败，请稍后重试」，日志里那句 OFFSET must not be negative
// 也不带任何能指回这个字段的线索。
func TestGetDrawAsksForAFullFirstPage(t *testing.T) {
	repo := &fakeDrawRepo{}
	svc := New(repo, nil, nil, Options{})

	_, winners, err := svc.GetDraw(context.Background(), "6b1f4a26-2c3e-4b0d-9a77-3d5e8f0c1a44")
	if err != nil {
		t.Fatalf("GetDraw 出错：%v", err)
	}
	if len(winners) != 1 {
		t.Fatalf("中奖名单应有 1 条，实际 %d 条", len(winners))
	}
	if repo.gotQuery.Page < 1 {
		t.Errorf("GetDraw 传给 ListWins 的 Page 是 %d，必须 >= 1：0 会算成负 OFFSET 把整条查询打回",
			repo.gotQuery.Page)
	}
	if repo.gotQuery.PageSize != dto.MaxPageSize {
		t.Errorf("PageSize 应是 dto.MaxPageSize(%d)，实际 %d", dto.MaxPageSize, repo.gotQuery.PageSize)
	}
	if repo.gotQuery.RoundID == "" {
		t.Error("名单要按 round_id 反查，RoundID 不能空")
	}
}
