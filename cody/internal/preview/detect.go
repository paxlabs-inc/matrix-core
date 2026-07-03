// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package preview

import (
	"strings"

	"matrix/cody/internal/workspace"
)

// StartSpec is a detected way to bring a project's app up inside the sandbox:
// an optional one-shot install/build command, the long-lived serve command,
// and the port the app is expected to bind. Host is forced to 0.0.0.0 via env
// so the router can reach it over the private network.
type StartSpec struct {
	// Install runs once before Serve (npm install, pip install …); empty skips.
	Install string
	// Serve is the long-lived dev/serve command; started detached in the sandbox.
	Serve string
	// Port is the port the app binds (string form for the router register call).
	Port string
	// Env is exported before Serve (PORT/HOST so frameworks bind reachably).
	Env map[string]string
}

// DetectStart infers how to run a project from its scanned model. It is a pure
// function over the workspace.Model (framework + build targets) so it is unit
// tested without a sandbox. ok=false means no runnable app was detected — the
// caller surfaces preview.failed with an honest reason rather than guessing.
func DetectStart(m *workspace.Model) (StartSpec, bool) {
	if m == nil {
		return StartSpec{}, false
	}
	// BuildTargets are namespaced by the scanner (npm:dev, go:build, make:test).
	// Index both the raw target and the bare name after the "tool:" prefix so a
	// package.json "dev" script matches whether it arrives as "dev" or "npm:dev".
	targets := map[string]bool{}
	for _, t := range m.BuildTargets {
		lt := strings.ToLower(t)
		targets[lt] = true
		if i := strings.IndexByte(lt, ':'); i >= 0 {
			targets[lt[i+1:]] = true
		}
	}
	frameworks := map[string]bool{}
	for _, f := range m.Frameworks {
		frameworks[strings.ToLower(f)] = true
	}
	langs := map[string]bool{}
	for l := range m.Languages {
		langs[strings.ToLower(l)] = true
	}

	// Node/TS projects: a package.json script is the source of truth. Prefer the
	// dev server (hot-reload preview); fall back to start (a built app).
	if hasPackageJSON(m) {
		port := nodePort(frameworks)
		env := map[string]string{"HOST": "0.0.0.0", "PORT": port, "BROWSER": "none"}
		switch {
		case targets["dev"]:
			return StartSpec{
				Install: "npm install",
				Serve:   nodeServe(frameworks, "npm run dev", port),
				Port:    port,
				Env:     env,
			}, true
		case targets["start"]:
			return StartSpec{Install: "npm install", Serve: "npm start", Port: port, Env: env}, true
		case targets["preview"]:
			return StartSpec{Install: "npm install", Serve: "npm run preview", Port: port, Env: env}, true
		}
	}

	// Python web apps.
	if langs["python"] {
		if frameworks["fastapi"] || hasEntry(m, "main.py", "app.py") {
			return StartSpec{
				Install: "pip install -r requirements.txt 2>/dev/null || true",
				Serve:   "uvicorn main:app --host 0.0.0.0 --port 8000",
				Port:    "8000",
				Env:     map[string]string{"HOST": "0.0.0.0", "PORT": "8000"},
			}, true
		}
		if frameworks["flask"] {
			return StartSpec{
				Install: "pip install -r requirements.txt 2>/dev/null || true",
				Serve:   "flask run --host 0.0.0.0 --port 8000",
				Port:    "8000",
				Env:     map[string]string{"FLASK_RUN_HOST": "0.0.0.0", "PORT": "8000"},
			}, true
		}
	}

	// Go services: run the main package; the app must honour PORT.
	if langs["go"] {
		return StartSpec{
			Serve: "go run .",
			Port:  "8080",
			Env:   map[string]string{"PORT": "8080", "HOST": "0.0.0.0"},
		}, true
	}

	return StartSpec{}, false
}

// nodePort picks the conventional dev-server port for the detected framework.
func nodePort(fw map[string]bool) string {
	switch {
	case fw["next"], fw["remix"]:
		return "3000"
	case fw["astro"]:
		return "4321"
	case fw["svelte"], fw["sveltekit"], fw["vite"], fw["react"], fw["vue"]:
		return "5173"
	default:
		return "3000"
	}
}

// nodeServe augments the dev command with the host/port flags a framework needs
// to bind reachably (env alone is not always honoured by Vite-family servers).
func nodeServe(fw map[string]bool, base, port string) string {
	switch {
	case fw["astro"]:
		return base + " -- --host 0.0.0.0 --port " + port
	case fw["svelte"], fw["sveltekit"], fw["vite"], fw["react"], fw["vue"]:
		return base + " -- --host 0.0.0.0 --port " + port
	case fw["next"], fw["remix"]:
		return base + " -- -H 0.0.0.0 -p " + port
	default:
		return base
	}
}

func hasPackageJSON(m *workspace.Model) bool {
	for _, f := range m.Files {
		if strings.EqualFold(f.Path, "package.json") {
			return true
		}
	}
	// A detected node framework implies one even if the file list was trimmed.
	for _, fw := range m.Frameworks {
		switch strings.ToLower(fw) {
		case "next", "react", "vue", "svelte", "sveltekit", "astro", "remix", "vite":
			return true
		}
	}
	return false
}

func hasEntry(m *workspace.Model, names ...string) bool {
	want := map[string]bool{}
	for _, n := range names {
		want[strings.ToLower(n)] = true
	}
	for _, f := range m.Files {
		base := f.Path
		if i := strings.LastIndexByte(base, '/'); i >= 0 {
			base = base[i+1:]
		}
		if want[strings.ToLower(base)] {
			return true
		}
	}
	for _, e := range m.EntryPoints {
		base := e
		if i := strings.LastIndexByte(base, '/'); i >= 0 {
			base = base[i+1:]
		}
		if want[strings.ToLower(base)] {
			return true
		}
	}
	return false
}
