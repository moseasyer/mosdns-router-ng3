package main

import (
	"bytes"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"mosdns-router/internal/rules"
)

// The systemd budget for the daily list work, and the request count it is derived
// from. Both live here rather than in a comment in the unit file because both are
// measurements, and a measurement nobody re-measures is a number somebody guessed:
//
//   - dailyRunRequestMaximum is MEASURED, by the table in
//     TestTheDailyRunCostsWhatItIsMeasuredToCost below, which drives the real
//     command through every path `update-lists --automatic` can take against the
//     fake origin that records each request.
//   - The unit's TimeoutStartSec is HELD to that measurement by
//     TestTheDailyListUnitBudgetsTheMeasuredRequestMaximum, which reads the
//     shipped unit file.
//
// The chain is the point. A code path that starts making a request fails the
// table; a table figure that is raised past dailyRunRequestMaximum fails the
// table too; and a unit file whose TimeoutStartSec no longer covers
// dailyRunRequestMaximum x updateListTimeout fails the gate. Changing the code,
// the measurement and the number in the unit without changing the other two is
// not a sequence any of them allows.
//
// It is here rather than in installer/tests/test_units.py because this is the only
// language that can MEASURE the count: the figure comes from running the command,
// and a Python gate that compared two literals would be the check that cannot
// fail this repository has been correcting for weeks. The Python suite keeps its
// own job -- it holds the directive against the table in EXPECTED_DIRECTIVES.

// dailyRunRequestMaximum is the most HTTP requests one run of
// `mosdns-cdnctl update-lists --automatic` has been MEASURED to make, over every
// path the table below drives.
//
// It is 20, and the shape that reaches it is the FULLY ENABLED cold start: a
// machine with nothing published, both policy switches on, and upstream committing
// while the run is in flight -- which is what makes the second refreshChinaList in
// one run publish instead of reporting already-at.
//
// It is not the number the shipped default costs. With both switches off the run
// is the drift report alone, and the table measures that at 6 (both sources
// drifted) and 2 (neither). One TimeoutStartSec covers the larger of the two, so
// it covers this one; the unit file says so, because "two budgets and one
// directive" is the honest description and a comment that quotes only the cheap
// one is how the previous figure went stale.
const dailyRunRequestMaximum = 20

// budgetSlack is what TimeoutStartSec adds on top of
// dailyRunRequestMaximum x updateListTimeout.
//
// One more request-timeout, and the reason it is a full one rather than a token:
// everything the run does that is NOT a request. A publication extracts a
// source tarball, converts and sorts a list of 110,551 rules, writes a prefix
// list, and replaces two files by two renames under the control lock. The full
// list takes 100 ms to LOAD once it is converted (docs/measured-environment.md,
// "A 13-fold rule count adds 95 ms"), so a whole request-timeout for all of that
// is generous by a wide margin -- and generosity costs nothing here, because this
// budget only ever binds when an origin is degraded.
const budgetSlack = updateListTimeout

// originSetup is the machine state one measured run starts from: where the
// command writes, a GitHub origin to reach, and a Cloudflare one.
//
// All three are always supplied even for a path that never reaches one of them,
// because a run that could only be measured against the origin it happened to
// touch would report whichever paths its fixture happened to serve.
type originSetup func(t *testing.T) (rangePaths, *fakeOrigin, *rangeOrigin)

// countRequests runs one `update-lists --automatic` over the given policy and
// returns how many requests it made in total, across BOTH origins.
//
// The Cloudflare range document is counted rather than ignored, because
// lists.cloudflare.automatic makes the same command reach api.cloudflare.com and
// that request is bounded by the same client timeout: a budget that counted only
// the GitHub ones would be under the real one by exactly the amount the cheap
// switch costs.
func countRequests(t *testing.T, wantExit int, china, cloudflare bool, setup originSetup) int {
	t.Helper()
	paths, github, ranges := setup(t)
	var stdout, stderr bytes.Buffer
	code := runWith(append([]string{"update-lists", "--automatic",
		"--policy", automaticPolicy(t, china, cloudflare),
		"--previous-lock", filepath.Join(paths.dir, "source-lock.previous.json")},
		refreshArgs(paths)...), &stdout, &stderr, ranges.services(github.client))
	if code != wantExit {
		t.Fatalf("the measured run exited %d, want %d (stderr: %s)", code, wantExit, stderr.String())
	}
	return len(github.requests) + ranges.bodies()
}

