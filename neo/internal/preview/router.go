package preview

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// RouterRegistrar exposes an already-running dev server through Centra AI's
// existing authenticated /preview/{user}/ reverse proxy. It owns no sandbox
// and starts no process; the private coding worker owns the local server.
type RouterRegistrar struct {
	userID      string
	privateHost string
	internalURL string
	token       string
	publicBase  string
	client      *http.Client
}

func NewRouterRegistrar(cfg Config, privateHost string) *RouterRegistrar {
	return &RouterRegistrar{
		userID: strings.TrimSpace(cfg.UserID), privateHost: strings.TrimSpace(privateHost),
		internalURL: strings.TrimRight(strings.TrimSpace(cfg.RouterInternalURL), "/"),
		token:       strings.TrimSpace(cfg.RouterToken), publicBase: strings.TrimRight(strings.TrimSpace(cfg.PublicBase), "/"),
		client: &http.Client{Timeout: 10 * time.Second},
	}
}

func (r *RouterRegistrar) Enabled() bool {
	return r != nil && r.userID != "" && r.privateHost != "" && r.internalURL != "" && r.token != ""
}

func (r *RouterRegistrar) Register(ctx context.Context, port int) (string, error) {
	if !r.Enabled() {
		return "", errors.New("router preview registration is unavailable")
	}
	if port < 1024 || port > 65535 || net.ParseIP(r.privateHost) == nil && !validPreviewHost(r.privateHost) {
		return "", errors.New("private preview host or port is invalid")
	}
	body, _ := json.Marshal(map[string]string{
		"user_id": r.userID, "host": r.privateHost, "port": strconv.Itoa(port),
	})
	if err := r.call(ctx, http.MethodPost, body); err != nil {
		return "", err
	}
	base := r.publicBase
	if base == "" {
		return "/preview/" + r.userID + "/", nil
	}
	return base + "/preview/" + r.userID + "/", nil
}

func (r *RouterRegistrar) Deregister(ctx context.Context) error {
	if !r.Enabled() {
		return nil
	}
	body, _ := json.Marshal(map[string]string{"user_id": r.userID})
	return r.call(ctx, http.MethodDelete, body)
}

func (r *RouterRegistrar) call(ctx context.Context, method string, body []byte) error {
	request, err := http.NewRequestWithContext(ctx, method, r.internalURL+"/internal/preview/register", bytes.NewReader(body))
	if err != nil {
		return err
	}
	request.Header.Set("Authorization", "Bearer "+r.token)
	request.Header.Set("Content-Type", "application/json")
	response, err := r.client.Do(request)
	if err != nil {
		return fmt.Errorf("router preview registration: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode/100 != 2 {
		return fmt.Errorf("router preview registration returned %s", response.Status)
	}
	return nil
}

func validPreviewHost(value string) bool {
	if len(value) > 253 || strings.HasPrefix(value, ".") || strings.HasSuffix(value, ".") {
		return false
	}
	for _, label := range strings.Split(value, ".") {
		if label == "" || len(label) > 63 || strings.HasPrefix(label, "-") || strings.HasSuffix(label, "-") {
			return false
		}
		for _, current := range label {
			if current >= 'a' && current <= 'z' || current >= 'A' && current <= 'Z' || current >= '0' && current <= '9' || current == '-' {
				continue
			}
			return false
		}
	}
	return true
}
