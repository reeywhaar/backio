package main

import (
	"fmt"
	"sort"
	"strings"
	"testing"
	"time"
)

// A Wednesday, so the three newest days sit inside one ISO week and the day before them
// falls into the previous one.
var testNow = time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)

const testPrefix = "myapp"

func name(t time.Time) string {
	return testPrefix + "-" + t.UTC().Format("20060102_150405") + ".tgz"
}

// maxKept is the ceiling the slots imply. The newest bucket of each kind holds the newest
// archive, which the day slots already keep, so every slot past the first of its kind adds
// one archive.
var maxKept = defaultPolicy.today + (defaultPolicy.daily - 1) +
	(defaultPolicy.weekly - 1) + (defaultPolicy.monthly - 1)

// hourly returns n names at one-hour intervals ending at testNow, newest first.
func hourly(n int) []string {
	out := make([]string, 0, n)
	for i := range n {
		out = append(out, name(testNow.Add(-time.Duration(i)*time.Hour)))
	}
	return out
}

func keepSet(names []string, p policy) map[string]bool {
	return selectBackupsToKeep(parseEntries(names, newNaming(testPrefix)), p)
}

func TestSelectBackupsToKeep(t *testing.T) {
	// A spread with something in every slot: two archives today, one each on the two days
	// before, one last week, one last month.
	spread := []string{
		name(testNow),
		name(testNow.Add(-2 * time.Hour)),
		name(testNow.AddDate(0, 0, -1)),
		name(testNow.AddDate(0, 0, -2)),
		name(testNow.AddDate(0, 0, -10)),
		name(testNow.AddDate(0, 0, -40)),
	}

	tests := []struct {
		name  string
		names []string
		want  []string
	}{
		{
			"nothing",
			nil,
			nil,
		},
		{
			// The only archive there is fills every slot, so it survives. Deleting the
			// single backup a deployment owns is the one outcome that must be impossible.
			"one archive",
			[]string{name(testNow)},
			[]string{name(testNow)},
		},
		{
			// 72 hourly archives ending at noon span four calendar days. The three newest
			// from today, the last hour of the two days before it, and 07-26 for the
			// previous ISO week. The month slot adds nothing — it is all one month.
			"hourly for three days",
			hourly(72),
			[]string{
				name(testNow),
				name(testNow.Add(-1 * time.Hour)),
				name(testNow.Add(-2 * time.Hour)),
				name(testNow.AddDate(0, 0, -1).Add(11 * time.Hour)),
				name(testNow.AddDate(0, 0, -2).Add(11 * time.Hour)),
				name(testNow.AddDate(0, 0, -3).Add(11 * time.Hour)),
			},
		},
		{
			// Every archive is the only occupant of its slot, so nothing is dropped.
			"a full spread",
			spread,
			spread,
		},
		{
			// Whichever extension the source produced, and whether or not the password
			// turned it into a zip, retention must see them all — otherwise archives
			// taken under the old setting accumulate unbounded.
			"mixed extensions together",
			[]string{
				"myapp-20260729_120000.zip",
				"myapp-20260728_120000.tgz",
				"myapp-20260727_120000.sql.gz",
				"myapp-20260726_120000.tar.zst",
			},
			[]string{
				"myapp-20260729_120000.zip",
				"myapp-20260728_120000.tgz",
				"myapp-20260727_120000.sql.gz",
				// Previous ISO week: 07-27 is a Monday.
				"myapp-20260726_120000.tar.zst",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := keepSet(tt.names, defaultPolicy)
			want := map[string]bool{}
			for _, n := range tt.want {
				want[n] = true
			}
			if joinKeys(got) != joinKeys(want) {
				t.Errorf("keep = %s\nwant  = %s", joinKeys(got), joinKeys(want))
			}
		})
	}
}

// The policy caps the archive count regardless of how long the loop has been running,
// which is what stops an hourly backup from filling a bucket.
func TestSelectBackupsToKeepIsBounded(t *testing.T) {
	// A year of hourly archives.
	var names []string
	for i := range 24 * 365 {
		names = append(names, name(testNow.Add(-time.Duration(i)*time.Hour)))
	}
	keep := keepSet(names, defaultPolicy)
	if len(keep) > maxKept {
		t.Errorf("kept %d archives, want at most %d: %s", len(keep), maxKept, joinKeys(keep))
	}
	if !keep[name(testNow)] {
		t.Error("the newest archive was not kept")
	}
}

