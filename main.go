package main

import (
	"backio/internal"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

var logger = slog.New(slog.NewJSONHandler(os.Stderr, nil))

func jsonError(w http.ResponseWriter, message string, code int) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	b, _ := json.Marshal(map[string]any{"error": true, "message": message})
	w.Write(b)
}

func bearerToken(r *http.Request) string {
	auth := r.Header.Get("Authorization")
	if after, ok := strings.CutPrefix(auth, "Bearer "); ok {
		return strings.TrimSpace(after)
	}
	return ""
}

func authorize(w http.ResponseWriter, r *http.Request, provider, subdirectory, permission string) bool {
	token := bearerToken(r)
	if token == "" {
		logger.Warn("authorization missing",
			"provider", provider,
			"subdirectory", subdirectory,
			"permission", permission,
		)
		jsonError(w, "authorization required", http.StatusUnauthorized)
		return false
	}
	ok, err := internal.CheckToken(token, provider, subdirectory, permission)
	if err != nil {
		logger.Error("token verification failed",
			"provider", provider,
			"subdirectory", subdirectory,
			"permission", permission,
			"error", err,
		)
		jsonError(w, "failed to verify token: "+err.Error(), http.StatusInternalServerError)
		return false
	}
	if !ok {
		logger.Warn("authorization denied",
			"provider", provider,
			"subdirectory", subdirectory,
			"permission", permission,
		)
		jsonError(w, "forbidden", http.StatusForbidden)
		return false
	}
	logger.Debug("authorization granted",
		"provider", provider,
		"subdirectory", subdirectory,
		"permission", permission,
	)
	return true
}

func rcloneError(w http.ResponseWriter, out []byte, err error) {
	msg := strings.TrimSpace(string(out))
	if msg == "" {
		msg = err.Error()
	}
	logger.Error("rclone command failed", "error", msg)
	jsonError(w, "rclone failed: "+msg, http.StatusInternalServerError)
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

// clientAddr returns the originating client address, preferring proxy-forwarded
// headers (X-Forwarded-For, X-Real-IP) and falling back to r.RemoteAddr.
func clientAddr(r *http.Request) string {
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		// X-Forwarded-For may be a comma-separated list; the first entry is the
		// original client.
		first, _, _ := strings.Cut(xff, ",")
		return strings.TrimSpace(first)
	}
	if xrip := strings.TrimSpace(r.Header.Get("X-Real-IP")); xrip != "" {
		return xrip
	}
	return r.RemoteAddr
}

// logRequests logs every incoming HTTP request with method, path, status and duration.
func logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}

		logger.Info("request received",
			"method", r.Method,
			"path", r.URL.Path,
			"remote_addr", clientAddr(r),
		)

		next.ServeHTTP(rec, r)

		logger.Info("request completed",
			"method", r.Method,
			"path", r.URL.Path,
			"status", rec.status,
			"duration_ms", time.Since(start).Milliseconds(),
		)
	})
}

func listBackupsHandler(w http.ResponseWriter, r *http.Request) {
	subdirectory := strings.TrimSpace(r.URL.Query().Get("subdirectory"))
	provider := strings.TrimSpace(r.URL.Query().Get("provider"))

	var errs []string
	for _, check := range [][2]string{{"subdirectory", subdirectory}, {"provider", provider}} {
		if msg := internal.ValidateField(check[1], check[0]); msg != "" {
			errs = append(errs, msg)
		}
	}
	if len(errs) > 0 {
		jsonError(w, strings.Join(errs, "\n"), http.StatusBadRequest)
		return
	}

	if !authorize(w, r, provider, subdirectory, "read") {
		return
	}

	target := provider + ":" + subdirectory
	logger.Info("listing backups", "target", target)
	out, err := exec.Command("rclone", "lsjson", target).CombinedOutput()
	if err != nil {
		rcloneError(w, out, err)
		return
	}

	logger.Info("backups listed", "target", target)
	w.Header().Set("Content-Type", "application/json")
	w.Write(out)
}

