# China 列表 entry 开关 实现计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 让操作员能按 entry 粒度决定 `google.china.conf` 与 `apple.china.conf` 是否计入中国域名列表，默认两者都计入，且这个选择与"是否自动更新"完全正交。

**Architecture:** 开关是 `policy.yaml` 里 `lists.china` 下的两个可选布尔字段，与 `automatic`（更新节奏）平级但语义不同。`internal/rules` 拥有 entry 文件名与选择逻辑，`internal/config` 拥有开关字段，`cmd/mosdns-cdnctl` 在两条生成列表的路径（定时器刷新、手动 pin）上都读 policy 并把 entry 集合收窄。锁的 `primary.entries` 记录本次实际包含的 entry，因此锁自描述，不需要新字段。

**Tech Stack:** Go 1.25、YAML v3、mosdns v5.3.4、Make、Python 3（installer 测试）

**Spec:** `docs/superpowers/specs/2026-10-16-china-list-entry-switches-design.md`

## Global Constraints

- `accelerated-domains.china.conf` 固定包含，**不做开关**——它占已发布列表 110,481 条中的 110,205 条（99.75%），把它做成开关等于一键关闭几乎全部国内分流。
- `schema_version` **保持 1 不变**。`internal/config/policy.go:131` 硬检查 `SchemaVersion != 1`，升版会让每一台已部署机器现有的 policy 被拒绝。新增字段是可选的，`nil` 的含义（开启）恰好等于既有行为。
- 两个开关字段的类型必须是 `*bool`。Go 的 `bool` 零值是 `false`，而默认值是"开"；用 `bool` 会让"未写该字段"被解析成"关闭"，与默认值相反且无法察觉。
- `configs/policy.yaml` 是 `config.Marshal(config.Defaults())` 的生成产物，`internal/config/marshal_test.go` 逐字节比对。**改 `Defaults()`，重新生成，不得手工编辑。**
- 未知键必须失败（`internal/config/load.go:22` 的 `decoder.KnownFields(true)`）。不得为新字段放宽。
- 防塌缩护栏阈值 `0.95` **不变，不加豁免**。关掉两个可选 entry 最多掉 277 条（0.251%），下限 104,957，关闭后仍有 110,204 条，余量 5,247 条，护栏天然放行。
- policy 读不出来必须**拒绝**（`exitInvalidCLI`），不得猜默认值。猜错的后果是"静静地少装了一类规则"。
- 上游新增文件**不自动纳入** entry 集合。新增可选 entry 必须同时改 `config` schema 与 `rules` 的可选表。
- TDD 强制；每个 task 结束一次聚焦提交。

## Review Focus

以下是 spec 蕴含、但任何 task 的测试都不会自然覆盖的输入。写测试时逐条落到拥有该代码的 task。

- **升级场景**：`/etc/mosdns/policy.yaml` 是 conffile，操作员升级后文件里**没有**这两个字段。`nil` 必须意味着开启，即旧 policy 的行为完全不变。→ Task 1
- **类型错误**：操作员把 `google` 写成 `google: "false"`（带引号的字符串）。YAML 会解析成字符串，`*bool` 解码失败必须报错，而不是当成 false 或 true。→ Task 1
- **`lists.china` 被整个删除**：policy 里没有这一节。两个开关的 `nil` 必须仍然意味着开启。→ Task 1
- **两个开关同时关闭**：`entries` 只剩 1 个元素（只有 base）。这必须是一个有效的集合，不能因为"长度不等于 3"而被拒。→ Task 2
- **锁里 entry 重复**：`entries` 写 `["accelerated-domains.china.conf", "accelerated-domains.china.conf"]`。重复的 entry 会被转换两次。→ Task 2

---

## File Map

```text
internal/config/policy.go            # ChinaListPolicy 加两个 *bool 字段与两个 Enabled() 方法
internal/config/defaults.go          # Defaults() 给出两个指针为 true 的默认值
internal/config/load_test.go         # 新增：省略=开、false=关、类型错误拒绝、缺节=开
internal/config/marshal_test.go      # 既有逐字节比对，新增默认值的断言
configs/policy.yaml                  # 生成产物，由 Marshal(Defaults()) 重新产出
internal/rules/chinaconf.go          # ChinaConfEntries 拆出 base 常量与可选表
internal/rules/download.go           # validateRef 接受 base+可选子集，拒绝重复
internal/rules/chinaconf_test.go     # 新增：选择函数的四种组合
internal/rules/download_test.go      # 新增：子集合法、缺 base 拒绝、未知 entry 拒绝、重复拒绝
internal/rules/committed_test.go     # len(entries)!=3 改为"必须含 base"
cmd/mosdns-cdnctl/update_lists.go    # 两条路径读 policy 并收窄；--check 报 entry 漂移
cmd/mosdns-cdnctl/update_lists_test.go  # 新增：开关生效、policy 不可读拒绝、check 报漂移
```

### 本计划产出的共享接口

```go
// internal/config
type ChinaListPolicy struct {
    Automatic bool  `yaml:"automatic"`
    Google    *bool `yaml:"google"`
    Apple     *bool `yaml:"apple"`
}
func (p ChinaListPolicy) GoogleEnabled() bool
func (p ChinaListPolicy) AppleEnabled() bool

// internal/rules
const ChinaConfBaseEntry = "accelerated-domains.china.conf"

type ChinaConfOptionalEntry struct {
    Field string
    Entry string
}
var ChinaConfOptionalEntries = []ChinaConfOptionalEntry

func ChinaConfEntriesFor(google, apple bool) []string
```

`ChinaConfEntries` 保持为一个 `[]string` 变量（全部被审核的 entry），本计划不改它的名字或类型——它在六个测试文件里被按值和按下标使用。

---

### Task 1: policy 的两个开关字段

