package main

import (
	"backio/internal"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

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
		jsonError(w, "authorization required", http.StatusUnauthorized)
		return false
	}
	ok, err := internal.CheckToken(token, provider, subdirectory, permission)
	if err != nil {
		jsonError(w, "failed to verify token: "+err.Error(), http.StatusInternalServerError)
		return false
	}
	if !ok {
		jsonError(w, "forbidden", http.StatusForbidden)
		return false
	}
	return true
}

func rcloneError(w http.ResponseWriter, out []byte, err error) {
	msg := strings.TrimSpace(string(out))
	if msg == "" {
		msg = err.Error()
	}
	jsonError(w, "rclone failed: "+msg, http.StatusInternalServerError)
}

func listBackupsHandler(w http.ResponseWriter, r *http.Request) {
	subdirectory := strings.TrimSpace(r.URL.Query().Get("subdirectory"))
	provider     := strings.TrimSpace(r.URL.Query().Get("provider"))

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
	out, err := exec.Command("rclone", "lsjson", target).CombinedOutput()
	if err != nil {
		rcloneError(w, out, err)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.Write(out)
}

func deleteBackupHandler(w http.ResponseWriter, r *http.Request) {
	name         := strings.TrimSpace(r.URL.Query().Get("name"))
	subdirectory := strings.TrimSpace(r.URL.Query().Get("subdirectory"))
	provider     := strings.TrimSpace(r.URL.Query().Get("provider"))

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
	out, err := exec.Command("rclone", "deletefile", target).CombinedOutput()
	if err != nil {
		rcloneError(w, out, err)
		return
	}

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
		jsonError(w, "failed to parse form: "+err.Error(), http.StatusBadRequest)
		return
	}

	name         := strings.TrimSpace(r.FormValue("name"))
	subdirectory := strings.TrimSpace(r.FormValue("subdirectory"))
	provider     := strings.TrimSpace(r.FormValue("provider"))

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
		jsonError(w, "failed to create temp file: "+err.Error(), http.StatusInternalServerError)
		return
	}
	defer os.Remove(tmp.Name())

	if _, err := io.Copy(tmp, file); err != nil {
		tmp.Close()
		jsonError(w, "failed to write upload: "+err.Error(), http.StatusInternalServerError)
		return
	}
	tmp.Close()

	destination := provider + ":" + filepath.Join(subdirectory, name)
	out, err := exec.Command("rclone", "copyto", tmp.Name(), destination).CombinedOutput()
	if err != nil {
		rcloneError(w, out, err)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	fmt.Fprintf(w, `{"status":"ok","destination":%q}`, destination)
}

func healthHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Write([]byte(`{"status":"ok"}`))
}

func main() {
	if len(os.Args) > 1 {
		switch os.Args[1] {
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
		default:
			fmt.Fprintf(os.Stderr, "unknown command %q\n", os.Args[1])
			fmt.Fprintln(os.Stderr, "commands: issue-token, list-tokens")
			os.Exit(1)
		}
		return
	}

	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}

	http.HandleFunc("/health", healthHandler)
	http.HandleFunc("/backup", backupHandler)
	log.Printf("listening on :%s", port)
	log.Fatal(http.ListenAndServe(":"+port, nil))
}
