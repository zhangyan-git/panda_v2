import {
  ModalForm,
  PageContainer,
  ProDescriptions,
  ProFormDependency,
  ProFormDigit,
  ProFormGroup,
  ProFormList,
  ProFormSelect,
  ProFormText,
  ProFormTextArea,
  ProTable,
} from '@ant-design/pro-components';
import type {
  ActionType,
  ProColumns,
  ProDescriptionsItemProps,
  ProFormInstance,
} from '@ant-design/pro-components';
import { useAccess } from '@umijs/max';
// 查看弹窗里那张表是**普通的 antd Table**（数据一次性给全，不走 ProTable 的请求生命周期），
// 所以列类型用它自己的 ColumnsType 而不是 ProColumns —— 两者在 title/render 的签名上不兼容。
import type { ColumnsType } from 'antd/es/table';
import { Alert, Button, Form, Modal, Popconfirm, Table, Tag, message } from 'antd';
import { useEffect, useRef, useState } from 'react';
import { enumMeta, searchOptions } from '../../../services/labels';
import { formatYuan } from '../../../services/money';
import { FULL_PAGE_PARAMS } from '../../../services/pagination';
import { requestErrorMessage } from '../../../services/requestError';
import {
  createSettlementRule,
  deleteSettlementRule,
  getSettlementRule,
  listSettlementAccounts,
  listSettlementChannels,
  listSettlementRules,
  updateSettlementRule,
  type SettlementAccount,
  type SettlementRule,
  type SettlementRuleInput,
  type SettlementRuleItem,
  type SettlementRuleQuery,
} from '../../../services/settlement';
import {
  ALLOCATION_MODE,
  BIZ_TYPE,
  CALC_TYPE,
  PARTY_TYPE,
  PERCENT_MAX_HUNDREDTHS,
  RECORD_STATUS,
  SCOPE_TYPE,
  formatPercent,
  percentSumHundredths,
  scopeNeedsRef,
  scopeNeverHits,
} from '../../../services/settlementLabels';
import { EMPTY_SETTLEMENT_REFS, loadSettlementRefs, refName, type SettlementRefs } from '../settlementRefs';
import { emptyItem, itemToForm, itemToPayload, ruleScopeRef } from './ruleFormMapping';
import { scrollableModalBody } from '../../../components/common/modalProps';

/**
 * 分账规则：**这类业务、这个范围上的钱怎么分**。
 *
 * # 这一页要回答的问题，按重要性排
 *
 * 1. **这条规则今天生效吗**——`status` 是启用的、而且这一档今天会被订单侧推上来。品牌与商品
 *    两档页面给得了、配得出来，但**今天命不中**（订单侧只有门店与设备两维），所以选中它们时
 *    有一条 Alert。藏起这两档是不行的：将来订单侧补齐那两维时还得把它们放回来，而中间这段
 *    时间里没人知道它们曾经存在过。
 * 2. **比例加起来是多少**——合计超过 100% 时**保存会被后端拒掉**（400，`ErrSettlementRuleItem
 *    RatioOverflow`），所以从这一页配进去的规则不会超。但那个 400 要到点了保存才出现，一屏项
 *    目里看不出是哪两项凑过头了；而且**执行期**那条兜底还在：`computeSettlement` 遇到合计超了
 *    （历史数据、直接写库进来的行）会静默走「分出去的钱比收进来的多」那一支——整单归平台，
 *    配错的人与收到钱的人都看不见异常。所以表单里有一个实时合计（整数百分点求和，见
 *    settlementLabels.percentSumHundredths 的说明），在提交之前就把话说了。
 * 3. **分给谁**——每一项挂的账户。停用的、别的渠道的账户会在**执行期被静默滤掉**（那条 LEFT
 *    JOIN），所以后端在保存时就拦；这里把渠道显示在账户选项上，好让人一眼看出自己混了渠道。
 *
 * # 命中顺序与「同档位只能有一条」
 *
 * 同一个业务分类下，档位从具体到宽泛（设备 → 门店 → 品牌 → 商品 → 全局）先命中先返回；
 * 同档位**只能有一条启用中的规则**（008 的部分唯一索引，在 `WHERE status='enabled'` 上）。
 * 所以配重了会 409，而出口是**把旧的那条停用**——停用之后同档位就能再配一条。
 *
 * # 为什么列表里看不到「分给了谁、分多少」
 *
 * 列表接口不回规则的项（items 是空数组），项只在详情里。列表一次拉 20 条就要多 20 次 JOIN，
 * 而这一列的信息在「查看」里一眼就能看全。
 *
 * # 删除与停用
 *
 * **被分账任务引用过的规则删不掉**（`settlement_tasks.rule_id` 是 ON DELETE RESTRICT），后端
 * 回 409。常规出口是**停用**：已经发生过的那几笔分账不受影响（它们用的是任务上的快照），而这条
 * 规则不再参与新的命中。停用是把状态改掉再整份保存——后端没有单独的开关接口，见
 * services/settlement.ts 里 updateSettlementRule 的说明。
 */

