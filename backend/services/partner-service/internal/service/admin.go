package service

import (
	"context"
	"strings"

	"github.com/panda-dev/panda-v2/backend/services/partner-service/internal/dto"
	"github.com/panda-dev/panda-v2/backend/services/partner-service/internal/ingress"
	"github.com/panda-dev/panda-v2/backend/services/partner-service/internal/model"
	"github.com/panda-dev/panda-v2/backend/services/partner-service/internal/repository"
)

// 这个文件是**合作方账号**的治理：列表、详情、新增、修改、启停，加上调用日志的查询。
//
// 密钥那一半在 apikey.go —— 它与这里分成两个文件，是因为它多一条别的路径都没有的性质：
// 明文密钥只在签发那一次出现（见 IssueAPIKey）。放在一起读的人很难一眼看出「这个包里
// 哪一行碰过明文」。
//
// # 每个写方法都收一个 operator
//
// operator 是后台管理员的 ID，由 controller 从令牌里取出来传进来（这一层不认识 HTTP）。
// 它有两个去处，都是必须的：库里那一行的 created_by，以及审计（audit.Recorder 另外从
// 上下文里取一次 ActorID；两者都由同一次登录产生，不会打架）。缺了它要报错而不是写空串
// ——「不知道是谁签发了这把密钥」是事后追责时唯一要看的那一列。

// ListPartners 读一页合作方。
//
// status 筛选**先校验再查**：一个拼错的词（"Enabled"）在 SQL 里只是匹配不上，返回的是
// 空列表——而运营会把它读成「这个状态下没有合作方」。与 payment 那边 msgInvalidFilter
// 同一条理由，宁可 400。
func (s *AdminService) ListPartners(ctx context.Context, q dto.PartnerQuery) ([]*repository.PartnerListRow, int, error) {
	if strings.TrimSpace(q.Status) != "" {
		status, err := normalizeStatus(q.Status)
		if err != nil {
			return nil, 0, err
		}
		q.Status = status
	}
	// keyword 直接透传（仓储那边做 ILIKE 并转义元字符，见 repository.escapeLike）。这里只
	// trim：粘贴过来的搜索词常常带一个看不到的尾空格，而那会让「明明有这一家」搜不出来。
	q.Keyword = strings.TrimSpace(q.Keyword)
	return s.repository.ListPartners(ctx, q)
}

// GetPartner 读一条合作方。
func (s *AdminService) GetPartner(ctx context.Context, id string) (*model.PartnerAccount, error) {
	return s.repository.FindPartner(ctx, id)
}

// CreatePartner 新增一条合作方。
func (s *AdminService) CreatePartner(ctx context.Context, in dto.PartnerInput, operator string) (*model.PartnerAccount, error) {
	write, err := s.partnerWrite(in)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(operator) == "" {
		return nil, ErrOperatorRequired
	}
	return s.repository.CreatePartner(ctx, write, operator)
}

// UpdatePartner 整份覆盖一条合作方。
//
// 编码**不进 UPDATE 的 SET**（见 repository.UpdatePartner）：请求里带着的 code 只用来核对，
// 不一致时报 ErrPartnerCodeImmutable。这里不额外判「code 为空」——空串表示「这次不改编码」，
// 与支付域那两张配置表的语义一致。
func (s *AdminService) UpdatePartner(ctx context.Context, id string, in dto.PartnerInput) (*model.PartnerAccount, error) {
	write, err := s.partnerWrite(in)
	if err != nil {
		return nil, err
	}
	return s.repository.UpdatePartner(ctx, id, write)
}

// SetPartnerStatus 启用或停用一条合作方。
//
// 停用会**当场**让名下所有密钥失效（中间件每个请求都真查这一行，没有缓存）。这一条的分量
// 与「改个联系方式」不是一个量级，所以它是单独一条路径、单独一个审计动作（update_status）。
func (s *AdminService) SetPartnerStatus(ctx context.Context, id, status string) (*model.PartnerAccount, error) {
	normalized, err := normalizeStatus(status)
	if err != nil {
		return nil, err
	}
	return s.repository.SetPartnerStatus(ctx, id, normalized)
}

// partnerWrite 把 dto 翻成仓储的写入值，校验都在这里。
//
// 新建与修改共用一个：**它俩的字段规则必须一样**。分开写的那天，改路径上一处校验被漏掉，
// 表现是「建的时候不让填的编码，改的时候能改成一个不合法（但还没被占用）的值」。
func (s *AdminService) partnerWrite(in dto.PartnerInput) (repository.PartnerWrite, error) {
	code := strings.TrimSpace(in.Code)
	if !codePattern.MatchString(code) {
		return repository.PartnerWrite{}, ErrCodeInvalid
	}
	name := strings.TrimSpace(in.Name)
	if name == "" {
		return repository.PartnerWrite{}, ErrNameRequired
	}
	expiresAt, err := parseExpiresAt(in.ExpiresAt)
	if err != nil {
		return repository.PartnerWrite{}, err
	}
	// 联系人那几个字段只 trim，不做格式校验（邮箱/电话的形状各国有各的标准，钉一条正则
	// 只会挡掉合法输入）。它们的用途是「出事了知道找谁」，写错一个字不影响任何判定。
	return repository.PartnerWrite{
		Code:         code,
		Name:         name,
		ContactName:  strings.TrimSpace(in.ContactName),
		ContactPhone: strings.TrimSpace(in.ContactPhone),
		ContactEmail: strings.TrimSpace(in.ContactEmail),
		Description:  strings.TrimSpace(in.Description),
		ExpiresAt:    expiresAt,
	}, nil
}

// ============================================================
// 调用日志
// ============================================================

// ListCallLogs 读一页调用日志。
//
// 三个筛选值都**先校验再查**（error_code / status_code / 时间端点）：它们在页面上是三个
// 输入框，而 SQL 对拼错的值的回答是「空列表」——那与「这段时间没有失败」长得一模一样。
//
// 校验用的是**同一份词表**：error_code 的取值来自 ingress.ErrorCodes()（中间件真正会写的
// 那一套常量），不是这里另抄一份。抄一份的结果是中间件加了一个新码之后，日志里出现了它的
// 行，而筛选框里没有这个选项。
func (s *AdminService) ListCallLogs(ctx context.Context, q dto.CallLogQuery) ([]*repository.CallLogRow, int, error) {
	if code := strings.TrimSpace(q.ErrorCode); code != "" {
		if !ingress.IsErrorCode(code) {
			return nil, 0, ErrErrorCodeInvalid
		}
		q.ErrorCode = code
	}
	if q.StatusCode != 0 && (q.StatusCode < 100 || q.StatusCode > 599) {
		return nil, 0, ErrStatusCodeInvalid
	}
	return s.repository.ListCallLogs(ctx, q)
}
