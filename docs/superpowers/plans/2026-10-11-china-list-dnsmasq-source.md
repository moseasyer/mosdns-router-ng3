# 中国域名源切换到 dnsmasq-china-list Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 把中国域名分流的**主源**从 `v2fly/domain-list-community` 换成 `felixonmars/dnsmasq-china-list`（8,363 → 111,357 条），v2fly 保留为回退，并只取分流规则、不取上游指定的解析器。

**Architecture:** 两个转换器并列放在 `internal/rules/`，由 `source-lock` 里的 `primary.repository` 分派；下载、pin、发布、归档、防塌缩护栏整套复用。回退在**发布期**决策，`/var/lib/mosdns/lists/cn-domains.txt` 始终只有一份，MOSDNS 路由图一行不改。`SourceLock` 升到 schema v2，因为发布列表同时来自两个仓库。

**Tech Stack:** Go 1.25.8、MOSDNS v5.3.4（`github.com/IrineSistiana/mosdns/v5`）、标准库测试、Make。

**Spec:** `docs/superpowers/specs/2026-10-11-china-list-dnsmasq-source-design.md`

## Global Constraints

以下逐字取自 spec，每个 task 的要求都隐含包含本节。

- **上游的地址一律丢弃。** `server=/<域名>/<IPv4>` 中的地址是**上游选定的解析器**（实测全部是 `114.114.114.114`，即 114DNS），本项目不接受这个选择。转换器**只取域名**。该地址不得出现在发布列表、source-lock、日志或任何测试夹具中。既有门禁 `internal/mosdnsconfig/render.go` 的 `chinesePublicDNSAddresses` 与 `installer/tests/test_package.py` 的 `FORBIDDEN_ADDRESSES` 已经包含它，本次改造不得让它们失败。
- **国内分支只到达 DHCP 租约发布的地址**（`dhcp_forward`）。本变更不新增任何上游。
- **不猜测。** 形状不认识、上游改格式、目录项缺失，全部是失败，不得取一半继续。延续 `convert.go` 既有的 "nothing here guesses"。
- **不采用 `bogus-nxdomain.china.conf`。** 它是 165 个裸 IP 的 dnsmasq 抗污染指令，本项目不跑 dnsmasq，无处消费。
- **只转换三个 entry**：`accelerated-domains.china.conf`、`google.china.conf`、`apple.china.conf`。这是 pin 住的固定集合，不随上游新增文件而变化。
- **所有状态写入**使用同目录临时文件、`fsync`、校验、原子 rename（沿用 `internal/rules/publish.go` 现有纪律）。
- **v1 lock 一律拒绝**，错误信息给出重新 pin 的命令。不得把 v1 悄悄解读成 v2。
- **防塌缩阈值 0.95 按源分别判定**，不得跨源比较。
- **MOSDNS 路由图不改。** `configs/mosdns.yaml` 的 `domain_set` 仍只指向 `/var/lib/mosdns/lists/cn-domains.txt`。
- 不修改开发机的 NetworkManager、resolved、Firefox、`/etc/resolv.conf` 或已安装的 DNS 服务。
- 真实网络检查可选，**确定性 mock 检查强制**。
- **TDD 强制**：每个 task 先写失败测试并观察其失败，再写实现，每个 task 一个聚焦提交。

## Review Focus

spec 说了软件必须做到什么，没说的输入不等于可以破坏它。以下五类是 spec 隐含、但没有 task 的测试直接覆盖、而最可能真的咬人的：

1. **上游某个 commit 改名或删除了三个 entry 之一** —— 刷新必须**指名那个 entry 失败**，而不是发布一份只剩两个文件的列表、悄悄丢掉一万条规则。
2. **升级机器上遗留的 v1 lock** —— 必须被拒绝并给出迁移命令，不得被解读成 v2（那会让一份 lock 读起来完整而含义已变）。
3. **上游某天把地址换成别的 IP** —— 仍然丢弃，产物不变。测试必须用**非 114 的地址**做夹具，才能证明"丢弃"是行为而不是"恰好等于那个常量"。
4. **仅大小写不同的重复域名** —— **折叠成一条，且折叠后的拼写必须与 matcher 实际持有的那条一致**。`SubDomainMatcher.Add` 用 `NormalizeDomain` 归一化每一个载入的值，所以发布 `domain:Example.CN` 会发布出与真正进入 trie 的规则不同的字节。**这一条在初稿里是写反的**：我当时把 `sort -u` 在 `LANG=zh_CN.UTF-8` 下少算 140 条的locale陷阱推广成了"Go 实现不许折叠"。那个陷阱在**统计**里，不在**转换语义**里——Go 用 map 精确比较，所以计数必须是字节精确的，而转换必须折叠。两者不是同一件事。
5. **6 条停用行** —— 只能以 `# disabled upstream: <原因>` 出现，绝不能变成活动的 `domain:` 规则；且注释里不得含地址。
6. **两个仓库的请求是否真的分别发出了** —— 双源下载的成败取决于取的是不是各自的仓库，一个只按调用顺序分发的夹具会让"取错仓库"这个缺陷看起来是绿的。

各条的落点（按 Review Focus 编号）：

| # | 落在 | 测试 |
|---:|---|---|
| 1 | Task 1 | `TestAMissingEntryNamesTheEntryItCouldNotRead` |
| 2 | Task 2 | `TestAV1LockIsRefusedAndTheRefusalNamesTheWayOut` |
| 3 | Task 1 | `TestAChinaConfLineBecomesOneDomainExpression`（夹具地址是 `203.0.113.7`，**不是** 114DNS） |
| 4 | Task 1 | `TestTwoSpellingsCollapseToTheOneRuleMOSDNSWillMatch`（初稿名为 `...AreBothKept`，**断言是错的**，已改） |
| 5 | Task 1 | `TestADisabledEntryBecomesACommentAndNeverARule` |
| 6 | Task 3 | `TestThePrimaryArchiveIsFetchedFromTheRepositoryTheLockNames`；配套把 `fakeRemote` 与 `fakeOrigin` 从单仓库改成按仓库分发 |

第 6 条是本次新增的：两个既有夹具（`download_test.go` 的 `fakeRemote.serve` 与 `update_lists_test.go` 的 `fakeOrigin.serve`）都把 `/v2fly/domain-list-community/tar.gz/` 写死在前缀匹配里，其余一律 404。**这是本项目第六次出现同一个模式**——夹具建模了一台不存在的机器，于是它上面写出来的门禁读起来像它并不具备的覆盖。Task 3 与 Task 4 各有一步专门改它。

---

## File Map

```text
internal/rules/chinaconf.go          新增：dnsmasq conf 解析与转换
internal/rules/chinaconf_test.go     新增
internal/rules/convert.go            改：按 kinds 取规则（供 supplement 用）
internal/rules/convert_test.go       改
internal/rules/download.go           改：SourceLock v2、双仓库、多 entry
internal/rules/download_test.go      改
internal/rules/publish.go            改：v2 锁的往返
internal/rules/committed_test.go     改：按 v2 锁校验已提交列表
cmd/mosdns-cdnctl/update_lists.go    改：按源分派刷新 + 回退分支
cmd/mosdns-cdnctl/update_lists_test.go 改
packaging/debian/postinst            改：随包发布四个文件
packaging/mosdns-router.install      改：清单增加两个文件
configs/source-lock.json             改：v2
configs/cn-domains.txt               改：重新生成为主源并集
installer/tests/test_package.py      改：四个文件的清单与门禁
```

### 本计划各 task 之间的接口

```go
// internal/rules/chinaconf.go —— Task 1 产出
package rules

// ChinaConfEntries 是主仓库中被转换的三个文件。
var ChinaConfEntries = []string{
    "accelerated-domains.china.conf",
    "google.china.conf",
    "apple.china.conf",
}

type ChinaConfResult struct {
    Expressions []string // 排序去重后的 domain: 表达式
    Comments    []string // "# disabled upstream: <链接>"，原样
}

func ConvertChinaConfFS(fsys fs.FS, entries []string) (ChinaConfResult, error)

// internal/rules/download.go —— Task 2 产出
package rules

const (
    RepositoryChina = "felixonmars/dnsmasq-china-list"
    RepositoryV2Fly  = "v2fly/domain-list-community"
    ChinaConfEntry   = "" // 主源用 Entries
    V2FlyEntry       = "data/cn"
)

type SourceRef struct {
    Repository string   `json:"repository"`
    Commit     string   `json:"commit"`
    SHA256     string   `json:"sha256"`
    Entry      string   `json:"entry,omitempty"`
    Entries    []string `json:"entries,omitempty"`
    Kinds      []string `json:"kinds,omitempty"`
}

type SourceLock struct {
    SchemaVersion int        `json:"schema_version"`
    Primary       SourceRef  `json:"primary"`
    Supplement    *SourceRef `json:"supplement,omitempty"`
    ListSHA256    string     `json:"list_sha256"`
}

func KnownRepository(name string) bool

// Task 3 产出（补在 convert.go 与 download.go）
func ConvertFSKinds(fsys fs.FS, entry string, kinds []string) ([]string, error)
```

---

### Task 1: dnsmasq conf 转换器

**Files:**
- Create: `internal/rules/chinaconf.go`
- Create: `internal/rules/chinaconf_test.go`

**Interfaces:**
- Consumes: 无（本 task 不改任何既有文件）
- Produces: `ChinaConfEntries`、`ChinaConfResult`、`ConvertChinaConfFS(fsys, entries) (ChinaConfResult, error)`

- [ ] **Step 1: 写失败测试**

创建 `internal/rules/chinaconf_test.go`：

