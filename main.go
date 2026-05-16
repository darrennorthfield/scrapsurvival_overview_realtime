package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
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
	serverPort  int

	cells       = make(map[[2]int]Cell)
	cellsMu     sync.RWMutex
	cellsSeed   string
	cellsTotal  int
	cellsLoaded bool   // true once we've seen SMOVERVIEW_CELLS_END
	pendingCells map[[2]int]Cell // accumulator between BEGIN and END
)

type Cell struct {
	X    int    `json:"x"`
	Y    int    `json:"y"`
	Tile string `json:"t,omitempty"`
}

var (
	posLineRE       = regexp.MustCompile(`SMOVERVIEW_POS:(\[.*\])`)
	cellsBeginRE    = regexp.MustCompile(`SMOVERVIEW_CELLS_BEGIN:(\{.*\})`)
	cellsBatchRE    = regexp.MustCompile(`SMOVERVIEW_CELLS:(\[.*\])`)
	cellsEndRE      = regexp.MustCompile(`SMOVERVIEW_CELLS_END`)
)

func main() {
	smPath := flag.String("sm-path", defaultSMPath, "Scrap Mechanic install directory")
	port := flag.Int("port", defaultPort, "HTTP server port")
	noOpen := flag.Bool("no-open-browser", false, "skip auto-opening the browser")
	flag.Parse()

	serverPort = *port
	logsDir = filepath.Join(*smPath, "Logs")
	if info, err := os.Stat(logsDir); err != nil || !info.IsDir() {
		fmt.Fprintf(os.Stderr, "Could not find Scrap Mechanic logs directory at:\n  %s\n\nPass --sm-path \"<path-to-Scrap Mechanic>\" if your install is elsewhere.\n", logsDir)
		os.Exit(1)
	}

	go tailLogs()

	http.HandleFunc("/positions", handlePositions)
	http.HandleFunc("/cells", handleCells)
	http.HandleFunc("/health", handleHealth)
	http.HandleFunc("/info", handleInfo)
	http.HandleFunc("/", handleIndex)

	// Bind to 0.0.0.0 so other machines on the LAN can connect (viewer mode).
	// Anyone on your network with the URL can see player positions — fine for
	// the "playing with one friend on the same Wi-Fi" use case.
	addr := fmt.Sprintf(":%d", *port)
	localURL := fmt.Sprintf("http://127.0.0.1:%d", *port)
	fmt.Printf("sm_overview realtime — open %s in your browser\n", localURL)
	fmt.Printf("tailing logs in %s\n", logsDir)
	if lanIPs := lanIPv4s(); len(lanIPs) > 0 {
		fmt.Printf("LAN access for other players in your game:\n")
		for _, ip := range lanIPs {
			fmt.Printf("  http://%s:%d\n", ip, *port)
		}
	}

	if !*noOpen {
		go func() {
			time.Sleep(500 * time.Millisecond)
			openBrowser(localURL)
		}()
	}

	if err := http.ListenAndServe(addr, nil); err != nil {
		log.Fatal(err)
	}
}

// lanIPv4s returns this machine's non-loopback IPv4 addresses (RFC1918 ranges
// + link-local). One of these is what a friend types into the "Connect to host"
// field on another PC on the same network.
func lanIPv4s() []string {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return nil
	}
	var out []string
	for _, a := range addrs {
		ipnet, ok := a.(*net.IPNet)
		if !ok || ipnet.IP.IsLoopback() {
			continue
		}
		ip4 := ipnet.IP.To4()
		if ip4 == nil {
			continue
		}
		out = append(out, ip4.String())
	}
	return out
}

