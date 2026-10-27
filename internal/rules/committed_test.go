package rules

import (
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"

	mosdnsdomain "github.com/IrineSistiana/mosdns/v5/pkg/matcher/domain"
)

// configs/source-lock.json and configs/cn-domains.txt are the output of one
// `mosdns-cdnctl update-lists --pin-remote HEAD` run against the reviewed
// upstream commit the lock records. They are committed so the repository is
// reviewable and so an installation ships a list whose source is known without
// any network access, and the tests below are what hold the committed pair to the
// promise the lock makes. No test here reads the network: it checks the committed
// bytes against the lock, and the lock against the shape this build understands.

const (
	committedLockPath = "../../configs/source-lock.json"
	committedListPath = "../../configs/cn-domains.txt"

	// chinaDomain and foreignDomain are the routing outcome the committed list
	// has to produce: a well-known China domain has to match, and a well-known
	// foreign domain has not to. Both are stated literally so a converter that
	// published something unusable could not satisfy the test with its own
	// output.
	chinaDomain   = "baidu.com"
	foreignDomain = "www.google.com"

	// disabledUpstreamCommentPrefix is the shape the dnsmasq converter writes a
	// disabled upstream entry's reason as (chinaconf.go, ConvertChinaConfFS). It
	// is restated here rather than imported because the converter builds it
	// inline; if the two ever disagree this gate reports a comment line as one
	// this package does not write, which is the answer a reader of that failure
	// needs even though the cause is a changed literal.
	disabledUpstreamCommentPrefix = "# disabled upstream: "
)

func TestCommittedSourceLockIsAFullyPinnedReviewedSource(t *testing.T) {
	// The lock is the record of what was reviewed. A lock that is incomplete, or
	// that names a repository, an entry or a shape this build does not
	// understand, would leave an installation claiming a pin it cannot verify.
	lockBytes, err := os.ReadFile(committedLockPath)
	if err != nil {
		t.Fatalf("cannot read the committed %s: %v", committedLockPath, err)
	}
	lock, err := ParseSourceLock(lockBytes)
	if err != nil {
		t.Fatalf("parse the committed source lock: %v", err)
	}
	if err := lock.Validate(); err != nil {
		t.Fatalf("the committed source lock is not a complete pin: %v", err)
	}
	// Nothing else about WHICH source is asserted here, and that is the migration
	// done properly. Under schema 1 "the lock names exactly one reviewed
	// repository" was a shape, because there was one repository; under schema 2
	// Validate is what enforces it -- the primary is one of the two reviewed
	// names, and a supplement may only be the v2fly one -- so restating the
	// current values here would be a test that fails the moment this repository
	// re-pins, which is an ordinary operation and not a fault.
	//
	// SchemaVersion is not asserted either, and the parse above is why that costs
	// nothing: ParseSourceLock refuses any version but the current one, so a
	// committed lock of another version fails before this point with a message
	// naming the version. An assertion the parse has already made unreachable is
	// coverage that reads as coverage and cannot fail.
	//
	// What the committed pair is pinned to is asserted by shape, next door, in
	// TestTheCommittedLockDescribesTheSourceTheCommittedListCameFrom and
	// TestTheCommittedSupplementIsTheKindsTheDnsmasqSyntaxCannotExpress.
	if lock.Primary.Commit == strings.Repeat("0", 40) {
		t.Error("the committed commit is all zeroes, which is not a reviewed commit")
	}
}

func TestCommittedListIsExactlyWhatTheCommittedLockDescribes(t *testing.T) {
	// The digest in the lock is only worth anything if it is checked against the
	// committed bytes: an edited list beside an untouched lock would otherwise
	// read as a reviewed source.
	lock, err := ParseSourceLock(mustReadCommitted(t, committedLockPath))
	if err != nil {
		t.Fatal(err)
	}
	list := mustReadCommitted(t, committedListPath)
	if actual := digestHex(list); actual != lock.ListSHA256 {
		t.Fatalf("committed list sha256 = %s, the lock records %s", actual, lock.ListSHA256)
	}
}

