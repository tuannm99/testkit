package evidence

import (
	"archive/zip"
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// Finding is a secret found in the evidence.
type Finding struct {
	Path string `json:"path"`
	Line int    `json:"line"`
	What string `json:"what"` // never the secret itself
}

// secretPatterns catch credentials whatever their value.
var secretPatterns = []struct {
	name string
	re   *regexp.Regexp
}{
	{"Authorization header with credentials", regexp.MustCompile(`(?i)authorization["']?\s*[:=]\s*["']?(bearer|basic)\s+[A-Za-z0-9._~+/=-]{8,}`)},
	{"private key", regexp.MustCompile(`-----BEGIN [A-Z ]*PRIVATE KEY-----`)},
	{"AWS access key", regexp.MustCompile(`\bAKIA[0-9A-Z]{16}\b`)},
}

// MinSecretLen is the shortest secret value the scan can look for reliably.
const MinSecretLen = 8

// ScanText reports secrets in a text (line numbers 1-based).
func ScanText(text string, secrets map[string]string) []Finding {
	var out []Finding
	for n, line := range strings.Split(text, "\n") {
		for name, v := range secrets {
			if len(v) >= MinSecretLen && strings.Contains(line, v) {
				out = append(out, Finding{Line: n + 1, What: "value of " + name})
			}
		}
		for _, sp := range secretPatterns {
			if sp.re.MatchString(line) {
				out = append(out, Finding{Line: n + 1, What: sp.name})
			}
		}
	}
	return out
}

// ScanSecrets looks for known secret values (exact match, at least 8
// characters) and credential patterns in every text file of the bundle.
// Shorter values cannot be told apart from ordinary words: callers report them.
func ScanSecrets(root string, secrets map[string]string) ([]Finding, error) {
	var out []Finding
	err := filepath.Walk(root, func(p string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return err
		}
		raw, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		if !isText(raw) {
			return nil
		}
		rel, _ := filepath.Rel(root, p)
		sc := bufio.NewScanner(bytes.NewReader(raw))
		sc.Buffer(make([]byte, 1<<20), 64<<20)
		for n := 1; sc.Scan(); n++ {
			line := sc.Text()
			for name, v := range secrets {
				if len(v) >= MinSecretLen && strings.Contains(line, v) {
					out = append(out, Finding{Path: filepath.ToSlash(rel), Line: n, What: "value of " + name})
				}
			}
			for _, sp := range secretPatterns {
				if sp.re.MatchString(line) {
					out = append(out, Finding{Path: filepath.ToSlash(rel), Line: n, What: sp.name})
				}
			}
		}
		return sc.Err()
	})
	return out, err
}

func isText(b []byte) bool {
	n := min(len(b), 8000)
	return bytes.IndexByte(b[:n], 0) < 0
}

// Pack zips a sealed run directory into zipPath after checking it still
// matches its manifest. Entries are sorted and timestamped with the
// manifest time, so packing the same bundle twice gives the same bytes.
// It returns the sha256 of the zip.
func Pack(root, zipPath string) (string, error) {
	problems, err := Verify(root)
	if err != nil {
		return "", err
	}
	if len(problems) > 0 {
		return "", fmt.Errorf("bundle does not match its manifest: %s", strings.Join(problems, "; "))
	}
	files, err := HashTree(root)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(filepath.Join(root, ManifestFile))
	if err != nil {
		return "", err
	}
	stamp := info.ModTime().UTC().Truncate(2 * time.Second)
	paths := []string{ManifestFile}
	for _, f := range files {
		paths = append(paths, f.Path)
	}
	tmp := zipPath + ".tmp"
	zf, err := os.Create(tmp)
	if err != nil {
		return "", err
	}
	h := sha256.New()
	zw := zip.NewWriter(io.MultiWriter(zf, h))
	prefix := filepath.Base(root) + "/"
	for _, rel := range paths {
		w, err := zw.CreateHeader(&zip.FileHeader{Name: prefix + rel, Method: zip.Deflate, Modified: stamp})
		if err != nil {
			zf.Close()
			return "", err
		}
		f, err := os.Open(filepath.Join(root, filepath.FromSlash(rel)))
		if err != nil {
			zf.Close()
			return "", err
		}
		_, err = io.Copy(w, f)
		f.Close()
		if err != nil {
			zf.Close()
			return "", err
		}
	}
	if err := zw.Close(); err != nil {
		zf.Close()
		return "", err
	}
	if err := zf.Close(); err != nil {
		return "", err
	}
	if err := os.Rename(tmp, zipPath); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
