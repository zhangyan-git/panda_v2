import {
  ModalForm,
  PageContainer,
  ProFormDateTimePicker,
  ProFormDigit,
  ProFormSelect,
  ProFormText,
  ProFormTextArea,
  ProTable,
} from '@ant-design/pro-components';
import type { ActionType, ProColumns } from '@ant-design/pro-components';
import { history, useAccess } from '@umijs/max';
import { Button, message, Popconfirm, Tag, Typography } from 'antd';
import { useEffect, useRef, useState } from 'react';
import { toRFC3339 } from '../../../services/datetime';
import { enumMeta, searchOptions } from '../../../services/labels';
import {
  activateLocation,
  listActivations,
  updateActivation,
  type Activation,
  type ActivationQuery,
  type ActivateInput,
} from '../../../services/lottery';
import { ACTIVATION_STATUS, roundProgressLabel } from '../../../services/lotteryLabels';
import { FULL_PAGE_PARAMS } from '../../../services/pagination';
import { requestErrorMessage } from '../../../services/requestError';
import { listStores, type Store } from '../../../services/store';

/**
 * 开通门店抽奖。
 *
 * 这一页是抽奖域的入口：**开通是一个动作**，它同时建出默认活动与第一期。所以这一个按钮
 * 按下去之后，这家店就已经在收参与了——表单里那个门槛填的不是「将来某一期的门槛」，而是
 * **第一期**的门槛（开期时冻结到期次上，之后每期的默认值从这里继承）。
 *
 * 开通挂在门店上、活动再分粒度（门店级 / 该门店下某台咖啡机）。所以这一页只管「这家店开
 * 没开通」，具体跑到哪台设备是活动的事。
 *
 * 已开奖的期次是终局：停用不会动它们，只是不再开新期。
 */

/** 接口给的空值一律显示成「—」：留白和「有个空字符串」在表里分不出来。 */
const dash = (value?: string | null) => (value ? value : '—');

/**
 * 精确匹配的筛选参数：发出去之前 trim，空的整个丢掉。
 *
 * locationId 是 uuid 上的等值比较，多一个尾空格换来的是「查不到」而不是模糊匹配——而
 * 调用方只会以为这家店没开通。
 */
const exact = (value: unknown): string | undefined => {
  const trimmed = typeof value === 'string' ? value.trim() : '';
  return trimmed || undefined;
};

/** 开通表单。 */
type ActivateForm = {
  locationId: string;
  campaignName?: string;
  participantTarget?: number;
  window?: [unknown, unknown];
  remark?: string;
};