**Files:**
- Modify: `internal/config/policy.go`（`ChinaListPolicy` 结构体，约 52 行）
- Modify: `internal/config/defaults.go`（`Defaults()`，约 92 行）
- Modify: `internal/config/load_test.go`
- Modify: `internal/config/marshal_test.go`
- Regenerate: `configs/policy.yaml`

**Interfaces:**
- Consumes: 无
- Produces: `ChinaListPolicy.Google *bool`、`.Apple *bool`、`GoogleEnabled() bool`、`AppleEnabled() bool`

- [ ] **Step 1: 写失败测试**

追加到 `internal/config/load_test.go`：

```go
func TestAnEntrySwitchThisProjectShippedIsOnUntilAnOperatorTurnsItOff(t *testing.T) {
	// 三种写法，三种含义，而前两种必须是同一个含义。
	for _, tc := range []struct {
		name string
		doc  string
		want bool
	}{
		{"omitted entirely", "schema_version: 1\nschedule: \"03:30\"\nlists:\n  china:\n    automatic: false\n", true},
		{"absent because lists.china is gone", "schema_version: 1\nschedule: \"03:30\"\n", true},
		{"written true", "schema_version: 1\nschedule: \"03:30\"\nlists:\n  china:\n    automatic: false\n    google: true\n    apple: true\n", true},
		{"written false", "schema_version: 1\nschedule: \"03:30\"\nlists:\n  china:\n    automatic: false\n    google: false\n    apple: true\n", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := decodePolicy([]byte(tc.doc))
			if err != nil {
				t.Fatalf("a policy without the entry switches must load: %v", err)
			}
			if got.Lists.China.GoogleEnabled() != tc.want {
				t.Fatalf("google = %t, want %t", got.Lists.China.GoogleEnabled(), tc.want)
			}
			if !got.Lists.China.AppleEnabled() {
				t.Fatal("apple was omitted, so it is on")
			}
		})
	}
}

func TestAnEntrySwitchThatIsNotABooleanIsRefused(t *testing.T) {
	// "false" 加引号会解析成字符串。解成 true 或 false 都是错的：一个读不出
	// 布尔值的人以为自己写了 false，而机器以为他写了 true。
	_, err := decodePolicy([]byte("schema_version: 1\nschedule: \"03:30\"\nlists:\n  china:\n    google: \"false\"\n"))
	if err == nil {
		t.Fatal(`a switch written as the string "false" loaded, and a switch that is not a boolean is not a choice`)
	}
}
```

若 `load_test.go` 里没有 `decodePolicy` 这个辅助函数，改用该文件既有的写入并加载方式（先看 `load_test.go:410` 附近，它已经在临时目录里写 `policy.yaml` 再加载）——**沿用它，不要新造一个。**

追加到 `internal/config/marshal_test.go`：

```go
func TestTheShippedPolicySaysWhichEntrySwitchesAreOn(t *testing.T) {
	encoded, err := Marshal(Defaults())
	if err != nil {
		t.Fatalf("Marshal(Defaults()): %v", err)
	}
	// 逐字节比对那个既有测试会抓住位置；这里抓的是**为什么**：一个默认关掉的
	// 开关会安静地改变每次安装的列表，而没有任何测试会失败。
	var decoded struct {
		Lists struct {
			China struct {
				Google *bool `yaml:"google"`
				Apple  *bool `yaml:"apple"`
			} `yaml:"china"`
		} `yaml:"lists"`
	}
	if err := yaml.Unmarshal(encoded, &decoded); err != nil {
		t.Fatalf("the marshalled policy does not parse: %v", err)
	}
	if decoded.Lists.China.Google == nil || !*decoded.Lists.China.Google {
		t.Fatal("the shipped policy does not say google is on, so a fresh install does not get it")
	}
	if decoded.Lists.China.Apple == nil || !*decoded.Lists.China.Apple {
		t.Fatal("the shipped policy does not say apple is on, so a fresh install does not get it")
	}
}
```

- [ ] **Step 2: 运行测试并确认失败**

```bash
source /home/ubuntu/mosdns-env.sh
go test ./internal/config -run 'TestAnEntrySwitch|TestTheShippedPolicySays' -v 2>&1 | tail -15
```

Expected: 编译失败——`ChinaListPolicy` 没有 `Google` / `GoogleEnabled`。

- [ ] **Step 3: 加字段与两个方法**

把 `internal/config/policy.go` 的 `ChinaListPolicy` 换成：

```go
// ChinaListPolicy is unattended refresh of the pinned domain list, plus which of
// the primary source's files are converted at all.
//
// Automatic is TIME — how often to fetch. Google and Apple are CONTENT — which of
// the source's files become domestic names. They are orthogonal: a machine
// refreshing nightly and a machine that has turned google off do exactly the same
// work, and the second one simply ends up with a list that does not contain it.
//
// The two are *bool and not bool because the zero value of a bool is false, and
// the default here is on. A bool would make "the operator did not write this
// field" mean "off" — the opposite of the default, silently, on every machine
// whose policy predates these fields. nil means on, which is also what an
// operator's existing policy must keep meaning after an upgrade.
type ChinaListPolicy struct {
	Automatic bool `yaml:"automatic"`
	Google    *bool `yaml:"google"`
	Apple     *bool `yaml:"apple"`
}

// GoogleEnabled reports whether google.china.conf is converted into the list.
// Absent means on.
func (p ChinaListPolicy) GoogleEnabled() bool { return p.Google == nil || *p.Google }

// AppleEnabled reports whether apple.china.conf is converted into the list.
// Absent means on.
func (p ChinaListPolicy) AppleEnabled() bool { return p.Apple == nil || *p.Apple }
```

- [ ] **Step 4: 默认值全开**

在 `internal/config/defaults.go` 的 `Defaults()` 里，把

