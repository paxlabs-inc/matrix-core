// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package mcpcontrol

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"matrix/executor/mcp"
	"matrix/executor/tool"
	neotools "matrix/neo/internal/tools"
)

func (s *Store) Probe(ctx context.Context, alias string) (Server, error) {
	server, err := s.Get(ctx, alias)
	if err != nil {
		return Server{}, err
	}
	secrets, err := s.serverSecrets(alias)
	if err != nil {
		_ = s.ProbeFailure(ctx, alias, err)
		return Server{}, err
	}
	spec, err := s.probeSpec(ctx, server, secrets)
	if err != nil {
		_ = s.ProbeFailure(ctx, alias, err)
		return Server{}, err
	}
	manager := mcp.NewManager(mcp.ManagerParams{StderrSink: os.Stderr})
	defer manager.Close()
	started := time.Now()
	probeCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	client, err := manager.Spawn(probeCtx, spec)
	if err != nil {
		safe := SafeRuntimeError(err)
		_ = s.ProbeFailure(ctx, alias, safe)
		return Server{}, safe
	}
	if err := client.Ping(probeCtx); err != nil {
		safe := SafeRuntimeError(err)
		_ = s.ProbeFailure(ctx, alias, safe)
		return Server{}, safe
	}
	discovered := manager.Tools(alias)
	if len(discovered) == 0 {
		err := errors.New("MCP server advertised no tools")
		_ = s.ProbeFailure(ctx, alias, err)
		return Server{}, err
	}
	tools := make([]Tool, 0, len(discovered))
	for _, advertised := range discovered {
		input := map[string]any{"type": "object", "properties": map[string]any{}}
		if len(advertised.InputSchema) > 0 {
			if err := json.Unmarshal(advertised.InputSchema, &input); err != nil {
				failure := fmt.Errorf("MCP tool %q has invalid input schema: %w", advertised.Name, err)
				_ = s.ProbeFailure(ctx, alias, failure)
				return Server{}, failure
			}
		}
		tools = append(tools, Tool{
			Name: advertised.Name, Function: canonicalFunction(alias, advertised.Name),
			Description: strings.TrimSpace(advertised.Description), InputSchema: input,
		})
	}
	return s.SaveProbe(ctx, alias, tools, time.Since(started))
}

func (s *Store) probeSpec(ctx context.Context, server Server, secrets serverSecrets) (mcp.ServerSpec, error) {
	environment, runAs, err := stdioEnvironment(server.Config, secrets)
	if err != nil {
		return mcp.ServerSpec{}, err
	}
	headers := cloneMap(secrets.Headers)
	if server.Config.OAuth != nil {
		token, err := s.validAccessToken(ctx, server.Config.Alias)
		if err != nil {
			return mcp.ServerSpec{}, err
		}
		if token == "" {
			return mcp.ServerSpec{}, errors.New("MCP OAuth authorization is required before probing")
		}
		if headers == nil {
			headers = map[string]string{}
		}
		headers["Authorization"] = "Bearer " + token
	}
	var httpClient *http.Client
	if server.Config.Transport == "http" {
		endpoint, parseErr := url.Parse(server.Config.Endpoint)
		if parseErr != nil {
			return mcp.ServerSpec{}, parseErr
		}
		httpClient, err = pinnedHTTPSClient(endpoint)
		if err != nil {
			return mcp.ServerSpec{}, err
		}
	}
	return mcp.ServerSpec{
		Alias: server.Config.Alias, Transport: server.Config.Transport,
		Command: server.Config.Command, Args: append([]string(nil), server.Config.Args...),
		Env: environment, RunAs: runAs, Endpoint: server.Config.Endpoint,
		Headers: headers, HTTPClient: httpClient, PackageDigest: server.Config.PackageDigest,
	}, nil
}

func (s *Store) RuntimeEntries(ctx context.Context) ([]neotools.DynamicMCPServer, error) {
	servers, err := s.RuntimeServers(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]neotools.DynamicMCPServer, 0, len(servers))
	for _, server := range servers {
		secrets, err := s.serverSecrets(server.Config.Alias)
		if err != nil {
			return nil, err
		}
		spec, err := s.probeSpec(ctx, server, secrets)
		if err != nil {
			return nil, err
		}
		declared := make([]tool.ToolEntry, 0, len(server.Tools))
		enabled := make(map[string]bool, len(server.Tools))
		for _, discovered := range server.Tools {
			if !validEffect(discovered.EffectClass) {
				return nil, fmt.Errorf("%w: %s/%s", ErrUnclassified, server.Config.Alias, discovered.Name)
			}
			declared = append(declared, tool.ToolEntry{
				Name: discovered.Name, Description: discovered.Description,
				SideEffectClass: discovered.EffectClass,
			})
			enabled[discovered.Name] = discovered.Enabled
		}
		activeCount := 0
		for _, active := range enabled {
			if active {
				activeCount++
			}
		}
		if activeCount == 0 {
			return nil, fmt.Errorf("MCP server %q has no enabled tools", server.Config.Alias)
		}
		out = append(out, neotools.DynamicMCPServer{
			Manifest: tool.ServerEntry{
				Alias: server.Config.Alias, Transport: server.Config.Transport,
				PackageDigest: server.Config.PackageDigest, Version: server.Config.Version,
				Command: server.Config.Command, Args: append([]string(nil), server.Config.Args...),
				Endpoint: server.Config.Endpoint, Tools: declared,
			},
			Environment: spec.Env, Headers: spec.Headers, RunAs: spec.RunAs, Enabled: enabled, HTTPClient: spec.HTTPClient,
		})
	}
	return out, nil
}

func stdioEnvironment(config Config, secrets serverSecrets) ([]string, *mcp.ProcessIdentity, error) {
	if config.Transport != "stdio" {
		return nil, nil, nil
	}
	environment := tool.AgentEnvironment(os.Environ())
	for _, key := range config.EnvKeys {
		if tool.ProtectedEnvironment(key) {
			return nil, nil, fmt.Errorf("MCP environment name %q belongs to a protected Matrix capability", key)
		}
		value, ok := secrets.Env[key]
		if !ok {
			return nil, nil, fmt.Errorf("MCP encrypted environment value %q is unavailable", key)
		}
		environment = append(environment, key+"="+value)
	}
	identity, configured, err := tool.AgentIdentityFromEnv()
	if err != nil {
		return nil, nil, err
	}
	if !configured {
		return environment, nil, nil
	}
	return environment, &mcp.ProcessIdentity{UID: identity.UID, GID: identity.GID, Home: identity.Home, User: identity.User}, nil
}
