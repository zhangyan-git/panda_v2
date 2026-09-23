package repository

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// DeductDeviceBalanceParams 是一次设备余额扣减（取货码那条路）。
//
// Amount 是**正数**，符号由本层补：流水记 -Amount。让调用方自己带负号，等于把
// 「负数成了充值」这种错留给下一次改动。
type DeductDeviceBalanceParams struct {
	DeviceID string
	Amount   int64
	// RequestID 是幂等键，**必填**。它是这条路唯一的防重投手段（见 DeductDeviceBalance）。
	RequestID string
	Remark    string
	// PickupPassword 是顾客在设备上敲的那个静态验证码，与 devices.pickup_password 比对。
	//
	// 它在**这里**比、在行锁之内比，而不是由调用方取回码自己比：那个码是这台设备自己的
	// 东西，只该待在持有它的库里（取回调用方的进程意味着每取一次货它就跑一次服务边界，
	// 而校验方拿着的是副本）。放在同一个事务里还顺手消掉了 TOCTOU——不存在「码校验通过
	// 之后、钱扣走之前，码被改了或者余额被另一笔扣走」的窗口。
	PickupPassword string
}

// DeductDeviceBalanceResult 是一次扣减的结果。
type DeductDeviceBalanceResult struct {
	// BalanceAfter 是扣完之后的余额；命中幂等时是**当初那一次**记下的余额。
	BalanceAfter int64
	// Applied=false 表示这个 request_id 已经扣过了，本次一个字段都没写。
	//
	// 调用方必须把它当成成功继续往下走：重投是常态（对方重试、网络重放），回错误会让
	// 一次已经成功的取货变成一条永远建不出来的单。
	Applied bool
	// Amount 是这一次**实际扣掉**的金额，正数；命中幂等时是当初那一次从流水上读回来的。
	//
	// 调用方要用它记账，而不是用自己算出来的价：命中幂等时两者可以不等（两次之间这一杯
	// 被改过价），而钱只动过一次——订单上的金额与流水上的对不上，那笔差额没有任何一处
	// 能解释。流水把金额记成负数（有符号口径），所以这里取回来的是它的相反数。
	Amount int64
	// EntryID 是这次（或当初那次）流水的 id，记日志用。
	EntryID string
}

// rowQuerier 让同一段读既能用事务也能用连接池。
//
// 这条路上两处都要读「这个 request_id 的那一行流水」：重投的短路读在**事务里**（与设备
// 行锁同一个事务），撞唯一索引之后的兜底读在**池上**（那一刻事务已经回滚了，报错的事务
// 处于失效状态，只能重开一条）。
type rowQuerier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// 取货码验证不通过的两种情形。分开是因为**要修的东西不同**：一个是顾客/店员敲错了码，
// 一个是这台设备压根没配过码（运营要去后台补上）。对调用方它们都回 PermissionDenied
// ——都是「你没资格动这台机器的钱」，重试无用。
var (
	// ErrPickupPasswordMismatch 表示报上来的验证码与这台设备上存的那一个不一致。
	ErrPickupPasswordMismatch = errors.New("pickup password does not match")
	// ErrPickupPasswordNotSet 表示这台设备没有配过验证码。
	//
	// 它**不是**「空对空就通过」：设备列是 NOT NULL DEFAULT ''，没配过就是空串，而拿
	// 空串当通行证等于这台机器上的钱谁都能扣走——后台看不出任何异常。
	ErrPickupPasswordNotSet = errors.New("device has no pickup password configured")
	// ErrRequestIDUsedByAnotherEntry 表示这个 request_id 已经有一行流水了，但那一行
	// **不是这条路上扣减**（type/设备对不上）。
	//
	// 它回错误而不是当成「重投」：device_balance_ledger.request_id 的唯一索引是**跨写路径
	// 共享**的一个命名空间——后台调整那条路（admin.go 的 adjust）也从请求体里收一个
	// request_id。当成重投的后果是**这一杯白送**：调用方拿到 Applied=false 就当作「上一次
	// 已经扣成功了」继续建单，而钱一分没动。回错误只是这一单建不出来，看得见、也查得到。
	ErrRequestIDUsedByAnotherEntry = errors.New("request_id already belongs to a different balance entry")
)