```go
China:      ChinaListPolicy{Automatic: false},
```

换成

```go
China:      ChinaListPolicy{Automatic: false, Google: enabledByDefault(), Apple: enabledByDefault()},
```

并在同文件加：

```go
// enabledByDefault is the *bool a switch carries when the project ships it on. A
// pointer rather than a value because the field's meaning is three-state, and a
// value cannot spell the third.
func enabledByDefault() *bool {
	on := true
	return &on
}
```

- [ ] **Step 5: 重新生成 configs/policy.yaml**

这个文件不得手工编辑。写一个一次性程序，产出到 stdout 再落盘：

```bash
source /home/ubuntu/mosdns-env.sh
cat > /tmp/opencode/regen_policy.go <<'EOF'
package main

import (
	"fmt"
	"os"

	"mosdns-router/internal/config"
)

func main() {
	encoded, err := config.Marshal(config.Defaults())
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if _, err := os.Stdout.Write(encoded); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
EOF
mkdir -p cmd/regen-policy-tmp
cp /tmp/opencode/regen_policy.go cmd/regen-policy-tmp/main.go
go run ./cmd/regen-policy-tmp > configs/policy.yaml
rm -rf cmd/regen-policy-tmp
```

然后确认新文件里有这两行，且**顺序稳定**（`Marshal` 按结构体字段顺序输出）：

```bash
grep -n -A3 "^  china:" configs/policy.yaml
```

Expected: 形如
```
  china:
    automatic: false
    apple: true
    google: true
```

（YAML 库的字段排序若与结构体顺序不同，**以 `Marshal` 的实际输出为准**，不要手工调整顺序去迎合本段文字。）

- [ ] **Step 6: 运行测试并确认通过**

```bash
source /home/ubuntu/mosdns-env.sh
gofmt -l internal/config
go vet ./internal/config
go test ./internal/config -count=1 2>&1 | tail -5
```

Expected: 全部通过，既有逐字节比对测试也通过（它现在比对的正是重新生成后的文件）。

- [ ] **Step 7: 确认全仓仍绿**

```bash
source /home/ubuntu/mosdns-env.sh
go build ./... && go vet ./... && go test ./... -count=1 2>&1 | grep -vE "^ok |no test files" | head -5
```

Expected: 无输出（无失败）。

- [ ] **Step 8: 提交**

```bash
git add internal/config/policy.go internal/config/defaults.go internal/config/load_test.go \
        internal/config/marshal_test.go configs/policy.yaml
git commit -m "feat(config): the China list's entry switches, on until turned off"
```

---

### Task 2: entry 集合的选择与锁的校验

**Files:**
- Modify: `internal/rules/chinaconf.go`（`ChinaConfEntries`，约 28-34 行）
- Modify: `internal/rules/download.go`（`validateRef` 约 361-373、`entriesFor` 约 413-418）
- Modify: `internal/rules/chinaconf_test.go`
- Modify: `internal/rules/download_test.go`
- Modify: `internal/rules/committed_test.go`（约 204 行）

**Interfaces:**
- Consumes: 无（Task 1 的 `ChinaListPolicy` 本 task 不用）
- Produces: `rules.ChinaConfBaseEntry`、`rules.ChinaConfOptionalEntries`、`rules.ChinaConfEntriesFor(google, apple bool) []string`；`validateRef` 接受"含 base 的、被审核集合的子集"

- [ ] **Step 1: 写失败测试**

追加到 `internal/rules/chinaconf_test.go`：

```go
func TestTheEntrySetIsTheBasePlusWhicheverOptionalEntriesAreOn(t *testing.T) {
	for _, tc := range []struct {
		name          string
		google, apple bool
		want          []string
	}{
		{"both on", true, true, []string{ChinaConfBaseEntry, "apple.china.conf", "google.china.conf"}},
		{"google off", false, true, []string{ChinaConfBaseEntry, "apple.china.conf"}},
		{"apple off", true, false, []string{ChinaConfBaseEntry, "google.china.conf"}},
		{"both off", false, false, []string{ChinaConfBaseEntry}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := ChinaConfEntriesFor(tc.google, tc.apple)
			if !slices.Equal(got, tc.want) {
				t.Fatalf("ChinaConfEntriesFor(%t, %t) = %v, want %v", tc.google, tc.apple, got, tc.want)
			}
		})
	}
}

func TestNoChoiceEverDropsTheBaseEntry(t *testing.T) {
	// 那个文件是列表的 99.75%。任何组合把它拿掉，都是"关闭几乎全部国内分流"
	// 而不是"选一类域名"。
	for _, google := range []bool{true, false} {
		for _, apple := range []bool{true, false} {
			if !slices.Contains(ChinaConfEntriesFor(google, apple), ChinaConfBaseEntry) {
				t.Fatalf("google=%t apple=%t dropped %q", google, apple, ChinaConfBaseEntry)
			}
		}
	}
}

func TestTheBaseEntryIsNotOneOfTheOptionalEntries(t *testing.T) {
	// 那个文件是列表的 99.75%。把它放进可选表，操作员就能关掉几乎全部国内
	// 分流——而上面每一个测试仍然会通过，因为它们只问集合里有没有它。
	for _, optional := range ChinaConfOptionalEntries {
		if optional.Entry == ChinaConfBaseEntry {
			t.Fatalf("%q is optional, so an operator can turn off 99.75%% of the list", ChinaConfBaseEntry)
		}
	}
}
```

追加到 `internal/rules/download_test.go`：

