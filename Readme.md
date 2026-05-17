# sm_overview realtime

A **live, multiplayer map** for Scrap Mechanic survival mode.

Run a small `.exe` while you play and a browser window shows the world with you and your friends moving on it in real time — your terrain, your POIs (Warehouses, Mechanic Stations, Hideouts, Ruin City, the Crashed Ship, etc.), and where everyone is right now.

![screenshot placeholder — drop a real one in later](https://via.placeholder.com/900x500.png?text=sm_overview+realtime)

This is a fork of [the1killer/sm_overview](https://github.com/the1killer/sm_overview) which generated a one-shot static map. This version:

- **streams live player positions** every second
- **labels POIs by name** on the map (Mechanic Station, Crashed Ship, etc.)
- **shares over LAN** — your friends can see the same map in their own browser
- **auto-patches and auto-restores** the game's Lua files so you never have to edit them yourself
- works with **Scrap Mechanic 0.7.4.778**

---

## Quick start

1. **Download** [`smoverview.exe`](https://github.com/darrennorthfield/scrapsurvival_overview_realtime/raw/main/smoverview.exe) (~9 MB).
2. **Right-click → Run as administrator.** It needs to write to your Steam install directory; this is the one-time UAC prompt.
   - First launch may trigger Windows SmartScreen ("unrecognised app"). Click **More info → Run anyway**. The binary is unsigned.
3. Your default browser opens to `http://127.0.0.1:7777` showing an empty dark map.
4. **Launch Scrap Mechanic, load your Survival save.** Within a couple of seconds the map fills in: terrain, biomes, POI labels, and a coloured dot for your character that updates as you walk.
5. **When you're done playing, close the smoverview.exe console window** (X button or Ctrl+C). It restores your game files to vanilla automatically — Steam updates and SM patches will never conflict because nothing is left modified between sessions.

That's the whole workflow. No manual file editing. No leftover state.

---

## Playing with a friend on your LAN

The Lua hook that emits player positions runs on the **host's** machine (whoever is hosting the Steam game). So the host is the source of truth. The friend who's joined just needs to point their browser at the host.

**On the host's PC:**
- Run `smoverview.exe` as normal. The panel in the top-right of the map page lists your **LAN IPs** (e.g. `192.168.1.42:7777`). Share one with your friend.

**On the friend's PC:**
- Also run `smoverview.exe` (they don't need a patched SM — they're only viewing, but installing the .exe is the easiest way to get the map UI).
- In the top-right panel, paste the host's `IP:port` into the **"Connect to host"** field and hit Tab.
- Both of you now see the same live map.

Two players on the same Wi-Fi: works out of the box. Over the internet from different houses: not yet supported (would need a relay; tracked as a future improvement).

---

## What's on the map

| Colour / icon | Meaning |
|---|---|
| Blue | Lake / open water |
| Muted green squares in water | Small islands |
| Dark green | Pine forest |
| Brown-orange | Autumn forest |
| Charcoal | Burnt forest |
| Mid green | Meadow |
| Yellow-green | Field / farmland |
| Tan | Desert |
| Bright tan paths | Roads |
| Vivid purple area | Survival start area (your spawn point) |
| Yellow-green pool | Chemical lake |
| **Yellow / orange dot** | POI — hover for name, big ones are always labelled |
| **Coloured circle with name** | Live player |

All terrain colours, POI names and player markers update live as long as smoverview.exe is running.

---

## Behind the scenes

When you start the `.exe`:

1. It backs up two of SM's Lua files (`SurvivalGame.lua` and `terrain_overworld.lua`) to `<file>.smoverview-backup` next to the original.
2. It overwrites them with patched versions (embedded inside the `.exe`) that emit player positions and terrain cell data to SM's own log file.
3. It tails SM's log file (`<SM>\Logs\game-*.log`) and serves the parsed data at `http://127.0.0.1:7777`.

When you stop it (Ctrl+C, close console, etc.), it copies each backup back over the patched file and deletes the backup. SM is back to vanilla.

---

## Command-line options

You shouldn't need any of these for normal use.

```
smoverview.exe [flags]

  --sm-path <path>        Scrap Mechanic install directory
                          (default: C:\Program Files (x86)\Steam\steamapps\common\Scrap Mechanic)
  --port <port>           HTTP server port (default: 7777)
  --no-open-browser       Don't auto-open the browser at startup
  --no-patch              Skip automatic Lua patching (advanced — patch manually)
  --restore               Restore vanilla SM Lua files from backups and exit
  --scan-tiles            Inspect SM's .tile files (developer diagnostic)
```

## Troubleshooting

**"This app can't run on your PC"** — wrong CPU architecture or the download came through as an HTML page. Re-download from the [raw URL](https://github.com/darrennorthfield/scrapsurvival_overview_realtime/raw/main/smoverview.exe) directly; the file should be ~9 MB.

**"Could not patch... access denied"** — you're not running as administrator. Right-click `smoverview.exe` → Run as administrator.

**Map page shows 🔴 no data** — Scrap Mechanic isn't running, or you haven't loaded into a Survival save yet, or you're on a different SM mode (Creative/Challenge — only Survival is supported).

**Map is unstyled / colours look like random noise** — the tile database might not have finished scanning yet. Refresh the browser tab after a few seconds. (Look for "tile DB loaded: ~351 tiles" in the .exe's console window.)

**Smoverview crashed and left my SM modified** — run `smoverview.exe --restore`. It'll detect the leftover backup files and put vanilla back.

**Scrap Mechanic updated and now something's broken** — the patches embedded in the .exe are version-specific. If SM ships an incompatible update, the .exe may stop working until a new build is released here. Run `smoverview.exe --restore` to clean up.

## Known limitations

- **SM version coupling.** The embedded Lua patches assume Scrap Mechanic 0.7.4.778. A future SM update may require a corresponding update to this tool.
- **Host-only data.** Player positions are emitted by the host's game. If your friend hosts and only you run smoverview.exe, you see nothing — they need to host it for both of you to see the map. (Workaround: have whoever's running smoverview also be the host.)
- **No internet multiplayer yet.** LAN works; over-the-internet needs a relay server, which is on the roadmap.
- **Unsigned binary.** SmartScreen will warn on first launch. We don't currently ship a code-signing certificate.

## Credits

- Original [sm_overview](https://github.com/the1killer/sm_overview) by [the1killer](https://github.com/the1killer) — the basis for terrain dumping and the Leaflet rendering approach.
- Scrap Mechanic is property of Axolot Games AB. This project has no affiliation with them.

## Licence

This work is licensed under a [Creative Commons Attribution-NonCommercial-ShareAlike 4.0 International License](http://creativecommons.org/licenses/by-nc-sa/4.0/), matching the upstream project.
