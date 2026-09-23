import { PageContainer, ProTable } from '@ant-design/pro-components';
import type { ProColumns } from '@ant-design/pro-components';
import { history } from '@umijs/max';
import { Alert, Tag, Typography, message } from 'antd';
import { useEffect, useState } from 'react';
import { toRFC3339 } from '../../../services/datetime';
import { enumMeta, searchOptions } from '../../../services/labels';
import { formatYuan } from '../../../services/money';
import { requestErrorMessage } from '../../../services/requestError';
import {
  listSettlementTasks,
  type SettlementTask,
  type SettlementTaskQuery,
} from '../../../services/settlement';
import { SCOPE_TYPE, TASK_STATUS, scopeNeedsRef } from '../../../services/settlementLabels';
import { EMPTY_SETTLEMENT_REFS, loadSettlementRefs, refName, type SettlementRefs } from '../settlementRefs';

/**
 * 分账明细：**已经发生的那一笔分给了谁多少**。只读。
 *
 * # 这一页上的每一格都是当时那一刻的事实
 *
 * 分账任务的接收方明细全是**快照列**（`settlement_receivers` 的设计）：比例、金额、主体名、子商户号都是建任务那一刻
 * 冻结下来的。账户后来改了名、门店后来换了品牌，这里的数字都不跟着变——这是对的，「当时分给了
 * 谁」不该随现在的配置而变。所以这一页**不回查**规则与账户补当前值（只有规则名是 LEFT JOIN 出来
 * 的展示列）。
 *
 * # 「支付成功即分账成功」这句话在这一页上的样子
 *
 * 银联商务的分账指令随下单一次下发，成功应答本身就是分账成功的凭据，所以**没有「分账中」这个
 * 中间态**：任务要么 pending（还没付款）、要么 succeeded（付款成功，钱已经按子单分下去了）。
 * pending 的会一直留着——它等的是那笔支付，不是渠道。
 *
 * # 与「支付单」那一页的分工
 *
 * 支付单看的是「这笔钱是怎么收上来的」（出资行、渠道调用、回调报文），这一页看的是「收上来之后
 * 按什么分给了谁」。两边都有支付单号，详情里互链。
 */

/** 接口给的空值一律显示成「—」：留白和「有个空字符串」在表里分不出来。 */
const dash = (value?: string | null) => (value ? value : '—');

const money = (fen?: number | null) => `¥${formatYuan(fen)}`;

/** 精确匹配的筛选参数：发出去之前 trim，空的整个丢掉。 */
const exact = (value: unknown): string | undefined => {
  const trimmed = typeof value === 'string' ? value.trim() : '';
  return trimmed || undefined;
};