func TestCommittedListIsSortedDeduplicatedAndReadableByMOSDNS(t *testing.T) {
	// The list is read by MOSDNS v5.3.4, so it is checked there: a duplicate, an
	// out-of-order line, an expression the loader would refuse, or a China domain
	// the loader would not match all fail here rather than at the router's first
	// query.
	list := mustReadCommitted(t, committedListPath)
	if len(list) == 0 {
		t.Fatal("the committed list is empty, so nothing would route to the domestic branch")
	}
	text := strings.TrimSuffix(string(list), "\n")
	lines := strings.Split(text, "\n")
	forms := make(map[string]int)
	ruleLines := make([]string, 0, len(lines))
	comments, blanks := 0, 0
	inRules := false
	for number, line := range lines {
		trimmed := strings.TrimSpace(line)
		// Three shapes share the file, and each is held to what it actually is.
		//
		// A comment line is not an expression MOSDNS will read: its loader
		// truncates every line at the first `#`, so a comment contributes no rule
		// and never was what the rule gate is about. It is therefore held to a
		// different requirement -- that it says where it came from and names no
		// resolver -- rather than fed to a gate about rule syntax.
		//
		// The blank line is renderList's separator between the comment block and
		// the rules, and it is held to that: exactly one, and only once the
		// comment block it divides has been written.
		switch {
		case trimmed == "":
			blanks++
			if blanks > 1 {
				t.Fatalf("committed list line %d is a second blank line, and renderList writes exactly one", number+1)
			}
			if comments == 0 {
				t.Fatalf("committed list line %d is blank and the list carries no disabled-upstream block to divide", number+1)
			}
			inRules = true
			continue
		case strings.HasPrefix(trimmed, "#"):
			comments++
			if inRules {
				t.Fatalf("committed list line %d is a comment below the rules, and every comment this package writes is a block above them", number+1)
			}
			if !strings.HasPrefix(trimmed, disabledUpstreamCommentPrefix) {
				t.Fatalf("committed list line %d is a comment this package does not write: %q", number+1, line)
			}
			if addressShaped.MatchString(line) {
				t.Fatalf("committed list line %d carries an address in a comment: %q", number+1, line)
			}
		default:
			inRules = true
			if err := validateRenderedRule(line); err != nil {
				t.Fatalf("committed list line %d is not a MOSDNS expression: %v", number+1, err)
			}
			forms[strings.SplitN(line, ":", 2)[0]]++
			ruleLines = append(ruleLines, line)
		}
	}
	if comments == 0 {
		t.Fatal("the committed list carries no disabled-upstream comment, so the provenance this plan exists to keep was dropped")
	}
	// The comment block sits at the top of the file, so the document is sorted
	// whole only because renderList puts the two shapes in that order. What this
	// gate is about -- a rule out of order -- is a property of the rules alone,
	// and the rules are what is checked here.
	if !sort.StringsAreSorted(ruleLines) {
		t.Errorf("the committed list's rules are not sorted:\nfirst out of order: %s", firstUnsorted(ruleLines))
	}
	if duplicate := firstDuplicate(ruleLines); duplicate != "" {
		t.Errorf("the committed list repeats %q", duplicate)
	}
	for _, form := range []string{"domain", "full", "regexp"} {
		if forms[form] == 0 {
			t.Errorf("the committed list carries no %s: rule, which the pinned data does", form)
		}
	}

	matcher := mosdnsdomain.NewDomainMixMatcher()
	if err := mosdnsdomain.LoadFromTextReader(matcher, strings.NewReader(text+"\n"), nil); err != nil {
		t.Fatalf("MOSDNS refused the committed list of %d rules: %v", len(lines), err)
	}
	if matcher.Len() == 0 {
		t.Fatal("MOSDNS loaded no rule from the committed list")
	}
	if _, ok := matcher.Match(chinaDomain); !ok {
		t.Errorf("the committed list does not match the China domain %q", chinaDomain)
	}
	if _, ok := matcher.Match(foreignDomain); ok {
		t.Errorf("the committed list matches the foreign domain %q", foreignDomain)
	}
}

func mustReadCommitted(t *testing.T, path string) []byte {
	t.Helper()
	contents, err := os.ReadFile(filepath.Clean(path))
	if err != nil {
		t.Fatalf("cannot read the committed %s: %v", path, err)
	}
	return contents
}

