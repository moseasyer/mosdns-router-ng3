// dnsmasq-china-list 的规则文件转换。
//
// 上游语法只有一种形状:`server=/<域名>/<地址>`。地址是**上游选定的解析器**
// (实测三个文件全部是 114.114.114.114),不是语法占位符。本项目不接受这个
// 选择——国内分支只应到达 DHCP 租约发布的地址,既有门禁
// chinesePublicDNSAddresses 与 FORBIDDEN_ADDRESSES 都在扫它。因此这里读出
// 域名、**丢弃地址**,地址不进入任何产物。
//
// 形状不认识、entry 读不到、解析出 0 条,都是失败,不是一个尽力而为的结果。
// 带着缺口发布的列表是一个洞:每一条漏掉的规则都让一个本该走国内的域名走
// 国外,而没有任何东西会报告这件事。
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

// chinaConfLine 匹配一行有效规则。分组 1 是域名,分组 2 是地址。
//
// 地址只验证合法性、不被使用,但仍然要求它存在且合法:上游若改了形状,
// 这里应当失败,而不是取走域名那一半继续。
var chinaConfLine = regexp.MustCompile(`^server=/([A-Za-z0-9.-]+)/([0-9.]+)$`)

// disabledReason 匹配上游自己停用的那一行,取值是它给出的原因。
var disabledReason = regexp.MustCompile(`^#server=/[A-Za-z0-9.-]+/[0-9.]+\s*#\s*Disabled:\s*(.+)$`)

// ChinaConfResult 是一次转换的产物。
type ChinaConfResult struct {
	// Expressions 是排序去重后的 MOSDNS 表达式,只有 domain: 一种。
	Expressions []string
	// Comments 是上游停用条目的原因,原样保留为注释行。
	//
	// 保留它们是因为丢掉等于把上游的取舍记录一起丢掉;而原样保留会把
	// 114.114.114.114 写进发布列表,违反既有门禁。只留链接这一段。
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
		countBefore := len(result.Expressions)
		for number, line := range strings.Split(string(body), "\n") {
			text := strings.TrimSpace(line)
			if text == "" {
				continue
			}
			// 注释在第一个 `#` 处开始,与 dnsmasq 与 MOSDNS 都一致。
			if comment := disabledReason.FindStringSubmatch(text); comment != nil {
				reason := strings.TrimSpace(comment[1])
				if !comments[reason] {
					comments[reason] = true
					result.Comments = append(result.Comments, "# disabled upstream: "+reason)
				}
				continue
			}
			if strings.HasPrefix(text, "#") {
				continue
			}
			matches := chinaConfLine.FindStringSubmatch(text)
			if matches == nil {
				return ChinaConfResult{}, fmt.Errorf("%s:%d: not a rule this converter defines: %q", entry, number+1, text)
			}
			if _, err := netip.ParseAddr(matches[2]); err != nil {
				return ChinaConfResult{}, fmt.Errorf("%s:%d: %q is not an IPv4 address: %w", entry, number+1, matches[2], err)
			}
			name := NormalizeChinaDomain(matches[1])
			if name == "" {
				return ChinaConfResult{}, fmt.Errorf("%s:%d: %q is not a domain this gateway can match", entry, number+1, matches[1])
			}
			expression := "domain:" + name
			if seen[expression] {
				continue
			}
			seen[expression] = true
			result.Expressions = append(result.Expressions, expression)
		}
		if len(result.Expressions) == countBefore {
			return ChinaConfResult{}, fmt.Errorf("%s: resolved to no rules, and an empty China set sends every query abroad", entry)
		}
	}

	if len(result.Expressions) == 0 {
		return ChinaConfResult{}, errors.New("the named entries resolved to no rules at all")
	}
	sort.Strings(result.Expressions)
	sort.Strings(result.Comments)
	return result, nil
}
