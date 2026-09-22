import { ProTable } from '@ant-design/pro-components';
import type { ProColumns } from '@ant-design/pro-components';
import { Descriptions, Tag, Typography, message } from 'antd';
import { toRFC3339 } from '../../services/datetime';
import { enumMeta, searchOptions } from '../../services/labels';
import {
  listPartnerCallLogs,
  type CallLogQuery,
  type Partner,
  type PartnerCallLog,
} from '../../services/partner';
import { PARTNER_ERROR_CODE } from '../../services/partnerLabels';
import { requestErrorMessage } from '../../services/requestError';
// dash / RawBlock 是支付单详情那几个页签的共用零件（它们与支付无关，是通用显示件：空值口径
// 与「原样贴一段 JSON」）。这一刀不把它们搬去 components/common——那要同时改六个支付页面的
// import，与本次改动无关。抄一份的代价是两处对「空值显示成什么」慢慢给出两种答案。
import { dash, RawBlock } from '../payments/detail/render';

/**
 * 调用日志。
 *
 * # 这是这一域唯一一条会随量增长的读路径
 *
 * 合作方每调一次就写一行，只增不改（后端连一条写它的路径都没有）。所以它**必须是服务端
 * 分页 + 服务端筛选**：一次拉全量迟早会撞上后端每页 200 的上限，而撞上的表现是「列表里少
 * 了一些记录」——不报错，只是排查时对不上账。合作方列表那边同理。
 *
 * # 报文在这一行的展开里，不占列
 *
 * 请求体与响应体是这张表存在的全部理由（「我发了」「我们没收到」对质时就靠它），但它们是
 * 几 KB 的 JSON——铺成列会把整张表挤成两行。所以放进展开行，用 RawBlock 原样贴出来。
 * 两段都已被 ingress 截断（8 KiB，截断处带显式标记），前端不再二次裁剪：裁出来的截断点比
 * 实际存储的多一处，查起来更绕。
 *
 * # 认不出调用方的那几条**不隐藏**
 *
 * 密钥查不到、头都没带的那种行，partnerId 是全零 UUID、partnerName 是空串。它们仍然要显示
 * ——那正是「有人在试」的痕迹，也是这一页与「合作方说他调过」对质时的另一半证据。
 */
type Props = {
  /**
   * 已经加载好的合作方（**全部**，不是列表那一页）：筛选项要能选到任意一家。
   */
  partners: Partner[];
};

/** 筛选值 trim 之后发，空的整个丢掉——多一个尾空格换来的是「查不到」。 */
const exact = (value: unknown): string | undefined => {
  const trimmed = typeof value === 'string' ? value.trim() : '';
  return trimmed || undefined;
};

/**
 * 状态码那一格的类型是 any（搜索表单与 URL 都可能给字符串）。
 *
 * 空串与 undefined 都当「没筛」；剩下的交给 Number()——真填了「abc」，后端会回一句
 * 「状态码必须是整数」（它只判形状，范围由 service 判），比在这里静默丢掉更像一件好事。
 */
const toStatusCode = (value: unknown): number | undefined => {
  if (value === undefined || value === null || value === '') return undefined;
  const parsed = Number(value);
  return Number.isNaN(parsed) ? undefined : parsed;
};

