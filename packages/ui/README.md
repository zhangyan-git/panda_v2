# @panda-v2/ui

跨端共享的 React 组件。目前是 `admin-web` 在用，`merchant-web` 是它预留的位置——组件
放这里而不是应用里，是为了在第二个消费者出现时不必回头做「提升」这件事。

## 怎么被消费

`package.json` 的 `main` / `types` 都指向 **`src/index.ts`**，与 `packages/api` 同一形状：
**没有构建步骤**，宿主打包器直接编译这个包的 TS/TSX 源码。

- 因此 `admin-web` 不需要 `extraBabelIncludes`（`mfsu: false` 已设）。这条链路在
  `pnpm dev` 与 `pnpm build` 两边都验过，生产包里能直接找到本包的组件代码与文案。
- 本包 tsconfig 与 `packages/api` 只差两处，都因为这里有 JSX：`module: ESNext` +
  `moduleResolution: Bundler`、`jsx: react-jsx`。相对 import **不写 `.js` 后缀**。
- `react` / `react-dom` / `antd` / `@ant-design/pro-components` / `@ant-design/icons`
  是 **peerDependencies**：组件库由宿主提供，不重复安装（重复安装会造出两个 antd 实例，
  context、主题、`message` 全都对不上）。开发时它们同样列在 `devDependencies` 里。

## 组件

### `ImageUpload` / `ProFormImageUpload`

图片上传控件，只管 UI 状态（上传中 / 失败 / 回显 / 删除），**传输由宿主注入**：
`upload: (file: File) => Promise<string>` 返回图片的绝对 URL。共享组件不能 import
`@umijs/max`（那是应用级的，看不见宿主的 token 与网关前缀），所以鉴权头留在宿主——
`admin-web` 的注入点是 `src/services/upload.ts`。

- `maxCount === 1` 时值是字符串，大于 1 时是数组，因此 `logo`（单图）与 `photos`
  （多图）喂同一个组件。
- `ProFormImageUpload` 是表单里的一行；`name` 被收紧成必填——表单字段没绑 `name`
  就是个「填了也不提交」的坑，pro 自己的类型里它是可选的。
- 老数据（字符串数组）直接渲染成缩略图，**不做数据迁移**。

### `RegionCascader` / `ProFormRegionCascader`

省/市/区三级级联，值是**编码路径**（`['33','3301','330106']`），选项默认取
`element-china-area-data` 的 `regionData`。`changeOnSelect` 关着：只有选到区一级才算
选完，半截路径存进库就是一个定位不到的区划。

### `region.ts`

数据源与纯函数：`REGION_DATA`、`namesToPath`、`pathToNames`、`pathToNodes`、
`regionFields`、`emptyRegion`。**不复制数据文件**——直接 re-export 依赖的数据。

- `regionFields(path, original)` 把编码路径摊成六个字段（三个区名 + 三个编码），
  提交时调一次。**解析不出来时原值兜底**：库里可能有历史自由文本（「北京」而非
  「北京市」），级联框解析不了、用户也没碰过这个字段，此时必须原样保留，绝不能因为
  打开一次编辑就把老数据洗成空。
- 名字与编码**并存不冗余**：名字给人看，编码用来定位与迁移。数据源会过期（另一份
  `pca-code.json` 里至今挂着 2021 年撤销的「下城区」），而**编码事后算不出来、名字随时
  能从编码推出来**；全国还有 28 组重名区县，只有三元组能定位。
- 数据只覆盖 **31 个省级，不含港澳台**。

## 数据源与后端回填的接缝

级联选项用 npm 包，后端回填工具是 Go、读不了 npm 包，所以由本包导出成 JSON 当入参：

```sh
pnpm --filter @panda-v2/ui regions:export /tmp/regions.json
go run ./cmd/backfill-region -regions /tmp/regions.json        # 在 merchant-service 里预演
```

**导出的 JSON 不提交进仓库**：那份副本从提交那刻就开始漂移，而区划数据恰恰会过期。

## 加新组件

1. 建 `src/<Name>/index.tsx`，导出组件本体与 props 类型。
2. 需要表单形态就再加一个 `ProForm<Name>` 薄包装，用 **`ProFormField`**——它是
   `createField` 造出来的，带 Col 包装，能接住 `colProps` 与 `grid` 表单的栅格。
   **不要用 `ProFormItem`**：那只是个裸 `Form.Item`，在 `grid` 表单里会脱离栅格，
   还会把 `colProps` 漏到 DOM 上。
3. 在 `src/index.ts` 里导出组件与类型。
4. 纯逻辑抽成不依赖 React 的函数，单测直接喂它（本包目前没有 jsdom/RTL，与
   `admin-web` 一致，测试都是纯逻辑）。跑 `pnpm typecheck && pnpm test`。

## 脚本

```sh
pnpm typecheck   # tsc --noEmit
pnpm test        # vitest run
pnpm build       # tsc --noEmit（本包无产物，宿主负责编译）
pnpm regions:export [file]   # 导出区划 JSON；不给路径就写 stdout
```
