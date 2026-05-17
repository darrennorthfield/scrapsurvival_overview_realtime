package main

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"
)

//go:embed SurvivalGame.lua
var patchedSurvivalGameLua []byte

//go:embed terrain_overworld.lua
var patchedTerrainLua []byte

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

// Tile UUID → biome / human label, populated once at startup from SM's .tile
// files. Label is only populated for landmark POIs (so the UI knows which to
// show as named markers); biome alone is enough to colour the cell.
type TileInfo struct {
	Biome string `json:"biome"`
	Label string `json:"label,omitempty"`
}

var (
	tileDB   = make(map[string]TileInfo)
	tileDBMu sync.RWMutex
)

func main() {
	smPath := flag.String("sm-path", defaultSMPath, "Scrap Mechanic install directory")
	port := flag.Int("port", defaultPort, "HTTP server port")
	noOpen := flag.Bool("no-open-browser", false, "skip auto-opening the browser")
	scanTiles := flag.Bool("scan-tiles", false, "inspect .tile files under <sm-path>/Survival/Terrain/Tiles and exit")
	noPatch := flag.Bool("no-patch", false, "skip automatic patch/restore of SM Lua files (advanced)")
	restoreOnly := flag.Bool("restore", false, "restore vanilla SM Lua files from backups and exit")
	flag.Parse()

	if *scanTiles {
		runTileScan(*smPath)
		return
	}
	if *restoreOnly {
		fmt.Println("Restoring vanilla Scrap Mechanic Lua files...")
		restoreAllPatches(*smPath)
		fmt.Println("Done.")
		return
	}

	serverPort = *port
	logsDir = filepath.Join(*smPath, "Logs")
	go loadTileDatabase(*smPath)

	// Apply patches and arrange for the vanilla files to be restored on any
	// exit path — signal handlers for Ctrl+C / window close / SIGTERM, plus a
	// defer that catches normal returns and panics (when paired with recover).
	if !*noPatch {
		if err := applyAllPatches(*smPath); err != nil {
			fmt.Fprintf(os.Stderr, "\nCould not patch Scrap Mechanic Lua files:\n  %v\n\n", err)
			fmt.Fprintln(os.Stderr, "If this looks like a permissions error, right-click smoverview.exe and choose 'Run as administrator'.")
			fmt.Fprintln(os.Stderr, "If you'd rather patch manually, re-run with --no-patch.")
			os.Exit(1)
		}
		defer restoreAllPatches(*smPath)

		sigCh := make(chan os.Signal, 1)
		signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
		go func() {
			<-sigCh
			fmt.Println("\nshutting down — restoring vanilla files...")
			restoreAllPatches(*smPath)
			fmt.Println("done.")
			os.Exit(0)
		}()
	}
	if info, err := os.Stat(logsDir); err != nil || !info.IsDir() {
		fmt.Fprintf(os.Stderr, "Could not find Scrap Mechanic logs directory at:\n  %s\n\nPass --sm-path \"<path-to-Scrap Mechanic>\" if your install is elsewhere.\n", logsDir)
		os.Exit(1)
	}

	go tailLogs()

	http.HandleFunc("/positions", handlePositions)
	http.HandleFunc("/cells", handleCells)
	http.HandleFunc("/tile-info", handleTileInfo)
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

func handleTileInfo(w http.ResponseWriter, _ *http.Request) {
	tileDBMu.RLock()
	out := make(map[string]TileInfo, len(tileDB))
	for k, v := range tileDB {
		out[k] = v
	}
	tileDBMu.RUnlock()
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "max-age=300")
	_ = json.NewEncoder(w).Encode(struct {
		Count int                 `json:"count"`
		Tiles map[string]TileInfo `json:"tiles"`
	}{Count: len(out), Tiles: out})
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
  .leaflet-image-layer { image-rendering: pixelated; image-rendering: crisp-edges; }
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
  .landmark-dot {
    width: 10px; height: 10px; border-radius: 50%;
    background: #ffd966; border: 2px solid #2a1a08;
    box-shadow: 0 0 4px rgba(0,0,0,0.6);
  }
  .landmark-md .landmark-dot { width: 14px; height: 14px; }
  .landmark-lg .landmark-dot { width: 18px; height: 18px; background: #ff9b4a; }
  .landmark-tooltip {
    background: rgba(20,10,0,0.92) !important; color: #ffe599 !important;
    border: 1px solid #44331a !important; box-shadow: none !important;
    font: 11px ui-monospace, monospace !important; padding: 2px 6px !important;
  }
  .landmark-tooltip::before { display: none !important; }
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
// We don't have a UUID→biome mapping, so do a frequency-based heuristic:
// the most common UUIDs in this world get curated map-themed colours
// (water/forest/meadow/field/desert in roughly that order — matches the
// usual distribution in SM where lakes dominate); the long tail of less
// common tiles falls back to a muted hash-based hue.
//
// Result: a recognisable map even without ground-truth biome info.

// Curated biome → colour palette. Biome names come from SM's own
// Survival/Terrain/Tiles/ subdirectory layout (loadTileDatabase on the Go
// side reads each .tile file's header UUID and pairs it with the parent
// directory name).
const BIOME_COLORS = {
  // Terrain biomes
  'lake':             '#2c4d6a',  // open water — blue
  'island':           '#5a7a55',  // small island poking out of lake — muted green
  'chemical_lake':    '#5a6a2c',  // acid pool — sickly yellow-green
  'forest':           '#2a4530',  // pine forest — dark green
  'autumn_forest':    '#7a5532',  // warm brown-orange
  'burnt_forest':     '#3d3a36',  // charcoal grey
  'meadow':           '#4d6a3a',  // mid grass green
  'field':            '#9a9f4a',  // crops — yellow-green
  'desert':           '#a89060',  // tan sand
  'road':             '#e8c89a',  // dirt road — bright tan
  'roads_and_cliffs': '#c4a878',  // generic road/cliff tile — pale tan (most paths in this category)
  // Landmarks (things you'd navigate to)
  'landmark':         '#c25555',  // POIs/ruins — bright red
  'start_area':       '#a85ac4',  // spawn — vivid purple
  // Fallback
  'unknown':          '#3a3a3a',
  'poi':              '#c25555',  // legacy key — same as landmark
};

let tileBiomeLookup = {}; // uuid -> biome name (fetched from /tile-info)

async function loadTileInfo() {
  try {
    const r = await fetch(endpoint('/tile-info'));
    const data = await r.json();
    if (data && data.tiles) tileBiomeLookup = data.tiles;
  } catch (e) { /* tile DB might still be scanning, retry handled by reloadTerrain */ }
}

function hashColorForTile(uuid) {
  if (!uuid) return BIOME_COLORS.unknown;
  let h = 5381;
  for (let i = 0; i < uuid.length; i++) h = ((h << 5) + h + uuid.charCodeAt(i)) | 0;
  const hue = (Math.abs(h) * 37) % 360;
  const light = 24 + ((Math.abs(h) >> 8) % 12);
  return 'hsl(' + hue + ',26%,' + light + '%)';
}

function colorForTile(uuid) {
  if (!uuid) return BIOME_COLORS.unknown;
  const info = tileBiomeLookup[uuid];
  if (info && info.biome && BIOME_COLORS[info.biome]) return BIOME_COLORS[info.biome];
  // Tile UUID not in DB yet (DB still loading, or tile is unknown).
  return hashColorForTile(uuid);
}

function labelForTile(uuid) {
  const info = tileBiomeLookup[uuid];
  return info && info.label ? info.label : null;
}

let terrainOverlay = null;
let terrainLoadedFor = '';   // host:cellsLoaded snapshot key — avoid re-rendering identical data
let landmarkMarkers = [];    // L.marker[]  — one per detected landmark cluster

function clearLandmarkMarkers() {
  for (const m of landmarkMarkers) map.removeLayer(m);
  landmarkMarkers = [];
}

// clusterLandmarks groups labeled cells by tile UUID, then finds contiguous
// (8-connected) clusters within each group so a 4x4 RuinCity gets one marker
// instead of 16. Returns one entry per cluster with its world-space center.
function clusterLandmarks(cellArr) {
  const byUuid = new Map();
  for (const c of cellArr) {
    const label = labelForTile(c.t);
    if (!label) continue;
    if (!byUuid.has(c.t)) byUuid.set(c.t, []);
    byUuid.get(c.t).push(c);
  }
  const clusters = [];
  for (const [uuid, group] of byUuid) {
    const label = tileBiomeLookup[uuid].label;
    const cellMap = new Map();
    for (const c of group) cellMap.set(c.x + ',' + c.y, c);
    const visited = new Set();
    for (const c of group) {
      const key = c.x + ',' + c.y;
      if (visited.has(key)) continue;
      const stack = [c];
      const component = [];
      while (stack.length) {
        const cur = stack.pop();
        const ck = cur.x + ',' + cur.y;
        if (visited.has(ck)) continue;
        visited.add(ck);
        component.push(cur);
        for (let dx = -1; dx <= 1; dx++) {
          for (let dy = -1; dy <= 1; dy++) {
            if (dx === 0 && dy === 0) continue;
            const nk = (cur.x + dx) + ',' + (cur.y + dy);
            if (!visited.has(nk) && cellMap.has(nk)) stack.push(cellMap.get(nk));
          }
        }
      }
      let cx = 0, cy = 0;
      for (const p of component) { cx += p.x; cy += p.y; }
      cx = cx / component.length + 0.5; // +0.5 to land on the cell center
      cy = cy / component.length + 0.5;
      clusters.push({
        worldX: cx * 64, worldY: cy * 64,
        label, size: component.length,
      });
    }
  }
  return clusters;
}

function renderLandmarks(cellArr) {
  clearLandmarkMarkers();
  const clusters = clusterLandmarks(cellArr);
  for (const c of clusters) {
    const klass = c.size >= 16 ? 'landmark-lg' : (c.size >= 4 ? 'landmark-md' : '');
    const icon = L.divIcon({
      className: klass,
      html: '<div class="landmark-dot"></div>',
      iconSize: [18, 18], iconAnchor: [9, 9],
    });
    // Leaflet CRS.Simple: lat=-y, lng=x
    const m = L.marker([-c.worldY, c.worldX], { icon, interactive: true }).addTo(map);
    // Permanent labels for medium and large landmarks so you can actually
    // navigate to a named POI without hovering every dot. Tiny 1-cell things
    // (kiosks, single ruins) stay hover-only to avoid clutter.
    m.bindTooltip(c.label, {
      direction: 'top', offset: [0, -8], className: 'landmark-tooltip',
      permanent: c.size >= 4,
    });
    landmarkMarkers.push(m);
  }
}

async function loadTerrain() {
  try {
    // Refresh the biome lookup first so any newly-loaded tile UUIDs colour right.
    await loadTileInfo();
    const r = await fetch(endpoint('/cells'));
    const data = await r.json();
    if (!data.loaded || !data.cells || data.cells.length === 0) return;
    // Include biome-lookup size in the cache key so we re-render if new biome
    // data becomes available even when the cells themselves haven't changed.
    const key = host + '|' + data.seed + '|' + data.count + '|' + Object.keys(tileBiomeLookup).length;
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
  const scale = 4; // canvas pixels per SM cell; CSS keeps it crisp when zoomed
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

  renderLandmarks(cellArr);
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
  clearLandmarkMarkers();
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

// loadTileDatabase scans <smPath>/Survival/Terrain/Tiles for .tile files,
// extracts each tile's UUID from the file header, and stores a UUID → biome
// (= first subdirectory under Tiles/) mapping. That mapping is what the map UI
// uses to colour cells with real biome colours instead of hash-based noise.
//
// .tile file format (discovered via --scan-tiles):
//   bytes 0..3:   ASCII "TILE"
//   bytes 4..7:   version uint32 (LE), seen as 9
//   bytes 8..23:  16-byte tile UUID, RFC-4122 byte order
//   bytes 24..:   tile content (we don't parse this)
func loadTileDatabase(smPath string) {
	tilesDir := filepath.Join(smPath, "Survival", "Terrain", "Tiles")
	info, err := os.Stat(tilesDir)
	if err != nil || !info.IsDir() {
		log.Printf("tile DB: %s not found, terrain colours will fall back to UUID hash", tilesDir)
		return
	}
	m := make(map[string]TileInfo)
	var skipped int
	err = filepath.Walk(tilesDir, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil
		}
		if !strings.HasSuffix(strings.ToLower(path), ".tile") {
			return nil
		}
		rfc, ms, ok := readTileUUID(path)
		if !ok {
			skipped++
			return nil
		}
		rel, _ := filepath.Rel(tilesDir, path)
		tinfo := classifyTile(rel)
		m[rfc] = tinfo
		m[ms] = tinfo
		return nil
	})
	if err != nil {
		log.Printf("tile DB walk: %v", err)
	}
	tileDBMu.Lock()
	tileDB = m
	tileDBMu.Unlock()
	// len(m) is ~2× the file count (RFC + MS layouts per tile); divide for the human-friendly number.
	log.Printf("tile DB loaded: ~%d tiles, %d biomes (%d skipped)", len(m)/2, countDistinctBiomes(m), skipped)
}

func countDistinctBiomes(m map[string]TileInfo) int {
	seen := make(map[string]struct{})
	for _, v := range m {
		seen[v.Biome] = struct{}{}
	}
	return len(seen)
}

// readTileUUID returns BOTH the RFC-4122 layout and the Microsoft mixed-endian
// layout of the 16-byte UUID at offset 8 in a .tile file. We populate the
// biome map under both keys so whichever string format SM produces when it
// runs tostring(uuid) in our cells dump, one of them will match.
// classifyTile maps a Tiles-relative path to a biome label used by the UI.
// The base biome is the top-level subdirectory (lake/, forest/, meadow/, …).
// Files under poi/ are heterogeneous though — they include both scenic biome
// fillers (Random_Lake_*, Random_Island_*, Random_Forest_*, Random_Meadow_*,
// Random_Road_*) and actual man-made landmarks (Ruin_*, Kiosk_*, Warehouse_*,
// CrashedShip_*, Hideout_*, MechanicStation_*, etc.). Splitting them keeps
// the "red" landmark colour reserved for things you'd actually navigate to.
func classifyTile(relPath string) TileInfo {
	rel := filepath.ToSlash(relPath)
	parts := strings.Split(rel, "/")
	if len(parts) < 2 {
		return TileInfo{Biome: "unknown"}
	}
	dir := parts[0]
	name := strings.TrimSuffix(parts[len(parts)-1], ".tile")
	if dir != "poi" {
		return TileInfo{Biome: dir}
	}
	lower := strings.ToLower(name)
	switch {
	case strings.HasPrefix(lower, "random_lake"):
		return TileInfo{Biome: "lake"}
	case strings.HasPrefix(lower, "random_island"):
		return TileInfo{Biome: "island"}
	case strings.HasPrefix(lower, "random_forest"):
		return TileInfo{Biome: "forest"}
	case strings.HasPrefix(lower, "random_meadow"):
		return TileInfo{Biome: "meadow"}
	case strings.HasPrefix(lower, "random_road"):
		return TileInfo{Biome: "road"}
	case strings.HasPrefix(lower, "chemicallake"):
		return TileInfo{Biome: "chemical_lake", Label: "Chemical Lake"}
	case strings.HasPrefix(lower, "farmingpatch"):
		return TileInfo{Biome: "field"}
	}
	// Actual landmark — humanize for the map label.
	return TileInfo{Biome: "landmark", Label: humanizeLandmark(name)}
}

// humanizeLandmark turns "MechanicStation_128_01" into "Mechanic Station",
// "Warehouse_Exterior_3Floors_256_01" into "Warehouse Exterior 3Floors", etc.
var (
	tileSizeSuffixRE = regexp.MustCompile(`_\d+(_\d+)?$`)
	camelCaseRE      = regexp.MustCompile(`([a-z0-9])([A-Z])`)
)

func humanizeLandmark(name string) string {
	name = tileSizeSuffixRE.ReplaceAllString(name, "")
	name = strings.ReplaceAll(name, "_", " ")
	name = camelCaseRE.ReplaceAllString(name, "$1 $2")
	return name
}

func readTileUUID(path string) (rfc, ms string, ok bool) {
	f, err := os.Open(path)
	if err != nil {
		return "", "", false
	}
	defer f.Close()
	var buf [24]byte
	n, _ := f.Read(buf[:])
	if n < 24 {
		return "", "", false
	}
	if string(buf[0:4]) != "TILE" {
		return "", "", false
	}
	u := buf[8:24]
	rfc = fmt.Sprintf("%02x%02x%02x%02x-%02x%02x-%02x%02x-%02x%02x-%02x%02x%02x%02x%02x%02x",
		u[0], u[1], u[2], u[3],
		u[4], u[5],
		u[6], u[7],
		u[8], u[9],
		u[10], u[11], u[12], u[13], u[14], u[15])
	// Microsoft layout: first three fields are stored little-endian, last two as-is.
	ms = fmt.Sprintf("%02x%02x%02x%02x-%02x%02x-%02x%02x-%02x%02x-%02x%02x%02x%02x%02x%02x",
		u[3], u[2], u[1], u[0],
		u[5], u[4],
		u[7], u[6],
		u[8], u[9],
		u[10], u[11], u[12], u[13], u[14], u[15])
	return rfc, ms, true
}

// --- Patch / restore engine ------------------------------------------------
//
// On startup we back up the user's vanilla Lua files, write our patched
// versions over them, and register a deferred restore so the originals come
// back when the .exe exits. SM stays in a clean state any time we're not
// actively running.

type Patch struct {
	Name        string // for logs
	RelGamePath string // relative to <sm-path>
	Data        []byte // patched file content embedded in the .exe
	Marker      string // unique substring proving a file is already our patched version
}

var allPatches = []Patch{
	{
		Name:        "SurvivalGame.lua",
		RelGamePath: filepath.Join("Survival", "Scripts", "game", "SurvivalGame.lua"),
		Data:        patchedSurvivalGameLua,
		Marker:      "SMOVERVIEW_POS",
	},
	{
		Name:        "terrain_overworld.lua",
		RelGamePath: filepath.Join("Survival", "Scripts", "terrain", "terrain_overworld.lua"),
		Data:        patchedTerrainLua,
		Marker:      "SMOVERVIEW_CELLS_BEGIN",
	},
}

func backupPathFor(gameFile string) string {
	return gameFile + ".smoverview-backup"
}

func applyAllPatches(smPath string) error {
	for _, p := range allPatches {
		if err := applyOnePatch(smPath, p); err != nil {
			return fmt.Errorf("%s: %w", p.Name, err)
		}
	}
	return nil
}

func applyOnePatch(smPath string, p Patch) error {
	gameFile := filepath.Join(smPath, p.RelGamePath)
	backupFile := backupPathFor(gameFile)

	current, err := os.ReadFile(gameFile)
	if err != nil {
		return fmt.Errorf("read %s: %w", gameFile, err)
	}
	if bytes.Contains(current, []byte(p.Marker)) {
		// Already our patched version — likely a crash on the previous run left
		// it that way. Don't double-patch; don't touch the (possibly missing)
		// backup either.
		if _, err := os.Stat(backupFile); err == nil {
			log.Printf("patch: %s already patched from previous session", p.Name)
		} else {
			log.Printf("patch: %s already patched but no backup found — leaving it; restore on exit will be a no-op", p.Name)
		}
		return nil
	}

	// Fresh vanilla file. Back it up first, then write the patched version.
	if err := os.WriteFile(backupFile, current, 0o644); err != nil {
		return fmt.Errorf("write backup %s: %w (try running as administrator)", backupFile, err)
	}
	if err := os.WriteFile(gameFile, p.Data, 0o644); err != nil {
		// Try to undo the backup-write if patch-write failed.
		_ = os.Remove(backupFile)
		return fmt.Errorf("write patched %s: %w (try running as administrator)", gameFile, err)
	}
	log.Printf("patch: %s patched (backup: %s)", p.Name, filepath.Base(backupFile))
	return nil
}

func restoreAllPatches(smPath string) {
	for _, p := range allPatches {
		if err := restoreOnePatch(smPath, p); err != nil {
			log.Printf("restore: %s: %v", p.Name, err)
		}
	}
}

func restoreOnePatch(smPath string, p Patch) error {
	gameFile := filepath.Join(smPath, p.RelGamePath)
	backupFile := backupPathFor(gameFile)

	data, err := os.ReadFile(backupFile)
	if err != nil {
		if os.IsNotExist(err) {
			return nil // nothing to restore — was already vanilla, or user is starting cleanly
		}
		return err
	}
	if err := os.WriteFile(gameFile, data, 0o644); err != nil {
		return err
	}
	if err := os.Remove(backupFile); err != nil {
		log.Printf("restore: %s restored but backup not removed: %v", p.Name, err)
	} else {
		log.Printf("restore: %s back to vanilla", p.Name)
	}
	return nil
}

// runTileScan walks <smPath>/Survival/Terrain/Tiles, reads the first 1 KB of
// each .tile file, and prints anything that might be a UUID or filename so
// we can figure out how SM stores the tile UUID. Used once to design the
// real UUID→biome lookup; not part of normal operation.
func runTileScan(smPath string) {
	tilesDir := filepath.Join(smPath, "Survival", "Terrain", "Tiles")
	info, err := os.Stat(tilesDir)
	if err != nil || !info.IsDir() {
		fmt.Fprintf(os.Stderr, "tiles dir not found: %s\n", tilesDir)
		os.Exit(1)
	}

	uuidRE := regexp.MustCompile(`[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}`)

	var allFiles []string
	_ = filepath.Walk(tilesDir, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil
		}
		if strings.HasSuffix(strings.ToLower(path), ".tile") {
			allFiles = append(allFiles, path)
		}
		return nil
	})

	fmt.Printf("Found %d .tile files under %s\n\n", len(allFiles), tilesDir)
	if len(allFiles) == 0 {
		return
	}

	// Inspect first 8 files in detail
	for i, path := range allFiles {
		if i >= 8 {
			break
		}
		rel, _ := filepath.Rel(tilesDir, path)
		fi, _ := os.Stat(path)
		f, err := os.Open(path)
		if err != nil {
			fmt.Printf("=== %s — open error: %v\n\n", rel, err)
			continue
		}
		buf := make([]byte, 1024)
		n, _ := f.Read(buf)
		f.Close()

		fmt.Printf("=== %s (%d bytes total, read %d) ===\n", rel, fi.Size(), n)

		hits := uuidRE.FindAll(buf[:n], -1)
		if len(hits) > 0 {
			fmt.Printf("  UUID-shaped strings: %d\n", len(hits))
			for j, h := range hits {
				if j >= 4 {
					break
				}
				fmt.Printf("    [%d] %s\n", j, string(h))
			}
		} else {
			fmt.Printf("  (no UUID text in first 1KB)\n")
		}

		// First 64 bytes hex
		hexLen := 64
		if n < hexLen {
			hexLen = n
		}
		fmt.Printf("  First %d bytes hex: %x\n", hexLen, buf[:hexLen])

		// Extract printable ASCII runs >= 6 chars (likely strings)
		printable := extractPrintableStrings(buf[:n], 6)
		shown := 0
		for _, s := range printable {
			if shown >= 6 {
				break
			}
			fmt.Printf("  string: %q\n", s)
			shown++
		}
		fmt.Println()
	}

	// Also tally by directory so we see the overall layout
	byDir := make(map[string]int)
	for _, p := range allFiles {
		rel, _ := filepath.Rel(tilesDir, p)
		dir := filepath.Dir(rel)
		byDir[dir]++
	}
	fmt.Println("Files per subdirectory:")
	keys := make([]string, 0, len(byDir))
	for k := range byDir {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		fmt.Printf("  %4d  %s\n", byDir[k], k)
	}
}

func extractPrintableStrings(b []byte, minLen int) []string {
	var out []string
	var cur []byte
	for _, c := range b {
		if c >= 0x20 && c <= 0x7e {
			cur = append(cur, c)
		} else {
			if len(cur) >= minLen {
				out = append(out, string(cur))
			}
			cur = nil
		}
	}
	if len(cur) >= minLen {
		out = append(out, string(cur))
	}
	return out
}

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
