package main

import (
	"fmt"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Retention slots. Every slot is defined against the archives that exist rather than
// against the wall clock, because each run prunes what the last run left behind: a
// bucket's keeper is the newest archive in it, and no later archive can land in an
// earlier bucket, so the keeper is settled the moment its bucket ends.
//
// Age windows cannot do that. "The newest archive at least a week old" deletes every
// archive that is four days old — too new for the window, too old for the day slots —
// so nothing survives to reach a week and the slot only ever holds whatever the first
// run happened to pin.
type policy struct {
	// today is how many of the newest archives from the newest day are kept.
	// Everything else that day is pruned, however short the interval.
	today int
	// daily is how many calendar days keep their newest archive.
	daily int
	// weekly and monthly count calendar buckets, and the newest bucket's keeper is the
	// newest archive overall, already held by the day slots. Two buckets each is
	// therefore one week-old copy and one month-old copy.
	weekly  int
	monthly int
}

var defaultPolicy = policy{today: 3, daily: 3, weekly: 2, monthly: 2}

// String is what the logs say a run is about to enforce, in the terms the environment
// variables use rather than the "1 weekly" the bucket counts work out to.
func (p policy) String() string {
	return fmt.Sprintf("%d today, %d daily, %d weekly, %d monthly buckets",
		p.today, p.daily, p.weekly, p.monthly)
}

// loadPolicy reads the slot counts from the environment. Zero is allowed — a deployment
// that only wants the last three archives sets the week and month slots to 0 — but the
// day slots cannot both be zero, which would delete every backup on the first run.
func loadPolicy() (policy, error) {
	p := defaultPolicy
	for _, slot := range []struct {
		key  string
		into *int
	}{
		{"RETENTION_TODAY", &p.today},
		{"RETENTION_DAILY", &p.daily},
		{"RETENTION_WEEKLY", &p.weekly},
		{"RETENTION_MONTHLY", &p.monthly},
	} {
		raw := strings.TrimSpace(os.Getenv(slot.key))
		if raw == "" {
			continue
		}
		n, err := strconv.Atoi(raw)
		if err != nil || n < 0 {
			return p, fmt.Errorf("%s %q: expected a non-negative number", slot.key, raw)
		}
		*slot.into = n
	}
	if p.today == 0 && p.daily == 0 {
		return p, fmt.Errorf("RETENTION_TODAY and RETENTION_DAILY are both 0: " +
			"every archive would be deleted as soon as it is uploaded")
	}
	return p, nil
}

// extPattern matches the extensions archives are named with: tgz, zip, and dotted ones
// like sql.gz, because the extension is whatever the source produced.
const extPattern = `[A-Za-z0-9]+(?:\.[A-Za-z0-9]+)*`

// naming recognises the archives this agent produces — myapp-20260729_031500.tgz, or
// .zip when BACKUP_PASSWORD turned it into an encrypted archive.
//
// Every extension matches on purpose. Toggling the password, or a source that changes
// format, would otherwise make every archive taken under the old setting invisible to
// retention, and they would pile up forever with nothing to remove them.
type naming struct {
	re *regexp.Regexp
}

func newNaming(prefix string) naming {
	return naming{re: regexp.MustCompile(
		`^` + regexp.QuoteMeta(prefix) + `-(\d{4})(\d{2})(\d{2})_(\d{2})(\d{2})(\d{2})\.` + extPattern + `$`,
	)}
}

type backupEntry struct {
	name string
	date time.Time
}

// parseEntries drops names it cannot date and sorts the rest newest first.
func parseEntries(names []string, n naming) []backupEntry {
	var out []backupEntry
	for _, name := range names {
		if date, ok := n.parseDate(name); ok {
			out = append(out, backupEntry{name: name, date: date})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].date.After(out[j].date) })
	return out
}

func (n naming) parseDate(name string) (time.Time, bool) {
	m := n.re.FindStringSubmatch(name)
	if m == nil {
		return time.Time{}, false
	}
	nums := make([]int, 6)
	for i := range nums {
		for _, c := range m[i+1] {
			nums[i] = nums[i]*10 + int(c-'0')
		}
	}
	date := time.Date(nums[0], time.Month(nums[1]), nums[2], nums[3], nums[4], nums[5], 0, time.UTC)
	// time.Date normalises the impossible — month 13 becomes January of the next year —
	// which would let a name this agent could never have written be dated, ordered, and
	// eventually deleted. Round-tripping rejects those instead.
	if date.Format("20060102_150405") != m[1]+m[2]+m[3]+"_"+m[4]+m[5]+m[6] {
		return time.Time{}, false
	}
	return date, true
}

func dayKey(t time.Time) string { return t.Format("2006-01-02") }

func weekKey(t time.Time) string {
	year, week := t.ISOWeek()
	return fmt.Sprintf("%04d-W%02d", year, week)
}

func monthKey(t time.Time) string { return t.Format("2006-01") }

// selectBackupsToKeep applies the retention policy: the newest few archives from the
// newest day, the newest archive of each of the newest days, and the newest archive of
// the second-newest week and month. With the defaults that is seven archives at most,
// whatever the interval.
//
// Expects backups sorted newest first.
func selectBackupsToKeep(backups []backupEntry, p policy) map[string]bool {
	keep := map[string]bool{}
	if len(backups) == 0 {
		return keep
	}

	// The newest archive's day rather than today's date, so a loop that stopped a week
	// ago keeps the last day it managed instead of leaving the slot empty.
	day := dayKey(backups[0].date)
	for i, b := range backups {
		if i == p.today || dayKey(b.date) != day {
			break
		}
		keep[b.name] = true
	}

	for _, slot := range []struct {
		n   int
		key func(time.Time) string
	}{
		{p.daily, dayKey},
		{p.weekly, weekKey},
		{p.monthly, monthKey},
	} {
		for _, name := range bucketKeepers(backups, slot.n, slot.key) {
			keep[name] = true
		}
	}
	return keep
}

// bucketKeepers returns the newest archive from each of the n newest buckets, where key
// puts an archive in a bucket. Expects backups sorted newest first, so the first archive
// seen in a bucket is that bucket's keeper and a bucket's archives are contiguous.
func bucketKeepers(backups []backupEntry, n int, key func(time.Time) string) []string {
	if n == 0 {
		return nil
	}
	var out []string
	seen := ""
	for _, b := range backups {
		bucket := key(b.date)
		if bucket == seen {
			continue
		}
		seen = bucket
		out = append(out, b.name)
		if len(out) == n {
			break
		}
	}
	return out
}

// toRemove is the set difference the callers actually want: everything recognisable that
// no slot claimed. Unrecognisable names are left alone — they are not ours to delete,
// and a subdirectory shared with another project's archives is a configuration mistake,
// not a licence to prune them.
func toRemove(names []string, prefix string, p policy) []string {
	n := newNaming(prefix)
	entries := parseEntries(names, n)
	keep := selectBackupsToKeep(entries, p)
	var out []string
	for _, e := range entries {
		if !keep[e.name] {
			out = append(out, e.name)
		}
	}
	sort.Strings(out)
	return out
}
