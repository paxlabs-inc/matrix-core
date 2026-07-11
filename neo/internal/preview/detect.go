// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package preview

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
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

// DetectStart infers how to run a project by reading its REAL manifest files
// from the workspace root (package.json scripts + dependencies, python
// entrypoints, go.mod). ok=false means no runnable app was detected — the
// caller surfaces preview.failed with an honest reason rather than guessing.
func DetectStart(root string) (StartSpec, bool) {
	if pkg, ok := readPackageJSON(root); ok {
		fw := nodeFrameworks(pkg)
		port := nodePort(fw)
		env := map[string]string{"HOST": "0.0.0.0", "PORT": port, "BROWSER": "none"}
		switch {
		case pkg.Scripts["dev"] != "":
			return StartSpec{
				Install: "npm install",
				Serve:   nodeServe(fw, "npm run dev", port),
				Port:    port,
				Env:     env,
			}, true
		case pkg.Scripts["start"] != "":
			return StartSpec{Install: "npm install", Serve: "npm start", Port: port, Env: env}, true
		case pkg.Scripts["preview"] != "":
			return StartSpec{Install: "npm install", Serve: "npm run preview", Port: port, Env: env}, true
		}
	}

	// Python web apps: FastAPI/uvicorn by entrypoint, Flask by dependency.
	if fileExists(root, "main.py") || fileExists(root, "app.py") {
		reqs := readFileLower(root, "requirements.txt")
		if strings.Contains(reqs, "flask") && !strings.Contains(reqs, "fastapi") {
			return StartSpec{
				Install: "pip install -r requirements.txt 2>/dev/null || true",
				Serve:   "flask run --host 0.0.0.0 --port 8000",
				Port:    "8000",
				Env:     map[string]string{"FLASK_RUN_HOST": "0.0.0.0", "PORT": "8000"},
			}, true
		}
		return StartSpec{
			Install: "pip install -r requirements.txt 2>/dev/null || true",
			Serve:   "uvicorn main:app --host 0.0.0.0 --port 8000",
			Port:    "8000",
			Env:     map[string]string{"HOST": "0.0.0.0", "PORT": "8000"},
		}, true
	}

	// Go services: run the main package; the app must honour PORT.
	if fileExists(root, "go.mod") {
		return StartSpec{
			Serve: "go run .",
			Port:  "8080",
			Env:   map[string]string{"PORT": "8080", "HOST": "0.0.0.0"},
		}, true
	}

	return StartSpec{}, false
}

type packageJSON struct {
	Scripts         map[string]string `json:"scripts"`
	Dependencies    map[string]string `json:"dependencies"`
	DevDependencies map[string]string `json:"devDependencies"`
}

func readPackageJSON(root string) (packageJSON, bool) {
	data, err := os.ReadFile(filepath.Join(root, "package.json"))
	if err != nil {
		return packageJSON{}, false
	}
	var pkg packageJSON
	if err := json.Unmarshal(data, &pkg); err != nil {
		return packageJSON{}, false
	}
	if pkg.Scripts == nil {
		pkg.Scripts = map[string]string{}
	}
	return pkg, true
}

// nodeFrameworks reports the framework set from the REAL dependency lists.
func nodeFrameworks(pkg packageJSON) map[string]bool {
	fw := map[string]bool{}
	for _, deps := range []map[string]string{pkg.Dependencies, pkg.DevDependencies} {
		for name := range deps {
			switch strings.ToLower(name) {
			case "next":
				fw["next"] = true
			case "@remix-run/react", "@remix-run/node":
				fw["remix"] = true
			case "astro":
				fw["astro"] = true
			case "vite":
				fw["vite"] = true
			case "react", "react-dom":
				fw["react"] = true
			case "vue":
				fw["vue"] = true
			case "svelte", "@sveltejs/kit":
				fw["svelte"] = true
			}
		}
	}
	return fw
}

// nodePort picks the conventional dev-server port for the detected framework.
func nodePort(fw map[string]bool) string {
	switch {
	case fw["next"], fw["remix"]:
		return "3000"
	case fw["astro"]:
		return "4321"
	case fw["svelte"], fw["vite"], fw["react"], fw["vue"]:
		return "5173"
	default:
		return "3000"
	}
}

// nodeServe augments the dev command with the host/port flags a framework needs
// to bind reachably (env alone is not always honoured by Vite-family servers).
func nodeServe(fw map[string]bool, base, port string) string {
	switch {
	case fw["astro"], fw["svelte"], fw["vite"], fw["react"], fw["vue"]:
		return base + " -- --host 0.0.0.0 --port " + port
	case fw["next"], fw["remix"]:
		return base + " -- -H 0.0.0.0 -p " + port
	default:
		return base
	}
}

func fileExists(root, name string) bool {
	info, err := os.Stat(filepath.Join(root, name))
	return err == nil && !info.IsDir()
}

func readFileLower(root, name string) string {
	data, err := os.ReadFile(filepath.Join(root, name))
	if err != nil {
		return ""
	}
	return strings.ToLower(string(data))
}
