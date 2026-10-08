package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
)

// The agent logs a line per step, which is what makes a failing container readable and
// what makes `go test` output unreadable. Only the assertions matter here.
func TestMain(m *testing.M) {
	logger = slog.New(slog.NewJSONHandler(io.Discard, nil))
	os.Exit(m.Run())
}

// fakeBackio stands in for the server the agent forwards to, recording what it was sent
// and answering in backio's own shapes.
type fakeBackio struct {
	*httptest.Server

	auth     string
	fields   map[string]string
	payload  []byte
	existing []string
	deleted  []string
	listCode int
}

func newFakeBackio(t *testing.T) *fakeBackio {
	t.Helper()
	f := &fakeBackio{fields: map[string]string{}, listCode: http.StatusOK}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.auth = r.Header.Get("Authorization")
		switch r.Method {
		case http.MethodPost:
			if err := r.ParseMultipartForm(1 << 20); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			for _, k := range []string{"name", "subdirectory", "provider"} {
				f.fields[k] = r.FormValue(k)
			}
			file, _, err := r.FormFile("backup")
			if err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			defer file.Close()
			f.payload, _ = io.ReadAll(file)
			f.existing = append(f.existing, f.fields["name"])
			fmt.Fprintf(w, `{"status":"ok","destination":%q}`,
				f.fields["provider"]+":"+f.fields["subdirectory"]+"/"+f.fields["name"])
		case http.MethodGet:
			if f.listCode != http.StatusOK {
				http.Error(w, "nope", f.listCode)
				return
			}
			items := make([]map[string]any, 0, len(f.existing))
			for _, n := range f.existing {
				items = append(items, map[string]any{"Name": n, "IsDir": false})
			}
			json.NewEncoder(w).Encode(items)
		case http.MethodDelete:
			name := r.URL.Query().Get("name")
			f.deleted = append(f.deleted, name)
			kept := f.existing[:0]
			for _, n := range f.existing {
				if n != name {
					kept = append(kept, n)
				}
			}
			f.existing = kept
			fmt.Fprint(w, `{"status":"ok"}`)
		}
	}))
	t.Cleanup(f.Close)
	return f
}

// newAgent builds an agent writing to a temp directory, the way serve() would.
func newAgent(t *testing.T, cfg config) *agent {
	t.Helper()
	if cfg.dir == "" {
		cfg.dir = t.TempDir()
	}
	if cfg.prefix == "" {
		cfg.prefix = "myapp-production"
	}
	if cfg.uploadTimeout == 0 {
		cfg.uploadTimeout = time.Minute
	}
	if cfg.retention == (policy{}) {
		cfg.retention = defaultPolicy
	}
	return &agent{cfg: cfg, startedAt: time.Now()}
}

// postArchive posts an archive the way a service would: a multipart form with a "backup"
// file part, which is exactly what backio itself accepts.
func postArchive(t *testing.T, a *agent, filename string, body []byte) *httptest.ResponseRecorder {
	t.Helper()
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	part, err := w.CreateFormFile("backup", filename)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := part.Write(body); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodPost, "/backup", &buf)
	req.Header.Set("Content-Type", w.FormDataContentType())
	rec := httptest.NewRecorder()
	a.backupHandler(rec, req)
	return rec
}

func TestBackupHandlerForwardsToBackio(t *testing.T) {
	backio := newFakeBackio(t)
	a := newAgent(t, config{
		url: backio.URL, token: "secret", provider: "gdrive", subdirectory: "myapp/production",
	})

	rec := postArchive(t, a, "whatever-the-service-called-it.tar", []byte("archive contents"))

	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	if backio.auth != "Bearer secret" {
		t.Errorf("Authorization = %q — the agent holds the credential, not the service", backio.auth)
	}
	if string(backio.payload) != "archive contents" {
		t.Errorf("payload = %q", backio.payload)
	}
	// The destination is the agent's to decide: the service asked for nothing and got
	// the subdirectory and provider the sidecar was configured with.
	for k, want := range map[string]string{
		"subdirectory": "myapp/production",
		"provider":     "gdrive",
	} {
		if backio.fields[k] != want {
			t.Errorf("field %s = %q, want %q", k, backio.fields[k], want)
		}
	}

	// The name is regenerated: retention can only prune what it can date, and the name
	// the service chose carries no promise of a parseable timestamp.
	name := backio.fields["name"]
	if !strings.HasPrefix(name, "myapp-production-") || !strings.HasSuffix(name, ".tar") {
		t.Errorf("name = %q, want myapp-production-<timestamp>.tar", name)
	}
	if _, ok := newNaming("myapp-production").parseDate(name); !ok {
		t.Errorf("name %q is not one retention can date", name)
	}

	var body struct{ Status, Destination string }
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Status != "ok" || body.Destination != "gdrive:myapp/production/"+name {
		t.Errorf("response = %s", rec.Body)
	}
}

