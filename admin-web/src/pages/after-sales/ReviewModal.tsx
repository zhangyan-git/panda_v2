import {
  ModalForm,
  ProFormCheckbox,
  ProFormTextArea,
} from '@ant-design/pro-components';
import { message, Typography } from 'antd';
import { requestErrorCode, requestErrorMessage } from '../../services/requestError';
import { reviewAfterSale, type AfterSale } from '../../services/order';
import { scrollableModalBody } from '../../components/common/modalProps';

/**
 * 审核一张退款申请：通过或驳回。两个入口共用——退款申请列表的行操作，以及订单详情里
 * 「售后记录」那个 tab。共用的不只是这个弹窗，还有那条福卡规则：两个入口都能点「通过」，
 * 那么两个入口都必须先让人勾上那句话。
 *
 * 动作（approve/reject）由调用方给，不在这里选：按钮文案已经说明要做什么了，再让人在弹窗
 * 里选一次，选错了就是审错。后端也把动作写在路径上，两边一致。
 */

const { Paragraph, Text } = Typography;

/** 通过时额外要的确认。字段名与后端 dto.ReviewAfterSaleRequest 一致。 */
type ReviewFormValues = {
  remark?: string;
  fortuneCardUnusedConfirmed?: boolean;
};

export type ReviewModalProps = {
  open: boolean;
  onOpenChange: (open: boolean) => void;
  /** 要审的那一张。为 undefined 时弹窗不打开（关掉之后调用方会清掉）。 */
  afterSale?: AfterSale;
  action: 'approve' | 'reject';
  /** 审完通知调用方刷新列表。 */
  onReviewed: () => void;
};

export default function ReviewModal({
  open,
  onOpenChange,
  afterSale,
  action,
  onReviewed,
}: ReviewModalProps) {
  const isApprove = action === 'approve';
  // 福卡规则：订单承诺送过福卡时，通过之前必须由人确认那些卡没抽过奖。判据是后端给的
  // fortuneCardsExpected（这一单承诺发几张），不是本地猜的。
  const needsFortuneCardCheck = isApprove && (afterSale?.fortuneCardsExpected ?? 0) > 0;

  return (
    <ModalForm<ReviewFormValues>
      // key 跟单据走 + destroyOnClose：换一张单再审时，上一条填的备注和勾过的确认不会
      // 跟过来——那是最坏的一种错，第二次审核会带着第一次的结论直接过。
      key={afterSale?.id ?? 'none'}
      open={open}
      onOpenChange={onOpenChange}
      modalProps={{ ...scrollableModalBody, destroyOnClose: true }}
      title={
        afterSale
          ? `${isApprove ? '通过' : '驳回'}退款申请 ${afterSale.afterSaleNo}`
          : '审核退款申请'
      }
      onFinish={async (values) => {
        if (!afterSale) return true;
        try {
          await reviewAfterSale(afterSale.afterSaleNo, action, {
            remark: values.remark?.trim(),
            // 驳回时后端不看这个字段（驳回本来就在说「不退」），不发。
            ...(needsFortuneCardCheck
              ? { fortuneCardUnusedConfirmed: values.fortuneCardUnusedConfirmed === true }
              : {}),
          });
        } catch (error) {
          if (requestErrorCode(error) === 'FORTUNE_CARD_CONFIRMATION_REQUIRED') {
            // 后端也判了一次（人工勾选是它唯一能拿到的事实）。走到这里说明确认没勾上，
            // 把弹窗留在原地并说清该怎么办，而不是把它关掉让人重新点一遍。
            message.error('这一单赠送过福卡，需先确认福卡未参与抽奖');
            return false;
          }
          if (requestErrorCode(error) === 'REFUND_NOT_STARTED') {
            // 这一条**不是「审核失败」**：审核已经落库了（单已是「已通过」），只是紧接着那次
            // 发起退款没成（支付服务抖了、或者没接上）。弹窗照常关掉、列表照常刷新——留在原地
            // 会让人以为没审成，再点一次「通过」，而那张单已经不在待审核状态了，只会拿到
            // 一个 409 死胡同。该怎么继续写进提示里：列表里那一行有「发起退款」。
            message.warning(
              `${requestErrorMessage(error, '已通过申请，但退款未发起')}；可在列表里对该单点「发起退款」重试`,
            );
            onReviewed();
            return true;
          }
          message.error(requestErrorMessage(error, isApprove ? '通过失败，请稍后重试' : '驳回失败，请稍后重试'));
          return false;
        }
        // 通过即发起：审核与退款是同一件事的两步（钱由支付服务退，结果回写到这张单上）。
        // 说清「发起」而不是「已退」——退款成没成要看回来的那个结果。
        message.success(isApprove ? '已通过申请，并已向支付服务发起退款' : '已驳回');
        onReviewed();
        return true;
      }}
    >
      {isApprove ? (
        <>
          <Paragraph type="secondary">
            通过之后这一笔<Text strong>就会去退</Text>：服务端先记下「已通过」，紧接着向支付服务
            发起退款，退款结果会回写到这张单上。若那一次发起没成，单据会停在「已通过」——那是一个
            准确的停留态（同意退、退款还没发起），在列表里对该单点「发起退款」可以再推一次，
            重发不会退两次钱。
          </Paragraph>
          {needsFortuneCardCheck && (
            <>
              <Paragraph type="secondary" style={{ marginBottom: 8 }}>
                这一单承诺赠送 {afterSale?.fortuneCardsExpected} 张福卡。规则：订单赠送的福卡若
                已参与抽奖，整笔不可退。请先查清这些福卡的抽奖情况——已抽奖的请改用「驳回」，并在
                理由里写明。
              </Paragraph>
              <ProFormCheckbox
                name="fortuneCardUnusedConfirmed"
                // 用 validator 而不是 required：单个复选框的值是布尔 false，而校验器眼里的
                // 「空」是 undefined/null/空串——false 不算空，required 会直接放过没勾的情形。
                rules={[
                  {
                    validator: (_, value) =>
                      value === true
                        ? Promise.resolve()
                        : Promise.reject(new Error('请确认本单赠送的福卡未参与抽奖')),
                  },
                ]}
              >
                本单赠送的福卡未参与抽奖
              </ProFormCheckbox>
            </>
          )}
          <ProFormTextArea
            name="remark"
            label="审核备注"
            placeholder="选填，会写进审核记录"
            fieldProps={{ rows: 3 }}
          />
        </>
      ) : (
        <>
          <Paragraph type="secondary">
            驳回是一个结论：这一笔<Text strong>不退</Text>。用户看到的只有你写的这句话，请把
            依据写清楚（例如「福卡已参与抽奖，按规则整笔不可退」）。
          </Paragraph>
          <ProFormTextArea
            name="remark"
            label="驳回理由"
            placeholder="必填，用户与客服都靠这句话理解为什么不退"
            fieldProps={{ rows: 4 }}
            rules={[{ required: true, message: '请输入驳回理由' }]}
          />
        </>
      )}
    </ModalForm>
  );
}