```go
func TestAnEntrySetIsAnyReviewedSubsetThatKeepsTheBase(t *testing.T) {
	// 一个除 entries 外处处合法的锁，然后只改 entries。Validate() 是公开入口，
	// 既有测试也是走它，所以这里不走包内的 validateRef——那会绕过锁其余部分的
	// 校验，测到的东西比看起来少。
	valid := func(entries []string) SourceLock {
		return SourceLock{
			SchemaVersion: 2,
			Primary: SourceRef{
				Repository: RepositoryChina,
				Commit:     strings.Repeat("a", 40),
				SHA256:     strings.Repeat("b", 64),
				Entries:    entries,
			},
			ListSHA256: strings.Repeat("c", 64),
		}
	}
	if err := valid(ChinaConfEntries).Validate(); err != nil {
		t.Fatalf("the whole reviewed set is not valid: %v", err)
	}
	for _, tc := range []struct {
		name    string
		entries []string
		wantErr bool
	}{
		{"base plus google", []string{ChinaConfBaseEntry, "google.china.conf"}, false},
		{"base alone", []string{ChinaConfBaseEntry}, false},
		{"order does not matter", []string{"google.china.conf", ChinaConfBaseEntry}, false},
		{"without the base", []string{"google.china.conf"}, true},
		{"empty", nil, true},
		{"a fourth upstream file", append(append([]string(nil), ChinaConfEntries...), "bogus-nxdomain.china.conf"), true},
		{"the base named twice", []string{ChinaConfBaseEntry, ChinaConfBaseEntry}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := valid(tc.entries).Validate()
			if tc.wantErr && err == nil {
				t.Fatalf("entries %v were accepted", tc.entries)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("entries %v were refused: %v", tc.entries, err)
			}
		})
	}
}
```

（`ChinaConfEntries` 那一行已经在上面的"先确认合法"里跑过，不需再放进表里。）

- [ ] **Step 2: 运行测试并确认失败**

```bash
source /home/ubuntu/mosdns-env.sh
go test ./internal/rules -run 'TestTheEntrySet|TestNoChoiceEver|TestTheBaseEntry|TestAnEntrySetIsAny' -v 2>&1 | tail -12
```

Expected: 编译失败——`ChinaConfBaseEntry` / `ChinaConfOptionalEntries` / `ChinaConfEntriesFor` 未定义。

- [ ] **Step 3: 拆分常量与可选表**

把 `internal/rules/chinaconf.go` 的 `ChinaConfEntries` 及其注释替换为：

```go
// ChinaConfEntries 是主仓库中全部被转换的文件。这是一个 pin 住的固定集合:
// 上游新增文件不会被自动纳入,少一个则整个转换失败。
var ChinaConfEntries = []string{
	ChinaConfBaseEntry,
	"apple.china.conf",
	"google.china.conf",
}

// ChinaConfBaseEntry 是**永远**被转换的那一个文件,不是一类可选项。
// 它占已发布列表 110,481 条中的 110,205 条——99.75%。把它做成开关等于给
// 操作员一个一键关闭几乎全部国内分流的按钮,而那不是任何人的真实意图。
const ChinaConfBaseEntry = "accelerated-domains.china.conf"

// ChinaConfOptionalEntry 把 policy 里的一个字段映射到它控制的上游文件。
// 新增一项要同时改 internal/config 的字段和这张表:那个摩擦是有意的,它就是
// "上游新增文件不会被自动纳入"这条属性在维护期的样子。
type ChinaConfOptionalEntry struct {
	Field string
	Entry string
}

// ChinaConfOptionalEntries 是可选项,按 policy 字段名索引。
var ChinaConfOptionalEntries = []ChinaConfOptionalEntry{
	{Field: "google", Entry: "google.china.conf"},
	{Field: "apple", Entry: "apple.china.conf"},
}

// ChinaConfEntriesFor 返回操作员的开关选中之后要转换的 entry 集合:恒定的
// base,加上开着的那几个可选项。返回的是新切片,调用方可以持有它。
func ChinaConfEntriesFor(google, apple bool) []string {
	enabled := map[string]bool{"google": google, "apple": apple}
	entries := make([]string, 0, len(ChinaConfEntries))
	entries = append(entries, ChinaConfBaseEntry)
	for _, optional := range ChinaConfOptionalEntries {
		if enabled[optional.Field] {
			entries = append(entries, optional.Entry)
		}
	}
	return entries
}
```

**保留 `ChinaConfEntries` 作为变量**——它在六个测试文件里被按值（`...`）和按下标（`[0]`）使用，改成函数会让本 task 的 diff 大出一截且与本变更无关。

- [ ] **Step 4: 放宽 validateRef**

把 `internal/rules/download.go` 中 `case RepositoryChina:` 的 entries 判定从：

```go
		if !sameEntries(ref.Entries, ChinaConfEntries) {
			return fmt.Errorf("entries %v are not the reviewed set %v", ref.Entries, ChinaConfEntries)
		}
```

换成：

```go
		if !reviewedChinaConfSubset(ref.Entries) {
			return fmt.Errorf("entries %v are not a subset of the reviewed set %v that includes %q",
				ref.Entries, ChinaConfEntries, ChinaConfBaseEntry)
		}
```

并在该函数之后新增：

```go
// reviewedChinaConfSubset reports whether a lock names a set of dnsmasq source
// files this build is willing to convert: every one of them reviewed, the base
// file among them, and none named twice.
//
// A SUBSET rather than the whole reviewed set, because an operator may have
// turned an entry off; the lock then records what was actually converted, which
// is what lets list_sha256 mean anything. Still not "any hint": upstream adds
// files -- `bogus-nxdomain.china.conf` among them -- and a lock naming one this
// build has never converted would be a list no review ever covered, under a lock
// claiming it was reviewed.
//
// The base file is required because it is 99.75% of the list. Its absence is not
// "the operator chose not to include a file", it is "this machine will send
// almost every domestic name abroad", and a lock is not the place to say that
// quietly.
//
// No repeats, because the converter merges each named file and a file named
// twice contributes its rules twice -- a lock that reads as a set but is a bag.
func reviewedChinaConfSubset(named []string) bool {
	if len(named) == 0 || !slices.Contains(named, ChinaConfBaseEntry) {
		return false
	}
	reviewed := make(map[string]bool, len(ChinaConfEntries))
	for _, entry := range ChinaConfEntries {
		reviewed[entry] = true
	}
	seen := make(map[string]bool, len(named))
	for _, entry := range named {
		if !reviewed[entry] || seen[entry] {
			return false
		}
		seen[entry] = true
	}
	return true
}
```

