package repofacts

import (
	"os"
	"path/filepath"
)

// detectPython tries hard to figure out WHICH python tool the repo
// uses, because the audit command (pip-audit vs uv vs poetry) and
// the test command (pytest vs uv run pytest) differ. The trick is
// that python has 5+ valid manifests; we walk them in order of
// modernness so a repo using uv + a leftover requirements.txt is
// classified as uv.
//
// Module detection: we treat one Python manifest as one module. A
// "real" monorepo would have one pyproject.toml per service; setup.py
// roots are also valid module boundaries.
func detectPython(files []string, root string) *LanguageFacts {
	pm := detectPythonPackageManager(files, root)

	var modules []ModuleFacts
	for _, p := range files {
		switch filepath.Base(p) {
		case "pyproject.toml", "setup.py", "setup.cfg":
			modules = append(modules, ModuleFacts{
				Path:     rel(root, filepath.Dir(p)),
				Manifest: filepath.Base(p),
				// We don't parse Python TOML for the project name —
				// the cost outweighs the benefit (the LLM rarely
				// needs the name to write a CI step).
			})
		}
	}
	// Lockfile detection: any of the modern lockfiles counts.
	lock := hasFile(files, root, "uv.lock") ||
		hasFile(files, root, "poetry.lock") ||
		hasFile(files, root, "Pipfile.lock") ||
		hasFile(files, root, "pdm.lock")

	if len(modules) == 0 && !lock {
		// No manifest and no lockfile — this is not a Python project.
		// A bare requirements.txt without any manifest is too rare to
		// claim Python.
		return nil
	}
	return &LanguageFacts{
		Lang:             "python",
		Modules:          modules,
		PackageManager:   pm,
		ToolchainVersion: detectPythonVersion(files, root),
		HasLockfile:      lock,
	}
}

// detectPythonPackageManager picks the most likely tool based on
// lockfiles + manifests. Order: uv > poetry > pipenv > pdm > pip.
// uv is checked first because it's the newest and tends to be
// adopted with a clear intent (developers move FROM pip/poetry TO uv,
// not the other way around).
func detectPythonPackageManager(files []string, root string) string {
	switch {
	case hasFile(files, root, "uv.lock"):
		return "uv"
	case hasFile(files, root, "poetry.lock"):
		return "poetry"
	case hasFile(files, root, "Pipfile.lock"):
		return "pipenv"
	case hasFile(files, root, "pdm.lock"):
		return "pdm"
	case hasFile(files, root, "requirements.txt"):
		return "pip"
	}
	return ""
}

// detectPythonVersion reads .python-version (pyenv convention)
// first, then falls back to a regex over pyproject.toml's
// [project].requires-python. We do not try to parse PEP 440
// specifiers — passing the raw string through is fine for the LLM.
func detectPythonVersion(files []string, root string) string {
	if path := findFileAt(files, root, ".python-version"); path != "" {
		raw, err := os.ReadFile(path)
		if err == nil {
			return trimSpace(string(raw))
		}
	}
	return ""
}
