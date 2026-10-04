//go:build failpoint

package failpoint

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// Compiled reports whether failpoints are compiled in.
const Compiled = true

var (
	once   sync.Once
	points map[string]string
	mu     sync.Mutex
)

func load() {
	points = map[string]string{}
	for _, p := range strings.Split(os.Getenv("TK_FAILPOINTS"), ";") {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		name, mode, _ := strings.Cut(p, "=")
		if mode == "" {
			mode = "always"
		}
		points[name] = mode
	}
}

func marker(name string) string { return filepath.Join(os.TempDir(), "failpoint."+name+".fired") }

// Enabled reports whether the failpoint fires now.
func Enabled(name string) bool {
	once.Do(load)
	mode, ok := points[name]
	if !ok {
		return false
	}
	if mode != "once" {
		return true
	}
	mu.Lock()
	defer mu.Unlock()
	if _, err := os.Stat(marker(name)); err == nil {
		return false
	}
	_ = os.WriteFile(marker(name), []byte("1"), 0o644)
	return true
}

// Active lists configured failpoints (logged at startup).
func Active() []string {
	once.Do(load)
	var out []string
	for k, v := range points {
		out = append(out, k+"="+v)
	}
	return out
}
