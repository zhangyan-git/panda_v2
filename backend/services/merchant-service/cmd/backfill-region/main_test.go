package main

import (
	"reflect"
	"testing"
)

func testIndex() map[key]codes {
	return index([]regionNode{
		{Value: "33", Label: "浙江省", Children: []regionNode{
			{Value: "3301", Label: "杭州市", Children: []regionNode{
				{Value: "330106", Label: "西湖区"},
				{Value: "330105", Label: "拱墅区"},
			}},
		}},
		// 桥西区是全国 28 组重名区县里的真实一例：同一个省、不同地级市。
		{Value: "13", Label: "河北省", Children: []regionNode{
			{Value: "1301", Label: "石家庄市", Children: []regionNode{
				{Value: "130104", Label: "桥西区"},
			}},
			{Value: "1307", Label: "张家口市", Children: []regionNode{
				{Value: "130703", Label: "桥西区"},
			}},
		}},
		{Value: "11", Label: "北京市", Children: []regionNode{
			{Value: "1101", Label: "市辖区", Children: []regionNode{
				{Value: "110101", Label: "东城区"},
			}},
		}},
	})
}

// TestPlanBucketsRows 覆盖回填的三条真实路径：能查到就写、查不到就报告、
// 区划不全就跳过。这三者的处置方式完全不同，混在一起计数就再也分不开了。
func TestPlanBucketsRows(t *testing.T) {
	rows := []storeRow{
		{ID: "matched", Province: "浙江省", City: "杭州市", District: "西湖区"},
		{ID: "unmatched", Province: "北京", City: "北京市", District: "东城区"},
		{ID: "empty", Province: "浙江省", City: "", District: ""},
		{ID: "already", Province: "浙江省", City: "杭州市", District: "拱墅区", ProvinceCode: "33", CityCode: "3301", DistrictCode: "330105"},
		{ID: "wrong", Province: "浙江省", City: "杭州市", District: "拱墅区", ProvinceCode: "99", CityCode: "9901", DistrictCode: "990105"},
	}
	updates, unmatched, skipped := plan(rows, testIndex())

	want := []update{
		{ID: "matched", Codes: codes{province: "33", city: "3301", district: "330106"}},
		{ID: "wrong", Codes: codes{province: "33", city: "3301", district: "330105"}},
	}
	if !reflect.DeepEqual(updates, want) {
		t.Fatalf("updates = %+v, want %+v", updates, want)
	}
	if ids := idsOf(unmatched); !reflect.DeepEqual(ids, []string{"unmatched"}) {
		t.Fatalf("unmatched = %v", ids)
	}
	if ids := idsOf(skipped); !reflect.DeepEqual(ids, []string{"empty"}) {
		t.Fatalf("skipped = %v", ids)
	}
}

// TestPlanIsIdempotent 是这条工具最重要的性质：它改的是历史数据，跑完总想再确认
// 一遍。第二次必须一行都不写。
func TestPlanIsIdempotent(t *testing.T) {
	indexed := testIndex()
	rows := []storeRow{{ID: "a", Province: "浙江省", City: "杭州市", District: "西湖区"}}

	first, _, _ := plan(rows, indexed)
	if len(first) != 1 {
		t.Fatalf("first run updates = %d, want 1", len(first))
	}
	apply := func(rows []storeRow, updates []update) {
		for _, item := range updates {
			for i := range rows {
				if rows[i].ID != item.ID {
					continue
				}
				rows[i].ProvinceCode = item.Codes.province
				rows[i].CityCode = item.Codes.city
				rows[i].DistrictCode = item.Codes.district
			}
		}
	}
	apply(rows, first)

	second, unmatched, skipped := plan(rows, indexed)
	if len(second) != 0 || len(unmatched) != 0 || len(skipped) != 0 {
		t.Fatalf("second run = %d updates, %d unmatched, %d skipped; want no work left",
			len(second), len(unmatched), len(skipped))
	}
}

func TestPlanTrimsWhitespace(t *testing.T) {
	updates, unmatched, _ := plan([]storeRow{
		{ID: "padded", Province: " 浙江省 ", City: "杭州市\t", District: " 西湖区"},
	}, testIndex())
	if len(updates) != 1 || len(unmatched) != 0 {
		t.Fatalf("updates = %+v, unmatched = %+v", updates, unmatched)
	}
}

// TestIndexKeysOnTheWholeTriple 钉住「区名会重名」这件事：石家庄市桥西区与张家口市
// 桥西区同名，编码不同，靠区名单独查会串到别的城市去。
func TestIndexKeysOnTheWholeTriple(t *testing.T) {
	indexed := testIndex()
	if got := indexed[key{"河北省", "石家庄市", "桥西区"}]; got.district != "130104" {
		t.Fatalf("石家庄市桥西区 = %+v", got)
	}
	if got := indexed[key{"河北省", "张家口市", "桥西区"}]; got.district != "130703" {
		t.Fatalf("张家口市桥西区 = %+v", got)
	}
	// 直辖市在数据里是「北京市 / 市辖区 / 东城区」，与小程序端现有的选择器一致。
	if got := indexed[key{"北京市", "市辖区", "东城区"}]; got.province != "11" || got.district != "110101" {
		t.Fatalf("北京市东城区 = %+v", got)
	}
	if _, ok := indexed[key{"北京市", "北京市", "东城区"}]; ok {
		t.Fatal("索引里不该存在「北京市/北京市」这种拼法")
	}
}

func idsOf(rows []storeRow) []string {
	ids := make([]string, 0, len(rows))
	for _, row := range rows {
		ids = append(ids, row.ID)
	}
	return ids
}
