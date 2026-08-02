package codingworker

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/sha1"
	"crypto/tls"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

const websocketGUID = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"

type wsConn struct {
	conn         net.Conn
	reader       *bufio.Reader
	writeMu      sync.Mutex
	maxFrame     int64
	expectMasked bool
}

func validateLoopbackURL(raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("parse agentcore endpoint: %w", err)
	}
	if u.Scheme != "ws" && u.Scheme != "wss" {
		return nil, fmt.Errorf("agentcore endpoint must use ws or wss")
	}
	host := strings.TrimSpace(strings.ToLower(u.Hostname()))
	ip := net.ParseIP(host)
	if ip == nil || !ip.IsLoopback() {
		return nil, fmt.Errorf("agentcore endpoint must be loopback, got %q", host)
	}
	if u.User != nil || u.Host == "" {
		return nil, errors.New("agentcore endpoint has invalid authority")
	}
	return u, nil
}

func dialWebSocket(ctx context.Context, raw string, token []byte, timeout time.Duration, maxFrame int64) (*wsConn, error) {
	u, err := validateLoopbackURL(raw)
	if err != nil {
		return nil, err
	}
	if len(token) < 32 {
		return nil, errors.New("agentcore loopback token must contain at least 32 bytes")
	}
	query := u.Query()
	query.Set("token", string(token))
	u.RawQuery = query.Encode()
	port := u.Port()
	if port == "" {
		if u.Scheme == "wss" {
			port = "443"
		} else {
			port = "80"
		}
	}
	dialCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	dialer := &net.Dialer{}
	conn, err := dialer.DialContext(dialCtx, "tcp", net.JoinHostPort(u.Hostname(), port))
	if err != nil {
		return nil, fmt.Errorf("dial agentcore: %w", err)
	}
	if u.Scheme == "wss" {
		tlsConn := tls.Client(conn, &tls.Config{ServerName: u.Hostname(), MinVersion: tls.VersionTLS12})
		if err := tlsConn.HandshakeContext(dialCtx); err != nil {
			conn.Close()
			return nil, fmt.Errorf("agentcore tls handshake: %w", err)
		}
		conn = tlsConn
	}
	keyBytes := make([]byte, 16)
	if _, err := rand.Read(keyBytes); err != nil {
		conn.Close()
		return nil, fmt.Errorf("create websocket key: %w", err)
	}
	key := base64.StdEncoding.EncodeToString(keyBytes)
	path := u.EscapedPath()
	if path == "" {
		path = "/"
	}
	if u.RawQuery != "" {
		path += "?" + u.RawQuery
	}
	req := "GET " + path + " HTTP/1.1\r\n" +
		"Host: " + u.Host + "\r\n" +
		"Upgrade: websocket\r\n" +
		"Connection: Upgrade\r\n" +
		"Sec-WebSocket-Key: " + key + "\r\n" +
		"Sec-WebSocket-Version: 13\r\n\r\n"
	if deadline, ok := dialCtx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	}
	if _, err := io.WriteString(conn, req); err != nil {
		conn.Close()
		return nil, fmt.Errorf("write websocket handshake: %w", err)
	}
	reader := bufio.NewReader(conn)
	resp, err := http.ReadResponse(reader, &http.Request{Method: http.MethodGet})
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("read websocket handshake: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusSwitchingProtocols {
		conn.Close()
		return nil, fmt.Errorf("agentcore websocket rejected: %s", resp.Status)
	}
	wantAcceptRaw := sha1.Sum([]byte(key + websocketGUID))
	wantAccept := base64.StdEncoding.EncodeToString(wantAcceptRaw[:])
	if !strings.EqualFold(strings.TrimSpace(resp.Header.Get("Upgrade")), "websocket") ||
		!headerHasToken(resp.Header.Get("Connection"), "upgrade") ||
		resp.Header.Get("Sec-WebSocket-Accept") != wantAccept {
		conn.Close()
		return nil, errors.New("agentcore websocket returned an invalid upgrade response")
	}
	_ = conn.SetDeadline(time.Time{})
	return &wsConn{conn: conn, reader: reader, maxFrame: maxFrame}, nil
}

func headerHasToken(value, token string) bool {
	for _, part := range strings.Split(value, ",") {
		if strings.EqualFold(strings.TrimSpace(part), token) {
			return true
		}
	}
	return false
}

func (w *wsConn) close() error { return w.conn.Close() }

func (w *wsConn) writeText(payload []byte) error { return w.writeFrame(0x1, payload) }
func (w *wsConn) writePong(payload []byte) error { return w.writeFrame(0xA, payload) }