确认 `download.go` 已 import `slices`；没有就加。

`sameEntries` 若因此不再被调用，**删掉它**——留一个死函数比没有更糟。若还有别的调用点则保留。

- [ ] **Step 5: 修 committed_test.go 的断言**

`internal/rules/committed_test.go:204` 的

```go
	if len(lock.Primary.Entries) != 3 {
		t.Fatalf("the primary names %d entries, want the three the pin covers", len(lock.Primary.Entries))
	}
```

换成：

```go
	// 断言"包含 base",不是"恰好三个":默认是三个,但操作员关掉一个开关之后这本
	// 仓库的默认快照依然是三个,而一份真实的锁可以是两个或一个。断言长度会把
	// 这个特性锁死成"永远三个"。
	if !slices.Contains(lock.Primary.Entries, ChinaConfBaseEntry) {
		t.Fatalf("the committed primary names %v, which does not include %q", lock.Primary.Entries, ChinaConfBaseEntry)
	}
```

确认该文件 import 了 `slices`。

- [ ] **Step 6: 运行测试并确认通过**

```bash
source /home/ubuntu/mosdns-env.sh
gofmt -l internal/rules
go vet ./internal/rules
go test ./internal/rules -count=1 2>&1 | tail -20
```

Expected: 全部通过。既有测试里若有断言"必须恰好 3 个"或"第四个 entry 被拒绝"，它们应当仍通过——`bogus-nxdomain.china.conf` 仍在 `ChinaConfEntries` 之外，所以那条性质没被放宽。

- [ ] **Step 7: 确认下游仍绿**

`entriesFor` 的返回值被 `cmd/mosdns-cdnctl` 的多处使用，本 task 没改它，所以下游不应受影响：

```bash
source /home/ubuntu/mosdns-env.sh
go build ./... && go test ./... -count=1 2>&1 | grep -vE "^ok |no test files" | head -5
```

Expected: 无失败输出。

- [ ] **Step 8: 提交**

```bash
git add internal/rules/chinaconf.go internal/rules/download.go \
        internal/rules/chinaconf_test.go internal/rules/download_test.go \
        internal/rules/committed_test.go
git commit -m "feat(rules): an entry set is the base plus whichever optionals are on"
```

---

### Task 3: 两条路径都读 policy

**Files:**
- Modify: `cmd/mosdns-cdnctl/update_lists.go`
- Modify: `cmd/mosdns-cdnctl/update_lists_test.go`

**Interfaces:**
- Consumes: Task 1 的 `ChinaListPolicy.GoogleEnabled()` / `.AppleEnabled()`；Task 2 的 `rules.ChinaConfEntriesFor(google, apple bool) []string`
- Produces: `updateListOptions` 不变；新增包内函数 `chinaEntriesFromPolicy(services, options) ([]string, error)`

- [ ] **Step 1: 写失败测试**

追加到 `cmd/mosdns-cdnctl/update_lists_test.go`。**先读该文件里既有的 `--automatic` 与 `--pin-remote` 用例**（搜 `newListPaths`、`runCLI`、`services.loadPolicy`），沿用它们的夹具与调用方式，不要新造：

```go
func TestAPinPublishesOnlyTheEntriesTheOperatorLeftOn(t *testing.T) {
	paths := newListPaths(t)
	policy := config.Defaults()
	off := false
	policy.Lists.China.Google = &off
	origin := newFakeOriginSources(t, map[string]fakeSource{
		rules.RepositoryChina: {
			head: remoteCommit,
			archives: map[string][]byte{
				remoteCommit: chinaSourceArchive(t, remoteCommit, "domain:kept.cn\n"),
			},
		},
		rules.RepositoryV2Fly: {
			head: remoteCommit,
			archives: map[string][]byte{
				remoteCommit: sourceArchive(t, remoteCommit, firstInstallArchive),
			},
		},
	})
	services := origin.services()
	services.loadPolicy = func(string) (config.Policy, error) { return policy, nil }

	code, stdout, stderr := runCLI(t, services, "--pin-remote", "HEAD",
		"--source-lock", paths.sourceLock, "--list-file", paths.listFile,
		"--control-lock", paths.controlLock, "--previous-lock", paths.previousLock)
	if code != exitSuccess {
		t.Fatalf("--pin-remote exit = %d (stdout: %s, stderr: %s)", code, stdout, stderr)
	}

	lock, list, found, err := rules.ReadPublishedPair(paths.sourceLock, paths.listFile)
	if err != nil || !found {
		t.Fatalf("published pair: %v found=%t", err, found)
	}
	if slices.Contains(lock.Primary.Entries, "google.china.conf") {
		t.Fatalf("google is off but the lock records it: %v", lock.Primary.Entries)
	}
	if !slices.Contains(lock.Primary.Entries, rules.ChinaConfBaseEntry) {
		t.Fatalf("the base entry went missing: %v", lock.Primary.Entries)
	}
	if !strings.Contains(string(list), "kept.cn") {
		t.Fatalf("the list does not carry what the remaining entries contributed: %q", list)
	}
}

func TestAMachineThatCannotReadItsPolicyCannotPinEither(t *testing.T) {
	paths := newListPaths(t)
	origin := newFakeOriginSources(t, map[string]fakeSource{
		rules.RepositoryChina: {head: remoteCommit, archives: map[string][]byte{
			remoteCommit: chinaSourceArchive(t, remoteCommit, "domain:a.cn\n")}},
	})
	services := origin.services()
	services.loadPolicy = func(string) (config.Policy, error) {
		return config.Policy{}, errors.New("policy.yaml: line 11: found a tab")
	}

	code, stdout, stderr := runCLI(t, services, "--pin-remote", "HEAD",
		"--source-lock", paths.sourceLock, "--list-file", paths.listFile,
		"--control-lock", paths.controlLock, "--previous-lock", paths.previousLock)
	if code != exitInvalidCLI {
		t.Fatalf("exit = %d, want %d: a machine that cannot read which entries it wants is not "+
			"one to guess on (stderr: %s)", code, exitInvalidCLI, stderr)
	}
	if !strings.Contains(stderr, "policy") {
		t.Fatalf("the refusal does not name what it could not read: %s", stderr)
	}
}
```