export default function SettlementTasksPage() {
  /** 引用字典（门店 / 品牌 / 商户 / 设备），给范围与归属门店那两列的名字用。 */
  const [refs, setRefs] = useState<SettlementRefs>(EMPTY_SETTLEMENT_REFS);

  useEffect(() => {
    // 取不到就是空字典，列里退回显示原始 id —— 这一页本身必须打得开（另一半是从
    // settlement:read 进来的，那枚码与门店/设备的读码是分开的）。四份请求各自兜住了，
    // 这里的 catch 是兜住「映射里的 bug」那一种：真抛出来的话，也别在控制台留一个没人
    // 处理的 rejection。
    void loadSettlementRefs()
      .then(setRefs)
      .catch(() => setRefs(EMPTY_SETTLEMENT_REFS));
  }, []);

  const columns: ProColumns<SettlementTask>[] = [
    {
      // 仅搜索用的时间范围。**独立 dataIndex，不能与下面的 createdAt 同名**：同名的两列会在
      // 搜索表单里争同一个键，范围数组覆盖掉字符串之后表格里的时间列就空了（后端收到数组则是
      // 直接 400）。这是仓库里踩过的坑。
      title: '创建时间',
      dataIndex: 'createdRange',
      valueType: 'dateTimeRange',
      hideInTable: true,
      search: {
        // 后端只认带时区的 RFC3339（纯日期串没有时区，「今天」是业务时区的今天）。
        transform: (value: unknown) => {
          const range = (value ?? []) as unknown[];
          return { createdFrom: toRFC3339(range[0]), createdTo: toRFC3339(range[1]) };
        },
      },
    },
    {
      title: '分账任务号',
      dataIndex: 'taskNo',
      copyable: true,
      ellipsis: true,
      width: 220,
      fieldProps: { placeholder: '支持模糊匹配，可只填一截' },
    },
    {
      title: '支付单号',
      dataIndex: 'paymentNo',
      copyable: true,
      ellipsis: true,
      width: 220,
      fieldProps: { placeholder: '支持模糊匹配，可只填一截' },
    },
    {
      // 订单号**不在分账库上**，后端 JOIN payments 才有——所以这一列的模糊匹配会走一次 JOIN，
      // 与另外两个单号不是同一条路。
      title: '订单号',
      dataIndex: 'orderNo',
      copyable: true,
      ellipsis: true,
      width: 220,
      fieldProps: { placeholder: '支持模糊匹配，可只填一截' },
    },
    {
      title: '分账基数',
      dataIndex: 'baseAmount',
      search: false,
      width: 110,
      align: 'right',
      render: (_, row) => money(row.baseAmount),
    },
    {
      // 平台自留是**差额倒挤**出来的。恒等式：分账基数 = 平台自留 + 各接收方之和，
      // 详情页里把三项摆在一起核。
      title: '平台自留',
      dataIndex: 'platformAmount',
      search: false,
      width: 110,
      align: 'right',
      render: (_, row) => money(row.platformAmount),
    },
    {
      title: '状态',
      dataIndex: 'status',
      valueType: 'select',
      valueEnum: searchOptions(TASK_STATUS),
      width: 110,
      render: (_, row) => {
        const meta = enumMeta(TASK_STATUS, row.status);
        return <Tag color={meta.color}>{meta.text}</Tag>;
      },
    },
    {
      // 空档位**不是异常**：没命中任何规则的那一笔就是这样（scope_type 是 `DEFAULT ''`，
      // 与 scope_ref 一起表示「这笔没走规则」）。所以空的时候给个「—」而不是一枚
      // 写着一横的灰标签——标签是给「有个档位值、只是我没见过」留的。
      title: '范围档位',
      dataIndex: 'scopeType',
      valueType: 'select',
      valueEnum: searchOptions(SCOPE_TYPE),
      width: 110,
      render: (_, row) => {
        if (!row.scopeType) return '—';
        const meta = enumMeta(SCOPE_TYPE, row.scopeType);
        return <Tag color={meta.color}>{meta.text}</Tag>;
      },
    },
    {
      // 全局档的范围引用是空串（`settlement_rules` 上那条 CHECK 是充要条件），显示成「全部」而不是「—」：
      // 那不是缺值，那是这条规则管所有店铺。
      title: '范围',
      dataIndex: 'scopeRef',
      search: false,
      ellipsis: true,
      width: 150,
      // **没有档位就既不是「全部」也不是某个范围**：`scope_type` 为空表示这一笔没走规则
      // （整单归平台），不是「规则管所有店铺」。少了外面这一层判断，没命中规则的每一笔都会
      // 在「范围」这一列显示成「全部」——那是这一页上最容易被读反的一格。
      render: (_, row) =>
        row.scopeType ? (scopeNeedsRef(row.scopeType) ? refName(refs, row.scopeRef) : '全部') : '—',
    },
    {
      // **建任务那一刻的快照**：门店后来改了归属，这一笔的分账还是按当时那家店算的。
      // 搜索框给的是下拉（后端只认 uuid，手打一个 uuid 是没法用的）。
      // **品牌与商户两列没有摆出来**：订单侧今天只推门店与设备两个维度，那两列永远是空的，
      // 摆出来就是两列恒为「—」的噪音。它们仍然能通过接口筛（SettlementTaskQuery 里有），
      // 只是没有界面入口——真要用的那一天在筛选区补两个下拉即可。
      title: '归属门店',
      dataIndex: 'storeRef',
      valueType: 'select',
      valueEnum: Object.fromEntries(refs.stores.map((store) => [store.value, { text: store.label }])),
      ellipsis: true,
      width: 170,
      render: (_, row) => refName(refs, row.storeRef),
    },
    {
      // **空是常态**：门店没配规则时这条任务照样建，整单归平台。所以这一列的空不表示出错了。
      title: '命中的规则',
      dataIndex: 'ruleId',
      search: false,
      ellipsis: true,
      width: 160,
      render: (_, row) => dash(row.ruleName),
    },
    { title: '创建时间', dataIndex: 'createdAt', valueType: 'dateTime', search: false, width: 170 },
    {
      title: '操作',
      valueType: 'option',
      width: 100,
      fixed: 'right',
      render: (_, row) => [
        // 用 history.push 而不是 <a href>：这是个 SPA，<a> 会整页重载。路径参数是任务的
        // **id**（uuid）不是单号——这一条接口是按主键查的。
        <Typography.Link
          key="view"
          onClick={() => history.push(`/payments/settlement-tasks/${row.id}`)}
        >
          详情
        </Typography.Link>,
      ],
    },
  ];

  return (
    <PageContainer
      title="分账明细"
      content="每一笔支付分账的那一次：分账基数、平台自留、以及分给了谁多少。这里的每一格都是当时那一刻的快照，账户与规则后来改了都不会回头变。分账随支付一次下发，支付成功即分账成功——所以没有「分账中」这个中间态。"
    >
      <Alert
        type="info"
        showIcon
        style={{ marginBottom: 16 }}
        message="「命中的规则」空着是常态"
        description="门店没配规则时，分账任务照样建：整单归平台，接收方一个都没有。所以这一列的空不表示出错——它表示这笔钱全留在平台上。"
      />

      <ProTable<SettlementTask>
        rowKey="id"
        columns={columns}
        // 1850 = 220+220+220+110+110+110+110+150+170+160+170+100，各列 width 之和。改任何一列的
        // 宽度都要同批改这个数（钉右列的不变式：fixed 列必须显式 width，scroll.x = 各列之和）。
        scroll={{ x: 1850 }}
        search={{ labelWidth: 'auto' }}
        request={async (params) => {
          const query: SettlementTaskQuery = {
            page: params.current,
            pageSize: params.pageSize,
            taskNo: exact(params.taskNo),
            paymentNo: exact(params.paymentNo),
            orderNo: exact(params.orderNo),
            status: exact(params.status) as SettlementTaskQuery['status'],
            scopeType: exact(params.scopeType) as SettlementTaskQuery['scopeType'],
            storeRef: exact(params.storeRef),
            createdFrom: params.createdFrom,
            createdTo: params.createdTo,
          };
          try {
            const result = await listSettlementTasks(query);
            return { data: result.items, total: result.total, success: true };
          } catch (error) {
            // 后端对不合法的筛选（不是 uuid 的 storeRef、打错的状态码）回 400，且带着一句能看懂的
            // 话。默不作声地显示空表会让人以为「这段时间真的一笔分账都没有」。
            message.error(requestErrorMessage(error, '加载分账明细失败'));
            return { data: [], total: 0, success: false };
          }
        }}
      />
    </PageContainer>
  );
}