export default function LotteryActivationsPage() {
  const access = useAccess();
  const actionRef = useRef<ActionType>();
  const [stores, setStores] = useState<Store[]>([]);
  // 开通弹窗的开合。开通只有一个动作，不像别的页面要区分「新建 / 编辑」，所以用布尔量
  // 而不是「正在编辑的那一行」。
  const [activating, setActivating] = useState(false);
  // 正在停用 / 启用哪一行（串行化那一下点击，避免连点发出两个相反的请求）。
  const [switching, setSwitching] = useState<string>();

  useEffect(() => {
    void (async () => {
      try {
        const { items } = await listStores(FULL_PAGE_PARAMS);
        setStores(items);
      } catch {
        // 忽略：门店列表要 admin:stores:view，只有抽奖权限的人不一定有。取不到就退回
        // 「填门店 ID」——开通表单的候选为空时会有另一条输入路径（见下面的 ProFormSelect）。
      }
    })();
  }, []);

  const storeOptions = stores.map((store) => ({
    label: `${store.name}（${store.city || '—'}）`,
    value: store.id,
  }));

  /**
   * 停用 / 启用。两个方向共用一次请求（后端是同一个 POST /status），但确认文案不一样：
   * 停用是**让人停下正在跑的活动**，得说清它不会撤销已开出的中奖记录。
   */
  const switchStatus = async (row: Activation, next: 'enabled' | 'disabled') => {
    setSwitching(row.id);
    try {
      await updateActivation(row.id, { status: next, remark: row.remark });
      message.success(next === 'enabled' ? '已恢复开通' : '已停用');
      actionRef.current?.reload();
    } catch (error) {
      message.error(requestErrorMessage(error, '操作失败，请稍后重试'));
    } finally {
      setSwitching(undefined);
    }
  };

  const columns: ProColumns<Activation>[] = [
    {
      // 搜索发出去的键是 `name`（后端按门店名做模糊匹配），表格里显示的是接口自带的
      // locationName。**两者不是同一个字段**：开通记录上存的是开通那一刻的名字快照，
      // 商户后来改了店名它也不会变（见 dto.ActivateRequest 的说明）。
      title: '门店',
      dataIndex: 'name',
      ellipsis: true,
      width: 180,
      fieldProps: { placeholder: '店名关键词' },
      render: (_, row) => dash(row.locationName),
    },
    {
      // 门店 ID 只用于搜索：uuid 上的等值比较，不做前缀匹配（那没有意义）。表格里已经有
      // 店名了，再摆一列 uuid 只会把表拉宽。
      title: '门店 ID',
      dataIndex: 'locationId',
      hideInTable: true,
      fieldProps: { placeholder: '完整门店 ID' },
    },
    {
      title: '状态',
      dataIndex: 'status',
      valueType: 'select',
      valueEnum: searchOptions(ACTIVATION_STATUS),
      width: 90,
      render: (_, row) => {
        const meta = enumMeta(ACTIVATION_STATUS, row.status);
        return <Tag color={meta.color}>{meta.text}</Tag>;
      },
    },
    {
      // 默认活动直接可点：运营问「这家店开通的是哪个活动」的时候，答案要一步到位。
      title: '默认活动',
      dataIndex: 'defaultCampaignName',
      search: false,
      ellipsis: true,
      width: 160,
      render: (_, row) =>
        row.defaultCampaignId ? (
          <Typography.Link onClick={() => history.push(`/lottery/campaigns/${row.defaultCampaignId}`)}>
            {row.defaultCampaignName || row.defaultCampaignId}
          </Typography.Link>
        ) : (
          '—'
        ),
    },
    {
      title: '活动数',
      dataIndex: 'campaignCount',
      search: false,
      width: 80,
    },
    {
      // 这一格是这一页存在的理由：运营最想知道的是「这家店现在第几期、还差几个人」。
      // 摆在这里就不必「点进活动再点进期次」两步。
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
          // 空档期（上一期开完了、下一期还没开）是正常状态，不是异常。显示成「—」而不是
          // 「未开通」——两者完全不同，见 ACTIVATION_STATUS 的说明。
          '—'
        ),
    },
    {
      title: '开通人',
      dataIndex: 'activatedBy',
      search: false,
      ellipsis: true,
      width: 140,
      render: (_, row) => dash(row.activatedBy),
    },
    {
      title: '开通时间',
      dataIndex: 'activatedAt',
      valueType: 'dateTime',
      search: false,
      width: 170,
    },
    {
      title: '停用时间',
      dataIndex: 'deactivatedAt',
      valueType: 'dateTime',
      search: false,
      width: 170,
      render: (_, row) => (row.deactivatedAt ? row.deactivatedAt : '—'),
    },
    {
      title: '操作',
      valueType: 'option',
      // fixed 的列必须显式给宽度，且下面的 scroll.x 必须等于各列宽度之和（见下面的 1330）。
      // 130 是量出来的：「停用 / 恢复开通」是一个两到四个字的 link 按钮，14px 下最长
      // 「恢复开通」约 56px，加左右各 8px 内边距远够。
      width: 130,
      fixed: 'right',
      render: (_, row) =>
        access.canManageLottery
          ? [
              row.status === 'enabled' ? (
                <Popconfirm
                  key="disable"
                  title="停用这家门店的抽奖？"
                  description={
                    // 已开奖的期次不受影响这件事必须写进确认里：运营最怕的是「停用会不会
                    // 把已经发出去的奖弄没」。不会。
                    '停用后不再开新期，已经在跑的这一期会走完并正常开奖。已开出的中奖记录不受影响。'
                  }
                  okText="停用"
                  cancelText="取消"
                  onConfirm={() => switchStatus(row, 'disabled')}
                >
                  <Button type="link" size="small" danger loading={switching === row.id}>
                    停用
                  </Button>
                </Popconfirm>
              ) : (
                <Button
                  key="enable"
                  type="link"
                  size="small"
                  loading={switching === row.id}
                  onClick={() => void switchStatus(row, 'enabled')}
                >
                  恢复开通
                </Button>
              ),
            ]
          : [],
    },
  ];

  return (
    <PageContainer
      title="开通门店"
      content="开通抽奖是按门店的。开通时会同时建出一个默认活动和第一期，随后这家店就在收参与了。"
      extra={
        access.canManageLottery
          ? [
              <Button key="activate" type="primary" onClick={() => setActivating(true)}>
                开通门店
              </Button>,
            ]
          : undefined
      }
    >
      <ProTable<Activation>
        actionRef={actionRef}
        rowKey="id"
        columns={columns}
        // 1330 = 180+90+160+80+170+140+170+170+130，各列 width 之和。钉右列必须有它，
        // 而且每一列的宽度都要装得下自己的内容（见上面各列的注释）。
        scroll={{ x: 1330 }}
        search={{ labelWidth: 'auto' }}
        request={async (params) => {
          const query: ActivationQuery = {
            page: params.current,
            pageSize: params.pageSize,
            locationId: exact(params.locationId),
            status: exact(params.status) as ActivationQuery['status'],
            name: exact(params.name),
          };
          const result = await listActivations(query);
          return { data: result.items, total: result.total, success: true };
        }}
      />

      {/*
        开通表单。**必须有 destroyOnClose**：没有它，开着弹窗切到另一家店再开一次，上一轮
        填的门槛 / 时间会留在表单里（antd 的 Form 实例是复用的），而它们看起来像默认值。
        这个坑在这个仓库里踩过（见 ModalForm 那一批修复）。
      */}
      <ModalForm<ActivateForm>
        title="开通门店抽奖"
        open={activating}
        onOpenChange={setActivating}
        modalProps={{ destroyOnClose: true }}
        onFinish={async (values) => {
          const store = stores.find((item) => item.id === values.locationId);
          const window = (values.window ?? []) as unknown[];
          const payload: ActivateInput = {
            locationId: values.locationId,
            // 门店名由这里带上去（选择器本来就有）。**不写空串**：服务端不回头问商户服务，
            // 空名字就是真的一直空着。
            locationName: store?.name ?? '',
            campaignName: values.campaignName?.trim() || undefined,
            participantTarget: values.participantTarget,
            startAt: toRFC3339(window[0]),
            endAt: toRFC3339(window[1]),
            remark: values.remark?.trim() || undefined,
          };
          try {
            await activateLocation(payload);
          } catch (error) {
            message.error(requestErrorMessage(error, '开通失败，请稍后重试'));
            // 留在原地：填过的东西不该让人重填一遍。重复开通同一家门店会回 409，
            // 那正是需要人看到并换一家店的情况。
            return false;
          }
          message.success('已开通，默认活动与第一期已建出');
          actionRef.current?.reload();
          return true;
        }}
      >
        {/* 强调用 Typography.Text 而不是 **：这段是给用户看的纯文本，markdown 星号
            会原样显示出来（截图里就是这样）。** 只在注释里用。 */}
        <Typography.Paragraph type="secondary">
          开通后这家店立刻开始收参与。下面的活动名与门槛会建在
          <Typography.Text strong>默认活动</Typography.Text>和
          <Typography.Text strong>第一期</Typography.Text>上，
          留空则用内置模板（活动名取「门店抽奖」、门槛 30 人、窗口从现在起 90 天，
          奖品是 1 份「神秘礼品」）。这几个默认值在服务端（service.Activate），
          改那里就要改这里——写成别的数字比不写还糟。
        </Typography.Paragraph>
        <ProFormSelect
          name="locationId"
          label="门店"
          options={storeOptions}
          showSearch
          rules={[{ required: true, message: '请选择门店' }]}
          fieldProps={{
            filterOption: (input: string, option) =>
              String(option?.label ?? '')
                .toLowerCase()
                .includes(input.toLowerCase()),
            placeholder: stores.length ? '选择要开通抽奖的门店' : '门店列表读不到，请确认有门店查看权限',
          }}
        />
        <ProFormText
          name="campaignName"
          label="默认活动名"
          placeholder="留空用「门店抽奖」"
          fieldProps={{ maxLength: 50 }}
        />
        <ProFormDigit
          name="participantTarget"
          label="参与门槛"
          tooltip="第一期收满这么多人就停止收人并开奖。之后的每一期默认也用它，但每期可以不同。"
          min={1}
          max={100000}
          fieldProps={{ precision: 0 }}
          placeholder="留空用 30"
        />
        <ProFormDateTimePicker
          name="window"
          label="活动窗口"
          tooltip="整场活动的起止时间，期次在这个区间里滚动。留空为现在起 90 天。"
          fieldProps={{ style: { width: '100%' } }}
        />
        <ProFormTextArea
          name="remark"
          label="备注"
          placeholder="给下一个人看的说明，可留空"
          fieldProps={{ rows: 2, maxLength: 200 }}
        />
      </ModalForm>
    </PageContainer>
  );
}
