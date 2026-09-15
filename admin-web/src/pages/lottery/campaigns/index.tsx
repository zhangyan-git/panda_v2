import {
  ModalForm,
  PageContainer,
  ProFormDateTimePicker,
  ProFormDigit,
  ProFormList,
  ProFormSelect,
  ProFormText,
  ProFormTextArea,
  ProTable,
} from '@ant-design/pro-components';
import type { ActionType, ProColumns } from '@ant-design/pro-components';
import { history, useAccess } from '@umijs/max';
import { Button, message, Popconfirm, Tag, Typography } from 'antd';
import { useEffect, useRef, useState } from 'react';
import { formatDateTime, toRFC3339 } from '../../../services/datetime';
import { enumMeta, searchOptions } from '../../../services/labels';
import {
  createCampaign,
  getCampaign,
  listActivations,
  listCampaigns,
  updateCampaign,
  updateCampaignStatus,
  type Campaign,
  type CampaignInput,
  type CampaignQuery,
  type CampaignStatus,
  type PrizeInput,
} from '../../../services/lottery';
import { CAMPAIGN_STATUS, PRIZE_KIND, roundProgressLabel } from '../../../services/lotteryLabels';
import { FULL_PAGE_PARAMS } from '../../../services/pagination';
import { requestErrorMessage } from '../../../services/requestError';

/**
 * 抽奖活动列表。
 *
 * 「开通挂门店，活动再分粒度」——所以一个活动的归属由**两段**决定：它属于哪条开通记录
 * （也就是哪家门店），以及它是不是只对**该门店下某一台咖啡机**开放。后者落在可空的
 * machineId 上：为空就是门店级，有值就是那台设备专属。
 *
 * 结构上不写成 scopeType + scopeId 两列，是为了让「某台咖啡机的活动不属于本门店」这种
 * 组合**写不出来**——门店来自 activation，设备只能在这条线之下。
 *
 * 奖池是活动的一部分，跟着活动一起整份提交：改奖池就是改**将来中奖的名额与奖品**，
 * 而名额总数正是开奖要用的那个数（开期时冻结成当期的 winner_count）。
 */

/** 接口给的空值一律显示成「—」：留白和「有个空字符串」在表里分不出来。 */
const dash = (value?: string | null) => (value ? value : '—');

/** 精确匹配的筛选参数：发出去之前 trim，空的整个丢掉（后端是等值比较，不是模糊）。 */
const exact = (value: unknown): string | undefined => {
  const trimmed = typeof value === 'string' ? value.trim() : '';
  return trimmed || undefined;
};

/** 活动表单。prizes 的顺序就是 sortOrder，提交时才落成数字，所以这里不带它。 */
type CampaignForm = {
  activationId: string;
  machineId?: string;
  code: string;
  name: string;
  participantTarget: number;
  description?: string;
  window: [unknown, unknown];
  status: CampaignStatus;
  prizes: Omit<PrizeInput, 'sortOrder'>[];
};

/** 新建时的空奖池：留一行，免得运营面对一个空表和一句「至少一个奖品」。 */
const DEFAULT_PRIZES: CampaignForm['prizes'] = [
  { prizeKind: 'custom', name: '', quantity: 1, claimInstructions: '', couponTemplateId: '', imageUrl: '' },
];