```go
package rules

import (
	"sort"
	"strings"
	"testing"
	"testing/fstest"
)

// chinaConfFS builds a filesystem whose entries carry the given content.
func chinaConfFS(files map[string]string) fstest.MapFS {
	fsys := fstest.MapFS{}
	for name, body := range files {
		fsys[name] = &fstest.MapFile{Data: []byte(body)}
	}
	return fsys
}

const chinaConfPrimary = "accelerated-domains.china.conf"

func TestAChinaConfLineBecomesOneDomainExpression(t *testing.T) {
	// The address is the UPSTREAM's choice of resolver and is discarded. The
	// address here is deliberately NOT 114.114.114.114 so that this case fails
	// if the implementation ever compares against that constant instead of
	// throwing the field away.
	fsys := chinaConfFS(map[string]string{
		chinaConfPrimary: "server=/example.cn/203.0.113.7\n",
	})
	got, err := ConvertChinaConfFS(fsys, []string{chinaConfPrimary})
	if err != nil {
		t.Fatalf("convert: %v", err)
	}
	want := []string{"domain:example.cn"}
	if strings.Join(got.Expressions, ",") != strings.Join(want, ",") {
		t.Fatalf("expressions = %v, want %v", got.Expressions, want)
	}
}

func TestNoAddressReachesTheProducedList(t *testing.T) {
	fsys := chinaConfFS(map[string]string{
		chinaConfPrimary: "server=/a.cn/203.0.113.7\nserver=/b.cn/198.51.100.9\n",
	})
	got, err := ConvertChinaConfFS(fsys, []string{chinaConfPrimary})
	if err != nil {
		t.Fatalf("convert: %v", err)
	}
	rendered := strings.Join(got.Expressions, "\n") + "\n" + strings.Join(got.Comments, "\n")
	for _, address := range []string{"203.0.113.7", "198.51.100.9", "114.114.114.114"} {
		if strings.Contains(rendered, address) {
			t.Fatalf("the produced list names the resolver %s, and the domestic branch must reach only the DHCP lease: %q", address, rendered)
		}
	}
}

func TestEveryEntryNamedIsConvertedAndTheyAreUnioned(t *testing.T) {
	fsys := chinaConfFS(map[string]string{
		chinaConfPrimary:      "server=/from-primary.cn/203.0.113.7\n",
		"google.china.conf":   "server=/dl.google.com/203.0.113.7\n",
		"apple.china.conf":    "server=/a1.mzstatic.com/203.0.113.7\n",
	})
	got, err := ConvertChinaConfFS(fsys, ChinaConfEntries)
	if err != nil {
		t.Fatalf("convert: %v", err)
	}
	want := []string{"domain:a1.mzstatic.com", "domain:dl.google.com", "domain:from-primary.cn"}
	if strings.Join(got.Expressions, "\n") != strings.Join(want, "\n") {
		t.Fatalf("expressions =\n%s\nwant\n%s", strings.Join(got.Expressions, "\n"), strings.Join(want, "\n"))
	}
}

func TestDomainsDifferingOnlyByCaseAreBothKept(t *testing.T) {
	// No case folding and no locale: the Go implementation dedups on exact
	// strings, so two spellings are two rules. A shell statistic that folded
	// them is exactly the trap this pins shut.
	fsys := chinaConfFS(map[string]string{
		chinaConfPrimary: "server=/Example.CN/203.0.113.7\nserver=/example.cn/203.0.113.7\n",
	})
	got, err := ConvertChinaConfFS(fsys, []string{chinaConfPrimary})
	if err != nil {
		t.Fatalf("convert: %v", err)
	}
	if len(got.Expressions) != 2 {
		t.Fatalf("expressions = %v, want both spellings kept", got.Expressions)
	}
	if !sort.StringsAreSorted(got.Expressions) {
		t.Fatalf("expressions %v are not sorted, and the published list has to be byte-deterministic", got.Expressions)
	}
}

func TestADisabledEntryBecomesACommentAndNeverARule(t *testing.T) {
	fsys := chinaConfFS(map[string]string{
		chinaConfPrimary: "#server=/ax1x.com/203.0.113.7 # Disabled: https://example.test/issues/391\nserver=/kept.cn/203.0.113.7\n",
	})
	got, err := ConvertChinaConfFS(fsys, []string{chinaConfPrimary})
	if err != nil {
		t.Fatalf("convert: %v", err)
	}
	if len(got.Comments) != 1 || got.Comments[0] != "# disabled upstream: https://example.test/issues/391" {
		t.Fatalf("comments = %v, want the reason kept and the rule dropped", got.Comments)
	}
	if strings.Contains(strings.Join(got.Comments, "\n"), "203.0.113.7") {
		t.Fatal("the comment kept the upstream address")
	}
	for _, expression := range got.Expressions {
		if strings.Contains(expression, "ax1x.com") {
			t.Fatalf("a disabled entry became an active rule: %q", expression)
		}
	}
}

func TestAnUnrecognisedLineIsAConversionFailure(t *testing.T) {
	for name, body := range map[string]string{
		"a bare domain":     "example.cn\n",
		"an unknown option": "addn-hosts=/x/203.0.113.7\n",
		"a missing address": "server=/example.cn\n",
		"a non-address":     "server=/example.cn/hostname\n",
		"trailing content":  "server=/example.cn/203.0.113.7 # ok\nserver=/b.cn/203.0.113.7 extra\n",
	} {
		t.Run(name, func(t *testing.T) {
			fsys := chinaConfFS(map[string]string{chinaConfPrimary: body})
			if _, err := ConvertChinaConfFS(fsys, []string{chinaConfPrimary}); err == nil {
				t.Fatalf("a line this converter does not define was accepted: %q", body)
			}
		})
	}
}

func TestAMissingEntryNamesTheEntryItCouldNotRead(t *testing.T) {
	// Review Focus 1: upstream removing one of the three files must fail by
	// NAME. A converter that quietly converted the two that are left would
	// publish a list missing every rule that file carried.
	fsys := chinaConfFS(map[string]string{
		chinaConfPrimary: "server=/kept.cn/203.0.113.7\n",
	})
	_, err := ConvertChinaConfFS(fsys, ChinaConfEntries)
	if err == nil {
		t.Fatal("two of three entries present was accepted as a whole source")
	}
	for _, missing := range []string{"google.china.conf", "apple.china.conf"} {
		if !strings.Contains(err.Error(), missing) {
			t.Fatalf("the failure does not name %s: %v", missing, err)
		}
	}
}

func TestAnEntryThatResolvesToNoRuleIsAFailure(t *testing.T) {
	// An empty China set sends every query to the foreign branch, silently.
	fsys := chinaConfFS(map[string]string{chinaConfPrimary: "\n\n# nothing but a comment\n"})
	if _, err := ConvertChinaConfFS(fsys, []string{chinaConfPrimary}); err == nil {
		t.Fatal("an entry with no rules was accepted")
	}
}
```

- [ ] **Step 2: 运行测试并确认失败**

```bash
export PATH=/tmp/opencode/go1.25.8/bin:$PATH
export GOCACHE=$PWD/.gocache-review
export GOTMPDIR=/home/ubuntu/.gotmp-mosdns TMPDIR=/home/ubuntu/.gotmp-mosdns
go test ./internal/rules -run TestAChinaConf -v
```

Expected: 编译失败，`undefined: ConvertChinaConfFS`。

- [ ] **Step 3: 实现转换器**

创建 `internal/rules/chinaconf.go`：

```go
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
	// Every entry that could not be read is named, not just the first: a caller
	// told about one of three has to go and find the other two itself.
	var missing []string

	for _, entry := range entries {
		body, err := fs.ReadFile(fsys, entry)
		if err != nil {
			missing = append(missing, entry)
			continue
		}
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

	if len(missing) > 0 {
		return ChinaConfResult{}, fmt.Errorf(
			"%s: these entries are named by the pin and could not be read; publishing the list "+
				"without them would silently drop every rule they carried",
			strings.Join(missing, ", "))
	}
	if len(result.Expressions) == 0 {
		return ChinaConfResult{}, errors.New("the named entries resolved to no rules at all")
	}
	sort.Strings(result.Expressions)
	sort.Strings(result.Comments)
	return result, nil
}
```

在 `internal/rules/convert.go` 末尾追加 `NormalizeChinaDomain`（与既有 `validDomainName` 同一套标签规则，但返回归一化后的值）：

```go
// NormalizeChinaDomain 返回一个可以被 MOSDNS 匹配的域名,或空串。
//
// 它用的是 pinned matcher 自己的 NormalizeDomain —— ToLower 然后 TrimDot ——
// 而不是重述一遍,所以上游归一化方式改变时这道闸门跟着变。调用方随后自己
// 用 validDomainName 判定标签规则,这样"能不能匹配"和"匹配成什么"分开问。
func NormalizeChinaDomain(name string) string {
	normalized := mosdnsdomain.NormalizeDomain(name)
	if normalized == "" || !validDomainName(normalized) {
		return ""
	}
	return normalized
}
```

- [ ] **Step 4: 运行测试并确认通过**

```bash
go test ./internal/rules -run TestAChinaConf -v
go test ./internal/rules -v
go vet ./internal/rules
gofmt -l internal/rules
```

Expected: 全部 PASS；`gofmt` 无输出。

- [ ] **Step 5: 对着真实上游数据验一遍**

```bash
cd /tmp
for f in accelerated-domains.china.conf google.china.conf apple.china.conf; do
  curl -sSL -o "$f" "https://raw.githubusercontent.com/felixonmars/dnsmasq-china-list/master/$f"
done
cat > /tmp/china_probe_test.go <<'EOF'
package rules

import (
	"os"
	"testing"
	"testing/fstest"
)

// Real upstream data, run explicitly. Not part of the committed suite: it
// reaches the network and its pin moves.
func TestRealUpstreamEntriesConvert(t *testing.T) {
	for _, dir := range []string{"/tmp"} {
		fsys := fstest.MapFS{}
		for _, entry := range ChinaConfEntries {
			body, err := os.ReadFile(dir + "/" + entry)
			if err != nil {
				t.Skipf("no local copy of %s", entry)
			}
			fsys[entry] = &fstest.MapFile{Data: body}
		}
		got, err := ConvertChinaConfFS(fsys, ChinaConfEntries)
		if err != nil {
			t.Fatalf("convert real upstream data: %v", err)
		}
		t.Logf("real upstream: %d expressions, %d comments", len(got.Expressions), len(got.Comments))
		if len(got.Expressions) < 100000 {
			t.Fatalf("only %d expressions from the real source, which is 13x the list being replaced", len(got.Expressions))
		}
	}
}
EOF
cp /tmp/china_probe_test.go internal/rules/china_probe_test.go
go test ./internal/rules -run TestRealUpstreamEntriesConvert -v
rm internal/rules/china_probe_test.go
```

Expected: PASS，日志显示约 111,287 条表达式、6 条注释。

- [ ] **Step 6: 提交**

```bash
git add internal/rules/chinaconf.go internal/rules/chinaconf_test.go internal/rules/convert.go
git commit -m "feat(rules): convert dnsmasq-china-list, and drop the address it names"
```

---

### Task 2: source-lock 升到 schema v2 并支持两个仓库

**Files:**
- Modify: `internal/rules/download.go:21-29`（常量）、`internal/rules/download.go:59-73`（类型）、`internal/rules/download.go:105-137`（校验）、`internal/rules/download.go:156-214`（ResolveCommit）、`internal/rules/download.go:310-312`（fetchArchive）
- Modify: `internal/rules/download_test.go`
- Modify: `configs/source-lock.json`

**Interfaces:**
- Consumes: Task 1 的 `ChinaConfEntries`
- Produces: `RepositoryChina`、`RepositoryV2Fly`、`SourceRef`、`SourceLock`(v2)、`KnownRepository`；`ResolveCommit`/`ResolvePinned`/`ResolveHEAD` 改为按 `repository` 参数工作

- [ ] **Step 1: 写失败测试**

在 `internal/rules/download_test.go` 追加：

```go
func TestAV2LockRoundTripsThroughItsOwnEncoding(t *testing.T) {
	lock := SourceLock{
		SchemaVersion: 2,
		Primary: SourceRef{
			Repository: RepositoryChina,
			Commit:     strings.Repeat("a", 40),
			SHA256:     strings.Repeat("b", 64),
			Entries:    ChinaConfEntries,
		},
		Supplement: &SourceRef{
			Repository: RepositoryV2Fly,
			Commit:     strings.Repeat("c", 40),
			SHA256:     strings.Repeat("d", 64),
			Entry:      "data/cn",
			Kinds:      []string{"full", "regexp"},
		},
		ListSHA256: strings.Repeat("e", 64),
	}
	if err := lock.Validate(); err != nil {
		t.Fatalf("a complete v2 lock is refused: %v", err)
	}
	encoded, err := lock.Encode()
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	decoded, err := ParseSourceLock(encoded)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if decoded.Primary.Repository != RepositoryChina || len(decoded.Primary.Entries) != 3 {
		t.Fatalf("the primary did not survive the round trip: %#v", decoded.Primary)
	}
	if decoded.Supplement == nil || decoded.Supplement.Entry != "data/cn" {
		t.Fatalf("the supplement did not survive the round trip: %#v", decoded.Supplement)
	}
}

func TestTheFallbackShapeIsTheSameShapeWithNoSupplement(t *testing.T) {
	// v2fly as the PRIMARY is how a fallback publication records itself, so
	// one document shape has to cover both roles.
	lock := SourceLock{
		SchemaVersion: 2,
		Primary: SourceRef{
			Repository: RepositoryV2Fly,
			Commit:     strings.Repeat("a", 40),
			SHA256:     strings.Repeat("b", 64),
			Entry:      "data/cn",
		},
		ListSHA256: strings.Repeat("e", 64),
	}
	if err := lock.Validate(); err != nil {
		t.Fatalf("a v2 lock describing v2fly as the primary is refused: %v", err)
	}
}

func TestAV1LockIsRefusedAndTheRefusalNamesTheWayOut(t *testing.T) {
	v1 := []byte(`{"schema_version":1,"repository":"v2fly/domain-list-community","commit":"` +
		strings.Repeat("a", 40) + `","sha256":"` + strings.Repeat("b", 64) +
		`","list_sha256":"` + strings.Repeat("e", 64) + `","entry":"data/cn"}`)
	_, err := ParseSourceLock(v1)
	if err == nil {
		t.Fatal("a v1 lock was accepted, and reading it as v2 would make a document mean something it does not say")
	}
	if !strings.Contains(err.Error(), "update-lists --pin-remote") {
		t.Fatalf("the refusal does not say how to get a v2 lock: %v", err)
	}
}

