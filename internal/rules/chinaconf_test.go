package rules

import (
	"slices"
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

func TestAnEntryWhoseEveryRuleIsAlreadySeenIsStillAnEntryWithRules(t *testing.T) {
	// Review finding 1. The old per-entry gate compared a running TOTAL against
	// the count from before the entry, so an entry contributing nothing new read
	// as an empty entry and aborted with "resolved to no rules, and an empty
	// China set sends every query abroad" -- a sentence describing something
	// that was not happening. This is not hypothetical: `qiao-cn.com` is in
	// `accelerated-domains.china.conf:73926` AND `google.china.conf:85`, so the
	// two files already collide on one domain, and the build survives today only
	// because the other 111 google rules happen to be new. If upstream ever
	// collapsed google.china.conf to its already-seen domains, the converter
	// would refuse a source carrying 111,011 rules and send an operator hunting
	// for an empty file. The count that decides must be the rules THIS entry
	// states, not the expressions this entry adds.
	fsys := chinaConfFS(map[string]string{
		chinaConfPrimary:    "server=/only-primary.cn/203.0.113.7\nserver=/shared.cn/203.0.113.7\n",
		"google.china.conf": "server=/shared.cn/203.0.113.7\nserver=/shared.cn/198.51.100.9\n",
		"apple.china.conf":  "server=/only-apple.cn/203.0.113.7\n",
	})
	got, err := ConvertChinaConfFS(fsys, ChinaConfEntries)
	if err != nil {
		t.Fatalf("an entry whose rules were all already seen was refused: %v", err)
	}
	// google.china.conf contributes two rules and zero new expressions. If the
	// gate counted new expressions, this is the entry it would call empty.
	want := []string{"domain:only-apple.cn", "domain:only-primary.cn", "domain:shared.cn"}
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

func TestTheProducedListIsSortedWhateverOrderUpstreamStatesItIn(t *testing.T) {
	// Byte-determinism is a property of the PUBLISHED file, which carries 111k
	// lines, so it is pinned here over a slice that can actually be out of order.
	// (A one-element slice is sorted by definition, so a sortedness assertion on
	// one would assert nothing.)
	fsys := chinaConfFS(map[string]string{
		chinaConfPrimary: strings.Join([]string{
			"server=/zulu.cn/203.0.113.7",
			"server=/alpha.cn/203.0.113.7",
			"server=/mike.cn/203.0.113.7",
			"server=/bravo.cn/203.0.113.7",
		}, "\n"),
	})
	got, err := ConvertChinaConfFS(fsys, []string{chinaConfPrimary})
	if err != nil {
		t.Fatalf("convert: %v", err)
	}
	want := []string{"domain:alpha.cn", "domain:bravo.cn", "domain:mike.cn", "domain:zulu.cn"}
	if strings.Join(got.Expressions, "\n") != strings.Join(want, "\n") {
		t.Fatalf("expressions =\n%s\nwant\n%s", strings.Join(got.Expressions, "\n"), strings.Join(want, "\n"))
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

func TestAReasonThatIsFreeTextIsKeptReadable(t *testing.T) {
	// A reason is prose, not a URL: the real source carries five links (one with
	// a `#fragment`) and one bare `very unstable`. Refusing or mangling either
	// would lose the upstream's own record of why it dropped a domain.
	fsys := chinaConfFS(map[string]string{
		chinaConfPrimary: strings.Join([]string{
			"#server=/a.cn/203.0.113.7 # Disabled: https://example.test/commit/abc#r31928598",
			"#server=/b.cn/203.0.113.7 # Disabled: very unstable",
			"server=/kept.cn/203.0.113.7",
		}, "\n"),
	})
	got, err := ConvertChinaConfFS(fsys, []string{chinaConfPrimary})
	if err != nil {
		t.Fatalf("convert: %v", err)
	}
	want := []string{
		"# disabled upstream: https://example.test/commit/abc#r31928598",
		"# disabled upstream: very unstable",
	}
	if strings.Join(got.Comments, "\n") != strings.Join(want, "\n") {
		t.Fatalf("comments =\n%s\nwant\n%s", strings.Join(got.Comments, "\n"), strings.Join(want, "\n"))
	}
}

func TestAReasonThatNamesAnAddressLosesTheAddressAndKeepsTheRest(t *testing.T) {
	// Review finding 3, comment half. The reason is copied into the PUBLISHED
	// list, so an address inside it is an address in a product.
	fsys := chinaConfFS(map[string]string{
		chinaConfPrimary: "#server=/x.cn/203.0.113.7 # Disabled: point at 114.114.114.114 instead\nserver=/kept.cn/203.0.113.7\n",
	})
	got, err := ConvertChinaConfFS(fsys, []string{chinaConfPrimary})
	if err != nil {
		t.Fatalf("convert: %v", err)
	}
	rendered := strings.Join(got.Comments, "\n")
	if strings.Contains(rendered, "114.114.114.114") {
		t.Fatalf("the comment kept the upstream address: %q", rendered)
	}
	want := "# disabled upstream: point at <address> instead"
	if rendered != want {
		t.Fatalf("comment = %q, want the reason kept with only the address replaced: %q", rendered, want)
	}
}

func TestAReasonNamingAnAddressShapedButInvalidValueLosesItToo(t *testing.T) {
	// The redaction asks "does this read as an address", not "does Go parse it":
	// `010.1.1.1` is refused by netip and is still unmistakably an address to
	// both the gate and a human reading the log.
	fsys := chinaConfFS(map[string]string{
		chinaConfPrimary: "#server=/x.cn/203.0.113.7 # Disabled: see 010.1.1.1 and 999.1.1.1\nserver=/kept.cn/203.0.113.7\n",
	})
	got, err := ConvertChinaConfFS(fsys, []string{chinaConfPrimary})
	if err != nil {
		t.Fatalf("convert: %v", err)
	}
	rendered := strings.Join(got.Comments, "\n")
	for _, leaked := range []string{"010.1.1.1", "999.1.1.1"} {
		if strings.Contains(rendered, leaked) {
			t.Fatalf("an address-shaped value reached the comment: %q", rendered)
		}
	}
}

func TestRedactionCatchesARunOfAddressShapedDigitsHoweverItIsGlued(t *testing.T) {
	// The word boundaries the first version leaned on did not survive contact
	// with prose: `see1.2.3.4`, `v1.2.3.4`, `1.2.3.4here` and `1234.1.1.1` all
	// sailed past it, because the first three have no boundary where the digits
	// begin and the last has a group the pattern refused to span.
	//
	// The result is asserted EXACTLY rather than by "does not contain", because
	// the failure being fixed is a PARTIAL redaction: `1.2.3.4.5` became
	// `<address>.5`, which neither removes the address nor leaves the text
	// intact. A "no leak" assertion passes on that output happily.
	fsys := chinaConfFS(map[string]string{
		chinaConfPrimary: strings.Join([]string{
			"#server=/x.cn/203.0.113.7 # Disabled: see1.2.3.4 v1.2.3.4 1.2.3.4here 1234.1.1.1 1.2.3.4.5",
			"server=/kept.cn/203.0.113.7",
		}, "\n"),
	})
	got, err := ConvertChinaConfFS(fsys, []string{chinaConfPrimary})
	if err != nil {
		t.Fatalf("convert: %v", err)
	}
	want := "# disabled upstream: see<address> v<address> <address>here <address> <address>"
	if got.Comments[0] != want {
		t.Fatalf("comment =\n%s\nwant\n%s", got.Comments[0], want)
	}
}

func TestRedactionLeavesThreeGroupsAndBareNumbersAlone(t *testing.T) {
	// The other half of the boundary: a three-group version and a bare number
	// are not addresses. Redacting them would edit the upstream's prose for
	// nothing, and `1.2.3` in the DOMAIN field is a legal name this converter
	// is required to keep -- a label group is not an octet.
	fsys := chinaConfFS(map[string]string{
		chinaConfPrimary: "#server=/x.cn/203.0.113.7 # Disabled: build 1.2.3 and issue 391\nserver=/kept.cn/203.0.113.7\n",
	})
	got, err := ConvertChinaConfFS(fsys, []string{chinaConfPrimary})
	if err != nil {
		t.Fatalf("convert: %v", err)
	}
	want := "# disabled upstream: build 1.2.3 and issue 391"
	if got.Comments[0] != want {
		t.Fatalf("comment = %q, want a three-group version left intact: %q", got.Comments[0], want)
	}
}

func TestADisabledLineThisConverterDoesNotDefineIsRefused(t *testing.T) {
	// Review finding 2. A `#server=` line that is not a recognised disabled entry
	// is upstream drifting in shape, and an active rule that drifts is a hard
	// failure. It must not fall through to the generic comment skip: that loses
	// the reason silently, so upstream could rewrite its disabled entries one
	// character at a time and the published list would lose them without one
	// failure. Each line below is a shape this converter does NOT define.
	for name, line := range map[string]string{
		"a space after the hash": "# server=/x.cn/203.0.113.7 # Disabled: reason two",
		"a lowercased keyword":   "#server=/x.cn/203.0.113.7 #disabled: reason three",
		"no second hash":         "#server=/x.cn/203.0.113.7 Disabled: reason four",
		"an empty reason":        "#server=/x.cn/203.0.113.7 # Disabled:",
		"a non-numeric address":  "#server=/x.cn/hostname # Disabled: reason six",
	} {
		t.Run(name, func(t *testing.T) {
			fsys := chinaConfFS(map[string]string{
				chinaConfPrimary: "server=/kept.cn/203.0.113.7\n" + line + "\n",
			})
			_, err := ConvertChinaConfFS(fsys, []string{chinaConfPrimary})
			if err == nil {
				t.Fatalf("a disabled line this converter does not define was accepted: %q", line)
			}
		})
	}
}

func TestACommentThatIsNotARemovedRuleIsStillSkipped(t *testing.T) {
	// The sibling of the case above: refusing every `#` line would refuse the
	// pinned source itself, whose google.china.conf opens with two prose headers.
	fsys := chinaConfFS(map[string]string{
		"google.china.conf": "# Google Corp aka ÜberProxy domains\n# Other corp related domain\nserver=/dl.google.com/203.0.113.7\n",
	})
	got, err := ConvertChinaConfFS(fsys, []string{"google.china.conf"})
	if err != nil {
		t.Fatalf("convert: %v", err)
	}
	want := []string{"domain:dl.google.com"}
	if strings.Join(got.Expressions, "\n") != strings.Join(want, "\n") {
		t.Fatalf("expressions = %v, want %v", got.Expressions, want)
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

func TestAnAddressShapedButInvalidAddressFieldIsRefused(t *testing.T) {
	// `[0-9.]+` admits IPv4-SHAPED text that is not an address, so this branch is
	// reachable from the grammar rather than defensive. netip refuses all four:
	// value > 255, too long, leading zero, too short.
	for _, field := range []string{"999.1.1.1", "1.2.3.4.5", "010.1.1.1", "1.2.3"} {
		t.Run(field, func(t *testing.T) {
			fsys := chinaConfFS(map[string]string{
				chinaConfPrimary: "server=/example.cn/" + field + "\n",
			})
			_, err := ConvertChinaConfFS(fsys, []string{chinaConfPrimary})
			if err == nil {
				t.Fatalf("an address field of %q was accepted as a rule", field)
			}
		})
	}
}

func TestADomainFieldThatIsAddressShapedIsRefused(t *testing.T) {
	// `server=/1.2.3.4/...` would publish `domain:1.2.3.4`: an address in the
	// list, reached through the field that is supposed to carry only names.
	// Real upstream has none (measured: 0 of 111,288), but the constraint is
	// that no address reaches a product, not that upstream has been well behaved
	// so far.
	//
	// The predicate is ADDRESS-SHAPED, not PARSEABLE. netip refuses 999.1.1.1,
	// 010.1.1.1 and 1.2.3.4.5, so a parse-based guard lets all three through
	// into the published list -- which is the same argument that made the
	// redaction shape-based, applied consistently here.
	for _, field := range []string{"1.2.3.4", "999.1.1.1", "010.1.1.1", "999.999.999.999", "1.2.3.4.5"} {
		t.Run(field, func(t *testing.T) {
			fsys := chinaConfFS(map[string]string{
				chinaConfPrimary: "server=/" + field + "/203.0.113.7\n",
			})
			if _, err := ConvertChinaConfFS(fsys, []string{chinaConfPrimary}); err == nil {
				t.Fatalf("the domain field %q was published as a rule", field)
			}
		})
	}
}

func TestAThreeGroupDomainFieldIsStillAPublishedRule(t *testing.T) {
	// The other side of that predicate, so it cannot be tightened into refusing
	// every dotted name: three groups is a legal name, not an address.
	fsys := chinaConfFS(map[string]string{
		chinaConfPrimary: "server=/1.2.3/203.0.113.7\n",
	})
	got, err := ConvertChinaConfFS(fsys, []string{chinaConfPrimary})
	if err != nil {
		t.Fatalf("convert: %v", err)
	}
	want := []string{"domain:1.2.3"}
	if strings.Join(got.Expressions, "\n") != strings.Join(want, "\n") {
		t.Fatalf("expressions = %v, want a three-group name published", got.Expressions)
	}
}

func TestAnUnrecognisedLineIsReportedByShapeAndNotByEchoingIt(t *testing.T) {
	// Review finding 3, error half. The old message was
	// `not a rule this converter defines: %q` with the whole line, so the address
	// reached the build log. `chinesePublicDNSAddresses` scans rendered
	// documents, not build output, so nothing catches this. The operator still
	// gets what they need: which entry, which line, and the shape that was
	// expected.
	fsys := chinaConfFS(map[string]string{
		chinaConfPrimary: "server=/a.cn/203.0.113.7\nserver=/b.cn/114.114.114.114 junk\n",
	})
	_, err := ConvertChinaConfFS(fsys, []string{chinaConfPrimary})
	if err == nil {
		t.Fatal("an unrecognised line was accepted")
	}
	if strings.Contains(err.Error(), "114.114.114.114") {
		t.Fatalf("the failure echoed the offending line, address and all: %v", err)
	}
	// The shape is spelled out here rather than read from the constant the
	// message is built from: a constant that can be edited to match a regression
	// would let this case pass on exactly the failure it exists to catch.
	for _, want := range []string{chinaConfPrimary, ":2", "server=/<domain>/<address>"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("the failure does not name %q: %v", want, err)
		}
	}
}

func TestAFailureAboutTheAddressFieldDoesNotEchoIt(t *testing.T) {
	// The sibling leak: the old message was `%q is not an IPv4 address: %w`
	// around a netip error, and netip's own message quotes the value back.
	fsys := chinaConfFS(map[string]string{
		chinaConfPrimary: "server=/example.cn/010.1.1.1\n",
	})
	_, err := ConvertChinaConfFS(fsys, []string{chinaConfPrimary})
	if err == nil {
		t.Fatal("an invalid address field was accepted")
	}
	if strings.Contains(err.Error(), "010.1.1.1") {
		t.Fatalf("the failure echoed the address field: %v", err)
	}
	if !strings.Contains(err.Error(), chinaConfPrimary) {
		t.Fatalf("the failure does not name the entry: %v", err)
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
	//
	// The assertions reach the WORDING on purpose. Asserting only "no rules"
	// certifies nothing: the misleading message this replaces also contains
	// "no rules", so a check that stops there passes on the very text that
	// describes the wrong thing -- which is exactly the half of round 1 that
	// was not delivered. What differs is whose shortfall the message names:
	// this entry's, not the whole set's.
	fsys := chinaConfFS(map[string]string{chinaConfPrimary: "\n\n# nothing but a comment\n"})
	_, err := ConvertChinaConfFS(fsys, []string{chinaConfPrimary})
	if err == nil {
		t.Fatal("an entry with no rules was accepted")
	}
	if !strings.Contains(err.Error(), chinaConfPrimary) {
		t.Fatalf("the failure does not name the entry: %v", err)
	}
	if !strings.Contains(err.Error(), "short of what the pin says") {
		t.Fatalf("the failure does not name this entry's own shortfall: %v", err)
	}
	if strings.Contains(err.Error(), "empty China set") {
		t.Fatalf("the failure blames the whole set rather than this entry: %v", err)
	}
}

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

func TestWhateverTheSwitchesSayIsASetTheValidatorAccepts(t *testing.T) {
	// 两份记录同一个事实：ChinaConfOptionalEntries 决定能被选中什么，
	// ChinaConfEntries（经 reviewedChinaConfSubset）决定什么被审核过。上面每一个
	// 测试都只测两半中的一半，所以这两个列表不同步时它们全部照样通过。
	//
	// 不同步的故障形态是：某个 entry 进了可选表却没进被审核集合，于是
	// ChinaConfEntriesFor(true, true)——两个开关都开，也就是**默认的那台机器**——
	// 产出一个校验器拒绝的集合。那台机器发不出任何列表，而没有任何东西说得出为什么，
	// 因为 policy 里两个开关写的都是默认的"开"。
	for _, google := range []bool{true, false} {
		for _, apple := range []bool{true, false} {
			entries := ChinaConfEntriesFor(google, apple)
			if !reviewedChinaConfSubset(entries) {
				t.Fatalf("google=%t apple=%t produced %v, which the validator refuses; "+
					"ChinaConfOptionalEntries and ChinaConfEntries have drifted apart",
					google, apple, entries)
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