export default function LotteryCampaignsPage() {
  const access = useAccess();
  const actionRef = useRef<ActionType>();
  const [activations, setActivations] = useState<{ id: string; locationName: string }[]>([]);
  const [open, setOpen] = useState(false);
  // 正在编辑的活动**详情**（带奖池）。列表那一行不带 prizes，所以编辑必须先把详情取回来
  // ——否则保存时提交的奖池是空的，等于把奖池清空了。
  const [editing, setEditing] = useState<Campaign>();
  const [loadingDetail, setLoadingDetail] = useState(false);

  useEffect(() => {
    void (async () => {
      try {
        const { items } = await listActivations(FULL_PAGE_PARAMS);
        setActivations(items);
      } catch {
        // 忽略：读开通记录要 lottery:read，能进这一页的人一定有；真失败了表单里的门店
        // 下拉就是空的，运营看得出「选不了店」而不是被静默放进一个错的活动。
      }
    })();
  }, []);

  const activationOptions = activations.map((item) => ({
    label: item.locationName || item.id,
    value: item.id,
  }));

  /**
   * 打开编辑。**先取详情再开弹窗**：弹窗一开就得把奖池填进去，而列表行里没有它。
   *
   * 取不到就报错并**不开弹窗**——开一个奖池空着的编辑框，运营点保存就把奖池清空了，
   * 而那正是开奖要用的名额。
   */
  const openEdit = async (id: string) => {
    setLoadingDetail(true);
    try {
      setEditing(await getCampaign(id));
      setOpen(true);
    } catch (error) {
      message.error(requestErrorMessage(error, '加载活动详情失败'));
    } finally {
      setLoadingDetail(false);
    }
  };

  const setStatus = async (row: Campaign, status: CampaignStatus) => {
    try {
      await updateCampaignStatus(row.id, status);
      message.success('已更新');
      actionRef.current?.reload();
    } catch (error) {
      message.error(requestErrorMessage(error, '操作失败，请稍后重试'));
    }
  };

  const columns: ProColumns<Campaign>[] = [
    {
      title: '活动名',
      dataIndex: 'name',
      ellipsis: true,
      width: 160,
      fieldProps: { placeholder: '活动名关键词' },
      render: (_, row) => (
        <Typography.Link onClick={() => history.push(`/lottery/campaigns/${row.id}`)}>
          {row.name}
        </Typography.Link>
      ),
    },
    {
      // 短名是期次号的前缀（`{code}-{seq:04d}`），建出来就不可改：已经开出去的期次号里
      // 嵌着它，改一个字母会让历史期次号与活动对不上。后端 CampaignQuery 也没有按它筛的
      // 能力，所以这一列纯展示。
      title: '短名',
      dataIndex: 'code',
      search: false,
      copyable: true,
      width: 120,
    },
    {
      // 搜索发出去的键是 locationId（后端按它筛），表格里显示接口自带的 locationName。
      // 与开通页、订单页同一写法。
      title: '门店',
      dataIndex: 'locationId',
      ellipsis: true,
      width: 160,
      fieldProps: { placeholder: '完整门店 ID' },
      render: (_, row) => dash(row.locationName),
    },
    {
      // 空 = 门店级。这一列是「活动分粒度」在界面上的全部体现，所以「门店级」三个字要
      // 写出来而不是留白——留白会让人以为这一格没数据。
      title: '适用设备',
      dataIndex: 'machineId',
      ellipsis: true,
      width: 160,
      fieldProps: { placeholder: '完整设备 ID' },
      render: (_, row) => (row.machineId ? row.machineId : '门店级'),
    },
    {
      title: '状态',
      dataIndex: 'status',
      valueType: 'select',
      valueEnum: searchOptions(CAMPAIGN_STATUS),
      width: 90,
      render: (_, row) => {
        const meta = enumMeta(CAMPAIGN_STATUS, row.status);
        return <Tag color={meta.color}>{meta.text}</Tag>;
      },
    },
    {
      title: '参与门槛',
      dataIndex: 'participantTarget',
      search: false,
      width: 100,
      render: (_, row) => `${row.participantTarget} 人`,
    },
    {
      // 奖池总名额 = SUM(prizes.quantity)，也就是下一期的 winner_count。运营核对奖池配得
      // 对不对，看的就是这个数。
      title: '奖池名额',
      dataIndex: 'prizeTotalQuantity',
      search: false,
      width: 100,
      render: (_, row) => `${row.prizeTotalQuantity} 个`,
    },
    {
      title: '进行中的期次',
      dataIndex: 'liveRoundNo',
      search: false,
      width: 170,
      render: (_, row) =>
        row.liveRoundId ? (
          <span>
            {row.liveRoundNo}
            <Typography.Text type="secondary">
              {' '}
              {roundProgressLabel(row.liveRoundDone, row.liveRoundSize)}
            </Typography.Text>
          </span>
        ) : (
          '—'
        ),
    },
    {
      title: '已开期数',
      dataIndex: 'roundCount',
      search: false,
      width: 100,
      render: (_, row) => `${row.roundCount} 期`,
    },
    {
      title: '活动窗口',
      dataIndex: 'window',
      search: false,
      width: 200,
      render: (_, row) => (
        <Typography.Text type="secondary">
          {formatDateTime(row.startAt)} ~ {formatDateTime(row.endAt)}
        </Typography.Text>
      ),
    },
    {
      title: '操作',
      valueType: 'option',
      // fixed 的列必须显式给宽度，且下面的 scroll.x 必须等于各列宽度之和（见下面的 1550）。
      // 190 是量出来的：三个 link 按钮（详情 / 编辑 / 暂停）并排内容宽约 174，加左右各
      // 8px 内边距。**钉右列宽度给窄了的后果是表格 scrollWidth 被顶大**，与 scroll.x
      // 声明的数对不上（这个仓库踩过）。
      width: 190,
      fixed: 'right',
      render: (_, row) => {
        const actions: React.ReactNode[] = [
          <Button
            key="detail"
            type="link"
            size="small"
            onClick={() => history.push(`/lottery/campaigns/${row.id}`)}
          >
            详情
          </Button>,
        ];
        if (access.canManageLottery) {
          actions.push(
            <Button key="edit" type="link" size="small" loading={loadingDetail} onClick={() => void openEdit(row.id)}>
              编辑
            </Button>,
          );
          // 只给「进行中」的提供暂停、给「已暂停」的提供恢复。草稿与已结束不在这里动：
          // 草稿要先编辑成启用，已结束是终态。
          if (row.status === 'enabled') {
            actions.push(
              <Popconfirm
                key="pause"
                title="暂停这个活动？"
                description="暂停后不再开新期，正在跑的这一期会走完并正常开奖。已开出的中奖记录不受影响。"
                okText="暂停"
                cancelText="取消"
                onConfirm={() => setStatus(row, 'paused')}
              >
                <Button type="link" size="small" danger>
                  暂停
                </Button>
              </Popconfirm>,
            );
          } else if (row.status === 'paused') {
            actions.push(
              <Button key="resume" type="link" size="small" onClick={() => setStatus(row, 'enabled')}>
                恢复
              </Button>,
            );
          }
        }
        return actions;
      },
    },
  ];

  return (
    <PageContainer
      title="抽奖活动"
      content="活动挂在门店下，可以只对某台咖啡机开放。每个活动带着一份奖池，奖池里的名额总数就是每一期的中奖名额。"
      extra={
        access.canManageLottery
          ? [
              <Button
                key="create"
                type="primary"
                onClick={() => {
                  setEditing(undefined);
                  setOpen(true);
                }}
              >
                新建活动
              </Button>,
            ]
          : undefined
      }
    >
      <ProTable<Campaign>
        actionRef={actionRef}
        rowKey="id"
        columns={columns}
        // 1550 = 160+120+160+160+90+100+100+170+100+200+190，各列 width 之和。
        scroll={{ x: 1550 }}
        search={{ labelWidth: 'auto' }}
        options={false}
        request={async (params) => {
          const query: CampaignQuery = {
            page: params.current,
            pageSize: params.pageSize,
            locationId: exact(params.locationId),
            machineId: exact(params.machineId),
            status: exact(params.status) as CampaignQuery['status'],
            name: exact(params.name),
          };
          const result = await listCampaigns(query);
          return { data: result.items, total: result.total, success: true };
        }}
      />

      {/*
        **destroyOnClose 是必须的**，而且这里比别处更要紧：奖池是一个动态列表，不销毁的话
        上一次编辑留下的奖品行会留在表单里，下一次新建时会带着别人的奖池一起提交——
        那等于凭空改了将来中奖的名额。

        key 跟着编辑对象走，两重保险：换一个活动编辑时表单整个重建，不会残留上一份奖池。
      */}
      <ModalForm<CampaignForm>
        key={editing?.id ?? 'new'}
        title={editing ? `编辑活动 ${editing.name}` : '新建抽奖活动'}
        open={open}
        onOpenChange={(value) => {
          setOpen(value);
          if (!value) setEditing(undefined);
        }}
        modalProps={{ destroyOnClose: true, width: 760 }}
        initialValues={
          editing
            ? {
                activationId: editing.activationId,
                machineId: editing.machineId ?? undefined,
                code: editing.code,
                name: editing.name,
                participantTarget: editing.participantTarget,
                description: editing.description,
                window: [editing.startAt, editing.endAt],
                status: editing.status,
                prizes: (editing.prizes ?? []).map((prize) => ({
                  prizeKind: prize.prizeKind,
                  name: prize.name,
                  quantity: prize.quantity,
                  claimInstructions: prize.claimInstructions,
                  couponTemplateId: prize.couponTemplateId,
                  imageUrl: prize.imageUrl,
                })),
              }
            : {
                status: 'draft' as CampaignStatus,
                participantTarget: 10,
                prizes: DEFAULT_PRIZES,
              }
        }
        onFinish={async (values) => {
          const window = (values.window ?? []) as unknown[];
          const startAt = toRFC3339(window[0]);
          const endAt = toRFC3339(window[1]);
          if (!startAt || !endAt) {
            message.error('请选择活动窗口');
            return false;
          }
          const payload: CampaignInput = {
            activationId: values.activationId,
            machineId: values.machineId?.trim() || null,
            code: values.code.trim().toUpperCase(),
            name: values.name.trim(),
            participantTarget: values.participantTarget,
            description: values.description?.trim(),
            startAt,
            endAt,
            status: values.status,
            // sortOrder 取行序：哪个奖排前面是运营的意思（开奖时按它依次分配名额），
            // 让他手填一个数字只会填出重复与空洞。
            prizes: (values.prizes ?? []).map((prize, index) => ({
              sortOrder: index,
              prizeKind: prize.prizeKind,
              name: prize.name.trim(),
              quantity: prize.quantity,
              claimInstructions: prize.claimInstructions?.trim() ?? '',
              couponTemplateId: prize.couponTemplateId?.trim() ?? '',
              imageUrl: prize.imageUrl?.trim() ?? '',
            })),
          };
          try {
            if (editing) {
              await updateCampaign(editing.id, payload);
            } else {
              await createCampaign(payload);
            }
          } catch (error) {
            message.error(requestErrorMessage(error, '保存失败，请稍后重试'));
            return false;
          }
          message.success(editing ? '已保存' : '已创建');
          actionRef.current?.reload();
          return true;
        }}
      >
        <ProFormSelect
          name="activationId"
          label="所属门店"
          options={activationOptions}
          showSearch
          // 编辑时禁用：活动不能换门店（换了等于新建一个），后端也整个忽略这个字段。
          disabled={!!editing}
          rules={[{ required: true, message: '请选择门店' }]}
          fieldProps={{
            filterOption: (input: string, option) =>
              String(option?.label ?? '')
                .toLowerCase()
                .includes(input.toLowerCase()),
          }}
        />
        <ProFormText
          name="machineId"
          label="适用设备"
          placeholder="留空 = 全门店可用；填设备 ID = 只对这台咖啡机开放"
          // 设备选择器要拉整份设备列表，而这一格大多数活动都不填。留一个填 ID 的输入框
          // 比为了少数活动给每次打开弹窗都拉一次设备表划算。
          tooltip="填了就是设备级活动：只有从这台咖啡机扫进来的参与算数。留空是门店级。"
        />
        <ProFormText
          name="code"
          label="活动短名"
          rules={[
            { required: true, message: '请输入活动短名' },
            // 与后端同一条规则：1-16 位大写字母或数字。它会是期次号的前缀（如 LAKE-0001），
            // 所以不接受小写与空格——那些会让期次号出现两种写法。
            { pattern: /^[A-Za-z0-9]{1,16}$/, message: '1-16 位字母或数字，不能有空格与符号' },
          ]}
          fieldProps={{ placeholder: '如 LAKE，会变成期次号 LAKE-0001' }}
          tooltip="建出来之后不可改：已经开出去的期次号里嵌着它。"
        />
        <ProFormText
          name="name"
          label="活动名"
          rules={[{ required: true, message: '请输入活动名' }]}
          fieldProps={{ maxLength: 50 }}
        />
        <ProFormDigit
          name="participantTarget"
          label="参与门槛"
          tooltip="每一期收满这么多人就停止收人并开奖。开期时会冻结到那一期上，所以改它只影响之后的期次。"
          min={1}
          max={100000}
          rules={[{ required: true, message: '请输入参与门槛' }]}
          fieldProps={{ precision: 0 }}
        />
        <ProFormDateTimePicker
          name="window"
          label="活动窗口"
          tooltip="整场活动的起止时间，期次在这个区间里滚动。窗口过了活动会自动结束。"
          rules={[{ required: true, message: '请选择活动窗口' }]}
          fieldProps={{ style: { width: '100%' } }}
        />
        <ProFormSelect
          name="status"
          label="状态"
          options={[
            { label: '草稿（不开期）', value: 'draft' },
            { label: '进行中（开始开期收人）', value: 'enabled' },
            { label: '已暂停', value: 'paused' },
            { label: '已结束', value: 'ended' },
          ]}
          rules={[{ required: true, message: '请选择状态' }]}
        />
        <ProFormTextArea
          name="description"
          label="活动说明"
          placeholder="会显示在小程序抽奖中心，可留空"
          fieldProps={{ rows: 2, maxLength: 200 }}
        />

        {/*
          奖池。**整份替换**：加一行删一行之后点保存，提交的是这份完整清单。所以每一行的
          顺序就是它的 sortOrder，开奖按这个顺序依次把名额发完（第一档拿满才轮到下一档）。
        */}
        <ProFormList
          name="prizes"
          label="奖池"
          tooltip="按顺序发奖：排在前面的档先拿满自己的名额，才轮到下一档。名额总数就是每一期的中奖名额。"
          creatorButtonProps={{ creatorButtonText: '加一个奖品' }}
          min={1}
          copyIconProps={false}
          itemRender={({ listDom, action }, { index }) => (
            <div style={{ display: 'flex', alignItems: 'flex-start', gap: 8 }}>
              <Typography.Text type="secondary" style={{ marginTop: 6, width: 24 }}>
                {index + 1}
              </Typography.Text>
              <div style={{ flex: 1 }}>{listDom}</div>
              <div style={{ marginTop: 6 }}>{action}</div>
            </div>
          )}
        >
          <ProFormSelect
            name="prizeKind"
            label="奖品类型"
            options={Object.entries(PRIZE_KIND).map(([value, meta]) => ({
              label: meta.text,
              value,
            }))}
            rules={[{ required: true, message: '请选择奖品类型' }]}
            width="sm"
          />
          <ProFormText
            name="name"
            label="奖品名称"
            rules={[{ required: true, message: '请输入奖品名称' }]}
            fieldProps={{ maxLength: 50, placeholder: '如 10 元咖啡兑换券' }}
            width="sm"
          />
          <ProFormDigit
            name="quantity"
            label="名额"
            tooltip="这一档发几个。所有档位加起来就是每一期的中奖名额。"
            min={1}
            max={100000}
            rules={[{ required: true, message: '请输入名额' }]}
            fieldProps={{ precision: 0 }}
            width="sm"
          />
          <ProFormText
            name="claimInstructions"
            label="领取说明"
            fieldProps={{ maxLength: 100, placeholder: '如 到门店出示凭证号即可' }}
          />
        </ProFormList>
      </ModalForm>
    </PageContainer>
  );
}
