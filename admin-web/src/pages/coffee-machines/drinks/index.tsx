import {
  PauseCircleOutlined,
  PlayCircleOutlined,
  PlusOutlined,
} from '@ant-design/icons';
import {
  ModalForm,
  PageContainer,
  ProFormDigit,
  ProFormSelect,
  ProFormText,
  ProFormTextArea,
  ProTable,
} from '@ant-design/pro-components';
import { ProFormImageUpload } from '@panda-v2/ui';
import { useAccess } from '@umijs/max';
import { Button, message, Popconfirm, Space, Tag } from 'antd';
import { useEffect, useRef, useState } from 'react';
import type { ActionType, ProColumns } from '@ant-design/pro-components';
import {
  createDrink,
  listDrinks,
  listManufacturers,
  updateDrink,
  updateDrinkStatus,
  type Drink,
  type DrinkStatus,
  type DrinkType,
  type Manufacturer,
} from '../../../services/coffeeMachine';
import { toPageParams } from '../../../services/pagination';
import { requestErrorMessage } from '../../../services/requestError';
import { uploadImage } from '../../../services/upload';

const STATUS_TAG: Record<DrinkStatus, { color: string; label: string }> = {
  on_shelf: { color: 'green', label: '上架' },
  off_shelf: { color: 'default', label: '下架' },
};

const TYPE_TEXT: Record<DrinkType, string> = {
  milk_coffee: '奶咖',
  black_coffee: '黑咖',
  other: '其他',
};

// 接口和库里金额一律是「分」的整数；页面按「元」录入和展示。换算只发生在这里，
// 别在别处再写一次 /100 —— 单位错位（12.50 存成 12）不会报错，只会静默算错钱。
//
// 用 Math.round 而不是直接截断：1.15 * 100 在 IEEE754 下是 114.99999999999999，
// 截断会悄悄少收一分钱。输入框另外用 precision={2} 限死两位小数。
const yuanToFen = (yuan?: number) => Math.round(Number(yuan ?? 0) * 100);
const fenToYuan = (fen?: number) => Number(fen ?? 0) / 100;
const formatYuan = (fen?: number) => fenToYuan(fen).toFixed(2);

type DrinkFormValues = {
  manufacturerId?: string;
  originId?: string;
  productNum?: string;
  productName: string;
  enName?: string;
  drinkType?: DrinkType;
  productImg?: string;
  productDesc?: string;
  price?: number;
  vipPrice?: number;
  pickupCodePrice?: number;
  sort?: number;
};

