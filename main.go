package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	defaultSMPath = `C:\Program Files (x86)\Steam\steamapps\common\Scrap Mechanic`
	defaultPort   = 7777
)

type Player struct {
	ID   int     `json:"id"`
	Name string  `json:"name"`
	X    float64 `json:"x"`
	Y    float64 `json:"y"`
	Z    float64 `json:"z"`
}

var (
	positions   = make(map[int]Player)
	positionsMu sync.RWMutex
	updatedAt   time.Time
	logsDir     string
)

var posLineRE = regexp.MustCompile(`SMOVERVIEW_POS:(\[.*\])`)

func main() {
	smPath := flag.String("sm-path", defaultSMPath, "Scrap Mechanic install directory")
	port := flag.Int("port", defaultPort, "HTTP server port")
	noOpen := flag.Bool("no-open-browser", false, "skip auto-opening the browser")
	flag.Parse()

	logsDir = filepath.Join(*smPath, "Logs")
	if info, err := os.Stat(logsDir); err != nil || !info.IsDir() {
		fmt.Fprintf(os.Stderr, "Could not find Scrap Mechanic logs directory at:\n  %s\n\nPass --sm-path \"<path-to-Scrap Mechanic>\" if your install is elsewhere.\n", logsDir)
		os.Exit(1)
	}

	go tailLogs()

	http.HandleFunc("/positions", handlePositions)
	http.HandleFunc("/health", handleHealth)
	http.HandleFunc("/", handleIndex)

	addr := fmt.Sprintf(":%d", *port)
	url := fmt.Sprintf("http://127.0.0.1:%d", *port)
	fmt.Printf("sm_overview realtime — serving %s\n", url)
	fmt.Printf("tailing logs in %s\n", logsDir)

	if !*noOpen {
		go func() {
			time.Sleep(500 * time.Millisecond)
			openBrowser(url)
		}()
	}

	if err := http.ListenAndServe(addr, nil); err != nil {
		log.Fatal(err)
	}
}

// tailLogs continuously follows the newest game-*.log in the Logs directory.
// When Scrap Mechanic rolls to a new log file, we detect it and switch.
func tailLogs() {
	var currentPath string
	var currentFile *os.File
	var reader *bufio.Reader

	for {
		newest := findNewestGameLog(logsDir)

		if newest != "" && newest != currentPath {
			if currentFile != nil {
				currentFile.Close()
			}
			f, err := os.Open(newest)
			if err != nil {
				log.Printf("open %s: %v", newest, err)
				time.Sleep(2 * time.Second)
				continue
			}
			// New session — start from beginning so we don't miss early lines.
			// (Old sessions: skip to end to avoid replaying stale positions.)
			if currentPath != "" {
				// switched to a newer file — read from start
				f.Seek(0, io.SeekStart)
			} else {
				// first open of newest existing file — skip ahead so we only
				// react to lines emitted after the binary starts
				f.Seek(0, io.SeekEnd)
			}
			currentFile = f
			currentPath = newest
			reader = bufio.NewReader(f)
			log.Printf("tailing %s", filepath.Base(newest))
		}

		if reader != nil {
			for {
				line, err := reader.ReadString('\n')
				if len(line) > 0 {
					handleLine(line)
				}
				if err == io.EOF {
					break
				}
				if err != nil {
					log.Printf("read: %v", err)
					break
				}
			}
		}

		time.Sleep(500 * time.Millisecond)
	}
}

func findNewestGameLog(dir string) string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return ""
	}
	type entryInfo struct {
		path    string
		modTime time.Time
	}
	var candidates []entryInfo
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasPrefix(name, "game-") || !strings.HasSuffix(name, ".log") {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		candidates = append(candidates, entryInfo{
			path:    filepath.Join(dir, name),
			modTime: info.ModTime(),
		})
	}
	if len(candidates) == 0 {
		return ""
	}
	sort.Slice(candidates, func(i, j int) bool {
		return candidates[i].modTime.After(candidates[j].modTime)
	})
	return candidates[0].path
}

func handleLine(line string) {
	m := posLineRE.FindStringSubmatch(line)
	if m == nil {
		return
	}
	var players []Player
	if err := json.Unmarshal([]byte(m[1]), &players); err != nil {
		log.Printf("bad SMOVERVIEW_POS payload: %v", err)
		return
	}
	positionsMu.Lock()
	// Each emit is authoritative for who is connected: replace, don't merge.
	positions = make(map[int]Player, len(players))
	for _, p := range players {
		positions[p.ID] = p
	}
	updatedAt = time.Now()
	positionsMu.Unlock()
}

type positionsResponse struct {
	Players      []Player `json:"players"`
	UpdatedAtSec int64    `json:"updated_at_unix"`
	AgeSeconds   int64    `json:"age_seconds"`
}

func handlePositions(w http.ResponseWriter, _ *http.Request) {
	positionsMu.RLock()
	resp := positionsResponse{
		Players: make([]Player, 0, len(positions)),
	}
	if !updatedAt.IsZero() {
		resp.UpdatedAtSec = updatedAt.Unix()
		resp.AgeSeconds = int64(time.Since(updatedAt).Seconds())
	}
	for _, p := range positions {
		resp.Players = append(resp.Players, p)
	}
	positionsMu.RUnlock()

	sort.Slice(resp.Players, func(i, j int) bool { return resp.Players[i].ID < resp.Players[j].ID })

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(resp)
}

func handleHealth(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	fmt.Fprintf(w, `{"ok":true,"logsDir":%q}`, logsDir)
}

func handleIndex(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprint(w, indexHTML)
}

// Minimal viewer: shows positions as JSON refreshed every second.
// Phase 3 replaces this with the Leaflet map UI.
const indexHTML = `<!doctype html>
<html>
<head>
<meta charset="utf-8">
<title>sm_overview realtime</title>
<style>
body { font-family: ui-monospace, monospace; padding: 24px; background: #1e1e1e; color: #e4e4e4; }
h1 { margin-top: 0; }
.card { background: #2a2a2a; border-radius: 6px; padding: 16px; margin-bottom: 12px; }
.muted { color: #999; font-size: 13px; }
pre { margin: 0; }
</style>
</head>
<body>
<h1>sm_overview realtime</h1>
<p class="muted">Phase 2 diagnostic UI. Phase 3 will replace this with the Leaflet map.</p>
<div class="card">
  <div id="status" class="muted">connecting…</div>
  <pre id="payload">{}</pre>
</div>
<script>
async function tick() {
  try {
    const r = await fetch('/positions');
    const data = await r.json();
    document.getElementById('payload').textContent = JSON.stringify(data, null, 2);
    const age = data.age_seconds;
    let status = age <= 2 ? '🟢 live' : (age < 10 ? '🟡 stale (' + age + 's)' : '🔴 no data — is Scrap Mechanic running and in a Survival save?');
    document.getElementById('status').textContent = status;
  } catch (e) {
    document.getElementById('status').textContent = 'error: ' + e;
  }
}
tick();
setInterval(tick, 1000);
</script>
</body>
</html>
`

func openBrowser(url string) {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	case "darwin":
		cmd = exec.Command("open", url)
	default:
		cmd = exec.Command("xdg-open", url)
	}
	if err := cmd.Start(); err != nil {
		log.Printf("could not open browser: %v", err)
	}
}