// Every run prunes what the last run left behind, so a slot no surviving archive can reach
// is a slot that never fills. Only running the loop shows that.
func TestRetentionSurvivesRepeatedPruning(t *testing.T) {
	start := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	const hours = 24 * 60

	pool := map[string]bool{}
	for h := range hours {
		pool[name(start.Add(time.Duration(h)*time.Hour))] = true
		for _, gone := range toRemove(sortedKeys(pool), testPrefix, defaultPolicy) {
			delete(pool, gone)
		}
		if len(pool) > maxKept {
			t.Fatalf("hour %d: pool grew to %d archives", h, len(pool))
		}
	}

	// Two months of hourly backups later: three archives from the last day, the last hour
	// of the two days before it, the last hour of the previous ISO week (07-26, a Sunday),
	// and the last hour of June for the month slot.
	last := start.Add((hours - 1) * time.Hour)
	want := map[string]bool{}
	for _, n := range []string{
		name(last),
		name(last.Add(-1 * time.Hour)),
		name(last.Add(-2 * time.Hour)),
		name(last.AddDate(0, 0, -1)),
		name(last.AddDate(0, 0, -2)),
		name(time.Date(2026, 7, 26, 23, 0, 0, 0, time.UTC)),
		name(time.Date(2026, 6, 30, 23, 0, 0, 0, time.UTC)),
	} {
		want[n] = true
	}
	if joinKeys(pool) != joinKeys(want) {
		t.Errorf("pool = %s\nwant  = %s", joinKeys(pool), joinKeys(want))
	}

	// The same pool pruned twice must lose nothing, or the slots are chasing the clock
	// instead of the archives.
	if again := toRemove(sortedKeys(pool), testPrefix, defaultPolicy); len(again) != 0 {
		t.Errorf("a second prune of the same pool removed %v", again)
	}
}

// The prefix is what separates this project's archives from anything else in the
// subdirectory, so it has to be the thing retention keys on.
func TestToRemoveLeavesForeignNamesAlone(t *testing.T) {
	names := append(hourly(48),
		"notes.txt",
		"otherapp-20260729_120000.zip",
		"myapp-latest.tgz",
		"myapp-20260729_120000",
		"prefixed-myapp-20260729_120000.tgz",
	)
	for _, got := range toRemove(names, testPrefix, defaultPolicy) {
		if !strings.HasPrefix(got, testPrefix+"-") || !strings.Contains(got, "_") {
			t.Errorf("toRemove wants to delete %q, which is not one of ours", got)
		}
	}
	// 48 hourly archives ending at noon span three calendar days, all in one ISO week and
	// one month: three keepers from today plus the last hour of the two days before it.
	// 48 - 5 = 43.
	if n := len(toRemove(names, testPrefix, defaultPolicy)); n != 43 {
		t.Errorf("toRemove returned %d names, want 43", n)
	}
}

// A prefix that is a prefix of another must not capture it, or two sidecars sharing a
// subdirectory prune each other's archives.
func TestToRemoveDoesNotMatchLongerPrefixes(t *testing.T) {
	names := []string{
		"app-20260729_120000.tgz",
		"app-db-20260729_120000.tgz",
		"app-db-20260728_120000.tgz",
		"app-db-20260727_120000.tgz",
		"app-db-20260726_120000.tgz",
		"app-db-20260625_120000.tgz",
		"app-db-20260524_120000.tgz",
	}
	for _, got := range toRemove(names, "app", defaultPolicy) {
		if strings.HasPrefix(got, "app-db-") {
			t.Errorf("pruning prefix %q removed %q, which belongs to prefix %q", "app", got, "app-db")
		}
	}
}