// DeviceBalanceRepository 是设备余额的**扣减**入口。
//
// 与 MasterDataRepository 分家的理由：那个接口的名字写的是「主数据的读路径」，
// 这个方法是本服务唯一的写口，混进去只会让那个接口的说明变成假的。
type DeviceBalanceRepository interface {
	DeductDeviceBalance(ctx context.Context, in DeductDeviceBalanceParams) (*DeductDeviceBalanceResult, error)
}

type pgBalanceRepository struct{ pool *pgxpool.Pool }

// NewDeviceBalanceRepository 创建设备余额扣减的数据访问实现。
func NewDeviceBalanceRepository(pool *pgxpool.Pool) DeviceBalanceRepository {
	return &pgBalanceRepository{pool: pool}
}

// DeductDeviceBalance 从一台设备的咖啡余额里扣一笔，并追加一条流水。
//
// # 三件事必须同一个事务
//
// 行锁设备、改余额、写流水。少任何一步都会留下一种对不上的状态：余额变了没有流水，
// 或者流水记了余额没变。老系统那条路上只有 `$inc`，一分钱流水都不留——这台机器的钱
// 怎么没的查不到。这里是有意不照抄。
//
// 与后台调整（AdminRepository.AdjustBalance）的两处不同是**有意的**：
//   - type 写 deduct 而不是 adjust：后台那次是管理员手工改数，这次是「这台机器上
//     卖出去了一杯」，流水表上两者要能分开（口径见 migrations/coffee_machine 的
//     type CHECK）。
//   - **不写审计**。审计记的是「谁在后台点了什么」，这条路上没有操作人——操作人是
//     机器前面的那个人，而他在我们库里没有账号（orders.user_id 在这一单上也是空的）。
//     硬塞一个 actor 进审计表，只会让审计里出现一批查无此人的记录。
//
// # 余额不够时一个字段都不写
//
// 判断在 UPDATE 之前，回 ErrInsufficientBalance（与后台调整共用同一个哨兵错误：
// 「余额会变成负数」在两处是同一句话）。调用方据此回 FailedPrecondition——这一单真的
// 做不成，重试也没用，而不是一次可以重试的故障。
//
// 判断的顺序是设备 → 验证码 → 余额，每一步失败都停在写之前：一次被拒的取货在这张表上
// 留不下任何痕迹（流水是只追加的，写进去了就删不掉）。
//
// # 幂等：真正的守门人是唯一索引，短路读只是让它不白跑
//
// 「先 SELECT 有没有这个 request_id，没有就扣」在并发下是错的：两次重投可以同时读到
// 「没有」，然后扣两次。真正的守门人是 device_balance_ledger_one_per_request——那个
// 部分唯一索引只索引 request_id 非空的行，第二次 INSERT 撞索引失败。
//
// 所以这里的次序是：**先读一次**（命中就直接回当初那一笔，一个字段都不写），写之前再按
// 老规矩由索引兜底。读那一次不改变幂等的**判据**（那条索引才是判据），它变的是重投的
// 代价与后果：
//
//   - 重投不再需要验证码对得上。它本来就只有一个正经用途——「上一次钱扣了、单没建出来，
//     把这一次补出来」，而那时候码可能已经被后台改过了。让重投卡在码上，等于把一笔已经
//     扣掉的钱永远变成一个建不出来的单。
//   - 重投不再走「先写再回滚」那条路。撞索引回滚是能跑的（下面那段说的是并发下的兜底），
//     但它每一次都要把一个已经改过的余额再回滚掉，代价与噪音都白出。
//
// 撞上唯一索引时的处理是**回滚整个事务**（余额那一次 UPDATE 连同它一起没了），再把当初
// 那条流水读回来，返回 applied=false。这里不能只回滚 INSERT：PostgreSQL 的事务一旦报错
// 就进入失效状态，后续语句一律拒绝，唯一能做的就是 rollback 重来。
//
// # reference_id 留空是有意的
//
// 流水上有 reference_type / reference_id 两列，指向这次变动的来源单据。取货码这条路上
// 来源是**订单，而订单在扣款之后才建**（扣在前、建单在后，中间断了是「扣了没建单」
// 而不是「建了单没扣钱」）——那时刻订单号还不存在，硬塞一个进去只能是伪造。要回溯
// 「这笔扣减对应哪一单」，用 request_id 去 orders 那边找 third_party_order_no。
func (r *pgBalanceRepository) DeductDeviceBalance(ctx context.Context, in DeductDeviceBalanceParams) (*DeductDeviceBalanceResult, error) {
	if in.Amount <= 0 {
		// 0 会撞流水表的 amount <> 0 约束，负数会变成充值。两件都不该走到 SQL 那里。
		return nil, fmt.Errorf("deduct device balance: amount must be positive, got %d", in.Amount)
	}
	if in.RequestID == "" {
		// 没有幂等键这条路不成立：重投会扣第二次，而那笔钱是同一个用户出的。部分唯一
		// 索引的条件正是 `request_id <> ''`，空串会**绕过**它——所以这里必须挡住，
		// 数据库那一层兜不住。
		return nil, errors.New("deduct device balance: request id is required")
	}

	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	// 行锁不能省：余额是读-改-写，两条并发扣减各自读到同一个旧值，后提交的那条会覆盖
	// 前一条，devices.coffee_balance 与流水表上的账就此对不上（与 AdjustBalance 同一条）。
	//
	// 验证码也在这条查询里取回来：它与余额必须来自**同一行、同一个时刻**，否则校验就
	// 失去了意义（码取自一行、钱扣自另一行的话，两次之间那一行可以被改）。
	var before int64
	var storedPassword string
	err = tx.QueryRow(ctx, `SELECT coffee_balance, pickup_password FROM devices WHERE id = $1 FOR UPDATE`,
		in.DeviceID).Scan(&before, &storedPassword)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrDeviceNotFound
	}
	if err != nil {
		return nil, err
	}

	// 重投的短路读：这个 request_id 在流水表上已经有这一路上的扣减时，本次一个字段都不会写，
	// 与验证码、余额都无关（见上面「幂等」那一节）。放在这里而不是更前面，是为了保持
	// 「设备不存在 → NotFound」优先于一切：一次报着不存在的设备号的重投不该被答成「已经扣过了」。
	existing, err := lookupDeduction(ctx, tx, in.RequestID, in.DeviceID)
	if err != nil || existing != nil {
		return existing, err
	}

	// 校验在扣减之前：码不对就什么都不写，连余额都不读第二次（读也只读了个值，没写）。
	//
	// 设备没配过验证码（空串）时**拒绝**，不搞「空对空放行」：那等于这台机器上的钱谁都能
	// 扣走，而运营在后台看不出任何异常。宁可让这台机器的取货码路径用不了，等运营把码配上。
	if storedPassword == "" {
		return nil, ErrPickupPasswordNotSet
	}
	if storedPassword != in.PickupPassword {
		// 比对用普通 ==：这是一串给人敲的静态码，不是需要恒定时间比对的密钥（它没有
		// 暴力枚举的价值——每次尝试都要经过厂商验签，而且码本身在后台是明文可查的）。
		return nil, ErrPickupPasswordMismatch
	}

	after := before - in.Amount
	// 数据库有 CHECK (coffee_balance >= 0) 兜底，但在这里先判一次，才能把一个「余额不够」
	// 的业务结论回成 FailedPrecondition，而不是让约束违反翻译出来的 500。
	if after < 0 {
		return nil, ErrInsufficientBalance
	}

	// type 固定 deduct：recharge 是小程序充值那条路、adjust 是后台调整（admin.go）、
	// reverse 需要 reverses_entry_id（冲正不走这里，它是另一个入口的事）。
	entryID := uuid.NewString()
	if _, err := tx.Exec(ctx, `INSERT INTO device_balance_ledger (id, device_id, type,
		amount, balance_after, reference_type, request_id, remark, operator_name)
		VALUES ($1, $2, 'deduct', $3, $4, 'pickup_code', $5, $6, '')`,
		entryID, in.DeviceID, -in.Amount, after, in.RequestID, in.Remark); err != nil {
		// 翻错误复用 mapPGError：约束名到哨兵错误的对照表只有那一份（admin.go），
		// 在这里再认一次 SQLSTATE 就是第二份口径，改名字时只会改到一份。
		mapped := mapPGError(err)
		if errors.Is(mapped, ErrDuplicateRequest) {
			// 这个 request_id 已经扣过了：本次重投。整个事务回滚（余额那一次 UPDATE 也
			// 一起没了），再把当初那条流水读回来当结果。
			//
			// 走到这里说明上面那次短路读没读到——并发下两次重投同时进来才会有这一幕
			// （另一次在两次读之间提交）。所以这不是一条死分支，是那条索引的兜底。
			_ = tx.Rollback(ctx)
			found, lookupErr := lookupDeduction(ctx, r.pool, in.RequestID, in.DeviceID)
			if lookupErr != nil || found != nil {
				return found, lookupErr
			}
			// 撞了索引却读不回来：那一行在这两条语句之间被删了（流水表是只追加的，
			// 正常不会发生）。回一个说得清楚的错，而不是一个 nil 结果让调用方以为成功了。
			return nil, fmt.Errorf("%w: request %q", ErrRequestIDUsedByAnotherEntry, in.RequestID)
		}
		return nil, mapped
	}

	if _, err := tx.Exec(ctx, `UPDATE devices SET coffee_balance = $2, updated_at = NOW() WHERE id = $1`,
		in.DeviceID, after); err != nil {
		return nil, err
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return &DeductDeviceBalanceResult{
		BalanceAfter: after,
		Applied:      true,
		Amount:       in.Amount,
		EntryID:      entryID,
	}, nil
}

