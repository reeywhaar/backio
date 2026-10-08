// Command agent is the backup sidecar: it accepts an archive from the service it sits
// next to and forwards it to backio.
//
// It speaks backio's own upload protocol, so a service already posting to backio only
// has to change the URL. What it adds is everything that would otherwise be
// reimplemented per project — the token, the archive naming, optional encryption, and a
// retention policy applied to both the remote and any local copies.
//
//	myapp ──POST /backup──▶ agent ──POST /backup──▶ backio ──▶ Drive, S3, …
//	        (no credential)         (Bearer token)
//
// The service decides when to back up and what goes in the archive. The agent decides
// nothing about the contents and everything about what happens to them afterwards.
package main

import (
	"backio/internal"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// defaultBackupDir is where archives are kept locally. Mount a volume over it to keep
// copies that do not depend on the remote being reachable; see resolveBackupDir for what
// happens when nothing is mounted there.
const defaultBackupDir = "/backups"

const (
	defaultPort          = "8080"
	defaultUploadTimeout = 30 * time.Minute
	defaultPrefix        = "backup"
	defaultExtension     = "tgz"
	// maxFieldSize bounds the non-file parts of the upload. They are three short
	// strings; anything larger is a mistake or an attempt at one.
	maxFieldSize = 4 << 10
)

// prefixRe keeps the archive prefix to characters that are safe in a filename, in a URL
// query, and in the regexp retention builds from it.
var prefixRe = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)

// extensionRe allows dotted extensions — sql.gz, tar.zst — because the extension is
// whatever the service produced, not a fixed archive format.
var extensionRe = regexp.MustCompile(`^[A-Za-z0-9]+(?:\.[A-Za-z0-9]+)*$`)

var logger = slog.New(slog.NewJSONHandler(os.Stderr, nil))

// sevenZip is the encryption command, as a variable so a test can point it at something
// that fails and check what the failure leaves behind.
var sevenZip = "7z"

// clock is where archive timestamps come from, as a variable so a test can hold it still
// or move it while a request waits.
var clock = time.Now

type config struct {
	port          string
	url           string
	provider      string
	subdirectory  string
	token         string
	password      string
	prefix        string
	extension     string
	dir           string
	keepLocal     bool
	expectEvery   time.Duration
	uploadTimeout time.Duration
	retention     policy
}

// agent holds the state one process accumulates: when it started, when it last managed a
// complete backup, and a lock that keeps two uploads from pruning against each other.
type agent struct {
	cfg       config
	startedAt time.Time

	mu          sync.Mutex
	lastSuccess time.Time
	lastError   string

	// Separate from mu, which an upload holds for minutes: a request is stamped the moment
	// it arrives, not once it gets its turn.
	stampMu   sync.Mutex
	lastStamp time.Time
}

func main() {
	command := "serve"
	if len(os.Args) > 1 {
		command = os.Args[1]
	}

	switch command {
	case "serve":
		if err := serve(); err != nil {
			logError("agent", err)
			os.Exit(1)
		}
	case "healthcheck":
		if err := healthcheck(); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\ncommands: serve, healthcheck\n", command)
		os.Exit(1)
	}
}

func serve() error {
	cfg, err := loadConfig()
	if err != nil {
		return err
	}

	dir, keepLocal, err := resolveBackupDir(defaultBackupDir, cfg.token)
	if err != nil {
		return err
	}
	cfg.dir, cfg.keepLocal = dir, keepLocal

	a := &agent{cfg: cfg, startedAt: time.Now()}

	mux := http.NewServeMux()
	mux.HandleFunc("/backup", a.backupHandler)
	mux.HandleFunc("/health", a.healthHandler)

	destination := "local only"
	if cfg.token != "" {
		destination = cfg.provider + ":" + cfg.subdirectory
	}
	log("agent", "Agent starting",
		"port", cfg.port,
		"destination", destination,
		"prefix", cfg.prefix,
		"encrypted", cfg.password != "",
		"keep_local", cfg.keepLocal,
	)
	return http.ListenAndServe(":"+cfg.port, logRequests(mux))
}