// tailLogs continuously follows the newest game-*.log in the Logs directory.
// When Scrap Mechanic rolls to a new log file, we detect it and switch.
//
// Tracks the read offset manually and Seeks to it before every Read because
// once os.File.Read returns io.EOF on macOS the file handle stays "sticky" at
// EOF and ignores subsequently appended bytes. An explicit seek resets that.
// (Linux is more forgiving but the seek is a cheap no-op there.)
func tailLogs() {
	var currentPath string
	var currentFile *os.File
	var offset int64
	var carry []byte // partial line left over from the previous read
	buf := make([]byte, 8192)

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
			// First file we ever open — skip to EOF so we ignore stale lines
			// from before the binary started. Subsequent rollovers (currentPath
			// already set) read from the beginning so we don't miss the early
			// SurvivalGame.server_onCreate lines of the new session.
			if currentPath == "" {
				offset, _ = f.Seek(0, io.SeekEnd)
			} else {
				offset = 0
			}
			currentFile = f
			currentPath = newest
			carry = nil
			log.Printf("tailing %s", filepath.Base(newest))
		}

		if currentFile != nil {
			for {
				if _, err := currentFile.Seek(offset, io.SeekStart); err != nil {
					log.Printf("seek: %v", err)
					break
				}
				n, err := currentFile.Read(buf)
				if n > 0 {
					offset += int64(n)
					carry = append(carry, buf[:n]...)
					for {
						i := bytesIndex(carry, '\n')
						if i < 0 {
							break
						}
						handleLine(string(carry[:i]))
						carry = carry[i+1:]
					}
				}
				if err == io.EOF || n == 0 {
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

func bytesIndex(b []byte, c byte) int {
	for i, v := range b {
		if v == c {
			return i
		}
	}
	return -1
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
	if m := posLineRE.FindStringSubmatch(line); m != nil {
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
		return
	}

	if m := cellsBeginRE.FindStringSubmatch(line); m != nil {
		var header struct {
			Seed  json.Number `json:"seed"`
			Count int         `json:"count"`
		}
		_ = json.Unmarshal([]byte(m[1]), &header)
		cellsMu.Lock()
		cellsSeed = string(header.Seed)
		cellsTotal = header.Count
		pendingCells = make(map[[2]int]Cell, header.Count)
		cellsLoaded = false
		cellsMu.Unlock()
		log.Printf("cells dump starting: seed=%s count=%d", cellsSeed, cellsTotal)
		return
	}

	if m := cellsBatchRE.FindStringSubmatch(line); m != nil {
		var batch []Cell
		if err := json.Unmarshal([]byte(m[1]), &batch); err != nil {
			log.Printf("bad SMOVERVIEW_CELLS batch: %v", err)
			return
		}
		cellsMu.Lock()
		if pendingCells == nil {
			// Stray batch with no BEGIN seen yet (mid-stream restart) — start fresh.
			pendingCells = make(map[[2]int]Cell)
		}
		for _, c := range batch {
			pendingCells[[2]int{c.X, c.Y}] = c
		}
		cellsMu.Unlock()
		return
	}

	if cellsEndRE.MatchString(line) {
		cellsMu.Lock()
		if pendingCells != nil {
			cells = pendingCells
			pendingCells = nil
			cellsLoaded = true
		}
		n := len(cells)
		cellsMu.Unlock()
		log.Printf("cells dump complete: %d cells", n)
		return
	}
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

	// LAN viewer-mode: allow browsers on other machines to fetch this.
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(resp)
}

func handleHealth(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Content-Type", "application/json")
	fmt.Fprintf(w, `{"ok":true,"logsDir":%q}`, logsDir)
}

func handleCells(w http.ResponseWriter, _ *http.Request) {
	cellsMu.RLock()
	out := struct {
		Loaded bool   `json:"loaded"`
		Seed   string `json:"seed,omitempty"`
		Count  int    `json:"count"`
		Cells  []Cell `json:"cells"`
	}{
		Loaded: cellsLoaded,
		Seed:   cellsSeed,
		Count:  len(cells),
		Cells:  make([]Cell, 0, len(cells)),
	}
	for _, c := range cells {
		out.Cells = append(out.Cells, c)
	}
	cellsMu.RUnlock()

	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Content-Type", "application/json")
	// Cells are static for a session — let the browser cache briefly.
	w.Header().Set("Cache-Control", "max-age=60")
	_ = json.NewEncoder(w).Encode(out)
}

func handleInfo(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(struct {
		LANIPs []string `json:"lan_ips"`
		Port   int      `json:"port"`
	}{LANIPs: lanIPv4s(), Port: serverPort})
}

func handleIndex(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprint(w, indexHTML)
}

// Leaflet-based map with realtime player markers.
// Uses CRS.Simple so we can plot SM world coords directly without a lat/lon projection.
// Negates y so north points up on screen (SM y grows northward).
const indexHTML = `<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1.0, user-scalable=no">
<title>sm_overview realtime</title>
<link rel="stylesheet" href="https://unpkg.com/leaflet@1.9.4/dist/leaflet.css"
      integrity="sha256-p4NxAoJBhIIN+hmNHrzRCf9tD/miZyoHS5obTRR9BMY="
      crossorigin="">
<style>
  html, body { height: 100%; margin: 0; padding: 0; background: #14181d; color: #e4e4e4;
               font-family: ui-monospace, SFMono-Regular, Menlo, monospace; }
  #map { width: 100%; height: 100%; background: #1c2128; }
  .leaflet-container { background: #1c2128; outline: none; }
  #panel {
    position: fixed; top: 12px; right: 12px; z-index: 1000;
    background: rgba(20,24,29,0.92); padding: 10px 14px; border-radius: 8px;
    font-size: 13px; min-width: 240px; max-width: 280px; box-shadow: 0 4px 12px rgba(0,0,0,0.4);
  }
  #panel h1 { margin: 0 0 6px 0; font-size: 13px; font-weight: 600; letter-spacing: 0.5px; }
  #status { color: #aaa; font-size: 12px; margin-bottom: 6px; }
  #players { font-size: 12px; }
  #players .row { display: flex; align-items: center; gap: 6px; margin-top: 3px; }
  #players .dot { width: 10px; height: 10px; border-radius: 50%; border: 1.5px solid #fff; }
  .sep { height: 1px; background: #333; margin: 10px -14px; }
  .field { margin-top: 4px; font-size: 11px; color: #aaa; }
  .field label { display: block; margin-bottom: 3px; }
  .field input {
    width: 100%; box-sizing: border-box; background: #0d1117; color: #e4e4e4;
    border: 1px solid #333; border-radius: 4px; padding: 4px 6px; font: 12px ui-monospace, monospace;
  }
  .field input:focus { outline: 1px solid #6cb6ff; border-color: #6cb6ff; }
  .ip-list { font: 12px ui-monospace, monospace; color: #6cb6ff; line-height: 1.5; }
  .ip-list .copy { cursor: pointer; user-select: all; }
  .ip-list .copy:hover { text-decoration: underline; }
  .player-tooltip {
    background: rgba(0,0,0,0.78) !important; color: #fff !important;
    border: none !important; box-shadow: none !important;
    font: 11px ui-monospace, monospace !important; padding: 2px 6px !important;
  }
  .player-tooltip::before { display: none !important; }
</style>
</head>
<body>
<div id="map"></div>
<div id="panel">
  <h1>sm_overview realtime</h1>
  <div id="status">connecting…</div>
  <div id="players"></div>
  <div class="sep"></div>
  <div class="field">
    <label>Share with friends on your network:</label>
    <div id="lan-ips" class="ip-list">…</div>
  </div>
  <div class="field" style="margin-top:8px">
    <label>Connect to host (leave blank for local):</label>
    <input id="host-input" type="text" placeholder="e.g. 192.168.1.42:7777" autocomplete="off" spellcheck="false">
  </div>
</div>
<script src="https://unpkg.com/leaflet@1.9.4/dist/leaflet.js"
        integrity="sha256-20nQCchB9co0qIjJZRGuk2/Z9VM+kNiyxNV1lvTlZBo="
        crossorigin=""></script>
<script>
const COLORS = ['#6cb6ff','#ffae57','#ff7ab2','#a98cff','#7be6c4','#ffd965','#ff7878','#9be888'];
const colorFor = id => COLORS[id % COLORS.length];

const map = L.map('map', {
  crs: L.CRS.Simple,
  minZoom: -4, maxZoom: 4, zoomSnap: 0.25, zoomDelta: 0.5,
  attributionControl: false,
  preferCanvas: true,
});
map.setView([0, 0], -1);

// Background grid: 64-unit lines (SM cells are 64) + thicker every 512.
(function drawGrid() {
  const range = 4096, step = 64, big = 512;
  const minor = [], major = [];
  for (let i = -range; i <= range; i += step) {
    const arr = (i % big === 0) ? major : minor;
    arr.push([[-range, i], [range, i]]);
    arr.push([[i, -range], [i, range]]);
  }
  L.polyline(minor.flat(), { color: '#222a33', weight: 0.5, interactive: false }).addTo(map);
  L.polyline(major.flat(), { color: '#33404d', weight: 0.8, interactive: false }).addTo(map);
})();

// --- Terrain layer ---------------------------------------------------------
// Cells dump comes in as 16k+ records (one per SM cell) with a tile UUID.
// We don't know the UUID→biome mapping yet, so hash the UUID to a colour and
// paint a tiny canvas (8px per cell), then drape it on the map as an image
// overlay. Cheap to render even for huge worlds, sharp at high zooms.
function colorForTile(uuid) {
  if (!uuid) return '#262c33';
  let h = 5381;
  for (let i = 0; i < uuid.length; i++) {
    h = ((h << 5) + h + uuid.charCodeAt(i)) | 0;
  }
  const hue = (Math.abs(h) * 37) % 360;
  // Two slight lightness bands so adjacent same-tile clusters have some
  // texture rather than being flat. Picked from the high bits.
  const light = 24 + ((Math.abs(h) >> 8) % 12);
  return 'hsl(' + hue + ',32%,' + light + '%)';
}

let terrainOverlay = null;
let terrainLoadedFor = '';   // host:cellsLoaded snapshot key — avoid re-rendering identical data

async function loadTerrain() {
  try {
    const r = await fetch(endpoint('/cells'));
    const data = await r.json();
    if (!data.loaded || !data.cells || data.cells.length === 0) return;
    const key = host + '|' + data.seed + '|' + data.count;
    if (key === terrainLoadedFor) return;
    terrainLoadedFor = key;
    renderTerrain(data.cells);
  } catch (e) { /* probably not running yet */ }
}

function renderTerrain(cellArr) {
  let minX = Infinity, maxX = -Infinity, minY = Infinity, maxY = -Infinity;
  for (const c of cellArr) {
    if (c.x < minX) minX = c.x;
    if (c.x > maxX) maxX = c.x;
    if (c.y < minY) minY = c.y;
    if (c.y > maxY) maxY = c.y;
  }
  const scale = 8; // canvas pixels per SM cell
  const w = (maxX - minX + 1) * scale;
  const h = (maxY - minY + 1) * scale;
  const cv = document.createElement('canvas');
  cv.width = w; cv.height = h;
  const ctx = cv.getContext('2d');
  ctx.imageSmoothingEnabled = false;
  ctx.fillStyle = '#1c2128';
  ctx.fillRect(0, 0, w, h);
  for (const c of cellArr) {
    ctx.fillStyle = colorForTile(c.t);
    const px = (c.x - minX) * scale;
    const py = (maxY - c.y) * scale; // flip y so north is up on the canvas
    ctx.fillRect(px, py, scale, scale);
  }

  // Convert cell-grid bounds to world-space, then to Leaflet CRS.Simple (lat=-y).
  const worldMinX = minX * 64, worldMaxX = (maxX + 1) * 64;
  const worldMinY = minY * 64, worldMaxY = (maxY + 1) * 64;
  const bounds = [[-worldMaxY, worldMinX], [-worldMinY, worldMaxX]];

  if (terrainOverlay) map.removeLayer(terrainOverlay);
  terrainOverlay = L.imageOverlay(cv.toDataURL(), bounds, {
    opacity: 0.85, interactive: false,
  }).addTo(map);
  terrainOverlay.bringToBack();
}

loadTerrain();
setInterval(loadTerrain, 30000);
// Also reload terrain when the host changes (LAN viewer mode).
// (The hostInput change handler below already triggers refresh(); we hook into
// it by re-running loadTerrain() whenever host changes.)

const markers = {}; // id -> L.marker
let zoomedToInitial = false;

function smToLatLng(p) {
  // CRS.Simple: lat = y axis (we negate so north=up), lng = x axis.
  return [-p.y, p.x];
}

function makeIcon(p) {
  return L.divIcon({
    className: '',
    html: '<div style="width:14px;height:14px;border-radius:50%;background:' + colorFor(p.id) +
          ';border:2px solid #fff;box-shadow:0 0 6px rgba(0,0,0,0.7);"></div>',
    iconSize: [14, 14], iconAnchor: [7, 7],
  });
}

function renderPanel(players, ageSec) {
  const statusEl = document.getElementById('status');
  const listEl = document.getElementById('players');
  if (!players.length) {
    statusEl.textContent = '🔴 no data — is Scrap Mechanic running and in a Survival save?';
    listEl.innerHTML = '';
    return;
  }
  const tag = ageSec <= 2 ? '🟢 live' : (ageSec < 10 ? '🟡 stale (' + ageSec + 's)' : '🔴 stale (' + ageSec + 's)');
  statusEl.textContent = tag + ' · ' + players.length + ' player' + (players.length === 1 ? '' : 's');
  listEl.innerHTML = players.map(p =>
    '<div class="row"><span class="dot" style="background:' + colorFor(p.id) + '"></span>' +
    '<span>' + escapeHtml(p.name) + '</span>' +
    '<span style="color:#777;margin-left:auto">' + p.x.toFixed(0) + ',' + p.y.toFixed(0) + '</span></div>'
  ).join('');
}

function escapeHtml(s) {
  return String(s).replace(/[&<>"']/g, c => ({'&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;',"'":'&#39;'})[c]);
}

// Host selection: empty string = use this .exe's own /positions; otherwise
// fetch from http://<that>/positions (your friend's machine on the LAN).
let host = (localStorage.getItem('sm_overview_host') || '').trim();

function endpoint(path) {
  if (!host) return path;
  const h = /^https?:\/\//.test(host) ? host : 'http://' + host;
  return h.replace(/\/$/, '') + path;
}

(async function loadInfo() {
  try {
    const r = await fetch('/info');
    const info = await r.json();
    const ips = (info.lan_ips || []);
    const port = info.port || 7777;
    document.getElementById('lan-ips').innerHTML = ips.length === 0
      ? '<span style="color:#888">(no LAN IPs detected)</span>'
      : ips.map(ip => '<span class="copy">' + ip + ':' + port + '</span>').join('<br>');
  } catch (e) { /* ignore */ }
})();

const hostInput = document.getElementById('host-input');
hostInput.value = host;
hostInput.addEventListener('change', () => {
  host = hostInput.value.trim();
  localStorage.setItem('sm_overview_host', host);
  // Wipe markers and terrain so we don't keep stale data from the previous source.
  for (const id of Object.keys(markers)) {
    map.removeLayer(markers[id]);
    delete markers[id];
  }
  if (terrainOverlay) { map.removeLayer(terrainOverlay); terrainOverlay = null; }
  terrainLoadedFor = '';
  zoomedToInitial = false;
  refresh();
  loadTerrain();
});

async function refresh() {
  try {
    const r = await fetch(endpoint('/positions'));
    const data = await r.json();
    const players = data.players || [];
    renderPanel(players, data.age_seconds || 0);

    const seen = new Set();
    for (const p of players) {
      seen.add(p.id);
      if (!markers[p.id]) {
        const m = L.marker(smToLatLng(p), { icon: makeIcon(p) }).addTo(map);
        m.bindTooltip(p.name, { permanent: true, direction: 'top', offset: [0, -8], className: 'player-tooltip' });
        markers[p.id] = m;
      } else {
        markers[p.id].setLatLng(smToLatLng(p));
      }
    }
    for (const id of Object.keys(markers)) {
      if (!seen.has(parseInt(id, 10))) {
        map.removeLayer(markers[id]);
        delete markers[id];
      }
    }

    if (!zoomedToInitial && players.length > 0) {
      map.setView(smToLatLng(players[0]), 1);
      zoomedToInitial = true;
    }
  } catch (e) {
    document.getElementById('status').textContent = 'error: ' + e.message;
  }
}

refresh();
setInterval(refresh, 1000);
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
