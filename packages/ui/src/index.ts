// @panda-v2/ui：admin-web 与 merchant-web 共用的组件。
//
// 跨包引用走 `main → src/index.ts` 的裸 TS/TSX（与 packages/api 同形），没有构建产物：
// 这个包只被 Umi 打包进宿主应用，没有一个「独立发布」的场景，多一层产物只是多一层
// 会对不上的东西。新增组件时记得同步维护这里的导出。
export { ImageUpload, ProFormImageUpload } from './ImageUpload';
export type { ImageUploadProps, ImageUploadValue, ProFormImageUploadProps } from './ImageUpload';

export { ProFormRegionCascader, RegionCascader } from './RegionCascader';
export type { ProFormRegionCascaderProps, RegionCascaderProps } from './RegionCascader';

export { REGION_DATA, emptyRegion, namesToPath, pathToNames, pathToNodes, regionFields } from './region';
export type { RegionFields, RegionNode, RegionPath } from './region';
