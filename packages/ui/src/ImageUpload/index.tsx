// 图片上传控件：只管 UI 状态（上传中 / 失败 / 回显 / 删除），传输由宿主注入。
//
// 不 import `@umijs/max` 的 request：那是应用级的东西，共享组件看不见宿主的 token 与
// 网关前缀。宿主把 `upload(file) => Promise<string>` 传进来，组件拿返回的绝对 URL
// 当值用，因此单图字段（logo）和多图字段（photos）喂同一个组件都成立。
import { PlusOutlined } from '@ant-design/icons';
import { ProFormField } from '@ant-design/pro-components';
import { Upload, message } from 'antd';
import type { FormItemProps, UploadFile, UploadProps } from 'antd';
import { useCallback, useEffect, useMemo, useRef, useState } from 'react';
import type { ComponentProps, ReactNode } from 'react';

/** 组件的值：单图是 URL 字符串，多图是 URL 数组；没选、清空都是 undefined。 */
export type ImageUploadValue = string | string[] | undefined;

export type ImageUploadProps = {
  value?: ImageUploadValue;
  onChange?: (value: ImageUploadValue) => void;
  /**
   * 真正的传输由宿主注入。成功返回图片的绝对 URL，失败请抛 `Error`：
   * 消息会原样提示给用户（后端的中文文案经 app.ts 解包后就在 message 里）。
   */
  upload: (file: File) => Promise<string>;
  /** 1 = 单图（值是字符串），大于 1 = 多图（值是数组）。 */
  maxCount?: number;
  /** 单文件字节上限，与后端 `UPLOAD_MAX_FILE_SIZE` 对齐。这里只是省一次白跑的服务端往返。 */
  maxSize?: number;
  accept?: string;
  disabled?: boolean;
  /** 触发按钮上的文案。 */
  text?: string;
};

const DEFAULT_MAX_SIZE = 10 * 1024 * 1024;

export function ImageUpload({
  value,
  onChange,
  upload,
  maxCount = 1,
  maxSize = DEFAULT_MAX_SIZE,
  accept = 'image/*',
  disabled,
  text = '上传图片',
}: ImageUploadProps) {
  const single = maxCount === 1;
  const done = useMemo(() => normalize(value), [value]);

  // 只存「正在传」的那几个：已完成项的真相在表单的 value 里，组件再存一份就是两份
  // 真相，`initialValues` 一变就对不上。
  const [pending, setPending] = useState<UploadFile[]>([]);

  // 但提交时不能读闭包里的 done：一次选 3 张会有 3 个上传同时在飞，后回来的那个拿着
  // 同一份旧 done，会把前一张挤掉。所以维护一个写穿的镜像，提交前先更新它；等表单的
  // value 真的变了，再以表单为准（value 是权威，镜像只是让连续完成时有人接手）。
  const latest = useRef<string[]>(done);
  useEffect(() => {
    latest.current = done;
  }, [done]);

  const commit = useCallback(
    (urls: string[]) => {
      latest.current = urls;
      // 单图给字符串、多图给数组：两个字段类型都不用再包一层转换。
      onChange?.(single ? urls[0] : urls);
    },
    [onChange, single],
  );

  const startUpload = useCallback<NonNullable<UploadProps['customRequest']>>(
    (options) => {
      const file = options.file as File & { uid?: string };
      const uid = file.uid ?? `image-${Date.now()}-${Math.random()}`;
      setPending((list) => [
        ...list,
        {
          uid,
          name: file.name,
          status: 'uploading',
          originFileObj: file as UploadFile['originFileObj'],
        },
      ]);
      upload(file).then(
        (url) => {
          setPending((list) => list.filter((item) => item.uid !== uid));
          commit([...latest.current, url]);
          options.onSuccess?.(url);
        },
        (error: unknown) => {
          setPending((list) => list.filter((item) => item.uid !== uid));
          message.error(errorText(error));
          options.onError?.(error instanceof Error ? error : new Error(String(error)));
        },
      );
    },
    [commit, upload],
  );

  const beforeUpload = useCallback<NonNullable<UploadProps['beforeUpload']>>(
    (file) => {
      // 只是省一次白跑的服务端往返：真正的判据在服务端（按字节嗅探，不信 Content-Type）。
      if (file.size > maxSize) {
        message.error(`图片不能超过 ${formatSize(maxSize)}`);
        return Upload.LIST_IGNORE;
      }
      if (!looksLikeImage(file)) {
        message.error('仅支持 JPG / PNG / GIF / WebP 等图片格式');
        return Upload.LIST_IGNORE;
      }
      return true;
    },
    [maxSize],
  );

  const fileList = useMemo<UploadFile[]>(
    () => [
      ...done.map((url) => ({
        uid: `url:${url}`,
        name: fileNameFromURL(url),
        status: 'done' as const,
        url,
      })),
      ...pending,
    ],
    [done, pending],
  );

  return (
    <Upload
      accept={accept}
      beforeUpload={beforeUpload}
      customRequest={startUpload}
      disabled={disabled}
      fileList={fileList}
      listType="picture-card"
      maxCount={maxCount}
      // 上传中的项没有 url，天然不进这里；能删的只有已完成项。
      onRemove={(file) => {
        commit(latest.current.filter((url) => url !== file.url));
        return true;
      }}
    >
      {disabled ? null : (
        <div>
          <PlusOutlined />
          <div style={{ marginTop: 8 }}>{text}</div>
        </div>
      )}
    </Upload>
  );
}