// backupHandler is the whole job: take the archive the service just made, name it, lock
// it if asked to, hand it to backio, and prune what the policy no longer wants.
//
// Serialised, because two backups arriving at once would run retention against each
// other's view of the remote. Archives are infrequent and large; queueing one behind
// another costs nothing that matters.
func (a *agent) backupHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		jsonError(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	ts := a.stamp()

	a.mu.Lock()
	defer a.mu.Unlock()

	destination, err := a.receive(r, ts)
	if err != nil {
		a.lastError = err.Error()
		logError("backup", err)
		jsonError(w, err.Error(), statusFor(err))
		return
	}

	a.lastSuccess, a.lastError = time.Now(), ""
	w.Header().Set("Content-Type", "application/json")
	if destination == "" {
		// Local-only: there is no remote path to report, but the archive is safely on
		// disk and saying "ok" for it is honest.
		fmt.Fprint(w, `{"status":"ok"}`)
		return
	}
	fmt.Fprintf(w, `{"status":"ok","destination":%q}`, destination)
}

func (a *agent) receive(r *http.Request, ts string) (string, error) {
	cfg := a.cfg

	// The archive lands under a temporary name first: the extension comes from fields
	// that multipart is free to send after the file, and a half-written archive must
	// never sit in the directory retention reads under a name that looks complete.
	part := filepath.Join(cfg.dir, fmt.Sprintf(".incoming-%s.part", ts))
	defer os.Remove(part)

	sourceName, err := readArchive(r, part)
	if err != nil {
		return "", err
	}

	extension := cfg.extension
	if extension == "" {
		extension = extensionOf(sourceName)
	}
	archiveName := cfg.prefix + "-" + ts + "." + extension
	archive := filepath.Join(cfg.dir, archiveName)
	if err := os.Rename(part, archive); err != nil {
		return "", err
	}
	info, _ := os.Stat(archive)
	log("backup", "Received "+archiveName, "size_bytes", sizeOf(info), "from", sourceName)

	// Without a volume the archive is a courier's parcel, not a copy: it exists to be
	// forwarded and goes away whether or not that worked. Deferred through a closure
	// rather than on the value, because encryption is about to rebind it and it is the
	// file that exists at the end which has to be removed.
	if !cfg.keepLocal {
		defer func() { os.Remove(archive) }()
	}

	if cfg.password != "" {
		encryptedName := cfg.prefix + "-" + ts + ".zip"
		encrypted := filepath.Join(cfg.dir, encryptedName)

		log("encrypt", "Encrypting to "+encryptedName)
		// -mx=1: the archive a service produces is normally compressed already, so
		// compressing again buys nothing.
		if err := run(sevenZip, "a", "-tzip", "-p"+cfg.password, "-mem=AES256", "-mx=1",
			encrypted, archive); err != nil {
			os.Remove(encrypted)
			// The plaintext goes too, even with a volume mounted. A password says the
			// archive is not to be left lying around readable, and a directory of
			// encrypted backups with one plaintext file in it is the last place anyone
			// would think to look for that.
			os.Remove(archive)
			return "", err
		}
		// 7z creates the file at the umask, unlike the upload, which opens it 0600.
		// Encrypted or not, the contents are whatever the service considers its data.
		if err := os.Chmod(encrypted, 0o600); err != nil {
			return "", err
		}
		if err := os.Remove(archive); err != nil {
			return "", err
		}
		archive, archiveName = encrypted, encryptedName
	}

	destination, err := uploadToBackio(archive, archiveName, cfg)
	if err != nil {
		return "", err
	}

	if cfg.keepLocal {
		cleanupLocalBackups(cfg)
	}
	cleanupRemoteBackups(cfg)
	return destination, nil
}

