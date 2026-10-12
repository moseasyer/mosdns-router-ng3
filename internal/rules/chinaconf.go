// dnsmasq-china-list 的规则文件转换。
//
// 上游语法只有一种形状:`server=/<域名>/<地址>`。地址是**上游选定的解析器**
// (实测三个文件全部是 114.114.114.114),不是语法占位符。本项目不接受这个
// 选择——国内分支只应到达 DHCP 租约发布的地址,既有门禁
// chinesePublicDNSAddresses 与 FORBIDDEN_ADDRESSES 都在扫它。因此这里读出
// 域名、**丢弃地址**,地址不进入任何产物。
//
// 这条约束覆盖的不只是发布列表:错误信息与日志同样是产物,而
// chinesePublicDNSAddresses 扫的是渲染出来的文档、不是构建输出,所以一个从
// 错误信息里漏出去的地址不会被任何东西拦下。转换器因此**不回显文件内容**——
// 报错只给 entry、行号和它期待的形状;停用原因里出现的地址形状片段被替换成
// `<address>`。形状不认识、entry 读不到、某个 entry 自己一条规则也没有,都是
// 失败,不是一个尽力而为的结果。带着缺口发布的列表是一个洞:每一条漏掉的规则
// 都让一个本该走国内的域名走国外,而没有任何东西会报告这件事。
package rules

import (
	"errors"
	"fmt"
	"io/fs"
	"net/netip"
	"regexp"
	"sort"
	"strings"
)

// ChinaConfEntries 是主仓库中被转换的三个文件。这是一个 pin 住的固定集合:
// 上游新增文件不会被自动纳入,少一个则整个转换失败。
var ChinaConfEntries = []string{
	"accelerated-domains.china.conf",
	"google.china.conf",
	"apple.china.conf",
}

// chinaConfRuleShape 与 chinaConfDisabledShape 是本转换器能定义的两行形状,
// 它们只出现在错误信息里。报错要能告诉操作者它期待什么形状,又不能把文件内容
// 连同地址一起回显出来——这两件事只能同时做到,前提是形状被写成常量。
const (
	chinaConfRuleShape     = `server=/<domain>/<address>`
	chinaConfDisabledShape = `#server=/<domain>/<address> # Disabled: <reason>`
)

// chinaConfLine 匹配一行有效规则。分组 1 是域名,分组 2 是地址。
//
// 地址只验证合法性、不被使用,但仍然要求它存在且合法:上游若改了形状,
// 这里应当失败,而不是取走域名那一半继续。
var chinaConfLine = regexp.MustCompile(`^server=/([A-Za-z0-9.-]+)/([0-9.]+)$`)

// disabledReason 匹配上游自己停用的那一行,取值是它给出的原因。
var disabledReason = regexp.MustCompile(`^#server=/[A-Za-z0-9.-]+/[0-9.]+\s*#\s*Disabled:\s*(.+)$`)

// addressShaped 找出读起来像一段 IPv4 地址的连续数字:至少四段,点分隔。
//
// 两个刻意的选择,都是因为按"能否解析"来判断会把最该拦下的几种漏掉:
//
//   - 它**不**要求 netip 解析通过。`010.1.1.1`、`999.1.1.1`、`999.999.999.999`
//     都不是合法地址,但上游写下它们时指的就是地址,门禁和读日志的人也会把
//     它们读成地址。
//   - 它**不**要求两侧是词边界。带 `\b` 时 `see1.2.3.4`、`v1.2.3.4`、
//     `1.2.3.4here` 与 `1234.1.1.1` 全部漏过:前三者的数字开头处没有边界,
//     最后一者的首段是四位而模式只肯吃三位。
//
// `{3,}` 是贪婪的,所以 `1.2.3.4.5` 整段被替换,而不是留下 `<address>.5`
// ——半遮半掩既没去掉地址,也没留下原文,比两个极端都更糟。
//
// 三段(`1.2.3`)不算地址形状,刻意放过:版本号不是地址,而域名里的标签组也
// 不是八位组,`domain:1.2.3` 是必须保留的合法规则。
var addressShaped = regexp.MustCompile(`[0-9]+(?:\.[0-9]+){3,}`)

