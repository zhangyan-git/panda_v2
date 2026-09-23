package repository

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/panda-dev/panda-v2/backend/platform/api"
	"github.com/panda-dev/panda-v2/backend/services/lottery-service/internal/dto"
	"github.com/panda-dev/panda-v2/backend/services/lottery-service/internal/model"
)

// 这个文件是各张列表的**读路径**：只读、分页、带 JOIN 的富行。
//
// 写路径读回自己刚写的那一行走各聚合文件里的 readX（同一个事务、单表）。两条路用的是同一份
// 列清单，靠 qualify 拼前缀，不各抄一遍——抄一遍的后果是「列表页和详情页上同一条记录字段
// 不一样」，而那种 bug 只有在有人对着两个页面对数时才会被发现。

// qualify 给一份列清单里的每一项加上表别名前缀。
//
// 让同一份 xxxColumns 既能用于单表查询（写路径读回刚写的那一行），也能用于带 JOIN 的
// 列表查询。**前提：清单里每一项都是「列名」或「列名::类型」**——本包所有的 xxxColumns
// 都是这个形状，没有函数调用、没有嵌套逗号。往里加一个带括号的表达式会让它拼出一句语法
// 错误的 SQL，那会在第一次跑这条查询时当场炸掉，不会静默出错。
func qualify(alias, columns string) string {
	parts := strings.Split(columns, ",")
	for i, part := range parts {
		parts[i] = alias + "." + strings.TrimSpace(part)
	}
	return strings.Join(parts, ", ")
}

// pageClause 拼出 LIMIT / OFFSET 片段并把两个参数追加到 args 后面。
//
// 占位符号必须接在筛选条件后面，所以只能由这里算——写死 $5/$6 会在筛选条件多一个的时候
// 静默拿到错误的数（或者直接报参数越界）。
func pageClause(args []any, page, pageSize int) ([]any, string) {
	if pageSize <= 0 {
		pageSize = dto.DefaultPageSize
	}
	full := append(append([]any{}, args...), pageSize, api.PageOffset(page, pageSize))
	return full, fmt.Sprintf(" LIMIT $%d OFFSET $%d", len(args)+1, len(args)+2)
}

// whereClause 把 (条件, 值) 累积器拼成 WHERE 子句。
//
// 三个筛选字段以上的列表各写一遍这段是重复的，而重复的那一段里有一个很容易写错的细节：
// 占位符序号必须与 args 的下标同步。共用一个累积器就不会有第二个地方算错。
type whereClause struct {
	conds []string
	args  []any
}

func (w *whereClause) add(clause string, value any) {
	w.args = append(w.args, value)
	w.conds = append(w.conds, fmt.Sprintf(clause, len(w.args)))
}

func (w *whereClause) sql() string {
	if len(w.conds) == 0 {
		return ""
	}
	return " WHERE " + strings.Join(w.conds, " AND ")
}

// ============================================================
// 开通
// ============================================================

// ActivationListRow 是开通列表的一行：记录本身 + 列表页要显示的那几个派生数。
//
// 「默认活动」与「在跑的期次」由**子查询**取，而不是在 Go 里再跑两条查询：一页 200 家
// 门店就是 400 次往返，而这几条子查询各自都走得到索引。
type ActivationListRow struct {
	Activation *model.Activation
	// LocationName 是**读出来之后**由 service 层向商户域解析填上的，不是本表的列
	// （见 migrations/lottery）。仓储只管把 LocationID 带出来。
	LocationName        string
	DefaultCampaignID   string
	DefaultCampaignName string
	CampaignCount       int
	LiveRoundID         string
	LiveRoundNo         string
	LiveRoundSize       int32
	LiveRoundDone       int32
}

