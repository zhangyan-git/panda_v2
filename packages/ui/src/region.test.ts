import { describe, expect, it } from 'vitest';

import {
  REGION_DATA,
  emptyRegion,
  namesToPath,
  pathToNames,
  pathToNodes,
  regionFields,
} from './region';

describe('REGION_DATA 数据源', () => {
  // 数据源过期是这件事里最贵的失败模式，而它**不会**以任何形式报错：级联框照样能选，
  // 只是选项里悄悄少了新成立的区、多了已撤销的区。所以把「哪份数据」钉成可执行断言。
  it('杭州市的下一级是当前的区，不是 2021 年那份', () => {
    const hangzhou = REGION_DATA.find((p) => p.label === '浙江省')?.children?.find(
      (c) => c.label === '杭州市',
    );
    expect(hangzhou?.value).toBe('3301');
    const districts = (hangzhou?.children ?? []).map((d) => d.label);
    // 2021 年杭州市行政区划调整：下城区/江干区撤销，临平区/钱塘区设立。
    expect(districts).toContain('临平区');
    expect(districts).toContain('钱塘区');
    expect(districts).not.toContain('下城区');
    expect(districts).not.toContain('江干区');
  });

  it('31 个省级，每个省级都摊得开市与区', () => {
    // 只有 31 个：不含港澳台。
    expect(REGION_DATA).toHaveLength(31);
    for (const province of REGION_DATA) {
      expect(province.children?.length ?? 0).toBeGreaterThan(0);
      for (const city of province.children ?? []) {
        expect(city.children?.length ?? 0).toBeGreaterThan(0);
      }
    }
  });
});

describe('namesToPath / pathToNames', () => {
  it('名字进、编码出，名字再出去', () => {
    const path = namesToPath(['浙江省', '杭州市', '西湖区']);
    expect(path).toEqual(['33', '3301', '330106']);
    expect(pathToNames(path)).toEqual(['浙江省', '杭州市', '西湖区']);
    // 传几层回几层，长度不做校验（表单那里一律传三元组）。
    expect(namesToPath(['浙江省', '杭州市'])).toEqual(['33', '3301']);
  });

  it('直辖市在市一级是「市辖区」，与小程序端现有的选择器一致', () => {
    expect(namesToPath(['北京市', '市辖区', '东城区'])).toEqual(['11', '1101', '110101']);
  });

  it('同名区县按整个三元组定位，不会串到别的城市', () => {
    // 全国有 28 组区县同名。这一组连省都一样，只有市分得开：光靠区名会串。
    expect(namesToPath(['河北省', '石家庄市', '桥西区'])).toEqual(['13', '1301', '130104']);
    expect(namesToPath(['河北省', '张家口市', '桥西区'])).toEqual(['13', '1307', '130703']);
  });

  it('查不到连不上就返回 undefined', () => {
    // 「下城区」是被撤销的名字：数据源过期时才会查得到，这行反向钉住了新鲜度。
    expect(namesToPath(['浙江省', '杭州市', '下城区'])).toBeUndefined();
    // 历史自由文本（库里真有这种）。第二层对得上也没用，第三层接不上就是接不上。
    expect(namesToPath(['北京', '北京市', '东城区'])).toBeUndefined();
    // 每一层都落在上一层里，但「浙江省 / 朝阳区」这种组合根本不成立。
    expect(namesToPath(['浙江省', '杭州市', '朝阳区'])).toBeUndefined();
  });

  it('任一空值就是「没选完」，返回 undefined', () => {
    // 这条是给表单用的：库里 district 为空时必须回 undefined，
    // 半截路径会让级联框显示成「浙江省 / 杭州市」，看着像已经选好了。
    expect(namesToPath(['浙江省', '杭州市', ''])).toBeUndefined();
    expect(namesToPath(['浙江省', '杭州市', '  '])).toBeUndefined();
    expect(namesToPath(['', '杭州市', '西湖区'])).toBeUndefined();
    expect(namesToPath([undefined, undefined, undefined])).toBeUndefined();
    expect(namesToPath([])).toBeUndefined();
  });

  it('空格照吃不误，库里存的是人敲进去的文本', () => {
    expect(namesToPath([' 浙江省 ', '杭州市\t', ' 西湖区'])).toEqual(['33', '3301', '330106']);
  });
});

describe('pathToNodes', () => {
  it('逐层校验父子关系，拼错层级查不到', () => {
    expect(pathToNodes(['33', '3301', '330106'])?.map((n) => n.label)).toEqual([
      '浙江省',
      '杭州市',
      '西湖区',
    ]);
    // 1101（北京市辖区）不是浙江省的下级。
    expect(pathToNodes(['33', '1101', '110101'])).toBeUndefined();
    expect(pathToNodes(['99', '9901', '990101'])).toBeUndefined();
    expect(pathToNodes([])).toBeUndefined();
    expect(pathToNodes(undefined)).toBeUndefined();
  });

  it('允许半截路径：中间层不清空，缺哪层就少哪层', () => {
    expect(pathToNames(['33', '3301'])).toEqual(['浙江省', '杭州市']);
  });
});

describe('regionFields', () => {
  it('完整路径同时给出三个名字与三个编码', () => {
    expect(regionFields(['33', '3301', '330106'])).toEqual({
      province: '浙江省',
      city: '杭州市',
      district: '西湖区',
      provinceCode: '33',
      cityCode: '3301',
      districtCode: '330106',
    });
  });

  it('空值、残缺路径、查不到的编码都回退到原值', () => {
    const original = {
      province: '北京',
      city: '北京市',
      district: '东城区',
      provinceCode: '',
      cityCode: '',
      districtCode: '',
    };
    expect(regionFields(undefined, original)).toEqual(original);
    expect(regionFields([], original)).toEqual(original);
    expect(regionFields(['99', '9901', '990101'], original)).toEqual(original);
    // 用户清空级联框 = 不改动，而不是「把区划洗成空」。
    expect(regionFields(undefined, original).province).toBe('北京');
  });

  it('没有原值时给出六个空串，不是 undefined', () => {
    expect(regionFields(undefined)).toEqual(emptyRegion());
    expect(regionFields(undefined, null)).toEqual({
      province: '',
      city: '',
      district: '',
      provinceCode: '',
      cityCode: '',
      districtCode: '',
    });
    for (const value of Object.values(regionFields(undefined))) {
      expect(value).toBe('');
    }
  });

  it('原值里显式 undefined 的字段不覆盖兜底空串', () => {
    // Store 类型上这些字段是 string，但接口返回的历史行可能是 null/undefined，
    // 展开它会把兜底值一起覆盖掉，所以要按字符串筛一遍。
    const fields = regionFields(undefined, {
      province: null as unknown as string,
      city: undefined,
      district: '东城区',
      provinceCode: undefined,
      cityCode: null as unknown as string,
      districtCode: '110101',
    });
    expect(fields).toEqual({
      province: '',
      city: '',
      district: '东城区',
      provinceCode: '',
      cityCode: '',
      districtCode: '110101',
    });
  });
});