`runCLI` 的签名是 `runCLI(t *testing.T, services services, args ...string) (int, string, string)`，返回顺序是 **code、stdout、stderr**（`update_lists_test.go:587`）。只看 stderr 的用例把第二个返回值写 `_`；要断言报告内容的**必须**接住 stdout——写成 `_` 会让断言拿到空字符串。

- [ ] **Step 2: 运行测试并确认失败**

```bash
source /home/ubuntu/mosdns-env.sh
go test ./cmd/mosdns-cdnctl -run 'TestAPinPublishesOnlyTheEntries|TestAMachineThatCannotReadItsPolicy' -v 2>&1 | tail -14
```

Expected: 第二个测试**失败**（`--pin-remote` 目前完全不读 policy，所以它会成功而不是拒绝）；第一个测试失败于 `lock.Primary.Entries` 含 `google.china.conf`。

- [ ] **Step 3: 加一个共用的取集合函数**

在 `cmd/mosdns-cdnctl/update_lists.go` 里，`runAutomaticLists` 之前或附近新增：

```go
// chinaEntriesFromPolicy reads the entry set this machine wants, from the policy
// the same command already reads for its schedule.
//
// A switch is CONTENT -- which of the primary source's files become domestic
// names -- and `automatic` is TIME. Reading both from one file is what keeps them
// orthogonal: a machine refreshing nightly and a machine that has turned google
// off do the same work, and the second simply publishes a list without it.
//
// A policy that cannot be read is a refusal rather than a default. Guessing here
// publishes a list that is quietly missing a category of domestic names, and
// nothing about that is visible to the operator afterwards.
func chinaEntriesFromPolicy(services services, options updateListOptions) ([]string, error) {
	policy, err := services.loadPolicy(options.policy)
	if err != nil {
		return nil, fmt.Errorf("update-lists: the policy could not be read, so which entries to "+
			"convert is unknown, and guessing would publish a list that is quietly missing a "+
			"category of domestic names: %w", err)
	}
	return rules.ChinaConfEntriesFor(policy.Lists.China.GoogleEnabled(), policy.Lists.China.AppleEnabled()), nil
}
```

- [ ] **Step 4: `--automatic` 用上它**

`runAutomaticLists` 目前在函数开头直接 `config.Load(options.policy)` 并在出错时返回 `exitInvalidCLI`。改为先取 entry 集合，再决定刷新：

```go
	// 取一次，两处用：policy 既有更新节奏也有 entry 开关。
	policy, err := services.loadPolicy(options.policy)
	if err != nil {
		writeCLIError(stderr, "update-lists --automatic: %v", err)
		return exitInvalidCLI
	}
	chinaEntries := rules.ChinaConfEntriesFor(policy.Lists.China.GoogleEnabled(), policy.Lists.China.AppleEnabled())
```

然后把 `chinaEntries` 传进 `refreshChinaList`（在 `options` 上加一个字段，或作为参数）。**推荐加字段**：

```go
	// chinaEntries is the entry set this machine converts, read from the policy.
	// It is a field rather than an argument because three functions below need it
	// and a fourth decides whether to read the policy at all.
	chinaEntries []string
```

- [ ] **Step 5: 收窄解析结果**

`refreshChinaList` 里，`rules.ResolveHEAD(ctx, client, destination)` 之后立刻：

```go
	// The archive is the same whatever entries are converted; what the operator's
	// switches change is which files of it become rules. Narrowing here -- after
	// the digest was computed over the whole archive -- is what keeps the lock's
	// sha256 an archive digest and its entries an honest record of the conversion.
	resolved.Primary.Entries = chinaEntriesFrom(resolved.Primary.Entries, chinaEntries)
```

`runPinRemote` 里，两条分支（measured 快路径 `resolved = rules.SourceLock{...Primary: *options.primaryPin}` 与 `ResolvePinned` 之后）之后同样一行：

```go
	resolved.Primary.Entries = chinaEntriesFrom(resolved.Primary.Entries, options.chinaEntries)
```

`options.chinaEntries` 在 `runPinRemote` 开头用 `chinaEntriesFromPolicy` 取，取不到就 `return exitInvalidCLI` 并写明原因。

新增一个小助手：

```go
// chinaEntriesFrom narrows a resolved entry set to the ones this machine
// converts. Narrowing, never widening: a set the policy does not name is not
// something this machine converts, and a resolution that named one would have to
// be refused rather than trimmed.
func chinaEntriesFrom(resolved, wanted []string) []string {
	keep := make(map[string]bool, len(wanted))
	for _, entry := range wanted {
		keep[entry] = true
	}
	kept := make([]string, 0, len(wanted))
	for _, entry := range resolved {
		if keep[entry] {
			kept = append(kept, entry)
		}
	}
	return kept
}
```

- [ ] **Step 6: 运行测试并确认通过**