// activationViewExtras 是列表要的派生列。
//
// 子查询只引用外层那一张表（alias a），所以外层的基列可以不加前缀地拼进来——这也是这里
// 用子查询而不是 JOIN 的原因：JOIN 会让 `id`、`created_at` 这些名字在 SELECT 列表里变歧义，
// 于是 qualify 就得给每个子查询里的外层引用也用上，读起来反而更绕。
const activationViewExtras = `,
	COALESCE((SELECT d.id::text FROM lottery_campaigns d
		WHERE d.activation_id = a.id AND d.is_default), '') AS default_campaign_id,
	COALESCE((SELECT d.name FROM lottery_campaigns d
		WHERE d.activation_id = a.id AND d.is_default), '') AS default_campaign_name,
	(SELECT COUNT(*) FROM lottery_campaigns c WHERE c.activation_id = a.id) AS campaign_count,
	COALESCE((SELECT r.id::text FROM lottery_rounds r
		JOIN lottery_campaigns d ON d.id = r.campaign_id
		WHERE d.activation_id = a.id AND d.is_default AND r.status IN ('open','closed')), '') AS live_round_id,
	COALESCE((SELECT r.round_no FROM lottery_rounds r
		JOIN lottery_campaigns d ON d.id = r.campaign_id
		WHERE d.activation_id = a.id AND d.is_default AND r.status IN ('open','closed')), '') AS live_round_no,
	COALESCE((SELECT r.participant_target FROM lottery_rounds r
		JOIN lottery_campaigns d ON d.id = r.campaign_id
		WHERE d.activation_id = a.id AND d.is_default AND r.status IN ('open','closed')), 0) AS live_round_size,
	COALESCE((SELECT r.participant_count FROM lottery_rounds r
		JOIN lottery_campaigns d ON d.id = r.campaign_id
		WHERE d.activation_id = a.id AND d.is_default AND r.status IN ('open','closed')), 0) AS live_round_done`

// scanActivationListRow 一次扫完整行。
//
// 基列与派生列**必须在同一个 Scan 里**：database/sql 与 pgx 的 Row.Scan 是「这一行」的
// 一次性消费，先扫基列再扫派生列不是「接着扫」，而是对同一行第二次取值——第二次会拿到
// 从第一列开始的值，或者直接报列数不匹配。
func scanActivationListRow(row scanner) (*ActivationListRow, error) {
	item := &ActivationListRow{Activation: &model.Activation{}}
	a := item.Activation
	err := row.Scan(&a.ID, &a.LocationID, &a.Status, &a.Remark,
		&a.ActivatedBy, &a.ActivatedAt, &a.DeactivatedAt, &a.CreatedAt, &a.UpdatedAt,
		&item.DefaultCampaignID, &item.DefaultCampaignName, &item.CampaignCount,
		&item.LiveRoundID, &item.LiveRoundNo, &item.LiveRoundSize, &item.LiveRoundDone)
	if err != nil {
		return nil, err
	}
	return item, nil
}

// ListActivations 按筛选条件分页读开通记录，返回 (当页, 总数)。
//
// 排序按 created_at DESC, id：后台按「最近开通的店」看，同一毫秒的两行也有稳定顺序。
func (r *PostgresRepository) ListActivations(ctx context.Context, q dto.ActivationQuery) ([]*ActivationListRow, int, error) {
	var w whereClause
	if q.LocationID != "" {
		w.add("a.location_id = $%d", q.LocationID)
	}
	if q.Status != "" {
		w.add("a.status = $%d", q.Status)
	}
	// 按门店名搜这条路 2026-09-15 去掉了：名字不落库（migrations/lottery），而本表的
	// 其他列里没有任何一列能表达「门店名含某几个字」。后台的筛选换成了从门店下拉里选一家
	// （传 locationId，走上面那条等值比较），所以这里没有留下一个半功能的模糊搜。

	var total int
	if err := r.pool.QueryRow(ctx, `SELECT COUNT(*) FROM lottery_activations a`+w.sql(), w.args...).Scan(&total); err != nil {
		return nil, 0, err
	}

	page, limit := pageClause(w.args, q.Page, q.PageSize)
	rows, err := r.pool.Query(ctx, `SELECT `+qualify("a", activationColumns)+activationViewExtras+
		` FROM lottery_activations a`+w.sql()+` ORDER BY a.created_at DESC, a.id`+limit, page...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()

	items := make([]*ActivationListRow, 0, q.PageSize)
	for rows.Next() {
		item, err := scanActivationListRow(rows)
		if err != nil {
			return nil, 0, err
		}
		items = append(items, item)
	}
	return items, total, rows.Err()
}

// GetActivationView 读单条开通的富行（详情页与「开通完返回的那一份」用它）。
//
// 没有这一行时返回 ErrActivationNotFound，而不是 (nil, nil)：详情页的 404 与「开通记录
// 存在但没有任何活动」是两件事，前者要回 404，后者要正常渲染一个空的活动区。
func (r *PostgresRepository) GetActivationView(ctx context.Context, id string) (*ActivationListRow, error) {
	row := r.pool.QueryRow(ctx, `SELECT `+qualify("a", activationColumns)+activationViewExtras+
		` FROM lottery_activations a WHERE a.id = $1`, id)
	item, err := scanActivationListRow(row)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrActivationNotFound
		}
		return nil, err
	}
	return item, nil
}