// The extension follows the archive rather than a setting, so one sidecar image serves a
// tarball, a pg_dump and a zip without being told which it is getting.
func TestBackupHandlerNamesTheArchive(t *testing.T) {
	for _, tt := range []struct{ filename, wantExt string }{
		{"backup.tar", "tar"},
		{"dump.sql.gz", "sql.gz"},
		{"data.tar.zst", "tar.zst"},
		{"archive.tgz", "tgz"},
		// Nothing to go on: the generic extension rather than a name with none.
		{"backup", defaultExtension},
		{"", defaultExtension},
	} {
		backio := newFakeBackio(t)
		a := newAgent(t, config{
			url: backio.URL, token: "secret", provider: "gdrive", subdirectory: "myapp/production",
		})

		if rec := postArchive(t, a, tt.filename, []byte("x")); rec.Code != http.StatusOK {
			t.Fatalf("%q: status %d: %s", tt.filename, rec.Code, rec.Body)
		}
		if got := backio.fields["name"]; !strings.HasSuffix(got, "."+tt.wantExt) {
			t.Errorf("archive from %q named %q, want extension %q", tt.filename, got, tt.wantExt)
		}
	}
}

// An explicit setting wins, for a service whose filename says nothing useful.
func TestBackupHandlerHonoursExplicitExtension(t *testing.T) {
	backio := newFakeBackio(t)
	a := newAgent(t, config{
		url: backio.URL, token: "secret", provider: "gdrive",
		subdirectory: "myapp/production", extension: "tar.zst",
	})

	postArchive(t, a, "backup.tar", []byte("x"))
	if got := backio.fields["name"]; !strings.HasSuffix(got, ".tar.zst") {
		t.Errorf("name = %q, want the configured extension", got)
	}
}