// Zeroed week and month slots are how a deployment says it only wants recent copies.
func TestPolicySlotsAreHonoured(t *testing.T) {
	names := []string{
		name(testNow),
		name(testNow.Add(-2 * time.Hour)),
		name(testNow.AddDate(0, 0, -1)),
		name(testNow.AddDate(0, 0, -10)),
		name(testNow.AddDate(0, 0, -40)),
	}

	keep := keepSet(names, policy{today: 1, daily: 1, weekly: 0, monthly: 0})
	if joinKeys(keep) != name(testNow) {
		t.Errorf("keep = %s, want only %s", joinKeys(keep), name(testNow))
	}

	// More today slots than there are archives today keeps what exists, no more.
	keep = keepSet(names, policy{today: 10, daily: 0, weekly: 0, monthly: 0})
	if want := 2; len(keep) != want {
		t.Errorf("kept %d archives, want %d: %s", len(keep), want, joinKeys(keep))
	}
}

func TestLoadPolicy(t *testing.T) {
	t.Run("defaults", func(t *testing.T) {
		got, err := loadPolicy()
		if err != nil {
			t.Fatal(err)
		}
		if got != defaultPolicy {
			t.Errorf("loaded %+v, want %+v", got, defaultPolicy)
		}
	})

	t.Run("overrides", func(t *testing.T) {
		t.Setenv("RETENTION_TODAY", "5")
		t.Setenv("RETENTION_MONTHLY", "0")
		got, err := loadPolicy()
		if err != nil {
			t.Fatal(err)
		}
		want := policy{today: 5, daily: defaultPolicy.daily, weekly: defaultPolicy.weekly, monthly: 0}
		if got != want {
			t.Errorf("loaded %+v, want %+v", got, want)
		}
	})

	// A policy that keeps nothing deletes the archive the run just uploaded, which is
	// never what someone configuring backups meant.
	t.Run("no day slots is an error", func(t *testing.T) {
		t.Setenv("RETENTION_TODAY", "0")
		t.Setenv("RETENTION_DAILY", "0")
		if _, err := loadPolicy(); err == nil {
			t.Error("a policy keeping nothing was accepted")
		}
	})

	t.Run("nonsense is an error", func(t *testing.T) {
		for _, bad := range []string{"-1", "many", "3.5"} {
			t.Setenv("RETENTION_WEEKLY", bad)
			if _, err := loadPolicy(); err == nil {
				t.Errorf("RETENTION_WEEKLY=%q was accepted", bad)
			}
		}
	})
}

func TestParseEntriesSortsNewestFirst(t *testing.T) {
	got := parseEntries([]string{
		"myapp-20260701_000000.tgz",
		"myapp-20260729_120000.tgz",
		"myapp-20260715_060000.zip",
		"garbage",
	}, newNaming(testPrefix))
	if len(got) != 3 {
		t.Fatalf("parsed %d entries, want 3 (garbage dropped)", len(got))
	}
	for i := 1; i < len(got); i++ {
		if !got[i-1].date.After(got[i].date) {
			t.Errorf("entry %d (%s) is not newer than %d (%s)",
				i-1, got[i-1].name, i, got[i].name)
		}
	}
}

func TestParseDate(t *testing.T) {
	n := newNaming(testPrefix)

	for _, good := range []string{
		"myapp-20260729_031500.tgz",
		"myapp-20260729_031500.zip",
		"myapp-20260729_031500.sql.gz",
		"myapp-20260729_031500.tar.zst",
	} {
		got, ok := n.parseDate(good)
		if !ok {
			t.Errorf("%q did not parse", good)
			continue
		}
		if want := time.Date(2026, 7, 29, 3, 15, 0, 0, time.UTC); !got.Equal(want) {
			t.Errorf("%q parsed as %s, want %s", good, got, want)
		}
	}

	for _, bad := range []string{
		"myapp-2026729_031500.tgz",
		"myapp-20260729-031500.tgz",
		"prefix-myapp-20260729_031500.tgz",
		"myapp-20260729_031500.",
		"myapp-20260729_031500",
		fmt.Sprintf("myapp-%s.tgz", "20260729_0315"),
		// Dated by a normalising time.Date as 2027-01-29, which this agent could never
		// have written and must not be trusted to order or delete.
		"myapp-20261329_031500.tgz",
		"myapp-20260732_031500.tgz",
		"myapp-20260729_251500.tgz",
	} {
		if _, ok := n.parseDate(bad); ok {
			t.Errorf("%q parsed but should not have", bad)
		}
	}
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func joinKeys(m map[string]bool) string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return strings.Join(keys, ", ")
}
