import {
  ModalForm,
  ProFormDigit,
  ProFormSelect,
  ProFormText,
  ProFormTextArea,
} from '@ant-design/pro-components';
import { ProFormImageUpload } from '@panda-v2/ui';
import { message } from 'antd';
import {
  createDrink,
  listDeviceOptions,
  updateDrink,
  type DeviceSummary,
  type Drink,
  type DrinkType,
} from '../../../services/coffeeMachine';
import { fenToYuan, yuanToFen } from '../../../services/money';
import { requestErrorMessage } from '../../../services/requestError';
import { uploadImage } from '../../../services/upload';
import { scrollableModalBody } from '../../../components/common/modalProps';

/**
 * 饮品的新建 / 编辑表单。两个入口共用：饮品管理页的新建按钮，和设备详情页饮品 tab 的
 * 「添加饮品」——后者把 deviceId 预填成当前那台设备，这样一屏之内加的饮品就直接挂在这台
 * 机器上，不需要再让人在下拉里找一遍自己刚进来的那台。
 *
 * **表单里没有厂商**：厂商是设备的一列（设备表里必填），饮品挂到哪台设备就归属哪个厂商，
 * 由服务层按 deviceId 现取。让人在这里再选一次，就多出一份能和设备对不上的说法。
 *
 * 表单收在独立文件而不是留在页里，是为了不让同一份字段定义出现两遍：设备列是必填项，
 * 两处各写一遍的话，先改的那处会把另一处落在一个「没传 deviceId」的旧形状上。
 */

type DrinkFormValues = {
  deviceId?: string;
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

export type DrinkFormModalProps = {
  open: boolean;
  onOpenChange: (open: boolean) => void;
  /** null = 新建；有值 = 编辑那一条。 */
  editing: Drink | null;
  /** 新建时预填的设备（设备详情页那个入口传当前设备）。 */
  presetDeviceId?: string;
  onSaved: () => void;
};

/**
 * 设备下拉的选项。
 *
 * 取全集（上限见 listDeviceOptions），并在编辑时把这一行当前的设备补进去：只取了前
 * 200 台，正在编辑的这台有可能不在里面，那时下拉会退化成显示一串 uuid。
 */
async function deviceOptions(current?: string | null) {
  let devices: DeviceSummary[] = [];
  try {
    devices = await listDeviceOptions();
  } catch {
    // 权限或网络问题不该把整个表单卡死：新建时用户还能自己往下拉里找（找不到就该重试），
    // 但静默吞掉会让「设备列表是空的」看起来像「一台设备都没有」。
    message.warning('设备列表加载失败，请稍后重试');
  }
  const options = devices.map((device) => ({
    label: `${device.deviceName || '未命名设备'}（${device.serialUnique}）`,
    value: device.id,
  }));
  if (current && !options.some((option) => option.value === current)) {
    options.push({ label: `当前设备（${current}）`, value: current });
  }
  return options;
}

export default function DrinkFormModal({
  open,
  onOpenChange,
  editing,
  presetDeviceId,
  onSaved,
}: DrinkFormModalProps) {
  return (
    <ModalForm<DrinkFormValues>
      // key 让每次打开都重新挂载：ModalForm 的 initialValues 只在挂载时读一次，
      // 复用实例会把上一条记录的值带进下一条（设备也会跟着串）。
      key={editing?.id ?? `create-${presetDeviceId ?? ''}`}
      title={editing ? `编辑饮品「${editing.productName}」` : '新建饮品'}
      open={open}
      onOpenChange={onOpenChange}
      modalProps={{ ...scrollableModalBody, destroyOnClose: true }}
      initialValues={
        editing
          ? {
              deviceId: editing.deviceId ?? undefined,
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
          : { deviceId: presetDeviceId, sort: 0, price: 0 }
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
            // deviceId 在——换设备就是改这一行挂在哪台机器上。
            await updateDrink(editing.id, {
              deviceId: values.deviceId ?? null,
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
              deviceId: values.deviceId ?? null,
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
          // 「原价为 0 时会员价与提货码价也必须为 0」「这台设备上已经有同款」这类
          // 都是 409/400 加一句中文说明，比通用的「保存失败」有用得多。
          message.error(requestErrorMessage(error, '保存失败，请稍后重试'));
          return false;
        }
        onSaved();
        return true;
      }}
    >
      <ProFormSelect
        name="deviceId"
        label="所属设备"
        showSearch
        request={() => deviceOptions(editing?.deviceId)}
        // 不选就提交不了：饮品行就是「某台设备上的一杯」，没有设备的行卖不出去。
        rules={[{ required: true, message: '请选择所属设备' }]}
        tooltip={
          editing?.deviceId
            ? '改这里就是把这杯饮品挪到另一台设备上'
            : '这一行饮品挂在哪台机器上。历史遗留的「未分配设备」行编辑时必须补一台'
        }
        fieldProps={{
          // 默认的过滤比的是 option 的 value（一串 uuid），按设备名搜是搜不到的。
          filterOption: (input, option) =>
            String(option?.label ?? '')
              .toLowerCase()
              .includes(input.toLowerCase()),
        }}
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
  );
}
