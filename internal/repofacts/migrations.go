package repofacts

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// MigrationsFacts describes one detected migrations directory and the
// best-guess command to apply it. Used by `sklp init dev` to emit a
// `migrate` oneshot service without making the LLM hallucinate the
// command from scratch.
//
// All fields are best-effort: the suggester always emits a NOTE
// asking the user to verify, even when the inference is high
// confidence.
type MigrationsFacts struct {
	// Path is the directory containing the migration files, relative
	// to the repo root with forward slashes.
	Path string

	// Tool is the canonical tool name we matched: "golang-migrate",
	// "alembic", "prisma", "drizzle", "knex", "rails", "ecto",
	// "unknown". Drives the command we suggest.
	Tool string

	// FileCount is the number of migration files found. Surface as
	// "(N files)" in the facts block so the LLM sees this is a
	// non-trivial folder, not an empty placeholder.
	FileCount int

	// LikelyCommand is the command we'd run to apply the migrations.
	// Cross-referenced with Makefile targets / package.json scripts
	// when available, otherwise a reasonable default for the tool.
	LikelyCommand string

	// LikelyWorkdir is the directory the command should run in
	// (typically the parent service's workdir, e.g. services/api for
	// migrations sitting in services/api/migrations or apps/migrations).
	LikelyWorkdir string

	// Source describes where the LikelyCommand came from so the LLM
	// (and the user reading the NOTE) can judge confidence:
	// "Makefile target migrate", "package.json scripts.migrate",
	// "convention" (default for the tool).
	Source string
}

