import { PageContainer, ProTable } from '@ant-design/pro-components';
import type { ActionType, ProColumns } from '@ant-design/pro-components';
import { message } from 'antd';
import { useEffect, useRef, useState } from 'react';
import OperationLogDetailDrawer from '../../components/common/OperationLogDetailDrawer';
import OperationLogResultTag from '../../components/common/OperationLogResultTag';
import { actionText, adminUserText, moduleText } from '../../components/common/operationLogText';
import { toRFC3339 } from '../../services/datetime';
import {
  getOperationLogFacets,
  listOperationLogs,
  type OperationLog,
  type OperationLogFacets,
} from '../../services/operationLog';
import { listMerchants } from '../../services/merchant';
import { FULL_PAGE_PARAMS, toPageParams } from '../../services/pagination';
import { requestErrorMessage } from '../../services/requestError';

/**
 * 平台操作日志（全模块）。只读：这张表是审计证据，后端没有删除、清空之类的接口。
 *
 * 模块/动作/目标类型三张码表与详情抽屉都在 components/common 下：设备详情页的
 * 「操作日志」tab 用的是同一批，改码表时只需要改一处。
 */
export default function OperationLogsPage() {
  const actionRef = useRef<ActionType>();
  const [detail, setDetail] = useState<OperationLog>();
  // 候选值只在进页面时取一次：它来自历史数据，一次会话里不会变，而每翻一页都去
  // 取一遍等于给这张表加一份没必要的全表聚合。
  const [facets, setFacets] = useState<OperationLogFacets>({ modules: [], actions: [] });
  // 商户 id → 名称。日志里只有 id（跨库软指针），商品/门店/商户都要认人。
  const [merchantNames, setMerchantNames] = useState<Record<string, string>>({});

  useEffect(() => {
    getOperationLogFacets()
      .then(setFacets)
      // 取不到只影响两个下拉的候选值，列表本身还能用（关键词、操作人、时间照样筛），
      // 所以只提示不阻断。
      .catch((error) => message.error(requestErrorMessage(error, '加载筛选选项失败')));
    // 商户全集，只为了把详情里的商户 id 翻成名字。商户数量是个位/十位级，
    // 和 pages/coupon-templates 取商户名的做法一致；失败就显示 id。
    listMerchants(FULL_PAGE_PARAMS)
      .then(({ items }) => setMerchantNames(Object.fromEntries(items.map((item) => [item.id, item.name]))))
      .catch(() => undefined);
  }, []);

  const columns: ProColumns<OperationLog>[] = [
    {
      // 仅搜索用的时间范围。用独立的 dataIndex，不和下面那列 occurredAt 共用：
      // 同名的两列会在搜索表单里争同一个键，范围数组覆盖掉字符串之后，表格里的
      // 时间列就空了。transform 把它换成后端要的 startTime/endTime 两个参数。
      title: '时间范围',
      dataIndex: 'timeRange',
      valueType: 'dateTimeRange',
      hideInTable: true,
      search: {
        transform: (value: unknown) => {
          const range = (value ?? []) as unknown[];
          return { startTime: toRFC3339(range[0]), endTime: toRFC3339(range[1]) };
        },
      },
    },
    {
      // 仅搜索用：后端按「操作人用户名或姓名」的前缀匹配。
      title: '操作人',
      dataIndex: 'operator',
      hideInTable: true,
      fieldProps: { placeholder: '用户名或姓名前缀' },
    },
    {
      // 仅搜索用：匹配目标名称或操作描述。
      title: '关键词',
      dataIndex: 'keyword',
      hideInTable: true,
      fieldProps: { placeholder: '目标名称或操作描述' },
    },
    {
      title: '模块',
      dataIndex: 'module',
      valueType: 'select',
      width: 120,
      // valueEnum 只喂搜索下拉；表格里走下面的 render，好把没登记过的模块名原样显示。
      valueEnum: Object.fromEntries(
        facets.modules.map((value) => [value, { text: moduleText[value] ?? value }]),
      ),
      render: (_, row) => moduleText[row.module] ?? row.module,
    },
    {
      title: '动作',
      dataIndex: 'action',
      valueType: 'select',
      width: 120,
      valueEnum: Object.fromEntries(
        facets.actions.map((value) => [value, { text: actionText[value] ?? value }]),
      ),
      render: (_, row) => actionText[row.action] ?? row.action,
    },
    {
      title: '操作描述',
      dataIndex: 'operation',
      search: false,
      ellipsis: true,
      render: (_, row) => row.operation || '—',
    },
    {
      title: '操作人',
      dataIndex: 'adminUsername',
      search: false,
      width: 150,
      render: (_, row) => adminUserText(row),
    },
    {
      title: '目标',
      dataIndex: 'targetName',
      search: false,
      ellipsis: true,
      render: (_, row) => row.targetName || '—',
    },
    {
      title: '结果',
      dataIndex: 'result',
      valueType: 'select',
      width: 110,
      valueEnum: {
        success: { text: '成功' },
        failure: { text: '失败' },
      },
      render: (_, row) => <OperationLogResultTag log={row} />,
    },
    {
      title: '时间',
      dataIndex: 'occurredAt',
      // 交给 ProTable 按 dateTime 渲染（本地时区）。本页别自己拼时间字符串：
      // 同一个表里两种格式看着像两个系统拼出来的。
      valueType: 'dateTime',
      search: false,
      width: 170,
    },
    {
      title: '操作',
      valueType: 'option',
      fixed: 'right',
      width: 80,
      render: (_, row) => [
        <a key="detail" onClick={() => setDetail(row)}>
          详情
        </a>,
      ],
    },
  ];

  return (
    <PageContainer title="操作日志">
      <ProTable<OperationLog>
        actionRef={actionRef}
        rowKey="id"
        columns={columns}
        scroll={{ x: 1300 }}
        // 后端按 occurredAt 倒序（最近的在前），翻页也从近往远走。
        request={async (params) => {
          const result = await listOperationLogs(toPageParams(params));
          return { data: result.items, total: result.total, success: true };
        }}
        search={{ labelWidth: 'auto' }}
      />

      <OperationLogDetailDrawer
        log={detail}
        onClose={() => setDetail(undefined)}
        merchantNames={merchantNames}
      />
    </PageContainer>
  );
}