// ============================================================
// 活动
// ============================================================

// CampaignListRow 是活动列表的一行。
type CampaignListRow struct {
	Campaign *model.Campaign
	// 门店来自开通记录：活动挂在哪家店完全由 activation_id 决定（见 migrations/lottery
	// 为什么不用账号范围那种多态指针）。
	LocationID string
	// LocationName 与 ActivationListRow 的同名字段一样：**不是本表的列**，由 service 层
	// 拿着上面的 LocationID 向商户域批量解析后填上（见 migrations/lottery）。
	LocationName string
	// 这里原先还有一个 prizeTotalQuantity = SUM(prizes.quantity)，即「下一期的 winner_count」。
	// 名额恒为 1 之后它是一份恒等于 1 的第二事实（期次上那个冻结的 winnerCount 才是真的），
	// 2026-09-15 连同列表页那一列一起删了。
	RoundCount    int
	LiveRoundID   string
	LiveRoundNo   string
	LiveRoundSize int32
	LiveRoundDone int32
}

const campaignViewExtras = `,
	a.location_id::text AS location_id,
	(SELECT COUNT(*) FROM lottery_rounds r WHERE r.campaign_id = c.id) AS round_count,
	COALESCE(live.id::text, '') AS live_round_id,
	COALESCE(live.round_no, '') AS live_round_no,
	COALESCE(live.participant_target, 0) AS live_round_size,
	COALESCE(live.participant_count, 0) AS live_round_done`

// campaignViewJoin 里那个 LEFT JOIN 是安全的：lottery_rounds 上有一条
// lottery_rounds_one_live_per_campaign 的部分唯一索引（status IN ('open','closed')），
// 它保证最多只匹配到一行。
const campaignViewJoin = ` FROM lottery_campaigns c
	JOIN lottery_activations a ON a.id = c.activation_id
	LEFT JOIN lottery_rounds live ON live.campaign_id = c.id AND live.status IN ('open','closed')`

func scanCampaignListRow(row scanner) (*CampaignListRow, error) {
	item := &CampaignListRow{Campaign: &model.Campaign{}}
	c := item.Campaign
	err := row.Scan(&c.ID, &c.ActivationID, &c.MachineID, &c.Code, &c.Name,
		&c.ParticipantTarget, &c.Description, &c.IsDefault, &c.Status,
		&c.CreatedBy, &c.UpdatedBy, &c.CreatedAt, &c.UpdatedAt,
		&item.LocationID,
		&item.RoundCount, &item.LiveRoundID, &item.LiveRoundNo,
		&item.LiveRoundSize, &item.LiveRoundDone)
	if err != nil {
		return nil, err
	}
	return item, nil
}

