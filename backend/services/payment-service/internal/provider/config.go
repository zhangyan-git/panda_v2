package provider

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// ErrConfigInvalid：这棵配置树里的某个键缺失、类型不对，或者取了一个我们不支持的值。
//
// 它与 ErrSecretNotConfigured 有意分开：那个是「凭据没配」，这个是「配置本身拼错了」。
// 两者的收场不同——前者去 .env 补一行，后者是代码或部署配置的 bug——而排查的人只有错误串可看。
//
// 单独的哨兵还换来一件事：把「配置解不开」与「配置解得开、但渠道不认这笔」分开，而后者才是
// 真正需要打一通电话去问渠道的问题。（早先后台有一个「试跑」按钮就是靠这条分界线把两者并排
// 显示给人看的；那个按钮随配置面一起删了，分界线本身留着。）
var ErrConfigInvalid = errors.New("payment channel config is invalid")

// Config 是适配器认识的那棵报文配置树。
//
// 它是 map[string]any 而不是 map[string]string：报文天生分层（endpoints / sign / request /
// response / notify），拍平成 "sign.algorithm" 这样的**键名**会让拼的一方与读的一方各说各话
// ——改一段就要同时改三处字符串常量，而漏改一处不会报错，只会静默地取到空串。
//
// 这棵树今天是**代码拼的**：internal/catalog 的 umsChannel 按部署配置（环境变量）拼出
// ums.Parse 认识的那个形状。早先它来自 payment_channels.config 那一列、由运营在后台填。
// 适配器不需要知道这个差别，也就不该知道——它只看见一棵树。
//
// **这里不放密钥**：密钥是另一条路，Channel.SecretEnv 里存的是**变量名**，值在签名那一刻才
// os.Getenv（见 internal/provider/ums）。所以这棵树是可以被打印的——它只装账户值。
type Config map[string]any

// Object 取一个子对象。路径用点号分隔（`sign`、`notify`）。
//
// **键不存在返回空对象、不算错**：调用方多半会接着读里面的键，而那一层读空串时会给出
// 「某某是必填的」这条更贴切的错——在这一层就报「没有 sign 段」，信息量反而不如它。
// 键存在但**不是对象**才是这一层的错：那说明有人把一段声明写成了字符串或数字，
// 这时往下读是读不出东西的，早报比晚报好。
func (c Config) Object(path string) (Config, error) {
	value, found, err := c.lookup(path)
	if err != nil {
		return nil, err
	}
	if !found || value == nil {
		return Config{}, nil
	}
	object, ok := value.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("%w: %s must be an object, got %s", ErrConfigInvalid, path, describe(value))
	}
	return Config(object), nil
}

// String 读一个标量。
//
// 数字与布尔按字符串读（`"timeout": 30` 读出来是 `"30"`）：配置文件是人手填的，JSON 里
// 写 `30` 与写 `"30"` 在填的人看来是同一件事，为这个报错只会让人怀疑是不是自己看错了文档。
// 对象与数组返回空串——那两种形状在这两列里没有含义。
func (c Config) String(path string) string {
	value, found, err := c.lookup(path)
	if err != nil || !found {
		return ""
	}
	return scalarString(value)
}

// Has 判断一个路径上的键存不存在。
//
// String 把「键不在」与「键在但是空的」都读成空串（那是它对标量的正确处置），但读一份**渠道
// 应答**时这两件事不一样：判据字段整个不在，说明这份应答我们没读懂（结果不明）；字段在、
// 只是值为空，说明渠道明确地回了一个不是成功值的东西（渠道拒了）。把前者当成后者，会让
// 一次「对面可能已经建了单」变成一次「换个方式重付」。
func (c Config) Has(path string) bool {
	_, found, err := c.lookup(path)
	return err == nil && found
}

// RequireString 读一个必填的标量，缺失或为空时报错。
//
// error 里带**完整路径**（`config.sign.algorithm`）：配置是一棵树，只说「algorithm 没填」
// 会让人去 request 段里找。
func (c Config) RequireString(path string) (string, error) {
	value := strings.TrimSpace(c.String(path))
	if value == "" {
		return "", fmt.Errorf("%w: config.%s is required", ErrConfigInvalid, path)
	}
	return value, nil
}

// Bool 读一个布尔，缺失或不是布尔时返回 def。
//
// 字符串 "true"/"false"（不分大小写）也认：配置是手写的，有人在 JSON 里写 `"true"`。
func (c Config) Bool(path string, def bool) bool {
	value, found, err := c.lookup(path)
	if err != nil || !found {
		return def
	}
	switch typed := value.(type) {
	case bool:
		return typed
	case string:
		parsed, err := strconv.ParseBool(strings.TrimSpace(typed))
		if err != nil {
			return def
		}
		return parsed
	default:
		return def
	}
}

