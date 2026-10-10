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
		chinaConfPrimary:    "server=/from-primary.cn/203.0.113.7\n",
		"google.china.conf": "server=/dl.google.com/203.0.113.7\n",
		"apple.china.conf":  "server=/a1.mzstatic.com/203.0.113.7\n",
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

func TestTwoSpellingsCollapseToTheOneRuleMOSDNSWillMatch(t *testing.T) {
	// Case folding is neither this converter's policy nor a statistic's: it is
	// the pinned matcher's. `SubDomainMatcher.Add` normalises every value it
	// loads with `NormalizeDomain` -- ToLower then TrimDot -- so publishing
	// `domain:Example.CN` would publish bytes that differ from the rule that
	// actually lands in the trie. One spelling is therefore one rule, and the
	// spelling that survives is the one the matcher itself produces.
	//
	// The trap this case was written to close is real but lives upstream of the
	// converter: a COUNT taken with a case-folding `sort -fu` is not this
	// converter's count. Step 5 measures the exact string count.
	fsys := chinaConfFS(map[string]string{
		chinaConfPrimary: "server=/Example.CN/203.0.113.7\nserver=/example.cn/203.0.113.7\n",
	})
	got, err := ConvertChinaConfFS(fsys, []string{chinaConfPrimary})
	if err != nil {
		t.Fatalf("convert: %v", err)
	}
	want := []string{"domain:example.cn"}
	if strings.Join(got.Expressions, "\n") != strings.Join(want, "\n") {
		t.Fatalf("expressions =\n%s\nwant\n%s", strings.Join(got.Expressions, "\n"), strings.Join(want, "\n"))
	}
	if !sort.StringsAreSorted(got.Expressions) {
		t.Fatalf("expressions %v are not sorted, and the published list has to be byte-deterministic", got.Expressions)
	}
}

func TestNormalizeChinaDomainFoldsWithThePinnedMatcherAndRefusesWhatItCannotMatch(t *testing.T) {
	// NormalizeChinaDomain is the seam between this converter and the matcher
	// that will read the published list, so what it returns has to be exactly
	// what the matcher would store.
	for name, want := range map[string]string{
		"Example.CN":     "example.cn",
		"example.cn.":    "example.cn",
		"WWW.Example.Cn": "www.example.cn",
		// A name the matcher could never hold is refused, not repaired.
		"exa_mple.cn":    "",
		"-badlabel.cn":   "",
		"badlabel-.cn":   "",
		"double..dot.cn": "",
		"":               "",
	} {
		if got := NormalizeChinaDomain(name); got != want {
			t.Errorf("NormalizeChinaDomain(%q) = %q, want %q", name, got, want)
		}
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