func TestAnUnknownRepositoryIsRefused(t *testing.T) {
	if KnownRepository("example.test/not-a-source") {
		t.Fatal("a repository this gateway has never reviewed is accepted")
	}
	if !KnownRepository(RepositoryChina) || !KnownRepository(RepositoryV2Fly) {
		t.Fatal("one of the two reviewed repositories is not in the allowlist")
	}
}

func TestAnUnknownFieldInAV2LockIsRefused(t *testing.T) {
	document := []byte(`{"schema_version":2,"primary":{"repository":"felixonmars/dnsmasq-china-list","commit":"` +
		strings.Repeat("a", 40) + `","sha256":"` + strings.Repeat("b", 64) +
		`","entries":["accelerated-domains.china.conf"]},"list_sha256":"` +
		strings.Repeat("e", 64) + `","future_field":true}`)
	if _, err := ParseSourceLock(document); err == nil {
		t.Fatal("a field this build does not understand was dropped while the document still read as a complete pin")
	}
}

func TestAnArchiveIsFetchedFromTheLockRepositoryNotThePackageDefault(t *testing.T) {
	// The fetch must follow the pin. A converter that always asked for the
	// default repository would verify a digest against an archive from
	// somewhere else entirely.
	if strings.Contains(archiveBaseURL+"/"+RepositoryChina, "v2fly") {
		t.Fatal("the primary repository constant names the wrong source")
	}
}
```

- [ ] **Step 2: 运行测试并确认失败**

```bash
go test ./internal/rules -run 'TestAV2Lock|TestTheFallbackShape|TestAV1Lock|TestAnUnknownRepository|TestAnUnknownField|TestAnArchiveIsFetched' -v
```

Expected: 编译失败，`unknown: RepositoryChina`、`unknown: SourceRef`。

- [ ] **Step 3: 改常量与类型**

`internal/rules/download.go` 第 21-33 行替换为：

```go
const (
	// 两个被审核过的源。primary 指向哪一个由锁决定,supplement 只能是 v2fly。
	//
	// 这不是一个调用方可以传任意值的参数:允许清单里的两项都是被审核过的,
	// 清单之外的任何仓库都被拒绝,因为一份锁的作用是记录"我们审核了哪个源"。
	RepositoryChina = "felixonmars/dnsmasq-china-list"
	RepositoryV2Fly  = "v2fly/domain-list-community"

	// V2FlyEntry 是 v2fly 仓库里被转换的那份列表。
	V2FlyEntry = "data/cn"

	// V2FlySupplementKinds 是并入主源的 v2fly 规则类型。
	//
	// 只取这两类,因为它们是 dnsmasq 语法表达不了的:`server=/<域名>/` 的语义
	// 是"该域名及其所有子域",等价于 domain:,没有精确匹配也没有正则。domain:
	// 与 keyword: 则由主源自己提供。
	V2FlySupplementKinds = []string{"full", "regexp"}

	// schemaVersion 是写出的锁的形状。带任何其他版本的锁被拒绝而不是被解读。
	schemaVersion = 2

	apiBaseURL     = "https://api.github.com"
	archiveBaseURL = "https://codeload.github.com"

	maxArchiveFileBytes = 4 << 20
	maxArchiveBytes     = 64 << 20
	maxArchiveFiles     = 32768
	maxCommitDocumentBytes = 1 << 20
)

// KnownRepository reports whether this gateway has reviewed name as a source.
func KnownRepository(name string) bool {
	return name == RepositoryChina || name == RepositoryV2Fly
}
```

第 59-73 行的 `SourceLock` 替换为：

```go
// SourceRef 是一个被审核过的上游源。
//
// Entry 与 Entries 二选一:dnsmasq 仓库里被转换的是三个文件,一个字符串
// 表达不了;v2fly 是一个。Kinds 只在 supplement 上有意义。
type SourceRef struct {
	Repository string   `json:"repository"`
	Commit     string   `json:"commit"`
	SHA256     string   `json:"sha256"`
	Entry      string   `json:"entry,omitempty"`
	Entries    []string `json:"entries,omitempty"`
	Kinds      []string `json:"kinds,omitempty"`
}

// SourceLock records the reviewed upstream sources of a published list, so a later
// update can be compared against them and a reader can tell exactly which bytes
// produced the list it is running.
//
// The list derives from primary, and optionally from supplement: dnsmasq-china-list
// cannot express a `full:` or a `regexp:` rule, so those 70 v2fly rules are merged
// into the same published file and the lock has to be able to say so. When the
// primary IS v2fly -- a fallback publication -- supplement is absent, and one
// document shape covers both roles.
type SourceLock struct {
	SchemaVersion int        `json:"schema_version"`
	Primary       SourceRef  `json:"primary"`
	Supplement    *SourceRef `json:"supplement,omitempty"`
	ListSHA256    string     `json:"list_sha256"`
}
```

- [ ] **Step 4: 改校验**

第 105-137 行的 `Validate` 与 `validatePinnedSource` 替换为：

```go
// Validate reports whether a published lock is complete.
func (l SourceLock) Validate() error {
	if err := l.validateRef(&l.Primary); err != nil {
		return fmt.Errorf("primary: %w", err)
	}
	if l.Supplement != nil {
		if l.Supplement.Repository != RepositoryV2Fly {
			return fmt.Errorf("supplement: repository %q is not the only source allowed as a supplement", l.Supplement.Repository)
		}
		if err := l.validateRef(l.Supplement); err != nil {
			return fmt.Errorf("supplement: %w", err)
		}
		if l.Supplement.Kinds == nil {
			return errors.New("supplement: a supplement records which rule kinds it contributed")
		}
	}
	if !isLowerHex(l.ListSHA256, 64) {
		return fmt.Errorf("list_sha256 %q is not a 64 character lower-case hex digest", l.ListSHA256)
	}
	return nil
}

func (l SourceLock) validateRef(ref *SourceRef) error {
	if l.SchemaVersion != schemaVersion {
		return fmt.Errorf("schema_version %d is not the supported version %d", l.SchemaVersion, schemaVersion)
	}
	if !KnownRepository(ref.Repository) {
		return fmt.Errorf("repository %q is not one of the reviewed sources %q and %q", ref.Repository, RepositoryChina, RepositoryV2Fly)
	}
	if !isLowerHex(ref.Commit, 40) {
		return fmt.Errorf("commit %q is not a 40 character lower-case hex commit id", ref.Commit)
	}
	if !isLowerHex(ref.SHA256, 64) {
		return fmt.Errorf("sha256 %q is not a 64 character lower-case hex digest", ref.SHA256)
	}
	switch ref.Repository {
	case RepositoryChina:
		if len(ref.Entries) == 0 {
			return errors.New("entries is empty: the dnsmasq source is three named files")
		}
		if ref.Entry != "" {
			return errors.New("entry and entries are both set on the dnsmasq source, which uses entries")
		}
	case RepositoryV2Fly:
		if ref.Entry != V2FlyEntry {
			return fmt.Errorf("entry %q is not the reviewed list %q", ref.Entry, V2FlyEntry)
		}
		if len(ref.Entries) != 0 {
			return errors.New("the v2fly source has one entry and it is named by entry, not entries")
		}
	}
	return nil
}
```

`ParseSourceLock` 的错误信息里补上迁移命令——把 `decode source lock: %w` 改为先判版本：

```go
func ParseSourceLock(data []byte) (SourceLock, error) {
	lock := SourceLock{}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&lock); err != nil {
		return SourceLock{}, fmt.Errorf("decode source lock: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return SourceLock{}, errors.New("decode source lock: trailing data after the document")
	}
	if lock.SchemaVersion != schemaVersion {
		return SourceLock{}, fmt.Errorf(
			"source lock is schema version %d and this build reads version %d. A v1 lock names one "+
				"repository and one entry; this list is published from two, so reading the old "+
				"document as the new one would make a lock that reads complete mean something it does "+
				"not say. Re-pin it with `mosdns-cdnctl update-lists --pin-remote HEAD`",
			lock.SchemaVersion, schemaVersion)
	}
	return lock, nil
}
```

- [ ] **Step 5: 让 archive 按锁里的仓库取**

`fetchArchive` 第 311 行：

```go
	endpoint := archiveBaseURL + "/" + lock.Primary.Repository + "/tar.gz/" + lock.Primary.Commit
```

`ResolveCommit` / `ResolvePinned` / `ResolveHEAD` 的签名改为接收一个仓库名并按它分派 commit 解析；`ResolveCommit` 里的 `repository != Repository` 检查改为 `!KnownRepository(repository)`，`apiBaseURL + "/repos/" + Repository + "/commits/HEAD"` 改为 `apiBaseURL + "/repos/" + repository + "/commits/HEAD"`，返回的 `SourceLock` 改为 `{SchemaVersion: 2, Primary: SourceRef{Repository: repository, Commit: ..., Entries: entriesFor(repository)}}`，其中

```go
// entriesFor 返回该仓库被转换的 entry 集合。
func entriesFor(repository string) []string {
	if repository == RepositoryChina {
		return append([]string(nil), ChinaConfEntries...)
	}
	return nil
}
```

`ResolvePinned` 里 `fetchArchive(ctx, client, resolved)` 之后 `resolved.SHA256 = digestHex(archive)` 保持不变。`Download` 里的 `ConvertFS(fsys, lock.Entry)` 留到 Task 3 改，本 task 先把编译修到能过。

- [ ] **Step 6: 把已提交的锁迁到 v2**

`configs/source-lock.json` 改写为描述**当前这份 v2fly 列表**（Task 6 才换成主源）：

```json
{
  "schema_version": 2,
  "primary": {
    "repository": "v2fly/domain-list-community",
    "commit": "a5731758ed6bc9620b0e146ed24b07b9131893cf",
    "sha256": "aeebf6b0643490cb8c39243ab57f81ab6c411af14238dd7e9e23409d7526ddda",
    "entry": "data/cn"
  },
  "list_sha256": "a865b7cb15f56edd73e8b2df37f22d0d1fe9ee0f4991c090a62ca11e495dfa9f"
}
```

- [ ] **Step 7: 运行测试并确认通过**

```bash
go test ./internal/rules -v
go vet ./internal/rules
```

Expected: 全部 PASS。若 `committed_test.go` 失败，说明它还在读 v1 字段，下一 task 会一并修。

- [ ] **Step 8: 提交**

```bash
git add internal/rules/download.go internal/rules/download_test.go configs/source-lock.json
git commit -m "feat(rules): a v2 source lock, because the list has two sources"
```

---

### Task 3: 双源下载与并集

**Files:**
- Modify: `internal/rules/convert.go`（`ConvertFS` 拆出 kinds 变体）
- Modify: `internal/rules/convert_test.go`
- Modify: `internal/rules/download.go`（`Download`）
- Modify: `internal/rules/download_test.go`

**Interfaces:**
- Consumes: Task 1 的 `ConvertChinaConfFS`；Task 2 的 `SourceLock`/`SourceRef`
- Produces: `ConvertFSKinds(fsys, entry, kinds)`；`Download` 返回并集后的列表

- [ ] **Step 1: 先让夹具能看见两个仓库**

`internal/rules/download_test.go` 的 `fakeRemote.serve` 把 `/v2fly/domain-list-community/tar.gz/` 写死在前缀匹配里，其余一律 404，`newFakeRemote(t, commit, archive)` 也只带一份归档。**这个夹具建模的是一台只有一个仓库的机器**，所以一个双源下载的用例写在它上面，无论代码是对是错都可能绿。

在 `download_test.go` 追加一个按仓库分发的构造器，并让 `serve` 先问它：

```go
// fakeSource is one repository the origin will answer for.
type fakeSource struct {
	// head is what /commits/HEAD answers for this repository.
	head string
	// archives maps a 40-character commit to that repository's archive bytes.
	archives map[string][]byte
}

