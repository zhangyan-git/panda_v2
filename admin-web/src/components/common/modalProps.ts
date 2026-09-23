import type { ModalProps } from 'antd';

/**
 * 弹窗内容区限高、自己滚。所有 ModalForm 的 modalProps 都带上它：
 *
 *     modalProps={{ ...scrollableModalBody, destroyOnClose: true }}
 *
 * 不限高时，长表单弹窗会长过窗口，改由 .ant-modal-wrap 滚动，表单最后一格正好贴在窗口
 * 底边。antd 的下拉在下方放不下时会向上翻，而翻上去并不避让任何东西——它直接压住上一格。
 * 表现是「数据范围」被「品牌」压住，只露出顶上十几 px 的裁切文字，看着像这一格坏了。
 *
 * 限高之后弹窗整体留在窗口内，表单最后一格下方还剩 160px 左右，够 72px 的多选下拉往下开。
 * 260 = antd Modal 的 top(100) + 标题栏与底栏(140，实测) + 余量(20)。视口高到用不上这个
 * 上限时它是空操作，所以短弹窗带上也无妨——但别去掉，去掉就回到上面那个现象。
 */
export const scrollableModalBody: Pick<ModalProps, 'styles'> = {
  styles: {
    body: { maxHeight: 'calc(100vh - 260px)', overflowY: 'auto' },
  },
};