// bothSourcesMoved publishes a healthy two-source pair and then serves an origin
// whose HEAD has moved on for BOTH repositories.
//
// Both sources drifting is the expensive half of the drift report and of the
// refresh, and a machine published with one source only would measure a cheaper
// path than the one a current install actually takes.
func bothSourcesMoved(t *testing.T) (rangePaths, *fakeOrigin, *rangeOrigin) {
	t.Helper()
	paths := newRangePaths(t)
	publishChinaPairWithSupplement(t, paths.listPaths(), lockedCommit, lockedSupplementCommit,
		append(rulesOf(200), supplementRules(2)...))
	return paths, movedOrigin(t), newRangeOrigin(t)
}

// movedOrigin serves a reachable primary at remoteCommit and a reachable
// supplement at remoteSupplementCommit, each with the archive HEAD resolves to.
func movedOrigin(t *testing.T) *fakeOrigin {
	t.Helper()
	return newFakeOriginSources(t, map[string]fakeSource{
		rules.RepositoryChina: {head: remoteCommit, archives: map[string][]byte{
			remoteCommit: chinaSourceArchive(t, remoteCommit, string(rulesOf(200)))}},
		rules.RepositoryV2Fly: {head: remoteSupplementCommit, archives: map[string][]byte{
			remoteSupplementCommit: supplementArchive(t, remoteSupplementCommit, 2)}},
	})
}

// coldStart is the same origin with nothing published on the machine at all,
// which is the state that makes --automatic install a pair before it reports on
// one.
func coldStart(t *testing.T) (rangePaths, *fakeOrigin, *rangeOrigin) {
	t.Helper()
	return newRangePaths(t), movedOrigin(t), newRangeOrigin(t)
}