func deleteBackupHandler(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimSpace(r.URL.Query().Get("name"))
	subdirectory := strings.TrimSpace(r.URL.Query().Get("subdirectory"))
	provider := strings.TrimSpace(r.URL.Query().Get("provider"))

	var errs []string
	for _, check := range [][2]string{{"name", name}, {"subdirectory", subdirectory}, {"provider", provider}} {
		if msg := internal.ValidateField(check[1], check[0]); msg != "" {
			errs = append(errs, msg)
		}
	}
	if len(errs) > 0 {
		jsonError(w, strings.Join(errs, "\n"), http.StatusBadRequest)
		return
	}

	if !authorize(w, r, provider, subdirectory, "delete") {
		return
	}

	target := provider + ":" + filepath.Join(subdirectory, name)
	logger.Info("deleting backup", "target", target)
	out, err := exec.Command("rclone", "deletefile", target).CombinedOutput()
	if err != nil {
		rcloneError(w, out, err)
		return
	}

	logger.Info("backup deleted", "target", target)
	w.Header().Set("Content-Type", "application/json")
	fmt.Fprintf(w, `{"status":"ok","deleted":%q}`, target)
}

func backupHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		listBackupsHandler(w, r)
		return
	}
	if r.Method == http.MethodDelete {
		deleteBackupHandler(w, r)
		return
	}
	if r.Method != http.MethodPost {
		jsonError(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// 32MB in memory; larger spills to OS temp files automatically
	if err := r.ParseMultipartForm(32 << 20); err != nil {
		logger.Warn("failed to parse multipart form", "error", err)
		jsonError(w, "failed to parse form: "+err.Error(), http.StatusBadRequest)
		return
	}

	name := strings.TrimSpace(r.FormValue("name"))
	subdirectory := strings.TrimSpace(r.FormValue("subdirectory"))
	provider := strings.TrimSpace(r.FormValue("provider"))

	file, _, err := r.FormFile("backup")
	if err != nil {
		jsonError(w, "backup file is required", http.StatusBadRequest)
		return
	}
	defer file.Close()

	var errs []string
	for _, check := range [][2]string{{"name", name}, {"subdirectory", subdirectory}, {"provider", provider}} {
		if msg := internal.ValidateField(check[1], check[0]); msg != "" {
			errs = append(errs, msg)
		}
	}
	if len(errs) > 0 {
		jsonError(w, strings.Join(errs, "\n"), http.StatusBadRequest)
		return
	}

	if !authorize(w, r, provider, subdirectory, "create") {
		return
	}

	tmp, err := os.CreateTemp("", "backup-*.tar")
	if err != nil {
		logger.Error("failed to create temp file", "error", err)
		jsonError(w, "failed to create temp file: "+err.Error(), http.StatusInternalServerError)
		return
	}
	defer os.Remove(tmp.Name())

	written, err := io.Copy(tmp, file)
	if err != nil {
		tmp.Close()
		logger.Error("failed to write upload", "error", err)
		jsonError(w, "failed to write upload: "+err.Error(), http.StatusInternalServerError)
		return
	}
	tmp.Close()

	destination := provider + ":" + filepath.Join(subdirectory, name)
	logger.Info("uploading backup", "destination", destination, "size_bytes", written)
	out, err := exec.Command("rclone", "copyto", tmp.Name(), destination).CombinedOutput()
	if err != nil {
		rcloneError(w, out, err)
		return
	}

	logger.Info("backup uploaded", "destination", destination, "size_bytes", written)
	w.Header().Set("Content-Type", "application/json")
	fmt.Fprintf(w, `{"status":"ok","destination":%q}`, destination)
}

func healthHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Write([]byte(`{"status":"ok"}`))
}