// ListCampaigns 按筛选条件分页读活动，返回 (当页, 总数)。
//
// **不带奖池明细**：一页 20 个活动、每个 5 个奖品，列表就成了奖池查询。列表只带名额总数
// （运营要看的那个数），明细在详情接口与 ListPrizes 里。
func (r *PostgresRepository) ListCampaigns(ctx context.Context, q dto.CampaignQuery) ([]*CampaignListRow, int, error) {
	var w whereClause
	if q.ActivationID != "" {
		w.add("c.activation_id = $%d", q.ActivationID)
	}
	if q.LocationID != "" {
		w.add("a.location_id = $%d", q.LocationID)
	}
	if q.MachineID != "" {
		w.add("c.machine_id = $%d", q.MachineID)
	}
	if q.Status != "" {
		w.add("c.status = $%d", q.Status)
	}
	if q.Name != "" {
		w.add("c.name ILIKE '%%' || $%d || '%%'", q.Name)
	}

	var total int
	if err := r.pool.QueryRow(ctx, `SELECT COUNT(*) FROM lottery_campaigns c
		JOIN lottery_activations a ON a.id = c.activation_id`+w.sql(), w.args...).Scan(&total); err != nil {
		return nil, 0, err
	}

	page, limit := pageClause(w.args, q.Page, q.PageSize)
	rows, err := r.pool.Query(ctx, `SELECT `+qualify("c", campaignColumns)+campaignViewExtras+
		campaignViewJoin+w.sql()+` ORDER BY c.created_at DESC, c.id`+limit, page...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()

	items := make([]*CampaignListRow, 0, q.PageSize)
	for rows.Next() {
		item, err := scanCampaignListRow(rows)
		if err != nil {
			return nil, 0, err
		}
		items = append(items, item)
	}
	return items, total, rows.Err()
}

// GetCampaignView 读单个活动的富行（详情页用它）。
func (r *PostgresRepository) GetCampaignView(ctx context.Context, id string) (*CampaignListRow, error) {
	row := r.pool.QueryRow(ctx, `SELECT `+qualify("c", campaignColumns)+campaignViewExtras+
		campaignViewJoin+` WHERE c.id = $1`, id)
	item, err := scanCampaignListRow(row)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrCampaignNotFound
		}
		return nil, err
	}
	return item, nil
}

// ============================================================
// 期次
// ============================================================

// RoundListRow 是期次列表的一行。
type RoundListRow struct {
	Round *model.Round
	// 活动的短名与名字跟着期次一起回：后台的期次列表是按活动看的，少了它们每一行都要回头
	// 查一次活动。
	CampaignCode string
	CampaignName string
	// 开奖记录，未开奖时全为空。ActualWinnerCount 是**实际**抽出的中奖人数，不是开期时冻结
	// 的名额数——参与数少于名额时两者不一样，而运营要看的是前者。
	DrawID            string
	DrawMode          string
	DrawTrigger       string
	ActualWinnerCount int32
}

const roundViewExtras = `,
	c.code AS campaign_code,
	c.name AS campaign_name,
	COALESCE(d.id::text, '') AS draw_id,
	COALESCE(d.mode, '') AS draw_mode,
	COALESCE(d.trigger, '') AS draw_trigger,
	COALESCE(d.winner_count, 0) AS actual_winner_count`

// roundViewJoin 里 lottery_draws 上的 LEFT JOIN 是安全的：lottery_draws_round_unique 保证
// 一期最多一条开奖记录。
const roundViewJoin = ` FROM lottery_rounds r
	JOIN lottery_campaigns c ON c.id = r.campaign_id
	LEFT JOIN lottery_draws d ON d.round_id = r.id`

func scanRoundListRow(row scanner) (*RoundListRow, error) {
	item := &RoundListRow{Round: &model.Round{}}
	round := item.Round
	err := row.Scan(&round.ID, &round.CampaignID, &round.Seq, &round.RoundNo, &round.Status,
		&round.ParticipantTarget, &round.ParticipantCount, &round.WinnerCount,
		&round.DrawnAt, &round.CancelledAt,
		&round.CancelReason, &round.CancelledBy, &round.CreatedAt, &round.UpdatedAt,
		&item.CampaignCode, &item.CampaignName, &item.DrawID,
		&item.DrawMode, &item.DrawTrigger, &item.ActualWinnerCount)
	if err != nil {
		return nil, err
	}
	return item, nil
}

