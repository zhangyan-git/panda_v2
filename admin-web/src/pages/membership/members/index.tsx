import { PageContainer, ProTable } from '@ant-design/pro-components';
import type { ActionType, ProColumns } from '@ant-design/pro-components';
import { useAccess, useNavigate } from '@umijs/max';
import { Button, message, Tag, Tooltip } from 'antd';
import { useRef } from 'react';
import { enumMeta, searchOptions } from '../../../services/labels';
import { listMemberships, type Membership, type MembershipQuery } from '../../../services/membership';
import { MEMBERSHIP_STATUS } from '../../../services/membershipLabels';
import { listMiniappUsers } from '../../../services/miniappUser';
import { toRFC3339 } from '../../../services/datetime';

/**
 * 会员列表：谁在会员中。
 *
 * 与套餐页的分工：套餐页答「卖了什么」，这一页答「谁在会员中」。**变更流水不在这里**——
 * 它是会员身上的时间线，离开那个会员就没有意义，所以点进详情页看（与入库单详情不挂菜单
 * 同一条理由）。
 *
 * 这一页只有读的能力：冻结 / 解冻 / 撤销 / 改有效期都在详情页里，而且各自要填原因——
 * 那些动作**直接改一个人已经在享的权益**，权限码也是更高的一档（membership:adjust）。
 * 放在详情页而不是列表的行内按钮，是为了让人先看见这个人现在是什么状态再动手。
 *
 * 用户身份只有 userId（会员库不存昵称与手机号，接口也不 join 用户服务）。要按手机号找，
 * 走的是上面的那一格——它先用小程序用户接口把手机号换成 userId 再筛，做法与订单列表同款。
 */

/** 接口给的空值一律显示成「—」：留白与「有个空字符串」在表里分不出来。 */
const dash = (value?: string | null) => (value ? value : '—');

/** 精确匹配的筛选参数：发出去之前 trim，空的整个丢掉。 */
const exact = (value: unknown): string | undefined => {
  const trimmed = typeof value === 'string' ? value.trim() : '';
  return trimmed || undefined;
};