// readArchive streams the uploaded archive to dest and returns the name the service gave
// it, which is used for nothing but its extension.
//
// Two shapes are accepted. A multipart form with a "backup" file part is what backio
// itself takes, so a service already posting there only changes the URL. A raw body is
// what everything else can manage — `curl --data-binary @dump.sql.gz`, a shell script
// with no multipart tooling — and needs a name only if the extension matters.
func readArchive(r *http.Request, dest string) (string, error) {
	f, err := os.OpenFile(dest, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return "", err
	}
	defer f.Close()

	mediaType, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if !strings.HasPrefix(mediaType, "multipart/") {
		name := r.URL.Query().Get("name")
		n, err := io.Copy(f, r.Body)
		if err != nil {
			return "", err
		}
		if n == 0 {
			return "", badRequest("empty request body: expected an archive")
		}
		return name, nil
	}

	reader, err := r.MultipartReader()
	if err != nil {
		return "", badRequest("failed to read form: " + err.Error())
	}

	var (
		name  string
		found bool
		size  int64
	)
	for {
		p, err := reader.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			return "", badRequest("failed to read form: " + err.Error())
		}
		// The file part streams straight to disk; the rest are short strings, and only
		// "name" is read at all — the subdirectory and provider are the agent's to
		// decide, not the service's to ask for.
		if p.FormName() == "backup" {
			found = true
			if name == "" {
				name = p.FileName()
			}
			size, err = io.Copy(f, p)
			p.Close()
			if err != nil {
				return "", err
			}
			continue
		}
		if p.FormName() == "name" {
			value, err := io.ReadAll(io.LimitReader(p, maxFieldSize))
			p.Close()
			if err != nil {
				return "", err
			}
			if v := strings.TrimSpace(string(value)); v != "" {
				name = v
			}
			continue
		}
		p.Close()
	}

	if !found {
		return "", badRequest("backup file is required")
	}
	if size == 0 {
		return "", badRequest("empty archive: the backup file has no contents")
	}
	return name, nil
}