func TestTheCommittedLockDescribesTheSourceTheCommittedListCameFrom(t *testing.T) {
	lock, err := ParseSourceLock(mustReadCommitted(t, committedLockPath))
	if err != nil {
		t.Fatalf("the committed lock is not one this build can read: %v", err)
	}
	if lock.Primary.Repository != RepositoryChina {
		t.Fatalf("the committed list is pinned to %q, and the primary source is %q",
			lock.Primary.Repository, RepositoryChina)
	}
	// 而这份**已提交**快照是相反的情况:默认全开,所以它必须命名全部三个。
	// 这一条没有别的测试覆盖——committed_test.go 下面的合成锁从 ChinaConfEntries
	// 派生,cmd/mosdns-cdnctl 那几处也都从代码派生,而 source-lock.json 自己
	// 只被比对了仓库名与 sha256。它确实含 base,但含 base 不等于全开,而 google 只占
	// 0.10%,不触发 0.95 的防塌缩护栏:所以将来某个 task 重新生成锁与列表、顺手关掉
	// google,整个套件照样全绿——两个文件会互相印证彼此的 list_sha256。
	//
	// 这里没有"含 base 即可"那条 Contains,是一条死断言而不是一条规则:下面那条
	// 多重集相等已经蕴含 base 在内(ChinaConfEntries 恒含它),两者读同一个
	// lock.Primary.Entries,所以它永远不会失败,实际覆盖的是零。那条一般规则归
	// reviewedChinaConfSubset 管(download_test.go 的
	// TestAnEntrySetIsAnyReviewedSubsetThatKeepsTheBase 是它的用例),归的不是这份
	// 已提交快照。
	//
	// 比的是**集合**,不是顺序,两边都排序:这份快照把 google 排在 apple 之前,而
	// ChinaConfEntries 是 apple 在前。顺序进的是锁里序列化的 entries 数组——重排
	// 确实改锁文件自己的字节——而不进任何产物:转换器合并各文件之后排序,所以
	// list_sha256 与归档摘要一字不变。把这条比较钉成顺序敏感,断言的是本仓库从未
	// 做过、也不打算做的事:只重排数组就要重新 pin。
	committed := append([]string(nil), lock.Primary.Entries...)
	slices.Sort(committed)
	reviewed := append([]string(nil), ChinaConfEntries...)
	slices.Sort(reviewed)
	if !slices.Equal(committed, reviewed) {
		t.Fatalf("the committed primary names %v, want the whole reviewed set %v; "+
			"the committed snapshot is generated with every switch on, so a re-pin "+
			"that turned one off is not the snapshot this repository reviewed",
			lock.Primary.Entries, ChinaConfEntries)
	}
}

// A supplement naming no kind is self-contained here: the assertion below is proved to
// fire on one, without borrowing anything from the committed files or from another
// test's gate. Without that, the loop in the committed-lock case reads as coverage of
// this shape while being unable to catch it.
func TestASupplementNamingNoKindIsRefusedWhateverTheLockAroundIt(t *testing.T) {
	lock := SourceLock{
		SchemaVersion: 2,
		Primary: SourceRef{
			Repository: RepositoryChina,
			Commit:     "1111111111111111111111111111111111111111",
			SHA256:     strings.Repeat("a", 64),
			Entries:    append([]string(nil), ChinaConfEntries...),
		},
		Supplement: &SourceRef{
			Repository: RepositoryV2Fly,
			Commit:     "2222222222222222222222222222222222222222",
			SHA256:     strings.Repeat("b", 64),
			Entry:      V2FlyEntry,
			Kinds:      []string{},
		},
		ListSHA256: strings.Repeat("c", 64),
	}
	if err := lock.Validate(); err == nil {
		t.Fatal("a supplement naming no kinds validated; the pair says a second source contributed " +
			"something without recording what, so a later reader cannot count what it gave the list")
	}
	if !strings.Contains(lock.Validate().Error(), "kinds") {
		t.Errorf("the refusal does not name the field that is wrong: %v", lock.Validate())
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
	// The size is asserted as well as the membership, and that is the part the loop
	// above cannot reach: an empty or nil Kinds passes every iteration and reads as
	// coverage of a shape it cannot catch. A supplement naming no kind contributes
	// nothing, so the published list would be missing every rule the dnsmasq syntax
	// cannot express while this test reported the kinds as correct.
	//
	// Validate refuses an empty set -- TestCommittedSourceLockIsAFullyPinnedReviewedSource
	// calls it -- so the gate is closed elsewhere and nothing was silently uncovered by
	// adding this. It was closed in a DIFFERENT test, which is what made this one read as
	// if it were checking the thing and not a subset of it.
	if got := len(lock.Supplement.Kinds); got != len(want) {
		t.Fatalf("the supplement names %d kinds %v, want %d %v", got, lock.Supplement.Kinds, len(want), V2FlySupplementKinds)
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

func firstUnsorted(lines []string) string {
	for i := 1; i < len(lines); i++ {
		if lines[i-1] > lines[i] {
			return lines[i-1] + " then " + lines[i]
		}
	}
	return ""
}

func firstDuplicate(lines []string) string {
	for i := 1; i < len(lines); i++ {
		if lines[i-1] == lines[i] {
			return lines[i]
		}
	}
	return ""
}