const DrinksPage: React.FC = () => {
  const access = useAccess();
  const actionRef = useRef<ActionType>();
  const [formOpen, setFormOpen] = useState(false);
  const [editing, setEditing] = useState<Drink | null>(null);
  const [manufacturers, setManufacturers] = useState<Manufacturer[]>([]);

  // 列表要把 manufacturerId 显示成名称，搜索下拉也要全集，一次拉完建映射比逐行查接口划算。
  // 缺 coffee_machine:read 之外的权限时这个请求会失败，但厂商名只是给人看的，
  // 不该因此让整页报错，所以吞掉异常退回显示 id。
  useEffect(() => {
    void (async () => {
      try {
        setManufacturers(await listManufacturers());
      } catch {
        setManufacturers([]);
      }
    })();
  }, []);

  const manufacturerNames = Object.fromEntries(manufacturers.map((m) => [m.id, m.name]));

  const manufacturerOptions = async () => {
    const items = await listManufacturers();
    return items.map((m) => ({ label: m.name, value: m.id }));
  };

  const columns: ProColumns<Drink>[] = [
    {
      title: '饮品名称',
      dataIndex: 'productName',
      width: 180,
      ellipsis: true,
      fieldProps: { placeholder: '名称' },
    },
    {
      title: '厂商',
      dataIndex: 'manufacturerId',
      valueType: 'select',
      width: 160,
      ellipsis: true,
      // valueEnum 只喂搜索下拉；表格里走下面的 render，好把已停用/已删除厂商的 id 原样显示。
      valueEnum: Object.fromEntries(manufacturers.map((m) => [m.id, { text: m.name }])),
      render: (_, row) => manufacturerNames[row.manufacturerId] ?? row.manufacturerId,
    },
    {
      title: '商品编号',
      dataIndex: 'productNum',
      width: 120,
      search: false,
      render: (_, row) => row.productNum || '—',
    },
    {
      title: '类型',
      dataIndex: 'drinkType',
      width: 90,
      search: false,
      render: (_, row) => (row.drinkType ? TYPE_TEXT[row.drinkType] ?? row.drinkType : '不限'),
    },
    // 接口给的是「分」，直接铺出来就是「1800」；列表按元展示成「18.00」。
    {
      title: '原价（元）',
      dataIndex: 'price',
      width: 100,
      search: false,
      render: (_, row) => formatYuan(row.price),
    },
    {
      title: '会员价（元）',
      dataIndex: 'vipPrice',
      width: 110,
      search: false,
      render: (_, row) => (row.vipPrice ? formatYuan(row.vipPrice) : '—'),
    },
    {
      title: '提货码价（元）',
      dataIndex: 'pickupCodePrice',
      width: 120,
      search: false,
      render: (_, row) => (row.pickupCodePrice ? formatYuan(row.pickupCodePrice) : '—'),
    },
    { title: '排序', dataIndex: 'sort', width: 70, search: false },
    {
      title: '状态',
      dataIndex: 'status',
      valueType: 'select',
      width: 80,
      valueEnum: { on_shelf: { text: '上架' }, off_shelf: { text: '下架' } },
      render: (_, row) => {
        const tag = STATUS_TAG[row.status] ?? { color: 'default', label: row.status };
        return <Tag color={tag.color}>{tag.label}</Tag>;
      },
    },
    {
      title: '操作',
      valueType: 'option',
      width: 150,
      fixed: 'right',
      render: (_, row) => (
        <Space>
          {access.canWriteCoffeeMachines && (
            <Button
              type="link"
              size="small"
              onClick={() => {
                setEditing(row);
                setFormOpen(true);
              }}
            >
              编辑
            </Button>
          )}
          {access.canWriteCoffeeMachines && row.status === 'on_shelf' && (
            <Popconfirm
              title="下架后该饮品在设备上不可售，确认下架？"
              onConfirm={async () => {
                try {
                  await updateDrinkStatus(row.id, 'off_shelf');
                  message.success('已下架');
                  actionRef.current?.reload();
                } catch (error) {
                  message.error(requestErrorMessage(error, '下架失败，请稍后重试'));
                }
              }}
            >
              <Button type="link" size="small" danger icon={<PauseCircleOutlined />}>
                下架
              </Button>
            </Popconfirm>
          )}
          {access.canWriteCoffeeMachines && row.status === 'off_shelf' && (
            <Popconfirm
              title="确认上架该饮品？"
              onConfirm={async () => {
                try {
                  await updateDrinkStatus(row.id, 'on_shelf');
                  message.success('已上架');
                  actionRef.current?.reload();
                } catch (error) {
                  message.error(requestErrorMessage(error, '上架失败，请稍后重试'));
                }
              }}
            >
              <Button type="link" size="small" icon={<PlayCircleOutlined />}>
                上架
              </Button>
            </Popconfirm>
          )}
        </Space>
      ),
    },
  ];

  return (
    <PageContainer title="饮品管理">
      <ProTable<Drink>
        actionRef={actionRef}
        rowKey="id"
        columns={columns}
        scroll={{ x: 1300 }}
        search={{ labelWidth: 'auto' }}
        request={async (params) => {
          const result = await listDrinks(toPageParams(params));
          return { data: result.items, total: result.total, success: true };
        }}
        toolBarRender={() => [
          access.canWriteCoffeeMachines && (
            <Button
              key="add"
              type="primary"
              icon={<PlusOutlined />}
              onClick={() => {
                setEditing(null);
                setFormOpen(true);
              }}
            >
              新建饮品
            </Button>
          ),
        ]}
      />

      <ModalForm<DrinkFormValues>
        key={editing?.id ?? 'create'}
        title={editing ? `编辑饮品「${editing.productName}」` : '新建饮品'}
        open={formOpen}
        onOpenChange={setFormOpen}
        modalProps={{ destroyOnClose: true }}
        initialValues={
          editing
            ? {
                manufacturerId: editing.manufacturerId,
                originId: editing.originId,
                productNum: editing.productNum,
                productName: editing.productName,
                enName: editing.enName,
                drinkType: editing.drinkType ?? undefined,
                productImg: editing.productImg,
                productDesc: editing.productDesc,
                price: fenToYuan(editing.price),
                vipPrice: fenToYuan(editing.vipPrice),
                pickupCodePrice: fenToYuan(editing.pickupCodePrice),
                sort: editing.sort,
              }
            : { sort: 0, price: 0 }
        }
        onFinish={async (values) => {
          const prices = {
            price: yuanToFen(values.price),
            vipPrice: yuanToFen(values.vipPrice),
            pickupCodePrice: yuanToFen(values.pickupCodePrice),
          };
          try {
            if (editing) {
              // 厂商与 originId 不在编辑入参里：那是同步的自然键，改了匹配不上。
              await updateDrink(editing.id, {
                productNum: values.productNum,
                productName: values.productName,
                enName: values.enName,
                drinkType: values.drinkType ?? null,
                productImg: values.productImg,
                productDesc: values.productDesc,
                sort: values.sort,
                ...prices,
              });
              message.success('已保存');
            } else {
              await createDrink({
                manufacturerId: values.manufacturerId as string,
                originId: values.originId,
                productNum: values.productNum,
                productName: values.productName,
                enName: values.enName,
                drinkType: values.drinkType ?? null,
                productImg: values.productImg,
                productDesc: values.productDesc,
                sort: values.sort,
                ...prices,
              });
              message.success('已创建');
            }
          } catch (error) {
            // 「原价为 0 时会员价与提货码价也必须为 0」「厂商与商品编号已存在」这类
            // 都是 409/400 加一句中文说明，比通用的「保存失败」有用得多。
            message.error(requestErrorMessage(error, '保存失败，请稍后重试'));
            return false;
          }
          actionRef.current?.reload();
          return true;
        }}
      >
        <ProFormSelect
          name="manufacturerId"
          label="所属厂商"
          request={manufacturerOptions}
          disabled={!!editing}
          tooltip={editing ? '厂商创建后不可修改' : undefined}
          rules={[{ required: true, message: '请选择所属厂商' }]}
        />
        <ProFormText
          name="originId"
          label="厂商商品 ID"
          disabled={!!editing}
          tooltip={
            editing
              ? '厂商同步的自然键，创建后不可修改'
              : '厂商饮品库里的商品 ID，同步时靠它匹配这款饮品'
          }
        />
        <ProFormText name="productNum" label="商品编号" />
        <ProFormText
          name="productName"
          label="饮品名称"
          rules={[{ required: true, message: '请输入饮品名称' }]}
        />
        <ProFormText name="enName" label="英文名" />
        <ProFormSelect
          name="drinkType"
          label="饮品类型"
          allowClear
          placeholder="不选表示不限"
          options={[
            { label: '奶咖', value: 'milk_coffee' },
            { label: '黑咖', value: 'black_coffee' },
            { label: '其他', value: 'other' },
          ]}
        />
        <ProFormImageUpload
          name="productImg"
          label="饮品图片"
          upload={uploadImage}
          colProps={{ span: 12 }}
        />
        <ProFormTextArea name="productDesc" label="饮品描述" />
        {/* 单位是元：填 18.5，提交时 ×100 成分。precision 限死两位小数，
            否则 18.505 这种值会被 Math.round 悄悄修成 18.51，用户看不出来。 */}
        <ProFormDigit
          name="price"
          label="原价（元）"
          min={0}
          fieldProps={{ precision: 2 }}
          tooltip="原价为 0 时，会员价与提货码价也必须为 0"
        />
        <ProFormDigit name="vipPrice" label="会员价（元）" min={0} fieldProps={{ precision: 2 }} />
        <ProFormDigit
          name="pickupCodePrice"
          label="提货码价（元）"
          min={0}
          fieldProps={{ precision: 2 }}
        />
        <ProFormDigit name="sort" label="排序" min={0} fieldProps={{ precision: 0 }} />
      </ModalForm>
    </PageContainer>
  );
};

export default DrinksPage;