// extensionOf takes everything after the first dot, so a dump.sql.gz keeps both halves
// rather than being restored as a .gz nobody can identify.
func extensionOf(name string) string {
	// path.Base, not filepath.Base: the name came from another machine, whose separator
	// is not this platform's business.
	name = path.Base(strings.ReplaceAll(name, `\`, "/"))
	_, ext, ok := strings.Cut(name, ".")
	if !ok || !extensionRe.MatchString(ext) {
		return defaultExtension
	}
	return ext
}

// healthHandler reports what the container's healthcheck and a passing human both want
// to know: whether a backup has landed recently enough to believe the pipeline works.
//
// Without BACKUP_EXPECT_EVERY the agent has no idea how often the service intends to
// back up, so it reports on being able to serve and nothing more. With it, silence for
// longer than that window is the failure this whole thing exists to catch early.
func (a *agent) healthHandler(w http.ResponseWriter, r *http.Request) {
	a.mu.Lock()
	last, lastError := a.lastSuccess, a.lastError
	a.mu.Unlock()

	body := map[string]any{"status": "ok"}
	if !last.IsZero() {
		body["last_backup"] = last.UTC().Format(time.RFC3339)
	}
	if lastError != "" {
		body["last_error"] = lastError
	}

	status := http.StatusOK
	if a.cfg.expectEvery > 0 {
		// Before the first backup the clock runs from startup, so a container that has
		// just come up is not reported unhealthy for a backup that is not due yet.
		since, what := time.Since(a.startedAt), "startup"
		if !last.IsZero() {
			since, what = time.Since(last), "the last backup"
		}
		// Twice the window: one missed backup is a service that restarted at an
		// awkward moment, two is a pattern.
		if since > 2*a.cfg.expectEvery {
			status = http.StatusServiceUnavailable
			body["status"] = "stale"
			body["message"] = fmt.Sprintf("no backup in the %s since %s (expected every %s)",
				since.Round(time.Second), what, a.cfg.expectEvery)
		}
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(body)
}

// healthcheck is what the image's HEALTHCHECK runs: the server's own /health, from
// inside the container, so the check needs no tools the image would not otherwise ship.
func healthcheck() error {
	port := envOr("PORT", defaultPort)
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Get("http://127.0.0.1:" + port + "/health")
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(io.LimitReader(resp.Body, maxFieldSize))
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("unhealthy (%d): %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	return nil
}

func loadConfig() (config, error) {
	// BACKIO_* is the destination: the server this forwards to, the credential for it,
	// and where the archives land. All four or none — see requireDestinationGroup.
	if err := requireDestinationGroup(); err != nil {
		return config{}, err
	}

	cfg := config{
		port:         envOr("PORT", defaultPort),
		provider:     strings.TrimSpace(os.Getenv("BACKIO_PROVIDER")),
		subdirectory: strings.TrimSpace(os.Getenv("BACKIO_SUBDIRECTORY")),
		token:        strings.TrimSpace(os.Getenv("BACKIO_TOKEN")),
		// BACKUP_* is what happens to the archive on the way through.
		password:  os.Getenv("BACKUP_PASSWORD"),
		extension: strings.TrimPrefix(os.Getenv("BACKUP_EXTENSION"), "."),
	}
	if raw := strings.TrimSpace(os.Getenv("BACKIO_HOST")); raw != "" {
		host, err := backioHost(raw)
		if err != nil {
			return config{}, err
		}
		cfg.url = host
	}
	cfg.prefix = envOr("BACKUP_PREFIX", prefixFor(cfg.subdirectory))

	if !prefixRe.MatchString(cfg.prefix) {
		return cfg, fmt.Errorf("BACKUP_PREFIX %q: expected letters, digits, dot, dash or underscore", cfg.prefix)
	}
	if cfg.extension != "" && !extensionRe.MatchString(cfg.extension) {
		return cfg, fmt.Errorf("BACKUP_EXTENSION %q: expected an extension like tgz or tar.zst", cfg.extension)
	}
	if cfg.token != "" {
		// The same rules the server applies, so a bad subdirectory is a startup error
		// here rather than a 400 the first time the service posts an archive.
		if msg := internal.ValidateField(cfg.subdirectory, "BACKIO_SUBDIRECTORY"); msg != "" {
			return cfg, fmt.Errorf("%s", msg)
		}
		if msg := internal.ValidateField(cfg.provider, "BACKIO_PROVIDER"); msg != "" {
			return cfg, fmt.Errorf("%s", msg)
		}
	}

	for _, d := range []struct {
		key      string
		fallback time.Duration
		into     *time.Duration
	}{
		{"BACKUP_EXPECT_EVERY", 0, &cfg.expectEvery},
		{"UPLOAD_TIMEOUT", defaultUploadTimeout, &cfg.uploadTimeout},
	} {
		v, err := envDuration(d.key, d.fallback)
		if err != nil {
			return cfg, err
		}
		*d.into = v
	}

	retention, err := loadPolicy()
	if err != nil {
		return cfg, err
	}
	cfg.retention = retention

	return cfg, nil
}

// requireDestinationGroup enforces all-or-nothing on the BACKIO_ variables.
//
// All four is the normal deployment: forward to backio, and keep local copies too if a
// volume is mounted. None of them is the other real configuration — a mounted /backups
// and nothing else, copies kept on the host with no remote involved.
//
// Anything between is refused, because none of the four can be guessed on someone's
// behalf and the failure it would produce is the worst kind: an agent that starts clean,
// accepts every archive, answers "ok", and uploads nothing until the day someone goes
// looking for a backup. Missing names are listed together, since a sidecar is configured
// once and hunting one variable per restart is the slow way to do it.
func requireDestinationGroup() error {
	var set, missing []string
	for _, v := range []string{"BACKIO_HOST", "BACKIO_PROVIDER", "BACKIO_SUBDIRECTORY", "BACKIO_TOKEN"} {
		if strings.TrimSpace(os.Getenv(v)) != "" {
			set = append(set, v)
		} else {
			missing = append(missing, v)
		}
	}
	if len(set) == 0 || len(missing) == 0 {
		return nil
	}
	return fmt.Errorf("%s set but %s not: the destination is all four or none — "+
		"all four to forward to backio, none to keep local copies only",
		strings.Join(set, ", "), strings.Join(missing, ", "))
}

// backioHost turns BACKIO_HOST into the base URL the requests are built on.
//
// The scheme is required rather than assumed. Defaulting it to http would silently make
// "backio.example.com" a plaintext request carrying a bearer token, which is the one
// mistake here worth refusing to make on someone's behalf. Inside a compose network it
// is http://backio:8080; over anything else it is https, and the difference has to be
// stated.
//
// A trailing /backup is trimmed, because that is the endpoint the root README hands out
// for curl and send-backup.sh, and pasting it here should not produce a 404 for
// /backup/backup an hour later.
func backioHost(raw string) (string, error) {
	raw = strings.TrimSuffix(strings.TrimRight(strings.TrimSpace(raw), "/"), "/backup")
	raw = strings.TrimRight(raw, "/")

	u, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("BACKIO_HOST %q: %w", raw, err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", fmt.Errorf("BACKIO_HOST %q: needs a scheme, e.g. http://%s", raw,
			strings.TrimPrefix(raw, "//"))
	}
	if u.Host == "" {
		return "", fmt.Errorf("BACKIO_HOST %q: no host", raw)
	}
	return u.Scheme + "://" + u.Host + strings.TrimRight(u.Path, "/"), nil
}

// prefixFor names the archives after the subdirectory they live in, so a file downloaded
// from the remote still says which project and environment it came from. Slashes become
// dashes: myapp/production yields myapp-production-20260903_041500.tgz.
//
// Only a default. A deployment that wants its archives called something else sets
// BACKUP_PREFIX, and one with no subdirectory at all — local copies, no upload — gets
// the generic name because there is nothing to derive from.
func prefixFor(subdirectory string) string {
	prefix := strings.Trim(subdirectory, "/")
	if prefix == "" {
		return defaultPrefix
	}
	prefix = strings.ReplaceAll(prefix, "/", "-")
	if !prefixRe.MatchString(prefix) {
		return defaultPrefix
	}
	return prefix
}

// resolveBackupDir decides where archives are written and whether they stay there.
//
// The image does not create /backups, and Docker creates a mount target that the image is
// missing, so the directory exists exactly when something is mounted over it. Without it,
// local copies would sit in the container's writable layer, where they vanish the moment
// the container is recreated — the one time a local copy would have been useful. The
// archive goes to a temp directory instead, and is deleted once it has been forwarded.
//
// An explicit BACKUP_DIR is always kept, and created if it is missing: a path someone
// named is a path someone wants.
func resolveBackupDir(defaultDir, token string) (string, bool, error) {
	if dir := os.Getenv("BACKUP_DIR"); dir != "" {
		return dir, true, os.MkdirAll(dir, 0o700)
	}
	if isDir(defaultDir) {
		return defaultDir, true, nil
	}
	// Nowhere to keep the archive and nowhere to send it. The agent would accept every
	// archive, write it to a directory that dies with the container, and delete it —
	// the one configuration where it does nothing at all, so it says so at startup
	// rather than answering "ok" to a service that believes it is backed up.
	if token == "" {
		return "", false, fmt.Errorf("no %s directory and no BACKIO_ destination: "+
			"mount a volume at %s for local copies, or set all four BACKIO_ variables "+
			"to forward to backio", defaultDir, defaultDir)
	}

	log("agent", "No "+defaultDir+" directory: forwarding without keeping local copies")
	dir, err := os.MkdirTemp("", "backio-agent-")
	return dir, false, err
}

func isDir(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}

func cleanupLocalBackups(cfg config) {
	names, err := listLocalBackups(cfg.dir)
	if err != nil {
		logError("cleanup", fmt.Errorf("failed to list local backups: %w", err))
		return
	}
	remove := toRemove(names, cfg.prefix, cfg.retention)
	if len(remove) == 0 {
		return
	}
	log("cleanup", "Local retention: "+cfg.retention.String())
	for _, name := range remove {
		log("cleanup", "Removing local: "+name)
		if err := os.Remove(filepath.Join(cfg.dir, name)); err != nil {
			logError("cleanup", fmt.Errorf("failed to remove local %s: %w", name, err))
		}
	}
}

func cleanupRemoteBackups(cfg config) {
	if cfg.token == "" {
		return
	}
	names, err := listBackioBackups(cfg)
	if err != nil {
		logError("remote", fmt.Errorf("failed to list remote backups: %w", err))
		return
	}
	remove := toRemove(names, cfg.prefix, cfg.retention)
	if len(remove) == 0 {
		return
	}
	log("remote", "Remote retention: "+cfg.retention.String())
	for _, name := range remove {
		log("remote", "Removing remote: "+name)
		if err := deleteBackioBackup(name, cfg); err != nil {
			logError("remote", fmt.Errorf("failed to delete remote %s: %w", name, err))
		}
	}
}

// uploadToBackio streams the archive into the multipart body rather than building it in
// memory. A database dump is exactly the kind of thing that is larger than the sidecar's
// memory limit, and the alternative holds two copies of it at once.
func uploadToBackio(archive, archiveName string, cfg config) (string, error) {
	if cfg.token == "" {
		log("remote", "Keeping a local copy only: no BACKIO_ destination is configured")
		return "", nil
	}

	log("remote", "Uploading "+archiveName,
		"provider", cfg.provider, "subdirectory", cfg.subdirectory)

	f, err := os.Open(archive)
	if err != nil {
		return "", err
	}
	defer f.Close()

	pr, pw := io.Pipe()
	w := multipart.NewWriter(pw)
	contentType := w.FormDataContentType()

	go func() {
		var err error
		defer func() { pw.CloseWithError(err) }()

		// Fields before the file, so the server has them without buffering the archive.
		for _, field := range [][2]string{
			{"name", archiveName},
			{"subdirectory", cfg.subdirectory},
			{"provider", cfg.provider},
		} {
			if err = w.WriteField(field[0], field[1]); err != nil {
				return
			}
		}
		var part io.Writer
		if part, err = w.CreateFormFile("backup", archiveName); err != nil {
			return
		}
		if _, err = io.Copy(part, f); err != nil {
			return
		}
		err = w.Close()
	}()

	req, err := http.NewRequest(http.MethodPost, cfg.url+"/backup", pr)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+cfg.token)
	req.Header.Set("Content-Type", contentType)

	client := &http.Client{Timeout: cfg.uploadTimeout}
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("upload to %s: %w", cfg.url, err)
	}
	defer resp.Body.Close()

	responseBytes, _ := io.ReadAll(io.LimitReader(resp.Body, maxFieldSize))
	responseText := strings.TrimSpace(string(responseBytes))

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("upload failed (%d): %s", resp.StatusCode, responseText)
	}

	var result struct {
		Status      string `json:"status"`
		Destination string `json:"destination"`
	}
	if err := json.Unmarshal(responseBytes, &result); err != nil {
		return "", fmt.Errorf("invalid response: %s", responseText)
	}
	if result.Status != "ok" {
		return "", fmt.Errorf("upload failed: %s", responseText)
	}

	log("remote", "Remote backup success: "+result.Destination)
	return result.Destination, nil
}

func listBackioBackups(cfg config) ([]string, error) {
	params := url.Values{}
	params.Set("provider", cfg.provider)
	params.Set("subdirectory", cfg.subdirectory)

	resp, err := backioRequest(http.MethodGet, cfg, params)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	// A create-only token is a legitimate configuration: it produces working backups
	// with no remote pruning, rather than an error on every upload.
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		log("remote", "Skipping remote list: insufficient permissions")
		return nil, nil
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		text, _ := io.ReadAll(io.LimitReader(resp.Body, maxFieldSize))
		return nil, fmt.Errorf("list failed (%d): %s", resp.StatusCode, strings.TrimSpace(string(text)))
	}

	var items []struct {
		Name  string `json:"Name"`
		IsDir bool   `json:"IsDir"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&items); err != nil {
		return nil, err
	}

	var names []string
	for _, item := range items {
		if !item.IsDir {
			names = append(names, item.Name)
		}
	}
	return names, nil
}

func deleteBackioBackup(name string, cfg config) error {
	params := url.Values{}
	params.Set("provider", cfg.provider)
	params.Set("subdirectory", cfg.subdirectory)
	params.Set("name", name)

	resp, err := backioRequest(http.MethodDelete, cfg, params)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		logError("remote", fmt.Errorf("skipping remote delete %s: insufficient permissions", name))
		return nil
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		text, _ := io.ReadAll(io.LimitReader(resp.Body, maxFieldSize))
		return fmt.Errorf("delete failed (%d): %s", resp.StatusCode, strings.TrimSpace(string(text)))
	}
	return nil
}