// Strings 读一个**扁平**的字符串映射（request 的字段映射、response.payParams、static）。
//
// 元素只能是标量：里面再嵌一层就报错并点名是哪个键。这与 payment_methods.params 那条规矩
// 逐字相同（见 repository.jsonStrings），理由是同一件事——映射的键会变成渠道报文里的字段名，
// 而一个嵌套对象做不了字段名。
func (c Config) Strings(path string) (map[string]string, error) {
	object, err := c.Object(path)
	if err != nil {
		return nil, err
	}
	out := make(map[string]string, len(object))
	for key, value := range object {
		switch value.(type) {
		case map[string]any, []any:
			return nil, fmt.Errorf("%w: config.%s.%s must be a scalar, got %s",
				ErrConfigInvalid, path, key, describe(value))
		}
		out[key] = scalarString(value)
	}
	return out, nil
}

// Flat 把整棵对象树压成一层字符串映射。
//
// 给「k=v 拼串求摘要」那一类签名用：它们的待签串是一串扁平的键值对，而报文可能带嵌套
// （`{"data":{"status":"1"}}`）。**嵌套的值丢掉**，并且在这一点上不留情面——丢掉的后果是
// 签名对不上，那是一个响亮的失败；反过来，把 `map[status:1]` 这种 Go 的字面量形式拼进待签
// 串是一种**永远对不上、但看起来像「算法错了」**的静默错误。
//
// **空值保留**（与 Object 里的读法不同）：待签串里到底要不要空值由签名规则的 SkipEmpty 决定，
// 而它应当按报文原样判断。在这里先滤掉的话，一个 SkipEmpty=false 的渠道永远验不过签，而
// 原因看上去会是「对面算错了」。
func (c Config) Flat() map[string]string {
	out := make(map[string]string, len(c))
	for name, value := range c {
		switch value.(type) {
		case map[string]any, []any:
			continue
		}
		out[name] = scalarString(value)
	}
	return out
}

// StringList 读一个字符串列表。
//
// 数组（`["2","3"]`）与逗号分隔的单串（`"2,3"`）都认，空项丢掉：这两种写法在运营眼里是
// 同一件事，而「我写的是数组为什么报错」是个只浪费时间的错误。缺键返回空切片。
//
// 列表里的元素同样只允许标量，理由见 Strings。
func (c Config) StringList(path string) []string {
	value, found, err := c.lookup(path)
	if err != nil || !found || value == nil {
		return nil
	}
	switch typed := value.(type) {
	case []any:
		out := make([]string, 0, len(typed))
		for _, item := range typed {
			switch item.(type) {
			case map[string]any, []any:
				continue
			}
			if text := strings.TrimSpace(scalarString(item)); text != "" {
				out = append(out, text)
			}
		}
		return out
	case string:
		var out []string
		for _, part := range strings.Split(typed, ",") {
			if text := strings.TrimSpace(part); text != "" {
				out = append(out, text)
			}
		}
		return out
	default:
		return nil
	}
}

// lookup 按点号路径取一个原始值。
//
// 返回 (值, 是否存在, 错误)：路径中间遇到非对象是错误（往下读不出东西了），
// 中途缺键与末段缺键都算「不存在」——调用方对两者的处置一样。
func (c Config) lookup(path string) (any, bool, error) {
	segments := strings.Split(path, ".")
	var current any = map[string]any(c)
	for index, segment := range segments {
		object, ok := current.(map[string]any)
		if !ok {
			return nil, false, fmt.Errorf("%w: config.%s is not an object, so %s cannot be read out of it",
				ErrConfigInvalid, strings.Join(segments[:index], "."), path)
		}
		value, found := object[segment]
		if !found {
			return nil, false, nil
		}
		current = value
	}
	return current, true, nil
}

// scalarString 把一个标量读成字符串。对象与数组读成空串（见 String 的注释）。
//
// 整数那三个分支只在 **Go 代码直接构造一份 config** 时命中：库里的 config 是 JSONB，解出来
// 永远是 float64。认它们不是为了宽容，是为了不让「Go 里写 200」与「库里存 200.0」这两条路
// 读到不同的值——那种分歧的表现是一个配置项在测试里生效、上了线却悄悄回落到默认值
// （`notify.ack.failStatus` 就这么踩过一次）。
func scalarString(value any) string {
	switch typed := value.(type) {
	case string:
		return typed
	case float64:
		// JSON 数字在 Go 里是 float64。这两列里的数字是超时秒数、序号一类的整数，
		// %v 会打出 1.2e+06 这种形状，所以走整数格式（与 repository.jsonStrings 同款）。
		return strconv.FormatInt(int64(typed), 10)
	case int:
		return strconv.Itoa(typed)
	case int32:
		return strconv.FormatInt(int64(typed), 10)
	case int64:
		return strconv.FormatInt(typed, 10)
	case bool:
		return strconv.FormatBool(typed)
	case nil:
		// JSON null 与「键不存在」在这两列里没有区别（都是「没填」）。
		return ""
	default:
		return ""
	}
}

// describe 给错误串用的类型名。**不打印值本身**：这一列理论上不放凭据，但「理论上」不足以
// 让一个错误串把整段配置抄进日志——判错了类型说明写的人本来就在乱填。
func describe(value any) string {
	switch value.(type) {
	case map[string]any:
		return "an object"
	case []any:
		return "an array"
	case string:
		return "a string"
	case float64:
		return "a number"
	case bool:
		return "a boolean"
	case nil:
		return "null"
	default:
		return fmt.Sprintf("a %T", value)
	}
}