// ListRounds 按筛选条件分页读期次，返回 (当页, 总数)。
func (r *PostgresRepository) ListRounds(ctx context.Context, q dto.RoundQuery) ([]*RoundListRow, int, error) {
	var w whereClause
	if q.CampaignID != "" {
		w.add("r.campaign_id = $%d", q.CampaignID)
	}
	if q.Status != "" {
		w.add("r.status = $%d", q.Status)
	}

	var total int
	if err := r.pool.QueryRow(ctx, `SELECT COUNT(*) FROM lottery_rounds r`+w.sql(), w.args...).Scan(&total); err != nil {
		return nil, 0, err
	}

	page, limit := pageClause(w.args, q.Page, q.PageSize)
	rows, err := r.pool.Query(ctx, `SELECT `+qualify("r", roundColumns)+roundViewExtras+
		roundViewJoin+w.sql()+` ORDER BY r.created_at DESC, r.id`+limit, page...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()

	items := make([]*RoundListRow, 0, q.PageSize)
	for rows.Next() {
		item, err := scanRoundListRow(rows)
		if err != nil {
			return nil, 0, err
		}
		items = append(items, item)
	}
	return items, total, rows.Err()
}

// GetRoundView 读单期的富行（期次详情与「开完奖返回的那一份」用它）。
func (r *PostgresRepository) GetRoundView(ctx context.Context, id string) (*RoundListRow, error) {
	row := r.pool.QueryRow(ctx, `SELECT `+qualify("r", roundColumns)+roundViewExtras+
		roundViewJoin+` WHERE r.id = $1`, id)
	item, err := scanRoundListRow(row)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrRoundNotFound
		}
		return nil, err
	}
	return item, nil
}

// ============================================================
// 参与
// ============================================================

// ParticipationListRow 是参与列表的一行。
type ParticipationListRow struct {
	Participation *model.Participation
	// 这一笔有没有中奖。按 participation_id 反查中奖记录——一期里一条参与只能中一次
	// （lottery_wins_participation_unique），所以这个 LEFT JOIN 不会让行数变多。
	WinID      string
	WinClaimNo string
	PrizeName  string
}

const participationViewExtras = `,
	COALESCE(w.id::text, '') AS win_id,
	COALESCE(w.claim_no, '') AS win_claim_no,
	COALESCE(w.current_prize_name, '') AS prize_name`

const participationViewJoin = ` FROM lottery_participations p
	LEFT JOIN lottery_wins w ON w.participation_id = p.id`

func scanParticipationListRow(row scanner) (*ParticipationListRow, error) {
	item := &ParticipationListRow{Participation: &model.Participation{}}
	p := item.Participation
	err := row.Scan(&p.ID, &p.RoundID, &p.CampaignID, &p.CampaignName, &p.RoundNo, &p.UserID,
		&p.SourceOrderID, &p.SourceOrderNo, &p.SourceMachineID, &p.SourceLocationID,
		&p.Cost, &p.Status, &p.FailureCode, &p.FortuneEntryID, &p.ReverseEntryID,
		&p.Attempts, &p.LastError, &p.IdempotencyKey, &p.CreatedAt, &p.ConfirmedAt,
		&item.WinID, &item.WinClaimNo, &item.PrizeName)
	if err != nil {
		return nil, err
	}
	return item, nil
}