func backioRequest(method string, cfg config, params url.Values) (*http.Response, error) {
	req, err := http.NewRequest(method, cfg.url+"/backup?"+params.Encode(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+cfg.token)
	client := &http.Client{Timeout: time.Minute}
	return client.Do(req)
}

func listLocalBackups(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var names []string
	for _, e := range entries {
		if !e.IsDir() {
			names = append(names, e.Name())
		}
	}
	return names, nil
}

// badRequest marks the errors that are the caller's fault, so the handler can answer 400
// for a malformed upload and 500 for a remote that would not take it.
type badRequestError struct{ message string }

func (e badRequestError) Error() string { return e.message }

func badRequest(message string) error { return badRequestError{message} }

func statusFor(err error) int {
	if _, ok := err.(badRequestError); ok {
		return http.StatusBadRequest
	}
	return http.StatusInternalServerError
}

func jsonError(w http.ResponseWriter, message string, code int) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	b, _ := json.Marshal(map[string]any{"error": true, "message": message})
	w.Write(b)
}

// statusRecorder wraps http.ResponseWriter to capture the status code written.
type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

// logRequests logs every request with method, path, status and duration — the same shape
// backio's own log has, so a deployment running both reads as one system.
func logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The healthcheck runs on a timer and would otherwise be most of the log.
		if r.URL.Path == "/health" {
			next.ServeHTTP(w, r)
			return
		}

		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)

		log("request", "request completed",
			"method", r.Method,
			"path", r.URL.Path,
			"status", rec.status,
			"duration_ms", time.Since(start).Milliseconds(),
		)
	})
}