export default function MembershipMembersPage() {
  const access = useAccess();
  const navigate = useNavigate();
  const actionRef = useRef<ActionType>();

  /**
   * 手机号 → userId 的换算结果，按关键词缓存。只存最后一次：价值只在「同一次筛选的多次
   * 翻页」上，而 ProTable 的 request 在翻页、改每页条数、刷新时都会重跑，每次都去问一次
   * 用户列表等于给同一页数据多发几倍的请求。
   */
  const resolvedPhone = useRef<{ keyword: string; userId: string }>();

  /**
   * 把「手机号」这一格换成 userId。
   *
   * 会员库只存 userId（用户数据在身份库，跨库），后端也**没有** keyword 参数，所以走两步：
   * 用现成的用户列表接口按关键词换出 userId，再拿它去筛会员。0 条就当场说「查不到」——
   * 否则用户看到的是一个空列表，分不清是「这个人不是会员」还是「手机号打错了」。
   *
   * 这个输入框只在有 canViewMiniappUsers 时渲染，所以这里不做权限失败的分支：没权限的人
   * 根本填不出关键词。
   */
  const resolvePhone = async (keyword: string): Promise<string | undefined> => {
    const cached = resolvedPhone.current;
    if (cached?.keyword === keyword) return cached.userId;
    const { items } = await listMiniappUsers({ keyword, page: 1, pageSize: 5 });
    if (items.length === 0) {
      message.warning(`查不到手机号或昵称匹配「${keyword}」的用户`);
      return undefined;
    }
    if (items.length > 1) {
      // 关键词是「手机号前缀或昵称片段」，输入「138」或「咖啡」都会命中一批。取第一个并在
      // 提示里说清是谁，比静默挑一个（运营会以为是精确匹配）好。
      const first = items[0];
      message.info(
        `「${keyword}」匹配到 ${items.length} 个用户，按第一个筛：${first.phone || first.id}`,
      );
    }
    resolvedPhone.current = { keyword, userId: items[0].id };
    return items[0].id;
  };

  // 手机号那一格只对能看小程序用户的人渲染：换 userId 走的是另一个权限码的接口
  // （admin:miniapp-users:view），没有却渲染出来，填一次就是一次 403。
  const phoneSearchColumn: ProColumns<Membership> = {
    title: '用户手机号',
    dataIndex: 'phone',
    hideInTable: true,
    fieldProps: { placeholder: '手机号前缀或昵称片段' },
  };

  const columns: ProColumns<Membership>[] = [
    ...(access.canViewMiniappUsers ? [phoneSearchColumn] : []),
    {
      // 会员的身份就是这一列。**只有它**——会员库没有昵称也没有手机号，接口也不 join
      // 用户服务（跨库，且会员域不该依赖身份域）。所以不做「批量解析出用户名」这种美化：
      // 那是会员域之外的事实，在这里拼出来只会是一份没有归属的缓存。
      //
      // 220 是按内容留的：UUID 是 36 个字符，14px 下约 250px，加复制按钮与内边距，这一格
      // 靠 ellipsis 省略尾巴、靠复制按钮取原文——比订单列表那格宽，因为那里它只是十几列
      // 里的一列，这里是这一页的主角。
      title: '用户 ID',
      dataIndex: 'userId',
      copyable: true,
      ellipsis: true,
      width: 220,
      // 等值比较（uuid 列），给不出候选，所以是输入框。有 canViewMiniappUsers 的人可以
      // 改用上面那格手机号。
      fieldProps: { placeholder: '完整用户 ID' },
    },
    {
      // 套餐编码与名称是**成交当时的快照**，不是现查套餐：套餐改了名，这里不跟着变。
      // 所以「按套餐编码筛」筛的也是快照上的那个码——买过又下架的套餐照样筛得出来。
      title: '套餐编码',
      dataIndex: 'planCode',
      hideInTable: true,
      fieldProps: { placeholder: '完整套餐编码，如 monthly_auto' },
    },
    {
      title: '套餐',
      dataIndex: 'planName',
      ellipsis: true,
      search: false,
      width: 150,
      render: (_, row) => (
        // 名字是快照上给人的那一份，编码是同一行上给系统的另一份。两个都摆出来会再占一列，
        // 而绝大多数时候只需要名字——编码挂在悬停里。
        <Tooltip title={row.planCode}>{dash(row.planName)}</Tooltip>
      ),
    },
    {
      title: '状态',
      dataIndex: 'status',
      valueType: 'select',
      valueEnum: searchOptions(MEMBERSHIP_STATUS),
      width: 90,
      render: (_, row) => {
        const meta = enumMeta(MEMBERSHIP_STATUS, row.status);
        return <Tag color={meta.color}>{meta.text}</Tag>;
      },
    },
    {
      // 仅搜索用的到期时间范围。用独立的 dataIndex，不与下面那列 expireAt 共用：同名的两列
      // 会在搜索表单里争同一个键，范围数组覆盖掉字符串之后，表格里的时间列就空了。
      //
      // 后端要的是完整的 RFC3339（半开区间 [from, to)），「2026-01-01」这种短式会被拒——
      // 所以过一遍 toRFC3339，它出来的就是带时区的完整串。
      title: '到期时间',
      dataIndex: 'expireRange',
      valueType: 'dateTimeRange',
      hideInTable: true,
      search: {
        transform: (value: unknown) => {
          const range = (value ?? []) as unknown[];
          return { expireFrom: toRFC3339(range[0]), expireTo: toRFC3339(range[1]) };
        },
      },
    },
    {
      title: '开通时间',
      dataIndex: 'startAt',
      valueType: 'dateTime',
      search: false,
      width: 170,
    },
    {
      // 到期时间是这一页最要紧的一列：客服那句「我什么时候到期」问的就是它。
      // **别看状态就下结论**：到期扫描没跑完的那一小段时间里，status 还是 active 而这
      // 一列已经过去了——接口另外回了算好的 active 就是为这件事，详情页用它。
      title: '到期时间',
      dataIndex: 'expireAt',
      valueType: 'dateTime',
      search: false,
      width: 170,
    },
    {
      // 这个开关是**用户自己**能关的那个（关了就是到期不再扣款）。套餐支不支持自动续费
      // 是另一回事，在套餐页上。客服接到「为什么这个月没扣款」，先看的就是这一列。
      title: '归属门店',
      dataIndex: 'storeName',
      search: false,
      width: 160,
      // 名字是后端现解的（会员库只存门店 id），解不出来时是空串——那时退回显示 id，
      // 至少那条引用还能对上账。两个都空才是「没有归属门店」。
      render: (_, row) => row.storeName || row.storeId || '—',
    },
    {
      title: '自动续费',
      dataIndex: 'autoRenew',
      valueType: 'select',
      valueEnum: {
        true: { text: '已开启' },
        false: { text: '未开启' },
      },
      width: 100,
      render: (_, row) => (row.autoRenew ? <Tag color="blue">已开启</Tag> : <Tag>未开启</Tag>),
    },
    {
      title: '续期次数',
      dataIndex: 'renewalCount',
      search: false,
      width: 100,
    },
    {
      title: '更新时间',
      dataIndex: 'updatedAt',
      valueType: 'dateTime',
      search: false,
      width: 170,
    },
    {
      title: '操作',
      valueType: 'option',
      // fixed 的列必须显式给宽度，且下面的 scroll.x 必须等于各列宽度之和（见下面的 1430）。
      // 100 是量出来的：「详情」两个字 14px 下约 28px，加左右内边距远够。
      width: 100,
      fixed: 'right',
      render: (_, row) => [
        <Button
          key="detail"
          type="link"
          size="small"
          onClick={() => navigate(`/membership/members/${row.id}`)}
        >
          详情
        </Button>,
      ],
    },
  ];

  return (
    <PageContainer
      title="会员列表"
      content="一个人只有一条会员记录，续期是在这一行上往后叠，所以这里的续期次数才有意义。冻结、撤销与改有效期在详情页里，都要填原因。"
    >
      <ProTable<Membership>
        actionRef={actionRef}
        rowKey="id"
        columns={columns}
        // 1270 = 220+150+90+170+170+100+100+170+100，各列 width 之和（搜索专用的几列不进
        // 表格，也就不进这个和）。改任何一列的宽度都要同批改这个数。
        scroll={{ x: 1430 }}
        search={{ labelWidth: 'auto' }}
        request={async (params) => {
          const query: MembershipQuery = {
            page: params.current,
            pageSize: params.pageSize,
            userId: exact(params.userId),
            status: exact(params.status) as MembershipQuery['status'],
            planCode: exact(params.planCode),
            expireFrom: exact(params.expireFrom),
            expireTo: exact(params.expireTo),
            // 三态：下拉没选时给的是 undefined（不是 ''），所以这里必须判 **undefined** 而
            // 不是真假——`false` 是一个有效取值（只看没开自动续费的），把它当成「没填」
            // 会悄悄地少筛一半。
            autoRenew: params.autoRenew === undefined ? undefined : String(params.autoRenew) === 'true',
          };
          // 手机号换成 userId 要走一次网络，所以放在最后、且**覆盖**上面那个 userId：
          // 两格都填时，以「我知道这个人是谁」的那一格为准（手机号比 uuid 更确定意图）。
          const keyword = exact(params.phone);
          if (keyword) {
            const userId = await resolvePhone(keyword);
            // 换不出人就当场返回空页，不要退回「按原来的 userId 筛」——那样会把上一轮的
            // 结果当成这一轮的答案。
            if (!userId) return { data: [], total: 0, success: true };
            query.userId = userId;
          }
          const result = await listMemberships(query);
          return { data: result.items, total: result.total, success: true };
        }}
      />
    </PageContainer>
  );
}