// ListParticipations 按筛选条件分页读参与记录，返回 (当页, 总数)。
//
// 小程序那条路只允许按自己的 user_id 查——归属由令牌决定，不接受查询串里的 userId
// （见 routes/miniapp.go）。
func (r *PostgresRepository) ListParticipations(ctx context.Context, q dto.ParticipationQuery) ([]*ParticipationListRow, int, error) {
	var w whereClause
	if q.RoundID != "" {
		w.add("p.round_id = $%d", q.RoundID)
	}
	if q.CampaignID != "" {
		w.add("p.campaign_id = $%d", q.CampaignID)
	}
	if q.UserID != "" {
		w.add("p.user_id = $%d", q.UserID)
	}
	if q.Status != "" {
		w.add("p.status = $%d", q.Status)
	}

	// 计数**不带那个 LEFT JOIN**：一次参与最多中一次奖，两边都是 1、行数不变，但把 join
	// 放进 COUNT 里会让「不会变多」变成一个隐含前提——将来左侧多一个一对多的关联就会静默
	// 把总数算错。计数只数参与表自己。
	var total int
	if err := r.pool.QueryRow(ctx, `SELECT COUNT(*) FROM lottery_participations p`+w.sql(), w.args...).Scan(&total); err != nil {
		return nil, 0, err
	}

	page, limit := pageClause(w.args, q.Page, q.PageSize)
	rows, err := r.pool.Query(ctx, `SELECT `+qualify("p", participationColumns)+participationViewExtras+
		participationViewJoin+w.sql()+` ORDER BY p.created_at DESC, p.id`+limit, page...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()

	items := make([]*ParticipationListRow, 0, q.PageSize)
	for rows.Next() {
		item, err := scanParticipationListRow(rows)
		if err != nil {
			return nil, 0, err
		}
		items = append(items, item)
	}
	return items, total, rows.Err()
}

// ============================================================
// 中奖
// ============================================================

// ListWins 按筛选条件分页读中奖记录，返回 (当页, 总数)。
//
// 中奖记录的响应形状与表结构一一对应，所以这里直接回模型，不再包一层富行。
func (r *PostgresRepository) ListWins(ctx context.Context, q dto.WinQuery) ([]*model.Win, int, error) {
	var w whereClause
	if q.RoundID != "" {
		w.add("round_id = $%d", q.RoundID)
	}
	if q.CampaignID != "" {
		w.add("campaign_id = $%d", q.CampaignID)
	}
	if q.UserID != "" {
		w.add("user_id = $%d", q.UserID)
	}
	if q.Status != "" {
		w.add("status = $%d", q.Status)
	}
	if q.ClaimNo != "" {
		w.add("claim_no = $%d", q.ClaimNo)
	}

	var total int
	if err := r.pool.QueryRow(ctx, `SELECT COUNT(*) FROM lottery_wins`+w.sql(), w.args...).Scan(&total); err != nil {
		return nil, 0, err
	}

	page, limit := pageClause(w.args, q.Page, q.PageSize)
	rows, err := r.pool.Query(ctx, `SELECT `+winColumns+` FROM lottery_wins`+w.sql()+
		` ORDER BY created_at DESC, id`+limit, page...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()

	wins := make([]*model.Win, 0, q.PageSize)
	for rows.Next() {
		win, err := scanWin(rows)
		if err != nil {
			return nil, 0, err
		}
		wins = append(wins, win)
	}
	return wins, total, rows.Err()
}

// GetWin 按 id 读一条中奖记录。
func (r *PostgresRepository) GetWin(ctx context.Context, id string) (*model.Win, error) {
	win, err := scanWin(r.pool.QueryRow(ctx, `SELECT `+winColumns+`
		FROM lottery_wins WHERE id=$1`, id))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrWinNotFound
		}
		return nil, err
	}
	return win, nil
}

// ListWinEvents 读一条中奖记录的流水，按时间正序。
//
// 正序而不是倒序：流水是**读故事**的（先中奖、再领取、再核销），详情页从上往下讲一遍。
// 倒序的流水要让人从下往上读，那不是任何人的习惯。
func (r *PostgresRepository) ListWinEvents(ctx context.Context, winID string) ([]*model.WinEvent, error) {
	rows, err := r.pool.Query(ctx, `SELECT id::text, win_id::text, event_type, from_status,
		to_status, actor_type, actor_id::text, actor_name, reason, metadata, created_at
		FROM lottery_win_events WHERE win_id=$1 ORDER BY created_at, id`, winID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	events := make([]*model.WinEvent, 0, 4)
	for rows.Next() {
		event := &model.WinEvent{}
		if err := rows.Scan(&event.ID, &event.WinID, &event.EventType, &event.FromStatus,
			&event.ToStatus, &event.ActorType, &event.ActorID, &event.ActorName,
			&event.Reason, &event.Metadata, &event.CreatedAt); err != nil {
			return nil, err
		}
		events = append(events, event)
	}
	return events, rows.Err()
}

// CountWinsByUser 数一个用户的中奖记录：总数与待领取数。
//
// 两个数一次查询：抽奖中心顶部那两个数字永远是一起显示的，分两次查会在两次之间出现一个
// 「总数 5、待领取 6」的瞬间。
func (r *PostgresRepository) CountWinsByUser(ctx context.Context, userID string) (total int, pending int, err error) {
	err = r.pool.QueryRow(ctx, `SELECT COUNT(*),
		COUNT(*) FILTER (WHERE status='pending') FROM lottery_wins WHERE user_id=$1`, userID).
		Scan(&total, &pending)
	return total, pending, err
}