/** 接口给的空值一律显示成「—」：留白和「有个空字符串」在表里分不出来。 */
const dash = (value?: string | null) => (value ? value : '—');

const money = (fen?: number | null) => `¥${formatYuan(fen)}`;

/** 精确匹配的筛选参数：发出去之前 trim，空的整个丢掉。 */
const exact = (value: unknown): string | undefined => {
  const trimmed = typeof value === 'string' ? value.trim() : '';
  return trimmed || undefined;
};

/*
  表单与接口之间的换算（金额的分↔元、三项互斥的归零、范围引用的归零）全在 ruleFormMapping.ts
  里，连同它的测试。**别在这一页上就地再写一份**：单位写反了页面上看不出任何异常。
*/

export default function SettlementRulesPage() {
  const access = useAccess();
  const actionRef = useRef<ActionType>();
  /** 表单实例。只在「换档位时把范围引用清掉」这一处用得到——见下面那个 scopeType 的 onChange。 */
  const formRef = useRef<ProFormInstance<SettlementRuleInput>>();

  /** 正在编辑的那一条。null = 弹窗关着；没有 rule = 新建。 */
  const [editing, setEditing] = useState<{ rule?: SettlementRule } | null>(null);
  /** 正在查看的那一条（详情，带 items）。 */
  const [viewing, setViewing] = useState<SettlementRule>();

  /** 引用字典（门店 / 品牌 / 商户 / 设备），给范围下拉与列表里的名字用。 */
  const [refs, setRefs] = useState<SettlementRefs>(EMPTY_SETTLEMENT_REFS);
  /**
   * 可选账户：**只要启用中的**。
   *
   * 停用的账户挂上去会被后端拒（`ErrSettlementRuleItemAccountDisabled`），把它摆在选项里就是
   * 让人挑一个存不下去的东西。已经挂在规则上的停用账户仍然会显示成原始 id —— 这条规则因此
   * 存不下去，那正是要让人看见的事。
   *
   * 取的是**全集**（FULL_PAGE_PARAMS）：账户多到超过一页时末尾那些不会被列出来，与券模板、
   * 门店下拉是同一个取舍。真要治得给账户列表接口加「只回启用中的」条件。
   */
  const [accounts, setAccounts] = useState<SettlementAccount[]>([]);
  /** provider 码 → 渠道名。规则没有渠道列，渠道是从账户上读的，这里只为了显示。 */
  const [channels, setChannels] = useState<Record<string, string>>({});

  useEffect(() => {
    // `loadSettlementRefs` 的四份请求各自兜住了（四个权限码，一个都没有的运营也进得来这一页），
    // 所以它正常不会抛——这一句是兜住「映射里的 bug」那一种：真抛出来的话，不如让字典空着
    // （页面退回显示原始 id），也别在控制台留一个没人处理的 rejection。
    void loadSettlementRefs()
      .then(setRefs)
      .catch(() => setRefs(EMPTY_SETTLEMENT_REFS));
    void listSettlementAccounts({ ...FULL_PAGE_PARAMS, status: 'enabled' })
      .then((page) => setAccounts(page.items.filter((account) => account.status === 'enabled')))
      // 账户列表取不到不该让整页打不开：下拉是空的，规则列表照常。
      .catch(() => setAccounts([]));
    void listSettlementChannels()
      .then((list) => setChannels(Object.fromEntries(list.map((c) => [c.provider, c.name]))))
      .catch(() => setChannels({}));
  }, []);

  // 名字只取 partyName，没有 fallback：账户号那列随 017 删了，主体名现在是必填的（后端与库
  // 的 CHECK 都拦着空值），而渠道接收方号是给渠道看的号、不该拿来当人认的名字。
  const accountOptions = accounts.map((account) => ({
    label: `${account.partyName}（${account.receiverId}${
      channels[account.provider] ? ` · ${channels[account.provider]}` : ''
    }）`,
    value: account.id,
  }));

  const openCreate = () => setEditing({});

  /**
   * 取详情的请求序号。
   *
   * 列表行里没有 items，所以「编辑」与「查看」都要先取一次详情——而这两个动作是**并发**的：
   * 连点两行的「编辑」，或者点完「编辑」又去点另一行的「查看」，先发出去的那一次可能后回来。
   * 后回来的会盖掉先回来的：编辑弹窗的 `key` 里带着 id，晚到的那份详情会把弹窗**重建**一次，
   * 已经改到一半的字段就没了。所以每次发请求领一个号，只有**最后领号的那一次**的结果才落到
   * 状态上。
   */
  const detailSeq = useRef(0);

  /**
   * 正在取详情的那一行。放在这一层而不是每行一个 state：同一时刻只可能有一行在取（点了另一行
   * 就换成另一行），按钮转圈是为了让人知道「点了，在等」——详情要一次往返，点在没反应的那几百
   * 毫秒里，人会以为按钮坏了。
   */
  const [loadingRuleId, setLoadingRuleId] = useState<string>();

  const loadDetail = async (row: SettlementRule, apply: (rule: SettlementRule) => void) => {
    const seq = ++detailSeq.current;
    setLoadingRuleId(row.id);
    try {
      const rule = await getSettlementRule(row.id);
      // 落状态之前再看一眼序号：这中间点了别的一行的话，这一次拿到的就是过期数据。
      // 过期时**连错误也不报**：那次动作已经被后来的那一次取代了，弹一句话只会让人以为
      // 刚才点的那一下失败了。
      if (seq !== detailSeq.current) return;
      apply(rule);
    } catch (error) {
      if (seq !== detailSeq.current) return;
      message.error(requestErrorMessage(error, '加载规则详情失败'));
    } finally {
      // 只有最后那一次负责把转圈停掉——先发的那次回来时，按钮上转的是后发的那一行。
      if (seq === detailSeq.current) setLoadingRuleId(undefined);
    }
  };

  // 列表行里没有 items，整份编辑必须先取详情——直接把列表行发回去会把这条规则的项清空。
  const openEdit = (row: SettlementRule) => void loadDetail(row, (rule) => setEditing({ rule }));

  const openView = (row: SettlementRule) => void loadDetail(row, setViewing);

  const remove = async (row: SettlementRule) => {
    try {
      await deleteSettlementRule(row.id);
      message.success('规则已删除');
      actionRef.current?.reload();
    } catch (error) {
      // 多半是 409「已被 N 笔分账引用」——后端为这一种写了中文，照搬它，别在这儿再猜一遍。
      message.error(requestErrorMessage(error, '删除失败：可能已被分账引用，请改为停用'));
    }
  };

  const submit = async (values: SettlementRuleInput) => {
    const rule = editing?.rule;
    const payload: SettlementRuleInput = {
      name: values.name,
      bizType: values.bizType,
      scopeType: values.scopeType,
      scopeRef: ruleScopeRef(values.scopeType, values.scopeRef),
      allocationMode: values.allocationMode,
      status: values.status,
      remark: values.remark ?? '',
      // 顺序按提交时的先后重新编：拖拽排序没有做，项在列表里的次序就是它提交的次序。
      items: (values.items ?? []).map(itemToPayload),
    };
    try {
      if (rule) {
        await updateSettlementRule(rule.id, payload);
        message.success('规则已保存');
      } else {
        await createSettlementRule(payload);
        message.success('规则已创建');
      }
      setEditing(null);
      actionRef.current?.reload();
      return true;
    } catch (error) {
      // 失败时**不关弹窗**：填的东西留着，改一改再提交。话照搬后端那句——它为「同档位已有一条
      // 启用的规则」「比例合计超过 100%」「账户已停用」各写了一句中文，前端再猜一遍只会猜错。
      message.error(requestErrorMessage(error, '保存失败：请检查档位是否已被占用、比例合计是否超过 100%'));
      return false;
    }
  };

  const columns: ProColumns<SettlementRule>[] = [
    {
      title: '规则名称',
      dataIndex: 'name',
      ellipsis: true,
      width: 180,
      fieldProps: { placeholder: '支持模糊匹配，可只填一截' },
    },
    {
      title: '业务分类',
      dataIndex: 'bizType',
      valueType: 'select',
      valueEnum: searchOptions(BIZ_TYPE),
      width: 120,
      render: (_, row) => {
        const meta = enumMeta(BIZ_TYPE, row.bizType);
        return <Tag color={meta.color}>{meta.text}</Tag>;
      },
    },
    {
      // 这一列是「这条规则管多大一片地方」。顺序与命中顺序一致（从具体到宽泛）。
      title: '范围档位',
      dataIndex: 'scopeType',
      valueType: 'select',
      valueEnum: searchOptions(SCOPE_TYPE),
      width: 110,
      render: (_, row) => {
        const meta = enumMeta(SCOPE_TYPE, row.scopeType);
        return <Tag color={meta.color}>{meta.text}</Tag>;
      },
    },
    {
      // 全局档的范围引用是空串（008 的 CHECK 是充要条件），那是一种正常，不是缺值。
      // 解不出名字时退回原始 id —— 多半是字典没取到（门店列表要另一个权限码）。
      title: '范围',
      dataIndex: 'scopeRef',
      search: false,
      ellipsis: true,
      width: 160,
      render: (_, row) => (scopeNeedsRef(row.scopeType) ? refName(refs, row.scopeRef) : '全部'),
    },
    {
      title: '分法',
      dataIndex: 'allocationMode',
      search: false,
      width: 140,
      render: (_, row) => {
        const meta = enumMeta(ALLOCATION_MODE, row.allocationMode);
        return <Tag color={meta.color}>{meta.text}</Tag>;
      },
    },
    {
      title: '状态',
      dataIndex: 'status',
      valueType: 'select',
      valueEnum: searchOptions(RECORD_STATUS),
      width: 100,
      render: (_, row) => {
        const meta = enumMeta(RECORD_STATUS, row.status);
        return <Tag color={meta.color}>{meta.text}</Tag>;
      },
    },
    { title: '创建时间', dataIndex: 'createdAt', valueType: 'dateTime', search: false, width: 170 },
    { title: '更新时间', dataIndex: 'updatedAt', valueType: 'dateTime', search: false, width: 170 },
    {
      title: '操作',
      valueType: 'option',
      // 定右列必须显式给宽度（表格靠它算钉住的那一块），宽度按三个链接按钮的最长一行量。
      width: 180,
      fixed: 'right',
      render: (_, row) => {
        // 只读的人进得来这一页，但看不到任何写入口——这正是 settlement:read 这一枚存在的理由。
        // 两个入口都要先取一次详情，所以两个都要转圈——见 loadDetail 的说明。
        const loading = loadingRuleId === row.id;
        const view = (
          <Button key="view" type="link" size="small" loading={loading} onClick={() => openView(row)}>
            查看
          </Button>
        );
        if (!access.canManageSettlement) return [view];
        return [
          view,
          <Button key="edit" type="link" size="small" loading={loading} onClick={() => openEdit(row)}>
            编辑
          </Button>,
          <Popconfirm
            key="delete"
            title="删除这条规则？"
            description="已经发生过的分账不受影响。如果它被分账引用过，删除会被拒——那就改成停用。"
            onConfirm={() => remove(row)}
          >
            <Button type="link" size="small" danger>
              删除
            </Button>
          </Popconfirm>,
        ];
      },
    },
  ];

  const itemColumns: ColumnsType<SettlementRuleItem> = [
    {
      title: '收款主体',
      dataIndex: 'partyType',
      width: 100,
      render: (_, row) => {
        const meta = enumMeta(PARTY_TYPE, row.partyType);
        return <Tag color={meta.color}>{meta.text}</Tag>;
      },
    },
    {
      title: '算法',
      dataIndex: 'calcType',
      width: 100,
      render: (_, row) => enumMeta(CALC_TYPE, row.calcType).text,
    },
    {
      title: '比例',
      dataIndex: 'ratioPercent',
      width: 90,
      align: 'right',
      // 固定额项与平台自留项的比例是 0（008 的 CHECK 钉着），显示成 % 会让人以为它分不到。
      render: (_, row) => (row.calcType === 'percent' ? formatPercent(row.ratioPercent) : '—'),
    },
    {
      title: '固定额',
      dataIndex: 'fixedAmount',
      width: 110,
      align: 'right',
      render: (_, row) => (row.calcType === 'fixed' ? money(row.fixedAmount) : '—'),
    },
    {
      // 平台项没有账户（008 的 CHECK 是等价式），那不是缺值。
      title: '收款账户',
      dataIndex: 'accountId',
      ellipsis: true,
      width: 240,
      render: (_, row) =>
        row.partyType === 'platform'
          ? '—（剩下的都归它）'
          : dash(row.accountName || row.accountId),
    },
    {
      title: '渠道接收方号',
      dataIndex: 'receiverId',
      ellipsis: true,
      width: 200,
      render: (_, row) => dash(row.receiverId),
    },
    { title: '备注', dataIndex: 'remark', ellipsis: true, width: 160 },
  ];

  const detailColumns: ProDescriptionsItemProps<SettlementRule>[] = [
    { title: '规则名称', dataIndex: 'name' },
    {
      title: '业务分类',
      dataIndex: 'bizType',
      render: (_, row) => enumMeta(BIZ_TYPE, row.bizType).text,
    },
    {
      title: '范围档位',
      dataIndex: 'scopeType',
      render: (_, row) => enumMeta(SCOPE_TYPE, row.scopeType).text,
    },
    {
      title: '范围',
      dataIndex: 'scopeRef',
      render: (_, row) => (scopeNeedsRef(row.scopeType) ? refName(refs, row.scopeRef) : '全部'),
    },
    {
      title: '分法',
      dataIndex: 'allocationMode',
      render: (_, row) => enumMeta(ALLOCATION_MODE, row.allocationMode).text,
    },
    {
      title: '状态',
      dataIndex: 'status',
      render: (_, row) => {
        const meta = enumMeta(RECORD_STATUS, row.status);
        return <Tag color={meta.color}>{meta.text}</Tag>;
      },
    },
    { title: '备注', dataIndex: 'remark', span: 2, render: (_, row) => dash(row.remark) },
    { title: '创建时间', dataIndex: 'createdAt', valueType: 'dateTime' },
    { title: '更新时间', dataIndex: 'updatedAt', valueType: 'dateTime' },
    // 主键摆出来：拿到它才能在库里直接查这条规则与它的项。
    { title: '规则 ID', dataIndex: 'id', copyable: true, span: 2 },
  ];

  return (
    <PageContainer
      title="分账规则"
      content="这类业务、这个范围上的钱怎么分。同一个业务分类下，档位从具体到宽泛先命中先返回；同档位只能有一条启用中的规则。分账随支付一次下发给渠道，支付成功即分账成功——所以这里配的就是下一次支付要用的分法。"
      extra={
        access.canManageSettlement
          ? [
              <Button key="create" type="primary" onClick={openCreate}>
                新建规则
              </Button>,
            ]
          : undefined
      }
    >
      <Alert
        type="info"
        showIcon
        style={{ marginBottom: 16 }}
        message="品牌与商品两档今天命不中"
        description="订单侧发起支付时只推门店与设备两个维度，所以按品牌、按商品配出来的规则不会命中任何一笔订单——列表上它看起来完全正常。另外，比例项合计超过 100% 是保存不了的，但那个 400 要点了保存才出现；表单里的实时合计在填的时候就把它显示出来，请对着它核一眼。"
      />

      <ProTable<SettlementRule>
        actionRef={actionRef}
        rowKey="id"
        columns={columns}
        // 1330 = 180+120+110+160+140+100+170+170+180，各列 width 之和。改任何一列的宽度都要
        // 同批改这个数（钉右列的不变式：fixed 列必须显式 width，scroll.x = 各列 width 之和）。
        scroll={{ x: 1330 }}
        search={{ labelWidth: 'auto' }}
        request={async (params) => {
          const query: SettlementRuleQuery = {
            page: params.current,
            pageSize: params.pageSize,
            name: exact(params.name),
            bizType: exact(params.bizType) as SettlementRuleQuery['bizType'],
            scopeType: exact(params.scopeType) as SettlementRuleQuery['scopeType'],
            status: exact(params.status) as SettlementRuleQuery['status'],
          };
          try {
            const result = await listSettlementRules(query);
            return { data: result.items, total: result.total, success: true };
          } catch (error) {
            // 后端对打错的筛选码回 400（不是空列表），且带着一句能看懂的话。默不作声地显示
            // 空表会让人以为「这类业务真的一条规则都没有」。
            message.error(requestErrorMessage(error, '加载分账规则失败'));
            return { data: [], total: 0, success: false };
          }
        }}
      />

      {/*
        只读的「查看」。

        它**不是**把编辑弹窗设成只读：规则的项在列表里没有，只有详情有，所以这一页要能看见
        「这条规则到底分给了谁多少」——而那恰恰是只读的人唯一需要的东西。用 ProDescriptions +
        一张普通的 antd Table 渲染，形状是确定的，不依赖表单控件的只读语义。
      */}
      <Modal
        key={`view-${viewing?.id ?? ''}`}
        title={`规则详情 · ${viewing?.name ?? ''}`}
        open={!!viewing}
        width={900}
        footer={null}
        destroyOnClose
        onCancel={() => setViewing(undefined)}
      >
        {viewing ? (
          <>
            <ProDescriptions<SettlementRule>
              column={2}
              dataSource={viewing}
              columns={detailColumns}
            />
            <Table<SettlementRuleItem>
              rowKey="id"
              style={{ marginTop: 16 }}
              size="small"
              pagination={false}
              dataSource={viewing.items}
              // 1000 = 100+100+90+110+240+200+160，同 itemColumns 各列 width 之和。
              scroll={{ x: 1000 }}
              columns={itemColumns}
              locale={{
                // 空数组是**几乎不会发生**的（008 要求一条规则至少有一项），但真出现时
                // 一句「暂无数据」会让人以为是页面坏了。
                emptyText: '这条规则没有分账项——它不会分出去任何钱',
              }}
            />
          </>
        ) : null}
      </Modal>

      {/*
        新建 / 编辑共用这一个弹窗。**key 与 destroyOnClose 两个都要有**——缺了它们，跨行编辑
        时上一条的字段会落到下一条上（全仓踩过一次的坑）。key 里带上 id，切行即重建。
      */}
      <ModalForm<SettlementRuleInput>
        key={`rule-${editing?.rule?.id ?? 'new'}`}
        title={editing?.rule ? '编辑规则' : '新建规则'}
        open={!!editing}
        formRef={formRef}
        // 1000 而不是 720：分账项那一行要摆下「主体｜算法｜比例｜账户｜备注」五个字段加删除按钮，
        // 窄了就会折行——折了行就等于回到原来那种「一路往下滚」的形态。
        width={1000}
        modalProps={{
          ...scrollableModalBody,
          destroyOnClose: true,
          maskClosable: false,
          onCancel: () => setEditing(null),
        }}
        initialValues={
          editing?.rule
            ? {
                name: editing.rule.name,
                bizType: editing.rule.bizType,
                scopeType: editing.rule.scopeType,
                scopeRef: editing.rule.scopeRef || undefined,
                allocationMode: editing.rule.allocationMode,
                status: editing.rule.status,
                remark: editing.rule.remark,
                items: editing.rule.items.map(itemToForm),
              }
            : {
                bizType: 'coffee',
                scopeType: 'store',
                allocationMode: 'normal',
                status: 'enabled',
                items: [emptyItem()],
              }
        }
        onFinish={submit}
      >
        <ProFormText
          name="name"
          label="规则名称"
          rules={[{ required: true, message: '请填写规则名称' }]}
          fieldProps={{ maxLength: 60 }}
          extra="给运营看的，不进渠道报文。"
        />
        <ProFormSelect
          name="bizType"
          label="业务分类"
          rules={[{ required: true, message: '请选择业务分类' }]}
          valueEnum={searchOptions(BIZ_TYPE)}
          extra="规则命中的第一段键。写错的表现是安静地命不中、整单归平台。"
        />
        <ProFormSelect
          name="scopeType"
          label="范围档位"
          rules={[{ required: true, message: '请选择范围档位' }]}
          valueEnum={searchOptions(SCOPE_TYPE)}
          fieldProps={{
            // **换档位要把范围引用清掉**：换的是同一个输入框在装别的东西（门店的那个 uuid
            // 变成设备档的引用），留着它不只是「没清干净」——档位与引用对不上时后端会按
            // 新档位去校验那个旧 id，报的错会指向一个用户没填过的字段。提交时那条「全局档
            // 归零」管不了这种情况（它管的是 global 与其余四档之间）。
            //
            // 与「比例/固定额切换时提交归零」不是一回事：那两个值是**同一个档位的两种算法**，
            // 提交时按算法挑一个即可；这里是**换了一个东西**，旧值没有一个是能用的。
            onChange: () => formRef.current?.setFieldValue('scopeRef', undefined),
          }}
          extra="同一个业务分类下，档位从设备到全局先命中先返回。同档位只能有一条启用中的规则。换档位会清掉已经选好的范围引用。"
        />
        {/*
          范围引用跟着档位出现：全局档**不要**它（008 的 CHECK 是充要条件，带了就是一条违规），
          其余四档**必须**给。门店 / 品牌 / 设备三档给的是下拉（名字认得出），商品档今天没有
          可选的来源，只能手填一个 id。
        */}
        <ProFormDependency name={['scopeType']}>
          {({ scopeType }) => {
            if (!scopeNeedsRef(scopeType)) return null;
            return (
              <>
                {scopeNeverHits(scopeType) ? (
                  <Alert
                    type="warning"
                    showIcon
                    style={{ marginBottom: 16 }}
                    message="这一档今天不会命中"
                    description="订单侧只推门店与设备两个维度，按品牌、按商品配出来的规则不会命中任何一笔订单。"
                  />
                ) : null}
                {scopeType === 'product' ? (
                  <ProFormText
                    name="scopeRef"
                    label="范围引用"
                    rules={[
                      { required: true, message: '请填写范围引用' },
                      {
                        // 后端会把非 uuid 的值拦成 400（不是 500），但那是绕远路：先把形状说清楚。
                        pattern: /^[0-9a-fA-F-]{36}$/,
                        message: '请填一个完整的 UUID',
                      },
                    ]}
                    extra="商品档。填商品 ID——这一档今天没有可选的来源，也不会命中。"
                  />
                ) : (
                  <ProFormSelect
                    name="scopeRef"
                    label="范围引用"
                    rules={[{ required: true, message: '请选择范围' }]}
                    options={
                      scopeType === 'device'
                        ? refs.devices
                        : scopeType === 'brand'
                          ? refs.brands
                          : refs.stores
                    }
                    fieldProps={{
                      showSearch: true,
                      optionFilterProp: 'label',
                      // 空下拉是这里最容易让人以为「坏了」的地方，所以把「为什么空」写清楚：
                      // 这几个列表各要一个权限码（门店 / 品牌 / 设备各一枚），取不到就是空的。
                      notFoundContent: (
                        <span style={{ color: '#999' }}>
                          没有可选的来源：这份列表要另一个权限码，或者库里还没有建过。
                        </span>
                      ),
                    }}
                    extra="这条规则管的就是这一个。牌子对不上时它不会命中，也不会报错。"
                  />
                )}
              </>
            );
          }}
        </ProFormDependency>
        <ProFormSelect
          name="allocationMode"
          label="分法"
          rules={[{ required: true, message: '请选择分法' }]}
          valueEnum={{
            normal: { text: '各自计算（剩下的归平台）' },
            fixed_then_remaining: { text: '先扣固定额，再按比例分' },
          }}
          extra="「先扣固定额」只影响比例项拿到的那部分基数，固定额本身不变。"
        />
        <ProFormSelect
          name="status"
          label="状态"
          rules={[{ required: true, message: '请选择状态' }]}
          valueEnum={{ enabled: { text: '已启用' }, disabled: { text: '已停用' } }}
          extra="只有启用中的规则参与命中。被分账引用过的规则删不掉，停用是常规出口——停用之后同档位就能再配一条。"
        />
        <ProFormTextArea
          name="remark"
          label="备注"
          fieldProps={{ maxLength: 200, rows: 2, showCount: true }}
        />

        {/*
          分账项。**整体提交**：「同规则下比例合计不超过 100%、平台项至多一条、remainder 只能
          给平台」是跨行约束，拆成子资源就没有任何一个时刻能在一个事务里看见完整的一套项。
        */}
        {/*
          分账项**横着排成一行**（`ProFormGroup` 是一行、每个字段一个 `width`）。

          原先这里每个字段各占一行、底下还挂一句 `extra`，**一项就有 478px 高**（实测）：一条规则
          三项 = 1400px+，弹窗里只能一路滚，而右边那 200px 一直是空的。所以这一块有两件事必须
          同时做，只做一件都不到底：

          - 字段横排（`ProFormGroup` + 每个字段的 `width`）；
          - 把 `extra` 换成 `tooltip`（挂在标签旁边的小问号上）。`extra` 是撑高的主因，留着它
            横排之后每一格仍然是三四行字。

          两条缺一不可：只横排、不换 tooltip，每一格仍然是三四行字；只换 tooltip、不横排，
          字段还是各占一行。
        */}
        <ProFormList
          name="items"
          label="分账项"
          min={1}
          creatorButtonProps={{ creatorButtonText: '增加一项' }}
          // 新增的那一行从「门店 + 按比例」起手，省两次点击。拖拽排序没有做——顺序按提交时的
          // 先后重编，所以别把顺序当成能调的东西。
          creatorRecord={emptyItem}
          copyIconProps={false}
          itemRender={({ listDom, action }) => (
            // 删除按钮摆到行尾。`min={1}` 保证最后一项删不掉，所以不用像入库单那样按 index 挡。
            <div style={{ display: 'flex', gap: 8, alignItems: 'flex-start' }}>
              <div style={{ flex: 1 }}>{listDom}</div>
              {action}
            </div>
          )}
        >
          <ProFormGroup spaceProps={{ size: 12 }}>
            <ProFormSelect
              name="partyType"
              label="收款主体"
              width={140}
              rules={[{ required: true, message: '请选择收款主体' }]}
              valueEnum={searchOptions(PARTY_TYPE)}
            />
            <ProFormSelect
              name="calcType"
              label="算法"
              width={130}
              rules={[{ required: true, message: '请选择算法' }]}
              valueEnum={searchOptions(CALC_TYPE)}
            />
            {/*
              比例与固定额**二选一**，按算法显示。提交时会按算法把另一个归零——008 与 service
              都要求它们互斥（比例项带固定额会被拒），而在切换算法之后留一个不再渲染的输入框，
              是最容易发生的一种半截组合。

              **两个都必须由这一个 ProFormDependency 产出**：`ProFormGroup` 只对**直接子元素**
              排格子，一个子元素占一格。写成两个条件子元素会让它们各占一格、切换算法时整行跟着
              左右跳。
            */}
            <ProFormDependency name={['calcType']}>
              {({ calcType }) =>
                calcType === 'fixed' ? (
                  <ProFormDigit
                    name="fixedAmount"
                    label="固定额（元）"
                    width={130}
                    rules={[{ required: true, message: '请填写固定额' }]}
                    min={0.01}
                    fieldProps={{ precision: 2, addonBefore: '¥' }}
                    tooltip="每一笔都先扣这个数，与订单金额无关。"
                  />
                ) : (
                  <ProFormDigit
                    name="ratioPercent"
                    label="比例（%）"
                    width={130}
                    rules={[{ required: true, message: '请填写比例' }]}
                    min={0.01}
                    max={100}
                    fieldProps={{ precision: 2 }}
                    tooltip="百分数，最多两位小数（45 表示 45.00%）。"
                  />
                )
              }
            </ProFormDependency>
            {/*
              平台项没有账户（008 的 CHECK 钉着），所以这一格在平台项时换成一句说明。用 antd 的
              `Form.Item` 而不是 `Alert`：它跟旁边那些字段是同一套标签＋控件的结构，行高一样、
              格子的宽度也一样——换成 Alert 会把这一行撑高、把后面的字段挤歪。
            */}
            <ProFormDependency name={['partyType']}>
              {({ partyType }) =>
                partyType === 'platform' ? (
                  // 外面这层 240 是**必须**的：`Form.Item` 没有 `width`，不给宽度它就缩到文字那么宽，
                  // 同一行后面的「备注」会被向左带 128px——平台项那一行与别的行对不齐。240 与下面
                  // 账户下拉的 width 是同一个数，两行才是同一套列。
                  <div style={{ width: 240 }}>
                    <Form.Item
                      label="收款账户"
                      tooltip="它拿的是剩下的那一份（基数减去其他项）。平台项一条规则里至多一个。"
                    >
                      <span style={{ color: 'rgba(0, 0, 0, 0.45)' }}>平台项不用选账户</span>
                    </Form.Item>
                  </div>
                ) : (
                  <ProFormSelect
                    name="accountId"
                    label="收款账户"
                    width={240}
                    rules={[{ required: true, message: '请选择收款账户' }]}
                    options={accountOptions}
                    fieldProps={{
                      showSearch: true,
                      optionFilterProp: 'label',
                      notFoundContent: (
                        <span style={{ color: '#999' }}>
                          没有可选的账户：这里只列启用中的账户。去「分账账户」里建一个并启用。
                        </span>
                      ),
                    }}
                    tooltip="只列启用中的账户。一条规则里的所有账户必须挂在同一条渠道上，混了会被拒。"
                  />
                )
              }
            </ProFormDependency>
            <ProFormText
              name="remark"
              label="备注"
              width={150}
              fieldProps={{ maxLength: 60 }}
              tooltip="给运营看的，不进渠道报文。"
            />
          </ProFormGroup>
        </ProFormList>

        {/*
          实时合计。**在提交之前就把话说出来**：后端会拒（400），但那个 400 要到点了保存才出现，
          而且只说「合计超了」，不说超的是哪两项；填的时候就显示，改哪一格心里有数。

          用整数百分点求和，避免 95.29 + 2.93 + 1.78 这种正好 100 的组合被浮点误判
          （见 settlementLabels.percentSumHundredths 的说明）。
        */}
        <ProFormDependency name={['items']}>
          {({ items }) => {
            const sum = percentSumHundredths(items ?? []);
            if (sum <= PERCENT_MAX_HUNDREDTHS) return null;
            return (
              <Alert
                type="error"
                showIcon
                message={`比例项合计 ${formatPercent(sum / 100)}，超过了 100%`}
                description="这样保存会被后端拒掉，请减掉一些、或者把多出来的那份改成「平台自留」那一条的剩余项。"
              />
            );
          }}
        </ProFormDependency>
      </ModalForm>
    </PageContainer>
  );
}