export type ProFormImageUploadProps = Omit<ImageUploadProps, 'value' | 'onChange'> &
  Omit<ComponentProps<typeof ProFormField>, 'children' | 'value' | 'onChange' | 'name'> & {
    // 表单字段没绑 name 就是个「填了也不提交」的坑，pro 自己的字段类型是可选的，
    // 这里收紧成必填。真要一个不绑表单的上传控件，直接用 ImageUpload。
    name: NonNullable<FormItemProps['name']>;
  };

/**
 * 表单里的一行。`label` / `tooltip` / `colProps` / `rules` 这些是字段自己的属性，
 * 由 ProFormField 接住，不会变成控件的 props。
 *
 * 用 ProFormField 而不是包一层 ProFormItem：只有前者是 createField 造出来的，
 * 会带上 Col 包装（`colProps` 与 `grid` 表单的栅格），而 ProFormItem 只是一个裸的
 * Form.Item —— 在 `grid` 表单里它会脱离栅格（门店页那个表单就是 grid）。
 */
export function ProFormImageUpload({ upload, ...rest }: ProFormImageUploadProps) {
  const { maxCount, maxSize, accept, disabled, text, ...itemProps } = rest;
  return (
    <ProFormField {...itemProps}>
      <ImageUpload
        accept={accept}
        disabled={disabled}
        maxCount={maxCount}
        maxSize={maxSize}
        text={text}
        upload={upload}
      />
    </ProFormField>
  );
}

/** 表单值归一成 URL 列表：单图字符串、多图数组、空值与空串都收敛到这里。 */
function normalize(value: ImageUploadValue): string[] {
  if (Array.isArray(value)) {
    return value.filter((url): url is string => typeof url === 'string' && url !== '');
  }
  return typeof value === 'string' && value !== '' ? [value] : [];
}

function fileNameFromURL(url: string): string {
  const last = url.split('?')[0].split('/').pop() ?? '';
  try {
    return decodeURIComponent(last) || url;
  } catch {
    return last || url;
  }
}

function looksLikeImage(file: File): boolean {
  if (file.type) {
    // svg 也是 image/*，但它是可执行的 XML，服务端会拒，这里先拦一道免得白传。
    return file.type.startsWith('image/') && file.type !== 'image/svg+xml';
  }
  return /\.(jpe?g|png|gif|webp|bmp|avif|tiff?)$/i.test(file.name);
}

function formatSize(bytes: number): string {
  if (bytes >= 1024 * 1024) return `${Math.round(bytes / (1024 * 1024))}MB`;
  if (bytes >= 1024) return `${Math.round(bytes / 1024)}KB`;
  return `${bytes}B`;
}

function errorText(error: unknown): ReactNode {
  if (error instanceof Error && error.message) return error.message;
  return '图片上传失败，请重试';
}