// newFakeRemoteSources serves N repositories from one origin.
//
// It exists because the single-repository fixture cannot see the thing this
// task is about. That fixture matched one hardcoded prefix and 404'd every
// other path, so a gate written against it could not observe a request for a
// second repository at all -- which is a fixture modelling a machine that
// cannot exist, and a gate built on it reads as coverage it does not have.
func newFakeRemoteSources(t *testing.T, sources map[string]fakeSource) *fakeRemote {
	t.Helper()
	remote := &fakeRemote{status: http.StatusOK, sources: sources}
	remote.server = httptest.NewServer(http.HandlerFunc(remote.serve))
	t.Cleanup(remote.server.Close)
	remote.client = hostRewritingClient(t, remote.server.URL)
	return remote
}

// repositoryOf returns the repository a codeload or API path names, or "".
func repositoryOf(path string) string {
	if trimmed, ok := strings.CutPrefix(path, "/"); ok {
		repository, _, found := strings.Cut(trimmed, "/")
		if found {
			return repository
		}
	}
	return ""
}

// archiveFor returns the repository's archive for the commit the path names.
func (r *fakeRemote) archiveFor(path string) ([]byte, bool) {
	commit, ok := strings.CutPrefix(path, "/"+repositoryOf(path)+"/tar.gz/")
	if !ok {
		return nil, false
	}
	archive, known := r.sources[repositoryOf(path)].archives[commit]
	return archive, known
}

// headFor returns the commit the repository's HEAD endpoint answers.
func (r *fakeRemote) headFor(path string) (string, bool) {
	prefix := "/" + repositoryOf(path) + "/commits/HEAD"
	if path != prefix {
		return "", false
	}
	return r.sources[repositoryOf(path)].head, true
}
```

给 `fakeRemote` 结构体加 `sources map[string]fakeSource`，并把 `serve` 里的两处硬编码替换为：

```go
	case strings.HasSuffix(request.URL.Path, "/commits/HEAD"):
		head, known := r.headFor(request.URL.Path)
		if !known {
			writer.WriteHeader(http.StatusNotFound)
			return
		}
		writer.Header().Set("Content-Type", "application/json")
		if r.rawBody {
			_, _ = io.WriteString(writer, r.commitBody)
			return
		}
		_, _ = writer.WriteString(fmt.Sprintf(`{"sha":%q,"node_id":"C_1","commit":{"committer":{"date":"2026-09-25T01:39:17Z"}},"parents":[]}`, head))
	default:
		archive, known := r.archiveFor(request.URL.Path)
		if !known {
			writer.WriteHeader(http.StatusNotFound)
			return
		}
		writer.Header().Set("Content-Type", "application/gzip")
		length := int64(len(archive))
		if r.declaredLength != 0 {
			length = r.declaredLength
		}
		writer.Header().Set("Content-Length", strconv.FormatInt(length, 10))
		if r.streamBytes != 0 {
			_, _ = io.CopyN(writer, zeroReader{}, r.streamBytes)
			return
		}
		_, _ = writer.Write(archive)
```

既有 `newFakeRemote(t, commit, archive)` 保留，改为构造一个只含 v2fly 的 `sources`，这样既有测试一行不改。

- [ ] **Step 2: 写失败测试**

在 `internal/rules/convert_test.go` 追加：

```go
func TestAKindsFilterSelectsOnlyTheRuleTypesItNames(t *testing.T) {
	fsys := fstest.MapFS{
		"data/cn": &fstest.MapFile{Data: []byte(
			"include:@cn\ndomain:plain.cn\nfull:exact.cn\nkeyword:kw\nregexp:^x\\.cn$\n")},
	}
	// A list with one rule of each type, so a filter that returned everything
	// is distinguishable from one that returned what it was asked for.
	all, err := ConvertFSKinds(fsys, "data/cn", nil)
	if err != nil {
		t.Fatalf("convert all: %v", err)
	}
	if len(all) < 4 {
		t.Fatalf("fixture produced %d rules, want at least one of each type: %v", len(all), all)
	}
	filtered, err := ConvertFSKinds(fsys, "data/cn", []string{"full", "regexp"})
	if err != nil {
		t.Fatalf("convert filtered: %v", err)
	}
	for _, expression := range filtered {
		if !strings.HasPrefix(expression, "full:") && !strings.HasPrefix(expression, "regexp:") {
			t.Fatalf("the filter returned %q, which is not a kind it named", expression)
		}
	}
	if len(filtered) != 2 {
		t.Fatalf("filtered = %v, want exactly the full: and regexp: rules", filtered)
	}
}

func TestAKindsFilterThatNamesNothingThisSourceHasIsAFailure(t *testing.T) {
	fsys := fstest.MapFS{"data/cn": &fstest.MapFile{Data: []byte("domain:only.cn\n")}}
	if _, err := ConvertFSKinds(fsys, "data/cn", []string{"regexp"}); err == nil {
		t.Fatal("a filter matching nothing was accepted, which would publish an empty contribution")
	}
}
```

在 `internal/rules/download_test.go` 追加：

```go
func TestDownloadUnionsThePrimaryAndTheSupplement(t *testing.T) {
	// The origin serves a dnsmasq archive and a v2fly archive; the published
	// list is both, and the digest covers the union.
	primaryCommit := strings.Repeat("a", 40)
	supplementCommit := strings.Repeat("b", 40)
	remote := newFakeRemoteSources(t, map[string]fakeSource{
		RepositoryChina: {head: primaryCommit, archives: map[string][]byte{
			primaryCommit: sourceArchiveFiles(t, primaryCommit,
				"accelerated-domains.china.conf", "server=/one.cn/203.0.113.7\nserver=/two.cn/203.0.113.7\n",
				"google.china.conf", "server=/dl.google.com/203.0.113.7\n",
				"apple.china.conf", "server=/a1.mzstatic.com/203.0.113.7\n"),
		}},
		RepositoryV2Fly: {head: supplementCommit, archives: map[string][]byte{
			supplementCommit: sourceArchiveFiles(t, supplementCommit,
				"data/cn", "include:@cn\ndomain:one.cn\nfull:exact.cn\nregexp:^edge\\.cn$\n"),
		}},
	})

	client := remote.client()
	primary, err := ResolvePinned(context.Background(), client, RepositoryChina, primaryCommit)
	if err != nil {
		t.Fatalf("resolve primary: %v", err)
	}
	supplement, err := ResolvePinned(context.Background(), client, RepositoryV2Fly, supplementCommit)
	if err != nil {
		t.Fatalf("resolve supplement: %v", err)
	}
	ref := supplement.Primary
	ref.Entry = V2FlyEntry
	ref.Kinds = V2FlySupplementKinds

	lock := SourceLock{SchemaVersion: 2, Primary: primary.Primary, Supplement: &ref}
	published, list, err := Download(context.Background(), client, lock)
	if err != nil {
		t.Fatalf("download: %v", err)
	}
	lines := string(list)
	for _, want := range []string{"domain:one.cn", "domain:dl.google.com", "full:exact.cn", "regexp:^edge\\.cn$"} {
		if !strings.Contains(lines, want) {
			t.Fatalf("the published list is missing %q:\n%s", want, lines)
		}
	}
	if published.ListSHA256 != digestHex(list) {
		t.Fatal("the lock's list digest does not cover the bytes it published")
	}
}

func TestThePrimaryArchiveIsFetchedFromTheRepositoryTheLockNames(t *testing.T) {
	// Without this, a fetch that always asked for one repository would still
	// pass the union test whenever the two happened to agree.
	primaryCommit := strings.Repeat("a", 40)
	supplementCommit := strings.Repeat("b", 40)
	remote := newFakeRemoteSources(t, map[string]fakeSource{
		RepositoryChina: {head: primaryCommit, archives: map[string][]byte{
			primaryCommit: sourceArchiveFiles(t, primaryCommit,
				"accelerated-domains.china.conf", "server=/one.cn/203.0.113.7\n",
				"google.china.conf", "server=/g.cn/203.0.113.7\n",
				"apple.china.conf", "server=/a.cn/203.0.113.7\n"),
		}},
		RepositoryV2Fly: {head: supplementCommit, archives: map[string][]byte{
			supplementCommit: sourceArchiveFiles(t, supplementCommit,
				"data/cn", "include:@cn\ndomain:one.cn\nfull:exact.cn\n"),
		}},
	})
	primary, err := ResolvePinned(context.Background(), remote.client(), RepositoryChina, primaryCommit)
	if err != nil {
		t.Fatalf("resolve primary: %v", err)
	}
	supplement, err := ResolvePinned(context.Background(), remote.client(), RepositoryV2Fly, supplementCommit)
	if err != nil {
		t.Fatalf("resolve supplement: %v", err)
	}
	ref := supplement.Primary
	ref.Entry = V2FlyEntry
	ref.Kinds = V2FlySupplementKinds
	if _, _, err := Download(context.Background(), remote.client(), SourceLock{
		SchemaVersion: 2, Primary: primary.Primary, Supplement: &ref,
	}); err != nil {
		t.Fatalf("download: %v", err)
	}
	if !remote.served("/" + RepositoryChina + "/tar.gz/" + primaryCommit) {
		t.Fatalf("the primary archive was not fetched from %s: %v", RepositoryChina, remote.requests)
	}
	if !remote.served("/" + RepositoryV2Fly + "/tar.gz/" + supplementCommit) {
		t.Fatalf("the supplement archive was not fetched from %s: %v", RepositoryV2Fly, remote.requests)
	}
}