// TestTheDailyRunCostsWhatItIsMeasuredToCost is the measurement the unit's
// TimeoutStartSec is derived from.
//
// Every row is a real run of the real command against the fake origin, which
// appends to origin.requests before it handles anything -- so a request that 404s
// or that the run makes and then abandons is still counted. The figures were read
// off these runs rather than reasoned about, and the reasoning is not always
// right: the cold start publishes the same two archives TWICE, because
// refreshChinaList downloads them to size them and then hands the measured commit
// to runPinRemote, which downloads them again to publish. That duplication is 4
// of the 20 and it is not visible anywhere except here.
func TestTheDailyRunCostsWhatItIsMeasuredToCost(t *testing.T) {
	for _, path := range []struct {
		name              string
		china, cloudflare bool
		wantExit          int
		setup             originSetup
		requests          int
		why               string
	}{
		{
			name: "the shipped default, nothing moved",
			setup: func(t *testing.T) (rangePaths, *fakeOrigin, *rangeOrigin) {
				paths := newRangePaths(t)
				publishChinaPairWithSupplement(t, paths.listPaths(), lockedCommit, lockedSupplementCommit,
					append(rulesOf(200), supplementRules(2)...))
				return paths, quietOrigin(t), newRangeOrigin(t)
			},
			wantExit: exitSuccess,
			requests: 2,
			why: "one commit document per locked source and no archive at all: a check " +
				"that finds nothing moved never downloads anything. This is what almost " +
				"every machine on this package runs, because both switches ship off",
		},
		{
			name:     "the shipped default, both sources drifted",
			setup:    bothSourcesMoved,
			wantExit: exitSuccess,
			requests: 6,
			why: "one commit document per locked source, then ResolveHEAD for each " +
				"drifted source, which is the commit document again plus the archive it " +
				"digests. This is the DEFAULT machine's worst day",
		},
		{
			name:     "lists.china.automatic only, both sources drifted",
			china:    true,
			setup:    bothSourcesMoved,
			wantExit: exitSuccess,
			requests: 14,
			why:      "the 6 above, then a refresh: ResolveHEAD of each source (4), the download that sizes them (2), and the same download again inside the publication it hands the measured commit to (2)",
		},
		{
			name:       "both switches on, both sources drifted",
			china:      true,
			cloudflare: true,
			setup:      bothSourcesMoved,
			wantExit:   exitSuccess,
			requests:   15,
			why:        "the 14 above plus the one Cloudflare range document, which the cheap switch costs and which the same client timeout bounds",
		},
		{
			name:       "both switches on, a cold start",
			china:      true,
			cloudflare: true,
			setup:      coldStart,
			wantExit:   exitSuccess,
			requests:   16,
			why:        "a reachability probe (1), a full publication (8), the drift report on the pair that publication left behind (2), and a second refresh that finds it already published and says so (4), plus the range document",
		},
		{
			name:       "both switches on, a cold start, upstream commits during the run",
			china:      true,
			cloudflare: true,
			setup:      midRunMove,
			wantExit:   exitSuccess,
			requests:   20,
			why: "the cold start above with the supplement's HEAD moving between the two " +
				"refreshes in one run, so the second one PUBLISHES (8) where it would " +
				"otherwise report already-at (4). This is the measured maximum, and it " +
				"is the figure the unit's budget is derived from",
		},
		{
			name:       "both switches on, a cold start, the primary is unreachable",
			china:      true,
			cloudflare: true,
			setup:      fallbackPrimary,
			// The fallback is published and then the refresh cannot migrate it, so
			// the run stops with the code that means "the check could not do its job".
			// Counting it is the point: a path that exits early is still a path the
			// budget has to cover up to the moment it stops.
			wantExit: exitStateUnavailable,
			requests: 6,
			why: "a fallback publication is a primary with no supplement beside it, so " +
				"there is half as much to download",
		},
		{
			name:     "lists.china.automatic only, migrating off the fallback",
			china:    true,
			setup:    publishedFromFallback,
			wantExit: exitSuccess,
			requests: 11,
			why:      "the fallback pair names one source, so the drift report is 2 rather than 6, and the migration publishes in 9",
		},
	} {
		t.Run(path.name, func(t *testing.T) {
			got := countRequests(t, path.wantExit, path.china, path.cloudflare, path.setup)
			if got != path.requests {
				t.Errorf("--automatic made %d requests, and this table measures %d.\n%s",
					got, path.requests, path.why)
			}
			if got > dailyRunRequestMaximum {
				t.Errorf("--automatic made %d requests, which is more than the %d the unit's "+
					"TimeoutStartSec is derived from; raise the measurement and the unit with it",
					got, dailyRunRequestMaximum)
			}
		})
	}
}

// midRunMove is the cold start whose supplement moves on between the two
// refreshChinaList calls one --automatic run makes.
//
// It is a real scenario rather than a contrived one -- upstream commits on a
// schedule this package does not control, and the window between a cold start's
// publication and the refresh that follows the drift report is minutes wide --
// and it is the reason the budget is 20 rather than the 16 a quiet cold start
// costs. The flip happens on the third ask of the supplement's HEAD, which is the
// second refresh's own resolution.
func midRunMove(t *testing.T) (rangePaths, *fakeOrigin, *rangeOrigin) {
	t.Helper()
	const moved = "6666666666666666666666666666666666666666"
	origin := movedOrigin(t)
	origin.setArchives(rules.RepositoryV2Fly, map[string][]byte{
		remoteSupplementCommit: supplementArchive(t, remoteSupplementCommit, 2),
		moved:                  supplementArchive(t, moved, 2),
	})
	asked := 0
	origin.onServe = func(o *fakeOrigin, request *http.Request) {
		if request.URL.Path != origin.commitPath(rules.RepositoryV2Fly) {
			return
		}
		asked++
		if asked == 3 {
			o.setHead(rules.RepositoryV2Fly, moved)
		}
	}
	return newRangePaths(t), origin, newRangeOrigin(t)
}