```bash
source /home/ubuntu/mosdns-env.sh
gofmt -l cmd/mosdns-cdnctl
go vet ./cmd/mosdns-cdnctl
go test ./cmd/mosdns-cdnctl -run 'TestAPinPublishesOnlyTheEntries|TestAMachineThatCannotReadItsPolicy' -v 2>&1 | tail -8
go test ./cmd/mosdns-cdnctl -count=1 2>&1 | tail -10
```

Expected: 两个新测试通过，整包仍全绿——**既有测试里那些用 `services.loadPolicy` 之外的默认 services 的用例会开始失败**，因为 `--pin-remote` 现在必须能读到 policy。若某个既有测试失败，按同文件里 `check_upstream_test.go` 既有做法给它的 `services` 注入 `loadPolicy`（返回 `config.Defaults()`），**不要**让 `--pin-remote` 在读不到 policy 时回退到默认值——那正是 spec 第 9 节拒绝的猜测。

- [ ] **Step 7: 确认全仓仍绿**

```bash
source /home/ubuntu/mosdns-env.sh
go build ./... && go vet ./... && go test ./... -count=1 2>&1 | grep -vE "^ok |no test files" | head -8
```

- [ ] **Step 8: 提交**

```bash
git add cmd/mosdns-cdnctl/update_lists.go cmd/mosdns-cdnctl/update_lists_test.go
git commit -m "feat(cdnctl): both list-producing paths read which entries the operator wants"
```

---

### Task 4: `--check` 报告 entry 集合的漂移

**Files:**
- Modify: `cmd/mosdns-cdnctl/update_lists.go`（`runCheckLists` 约 493-620）
- Modify: `cmd/mosdns-cdnctl/update_lists_test.go`

**Interfaces:**
- Consumes: Task 3 的 `options.chinaEntries` 与 `chinaEntriesFrom`；Task 2 的 `rules.ChinaConfEntriesFor`
- Produces: `--check` 报告多两行 `desired-entries:` 与 `published-entries:`，且 entry 集合不一致时 `up-to-date: false`

- [ ] **Step 1: 写失败测试**

追加到 `cmd/mosdns-cdnctl/update_lists_test.go`：

```go
func TestTheCheckSaysWhenThePublishedListDoesNotMatchTheSwitches(t *testing.T) {
	paths := newListPaths(t)
	origin := newFakeOriginSources(t, map[string]fakeSource{
		rules.RepositoryChina: {head: remoteCommit, archives: map[string][]byte{
			remoteCommit: chinaSourceArchive(t, remoteCommit, "domain:a.cn\n")}},
		rules.RepositoryV2Fly: {head: remoteCommit, archives: map[string][]byte{
			remoteCommit: sourceArchive(t, remoteCommit, firstInstallArchive)}},
	})

	// 先按"全开"发布一份，再把 policy 改成关掉 google，然后问 --check。
	// 如果 --check 只看 commit，它会报 up-to-date: true，而那份列表里其实有
	// 一类域名，操作员刚要求去掉。
	// Task 3 之后 --pin-remote 必须能读到 policy，所以播种这一次也要注入。
	// 照抄 origin.services() 会去读宿主的 /etc/mosdns/policy.yaml，那台机器上
	// 没有可用的 policy，于是这一跑以 exitInvalidCLI 结束，而失败信息讲的是
	// policy，与本测试要证的事毫无关系。
	code, _, stderr := runCLI(t, pinServices(origin), "--pin-remote", "HEAD",
		"--source-lock", paths.sourceLock, "--list-file", paths.listFile,
		"--control-lock", paths.controlLock, "--previous-lock", paths.previousLock)
	if code != exitSuccess {
		t.Fatalf("seeding the pair: exit %d (%s)", code, stderr)
	}

	off := false
	policy := config.Defaults()
	policy.Lists.China.Google = &off
	services := origin.services()
	services.loadPolicy = func(string) (config.Policy, error) { return policy, nil }

	code, stdout, stderr := runCLI(t, services, "--check",
		"--source-lock", paths.sourceLock, "--list-file", paths.listFile)
	if code != exitSuccess {
		t.Fatalf("--check exit = %d (%s)", code, stderr)
	}
	if !strings.Contains(stdout, "up-to-date: false") {
		t.Fatalf("the check reports a machine whose list does not match its own switches as up to date:\n%s", stdout)
	}
	for _, want := range []string{"desired-entries:", "published-entries:"} {
		if !strings.Contains(stdout, want) {
			t.Fatalf("the report does not say %s, so an operator cannot tell which way it differs:\n%s", want, stdout)
		}
	}
}
```

`runCLI` 直接返回 stdout，不需要额外的捕获工具（见 Task 3 Step 1 对签名的说明）。

- [ ] **Step 2: 运行测试并确认失败**

```bash
source /home/ubuntu/mosdns-env.sh
go test ./cmd/mosdns-cdnctl -run TestTheCheckSaysWhenThePublishedList -v 2>&1 | tail -10
```

Expected: FAIL——报告里没有 `up-to-date: false`，也没有那两行。

- [ ] **Step 3: 报告里加上 entry 集合**

在 `runCheckLists` 里，读到 `published` 之后、写报告之前，加：

```go
	// The entry set is the third thing this report can be wrong about. The other
	// two are commits; this one is a set the operator chose, and a report that
	// only judged commits would say `up-to-date: true` about a machine whose
	// list still carries a category it was just told to stop including.
	desired := options.chinaEntries
	writeReportLine(&report, "desired-entries: %s\n", strings.Join(desired, " "))
	writeReportLine(&report, "published-entries: %s\n", strings.Join(published.Primary.Entries, " "))
	entriesDrifted := !sameEntrySet(desired, published.Primary.Entries)
```

把 `up-to-date` 的判定从

```go
	if !primaryDrifted && !supplementDrifted {
```

