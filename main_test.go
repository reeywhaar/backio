package main

import (
	"fmt"
	"strings"
	"testing"
)

// fakeRclone records the calls uploadAtomic makes and fails whichever subcommand a test
// names, so the ordering and the cleanup can be asserted without a remote.
type fakeRclone struct {
	calls    []string
	failOn   string
	failWith string
}

func (f *fakeRclone) install(t *testing.T) {
	t.Helper()
	old := runRclone
	runRclone = func(args ...string) ([]byte, error) {
		f.calls = append(f.calls, strings.Join(args, " "))
		if len(args) > 0 && args[0] == f.failOn {
			return []byte(f.failWith), fmt.Errorf("exit status 1")
		}
		return nil, nil
	}
	t.Cleanup(func() { runRclone = old })
}

// The whole point of the two-phase upload: the bytes land under a name nothing
// recognises, and only a finished transfer gets renamed to the one that counts.
func TestUploadAtomicRenamesOnlyOnSuccess(t *testing.T) {
	f := &fakeRclone{}
	f.install(t)

	destination, _, err := uploadAtomic("/tmp/archive.tar", "gdrive", "myapp/production", "myapp-20260903_041500.tgz")
	if err != nil {
		t.Fatal(err)
	}
	if want := "gdrive:myapp/production/myapp-20260903_041500.tgz"; destination != want {
		t.Errorf("destination = %q, want %q", destination, want)
	}

	want := []string{
		"copyto /tmp/archive.tar gdrive:myapp/production/.incomplete-myapp-20260903_041500.tgz",
		"moveto gdrive:myapp/production/.incomplete-myapp-20260903_041500.tgz gdrive:myapp/production/myapp-20260903_041500.tgz",
	}
	if len(f.calls) != len(want) {
		t.Fatalf("calls = %v, want %v", f.calls, want)
	}
	for i := range want {
		if f.calls[i] != want[i] {
			t.Errorf("call %d = %q, want %q", i, f.calls[i], want[i])
		}
	}
}

// A transfer that dies halfway must not leave a truncated file under a valid archive
// name: the request fails either way, but that corpse would outlive it and the sidecar's
// retention policy would count it as a real backup.
func TestUploadAtomicLeavesNothingUnderTheFinalName(t *testing.T) {
	for _, failOn := range []string{"copyto", "moveto"} {
		f := &fakeRclone{failOn: failOn, failWith: "connection reset"}
		f.install(t)

		_, out, err := uploadAtomic("/tmp/archive.tar", "gdrive", "myapp/production", "myapp-20260903_041500.tgz")
		if err == nil {
			t.Fatalf("%s: a failed transfer reported success", failOn)
		}
		if !strings.Contains(string(out), "connection reset") {
			t.Errorf("%s: output = %q, want rclone's own message", failOn, out)
		}

		final := "gdrive:myapp/production/myapp-20260903_041500.tgz"
		incomplete := "gdrive:myapp/production/.incomplete-myapp-20260903_041500.tgz"

		var moved, cleaned bool
		for _, call := range f.calls {
			if strings.HasPrefix(call, "moveto ") && strings.HasSuffix(call, final) && failOn == "copyto" {
				moved = true
			}
			if call == "deletefile "+incomplete {
				cleaned = true
			}
		}
		if moved {
			t.Errorf("%s: renamed to the final name after a failed copy", failOn)
		}
		if !cleaned {
			t.Errorf("%s: left the incomplete upload behind: %v", failOn, f.calls)
		}
	}
}

// The temporary name has to be one the sidecar's retention policy cannot match, or an
// interrupted upload becomes an archive that counts. That policy matches names beginning
// with a configured prefix, and internal.ValidateField never accepts a prefix starting
// with a dot.
func TestIncompleteNameCannotLookLikeAnArchive(t *testing.T) {
	f := &fakeRclone{}
	f.install(t)

	uploadAtomic("/tmp/a.tar", "gdrive", "myapp/production", "myapp-production-20260903_041500.tgz")

	for _, call := range f.calls {
		for _, field := range strings.Fields(call) {
			path, ok := strings.CutPrefix(field, "gdrive:myapp/production/")
			if !ok || path == "myapp-production-20260903_041500.tgz" {
				continue
			}
			if !strings.HasPrefix(path, ".") {
				t.Errorf("temporary name %q does not start with a dot, so retention could match it", path)
			}
		}
	}
}