// Not every service has multipart tooling. `curl --data-binary @dump.sql.gz` should work
// as well as a form does.
func TestBackupHandlerAcceptsARawBody(t *testing.T) {
	backio := newFakeBackio(t)
	a := newAgent(t, config{
		url: backio.URL, token: "secret", provider: "gdrive", subdirectory: "myapp/production",
	})

	req := httptest.NewRequest(http.MethodPost, "/backup?name=dump.sql.gz",
		strings.NewReader("raw archive"))
	req.Header.Set("Content-Type", "application/octet-stream")
	rec := httptest.NewRecorder()
	a.backupHandler(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	if string(backio.payload) != "raw archive" {
		t.Errorf("payload = %q", backio.payload)
	}
	if got := backio.fields["name"]; !strings.HasSuffix(got, ".sql.gz") {
		t.Errorf("name = %q, want the extension from the query", got)
	}
}

// A form part order the agent does not control: the name arriving after the file must
// still reach the naming, or the extension silently reverts to the default.
func TestBackupHandlerReadsTheNameFieldAfterTheFile(t *testing.T) {
	backio := newFakeBackio(t)
	a := newAgent(t, config{
		url: backio.URL, token: "secret", provider: "gdrive", subdirectory: "myapp/production",
	})

	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	part, _ := w.CreateFormFile("backup", "")
	part.Write([]byte("archive"))
	w.WriteField("subdirectory", "ignored/by/the/agent")
	w.WriteField("name", "dump.sql.gz")
	w.Close()

	req := httptest.NewRequest(http.MethodPost, "/backup", &buf)
	req.Header.Set("Content-Type", w.FormDataContentType())
	rec := httptest.NewRecorder()
	a.backupHandler(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	if got := backio.fields["name"]; !strings.HasSuffix(got, ".sql.gz") {
		t.Errorf("name = %q, want the extension from the trailing name field", got)
	}
	// The service does not get to choose where its archives land; that is the whole
	// reason it holds no credential.
	if got := backio.fields["subdirectory"]; got != "myapp/production" {
		t.Errorf("subdirectory = %q, want the agent's own", got)
	}
}

func TestBackupHandlerRejectsBadRequests(t *testing.T) {
	backio := newFakeBackio(t)
	cfg := config{url: backio.URL, token: "secret", provider: "gdrive", subdirectory: "myapp/production"}

	t.Run("no file part", func(t *testing.T) {
		a := newAgent(t, cfg)
		var buf bytes.Buffer
		w := multipart.NewWriter(&buf)
		w.WriteField("name", "backup.tar")
		w.Close()

		req := httptest.NewRequest(http.MethodPost, "/backup", &buf)
		req.Header.Set("Content-Type", w.FormDataContentType())
		rec := httptest.NewRecorder()
		a.backupHandler(rec, req)

		if rec.Code != http.StatusBadRequest {
			t.Errorf("status = %d, want 400", rec.Code)
		}
	})

	// An empty archive uploads and prunes exactly like a real one, quietly replacing the
	// history with nothing.
	t.Run("empty archive", func(t *testing.T) {
		a := newAgent(t, cfg)
		if rec := postArchive(t, a, "backup.tar", nil); rec.Code != http.StatusBadRequest {
			t.Errorf("status = %d, want 400", rec.Code)
		}
	})

	t.Run("empty raw body", func(t *testing.T) {
		a := newAgent(t, cfg)
		req := httptest.NewRequest(http.MethodPost, "/backup", strings.NewReader(""))
		req.Header.Set("Content-Type", "application/octet-stream")
		rec := httptest.NewRecorder()
		a.backupHandler(rec, req)

		if rec.Code != http.StatusBadRequest {
			t.Errorf("status = %d, want 400", rec.Code)
		}
	})

	t.Run("wrong method", func(t *testing.T) {
		a := newAgent(t, cfg)
		rec := httptest.NewRecorder()
		a.backupHandler(rec, httptest.NewRequest(http.MethodGet, "/backup", nil))
		if rec.Code != http.StatusMethodNotAllowed {
			t.Errorf("status = %d, want 405", rec.Code)
		}
	})
}

// The client must not be told "ok" until the archive is actually on the remote, or it
// marks the backup delivered and moves on while the upload is still in flight — or
// failing. True today because receive() is synchronous; asserted here so it stays true.
func TestBackupHandlerAnswersOnlyAfterTheUploadCompletes(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(entered)
		<-release // the upload is now in flight and going nowhere
		io.Copy(io.Discard, r.Body)
		fmt.Fprint(w, `{"status":"ok","destination":"gdrive:myapp/production/x.tgz"}`)
	}))
	defer srv.Close()

	a := newAgent(t, config{
		url: srv.URL, token: "secret", provider: "gdrive", subdirectory: "myapp/production",
	})

	var rec *httptest.ResponseRecorder
	done := make(chan struct{})
	go func() {
		defer close(done)
		rec = postArchive(t, a, "backup.tgz", []byte("archive contents"))
	}()

	<-entered
	// Mid-upload: nothing may have been said to the client yet.
	select {
	case <-done:
		t.Fatal("the handler answered before the upload finished")
	case <-time.After(50 * time.Millisecond):
	}

	close(release)
	<-done

	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), `"status":"ok"`) {
		t.Errorf("body = %s", rec.Body)
	}
}

// A rejected upload must not read as success: the service is entitled to know its backup
// did not land, and to retry.
func TestBackupHandlerReportsAFailedUpload(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "forbidden", http.StatusForbidden)
	}))
	defer srv.Close()

	a := newAgent(t, config{
		url: srv.URL, token: "secret", provider: "gdrive", subdirectory: "myapp/production",
	})

	rec := postArchive(t, a, "backup.tar", []byte("archive"))
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "403") {
		t.Errorf("body = %s, want the upstream status", rec.Body)
	}
}

// Forwarding without a volume must not leave archives in the container's writable layer,
// where they accumulate until the disk fills.
func TestBackupHandlerKeepsNothingWithoutAVolume(t *testing.T) {
	backio := newFakeBackio(t)
	dir := t.TempDir()
	a := newAgent(t, config{
		dir: dir, keepLocal: false,
		url: backio.URL, token: "secret", provider: "gdrive", subdirectory: "myapp/production",
	})

	postArchive(t, a, "backup.tar", []byte("archive"))

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("forwarding left %d files behind: %v", len(entries), entries)
	}
}