// DetectMigrations walks the repo for known migrations layouts and
// returns one MigrationsFacts per detected folder. We deliberately
// keep the depth shallow (≤4 levels) to avoid scanning vendored or
// nested test fixtures.
//
// Detection rules per tool:
//
//   - golang-migrate: a directory named `migrations` containing files
//     matching `<NNNNNN>_<name>.up.sql` (and usually a sibling
//     `.down.sql`).
//   - alembic: a `migrations/versions/` directory + `alembic.ini` at
//     repo root.
//   - prisma: a `prisma/migrations/` directory + `prisma/schema.prisma`.
//   - drizzle: a `drizzle/` directory containing `*.sql` + a
//     `drizzle.config.ts/js` at the repo root.
//   - knex: a `migrations/` directory containing `*.{js,ts}` files
//     prefixed with a timestamp + a `knexfile.{js,ts}` at the repo
//     root.
//   - rails: a `db/migrate/` directory.
//   - ecto: a `priv/repo/migrations/` directory.
//
// Multiple tools can coexist (rare but happens during a migration);
// each detection produces its own MigrationsFacts entry.
func DetectMigrations(repo string) []MigrationsFacts {
	var out []MigrationsFacts
	rootHasFile := func(name string) bool {
		return fileExists(filepath.Join(repo, name))
	}

	// Walk the repo with a shallow depth cap. We're looking for a
	// handful of well-known directory names, so a full WalkDir would
	// be overkill — but the structure varies enough between repos
	// (services/api/migrations vs migrations/ vs db/migrate) that we
	// must look beyond the root.
	candidateDirs := findCandidateMigrationDirs(repo, 4)

	for _, dir := range candidateDirs {
		rel, _ := filepath.Rel(repo, dir)
		rel = filepath.ToSlash(rel)
		base := filepath.Base(dir)

		// alembic
		if base == "versions" && filepath.Base(filepath.Dir(dir)) == "migrations" && rootHasFile("alembic.ini") {
			out = append(out, MigrationsFacts{
				Path:          rel,
				Tool:          "alembic",
				FileCount:     countFilesByExt(dir, ".py"),
				LikelyCommand: "alembic upgrade head",
				LikelyWorkdir: filepath.ToSlash(filepath.Dir(filepath.Dir(rel))),
				Source:        "convention (alembic.ini detected)",
			})
			continue
		}

		// prisma — `prisma/migrations`
		if base == "migrations" && filepath.Base(filepath.Dir(dir)) == "prisma" {
			cmd, src := pickNodeMigrationCommand(repo, "prisma migrate deploy", "migrate")
			out = append(out, MigrationsFacts{
				Path:          rel,
				Tool:          "prisma",
				FileCount:     countFilesRecursive(dir, ".sql"),
				LikelyCommand: cmd,
				LikelyWorkdir: ".",
				Source:        src,
			})
			continue
		}

		// drizzle — `drizzle/` with .sql files + drizzle config at root
		if base == "drizzle" && (rootHasFile("drizzle.config.ts") || rootHasFile("drizzle.config.js")) {
			cmd, src := pickNodeMigrationCommand(repo, "drizzle-kit migrate", "migrate")
			out = append(out, MigrationsFacts{
				Path:          rel,
				Tool:          "drizzle",
				FileCount:     countFilesByExt(dir, ".sql"),
				LikelyCommand: cmd,
				LikelyWorkdir: ".",
				Source:        src,
			})
			continue
		}

		// knex — `migrations/` with timestamped .js/.ts + knexfile
		if base == "migrations" && (rootHasFile("knexfile.js") || rootHasFile("knexfile.ts")) {
			cmd, src := pickNodeMigrationCommand(repo, "knex migrate:latest", "migrate")
			out = append(out, MigrationsFacts{
				Path:          rel,
				Tool:          "knex",
				FileCount:     countFilesByExt(dir, ".js") + countFilesByExt(dir, ".ts"),
				LikelyCommand: cmd,
				LikelyWorkdir: ".",
				Source:        src,
			})
			continue
		}

		// rails — `db/migrate/`
		if base == "migrate" && filepath.Base(filepath.Dir(dir)) == "db" {
			out = append(out, MigrationsFacts{
				Path:          rel,
				Tool:          "rails",
				FileCount:     countFilesByExt(dir, ".rb"),
				LikelyCommand: "bundle exec rails db:migrate",
				LikelyWorkdir: ".",
				Source:        "convention (db/migrate/ detected)",
			})
			continue
		}

		// ecto — `priv/repo/migrations/`
		if base == "migrations" && strings.HasSuffix(filepath.ToSlash(filepath.Dir(dir)), "priv/repo") {
			out = append(out, MigrationsFacts{
				Path:          rel,
				Tool:          "ecto",
				FileCount:     countFilesByExt(dir, ".exs"),
				LikelyCommand: "mix ecto.migrate",
				LikelyWorkdir: ".",
				Source:        "convention (priv/repo/migrations/ detected)",
			})
			continue
		}

		// golang-migrate — generic `migrations/` with NNNNNN_*.up.sql
		// pattern. Must be checked LAST because it's the most permissive
		// pattern and would match drizzle/prisma/etc. otherwise.
		if base == "migrations" && hasGolangMigrateFiles(dir) {
			workdir := guessGoMigrateWorkdir(repo, rel)
			cmd, src := pickGoMigrateCommand(repo, workdir)
			// `make <target>` resolves the Makefile relative to cwd —
			// when the target lives in the root Makefile but the
			// service workdir is a sub-directory, cd into that
			// sub-directory makes `make` fail with "No rule to make
			// target". Always run root-Makefile commands from the repo
			// root.
			if strings.HasPrefix(cmd, "make ") {
				workdir = "."
			}
			out = append(out, MigrationsFacts{
				Path:          rel,
				Tool:          "golang-migrate",
				FileCount:     countFilesByExt(dir, ".sql"),
				LikelyCommand: cmd,
				LikelyWorkdir: workdir,
				Source:        src,
			})
		}
	}

	// Stable order: by path so YAML diffs are reproducible.
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out
}

// findCandidateMigrationDirs walks the repo up to maxDepth levels and
// returns every directory whose basename hints at a migrations layout.
// We pre-filter on basename so the per-tool checks above don't pay
// for an exhaustive walk.
func findCandidateMigrationDirs(repo string, maxDepth int) []string {
	candidates := map[string]bool{
		"migrations": true, // golang-migrate, alembic (parent), prisma, knex
		"migrate":    true, // rails (parent: db/)
		"versions":   true, // alembic
		"drizzle":    true, // drizzle
	}
	var out []string
	_ = filepath.WalkDir(repo, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if path == repo {
			return nil
		}
		if d.IsDir() && isIgnoredDir(filepath.Base(path)) {
			return filepath.SkipDir
		}
		if depth(repo, path) > maxDepth {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if d.IsDir() && candidates[filepath.Base(path)] {
			out = append(out, path)
		}
		return nil
	})
	return out
}

// hasGolangMigrateFiles reports whether dir contains at least one
// file matching the `<NNNNNN>_<name>.up.sql` convention. The regex
// allows 4-7 digit prefixes to cover both classic golang-migrate
// (6 digits) and atlas (timestamped) conventions.
func hasGolangMigrateFiles(dir string) bool {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return false
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if golangMigrateRE.MatchString(e.Name()) {
			return true
		}
	}
	return false
}