改成

```go
	if !primaryDrifted && !supplementDrifted && !entriesDrifted {
```

并新增：

```go
// sameEntrySet reports whether two entry sets name the same files, in any order.
// Order carries no meaning -- the converter merges them -- but a DIFFERENT set
// does, and that is what this report is here to say.
func sameEntrySet(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	counts := make(map[string]int, len(a))
	for _, entry := range a {
		counts[entry]++
	}
	for _, entry := range b {
		counts[entry]--
	}
	for _, n := range counts {
		if n != 0 {
			return false
		}
	}
	return true
}
```

`runCheckLists` 需要能读到 `options.chinaEntries`——`--check` 也要读 policy。若 `--check` 入口尚未填这个字段，在此之前加上 `chinaEntriesFromPolicy` 的调用（读不到就拒绝，与 `--pin-remote` 一致）。

- [ ] **Step 4: 报告里说清集合变了**

在 `--automatic` 的刷新报告里，若本次发布的 entry 集合与上次不同，写一行：

```go
	if !sameEntrySet(previousEntries, pinned.Primary.Entries) {
		writeReportLine(stdout, "china-list: the entry set changed %s -> %s; the size guardrail "+
			"compared counts across that change, which is a configuration change and not an "+
			"upstream one\n", strings.Join(previousEntries, " "), strings.Join(pinned.Primary.Entries, " "))
	}
```

**理由**（写进注释）：一个操作员主动的配置变更与一个上游塌缩，结果都是"条数变少"。阈值不变、不加豁免——0.25% 远在 0.95 之上——但报告必须说清这次比较跨越了集合变更，否则读报告的人只能从数字里猜。

- [ ] **Step 5: 运行测试并确认通过**

```bash
source /home/ubuntu/mosdns-env.sh
gofmt -l cmd/mosdns-cdnctl
go vet ./cmd/mosdns-cdnctl
go test ./cmd/mosdns-cdnctl -run TestTheCheckSaysWhenThePublishedList -v 2>&1 | tail -6
go test ./cmd/mosdns-cdnctl -count=1 2>&1 | tail -10
```

- [ ] **Step 6: 确认全仓仍绿**

```bash
source /home/ubuntu/mosdns-env.sh
go build ./... && go vet ./... && go test ./... -count=1 2>&1 | grep -vE "^ok |no test files" | head -8
```

- [ ] **Step 7: 跑打包侧测试**

`configs/policy.yaml` 是随包发布的文档，`installer/tests` 里有逐字节比对的用例：

```bash
source /home/ubuntu/mosdns-env.sh
cd installer/tests && python3 -m unittest test_package -q 2>&1 | tail -4
```

Expected: `Ran 150 tests` / `OK`。若因缺 `build/dnscrypt-proxy-2.1.18.tar.gz` 而报 setup 错误，从
`/home/ubuntu/project_v2/.worktrees/mosdns-router/build/` 复制该文件过来——那是构建输入，不是代码。

- [ ] **Step 7b: 手册页——本计划之前没有任何 task 拥有它**

Task 3 让 `--pin-remote` 第一次依赖 policy：读不出来就拒绝。这**没有**任何 task 拥有对应的
文档，而它正是操作员唯一会读到的地方。手册页的 `update-lists` 小节（`mosdns-cdnctl.1:105` 起）
有 `--automatic`、`--check` 与那两个刷新开关的条目，**但既没有 `--policy` 也没有这两个新开关**
（`--policy` 只出现在 SYNOPSIS `:13` 与 `render` 小节 `:96`）。`installer/tests/test_package.py`
里有逐字比对手册页与 `--help` 输出的用例，所以两处会互相印证。

要加的：

1. `--pin-remote` 条目里说明它需要一份可读的 policy，读不出来会拒绝且不猜默认值——并写明理由
   （猜错的后果是"静静地少装了一类规则"）。
2. `lists.china` 那一段里加 `google` 与 `apple` 两个条目：默认开、关掉它们**不影响更新**、
   `accelerated` 没有开关且是列表的 99.75%。
3. `--check` 条目里说明它现在会报 `desired-entries` 与 `published-entries`。

改完用 `man -l packaging/man/mosdns-cdnctl.1` 渲染确认 roff 有效（已有一个既有的 `\(t` 拼写
警告在 207 行，与本次无关）。

- [ ] **Step 8: 提交**

```bash
git add cmd/mosdns-cdnctl/update_lists.go cmd/mosdns-cdnctl/update_lists_test.go \
        packaging/man/mosdns-cdnctl.1
git commit -m "feat(cdnctl): the check reports an entry set that does not match the switches"
```

---

## Plan Acceptance

```bash
source /home/ubuntu/mosdns-env.sh
gofmt -l cmd internal                        # 空
go vet ./...                                 # 空
go test ./... -count=1                       # 无失败
(cd installer/tests && python3 -m unittest test_package -q)   # OK
(cd bridge && python3 -m unittest discover -s tests -p 'test_*.py')  # OK

# 默认全开，所以已发布快照一个字节都不该变
git diff --stat 89590f6 -- configs/cn-domains.txt configs/source-lock.json   # 空
grep '^PACKAGE_VERSION=' scripts/build-deb.sh                              # PACKAGE_VERSION=0.4.0
grep -c 'google: true' configs/policy.yaml                                  # 1
grep -c 'apple: true' configs/policy.yaml                                   # 1
```

Expected：

- 全部测试通过；
- `configs/cn-domains.txt` 与 `configs/source-lock.json` **无改动**——默认全开，本次变更不需要重新 pin、不需要重新构建包、不需要重新发布；
- `PACKAGE_VERSION` 仍是 `0.4.0`（本变更不碰包版本：`.deb` 里的 policy.yaml 会多出两行，那是 0.4.1 的事，不是 0.4.0 的）。