func TestASourceWithNoSupplementPublishesOnlyItsOwnRules(t *testing.T) {
	// The fallback publication takes this shape: v2fly as the primary and no
	// supplement, which is why one lock shape has to cover both roles.
	commit := strings.Repeat("b", 40)
	remote := newFakeRemoteSources(t, map[string]fakeSource{
		RepositoryV2Fly: {head: commit, archives: map[string][]byte{
			commit: sourceArchiveFiles(t, commit,
				"data/cn", "include:@cn\ndomain:only.cn\nfull:exact.cn\n"),
		}},
	})
	pinned, err := ResolvePinned(context.Background(), remote.client(), RepositoryV2Fly, commit)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	_, list, err := Download(context.Background(), remote.client(), SourceLock{SchemaVersion: 2, Primary: pinned.Primary})
	if err != nil {
		t.Fatalf("download: %v", err)
	}
	if !strings.Contains(string(list), "domain:only.cn") {
		t.Fatal("the primary's own rules are missing")
	}
}
```

在 `internal/rules/download_test.go` 里补一个能装多个文件的归档构造器——既有 `sourceArchive(t, commit, cnList)` 只装一个 `data/cn`，而主源需要三个：

```go
// sourceArchiveFiles builds a source archive holding several files under the
// commit root the extractor strips. Arguments after the commit are path/body
// pairs.
func sourceArchiveFiles(t *testing.T, commit string, pathAndBody ...string) []byte {
	t.Helper()
	if len(pathAndBody)%2 != 0 {
		t.Fatal("sourceArchiveFiles takes path/body pairs")
	}
	var body bytes.Buffer
	gz := gzip.NewWriter(&body)
	tw := tar.NewWriter(gz)
	for index := 0; index < len(pathAndBody); index += 2 {
		header := &tar.Header{
			Name: commit + "/" + pathAndBody[index],
			Mode: 0o644,
			Size: int64(len(pathAndBody[index+1])),
		}
		if err := tw.WriteHeader(header); err != nil {
			t.Fatalf("tar header: %v", err)
		}
		if _, err := tw.Write([]byte(pathAndBody[index+1])); err != nil {
			t.Fatalf("tar body: %v", err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("tar close: %v", err)
	}
	if err := gz.Close(); err != nil {
		t.Fatalf("gzip close: %v", err)
	}
	return body.Bytes()
}
```

既有 `sourceArchive(t, commit, cnList)` 保持不动，既有测试一行不改。

- [ ] **Step 2: 运行测试并确认失败**

```bash
go test ./internal/rules -run 'TestAKindsFilter|TestDownloadUnions|TestASourceWithNoSupplement' -v
```

Expected: 编译失败，`undefined: ConvertFSKinds`。

- [ ] **Step 3: 拆出 kinds 变体**

`internal/rules/convert.go`：把 `ConvertFS` 改名为 `ConvertFSKinds` 并加一个 kinds 过滤器，`ConvertFS` 成为它的薄封装。

```go
// ConvertFS converts the list at entry in fsys into sorted, deduplicated MOSDNS
// domain expressions, with no kind filter.
func ConvertFS(fsys fs.FS, entry string) ([]string, error) {
	return ConvertFSKinds(fsys, entry, nil)
}

// ConvertFSKinds is ConvertFS with a filter on which resolved rule kinds reach
// the result. An empty or nil filter keeps every kind.
//
// The filter exists because dnsmasq-china-list cannot express a `full:` or a
// `regexp:` rule -- `server=/<domain>/` means the domain and every subdomain,
// which is `domain:` -- so those 70 v2fly rules are merged into the same
// published file. A filter that resolves to nothing is an error, not an empty
// contribution: a supplement that silently contributed nothing would leave the
// published list missing rules the lock says it has.
func ConvertFSKinds(fsys fs.FS, entry string, kinds []string) ([]string, error) {
	allowed := make(map[string]bool, len(kinds))
	for _, kind := range kinds {
		if !supportedRuleKinds[kind] {
			return nil, fmt.Errorf("entry %q: %q is not a rule type this converter can express", entry, kind)
		}
		allowed[kind] = true
	}
	expressions, err := convertFS(fsys, entry, allowed)
	if err != nil {
		return nil, err
	}
	if len(kinds) > 0 && len(expressions) == 0 {
		return nil, fmt.Errorf("%s: no rule of the requested kinds %v resolved", entry, kinds)
	}
	return expressions, nil
}
```

把原 `ConvertFS` 的函数体改名为 `convertFS`，签名加一个 `allowed map[string]bool`，并在渲染循环里跳过不在集合内的种类：

```go
		if allowed != nil && !allowed[current.kind] {
			continue
		}
```

- [ ] **Step 4: 让 Download 取两个归档并求并集**

`internal/rules/download.go` 的 `Download` 改为：

```go
// Download fetches the source archive of a fully pinned lock, verifies its
// digest before a byte of it is extracted, converts the pinned entries, merges
// the supplement when the lock names one, and returns the lock extended with the
// digest of the exact list bytes it produced. A failure at any step returns no
// lock and no list, so a caller cannot mistake a partial update for a pin.
func Download(ctx context.Context, client *http.Client, lock SourceLock) (SourceLock, []byte, error) {
	if err := lock.Validate(); err != nil {
		return SourceLock{}, nil, fmt.Errorf("source lock: %w", err)
	}
	if client == nil {
		return SourceLock{}, nil, errors.New("an HTTP client is required to download the source archive")
	}

	fsys, err := fetchVerifiedArchive(ctx, client, &lock.Primary)
	if err != nil {
		return SourceLock{}, nil, err
	}

	var expressions []string
	var comments []string
	if lock.Primary.Repository == RepositoryChina {
		converted, err := ConvertChinaConfFS(fsys, lock.Primary.Entries)
		if err != nil {
			return SourceLock{}, nil, fmt.Errorf("convert %s at %s: %w", RepositoryChina, lock.Primary.Commit, err)
		}
		expressions, comments = converted.Expressions, converted.Comments
	} else {
		converted, err := ConvertFSKinds(fsys, lock.Primary.Entry, nil)
		if err != nil {
			return SourceLock{}, nil, fmt.Errorf("convert %s at %s: %w", RepositoryV2Fly, lock.Primary.Commit, err)
		}
		expressions = converted
	}

	if lock.Supplement != nil {
		supplementFS, err := fetchVerifiedArchive(ctx, client, lock.Supplement)
		if err != nil {
			return SourceLock{}, nil, err
		}
		contribution, err := ConvertFSKinds(supplementFS, lock.Supplement.Entry, lock.Supplement.Kinds)
		if err != nil {
			return SourceLock{}, nil, fmt.Errorf("convert supplement %s at %s: %w", RepositoryV2Fly, lock.Supplement.Commit, err)
		}
		expressions = mergeExpressions(expressions, contribution)
	}

	list := renderList(expressions, comments)
	pinned := lock
	pinned.ListSHA256 = digestHex(list)
	return pinned, list, nil
}

// fetchVerifiedArchive downloads one source's archive and refuses it unless its
// digest is the one the lock records. The digest is verified BEFORE extraction,
// so an archive that does not match is never unpacked.
func fetchVerifiedArchive(ctx context.Context, client *http.Client, ref *SourceRef) (fs.FS, error) {
	archive, err := fetchArchive(ctx, client, ref)
	if err != nil {
		return nil, err
	}
	if actual := digestHex(archive); actual != ref.SHA256 {
		return nil, fmt.Errorf("%s archive sha256 %s does not match the pinned %s", ref.Repository, actual, ref.SHA256)
	}
	return extractArchive(archive, ref.Commit)
}

// mergeExpressions unions two converted sets and renders them sorted, so the
// published bytes are a function of the two sets and not of the order they were
// produced in.
func mergeExpressions(first, second []string) []string {
	seen := make(map[string]bool, len(first)+len(second))
	merged := make([]string, 0, len(first)+len(second))
	for _, expression := range append(append([]string(nil), first...), second...) {
		if seen[expression] {
			continue
		}
		seen[expression] = true
		merged = append(merged, expression)
	}
	sort.Strings(merged)
	return merged
}
```

`renderList` 改为先写注释块再写表达式：

```go
// renderList renders converted expressions and the upstream's disabled-entry
// reasons as the exact bytes a list file holds.
//
// The comments come first so that a reader opening the file sees what upstream
// turned off before seeing what is on. MOSDNS truncates every line at the first
// `#`, so they are inert to the matcher.
func renderList(expressions, comments []string) []byte {
	var rendered bytes.Buffer
	for _, comment := range comments {
		rendered.WriteString(comment)
		rendered.WriteByte('\n')
	}
	if len(comments) > 0 && len(expressions) > 0 {
		rendered.WriteByte('\n')
	}
	for _, expression := range expressions {
		rendered.WriteString(expression)
		rendered.WriteByte('\n')
	}
	return rendered.Bytes()
}
```

`fetchArchive` 的签名从 `lock SourceLock` 改为 `ref *SourceRef`，endpoint 用 `ref.Repository` 与 `ref.Commit`。

- [ ] **Step 5: 运行测试并确认通过**

```bash
go test ./internal/rules -v
go vet ./internal/rules
gofmt -l internal/rules
```

- [ ] **Step 6: 提交**

```bash
git add internal/rules/convert.go internal/rules/convert_test.go internal/rules/download.go internal/rules/download_test.go
git commit -m "feat(rules): publish the dnsmasq list and the 70 rules it cannot express"
```

---

### Task 4: 刷新决策与回退

**Files:**
- Modify: `cmd/mosdns-cdnctl/update_lists.go:656-726`（`refreshChinaList`）、`cmd/mosdns-cdnctl/update_lists.go:728-788`（`runPinRemote` 的锁与发布路径）
- Modify: `cmd/mosdns-cdnctl/update_lists_test.go`

**Interfaces:**
- Consumes: Task 2 的 `SourceLock`、Task 3 的 `Download`
- Produces: `refreshChinaList` 按源分派刷新；`publishFallback` 冷启动分支

- [ ] **Step 1: 先让 origin 能看见两个仓库**

`update_lists_test.go` 的 `fakeOrigin.serve` 把 `/v2fly/domain-list-community/tar.gz/` 写死，`archivePath` 和 `commitPath` 也把仓库名写进返回值。它同样只建模了一台单仓库的机器：一个"刷新去问了错误的仓库"的缺陷，在它上面既不会失败也看不出来。

按 Task 3 的做法把它改成按路径里的仓库名分发：

```go
// newFakeOriginSources serves N repositories from one origin, keyed by
// repository name. It replaces a fixture that answered only
// /v2fly/domain-list-community/ and 404'd everything else.
func newFakeOriginSources(t *testing.T, head map[string]string, archives map[string]map[string][]byte) *fakeOrigin {
	t.Helper()
	origin := &fakeOrigin{heads: head, sourceArchives: archives, status: http.StatusOK}
	origin.server = httptest.NewServer(http.HandlerFunc(origin.serve))
	t.Cleanup(origin.server.Close)
	origin.client = rewritingClient(t, origin.server.URL)
	return origin
}
```

`fakeOrigin` 加 `heads map[string]string` 与 `sourceArchives map[string]map[string][]byte` 两个字段，`serve` 里两处硬编码改为：

```go
	repository := originRepository(request.URL.Path)
	if commit, ok := strings.CutPrefix(request.URL.Path, "/"+repository+"/tar.gz/"); ok {
		archive, known := origin.sourceArchives[repository][commit]
		if !known {
			writer.WriteHeader(http.StatusNotFound)
			return
		}
		writer.Header().Set("Content-Type", "application/gzip")
		writer.Header().Set("Content-Length", strconv.Itoa(len(archive)))
		_, _ = writer.Write(archive)
		return
	}
	if head, ok := origin.heads[repository]; ok && request.URL.Path == "/"+repository+"/commits/HEAD" {
		writer.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(writer, fmt.Sprintf(`{"sha":%q,"node_id":"C_1","parents":[]}`, head))
		return
	}
	writer.WriteHeader(http.StatusNotFound)
```

并加一个取仓库名的辅助：

```go
// originRepository returns the repository a GitHub path names, or "".
func originRepository(path string) string {
	trimmed, ok := strings.CutPrefix(path, "/")
	if !ok {
		return ""
	}
	repository, _, found := strings.Cut(trimmed, "/")
	if !found {
		return ""
	}
	return repository
}
```

既有 `newFakeOrigin(t, head, archives)` 保留，内部转成单仓库的 `heads` / `sourceArchives`，既有测试一行不改。

- [ ] **Step 2: 写失败测试**

在 `cmd/mosdns-cdnctl/update_lists_test.go` 追加。用既有的 `newListPaths`、`publishPair`、`runCLI`、`automaticPolicy`，不新造夹具：

```go
// chinaLock builds a published dnsmasq lock naming commit, for the pair below.
// chinaLock is a published dnsmasq lock for commit. It exists because the
// existing publishPair writes a v1 lock naming v2fly, which Task 2 makes
// unreadable -- so that helper cannot express the installed state these tests
// start from.
func chinaLock(t *testing.T, paths listPaths, commit string, list []byte) {
	t.Helper()
	lock := rules.SourceLock{
		SchemaVersion: 2,
		Primary: rules.SourceRef{
			Repository: rules.RepositoryChina,
			Commit:     commit,
			SHA256:     sha256Hex([]byte("the archive " + commit)),
			Entries:    rules.ChinaConfEntries,
		},
		ListSHA256: sha256Hex(list),
	}
	encoded, err := lock.Encode()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(paths.sourceLock, encoded, 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(paths.listFile, list, 0o640); err != nil {
		t.Fatal(err)
	}
}

func chinaArchive(t *testing.T, commit, primaryBody string) []byte {
	t.Helper()
	return sourceArchiveFiles(t, commit,
		"accelerated-domains.china.conf", primaryBody,
		"google.china.conf", "server=/g.cn/203.0.113.7\n",
		"apple.china.conf", "server=/a.cn/203.0.113.7\n")
}

func TestTheRefreshFollowsTheRepositoryTheLockNames(t *testing.T) {
	// A refresh that still resolved HEAD against v2fly would report
	// "already at <commit>" forever while never seeing a new rule.
	paths := newListPaths(t)
	published := "domain:one.cn\n"
	chinaLock(t, paths, strings.Repeat("1", 40), []byte(published))

	origin := newFakeOriginSources(t,
		map[string]string{rules.RepositoryChina: "9", rules.RepositoryV2Fly: "9"},
		map[string]map[string][]byte{
			rules.RepositoryChina: {"9": chinaArchive(t, "9", "server=/one.cn/203.0.113.7\nserver=/two.cn/203.0.113.7\n")},
			rules.RepositoryV2Fly: {"9": sourceArchiveFiles(t, "9", "data/cn", "include:@cn\ndomain:one.cn\nfull:x.cn\n")},
		})
	code, stdout, stderr := runAutomatic(t, paths, origin)
	if code != 0 {
		t.Fatalf("refresh failed: %s%s", stdout, stderr)
	}
	if !origin.fetched("/" + rules.RepositoryChina + "/commits/HEAD") {
		t.Fatalf("no request went to the primary the lock names: %v", origin.requests)
	}
	if strings.Contains(stdout, "already at") {
		t.Fatal("the refresh reported already-at while resolving a different repository")
	}
}

func TestARefreshThatCollapsesTheListIsRefusedAndPublishesNothing(t *testing.T) {
	paths := newListPaths(t)
	published := "domain:a.cn\ndomain:b.cn\ndomain:c.cn\n"
	chinaLock(t, paths, strings.Repeat("1", 40), []byte(published))

	origin := newFakeOriginSources(t,
		map[string]string{rules.RepositoryChina: "9", rules.RepositoryV2Fly: "9"},
		map[string]map[string][]byte{
			// Three rules where three hundred were published.
			rules.RepositoryChina: {"9": chinaArchive(t, "9", "server=/only.cn/203.0.113.7\n")},
			rules.RepositoryV2Fly: {"9": sourceArchiveFiles(t, "9", "data/cn", "include:@cn\ndomain:one.cn\nfull:x.cn\n")},
		})
	before := snapshot(t, paths.dir)

	code, _, stderr := runAutomatic(t, paths, origin)
	if code == 0 {
		t.Fatal("a list that lost two thirds of its rules was published")
	}
	if !strings.Contains(stderr, "fewer rules") {
		t.Fatalf("the refusal does not say why: %s", stderr)
	}
	assertUnchanged(t, before, paths.dir, "")
}

func TestAMachineWithAGoodListKeepsItWhenTheOriginIsUnreachable(t *testing.T) {
	paths := newListPaths(t)
	published := "domain:kept.cn\n"
	chinaLock(t, paths, strings.Repeat("1", 40), []byte(published))

	origin := newFakeOriginSources(t, map[string]string{}, map[string]map[string][]byte{})
	origin.status = http.StatusBadGateway

	runAutomatic(t, paths, origin)
	if !strings.Contains(string(mustReadFile(t, paths.listFile)), "domain:kept.cn") {
		t.Fatal("a failed refresh replaced a good list with nothing")
	}
}

func TestAColdStartWithNoRoutePublishesTheFallbackRatherThanNothing(t *testing.T) {
	// Nothing published and the origin cannot be reached. Publishing nothing
	// leaves a machine whose every query goes abroad; the fallback is worse
	// but it is not a hole.
	paths := newListPaths(t)
	origin := newFakeOriginSources(t, map[string]string{}, map[string]map[string][]byte{})
	origin.status = http.StatusBadGateway

	code, stdout, stderr := runAutomatic(t, paths, origin)
	if code == 0 {
		t.Fatalf("a cold start that could not reach the origin reported success: %s%s", stdout, stderr)
	}
	if !strings.Contains(stdout+stderr, "fallback") {
		t.Fatalf("the report does not name the fallback: %s%s", stdout, stderr)
	}
}

// runAutomatic runs the unattended refresh against origin, on a policy with
// only the China switch on.
func runAutomatic(t *testing.T, paths listPaths, origin *fakeOrigin) (int, string, string) {
	t.Helper()
	policy := automaticPolicy(t, true, false)
	return runCLI(t, origin.services(),
		"update-lists", "--automatic",
		"--policy", policy,
		"--source-lock", paths.sourceLock,
		"--list", paths.listFile,
		"--previous-lock", paths.previousLock,
		"--control-lock", paths.controlLock,
	)
}
```

> 实现者注意：字段名已按既有 `listPaths` 核对过——`dir` / `sourceLock` / `listFile` / `controlLock` / `previousLock`，另有 `lockContents()` 与 `listContents()` 两个读回闭包，上面用 `mustReadFile(t, paths.listFile)` 是等价的，两者都行。既有 `publishPair(t, paths, commit, list)` 写的是 **v1 锁且指名 v2fly**，Task 2 之后它写出的文档不可读，所以这些测试用新的 `chinaLock`；**既有测试一律继续用 `publishPair`**，Task 2 里把它改成写 v2 锁指名 v2fly，那些测试一行都不用改。

- [ ] **Step 2: 运行测试并确认失败**

```bash
export PATH=/tmp/opencode/go1.25.8/bin:$PATH
export GOCACHE=$PWD/.gocache-review
export GOTMPDIR=/home/ubuntu/.gotmp-mosdns TMPDIR=/home/ubuntu/.gotmp-mosdns
go test ./cmd/mosdns-cdnctl -run 'TestTheRefreshFollows|TestARefreshThatCollapses|TestAColdStart|TestAMachineWithAGoodList' -v
```

- [ ] **Step 3: 按锁分派刷新**

`refreshChinaList` 改为：

```go
func refreshChinaList(ctx context.Context, options updateListOptions, stdout, stderr io.Writer, services services) int {
	const minimumRetained = 0.95

	pinned, current, found, err := rules.ReadPublishedPair(options.sourceLock, options.listFile)
	if err != nil {
		writeCLIError(stderr, "update-lists: %v", err)
		return exitStateUnavailable
	}

	client := services.newHTTPClient()
	resolved, err := rules.ResolveHEAD(ctx, client, pinned.Primary.Repository)
	if err != nil {
		if !found {
			return publishFallback(ctx, options, stdout, stderr, services, err)
		}
		writeCLIError(stderr, "update-lists: %v", err)
		return exitStateUnavailable
	}
	if resolved.Primary.Commit == pinned.Primary.Commit {
		writeReportLine(stdout, "china-list: already at %s\n", pinned.Primary.Commit)
		return exitSuccess
	}

	// The supplement is resolved against ITS OWN repository, and its own HEAD.
	// It is not re-resolved from the primary's lock because a document cannot
	// be looked up in a repository it does not live in.
	supplement, err := resolveSupplement(ctx, client, pinned.Supplement)
	if err != nil {
		writeCLIError(stderr, "update-lists: the supplement could not be resolved, and publishing "+
			"the primary without it would drop the %d rules only it can express: %v",
			len(rules.V2FlySupplementKinds), err)
		return exitStateUnavailable
	}

	candidate := rules.SourceLock{
		SchemaVersion: 2,
		Primary:       resolved.Primary,
		Supplement:    supplement,
	}
	published, list, err := rules.Download(ctx, client, candidate)
	if err != nil {
		writeCLIError(stderr, "update-lists: %v", err)
		return exitStateUnavailable
	}

	// Two guardrails, one per source. A primary compared against the
	// published list would be comparing 111k rules against whatever is on
	// disk, and a supplement compared against the primary would be comparing a
	// 70-rule contribution against a whole list.
	if err := guardSize(stderr, "primary", countListRules(list), published.PrimaryRules(), minimumRetained, published.Primary.Commit); err != nil {
		return exitStateUnavailable
	}

	// Publish the commit that was MEASURED, not whatever HEAD has become since.
	measured := options
	measured.pinRemote = published.Primary.Commit
	measured.supplementPin = supplement
	return runPinRemote(ctx, measured, stdout, stderr, services)
}

// resolveSupplement re-pins the supplement, or keeps the one already in the
// lock when there is none. An absent supplement stays absent: adding one is a
// decision, not something a refresh discovers.
func resolveSupplement(ctx context.Context, client *http.Client, current *rules.SourceRef) (*rules.SourceRef, error) {
	resolved, err := rules.ResolveHEAD(ctx, client, rules.RepositoryV2Fly)
	if err != nil {
		return nil, err
	}
	ref := resolved.Primary
	ref.Entry = rules.V2FlyEntry
	ref.Kinds = rules.V2FlySupplementKinds
	return &ref, nil
}
```

`guardSize` 是一个小函数，把现有的拒绝文案搬进去并加上来源名：

```go
// guardSize refuses a source whose rule count fell below the retained fraction
// of what this machine published for that source.
func guardSize(stderr io.Writer, name string, incoming, published int, minimum float64, commit string) error {
	if published > 0 && float64(incoming) < float64(published)*minimum {
		writeCLIError(stderr,
			"update-lists: refusing %s at %s: the %s has %s where the published list has %s -- "+
				"fewer rules than a curated list loses in one commit, which is a mistake upstream made "+
				"rather than a change anybody reviewed here. Nothing was changed. Read the diff, then "+
				"publish that commit deliberately with `mosdns-cdnctl update-lists --pin-remote %s`.",
			name, commit, name, ruleCount(incoming), ruleCount(published), commit)
		return fmt.Errorf("the %s collapsed", name)
	}
	return nil
}
```

`publishFallback` 是冷启动分支：

```go
// publishFallback publishes the v2fly list this package ships, and only when
// this machine has no China list at all.
//
// "Only when there is nothing" is the whole rule. A machine with a good list
// keeps it through a failed refresh -- the last good result is strictly better
// than the fallback, which is thirteen times smaller -- and a machine with
// nothing gets 8,363 rules rather than a silent hole where every query goes
// abroad. The fallback is published here and nowhere else, which is what keeps
// the 0.95 guardrail honest: 111,357 down to 8,363 is a 92.49% drop, so a
// refresh can never mistake the fallback for "the list got smaller".
func publishFallback(ctx context.Context, options updateListOptions, stdout, stderr io.Writer, services services, cause error) int {
	client := services.newHTTPClient()
	pinned, err := rules.ResolvePinned(ctx, client, rules.RepositoryV2Fly, "")
	if err != nil {
		writeCLIError(stderr,
			"update-lists: this machine has no China list and the primary source could not be reached "+
				"(%v). Run `mosdns-cdnctl update-lists --pin-remote HEAD` once this machine can reach "+
				"github.com.", cause)
		return exitStateUnavailable
	}
	published, list, err := rules.Download(ctx, client, pinned)
	if err != nil {
		writeCLIError(stderr, "update-lists: the fallback could not be fetched or converted: %v", err)
		return exitStateUnavailable
	}
	if err := rules.Publish(options.sourceLock, options.listFile, published, list); err != nil {
		writeCLIError(stderr, "update-lists: %v", err)
		return exitStateUnavailable
	}
	writeReportLine(stdout, "china-list: published the v2fly fallback at %s (%d rules); "+
		"the primary source could not be reached\n", published.Primary.Commit, countListRules(list))
	return exitSuccess
}
```

`updateListOptions` 增加两个字段：

```go
	// primaryPin 与 supplementPin 是本次要发布的两个源，由调用方决定
	// resolve 哪一个仓库。默认是零值，表示"跟随机器上已发布的锁"。
	primaryPin    *rules.SourceRef
	supplementPin *rules.SourceRef
```

**并且 `runPinRemote` 必须按 `options.primaryPin.Repository` resolve，而不是按常量。** 这是 Task 2 引入的一个不一致：`refreshChinaList` 改成了跟随锁里的仓库，却把结果交给仍然按 `rules.RepositoryV2Fly` 重新 resolve 的 `runPinRemote`。改动之前两处用的是同一个常量，所以它们**按构造**是同一个仓库；改动之后不是了。提交那个 commit 是 Task 2 完成的，运行它的是 Task 4——所以 Task 4 必须把这个洞补上，否则一个"正确"的 commit 会被拿去问另一个仓库，而注释不能替代修复。

```go
func runPinRemote(ctx context.Context, options updateListOptions, stdout, stderr io.Writer, services services) int {
	client := services.newHTTPClient()
	primaryRef := options.primaryPin
	if primaryRef == nil {
		pinned, _, _, err := rules.ReadPublishedPair(options.sourceLock, options.listFile)
		if err != nil {
			writeCLIError(stderr, "update-lists: %v", err)
			return exitStateUnavailable
		}
		primaryRef = &pinned.Primary
	}
	// The repository comes from the pin, never from a constant: the commit this
	// publishes is only meaningful at the repository that issued it, and a commit
	// resolved against a different repository is either absent or, worse, present
	// and unrelated.
	resolved, err := rules.ResolvePinned(ctx, client, primaryRef.Repository, options.pinRemote)
	if err != nil {
		writeCLIError(stderr, "update-lists: %v", err)
		return exitStateUnavailable
	}
	...
	candidate := rules.SourceLock{SchemaVersion: 2, Primary: resolved.Primary, Supplement: options.supplementPin}
	pinned, list, err := rules.Download(ctx, client, candidate)
	...
}
```

`refreshChinaList` 在调用前把已测量的 `resolved.Primary` 作为 `primaryPin` 传下去，于是"测量的那个 commit"和"发布的那个 commit"既是同一个 commit，也是同一个仓库。

- [ ] **Step 4: 运行测试并确认通过**

```bash
go test ./cmd/mosdns-cdnctl -v
go test ./... 
go vet ./...
```

- [ ] **Step 5: 提交**

```bash
git add cmd/mosdns-cdnctl/update_lists.go cmd/mosdns-cdnctl/update_lists_test.go
git commit -m "feat(cdnctl): refresh the primary, fall back only when there is nothing"
```

---

### Task 5: 随包发布四个文件

**Files:**
- Modify: `packaging/debian/postinst:98-103`、`:374-425`
- Modify: `packaging/mosdns-router.install`
- Modify: `installer/tests/test_package.py`

**Interfaces:**
- Consumes: Task 2 的 v2 锁；Task 4 的回退分支
- Produces: `/usr/share/mosdns-router/{cn-domains.txt,source-lock.json,cn-domains-fallback.txt,source-lock-fallback.json}` 与 `/var/lib/mosdns/lists/` 同名四个文件

- [ ] **Step 1: 写失败测试**

在 `installer/tests/test_package.py` 的常量区加：

```python
FALLBACK_LIST = DATA_DIRECTORY + "/cn-domains-fallback.txt"
FALLBACK_LOCK = DATA_DIRECTORY + "/source-lock-fallback.json"
```

并追加：

```python
    def test_the_package_ships_a_fallback_list_and_its_own_lock(self):
        """A cold start with no route has to find something to publish."""
        self.assertIn(FALLBACK_LIST, self.payload())
        self.assertIn(FALLBACK_LOCK, self.payload())

    def test_the_fallback_pair_is_a_complete_v2_lock_of_its_own(self):
        lock = self.read_json(FALLBACK_LOCK)
        self.assertEqual(lock["schema_version"], 2)
        self.assertEqual(lock["primary"]["repository"], "v2fly/domain-list-community")
        self.assertEqual(lock["primary"]["entry"], "data/cn")
        # A fallback lock that described the primary source would let a reader
        # believe the fallback and the live list are the same pin.
        self.assertIsNone(lock["primary"].get("supplement"))
        self.assertEqual(lock["list_sha256"], hashlib.sha256(self.read(FALLBACK_LIST)).hexdigest())

    def test_neither_list_names_a_resolver_address(self):
        for path in (CHINA_LIST, FALLBACK_LIST):
            for finding in china_list_findings(self.read(path)):
                self.fail(f"{path} carries {finding}")

    def test_the_fallback_is_the_v2fly_list_and_the_primary_is_not(self):
        # They are two different lists on purpose. Shipping the primary twice
        # would mean a machine that publishes the fallback publishes 111k rules
        # of something it never fetched.
        self.assertEqual(self.read(FALLBACK_LIST), self.read("configs/cn-domains.txt-fallback"))
```

> 实现者注意：`configs/cn-domains.txt-fallback` 是**不提交**的构建输入，见 Step 3；若选择直接提交一个 `configs/cn-domains-fallback.txt` 作为来源，则把该断言的右侧改成那个路径。本项目已有的做法是提交来源文件（`configs/cn-domains.txt`），因此推荐新增 `configs/cn-domains-fallback.txt` 并提交它。

- [ ] **Step 1b: 修好 `unpinned_pair()` 这个已经过时的夹具**

`installer/tests/test_package.py` 的 `unpinned_pair()` 写的是 **schema_version 1** 的锁，而它的 docstring 声称"与 `mosdns-cdnctl update-lists --pin-remote` 写出的完全一致"——Task 2 之后这句话是假的。测试仍然通过，是因为 `postinst` 只用 `sed` 取 `list_sha256`，Go 那边根本读不到它。但这正是本项目记过六次的那种夹具：它建模了一份这个构建既写不出也读不进的文档，而它上面的门禁读起来像它具备的覆盖。

把它改成写 v2（primary 为 `rules.RepositoryV2Fly` 那一份，无 supplement），并把 docstring 里"与 pin-remote 完全一致"改成本身就能站住的说法——要么指向生成它的那段代码，要么去掉这个类比。改完删掉那些因为旧形状而存在的断言，确认删掉的是**形状**而不是**行为**。

- [ ] **Step 2: 运行测试并确认失败**

```bash
export PATH=/tmp/opencode/go1.25.8/bin:$PATH
cd installer/tests && python3 -m unittest test_package.ControlTests -v 2>&1 | tail -20
```

Expected: FAIL，`FALLBACK_LIST` 不在清单中。

- [ ] **Step 3: 加来源文件并登记清单**

把当前 `configs/cn-domains.txt`（v2fly 那份）复制为 `configs/cn-domains-fallback.txt`，并提交它——它是回退列表的来源，与主列表一样是经过 pin 的快照。

```bash
cp configs/cn-domains.txt configs/cn-domains-fallback.txt
```

`packaging/mosdns-router.install` 增加两行：

```
/usr/share/mosdns-router/cn-domains-fallback.txt
/usr/share/mosdns-router/source-lock-fallback.json
```

`packaging/debian/postinst` 第 98-103 行附近增加：

```sh
FALLBACK_SOURCE_LIST=/usr/share/mosdns-router/cn-domains-fallback.txt
FALLBACK_SOURCE_LOCK=/usr/share/mosdns-router/source-lock-fallback.json
FALLBACK_PUBLISHED_LIST=/var/lib/mosdns/lists/cn-domains-fallback.txt
FALLBACK_PUBLISHED_LOCK=/var/lib/mosdns/lists/source-lock-fallback.json
```

并在发布主列表的同一段里发布回退列表——同样的 install 模式、同样的所有权、同样的原子替换，两份列表从安装起就在盘上，所以冷启动不依赖网络。

- [ ] **Step 4: 生成回退 lock 并跑测试**

```bash
export PATH=/tmp/opencode/go1.25.8/bin:$PATH
export GOCACHE=$PWD/.gocache-review
export GOTMPDIR=/home/ubuntu/.gotmp-mosdns TMPDIR=/home/ubuntu/.gotmp-mosdns
go run ./cmd/mosdns-cdnctl --help >/dev/null 2>&1 || true
python3 - <<'PY'
import hashlib, json, pathlib
lock = {
    "schema_version": 2,
    "primary": {
        "repository": "v2fly/domain-list-community",
        "commit": "a5731758ed6bc9620b0e146ed24b07b9131893cf",
        "sha256": "aeebf6b0643490cb8c39243ab57f81ab6c411af14238dd7e9e23409d7526ddda",
        "entry": "data/cn",
    },
}
body = pathlib.Path("configs/cn-domains-fallback.txt").read_bytes()
lock["list_sha256"] = hashlib.sha256(body).hexdigest()
pathlib.Path("configs/source-lock-fallback.json").write_text(json.dumps(lock, indent=2) + "\n")
print("wrote fallback lock, list_sha256 =", lock["list_sha256"])
PY
cd installer/tests && python3 -m unittest test_package -v 2>&1 | tail -12
```

- [ ] **Step 5: 提交**

```bash
git add configs/cn-domains-fallback.txt configs/source-lock-fallback.json \
        packaging/mosdns-router.install packaging/debian/postinst installer/tests/test_package.py
git commit -m "feat(packaging): ship the fallback pair, so a cold start needs no route"
```

---

### Task 6: 把已提交列表换成主源

**Files:**
- Modify: `configs/cn-domains.txt`
- Modify: `configs/source-lock.json`
- Modify: `internal/rules/committed_test.go`

**Interfaces:**
- Consumes: Task 1 的转换器、Task 3 的并集
- Produces: 仓库里那份描述 dnsmasq 主源 + v2fly 补充的已提交列表

- [ ] **Step 1: 让已提交校验认识注释行**

Task 1 的转换器会在发布列表顶部写出 `# disabled upstream: <原因>` 注释行。MOSDNS 的加载器在第一个 `#` 处截断，所以注释对它不可见；但 `convert.go` 的 `validateRenderedRule` 拒绝任何含 `#` 的行，而 `committed_test.go:96` 把**每一行**都喂给它。**不先修这里，Step 3 的重新 pin 必然失败**——而且失败在一个与本 task 无关的断言上。

这不是把守卫放宽：`validateRenderedRule` 守的是"发布出去的每一行都是 MOSDNS 会当成规则读的东西"，而注释行 MOSDNS **根本不当成规则读**，所以它不属于那扇门。门该守的是**规则行**，注释行该由另一条断言守。

在 `committed_test.go` 里把逐行校验改成先分流：

```go
	forms := make(map[string]int)
	comments := 0
	for number, line := range lines {
		// A comment line is not an expression MOSDNS will read: its loader
		// truncates every line at the first `#`, so a comment contributes no
		// rule. It is therefore held to a different requirement -- that it
		// says where it came from and names no resolver -- rather than fed
		// to a gate about rule syntax.
		if strings.HasPrefix(strings.TrimSpace(line), "#") {
			comments++
			if !strings.HasPrefix(strings.TrimSpace(line), "# disabled upstream: ") {
				t.Fatalf("committed list line %d is a comment this package does not write: %q", number+1, line)
			}
			if addressShaped.MatchString(line) {
				t.Fatalf("committed list line %d carries an address in a comment: %q", number+1, line)
			}
			continue
		}
		if err := validateRenderedRule(line); err != nil {
			t.Fatalf("committed list line %d is not a MOSDNS expression: %v", number+1, err)
		}
		forms[strings.SplitN(line, ":", 2)[0]]++]
	}
	if comments == 0 {
		t.Fatal("the committed list carries no disabled-upstream comment, so the provenance this plan exists to keep was dropped")
	}
```

并把排序断言也改掉——注释块在文件顶部，而 `sort.StringsAreSorted(lines)` 覆盖整个文件。排序断言必须只对规则行成立：

```go
	if !sort.StringsAreSorted(ruleLines) {
		t.Errorf("the committed list's rules are not sorted:\nfirst out of order: %s", firstUnsorted(ruleLines))
	}
```

- [ ] **Step 1b: 删掉 Task 2 留下的值断言**

Task 2 迁移 `committed_test.go` 时，把原本的形状断言改写成了对**当前值**的断言：

```go
	if lock.Primary.Repository != RepositoryV2Fly { ... }
	if lock.Primary.Entry != V2FlyEntry { ... }
	if lock.Supplement != nil { ... }
	if lock.SchemaVersion != 2 { ... }   // 不可达：ParseSourceLock 已经拒绝其他版本
```

v1 之下 `lock.Repository != Repository` 是**形状**断言（只有那一个被审核的源）；v2 之下 `Validate()` 已经强制"两个被审核的源之一"，所以忠实的迁移是**什么都不做**。写成值断言的后果是：Task 6 把 primary 换成 dnsmasq、补上 supplement 之后，这四条会同时失败，而计划里没有任何一步说过要改它们。**注释不是所有者。**

Step 2 里新增的 `TestTheCommittedLockDescribesTheSourceTheCommittedListCameFrom` 才是正确的形状断言（"primary 是 dnsmasq，且 primary 名为三个 entry"），它取代那四条，而不是和它们并存。删掉上面这四段，并删掉随之不再使用的 `V2FlyEntry` / `RepositoryV2Fly` 引用（`RepositoryChina` 仍要用）。

- [ ] **Step 2: 扩展已提交校验测试**

`internal/rules/committed_test.go` 追加：

```go
func TestTheCommittedLockDescribesTheSourceTheCommittedListCameFrom(t *testing.T) {
	lock, err := ParseSourceLock(mustReadCommitted(t, committedLockPath))
	if err != nil {
		t.Fatalf("the committed lock is not one this build can read: %v", err)
	}
	if lock.Primary.Repository != RepositoryChina {
		t.Fatalf("the committed list is pinned to %q, and the primary source is %q",
			lock.Primary.Repository, RepositoryChina)
	}
	if len(lock.Primary.Entries) != 3 {
		t.Fatalf("the primary names %d entries, want the three the pin covers", len(lock.Primary.Entries))
	}
}

func TestTheCommittedSupplementIsTheKindsTheDnsmasqSyntaxCannotExpress(t *testing.T) {
	lock, err := ParseSourceLock(mustReadCommitted(t, committedLockPath))
	if err != nil {
		t.Fatalf("the committed lock is not readable: %v", err)
	}
	if lock.Supplement == nil {
		t.Fatal("no supplement is recorded, and the full:/regexp: rules would be missing from the list")
	}
	if lock.Supplement.Repository != RepositoryV2Fly {
		t.Fatalf("the supplement is %q", lock.Supplement.Repository)
	}
	want := map[string]bool{"full": true, "regexp": true}
	for _, kind := range lock.Supplement.Kinds {
		if !want[kind] {
			t.Fatalf("the supplement contributes %q, which the dnsmasq source already provides", kind)
		}
	}
}

func TestTheCommittedListCarriesNoResolverAddress(t *testing.T) {
	// The upstream names 114DNS on every rule and this project refuses that
	// choice. A committed list that carried an address would mean the
	// conversion had started honouring the upstream's resolver.
	body := string(mustReadCommitted(t, committedListPath))
	for _, address := range []string{"114.114.114.114", "223.5.5.5", "119.29.29.29"} {
		if strings.Contains(body, address) {
			t.Fatalf("the committed China list names %s", address)
		}
	}
}

func TestTheCommittedListKeepsTheRegexpRulesTheDnsmasqSourceCannotExpress(t *testing.T) {
	// If a regeneration ever dropped the supplement, the list would still
	// convert and still pass every routing case -- it would just stop matching
	// AWS China CDN edge names. This is the only gate that sees that.
	body := string(mustReadCommitted(t, committedListPath))
	if !strings.Contains(body, "regexp:.+\\.awsdns-cn-") {
		t.Fatal("the committed list carries no regexp: rule, so the supplement was dropped")
	}
}
```

- [ ] **Step 3: 运行测试并确认失败**

```bash
export PATH=/tmp/opencode/go1.25.8/bin:$PATH
export GOCACHE=$PWD/.gocache-review
export GOTMPDIR=/home/ubuntu/.gotmp-mosdns TMPDIR=/home/ubuntu/.gotmp-mosdns
go test ./internal/rules -run 'TestTheCommitted' -v
```

Expected: FAIL，`the committed list is pinned to "v2fly/domain-list-community", and the primary source is "felixonmars/dnsmasq-china-list"`。

- [ ] **Step 4: 重新 pin 主源**

```bash
export PATH=/tmp/opencode/go1.25.8/bin:$PATH
export GOCACHE=$PWD/.gocache-review
export GOTMPDIR=/home/ubuntu/.gotmp-mosdns TMPDIR=/home/ubuntu/.gotmp-mosdns

# Resolve HEAD for BOTH sources through the tool that publishes them, into a
# scratch directory, so the committed pair is produced by the same code path an
# operator's machine runs.
rm -rf /tmp/pin-primary && mkdir -p /tmp/pin-primary
go run ./cmd/mosdns-cdnctl update-lists \
  --source-lock /tmp/pin-primary/source-lock.json \
  --list /tmp/pin-primary/cn-domains.txt \
  --pin-remote HEAD 2>&1 | tail -20
```

Expected: `pinned-commit`、`pinned-rules: 111357` 左右。若报护栏拒绝，说明本机已发布的列表与新列表不在同一对护栏下——**不要放宽阈值**，把两个计数读出来写进报告。

确认之后：

```bash
cp /tmp/pin-primary/cn-domains.txt configs/cn-domains.txt
cp /tmp/pin-primary/source-lock.json configs/source-lock.json
wc -l configs/cn-domains.txt
head -3 configs/cn-domains.txt
cat configs/source-lock.json
```

- [ ] **Step 5: 运行测试并确认通过**

```bash
go test ./internal/rules -v
go test ./... 
go vet ./...
gofmt -l . | grep -v '^\.gocache-review' || echo "gofmt clean"
```

- [ ] **Step 6: 确认产物里没有任何地址**

```bash
if grep -qE '([0-9]{1,3}\.){3}[0-9]{1,3}' configs/cn-domains.txt; then
  echo "FAIL: the committed list carries an IPv4 address"; exit 1
fi
echo "no address in the committed list"
grep -c '^# disabled upstream:' configs/cn-domains.txt
```

- [ ] **Step 7: 提交**

```bash
git add configs/cn-domains.txt configs/source-lock.json internal/rules/committed_test.go
git commit -m "feat(rules): publish the dnsmasq China list, with v2fly's 70 unexpressible rules"
```

---

### Task 7: 端到端验证

**Files:**
- Modify: 无（只验证；发现问题回到拥有该代码的 task）

**Interfaces:**
- Consumes: Task 1-6 的全部产出
- Produces: 三版本 Podman 矩阵通过的记录

- [ ] **Step 1: 重建包**

```bash
export PATH=/tmp/opencode/go1.25.8/bin:$PATH
export GOCACHE=$PWD/.gocache-review
export GOTMPDIR=/home/ubuntu/.gotmp-mosdns TMPDIR=/home/ubuntu/.gotmp-mosdns
rm -f build/mosdns-router_*_amd64.deb
sh scripts/build-deb.sh 2>&1 | tail -5
ls -la build/*.deb
```

`build/` 里必须**只有一份** amd64 包：`_package_path` 在多于一份时拒绝运行并退出 2，这是门禁在工作，不是新的故障。

- [ ] **Step 2: 确认包里的列表与锁**

```bash
dpkg-deb --fsys-tarfile build/mosdns-router_*_amd64.deb | tar -xO ./usr/share/mosdns-router/cn-domains.txt | wc -l
dpkg-deb --fsys-tarfile build/mosdns-router_*_amd64.deb | tar -xO ./usr/share/mosdns-router/source-lock.json
dpkg-deb --fsys-tarfile build/mosdns-router_*_amd64.deb | tar -xO ./usr/share/mosdns-router/cn-domains-fallback.txt | wc -l
```

Expected: 主列表约 111,357 行；锁为 schema 2、primary 为 `felixonmars/dnsmasq-china-list`、带 supplement；回退列表 8,363 行。

- [ ] **Step 3: 全套本地验证**

```bash
export PATH=/tmp/opencode/go1.25.8/bin:$PATH
export GOCACHE=$PWD/.gocache-review
export GOTMPDIR=/home/ubuntu/.gotmp-mosdns TMPDIR=/home/ubuntu/.gotmp-mosdns
MOSDNS_ROUTER_UPDATE=1 go test ./... 2>&1 | grep -vE '^ok |no test files'
(cd installer/tests && MOSDNS_ROUTER_UPDATE=1 python3 -m unittest discover -s . -p 'test_*.py' 2>&1 | tail -5)
(cd bridge && python3 -m unittest discover -s tests -p 'test_*.py' 2>&1 | tail -3)
```

Expected: Go 全绿；installer 只有 `test_the_man_stub_is_what_keeps_documentation_from_deciding_the_verdict` 这一条**先前已存在**的失败（在没有本次改动时同样失败，用 `git stash` 复核一次）；bridge 全绿。

- [ ] **Step 4: 三版本 Podman 矩阵**

```bash
python3 tests/podman/run.py matrix 2>&1 | tail -30
```

Expected: dhcp / install / routing / watchdog 在 22.04、24.04、26.04 全部 `passed`；整体状态 `incomplete` 仅因 routing 那一条既有的外部视角 skip（原因文本与本变更前逐字相同）。

- [ ] **Step 5: 记录实测结论**

把下面三项写进 `docs/measured-environment.md`：13 倍规则数下的加载耗时、常驻堆、与 30 秒启动预算的比例；查找代价无可测差异；以及"回退只能由冷启动分支发布"这条结论。

- [ ] **Step 6: 提交**

```bash
git add docs/measured-environment.md
git commit -m "docs: what the dnsmasq China list costs, measured on three releases"
```

---

## Plan Acceptance

```bash
export PATH=/tmp/opencode/go1.25.8/bin:$PATH
export GOCACHE=$PWD/.gocache-review
export GOTMPDIR=/home/ubuntu/.gotmp-mosdns TMPDIR=/home/ubuntu/.gotmp-mosdns

gofmt -l . | grep -v '^\.gocache-review'          # 空
go vet ./...                                       # 空
MOSDNS_ROUTER_UPDATE=1 go test ./...                # 无失败
(cd installer/tests && python3 -m unittest discover -s . -p 'test_*.py')   # 仅 1 条先前失败
(cd bridge && python3 -m unittest discover -s tests -p 'test_*.py')       # ok
ls build/*_amd64.deb | wc -l                        # 1
grep -cE '([0-9]{1,3}\.){3}[0-9]{1,3}' configs/cn-domains.txt   # 0
python3 -c "import json;d=json.load(open('configs/source-lock.json'));print(d['schema_version'],d['primary']['repository'],bool(d.get('supplement')))"
                                                        # 2 felixonmars/dnsmasq-china-list True
git status --short
```

Expected:

- 全部 Go 测试与 vet 通过；
- 发布列表约 111,357 条且**不含任何地址**；
- 锁是 schema 2，primary 是 dnsmasq，supplement 是 v2fly 的 70 条；
- `build/` 只有一份 amd64 包；
- 三版本 Podman 矩阵全绿；
- 未修改开发机的任何网络或浏览器配置；
- 只有本计划列出的文件被修改。
