// Command backfill-region 给历史门店补区划编码（province_code / city_code /
// district_code）。
//
// 一次性工具，不是服务的一部分：它读 stores 里「省/市/区」三元组齐全的行，在区划
// 数据里查出编码回写，匹配不上的留空并计数。默认只报告不写库，加 -apply 才落盘。
//
// 数据来自 element-china-area-data —— 与前端级联选择器同一个源。回填工具是 Go，
// 读不了 npm 包，所以先用前端仓库的脚本导出一份 JSON：
//
//	pnpm --filter @panda-v2/ui regions:export /tmp/regions.json
//	go run ./cmd/backfill-region -regions /tmp/regions.json          # 预演
//	go run ./cmd/backfill-region -regions /tmp/regions.json -apply   # 落库
//
// 库连接取 MERCHANT_DATABASE_URL，回落到 DATABASE_URL。刻意不走 platform/config：
// 那会为了连库顺带要求 JWT_SECRET、Redis、内部令牌等等全都配好，而一个一次性工具
// 应当只依赖它真正需要的那一个变量。
//
// 幂等：UPDATE 带 IS DISTINCT FROM 条件，重跑一次写入 0 行。之所以要幂等，是因为
// 这里改的是历史数据，第一次跑完总会想再确认一遍。
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"
)

// regionNode 是区划数据的一层：省 → 市 → 区，value 是编码，label 是名字。
type regionNode struct {
	Value    string       `json:"value"`
	Label    string       `json:"label"`
	Children []regionNode `json:"children"`
}

// codes 是三元组对应的三个编码。
type codes struct {
	province string
	city     string
	district string
}

// key 是区划索引的键：省 + 市 + 区三个名字。
//
// 必须是三元组：全国区县有 28 组重名（实测），只用区名定位会串到别的城市去。
type key [3]string

// storeRow 是一条待回填的门店。
type storeRow struct {
	ID           string
	Province     string
	City         string
	District     string
	ProvinceCode string
	CityCode     string
	DistrictCode string
}

// update 是一次要落库的修改。
type update struct {
	ID    string
	Codes codes
}

// plan 是纯函数：给定门店与索引，算出该写哪些行、哪些行匹配不上、哪些行跳过。
//
// 三种结果分开报，因为它们要的是三种不同的动作：可回填是自动化的，未匹配要人工
// 看一眼（多半是「北京」这种自由文本），跳过则说明这条数据的区划本身就不完整。
// 混成一个「成功/失败」计数就再也分不出来了。
func plan(rows []storeRow, index map[key]codes) (updates []update, unmatched, skipped []storeRow) {
	for _, row := range rows {
		province, city, district := strings.TrimSpace(row.Province), strings.TrimSpace(row.City), strings.TrimSpace(row.District)
		if province == "" || city == "" || district == "" {
			skipped = append(skipped, row)
			continue
		}
		found, ok := index[key{province, city, district}]
		if !ok {
			unmatched = append(unmatched, row)
			continue
		}
		// 已经是目标值就不写：重跑一次必须不产生任何写入。
		if found.province == row.ProvinceCode && found.city == row.CityCode && found.district == row.DistrictCode {
			continue
		}
		updates = append(updates, update{ID: row.ID, Codes: found})
	}
	return updates, unmatched, skipped
}

// index 把区划数据摊平成「三元组 → 编码」。重名在同一个省内也允许存在，
// 键是完整三元组，所以不会互相覆盖。
func index(roots []regionNode) map[key]codes {
	indexed := map[key]codes{}
	for _, province := range roots {
		for _, city := range province.Children {
			for _, district := range city.Children {
				indexed[key{province.Label, city.Label, district.Label}] = codes{
					province: province.Value, city: city.Value, district: district.Value,
				}
			}
		}
	}
	return indexed
}