var golangMigrateRE = regexp.MustCompile(`^\d{4,14}_[\w-]+\.(up|down)\.sql$`)

// guessGoMigrateWorkdir returns the workdir from which the migrate
// command should run. For migrations at <svc>/migrations or
// <svc>/db/migrations, the workdir is <svc>. For top-level migrations
// (e.g. bakery: apps/migrations) we pick the sibling Go service that
// likely owns them — typically services/api.
func guessGoMigrateWorkdir(repo, migrationsPath string) string {
	parent := filepath.ToSlash(filepath.Dir(migrationsPath))
	// Case 1: migrations live INSIDE the service dir.
	if fileExists(filepath.Join(repo, parent, "go.mod")) ||
		fileExists(filepath.Join(repo, parent, "main.go")) {
		return parent
	}
	// Case 2: migrations are siblings of one or more services. Pick
	// the one most likely to own them by name preference order.
	siblings, _ := os.ReadDir(filepath.Join(repo, parent))
	preferred := []string{"core", "api", "backend", "server"}
	for _, p := range preferred {
		for _, e := range siblings {
			if !e.IsDir() {
				continue
			}
			if e.Name() == p {
				return filepath.ToSlash(filepath.Join(parent, p))
			}
		}
	}
	// Fallback: first sibling Go module.
	for _, e := range siblings {
		if !e.IsDir() {
			continue
		}
		if fileExists(filepath.Join(repo, parent, e.Name(), "go.mod")) {
			return filepath.ToSlash(filepath.Join(parent, e.Name()))
		}
	}
	return parent
}

// pickGoMigrateCommand cross-references the Makefile and the
// service's main.go to pick the most likely command. Order:
//
//  1. Makefile target named `migrate` (or `migrate-up` / `db-migrate`)
//     → use it (`make migrate`).
//  2. Service has a `migrate` subcommand (greppable in main.go)
//     → `go run . migrate up`.
//  3. Default to `go run . migrate up` with a "convention" source so
//     the user knows it's a guess.
func pickGoMigrateCommand(repo, workdir string) (cmd, source string) {
	makefile := filepath.Join(repo, "Makefile")
	if raw, err := os.ReadFile(makefile); err == nil {
		body := string(raw)
		for _, target := range []string{"migrate:", "migrate-up:", "db-migrate:"} {
			if strings.Contains(body, "\n"+target) || strings.HasPrefix(body, target) {
				name := strings.TrimSuffix(target, ":")
				return "make " + name, "Makefile target `" + name + "`"
			}
		}
	}
	// Look for "migrate" subcommand in the service's main.go.
	mainGo := filepath.Join(repo, workdir, "main.go")
	if raw, err := os.ReadFile(mainGo); err == nil {
		body := string(raw)
		if strings.Contains(body, `"migrate"`) || strings.Contains(body, "`migrate`") {
			return "go run . migrate up", "main.go references a `migrate` subcommand"
		}
	}
	return "go run . migrate up", "convention (golang-migrate)"
}

// pickNodeMigrationCommand prefers a package.json script when one
// exists, then falls back to the canonical CLI command.
func pickNodeMigrationCommand(repo, fallback, scriptName string) (cmd, source string) {
	pkg := filepath.Join(repo, "package.json")
	if raw, err := os.ReadFile(pkg); err == nil {
		// Lazy substring match; full JSON parse is overkill for one
		// boolean. False positives are rare because `"migrate":` only
		// appears in a `scripts:` map.
		if strings.Contains(string(raw), `"`+scriptName+`":`) {
			return "pnpm run " + scriptName, "package.json scripts." + scriptName
		}
	}
	return fallback, "convention"
}

// countFilesByExt returns the number of files in dir with the given
// extension (e.g. ".sql"). Non-recursive on purpose: migration tools
// keep all their files in one flat directory.
func countFilesByExt(dir, ext string) int {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0
	}
	n := 0
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if strings.HasSuffix(e.Name(), ext) {
			n++
		}
	}
	return n
}

// countFilesRecursive walks dir and counts every file with the given
// extension. Used for prisma where each migration has its own
// timestamped subdirectory containing migration.sql.
func countFilesRecursive(dir, ext string) int {
	n := 0
	_ = filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if !d.IsDir() && strings.HasSuffix(path, ext) {
			n++
		}
		return nil
	})
	return n
}