// redactedAddress 是地址在产物里的样子。
const redactedAddress = "<address>"

// redactAddresses 把 text 里每一个地址形状的片段替换成 redactedAddress。
//
// 它存在的理由是上游的停用原因是自由散文:实测六条原因里五条是链接(其中一条
// 还带 `#fragment`),第六条是 `very unstable`。原因是散文不是规则,不该因为
// 里面碰巧出现一个地址就被整条丢掉,但也绝不能原样进入发布列表。
func redactAddresses(text string) string {
	return addressShaped.ReplaceAllStringFunc(text, func(string) string {
		return redactedAddress
	})
}

// ChinaConfResult 是一次转换的产物。
type ChinaConfResult struct {
	// Expressions 是排序去重后的 MOSDNS 表达式,只有 domain: 一种。
	Expressions []string
	// Comments 是上游停用条目给出的**原因**,每条渲染成一行注释。
	//
	// 保留它们是因为丢掉等于把上游的取舍记录一起丢掉。原文里若含地址形状的
	// 片段会被替换成 `<address>`:原因是散文,不是规则,而门禁扫的是渲染出来
	// 的文档。
	Comments []string
}

// ConvertChinaConfFS 把 entries 里的每个文件转换并合并成一份结果。
//
// 三个文件的内容全部进入同一份结果,因为本项目只读一个列表文件
// (见 configs/mosdns.yaml 的 domain_set),分流规则是同一种东西,来源
// 不改变它被匹配的方式。
func ConvertChinaConfFS(fsys fs.FS, entries []string) (ChinaConfResult, error) {
	if fsys == nil {
		return ChinaConfResult{}, errors.New("a source filesystem is required")
	}
	if len(entries) == 0 {
		return ChinaConfResult{}, errors.New("no entry was named to convert")
	}

	var result ChinaConfResult
	seen := make(map[string]bool)
	comments := make(map[string]bool)

	// Every entry is read before any of them is converted, so a pin naming a
	// file upstream has dropped reports every missing entry at once. Reporting
	// only the first would leave the other names to be discovered one failing
	// conversion per round, and each intermediate attempt looks like the pin
	// being wrong about exactly one file.
	bodies := make(map[string][]byte, len(entries))
	var unreadable []string
	for _, entry := range entries {
		body, err := fs.ReadFile(fsys, entry)
		if err != nil {
			unreadable = append(unreadable, fmt.Sprintf("%s (%v)", entry, err))
			continue
		}
		bodies[entry] = body
	}
	if len(unreadable) > 0 {
		return ChinaConfResult{}, fmt.Errorf("these entries are named by the pin and could not be read: %s", strings.Join(unreadable, "; "))
	}

	for _, entry := range entries {
		body := bodies[entry]
		// rulesInEntry 数的是**这个 entry 自己**声明了多少条规则,不是它往
		// 结果里新增了多少条。两者在正常数据上相等,但上游确实有跨文件重复的
		// 域名(qiao-cn.com 同时在 accelerated-domains.china.conf:73926 与
		// google.china.conf:85),而一个只贡献重复规则的 entry 并不是空文件。
		// 拿累计总数去比,会把一个装着 111,011 条规则的 entry 报成空 entry,
		// 并附上一句与实情相反的"空列表会把查询都送去国外"。
		rulesInEntry := 0
		for number, line := range strings.Split(string(body), "\n") {
			text := strings.TrimSpace(line)
			if text == "" {
				continue
			}
			// 注释从 `#` 开始,与 dnsmasq 一致。但本转换器**不**在第一个 `#`
			// 处截断后照单全收:注释行要么是它认识的停用行,要么是根本不指向
			// 规则的说明,两者之外的一切都拒绝。规则行改了形状会失败,停用行
			// 改了形状也必须失败,否则上游一次改写法就把停用原因悄悄丢掉,而
			// 丢掉原因等于丢掉上游的取舍记录。
			if comment := disabledReason.FindStringSubmatch(text); comment != nil {
				reason := redactAddresses(strings.TrimSpace(comment[1]))
				if !comments[reason] {
					comments[reason] = true
					result.Comments = append(result.Comments, "# disabled upstream: "+reason)
				}
				continue
			}
			if strings.HasPrefix(text, "#") {
				// A comment that is trying to be a removed rule but is not one
				// this converter defines is refused. A comment that never was a
				// rule -- upstream's two prose headers in google.china.conf are
				// the real examples -- is skipped.
				after := strings.TrimSpace(strings.TrimPrefix(text, "#"))
				if strings.HasPrefix(after, "server=") {
					return ChinaConfResult{}, fmt.Errorf("%s:%d: not a disabled entry this converter defines; a disabled entry is %s", entry, number+1, chinaConfDisabledShape)
				}
				continue
			}
			matches := chinaConfLine.FindStringSubmatch(text)
			if matches == nil {
				return ChinaConfResult{}, fmt.Errorf("%s:%d: not a rule this converter defines; a rule is %s", entry, number+1, chinaConfRuleShape)
			}
			// The address is validated and never used, and the message names
			// the line rather than quoting the field: `[0-9.]+` admits
			// address-SHAPED text such as 010.1.1.1, and quoting it here would
			// be exactly the leak the discarding is meant to prevent.
			if _, err := netip.ParseAddr(matches[2]); err != nil {
				return ChinaConfResult{}, fmt.Errorf("%s:%d: the address field is not an IPv4 address, so the line is not a rule this converter defines", entry, number+1)
			}
			name := NormalizeChinaDomain(matches[1])
			if name == "" {
				return ChinaConfResult{}, fmt.Errorf("%s:%d: the domain field is not a domain this gateway can match", entry, number+1)
			}
			// Address-SHAPED, not parseable, and deliberately the same predicate
			// the redaction above uses. netip refuses 999.1.1.1, 010.1.1.1 and
			// 1.2.3.4.5, so a parse-based guard would publish all three as
			// `domain:999.1.1.1` and friends -- the same reasoning that made the
			// redaction shape-based, applied to the one field that becomes a
			// published expression.
			if addressShaped.MatchString(name) {
				return ChinaConfResult{}, fmt.Errorf("%s:%d: the domain field reads as an IPv4 address rather than a domain name, and publishing it would put an address in the list", entry, number+1)
			}
			expression := "domain:" + name
			rulesInEntry++
			if seen[expression] {
				continue
			}
			seen[expression] = true
			result.Expressions = append(result.Expressions, expression)
		}
		if rulesInEntry == 0 {
			return ChinaConfResult{}, fmt.Errorf("%s: resolved to no rules, so the China set is short of what the pin says it contains", entry)
		}
	}

	// Unreachable while the per-entry gate above holds, and it will never be the
	// thing that fires: a non-empty `entries` plus `rulesInEntry` on every one of
	// them means a successful return already carries at least one expression.
	// It is kept anyway, and the honest reason is that it is a contract
	// backstop rather than a check -- if someone later adds a path that skips an
	// entry, this is the alarm, and an empty SUCCESS result is the one failure
	// that would silently publish an empty China set with nothing to notice it.
	// Unlike the dead test assertion the review rejected, this one cannot be
	// removed by accident without also removing the per-entry gate.
	if len(result.Expressions) == 0 {
		return ChinaConfResult{}, errors.New("the named entries resolved to no rules at all")
	}
	sort.Strings(result.Expressions)
	sort.Strings(result.Comments)
	return result, nil
}