export default function CallLogsTable({ partners }: Props) {
  const columns: ProColumns<PartnerCallLog>[] = [
    {
      // 仅搜索用的时间范围。**独立 dataIndex，不能与下面那一列 createdAt 同名**：同名的两列
      // 会在搜索表单里争同一个键，范围数组覆盖掉字符串之后表格里的时间列就空了（后端收到
      // 数组则是直接 400）。这是仓库里踩过的坑，支付单列表上也是这么写的。
      title: '时间范围',
      dataIndex: 'createdRange',
      valueType: 'dateTimeRange',
      hideInTable: true,
      search: {
        // 后端只认带时区的 RFC3339（纯日期串没有时区，「今天」是业务时区的今天）。
        // transform 出来的两个键会并进 request 的 params。
        transform: (value: unknown) => {
          const range = (value ?? []) as unknown[];
          return { from: toRFC3339(range[0]), to: toRFC3339(range[1]) };
        },
      },
    },
    {
      title: '合作方',
      dataIndex: 'partnerId',
      valueType: 'select',
      hideInTable: true,
      fieldProps: {
        showSearch: true,
        optionFilterProp: 'label',
        options: partners.map((partner) => ({
          label: `${partner.name}（${partner.code}）`,
          value: partner.id,
        })),
      },
    },
    { title: '时间', dataIndex: 'createdAt', valueType: 'dateTime', search: false, width: 170 },
    {
      title: '合作方',
      dataIndex: 'partnerName',
      search: false,
      width: 180,
      ellipsis: true,
      // 认不出调用方的那几条（密钥查不到、头都没带）名字是空串。显示成「未知调用方」而不是
      // 「—」：它们不是「这一格没值」，而是「我们不知道是谁」——那正是要查的东西。
      render: (_, row) =>
        row.partnerName || <Typography.Text type="secondary">未知调用方</Typography.Text>,
    },
    {
      title: '密钥',
      dataIndex: 'apiKeyMask',
      search: false,
      width: 150,
      render: (_, row) => (row.apiKeyMask ? <Typography.Text code>{row.apiKeyMask}</Typography.Text> : '—'),
    },
    { title: '方法', dataIndex: 'method', search: false, width: 80 },
    {
      title: '路径',
      dataIndex: 'path',
      search: false,
      width: 240,
      ellipsis: true,
      // 查询串在这条路径的展开行里（它是路径的一部分，但常常长到不能进列）。
    },
    { title: '来源 IP', dataIndex: 'requestIp', search: false, width: 140 },
    {
      title: '状态码',
      dataIndex: 'statusCode',
      valueType: 'digit',
      width: 100,
      render: (_, row) => {
        // 3xx 也要看着像「不是正常结果」：这一域没有重定向语义，出现 3xx 本身就是异常。
        const color =
          row.statusCode < 300 ? 'success' : row.statusCode < 400 ? 'warning' : 'error';
        return <Tag color={color}>{row.statusCode}</Tag>;
      },
      fieldProps: { placeholder: '如 401' },
    },
    {
      title: '耗时',
      dataIndex: 'durationMs',
      search: false,
      width: 90,
      render: (_, row) => `${row.durationMs} ms`,
    },
    {
      title: '失败原因',
      dataIndex: 'errorCode',
      valueType: 'select',
      // 空串 = 这次**没有出错**（验签通过），不是「这一格没值」。所以成功的行显示「—」，
      // 而不是一个「未知」标签。
      valueEnum: searchOptions(PARTNER_ERROR_CODE),
      width: 190,
      render: (_, row) => {
        const meta = enumMeta(PARTNER_ERROR_CODE, row.errorCode);
        return <Tag color={meta.color}>{meta.text}</Tag>;
      },
    },
  ];

  return (
    <ProTable<PartnerCallLog>
      headerTitle="调用日志"
      rowKey="id"
      columns={columns}
      // 1340 = 170+180+150+80+240+140+100+90+190，各列 width 之和（操作列没有，所以没有钉右列）。
      scroll={{ x: 1340 }}
      search={{ labelWidth: 'auto' }}
      // 只增表，所以不提供「默认按 id 排」之外的排序入口：后端没有排序参数，摆一个点了没用的
      // 箭头比不摆更糟。
      options={false}
      pagination={{ defaultPageSize: 20, showSizeChanger: true }}
      expandable={{
        expandedRowRender: (row) => (
          // 这三个值是排查时的全部一手材料，原样贴出来。查询串单独一格：它常常是「他到底传了
          // 什么参数」唯一的证据，而路径里已经看不到它（后端把 path 与 query 分开存）。
          <Descriptions column={1} size="small" bordered>
            <Descriptions.Item label="查询串">{dash(row.query)}</Descriptions.Item>
            <Descriptions.Item label="请求体">
              <RawBlock value={row.requestBody} maxHeight={240} />
            </Descriptions.Item>
            <Descriptions.Item label="响应体">
              <RawBlock value={row.responseBody} maxHeight={240} />
            </Descriptions.Item>
          </Descriptions>
        ),
      }}
      request={async (params) => {
        // 逐字段挑，不整个透传：搜索表单里的 createdRange 不是接口参数，透传过去只是给后端
        // 多几个它不认识的查询键。
        const query: CallLogQuery = {
          page: params.current,
          pageSize: params.pageSize,
          partnerId: exact(params.partnerId),
          errorCode: exact(params.errorCode),
          statusCode: toStatusCode(params.statusCode),
          from: params.from,
          to: params.to,
        };
        try {
          const result = await listPartnerCallLogs(query);
          return { data: result.items, total: result.total, success: true };
        } catch (error) {
          // 不合法的筛选（不是 uuid 的合作方、不是整数的状态码、词表外的错误码）后端回 400
          // 且带着一句能看懂的话。默不作声地显示空表会让人以为「这段时间真的一次调用都没有」。
          message.error(requestErrorMessage(error, '加载调用日志失败'));
          return { data: [], total: 0, success: false };
        }
      }}
    />
  );
}
