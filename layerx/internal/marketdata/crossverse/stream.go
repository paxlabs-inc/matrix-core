package crossverse

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/gorilla/websocket"
)

const (
	reconnectMinBackoff = 500 * time.Millisecond
	reconnectMaxBackoff = 15 * time.Second
	readDeadline        = 90 * time.Second
	writeDeadline       = 10 * time.Second
)

type subscribeFrame struct {
	Event string `json:"event"`
	Topic string `json:"topic"`
}

func toWSURL(raw string) (string, error) {
	base, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("crossverse ws url %q is invalid: %w", raw, err)
	}
	switch base.Scheme {
	case "https", "wss":
		base.Scheme = "wss"
	case "http", "ws":
		base.Scheme = "ws"
	default:
		return "", fmt.Errorf("crossverse ws url scheme %q is invalid", base.Scheme)
	}
	base.RawQuery = ""
	return base.String(), nil
}

func (m *Manager) dialer() *websocket.Dialer {
	d := &websocket.Dialer{
		HandshakeTimeout: 15 * time.Second,
	}
	if m.cfg.NetDialContext != nil {
		d.NetDialContext = m.cfg.NetDialContext
	}
	return d
}

func (m *Manager) runSymbolStream(ctx context.Context, f *feed) {
	defer m.wg.Done()
	topics := []string{
		strings.ToUpper(f.symbol) + "@perp_orderbook",
		strings.ToUpper(f.symbol) + "@perp_trade",
		strings.ToUpper(f.symbol) + "@perp_stats",
	}
	m.runStream(ctx, m.symbolWSBase(f.symbol), topics, func(raw []byte) error {
		return f.handleMessage(raw, m.nowMs())
	}, func(phase Health) {
		f.setPhase(phase)
	}, func() {
		f.disconnected()
	})
}

func (m *Manager) runAggregateStream(ctx context.Context) {
	defer m.wg.Done()
	topics := []string{"markets@all", "markets@totals"}
	m.runStream(ctx, m.marketsWSBase(), topics, func(raw []byte) error {
		return m.handleAggregateMessage(raw)
	}, func(Health) {}, func() {
		m.marketsDisconnected()
	})
}

func (m *Manager) runStream(
	ctx context.Context,
	wsBase string,
	topics []string,
	onMessage func([]byte) error,
	onPhase func(Health),
	onDisconnect func(),
) {
	backoff := reconnectMinBackoff
	for {
		if ctx.Err() != nil {
			onPhase(HealthStopped)
			return
		}
		onPhase(HealthConnecting)
		gotFrame, err := m.streamOnce(ctx, wsBase, topics, onMessage, onPhase)
		onDisconnect()
		if ctx.Err() != nil {
			onPhase(HealthStopped)
			return
		}
		if err != nil && m.cfg.Logf != nil {
			m.cfg.Logf("crossverse stream %s: %v", wsBase, err)
		}
		if gotFrame {
			backoff = reconnectMinBackoff
		}
		select {
		case <-ctx.Done():
			onPhase(HealthStopped)
			return
		case <-time.After(backoff):
		}
		backoff *= 2
		if backoff > reconnectMaxBackoff {
			backoff = reconnectMaxBackoff
		}
	}
}

func (m *Manager) streamOnce(
	ctx context.Context,
	wsBase string,
	topics []string,
	onMessage func([]byte) error,
	onPhase func(Health),
) (bool, error) {
	wsURL, err := toWSURL(wsBase)
	if err != nil {
		return false, err
	}
	conn, _, err := m.dialer().DialContext(ctx, wsURL, nil)
	if err != nil {
		return false, fmt.Errorf("dial %s: %w", wsURL, err)
	}
	defer conn.Close()

	stop := make(chan struct{})
	defer close(stop)
	go func() {
		select {
		case <-ctx.Done():
			conn.Close()
		case <-stop:
		}
	}()

	if err := conn.SetReadDeadline(time.Now().Add(readDeadline)); err != nil {
		return false, err
	}
	conn.SetPingHandler(func(appData string) error {
		if err := conn.SetReadDeadline(time.Now().Add(readDeadline)); err != nil {
			return err
		}
		return conn.WriteControl(websocket.PongMessage, []byte(appData), time.Now().Add(writeDeadline))
	})

	for _, topic := range topics {
		if err := conn.SetWriteDeadline(time.Now().Add(writeDeadline)); err != nil {
			return false, err
		}
		if err := conn.WriteJSON(subscribeFrame{Event: "subscribe", Topic: topic}); err != nil {
			return false, fmt.Errorf("subscribe %s: %w", topic, err)
		}
	}
	onPhase(HealthAwaitingSnapshot)

	gotFrame := false
	for {
		if err := conn.SetReadDeadline(time.Now().Add(readDeadline)); err != nil {
			return gotFrame, err
		}
		_, raw, err := conn.ReadMessage()
		if err != nil {
			return gotFrame, fmt.Errorf("read: %w", err)
		}
		gotFrame = true
		if err := onMessage(raw); err != nil {
			if errors.Is(err, errResubscribe) {
				return gotFrame, err
			}
			return gotFrame, err
		}
	}
}
