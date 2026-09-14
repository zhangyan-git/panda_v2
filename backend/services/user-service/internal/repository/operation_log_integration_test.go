package repository

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
)

// TestOperationLogTargetFilterIsolatesOneDevice 验证「只看某一台设备的操作日志」
// 这条查询在真库上确实只捞出那一台设备的行。
//
// 这个用例存在的直接原因是 operationLogsWhere 这一轮从 10 个占位符变成 12 个，
// 而 LIMIT/OFFSET 也跟着从 $9/$10 挪到了 $11/$12。占位符错位不一定会报语法错误：
// 有时是参数类型不匹配（一个 500），有时它照样能跑——只是把筛选值当成了偏移量，
// 于是第一页反复返回同一批行。接口层的测试用的是仓库桩，照不出这类问题。
//
// 设备详情页的「操作日志」那一屏就发 TargetType=device + TargetID=<设备 id>。
// 参数被吞掉的症状是「这台设备的操作记录」里混着别的设备的操作，比一张空表更难发现。
func TestOperationLogTargetFilterIsolatesOneDevice(t *testing.T) {
	pool := integrationPool(t)
	ctx := context.Background()
	repo := NewAdminOperationLogRepository(pool)
	prefix := "it-pg-" + uuid.NewString()[:8] + "-"

	deviceA, deviceB := uuid.NewString(), uuid.NewString()

	// occurred_at 用固定时间而不是 NOW()：断言「最近的排在前面」需要一个确定的次序。
	insertLog := func(targetType, targetID, targetName string, seconds int) string {
		t.Helper()
		id := uuid.NewString()
		occurredAt := time.Date(2020, 1, 1, 0, 0, seconds, 0, time.UTC)
		if _, err := pool.Exec(ctx, `
			INSERT INTO admin_operation_logs (
				id, module, action, operation, target_type, target_id, target_name, result, occurred_at
			) VALUES ($1, 'coffee_machines', 'update', $2, $3, NULLIF($4, '')::uuid, $5, 'success', $6)`,
			id, prefix+"改设备", targetType, targetID, targetName, occurredAt); err != nil {
			t.Fatalf("插入日志夹具: %v", err)
		}
		return id
	}

	idsA := []string{
		insertLog("device", deviceA, prefix+"设备A", 1),
		insertLog("device", deviceA, prefix+"设备A", 2),
		insertLog("device", deviceA, prefix+"设备A", 3),
	}
	idB := insertLog("device", deviceB, prefix+"设备B", 4)
	// 同一个 target_id、不同的目标类型：本表对目标对象没有外键（目标删了日志也要留着），
	// 两个类型共用一段 uuid 空间是可能的。这条夹具钉住的是「两个条件是与」——
	// 只看 target_id 的话，它会被带进设备 A 的详情页。
	idMerchant := insertLog("merchant", deviceA, prefix+"商户", 5)

	allIDs := append(append([]string{}, idsA...), idB, idMerchant)
	t.Cleanup(func() {
		if _, err := pool.Exec(context.Background(),
			`DELETE FROM admin_operation_logs WHERE id = ANY($1::uuid[])`, allIDs); err != nil {
			t.Errorf("清理日志夹具: %v", err)
		}
	})

	filterA := OperationLogFilter{TargetType: "device", TargetID: deviceA}
	total, err := repo.Count(ctx, filterA)
	if err != nil {
		t.Fatalf("Count 出错: %v", err)
	}
	if total != 3 {
		t.Fatalf("设备 A 的日志应为 3 条, got %d", total)
	}

	rows, err := repo.FindPage(ctx, filterA, 10, 0)
	if err != nil {
		t.Fatalf("FindPage 出错: %v", err)
	}
	if len(rows) != 3 {
		t.Fatalf("设备 A 的日志应查出 3 条, got %d", len(rows))
	}
	// 最近的在最前（同一次批量操作里的几条时间戳常常相同，id 是决胜位）。
	for i, wantID := range []string{idsA[2], idsA[1], idsA[0]} {
		if rows[i].ID != wantID {
			t.Fatalf("第 %d 条 = %s, 期望 %s（应按 occurred_at 倒序）", i, rows[i].ID, wantID)
		}
	}
	// SELECT 列表与筛选条件是两处独立的改动，扫列错位时 id 照样是个 uuid、不会报错。
	if rows[0].TargetType != "device" || rows[0].TargetID != deviceA ||
		rows[0].TargetName != prefix+"设备A" {
		t.Fatalf("目标字段没对上: %+v", rows[0])
	}

	// LIMIT/OFFSET 这一轮是从 $9/$10 挪到 $11/$12 的：挪错一位时第一页会反复
	// 返回同一批行，或者偏移量被当成 uuid 去比（参数类型不匹配 → 直接报错）。
	firstPage, err := repo.FindPage(ctx, filterA, 2, 0)
	if err != nil {
		t.Fatalf("第 1 页出错: %v", err)
	}
	secondPage, err := repo.FindPage(ctx, filterA, 2, 2)
	if err != nil {
		t.Fatalf("第 2 页出错: %v", err)
	}
	thirdPage, err := repo.FindPage(ctx, filterA, 2, 4)
	if err != nil {
		t.Fatalf("第 3 页出错: %v", err)
	}
	if len(firstPage) != 2 || len(secondPage) != 1 || len(thirdPage) != 0 {
		t.Fatalf("逐页条数 = %d/%d/%d, 期望 2/1/0", len(firstPage), len(secondPage), len(thirdPage))
	}
	if firstPage[0].ID == secondPage[0].ID {
		t.Fatal("相邻两页返回了同一行, OFFSET 没生效")
	}

	// 只给 TargetID（不带类型）必须把那条 merchant 行也算进来：4 = 3 条设备 + 1 条商户。
	// 它一次证明两件事——空 TargetType 的短路仍然有效，以及上面那个 3 确实是被
	// 目标类型这一条挡掉的。
	if n, err := repo.Count(ctx, OperationLogFilter{TargetID: deviceA}); err != nil {
		t.Fatalf("Count 出错: %v", err)
	} else if n != 4 {
		t.Fatalf("只按 target_id 筛应为 4 条, got %d", n)
	}
	// 只给 TargetType 则退化成「所有设备上的操作」，夹具的 4 条设备行都在里面。
	if n, err := repo.Count(ctx, OperationLogFilter{TargetType: "device"}); err != nil {
		t.Fatalf("Count 出错: %v", err)
	} else if n < 4 {
		t.Fatalf("按设备类型筛至少应有夹具的 4 条, got %d", n)
	}
	if n, err := repo.Count(ctx, OperationLogFilter{TargetType: "merchant", TargetID: deviceA}); err != nil {
		t.Fatalf("Count 出错: %v", err)
	} else if n != 1 {
		t.Fatalf("商户类型 + 该 id 应为 1 条, got %d", n)
	}
	if n, err := repo.Count(ctx, OperationLogFilter{TargetType: "device", TargetID: uuid.NewString()}); err != nil {
		t.Fatalf("Count 出错: %v", err)
	} else if n != 0 {
		t.Fatalf("不存在的目标 id 应为 0 条, got %d", n)
	}

	// 空筛选是后台日志页默认打开就走的那条路：$1..$10 全部落进「参数为空则恒真」的
	// 短路，其中 $10 那半边靠的是空串先被 NULLIF 变成 NULL（uuid 与 NULL 比较求值为
	// NULL、不为真）。短路一旦失效，症状是所有日志都查不出来，而不是报错。
	all, err := repo.Count(ctx, OperationLogFilter{})
	if err != nil {
		t.Fatalf("Count 出错: %v", err)
	}
	if all < int64(len(allIDs)) {
		t.Fatalf("空筛选的总数至少应含夹具的 %d 条, got %d", len(allIDs), all)
	}
	page, err := repo.FindPage(ctx, OperationLogFilter{}, 5, 0)
	if err != nil {
		t.Fatalf("空筛选 FindPage 出错: %v", err)
	}
	if len(page) != 5 {
		t.Fatalf("空筛选第一页应为 5 条, got %d", len(page))
	}
	for _, row := range page {
		if row.ID == "" {
			t.Fatalf("扫出了空 id, SELECT 列表与 Scan 没对齐: %+v", row)
		}
	}
	// 计数与翻页必须同源（共用 operationLogsWhere）：分叉的症状是第 2 页开始
	// 重复或跳行，而第一页看起来一切正常。
	next, err := repo.FindPage(ctx, OperationLogFilter{}, 5, 5)
	if err != nil {
		t.Fatalf("空筛选第 2 页出错: %v", err)
	}
	seen := map[string]bool{}
	for _, row := range page {
		seen[row.ID] = true
	}
	for _, row := range next {
		if seen[row.ID] {
			t.Fatalf("id %s 在两页里重复出现", row.ID)
		}
	}
}
