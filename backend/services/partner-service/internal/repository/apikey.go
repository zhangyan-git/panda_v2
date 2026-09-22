package repository

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/panda-dev/panda-v2/backend/platform/audit"
	"github.com/panda-dev/panda-v2/backend/platform/secret"
	"github.com/panda-dev/panda-v2/backend/services/partner-service/internal/model"
)

// 这个文件是**密钥**的签发、修改、启停，加上验签那条热路径上的查询（FindCredential）。
//
// # 关于明文
//
// 明文签名密钥在这个文件里**只以参数形式出现一次**：CreateAPIKey 的入参里那个
// secret.Envelope 是密文（加密封在 service 里做，见 service.issueAPIKey）。读方法一律把
// secret_sealed 读成 Envelope 原样带走，**没有任何一处解开它**——解开只发生在 ingress 验签
// 的那一刻（keyring.Open）。
//
// # 关于删除
//
// 没有 DELETE。密钥的「不再使用」是 status=disabled，不是删行：partner_call_logs 里那些历史
// 记录带着 api_key_id 与掩码，删掉密钥之后那一列就指向一个不存在的行，排查时「这是哪把钥匙」
// 只剩掩码可读（掩码本身还在，所以线索没有全丢，但少了一半）。停用是可逆的，删行不是。

// APIKeyWrite 是一把密钥的写入值。
//
// 三个 Mask 字段与 APIKey / Secret 一起由 service 在**同一个地方**产生（partnerkey.Issue）：
// 它们必须来自同一个随机串，掩码算错一位就会出现「列表里显示的掩码与手上那把钥匙对不上」，
// 而那种错在库里是查不出来的（掩码就是唯一线索）。
type APIKeyWrite struct {
	// APIKey 是公开标识（X-API-Key 的值），明文存、明文读。
	APIKey string
	// Secret 是签名密钥的**密文信封**（platform/secret）。明文在这条链路的上一层就被封掉了。
	Secret     secret.Envelope
	APIKeyMask string
	SecretMask string
	Name       string
	// ExpiresAt 为 nil 表示不过期。
	ExpiresAt *time.Time
	// IPWhitelist 为空表示不限制来源（不是全拒）。空切片与 nil 都归一成库里那个默认的 '{}'。
	IPWhitelist []string
	// RateLimitPerMinute 必须是正数（库上有 CHECK）。0 由 service 归一成默认值。
	RateLimitPerMinute int
}

// apiKeySnapshot 是一把密钥在审计里的样子。
//
// **既没有明文也没有密文**。理由有两层（与 payment-service 的 channelSnapshot 同一条）：
// 审计载荷会进身份库的 admin_operation_logs（另一个库、另一套权限、而且是要给人看的），
// 密文搬到那里等于把「换了主密钥就解不开」这件事又复制了一份；而 before/after 两份密文只要
// 差一个字节，整个 nonce 就换了，diff 里会是一条谁也读不懂的乱码，对「改坏了好回滚」一点用
// 都没有。真正有意义的信息是「这次改动动了哪几列」——掩码与白名单就是这个问题的答案。
//
// secret_mask 留着：它同时回答两件事——这把密钥有没有密钥（掩码非空），以及它对不对得上运营
// 手上的那一把。
type apiKeySnapshot struct {
	ID                 string     `json:"id"`
	PartnerID          string     `json:"partner_id"`
	Name               string     `json:"name"`
	APIKeyMask         string     `json:"api_key_mask"`
	SecretMask         string     `json:"secret_mask"`
	Status             string     `json:"status"`
	ExpiresAt          *time.Time `json:"expires_at"`
	IPWhitelist        []string   `json:"ip_whitelist"`
	RateLimitPerMinute int        `json:"rate_limit_per_minute"`
	CreatedBy          string     `json:"created_by"`

	CreatedAt time.Time `json:"-"`
	UpdatedAt time.Time `json:"-"`
}

// apiKeyColumns 是 partner_api_keys 的读取列。列顺序与扫描顺序严格一一对应，两边必须一起改。
//
// last_used_at / call_count 也读：它们是列表页上唯一能回答「这把钥匙到底有没有在用」的两列，
// 由 RecordCallLog 在写日志的同一个事务里更新（见 calllog.go）。
const apiKeyColumns = `id::text, partner_id::text, api_key, secret_sealed, api_key_mask,
	secret_mask, name, status, expires_at, ip_whitelist, rate_limit_per_minute,
	last_used_at, call_count, created_by, created_at, updated_at`