// A failed upload must not leave the archive behind either, or every failure adds a file
// nothing will ever remove.
func TestBackupHandlerKeepsNothingAfterAFailedUpload(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "nope", http.StatusInternalServerError)
	}))
	defer srv.Close()

	dir := t.TempDir()
	a := newAgent(t, config{
		dir: dir, url: srv.URL, token: "secret", provider: "gdrive", subdirectory: "myapp/production",
	})

	postArchive(t, a, "backup.tar", []byte("archive"))

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("a failed upload left %d files behind: %v", len(entries), entries)
	}
}

func TestBackupHandlerKeepsLocalCopies(t *testing.T) {
	backio := newFakeBackio(t)
	dir := t.TempDir()
	a := newAgent(t, config{
		dir: dir, keepLocal: true,
		url: backio.URL, token: "secret", provider: "gdrive", subdirectory: "myapp/production",
	})

	postArchive(t, a, "backup.tar", []byte("archive contents"))

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("kept %d files, want 1: %v", len(entries), entries)
	}
	got, err := os.ReadFile(filepath.Join(dir, entries[0].Name()))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "archive contents" {
		t.Errorf("local copy = %q", got)
	}
}

// Local-only: a mounted volume and no BACKIO_ variables at all. The archive is kept, no
// upload is attempted, and the service still gets an honest "ok".
func TestBackupHandlerWorksWithoutARemote(t *testing.T) {
	dir := t.TempDir()
	a := newAgent(t, config{dir: dir, keepLocal: true})

	rec := postArchive(t, a, "backup.tar", []byte("archive contents"))
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	// No destination to report, and claiming one would be a lie.
	if strings.Contains(rec.Body.String(), "destination") {
		t.Errorf("body = %s, want no destination", rec.Body)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("kept %d files, want 1: %v", len(entries), entries)
	}
	got, err := os.ReadFile(filepath.Join(dir, entries[0].Name()))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "archive contents" {
		t.Errorf("local copy = %q", got)
	}
}