func log(operation, message string, args ...any) {
	logger.Info(message, append([]any{"operation", operation}, args...)...)
}

func logError(operation string, err error) {
	logger.Error(err.Error(), "operation", operation)
}

// stamp is the timestamp an archive is named with: UTC, in the format retention.go parses
// back out.
//
// Taken when the request arrives. Taken once it had the lock, a request that queued behind
// another upload was named for when that upload finished, minutes after it came in.
//
// Never the same twice. Two archives in the same second would share a name, and the
// second would overwrite the first locally and on the remote, so the later one is moved
// on a second: still dateable, still in arrival order.
func (a *agent) stamp() string {
	a.stampMu.Lock()
	defer a.stampMu.Unlock()

	t := clock().UTC().Truncate(time.Second)
	if !t.After(a.lastStamp) {
		t = a.lastStamp.Add(time.Second)
	}
	a.lastStamp = t
	return t.Format("20060102_150405")
}

func sizeOf(info os.FileInfo) int64 {
	if info == nil {
		return 0
	}
	return info.Size()
}

func run(cmd string, args ...string) error {
	c := exec.Command(cmd, args...)
	var stderr bytes.Buffer
	c.Stderr = &stderr
	if err := c.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return fmt.Errorf("%s failed: %s", cmd, msg)
	}
	return nil
}

func envOr(key, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return fallback
}

// envDuration accepts either a Go duration ("6h", "90s") or a bare number of seconds,
// because a schedule written in one place as 3600 should not have to be rewritten here.
func envDuration(key string, fallback time.Duration) (time.Duration, error) {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return fallback, nil
	}
	if seconds, err := strconv.Atoi(raw); err == nil {
		if seconds <= 0 {
			return 0, fmt.Errorf("%s %q: must be positive", key, raw)
		}
		return time.Duration(seconds) * time.Second, nil
	}
	d, err := time.ParseDuration(raw)
	if err != nil {
		return 0, fmt.Errorf("%s %q: expected seconds or a duration like 6h", key, raw)
	}
	if d <= 0 {
		return 0, fmt.Errorf("%s %q: must be positive", key, raw)
	}
	return d, nil
}