func scanAPIKey(row scanner) (*model.APIKey, error) {
	key := &model.APIKey{}
	err := row.Scan(&key.ID, &key.PartnerID, &key.APIKey, &key.Secret, &key.APIKeyMask,
		&key.SecretMask, &key.Name, &key.Status, &key.ExpiresAt, &key.IPWhitelist,
		&key.RateLimitPerMinute, &key.LastUsedAt, &key.CallCount, &key.CreatedBy,
		&key.CreatedAt, &key.UpdatedAt)
	if err != nil {
		return nil, err
	}
	return key, nil
}

// FindCredential 是验签那条热路径上的**唯一**一次查询：按 api_key 取回密钥行与它所属的合作方。
//
// 一次 JOIN 取两行而不是两次查询：这两件事必须来自同一个快照——分开查时「合作方刚被停用」
// 与「密钥刚被签发」可能落在两次查询之间，于是判定用的是两个不同时刻的事实。合成一条 SQL
// 让这个时间差消失。
//
// 查不到返回 ErrAPIKeyNotFound（装配处会把它翻成 ingress.ErrCredentialNotFound）。**这里不
// 过滤 status 与过期时间**：那些判断属于 ingress（它要按不同的原因写不同的 error_code），
// 在 SQL 里顺手过滤掉的话，「密钥被停用了」会与「密钥不存在」收敛成同一个 401，而它们的
// 排查方向完全不同。
//
// 这条查询每个请求一次，走 partner_api_keys_api_key_key 那个唯一索引。它**不加 FOR UPDATE**：
// 这是只读判定，加了行锁会让同一把密钥的并发请求互相排队（限流与 nonce 那两步本来就已经在
// Redis 上串行了）。
func (r *PostgresRepository) FindCredential(ctx context.Context, apiKey string) (*model.APIKey, *model.PartnerAccount, error) {
	row := r.pool.QueryRow(ctx, `SELECT
		k.id::text, k.partner_id::text, k.api_key, k.secret_sealed, k.api_key_mask,
		k.secret_mask, k.name, k.status, k.expires_at, k.ip_whitelist,
		k.rate_limit_per_minute, k.last_used_at, k.call_count, k.created_by,
		k.created_at, k.updated_at,
		p.id::text, p.code, p.name, p.contact_name, p.contact_phone, p.contact_email,
		p.description, p.status, p.expires_at, p.created_by, p.created_at, p.updated_at
		FROM partner_api_keys k
		JOIN partner_accounts p ON p.id = k.partner_id
		WHERE k.api_key = $1`, apiKey)

	key := &model.APIKey{}
	partner := &model.PartnerAccount{}
	err := row.Scan(
		&key.ID, &key.PartnerID, &key.APIKey, &key.Secret, &key.APIKeyMask,
		&key.SecretMask, &key.Name, &key.Status, &key.ExpiresAt, &key.IPWhitelist,
		&key.RateLimitPerMinute, &key.LastUsedAt, &key.CallCount, &key.CreatedBy,
		&key.CreatedAt, &key.UpdatedAt,
		&partner.ID, &partner.Code, &partner.Name, &partner.ContactName,
		&partner.ContactPhone, &partner.ContactEmail, &partner.Description, &partner.Status,
		&partner.ExpiresAt, &partner.CreatedBy, &partner.CreatedAt, &partner.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil, ErrAPIKeyNotFound
	}
	if err != nil {
		return nil, nil, err
	}
	return key, partner, nil
}