// Retention is the half of the job that has no other home: the service posts an archive
// and knows nothing about the ones before it.
func TestBackupHandlerPrunesBothEnds(t *testing.T) {
	backio := newFakeBackio(t)
	backio.existing = []string{
		// A year of noon archives, all but the newest few beyond every slot.
		"myapp-production-20260903_120000.tgz",
		"myapp-production-20260902_120000.tgz",
		"myapp-production-20260901_120000.tgz",
		"myapp-production-20260831_120000.tgz",
		"myapp-production-20260830_120000.tgz",
		"myapp-production-20260829_120000.tgz",
		"myapp-production-20260715_120000.tgz",
		"myapp-production-20260615_120000.tgz",
		"myapp-production-20260515_120000.tgz",
		// Not ours: a different project's archives sharing the subdirectory is a
		// configuration mistake, not a licence to prune them.
		"otherapp-20260903_120000.tgz",
		"notes.txt",
	}

	dir := t.TempDir()
	for _, n := range backio.existing {
		if err := os.WriteFile(filepath.Join(dir, n), []byte("old"), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	a := newAgent(t, config{
		dir: dir, keepLocal: true,
		url: backio.URL, token: "secret", provider: "gdrive", subdirectory: "myapp/production",
	})

	if rec := postArchive(t, a, "backup.tgz", []byte("archive")); rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}

	if len(backio.deleted) == 0 {
		t.Fatal("no remote archives were pruned")
	}
	for _, name := range backio.deleted {
		if !strings.HasPrefix(name, "myapp-production-") {
			t.Errorf("pruned %q, which is not one of ours", name)
		}
	}

	local, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range local {
		names = append(names, e.Name())
	}
	sort.Strings(names)
	for _, foreign := range []string{"notes.txt", "otherapp-20260903_120000.tgz"} {
		if !contains(names, foreign) {
			t.Errorf("%q was deleted locally and should not have been", foreign)
		}
	}
	// Seven slots plus the two foreign files nothing may touch.
	if len(names) > 9 {
		t.Errorf("local directory kept %d files: %v", len(names), names)
	}
}

// A create-only token is a legitimate configuration: working backups with no pruning,
// rather than an error on every upload.
func TestBackupHandlerToleratesACreateOnlyToken(t *testing.T) {
	backio := newFakeBackio(t)
	backio.listCode = http.StatusForbidden

	a := newAgent(t, config{
		url: backio.URL, token: "secret", provider: "gdrive", subdirectory: "myapp/production",
	})

	if rec := postArchive(t, a, "backup.tar", []byte("archive")); rec.Code != http.StatusOK {
		t.Errorf("a create-only token failed the upload: %d %s", rec.Code, rec.Body)
	}
	if len(backio.deleted) != 0 {
		t.Errorf("deleted %v with a token that cannot list", backio.deleted)
	}
}

func TestBackupHandlerEncrypts(t *testing.T) {
	if _, err := exec.LookPath("7z"); err != nil {
		t.Skip("7z is not installed; the image provides it")
	}

	backio := newFakeBackio(t)
	a := newAgent(t, config{
		url: backio.URL, token: "secret", provider: "gdrive",
		subdirectory: "myapp/production", password: "hunter2",
	})

	if rec := postArchive(t, a, "backup.tar", []byte("secret contents")); rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}

	if got := backio.fields["name"]; !strings.HasSuffix(got, ".zip") {
		t.Errorf("name = %q, want a .zip", got)
	}
	// What left the container must not be the plaintext that entered it.
	if bytes.Contains(backio.payload, []byte("secret contents")) {
		t.Error("the plaintext archive was uploaded despite BACKUP_PASSWORD")
	}

	// And it must come back out with the password.
	dir := t.TempDir()
	zip := filepath.Join(dir, "archive.zip")
	if err := os.WriteFile(zip, backio.payload, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := run(sevenZip, "x", "-phunter2", "-o"+dir, zip); err != nil {
		t.Fatalf("the uploaded archive did not extract: %s", err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, e := range entries {
		if e.Name() == "archive.zip" {
			continue
		}
		got, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		if string(got) == "secret contents" {
			found = true
		}
	}
	if !found {
		t.Error("the extracted archive did not contain the original contents")
	}
}

// A failed encryption must not leave the plaintext behind — least of all in a directory
// whose whole point is that its contents are encrypted.
func TestBackupHandlerKeepsNothingAfterAFailedEncryption(t *testing.T) {
	old := sevenZip
	sevenZip = "definitely-not-installed-7z"
	t.Cleanup(func() { sevenZip = old })

	backio := newFakeBackio(t)
	for _, keepLocal := range []bool{false, true} {
		dir := t.TempDir()
		a := newAgent(t, config{
			dir: dir, keepLocal: keepLocal, password: "hunter2",
			url: backio.URL, token: "secret", provider: "gdrive", subdirectory: "myapp/production",
		})

		rec := postArchive(t, a, "backup.tar", []byte("plaintext contents"))
		if rec.Code != http.StatusInternalServerError {
			t.Errorf("keepLocal=%v: status = %d, want 500", keepLocal, rec.Code)
		}
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatal(err)
		}
		if len(entries) != 0 {
			t.Errorf("keepLocal=%v: a failed encryption left %d files behind: %v",
				keepLocal, len(entries), entries)
		}
	}
	if backio.payload != nil {
		t.Error("an archive was uploaded despite encryption failing")
	}
}

func TestHealthHandler(t *testing.T) {
	get := func(a *agent) (int, map[string]any) {
		rec := httptest.NewRecorder()
		a.healthHandler(rec, httptest.NewRequest(http.MethodGet, "/health", nil))
		var body map[string]any
		json.Unmarshal(rec.Body.Bytes(), &body)
		return rec.Code, body
	}

	// Without BACKUP_EXPECT_EVERY the agent does not know the service's schedule, so it
	// reports on being able to serve and nothing more.
	t.Run("no expectation is always healthy", func(t *testing.T) {
		a := newAgent(t, config{})
		a.startedAt = time.Now().Add(-30 * 24 * time.Hour)
		if code, _ := get(a); code != http.StatusOK {
			t.Errorf("status = %d, want 200", code)
		}
	})

	t.Run("a recent backup is healthy", func(t *testing.T) {
		a := newAgent(t, config{expectEvery: time.Hour})
		a.lastSuccess = time.Now().Add(-time.Minute)
		if code, body := get(a); code != http.StatusOK || body["status"] != "ok" {
			t.Errorf("status = %d, body = %v", code, body)
		}
	})

	// Silence for twice the expected window is the failure this exists to catch early.
	t.Run("a stale backup is unhealthy", func(t *testing.T) {
		a := newAgent(t, config{expectEvery: time.Hour})
		a.lastSuccess = time.Now().Add(-6 * time.Hour)
		code, body := get(a)
		if code != http.StatusServiceUnavailable {
			t.Errorf("status = %d, want 503", code)
		}
		if body["status"] != "stale" {
			t.Errorf("body = %v", body)
		}
	})

	// A container that has just come up has not missed anything yet.
	t.Run("a fresh start with no backup yet is healthy", func(t *testing.T) {
		a := newAgent(t, config{expectEvery: time.Hour})
		if code, _ := get(a); code != http.StatusOK {
			t.Errorf("status = %d, want 200", code)
		}
	})

	t.Run("a long silence since startup is unhealthy", func(t *testing.T) {
		a := newAgent(t, config{expectEvery: time.Hour})
		a.startedAt = time.Now().Add(-6 * time.Hour)
		if code, _ := get(a); code != http.StatusServiceUnavailable {
			t.Errorf("status = %d, want 503", code)
		}
	})

	// Whoever reads /health after a failure wants to know what went wrong, not just
	// that something did.
	t.Run("the last error is reported", func(t *testing.T) {
		a := newAgent(t, config{})
		a.lastError = "upload failed (403): forbidden"
		_, body := get(a)
		if body["last_error"] != "upload failed (403): forbidden" {
			t.Errorf("body = %v", body)
		}
	})
}

func TestResolveBackupDir(t *testing.T) {
	// A volume mounted at the default path shows up as the directory existing, and the
	// image creates no such directory, so nothing to mount over means nothing to keep.
	t.Run("mounted volume keeps local copies", func(t *testing.T) {
		mounted := t.TempDir()
		t.Setenv("BACKUP_DIR", "")

		dir, keepLocal, err := resolveBackupDir(mounted, "token")
		if err != nil {
			t.Fatal(err)
		}
		if dir != mounted || !keepLocal {
			t.Errorf("resolved %q keepLocal=%v, want %q keepLocal=true", dir, keepLocal, mounted)
		}
	})

	t.Run("no directory forwards without keeping a copy", func(t *testing.T) {
		missing := filepath.Join(t.TempDir(), "backups")
		t.Setenv("BACKUP_DIR", "")

		dir, keepLocal, err := resolveBackupDir(missing, "token")
		if err != nil {
			t.Fatal(err)
		}
		defer os.RemoveAll(dir)

		if keepLocal {
			t.Error("keepLocal is true with no volume mounted")
		}
		if dir == missing {
			t.Errorf("resolved %q, want a temp directory", dir)
		}
		if !isDir(dir) {
			t.Errorf("temp directory %q was not created", dir)
		}
		if isDir(missing) {
			t.Errorf("%q was created after all", missing)
		}
	})

	// Neither a place to keep archives nor a place to send them: the agent would accept
	// every archive and delete it, while the service believes it is backed up.
	t.Run("no directory and no destination is an error", func(t *testing.T) {
		missing := filepath.Join(t.TempDir(), "backups")
		t.Setenv("BACKUP_DIR", "")

		if _, _, err := resolveBackupDir(missing, ""); err == nil {
			t.Error("started with nowhere to keep archives and nowhere to send them")
		}
	})

	// An explicit path is honoured whether or not it exists yet, so a deployment that
	// wants local copies somewhere else does not have to pre-create the directory.
	t.Run("explicit BACKUP_DIR is created and kept", func(t *testing.T) {
		want := filepath.Join(t.TempDir(), "archives")
		t.Setenv("BACKUP_DIR", want)

		dir, keepLocal, err := resolveBackupDir("/nonexistent", "token")
		if err != nil {
			t.Fatal(err)
		}
		if dir != want || !keepLocal {
			t.Errorf("resolved %q keepLocal=%v, want %q keepLocal=true", dir, keepLocal, want)
		}
		if !isDir(want) {
			t.Errorf("%q was not created", want)
		}
	})
}

func TestLoadConfig(t *testing.T) {
	// Cleared rather than assumed absent: the test binary inherits the developer's
	// environment, and a stray BACKIO_TOKEN would change what these cases mean.
	clearEnv := func(t *testing.T) {
		for _, key := range []string{
			"PORT", "BACKIO_HOST", "BACKIO_PROVIDER", "BACKIO_SUBDIRECTORY", "BACKIO_TOKEN",
			"BACKUP_PASSWORD", "BACKUP_PREFIX", "BACKUP_EXTENSION", "BACKUP_DIR",
			"BACKUP_EXPECT_EVERY", "UPLOAD_TIMEOUT", "RETENTION_TODAY", "RETENTION_DAILY",
			"RETENTION_WEEKLY", "RETENTION_MONTHLY",
		} {
			t.Setenv(key, "")
		}
	}

	// Everything the destination needs is named explicitly; only the archive handling
	// has defaults.
	setDestination := func(t *testing.T) {
		t.Setenv("BACKIO_HOST", "http://backio:8080")
		t.Setenv("BACKIO_PROVIDER", "gdrive")
		t.Setenv("BACKIO_SUBDIRECTORY", "myapp/production")
		t.Setenv("BACKIO_TOKEN", "secret")
	}

	t.Run("defaults", func(t *testing.T) {
		clearEnv(t)
		setDestination(t)

		cfg, err := loadConfig()
		if err != nil {
			t.Fatal(err)
		}
		if cfg.port != defaultPort {
			t.Errorf("port = %q", cfg.port)
		}
		if cfg.url != "http://backio:8080" {
			t.Errorf("url = %q", cfg.url)
		}
		if cfg.provider != "gdrive" {
			t.Errorf("provider = %q", cfg.provider)
		}
		if cfg.prefix != "myapp-production" {
			t.Errorf("prefix = %q", cfg.prefix)
		}
		if cfg.retention != defaultPolicy {
			t.Errorf("retention = %+v", cfg.retention)
		}
		// Unset means "no opinion about the schedule", not "expect one immediately".
		if cfg.expectEvery != 0 {
			t.Errorf("expectEvery = %s, want unset", cfg.expectEvery)
		}
	})

	// None of the four can be guessed on someone's behalf, so a partial destination is
	// refused rather than producing an agent that answers "ok" and uploads nothing.
	t.Run("a partial destination is refused", func(t *testing.T) {
		all := []string{"BACKIO_HOST", "BACKIO_PROVIDER", "BACKIO_SUBDIRECTORY", "BACKIO_TOKEN"}
		for _, omitted := range all {
			clearEnv(t)
			setDestination(t)
			t.Setenv(omitted, "")

			_, err := loadConfig()
			if err == nil {
				t.Errorf("a destination with no %s was accepted", omitted)
				continue
			}
			// The message names what is missing: a sidecar is configured once, and
			// hunting one variable per restart is the slow way to do it.
			if !strings.Contains(err.Error(), omitted) {
				t.Errorf("error for missing %s does not name it: %s", omitted, err)
			}
		}
	})

	// The other half of all-or-nothing: none of them is a real configuration, not a
	// half-finished one.
	t.Run("no destination at all is accepted", func(t *testing.T) {
		clearEnv(t)

		cfg, err := loadConfig()
		if err != nil {
			t.Fatalf("local-only config rejected: %s", err)
		}
		if cfg.token != "" || cfg.url != "" {
			t.Errorf("token = %q, url = %q; want both empty", cfg.token, cfg.url)
		}
		// Nothing to derive a name from, so the generic one.
		if cfg.prefix != defaultPrefix {
			t.Errorf("prefix = %q, want %q", cfg.prefix, defaultPrefix)
		}
	})

	// The archive has to be called something, and the subdirectory already says which
	// project and environment it belongs to.
	t.Run("the prefix defaults to the subdirectory", func(t *testing.T) {
		clearEnv(t)
		setDestination(t)

		cfg, err := loadConfig()
		if err != nil {
			t.Fatal(err)
		}
		if cfg.prefix != "myapp-production" {
			t.Errorf("prefix = %q, want %q", cfg.prefix, "myapp-production")
		}

		t.Setenv("BACKUP_PREFIX", "db")
		cfg, err = loadConfig()
		if err != nil {
			t.Fatal(err)
		}
		if cfg.prefix != "db" {
			t.Errorf("prefix = %q, want the explicit %q", cfg.prefix, "db")
		}
	})

	// The same rules the server applies, so a subdirectory that could never work is a
	// startup error rather than a 400 the first time the service posts an archive.
	t.Run("the subdirectory is validated", func(t *testing.T) {
		for _, bad := range []string{"../escape", "with spaces", "sub/../../etc", `back\slash`} {
			clearEnv(t)
			setDestination(t)
			t.Setenv("BACKIO_SUBDIRECTORY", bad)

			if _, err := loadConfig(); err == nil {
				t.Errorf("BACKIO_SUBDIRECTORY=%q was accepted", bad)
			}
		}
	})

	t.Run("the prefix is validated", func(t *testing.T) {
		for _, bad := range []string{"my app", "my/app", "app*"} {
			clearEnv(t)
			setDestination(t)
			t.Setenv("BACKUP_PREFIX", bad)

			if _, err := loadConfig(); err == nil {
				t.Errorf("BACKUP_PREFIX=%q was accepted", bad)
			}
		}
	})

	t.Run("the extension is validated and its dot optional", func(t *testing.T) {
		clearEnv(t)
		setDestination(t)
		t.Setenv("BACKUP_EXTENSION", ".tar.zst")

		cfg, err := loadConfig()
		if err != nil {
			t.Fatal(err)
		}
		if cfg.extension != "tar.zst" {
			t.Errorf("extension = %q, want %q", cfg.extension, "tar.zst")
		}

		t.Setenv("BACKUP_EXTENSION", "tar gz")
		if _, err := loadConfig(); err == nil {
			t.Error(`BACKUP_EXTENSION="tar gz" was accepted`)
		}
	})

	t.Run("durations take seconds or units", func(t *testing.T) {
		clearEnv(t)
		setDestination(t)
		t.Setenv("BACKUP_EXPECT_EVERY", "21600")
		t.Setenv("UPLOAD_TIMEOUT", "2h")

		cfg, err := loadConfig()
		if err != nil {
			t.Fatal(err)
		}
		if cfg.expectEvery != 6*time.Hour {
			t.Errorf("expectEvery = %s, want 6h", cfg.expectEvery)
		}
		if cfg.uploadTimeout != 2*time.Hour {
			t.Errorf("uploadTimeout = %s, want 2h", cfg.uploadTimeout)
		}

		for _, bad := range []string{"0", "-5", "soon"} {
			t.Setenv("BACKUP_EXPECT_EVERY", bad)
			if _, err := loadConfig(); err == nil {
				t.Errorf("BACKUP_EXPECT_EVERY=%q was accepted", bad)
			}
		}
	})
}

func TestBackioHost(t *testing.T) {
	// A trailing /backup is trimmed: that is the endpoint the root README hands out for
	// curl and send-backup.sh, and pasting it here should not produce /backup/backup.
	for _, tt := range []struct{ in, want string }{
		{"http://backio:8080", "http://backio:8080"},
		{"http://backio:8080/", "http://backio:8080"},
		{"http://backio:8080/backup", "http://backio:8080"},
		{"http://backio:8080/backup/", "http://backio:8080"},
		{"https://backio.example.com", "https://backio.example.com"},
		{"https://backio.example.com/backup", "https://backio.example.com"},
		// A path that is not the endpoint is someone's reverse proxy prefix, and theirs
		// to keep.
		{"https://example.com/backio", "https://example.com/backio"},
	} {
		got, err := backioHost(tt.in)
		if err != nil {
			t.Errorf("backioHost(%q): %s", tt.in, err)
			continue
		}
		if got != tt.want {
			t.Errorf("backioHost(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}

	// Defaulting the scheme to http would silently send a bearer token in plaintext to
	// anything that is not a compose-network hostname.
	for _, bad := range []string{"backio", "backio:8080", "//backio:8080", "ftp://backio", ""} {
		if got, err := backioHost(bad); err == nil {
			t.Errorf("backioHost(%q) = %q, want an error demanding a scheme", bad, got)
		}
	}
}

func TestPrefixFor(t *testing.T) {
	for _, tt := range []struct{ in, want string }{
		{"myapp/production", "myapp-production"},
		{"myapp", "myapp"},
		{"/myapp/production/", "myapp-production"},
		{"kidsout/db/hourly", "kidsout-db-hourly"},
		// Nothing to derive from, and nothing that would survive being used as a
		// filename: the generic name rather than a broken one.
		{"", defaultPrefix},
		{"/", defaultPrefix},
	} {
		if got := prefixFor(tt.in); got != tt.want {
			t.Errorf("prefixFor(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestExtensionOf(t *testing.T) {
	// Everything after the first dot, so a dump.sql.gz is not restored as a .gz nobody
	// can identify.
	for _, tt := range []struct{ in, want string }{
		{"dump.sql.gz", "sql.gz"},
		{"backup.tgz", "tgz"},
		{"archive.tar.zst", "tar.zst"},
		{"/var/lib/app/data.tar", "tar"},
		{`C:\backups\data.tar`, "tar"},
		{"noextension", defaultExtension},
		{"", defaultExtension},
		{"weird name.tar gz", defaultExtension},
	} {
		if got := extensionOf(tt.in); got != tt.want {
			t.Errorf("extensionOf(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func contains(names []string, want string) bool {
	for _, n := range names {
		if n == want {
			return true
		}
	}
	return false
}