func cmdUpload(args []string) error {
	if len(args) != 3 {
		return fmt.Errorf("usage: backio upload <provider> <subdirectory> <name>")
	}
	provider, subdirectory, name := args[0], args[1], args[2]

	var errs []string
	for _, check := range [][2]string{{"provider", provider}, {"subdirectory", subdirectory}, {"name", name}} {
		if msg := internal.ValidateField(check[1], check[0]); msg != "" {
			errs = append(errs, msg)
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("%s", strings.Join(errs, "\n"))
	}

	tmp, err := os.CreateTemp("", "backup-*.tar")
	if err != nil {
		return fmt.Errorf("failed to create temp file: %w", err)
	}
	defer os.Remove(tmp.Name())

	if _, err := io.Copy(tmp, os.Stdin); err != nil {
		tmp.Close()
		return fmt.Errorf("failed to read stdin: %w", err)
	}
	tmp.Close()

	destination := provider + ":" + filepath.Join(subdirectory, name)
	out, err := exec.Command("rclone", "copyto", tmp.Name(), destination).CombinedOutput()
	if err != nil {
		msg := strings.TrimSpace(string(out))
		if msg == "" {
			msg = err.Error()
		}
		return fmt.Errorf("rclone failed: %s", msg)
	}

	fmt.Printf("uploaded to %s\n", destination)
	return nil
}

func cmdDelete(args []string) error {
	if len(args) != 3 {
		return fmt.Errorf("usage: backio delete <provider> <subdirectory> <name>")
	}
	provider, subdirectory, name := args[0], args[1], args[2]

	var errs []string
	for _, check := range [][2]string{{"provider", provider}, {"subdirectory", subdirectory}, {"name", name}} {
		if msg := internal.ValidateField(check[1], check[0]); msg != "" {
			errs = append(errs, msg)
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("%s", strings.Join(errs, "\n"))
	}

	target := provider + ":" + filepath.Join(subdirectory, name)
	out, err := exec.Command("rclone", "deletefile", target).CombinedOutput()
	if err != nil {
		msg := strings.TrimSpace(string(out))
		if msg == "" {
			msg = err.Error()
		}
		return fmt.Errorf("rclone failed: %s", msg)
	}

	fmt.Printf("deleted %s\n", target)
	return nil
}

func cmdList(args []string) error {
	if len(args) != 2 {
		return fmt.Errorf("usage: backio list <provider> <subdirectory>")
	}
	provider, subdirectory := args[0], args[1]

	var errs []string
	for _, check := range [][2]string{{"provider", provider}, {"subdirectory", subdirectory}} {
		if msg := internal.ValidateField(check[1], check[0]); msg != "" {
			errs = append(errs, msg)
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("%s", strings.Join(errs, "\n"))
	}

	target := provider + ":" + subdirectory
	out, err := exec.Command("rclone", "lsjson", target).CombinedOutput()
	if err != nil {
		msg := strings.TrimSpace(string(out))
		if msg == "" {
			msg = err.Error()
		}
		return fmt.Errorf("rclone failed: %s", msg)
	}

	os.Stdout.Write(out)
	return nil
}

func cmdDownload(args []string) error {
	if len(args) != 3 {
		return fmt.Errorf("usage: backio download <provider> <subdirectory> <name>")
	}
	provider, subdirectory, name := args[0], args[1], args[2]

	var errs []string
	for _, check := range [][2]string{{"provider", provider}, {"subdirectory", subdirectory}, {"name", name}} {
		if msg := internal.ValidateField(check[1], check[0]); msg != "" {
			errs = append(errs, msg)
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("%s", strings.Join(errs, "\n"))
	}

	source := provider + ":" + filepath.Join(subdirectory, name)
	cmd := exec.Command("rclone", "cat", source)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("rclone failed: %s", err.Error())
	}
	return nil
}

func main() {
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "upload":
			if err := cmdUpload(os.Args[2:]); err != nil {
				fmt.Fprintln(os.Stderr, err)
				os.Exit(1)
			}
		case "download":
			if err := cmdDownload(os.Args[2:]); err != nil {
				fmt.Fprintln(os.Stderr, err)
				os.Exit(1)
			}
		case "delete":
			if err := cmdDelete(os.Args[2:]); err != nil {
				fmt.Fprintln(os.Stderr, err)
				os.Exit(1)
			}
		case "list":
			if err := cmdList(os.Args[2:]); err != nil {
				fmt.Fprintln(os.Stderr, err)
				os.Exit(1)
			}
		case "issue-token":
			if err := internal.CmdIssueToken(os.Args[2:]); err != nil {
				fmt.Fprintln(os.Stderr, err)
				os.Exit(1)
			}
		case "list-tokens":
			if err := internal.CmdListTokens(); err != nil {
				fmt.Fprintln(os.Stderr, err)
				os.Exit(1)
			}
		case "delete-token":
			token := ""
			if len(os.Args) > 2 {
				token = os.Args[2]
			}
			if err := internal.CmdDeleteToken(token); err != nil {
				fmt.Fprintln(os.Stderr, err)
				os.Exit(1)
			}
		default:
			fmt.Fprintf(os.Stderr, "unknown command %q\n", os.Args[1])
			fmt.Fprintln(os.Stderr, "commands: upload, download, delete, list, issue-token, list-tokens, delete-token")
			os.Exit(1)
		}
		return
	}

	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/health", healthHandler)
	mux.HandleFunc("/backup", backupHandler)

	logger.Info("server starting", "port", port)
	if err := http.ListenAndServe(":"+port, logRequests(mux)); err != nil {
		logger.Error("server stopped", "error", err)
		os.Exit(1)
	}
}