// ListAPIKeys 读一个合作方名下的全部密钥，按创建时间倒序。
//
// **不分页**：这把密钥是发给一家公司的一个集成用的，现实里是几把（主用、备用、换掉待停用的），
// 不是几百条。为它加一套分页参数会让前端多一个「要不要翻页」的判断，而那个判断永远只有一个
// 答案。哪天真出现一把钥匙刷出几十条的情况，那时候再加也不迟——加之前先去问为什么。
func (r *PostgresRepository) ListAPIKeys(ctx context.Context, partnerID string) ([]*model.APIKey, error) {
	rows, err := r.pool.Query(ctx, `SELECT `+apiKeyColumns+`
		FROM partner_api_keys WHERE partner_id = $1 ORDER BY created_at DESC, id DESC`, partnerID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	keys := make([]*model.APIKey, 0, 4)
	for rows.Next() {
		key, err := scanAPIKey(rows)
		if err != nil {
			return nil, err
		}
		keys = append(keys, key)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return keys, nil
}

// CreateAPIKey 签发一把密钥。
//
// 合作方存不存在不在这里先查一次：partner_id 上有外键（partner_api_keys_partner_id_fkey），
// 撞了会被 mapPGError 翻成 ErrPartnerNotFound。先查一次只会多一次往返，而结果与插入时的
// 判断之间还隔着一次并发——那时候这一行会以一个裸外键错误冒成 500。
//
// ip_whitelist 走 emptyIfNil：Go 的 nil 切片在 pgx 那边是 **NULL**，而这一列是 NOT NULL，
// 于是「不填白名单」会以一句违反非空约束的 500 收场。空数组才是「不限制」。
func (r *PostgresRepository) CreateAPIKey(ctx context.Context, partnerID string, in APIKeyWrite, createdBy string) (*model.APIKey, error) {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// 信封先编码成 JSON 文本再交给 $3::jsonb：pgx 认这个形状（见 marshalEnvelope 的说明）。
	sealed, err := marshalEnvelope(in.Secret)
	if err != nil {
		return nil, err
	}
	var id string
	err = tx.QueryRow(ctx, `INSERT INTO partner_api_keys
		(partner_id, api_key, secret_sealed, api_key_mask, secret_mask, name,
		 expires_at, ip_whitelist, rate_limit_per_minute, created_by)
		VALUES ($1, $2, $3::jsonb, $4, $5, $6, $7, $8, $9, $10)
		RETURNING id::text`,
		partnerID, in.APIKey, sealed, in.APIKeyMask, in.SecretMask,
		in.Name, in.ExpiresAt, emptyIfNil(in.IPWhitelist), in.RateLimitPerMinute,
		createdBy).Scan(&id)
	if err != nil {
		return nil, mapPGError(err)
	}

	after, err := readAPIKeySnapshot(ctx, tx, partnerID, id)
	if err != nil {
		return nil, err
	}
	if err := r.recorder.Record(ctx, tx, audit.Entry{
		Module: "partner_api_keys", Action: "create", Operation: "签发合作方密钥",
		TargetType: "partner_api_key", TargetID: id, TargetName: after.Name,
		After: audit.Snapshot(after),
	}); err != nil {
		return nil, err
	}

	created, err := scanAPIKey(tx.QueryRow(ctx, `SELECT `+apiKeyColumns+`
		FROM partner_api_keys WHERE id = $1`, id))
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return created, nil
}

// UpdateAPIKey 整份覆盖一把密钥的可改列。
//
// **改不了的两列**：api_key（公开标识，换它等于换了一把钥匙——正确做法是签发一把新的、
// 停用旧的）与 secret_sealed（签名密钥，同一件事）。所以这里没有「轮换密钥」这个操作：
// 它由两次签发/停用组成，而那是可用性上的一个明确取舍——轮换期间新旧两把都有效，运营可以
// 先让对接方切过去再停旧的那把。就地换密钥会让对接方在同一瞬间失去调用能力。
//
// 归属核对（keyId 必须属于路径上那个 partnerId）写进 WHERE：两者对不上时 RowsAffected 为 0，
// 而**没有行可改**与**改了一个不该改的行**在这条路上是同一件事——返回 ErrAPIKeyNotFound 让
// 路径参数错误表现为 404。
func (r *PostgresRepository) UpdateAPIKey(ctx context.Context, partnerID, keyID string, in APIKeyWrite) (*model.APIKey, error) {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	before, err := lockAPIKeySnapshot(ctx, tx, partnerID, keyID)
	if err != nil {
		return nil, err
	}

	if _, err := tx.Exec(ctx, `UPDATE partner_api_keys SET
		name = $3, expires_at = $4, ip_whitelist = $5, rate_limit_per_minute = $6,
		updated_at = NOW()
		WHERE partner_id = $1 AND id = $2`,
		partnerID, keyID, in.Name, in.ExpiresAt, emptyIfNil(in.IPWhitelist),
		in.RateLimitPerMinute); err != nil {
		return nil, mapPGError(err)
	}

	after, err := readAPIKeySnapshot(ctx, tx, partnerID, keyID)
	if err != nil {
		return nil, err
	}
	if err := r.recorder.Record(ctx, tx, audit.Entry{
		Module: "partner_api_keys", Action: "update", Operation: "修改合作方密钥",
		TargetType: "partner_api_key", TargetID: keyID, TargetName: after.Name,
		Before: audit.Snapshot(before), After: audit.Snapshot(after),
	}); err != nil {
		return nil, err
	}

	updated, err := scanAPIKey(tx.QueryRow(ctx, `SELECT `+apiKeyColumns+`
		FROM partner_api_keys WHERE id = $1`, keyID))
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return updated, nil
}

// SetAPIKeyStatus 启用或停用一把密钥。
//
// **停用立即生效**：验签那条路每个请求都现查这一行（没有缓存），停掉之后下一个请求就被拒。
// 这一条是这套密钥体系里最有用的一个能力——怀疑泄露时，停用比「通知对接方改代码」快得多。
//
// 与 UpdateAPIKey 分成两条路，理由与 payment 那边相同：它们的破坏半径不同（改备注只影响
// 这一页，停用会让一个集成当场断掉），审计里也因此分成 update / update_status 两种动作。
func (r *PostgresRepository) SetAPIKeyStatus(ctx context.Context, partnerID, keyID, status string) (*model.APIKey, error) {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	before, err := lockAPIKeySnapshot(ctx, tx, partnerID, keyID)
	if err != nil {
		return nil, err
	}
	result, err := tx.Exec(ctx, `UPDATE partner_api_keys SET status = $3, updated_at = NOW()
		WHERE partner_id = $1 AND id = $2`, partnerID, keyID, status)
	if err != nil {
		return nil, mapPGError(err)
	}
	if result.RowsAffected() == 0 {
		return nil, ErrAPIKeyNotFound
	}
	after, err := readAPIKeySnapshot(ctx, tx, partnerID, keyID)
	if err != nil {
		return nil, err
	}
	if err := r.recorder.Record(ctx, tx, audit.Entry{
		Module: "partner_api_keys", Action: "update_status", Operation: apiKeyStatusOperation(status),
		TargetType: "partner_api_key", TargetID: keyID, TargetName: after.Name,
		Before: audit.Snapshot(before), After: audit.Snapshot(after),
	}); err != nil {
		return nil, err
	}

	updated, err := scanAPIKey(tx.QueryRow(ctx, `SELECT `+apiKeyColumns+`
		FROM partner_api_keys WHERE id = $1`, keyID))
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return updated, nil
}

// apiKeySnapshotColumns 是**审计快照**的读取列，与 apiKeyColumns 是两份。
//
// 它必须单独一份：快照的结构体上刻意没有 api_key / secret_sealed / last_used_at / call_count
// 四个字段（前三者的理由见 apiKeySnapshot 的说明，last_used_at 与 call_count 是「用没用过」的
// 运行统计，不是「谁改了什么」），而 apiKeyColumns 有 16 列。共用一份的代价不是编译错误——
// 是 pgx 在运行时报「number of field descriptions must equal number of destinations, got 16
// and 12」，而那一声只在**第一次真的签发密钥**时才响：用假仓储的单元测试与只读路径都碰不到
// 它。改成两份之后，这条 SQL 少读的那四列永远读不到，快照里也不可能混进密文。
//
// 列顺序与下面的 Scan 顺序严格一一对应，两边必须一起改。
const apiKeySnapshotColumns = `id::text, partner_id::text, api_key_mask, secret_mask,
	name, status, expires_at, ip_whitelist, rate_limit_per_minute, created_by,
	created_at, updated_at`

// lockAPIKeySnapshot 锁住一行并读回它的快照。归属条件在 WHERE 里（见 UpdateAPIKey）。
func lockAPIKeySnapshot(ctx context.Context, tx pgx.Tx, partnerID, keyID string) (*apiKeySnapshot, error) {
	snapshot := &apiKeySnapshot{}
	err := tx.QueryRow(ctx, `SELECT `+apiKeySnapshotColumns+`
		FROM partner_api_keys WHERE partner_id = $1 AND id = $2 FOR UPDATE`,
		partnerID, keyID).Scan(
		&snapshot.ID, &snapshot.PartnerID, &snapshot.APIKeyMask, &snapshot.SecretMask,
		&snapshot.Name, &snapshot.Status, &snapshot.ExpiresAt, &snapshot.IPWhitelist,
		&snapshot.RateLimitPerMinute, &snapshot.CreatedBy, &snapshot.CreatedAt,
		&snapshot.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrAPIKeyNotFound
	}
	if err != nil {
		return nil, err
	}
	return snapshot, nil
}

func readAPIKeySnapshot(ctx context.Context, tx pgx.Tx, partnerID, keyID string) (*apiKeySnapshot, error) {
	return lockAPIKeySnapshot(ctx, tx, partnerID, keyID)
}

// apiKeyStatusOperation 是审计里的中文动作名。与 partnerStatusOperation 同一条理由：分成
// 「启用密钥」与「停用密钥」两种，操作日志按动作筛时才看得出「这次集成断掉是停用密钥停出来的」。
func apiKeyStatusOperation(status string) string {
	if status == string(model.StatusEnabled) {
		return "启用合作方密钥"
	}
	return "停用合作方密钥"
}
