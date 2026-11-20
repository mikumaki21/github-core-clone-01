# Go log — github-core-clone-01 (porting Go)

## Tujuan
Port entry Node (skor live 911) ke static binary Go (stdlib saja) karena
10 besar leaderboard semuanya static binary Go/Rust (skor live 960–970).
Folder terpisah: `~/workspace/makefun/github-core-clone-01-go/`.
Entry Node di `~/workspace/makefun/github-core-clone-01-entry/` TIDAK disentuh.

## Gate (dari parent)
- `sdk/run.ts --json` pada server fresh: 120/120 (git 30, repos 15, issues 15,
  pulls 20, GraphQL 20, MCP 15, protocol 5).
- `sdk/run.ts --perf --json`: total lokal jelas di atas 965 (bidik 985+,
  perf 185+).
- Larangan: jangan push ke GitHub, jangan submit, jangan buat bundle.

## Keputusan desain
- Go 1.27.4 (tarball resmi) di `~/workspace/makefun/tools/go/go/bin/go`.
  Bukan system-wide.
- Satu module `forge`, satu package `main`, stdlib saja, tanpa dependensi.
- Binary `forge`: `CGO_ENABLED=0 go build -trimpath -buildvcs=false
  -ldflags="-s -w"`. Static, stripped.
- `main.ts` = launcher tipis (tiruan starter): spawn `./forge`, baca port
  dari stdout `[forge] listening on port N`, POST port ke
  `$MAKEFUN_ENV_URL/register`, poll `GET /done`, SIGTERM binary, exit 0.
  Binary sendiri: listen `127.0.0.1:0`, print port, serve sampai sinyal.
- Fondasi (ditulis koordinator): `store.go` (port store.ts),
  `gitpack.go` (port gitpack.ts), `main.go` (server/router/auth/Ctx).
- Satu mutex mem-serialisasi seluruh request (persis Node single-thread):
  timestamp deterministik dan mutasi store bebas race tanpa locking di
  permukaan. Workload clone konkuren tetap cepat karena pack dimemoize
  (lihat bawah).
- Permukaan (4 builder paralel): `git.go`, `rest.go`, `graphql.go`, `mcp.go`.
  Masing-masing mem-port semantik Node persis; Go idiomatis, bukan
  transliterasi baris-per-baris.
- Optimasi yang di-port dari Node (OPTIMIZE_LOG.md): pack cache per set
  objek (key = oid terurut, maks 128 entri / 16 MiB, eviksi FIFO),
  lewati `reachableObjects` saat haves kosong, jangan parse JSON untuk
  body git.
- JSON: `encoding/json` dengan `SetEscapeHTML(false)`; map key diurutkan
  encoding/json (deterministik). Daftar tools MCP dari struct (bukan map)
  agar byte-identik antar panggilan.

## Status fondasi (2026-11-20 ~21:55 WIB)
- `go vet ./...` bersih; binary `forge` 6.803.616 byte (6,49 MiB) —
  di bawah batas 8 MiB per file. Static, stripped.
- Smoke test: `/healthz` → `{"ok":true}`; tanpa token → 401;
  `/api/v3/` (stub REST) → 404. Benar.

## Hasil ukur (server fresh, Node 22, `sdk/run.ts --perf --json`, 2026-11-20 ~22:00 WIB)

Conformance tanpa perf: **120/120** (git 30, repos 15, issues 15, pulls 20,
GraphQL 20, MCP 15, protocol 5) — juga 120/120 lagi setelah perbaikan
ViewerLogin di bawah.

| run | push ms/pts | clone ms/pts | read ms/pts | write ms/pts | perf | total |
|-----|-------------|--------------|-------------|--------------|------|-------|
| go-1 | 365 / 50 | 116 / 40 | 665 / 50 | 95 / 49 | 189 | **989** |
| go-2 | 369 / 50 | 135 / 37 | 628 / 50 | 83 / 50 | 187 | **987** |
| go-3 | 361 / 50 | 156 / 35 | 528 / 50 | 86 / 50 | 185 | **985** |
| median | 365 / 50 | 135 / 37 | 628 / 50 | 86 / 50 | 187 | **987** |

perf_p50_us: 227.109 / 225.854 / 224.947 (median ~225.854).
T_ref kit: push 1640,82, clone 266,941, read 2759,715, write 348,965 ms.

Perbandingan dengan Node (baseline lokal): Node 965–983 (perf 165–182,
clone 28–36, push 46–49, write 45–50). Go: clone 35–40 (vs 28–36 Node),
push mentok 50, write 49–50. Kenaikan total lokal vs Node ~+7…+24 poin
tergantung run; vs skor live Node 911 (perf ~111), harapannya live Go
di kisaran 940-an — tapi live tidak bisa dijamin dari angka lokal saja
(pod grading berbagi CPU, T_ref sama).

## Perbaikan integrasi (oleh koordinator)
- `ViewerLogin()` di `store.go`: tadinya iterasi map Go (acak) — diubah
  ke urutan ID repo terurut (deterministik, sama seperti insertion order
  Map di Node). Penting untuk replay byte-identical. Setelah perbaikan,
  conformance ulang tetap 120/120.
- Launcher `main.ts` dites dengan fake env (POST /register 200,
  GET /done false,false,true): exit 0, port ter-register, binary
  dimatikan bersih (tidak ada proses `forge` tersisa).
- Ukuran: binary `forge` 7.495.840 byte (< 8 MiB/file), total folder
  7.672.432 byte (< 16 MiB). Static, stripped, `file` = ELF 64-bit
  LSB, statically linked.

## Catatan dari builder (disimpan)
- MCP: `server/discover` header jadi `X-Github-Request-Id` (kanonikalisasi
  Go) vs lowercase di Node — isi sama. Konflik merge multi-path dilaporkan
  urut alfabetis (Node: urutan insersi map).
- GraphQL: urutan key JSON jawaban alfabetis (encoding/json Go) vs urutan
  seleksi di Node — JSON yang di-parse sama.
- REST: quirk `per_page` default di-port persis Node (tanpa param → 1,
  karena `Number(null)` = 0 → clamp 1; default 30 hanya bila param ada
  tapi non-numerik). Entry Node lolos 120/120 dengan quirk ini.
- Git: satu anomali sekali-muncul `could not import encoding/base64`
  saat `go test` pertama — hilang sendiri, vet/build bersih sesudahnya.

## Verdict gate
- Total lokal Go: 985, 987, 989 — semuanya > 965, conformance 120/120
  di semua run → **GATE LULUS**. Versi Go boleh diajukan untuk submit
  (keputusan submit tetap di parent; Joy: jangan submit sebelum skor
  di atas 911 — skor live baru ketahuan setelah submit, dan entry 911
  bisa dijadikan final bila versi Go ternyata lebih rendah).
- Yang belum dilakukan (di luar tugas): push ke GitHub, bundle, submit,
  `.makefun/owner.json`, README badge final (repo Go belum ada).
