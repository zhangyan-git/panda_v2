// 把 element-china-area-data 的 regionData 导出成 JSON，作为后端回填工具的入参。
//
// 回填工具是 Go，读不了 npm 包，这是两者之间必须显式处理的接缝：前端用这个包生成
// 级联选项，后端用它把历史门店的「省/市/区」名字翻成编码。两边必须来自同一份数据，
// 所以导出这一个源，而不是再抄一份。
//
// 刻意不把导出的 JSON 提交进仓库：那份副本从提交的那一刻就开始漂移，而区划数据
// 恰恰是「会过期」的东西——NewCoffee 自带的 pca-code.json 里至今还挂着 2021 年
// 就已撤销的「下城区」。
//
//   node scripts/export-regions.mjs /tmp/regions.json   # 写文件
//   node scripts/export-regions.mjs                     # 写 stdout
import { createRequire } from 'node:module';
import { writeFileSync } from 'node:fs';

const require = createRequire(import.meta.url);
// 这个包是 CJS（main 指向 dist/element-china-area-data.cjs），用 require 拿最稳，
// 不依赖 Node 对 CJS 具名导出的静态分析。
const { regionData } = require('element-china-area-data');

if (!Array.isArray(regionData) || regionData.length === 0) {
  console.error('regionData 为空，导出中止');
  process.exit(1);
}

const json = JSON.stringify(regionData);
const target = process.argv[2];
if (target) {
  writeFileSync(target, `${json}\n`);
  console.error(`已写入 ${target}：${regionData.length} 个省级`);
} else {
  process.stdout.write(json);
}