func (w *wsConn) writeFrame(opcode byte, payload []byte) error {
	w.writeMu.Lock()
	defer w.writeMu.Unlock()
	if int64(len(payload)) > w.maxFrame {
		return fmt.Errorf("websocket payload exceeds %d bytes", w.maxFrame)
	}
	header := []byte{0x80 | opcode, 0x80}
	switch n := len(payload); {
	case n < 126:
		header[1] |= byte(n)
	case n <= 65535:
		header[1] |= 126
		var b [2]byte
		binary.BigEndian.PutUint16(b[:], uint16(n))
		header = append(header, b[:]...)
	default:
		header[1] |= 127
		var b [8]byte
		binary.BigEndian.PutUint64(b[:], uint64(n))
		header = append(header, b[:]...)
	}
	var mask [4]byte
	if _, err := rand.Read(mask[:]); err != nil {
		return err
	}
	header = append(header, mask[:]...)
	masked := make([]byte, len(payload))
	for i := range payload {
		masked[i] = payload[i] ^ mask[i%4]
	}
	if _, err := w.conn.Write(header); err != nil {
		return err
	}
	_, err := w.conn.Write(masked)
	return err
}

func (w *wsConn) readMessage() ([]byte, error) {
	var message []byte
	started := false
	for {
		fin, opcode, payload, err := w.readFrame()
		if err != nil {
			return nil, err
		}
		switch opcode {
		case 0x8:
			return nil, io.EOF
		case 0x9:
			if err := w.writePong(payload); err != nil {
				return nil, err
			}
			continue
		case 0xA:
			continue
		case 0x1:
			if started {
				return nil, errors.New("unexpected websocket text frame")
			}
			started = true
		case 0x0:
			if !started {
				return nil, errors.New("unexpected websocket continuation")
			}
		default:
			return nil, fmt.Errorf("unsupported websocket opcode %d", opcode)
		}
		if int64(len(message)+len(payload)) > w.maxFrame {
			return nil, fmt.Errorf("websocket message exceeds %d bytes", w.maxFrame)
		}
		message = append(message, payload...)
		if fin {
			return message, nil
		}
	}
}

func (w *wsConn) readFrame() (bool, byte, []byte, error) {
	var h [2]byte
	if _, err := io.ReadFull(w.reader, h[:]); err != nil {
		return false, 0, nil, err
	}
	fin := h[0]&0x80 != 0
	if h[0]&0x70 != 0 {
		return false, 0, nil, errors.New("websocket extensions are not supported")
	}
	opcode := h[0] & 0x0f
	masked := h[1]&0x80 != 0
	if masked != w.expectMasked {
		return false, 0, nil, errors.New("websocket frame masking direction is invalid")
	}
	length := int64(h[1] & 0x7f)
	if length == 126 {
		var b [2]byte
		if _, err := io.ReadFull(w.reader, b[:]); err != nil {
			return false, 0, nil, err
		}
		length = int64(binary.BigEndian.Uint16(b[:]))
	} else if length == 127 {
		var b [8]byte
		if _, err := io.ReadFull(w.reader, b[:]); err != nil {
			return false, 0, nil, err
		}
		u := binary.BigEndian.Uint64(b[:])
		if u > uint64(w.maxFrame) {
			return false, 0, nil, fmt.Errorf("websocket frame exceeds %d bytes", w.maxFrame)
		}
		length = int64(u)
	}
	if length < 0 || length > w.maxFrame {
		return false, 0, nil, fmt.Errorf("websocket frame exceeds %d bytes", w.maxFrame)
	}
	if opcode >= 0x8 && (!fin || length > 125) {
		return false, 0, nil, errors.New("invalid websocket control frame")
	}
	var mask [4]byte
	if masked {
		if _, err := io.ReadFull(w.reader, mask[:]); err != nil {
			return false, 0, nil, err
		}
	}
	payload := make([]byte, int(length))
	if _, err := io.ReadFull(w.reader, payload); err != nil {
		return false, 0, nil, err
	}
	if masked {
		for i := range payload {
			payload[i] ^= mask[i%4]
		}
	}
	return fin, opcode, payload, nil
}

func wsHTTPURL(raw string) (string, error) {
	u, err := validateLoopbackURL(raw)
	if err != nil {
		return "", err
	}
	if u.Scheme == "wss" {
		u.Scheme = "https"
	} else {
		u.Scheme = "http"
	}
	u.Path = "/api/health"
	u.RawQuery = ""
	u.Fragment = ""
	return u.String(), nil
}

func portFromEndpoint(raw string) (int, error) {
	u, err := validateLoopbackURL(raw)
	if err != nil {
		return 0, err
	}
	p := u.Port()
	if p == "" {
		if u.Scheme == "wss" {
			return 443, nil
		}
		return 80, nil
	}
	return strconv.Atoi(p)
}