func main() {
	regionsPath := flag.String("regions", "", "区划数据 JSON（pnpm --filter @panda-v2/ui regions:export 产出），必填")
	apply := flag.Bool("apply", false, "真正写库；不加只预演并打印将要写入的行")
	flag.Parse()

	if *regionsPath == "" {
		log.Fatal("backfill-region: -regions is required")
	}
	raw, err := os.ReadFile(*regionsPath)
	if err != nil {
		log.Fatalf("backfill-region: read regions: %v", err)
	}
	var roots []regionNode
	if err := json.Unmarshal(raw, &roots); err != nil {
		log.Fatalf("backfill-region: parse regions: %v", err)
	}
	indexed := index(roots)

	ctx := context.Background()
	pool, err := pgxpool.New(ctx, databaseURL())
	if err != nil {
		log.Fatalf("backfill-region: connect: %v", err)
	}
	defer pool.Close()

	rows, err := loadStores(ctx, pool)
	if err != nil {
		log.Fatalf("backfill-region: load stores: %v", err)
	}
	updates, unmatched, skipped := plan(rows, indexed)

	fmt.Printf("区划数据：%d 个省级、%d 个区县三元组\n", len(roots), len(indexed))
	fmt.Printf("门店 %d 条；可回填 %d，未匹配 %d，区划不全跳过 %d\n",
		len(rows), len(updates), len(unmatched), len(skipped))
	reportTriples("未匹配", unmatched)
	reportTriples("区划不全", skipped)

	if !*apply {
		for i, item := range updates {
			if i == listLimit {
				fmt.Printf("  …还有 %d 条\n", len(updates)-i)
				break
			}
			fmt.Printf("  将写入 %s: %s/%s/%s\n", item.ID, item.Codes.province, item.Codes.city, item.Codes.district)
		}
		fmt.Println("预演结束，未写库（加 -apply 落盘）")
		return
	}

	written, err := writeCodes(ctx, pool, updates)
	if err != nil {
		log.Fatalf("backfill-region: write: %v", err)
	}
	fmt.Printf("写入 %d 行（%d 行已是目标值，未改动）\n", written, len(updates)-written)
}

// listLimit 限制逐条打印的行数：一次几百行的输出没人看，计数和样例才有用。
const listLimit = 20

// reportTriples 打印一组门店的区划三元组，去重后按名字排序，只列前 listLimit 条。
func reportTriples(label string, rows []storeRow) {
	if len(rows) == 0 {
		return
	}
	seen := map[string]bool{}
	var triples []string
	for _, row := range rows {
		triple := strings.Join([]string{row.Province, row.City, row.District}, "/")
		if seen[triple] {
			continue
		}
		seen[triple] = true
		triples = append(triples, triple)
	}
	sort.Strings(triples)
	fmt.Printf("%s（%d 条门店、%d 种区划）：\n", label, len(rows), len(triples))
	for i, triple := range triples {
		if i == listLimit {
			fmt.Printf("  …还有 %d 种\n", len(triples)-i)
			break
		}
		fmt.Printf("  %s\n", triple)
	}
}

// databaseURL 与 merchant-service 用的是同一对变量，顺序也一致：
// MERCHANT_DATABASE_URL 优先，单库部署时回落 DATABASE_URL。
func databaseURL() string {
	if value := strings.TrimSpace(os.Getenv("MERCHANT_DATABASE_URL")); value != "" {
		return value
	}
	if value := strings.TrimSpace(os.Getenv("DATABASE_URL")); value != "" {
		return value
	}
	log.Fatal("backfill-region: MERCHANT_DATABASE_URL (or DATABASE_URL) is required")
	return ""
}

// loadStores 读全部门店。这条表在 v2 里是单库单写方，数据量以千计，不需要分页。
func loadStores(ctx context.Context, pool *pgxpool.Pool) ([]storeRow, error) {
	rows, err := pool.Query(ctx, `
		SELECT id::text, province, city, district, province_code, city_code, district_code
		FROM stores`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var stores []storeRow
	for rows.Next() {
		var s storeRow
		if err := rows.Scan(&s.ID, &s.Province, &s.City, &s.District, &s.ProvinceCode, &s.CityCode, &s.DistrictCode); err != nil {
			return nil, err
		}
		stores = append(stores, s)
	}
	return stores, rows.Err()
}

// writeCodes 逐行写入，返回真正被改动的行数。
//
// IS DISTINCT FROM 让「重跑」是真正的 no-op，而不是把同一份值再写一遍：后者会刷新
// updated_at 之外什么都看不出来，但审计和复制日志里全是噪声。
func writeCodes(ctx context.Context, pool *pgxpool.Pool, updates []update) (int, error) {
	written := 0
	for _, item := range updates {
		tag, err := pool.Exec(ctx, `
			UPDATE stores
			SET province_code = $2::text, city_code = $3::text, district_code = $4::text
			WHERE id = $1::uuid
			  AND (province_code IS DISTINCT FROM $2::text
			       OR city_code IS DISTINCT FROM $3::text
			       OR district_code IS DISTINCT FROM $4::text)`,
			item.ID, item.Codes.province, item.Codes.city, item.Codes.district)
		if err != nil {
			return written, fmt.Errorf("store %s: %w", item.ID, err)
		}
		written += int(tag.RowsAffected())
	}
	return written, nil
}