// fallbackPrimary is a cold start whose primary cannot be reached, so the run
// publishes the v2fly fallback and then stops, because the refresh cannot migrate
// what it just published without the primary.
//
// It is the one path whose exit is not success, and it is in the table anyway: a
// run that gives up early is still a run the budget has to cover up to the moment
// it gives up.
func fallbackPrimary(t *testing.T) (rangePaths, *fakeOrigin, *rangeOrigin) {
	t.Helper()
	origin := newFakeOriginSources(t, map[string]fakeSource{
		rules.RepositoryV2Fly: {head: remoteCommit, archives: map[string][]byte{
			remoteCommit: sourceArchive(t, remoteCommit, string(rulesOf(200)))}},
	})
	return newRangePaths(t), origin, newRangeOrigin(t)
}

// publishedFromFallback is a machine carrying the v2fly primary with no
// supplement beside it, which is what a first migration from the fallback leaves
// behind and what the unattended refresh exists to move off.
func publishedFromFallback(t *testing.T) (rangePaths, *fakeOrigin, *rangeOrigin) {
	t.Helper()
	paths := newRangePaths(t)
	publishPair(t, paths.listPaths(), lockedCommit, rulesOf(200))
	return paths, movedOrigin(t), newRangeOrigin(t)
}

// quietOrigin serves a reachable origin whose HEAD is exactly what the published
// pair was built from, for both repositories.
func quietOrigin(t *testing.T) *fakeOrigin {
	t.Helper()
	return newFakeOriginSources(t, map[string]fakeSource{
		rules.RepositoryChina: {head: lockedCommit, archives: map[string][]byte{
			lockedCommit: chinaSourceArchive(t, lockedCommit, string(rulesOf(200)))}},
		rules.RepositoryV2Fly: {head: lockedSupplementCommit, archives: map[string][]byte{
			lockedSupplementCommit: supplementArchive(t, lockedSupplementCommit, 2)}},
	})
}

// The unit's budget is derived from the measurement, and this is the gate over
// the derivation.
//
// Three things have to agree and none of them is checked against the others, so
// each can fail alone: the measured count (the table above), the per-request
// timeout the command actually applies, and the number in the unit file. The
// first two are read from the code this package ships -- `updateListTimeout` is
// the constant the production client is built from, not a second copy of it.
func TestTheDailyListUnitBudgetsTheMeasuredRequestMaximum(t *testing.T) {
	unit := readListCheckUnit(t)
	sections := parseUnitFile(t, unit)

	seconds, found := unitDirective(sections, "Service", "TimeoutStartSec")
	if !found {
		t.Fatalf("mosdns-list-check.service has no [Service] TimeoutStartSec, so the daily " +
			"list work runs on systemd's 90-second default for a oneshot")
	}
	budget, err := strconv.Atoi(seconds)
	if err != nil {
		t.Fatalf("TimeoutStartSec=%q is not a whole number of seconds: %v", seconds, err)
	}

	// The arithmetic the comment states. Every request is bounded by the command's
	// own client timeout, so requests x that is the longest a run can take, and a
	// budget at or below it kills the process before the run could report that the
	// origin was slow -- which is the one outcome the directive exists to prevent.
	perRequest := int(updateListTimeout / time.Second)
	required := dailyRunRequestMaximum * perRequest
	if budget < required {
		t.Errorf("mosdns-list-check.service budgets %ds, and one --automatic run was measured "+
			"to make as many as %d requests against a %d-second client timeout each (%ds). "+
			"systemd would SIGKILL a degraded-but-answering origin before it could report "+
			"that it was slow. Raise TimeoutStartSec to at least %d",
			budget, dailyRunRequestMaximum, perRequest, required, required)
	}
	if budget <= required {
		t.Errorf("mosdns-list-check.service budgets %ds, which is exactly the %d requests x %d "+
			"seconds arithmetic and leaves nothing for the work in a run that is not a "+
			"request: extracting, converting and sorting a list of 110,551 rules, and two "+
			"renames under the control lock. Add the slack", budget, dailyRunRequestMaximum, perRequest)
	}
	if budget > required+int(2*budgetSlack/time.Second) {
		t.Errorf("mosdns-list-check.service budgets %ds against a measured worst case of %ds. "+
			"That is more than double, and a budget that loose stops being a budget: it "+
			"stops being the number somebody has to keep in step with the code",
			budget, required)
	}

	// And the prose has to keep naming the relationship, because a number that is
	// right in the file and absent from the comment is a number the next reader
	// re-derives by hand -- which is how the 180 this replaced came to describe a
	// command that no longer runs. The last of these is the sentence the previous
	// comment got right, and the only reason this directive is not the oneshot
	// default.
	comments := unitCommentText(unit)
	for _, claim := range []string{
		"At most " + strconv.Itoa(dailyRunRequestMaximum) + " requests",
		"MEASURED",
		fmt.Sprintf("%d x %d = %d", dailyRunRequestMaximum, perRequest, required),
		"adds a request has to change this number with it",
		"cut a slow origin off before it could report that it was slow",
	} {
		if !strings.Contains(comments, claim) {
			t.Errorf("mosdns-list-check.service no longer says %q in its comments, so the "+
				"budget is a number in a unit file with no stated relationship to the code "+
				"that has to keep it true", claim)
		}
	}
}

