import { describe, expect, it } from 'vitest';
import { deletionErrorMessage, requestErrorMessage, roleSaveErrorMessage } from './requestError';

describe('requestErrorMessage', () => {
  it('prefers the backend message', () => {
    expect(requestErrorMessage({ response: { data: { errorMessage: '权限刷新失败' } } }, '兜底'))
      .toBe('权限刷新失败');
  });

  it.each([undefined, null, new Error('Network Error'), { response: { data: {} } }])(
    'falls back for %j', (error) => {
      expect(requestErrorMessage(error, '加载权限失败，请稍后重试')).toBe('加载权限失败，请稍后重试');
    },
  );
});

describe('role save errors', () => {
  it.each([400, 404, 409, 500])('shows the backend message for HTTP %s', (status) => {
    expect(roleSaveErrorMessage({ response: { status, data: { errorMessage: '后端明确错误' } } }))
      .toBe('后端明确错误');
  });

  it('explains conflicts even without a backend message', () => {
    expect(roleSaveErrorMessage({ response: { status: 409 } }))
      .toBe('角色代码已存在或与已有授权冲突，请更换角色代码');
  });

  it.each([undefined, null, new Error('Network Error'), { response: { status: 500 } }])(
    'provides a safe fallback for %j', (error) => {
      expect(roleSaveErrorMessage(error)).toBe('保存失败，请稍后重试');
    },
  );
});

describe('existing deletion errors', () => {
  it('preserves unsupported deletion wording', () => {
    expect(deletionErrorMessage({ response: { status: 503, data: { errorMessage: 'unavailable' } } }))
      .toBe('当前暂不支持删除，请稍后重试');
  });
  it('preserves backend and fallback messages', () => {
    expect(deletionErrorMessage({ response: { data: { errorMessage: '存在关联门店' } } })).toBe('存在关联门店');
    expect(deletionErrorMessage(null)).toBe('删除失败，请稍后重试');
  });
});
