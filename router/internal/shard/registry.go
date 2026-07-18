package shard

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"sort"
	"strings"

	"matrix/router/internal/db"
	"matrix/router/internal/provision"
	"matrix/router/internal/railway"
)

type Entry struct {
	ID              string            `json:"id"`
	ProjectID       string            `json:"project_id"`
	EnvironmentID   string            `json:"environment_id"`
	RouterURL       string            `json:"router_url"`
	RailwayTokenEnv string            `json:"railway_token_env"`
	IngressKeys     map[string]string `json:"ingress_key_envs"`
}

type Registry struct {
	entries      map[string]Entry
	provisioners map[string]provision.Provisioner
	ingress      map[string]map[string][]byte
}

func Load(raw, image string) (*Registry, error) {
	var entries []Entry
	if err := json.Unmarshal([]byte(raw), &entries); err != nil {
		return nil, fmt.Errorf("shard registry: %w", err)
	}
	r := &Registry{entries: map[string]Entry{}, provisioners: map[string]provision.Provisioner{}, ingress: map[string]map[string][]byte{}}
	for _, e := range entries {
		if e.ID == "" || e.ProjectID == "" || e.EnvironmentID == "" || e.RouterURL == "" || e.RailwayTokenEnv == "" {
			return nil, fmt.Errorf("shard registry: incomplete entry %q", e.ID)
		}
		if _, ok := r.entries[e.ID]; ok {
			return nil, fmt.Errorf("shard registry: duplicate id %q", e.ID)
		}
		u, err := url.Parse(e.RouterURL)
		if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil {
			return nil, fmt.Errorf("shard registry: invalid router_url for %q", e.ID)
		}
		token := os.Getenv(e.RailwayTokenEnv)
		if token == "" {
			return nil, fmt.Errorf("shard registry: missing credential env %s", e.RailwayTokenEnv)
		}
		keys := map[string][]byte{}
		for kid, env := range e.IngressKeys {
			if strings.TrimSpace(kid) == "" || env == "" {
				return nil, fmt.Errorf("shard registry: invalid ingress key reference for %q", e.ID)
			}
			v := os.Getenv(env)
			if v == "" {
				return nil, fmt.Errorf("shard registry: missing ingress credential env %s", env)
			}
			keys[kid] = []byte(v)
		}
		if len(keys) == 0 {
			return nil, fmt.Errorf("shard registry: no ingress credential for %q", e.ID)
		}
		r.entries[e.ID] = e
		r.ingress[e.ID] = keys
		r.provisioners[e.ID] = &railway.Provisioner{Client: railway.New(token, e.ProjectID, e.EnvironmentID), Image: image}
	}
	return r, nil
}

func (r *Registry) ValidateDB(ctx context.Context, d *db.DB) error {
	shards, err := d.RailwayShards(ctx)
	if err != nil {
		return err
	}
	for _, s := range shards {
		if s.State == "disabled" {
			continue
		}
		e, ok := r.entries[s.ID]
		if !ok {
			return fmt.Errorf("shard registry: database shard %q absent from configuration", s.ID)
		}
		if e.ProjectID != s.ProjectID || e.EnvironmentID != s.EnvironmentID || e.RouterURL != s.RouterURL {
			return fmt.Errorf("shard registry: database/config mismatch for %q", s.ID)
		}
	}
	return nil
}

func (r *Registry) Provisioner(id string) (provision.Provisioner, bool) {
	p, ok := r.provisioners[id]
	return p, ok
}
func (r *Registry) Provider(id string) (provision.Provisioner, bool) { return r.Provisioner(id) }
func (r *Registry) Entry(id string) (Entry, bool)                    { e, ok := r.entries[id]; return e, ok }
func (r *Registry) IngressKeys(id string) (map[string][]byte, bool) {
	k, ok := r.ingress[id]
	return k, ok
}
func (r *Registry) CurrentIngress(id string) (string, []byte, bool) {
	keys, ok := r.ingress[id]
	if !ok || len(keys) == 0 {
		return "", nil, false
	}
	ids := make([]string, 0, len(keys))
	for keyID := range keys {
		ids = append(ids, keyID)
	}
	sort.Strings(ids)
	keyID := ids[len(ids)-1]
	return keyID, keys[keyID], true
}