// readListCheckUnit reads the shipped unit file this package installs.
//
// The path is walked up out of the package directory rather than assumed, so the
// gate cannot be satisfied by a copy of the file that is not the one shipped.
func readListCheckUnit(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "..", "..", "packaging", "systemd", "mosdns-list-check.service")
	unit, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read the shipped list-check unit: %v", err)
	}
	return string(unit)
}

// parseUnitFile reads a unit file into section -> directive -> value.
//
// Only what this gate needs: the section headers, and the key=value pairs inside
// them. A comment line is not a directive and is not a continuation of the one
// above it, which is why the comment reader below is separate rather than this
// returning commented lines as values.
func parseUnitFile(t *testing.T, unit string) map[string]map[string][]string {
	t.Helper()
	sections := map[string]map[string][]string{}
	current := ""
	for number, raw := range strings.Split(unit, "\n") {
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, ";") {
			continue
		}
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			current = strings.TrimSuffix(strings.TrimPrefix(line, "["), "]")
			if _, seen := sections[current]; !seen {
				sections[current] = map[string][]string{}
			}
			continue
		}
		key, value, found := strings.Cut(line, "=")
		if !found {
			t.Fatalf("mosdns-list-check.service:%d: %q is not a directive", number+1, line)
		}
		if current == "" {
			t.Fatalf("mosdns-list-check.service:%d: %q is a directive before any section",
				number+1, line)
		}
		sections[current][strings.TrimSpace(key)] = append(
			sections[current][strings.TrimSpace(key)], strings.TrimSpace(value))
	}
	return sections
}

func unitDirective(sections map[string]map[string][]string, section, key string) (string, bool) {
	values, found := sections[section][key]
	if !found || len(values) == 0 {
		return "", false
	}
	return values[0], true
}

// unitCommentText is every comment in a unit file as ONE space-joined string.
//
// The same shape installer/tests/test_units.py uses, and for the same reason: a
// claim that happens to straddle a line break is the same claim, and a reader that
// reported where somebody pressed return would make every assertion about it a
// test of the wrapping rather than of the claim.
func unitCommentText(unit string) string {
	var lines []string
	for _, raw := range strings.Split(unit, "\n") {
		line := strings.TrimSpace(raw)
		if !strings.HasPrefix(line, "#") {
			continue
		}
		if text := strings.TrimSpace(strings.TrimPrefix(line, "#")); text != "" {
			lines = append(lines, text)
		}
	}
	return strings.Join(lines, " ")
}