// lookupDeduction 按 request_id 把这一行流水读回来，并分清**三种**而不是两种情形：
//
//	(nil, nil)                        这个 request_id 在流水表上没有行——照常往下扣。
//	(&result, nil)                    已经有这一路上的扣减了。调用方当成重投（applied=false）。
//	(nil, ErrRequestIDUsedByAnotherEntry)  有行，但那一行**不是这台设备上的这次扣减**。
//
// 第三种必须与「没有」分开：request_id 是**跨写路径共享**的一个命名空间（后台调整那条也带
// 一个，见 admin.go），而 device_balance_ledger_one_per_request 是**只按 request_id** 建的
// 唯一索引。把「被别人用了」当成「没有」的后果是 INSERT 撞索引、整个事务回滚，看起来只是
// 白跑一次；把它当成「重投」的后果严重得多——调用方拿着 applied=false 继续建单，而钱一分
// 没动，这一杯白送。所以查询**不带 type/device 过滤**，三格（id / type / device_id）都读出来
// 由这里判，而不是靠 SQL 的 WHERE 把它们藏掉：藏掉的那种写法正是这条判据最初出错的地方。
//
// 金额**故意不比**：一次扣款成功之后这一杯的价格可能被改过（改价 → 厂商重投），比金额会把
// 一次已经扣过钱的取货变成一条永远建不出来的单。改价带来的错配在**调用方**那一侧解决：
// 这里把当初那笔的金额（Amount）原样带回去，让订单照着流水记账。
func lookupDeduction(ctx context.Context, q rowQuerier, requestID, deviceID string) (*DeductDeviceBalanceResult, error) {
	var result DeductDeviceBalanceResult
	var entryType, entryDeviceID string
	var amount int64
	err := q.QueryRow(ctx, `SELECT id::text, type, device_id::text, balance_after, amount
		FROM device_balance_ledger WHERE request_id = $1`, requestID).
		Scan(&result.EntryID, &entryType, &entryDeviceID, &result.BalanceAfter, &amount)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	// 两个字面量与上面 INSERT 里那两个字面量必须一样（device_balance_ledger 的 type CHECK）：
	// deduct 是这条路写的，那台机器必须是同一台。
	if entryType != "deduct" || entryDeviceID != deviceID {
		return nil, fmt.Errorf("%w: request %q", ErrRequestIDUsedByAnotherEntry, requestID)
	}
	// 流水把扣减记成负数（有符号口径，见迁移里的说明），这里翻转成正数再交出去——调用方
	// 拿它当「这一次扣了多少钱」，与请求里那个正数金额是同一个口径。
	result.Amount = -amount
	result.Applied = false
	return &result, nil
}
