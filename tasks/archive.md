## Port render_telegraf.py to Go
**Tier:** 2  **Status:** complete  **Delegation:** in-harness  **Date:** 2026-10-12  **Archived:** 2026-10-12
### Spec
- Goal: Replace the Python Telegraf renderer with a stdlib Go command that reads the same manifests and writes byte-identical `telegraf/<host>.conf` files.
- Changes: `cmd/render-telegraf/main.go`, `internal/render`, `go.mod`, README.md, AGENTS.md. Delete `render_telegraf.py`, `tests/test_render.py`, and `__pycache__`.
- Contracts preserved: manifest validation, `--check` results, snippet bytes (banner still says `render_telegraf.py`), exit 0/1.
- Acceptance criteria:
  1. `go test ./...` covers every Python test case and passes.
  2. Regenerating snippets leaves `git diff --stat telegraf/` empty.
  3. `gofmt` and `go vet ./...` are clean.
### Progress
- [x] Exploration
- [x] Implementation
- [x] Verification
### Gates
- [x] Mandatory gates executed automatically post-implementation
- [x] Evidence logged
- Gate: gofmt
- Command: `gofmt -w cmd internal && gofmt -l cmd internal`
- Exit code: 0
- Result: pass, no files listed
- Gate: go vet
- Command: `go vet ./...`
- Exit code: 0
- Result: pass
- Gate: go test
- Command: `go test ./...`
- Exit code: 0
- Result: pass (`internal/render` ok)
- Gate: identical snippets
- Command: `go run ./cmd/render-telegraf && git diff --stat -- telegraf/`
- Exit code: 0
- Result: pass, empty diff after writing 5 files
