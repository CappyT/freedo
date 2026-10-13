package daemon

// hass_admin: asks Home Assistant whether a user is an administrator. The
// Supervisor proxies the WebSocket API of Home Assistant Core to an add-on that
// declares homeassistant_api, at ws://supervisor/core/websocket, and the add-on
// signs in with SUPERVISOR_TOKEN. The command config/auth/list returns every
// user. The REST API has no user lookup. The client below speaks the few parts
// of RFC 6455 that one request and one answer need.

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"slices"
	"time"
)

const (
	supervisorAddr    = "supervisor:80"
	coreWebSocketPath = "/core/websocket"
	// hassAdminGroup is GROUP_ID_ADMIN of Home Assistant Core.
	hassAdminGroup    = "system-admin"
	adminCheckTimeout = 5 * time.Second
	// wsMaxMessage bounds one answer of Home Assistant. A user list is a few kilobytes.
	wsMaxMessage = 4 << 20
	wsGUID       = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"
)

// adminChecker reports whether the Home Assistant user with this id is an
// active administrator. An error means that nobody answered the question.
type adminChecker interface {
	IsAdmin(ctx context.Context, userID string) (bool, error)
}

// coreAdmins asks Home Assistant Core through the Supervisor. It keeps no
// answer: an export or an import is rare, and a user who lost the administrator
// group must lose the access at once.
type coreAdmins struct {
	addr  string // host:port of the Supervisor
	token string // SUPERVISOR_TOKEN
}

// hassUser is one entry of config/auth/list.
type hassUser struct {
	ID              string   `json:"id"`
	IsOwner         bool     `json:"is_owner"`
	IsActive        bool     `json:"is_active"`
	SystemGenerated bool     `json:"system_generated"`
	GroupIDs        []string `json:"group_ids"`
}

// admin follows User.is_admin of Home Assistant Core, and is stricter in two
// points: an inactive owner and a system user are not administrators here.
func (u hassUser) admin() bool {
	return u.IsActive && !u.SystemGenerated && (u.IsOwner || slices.Contains(u.GroupIDs, hassAdminGroup))
}

func (c coreAdmins) IsAdmin(ctx context.Context, userID string) (bool, error) {
	if c.token == "" {
		return false, errors.New("no " + supervisorTokenEnv)
	}
	ctx, cancel := context.WithTimeout(ctx, adminCheckTimeout)
	defer cancel()
	users, err := c.users(ctx)
	if err != nil {
		return false, err
	}
	for _, u := range users {
		if u.ID == userID {
			return u.admin(), nil
		}
	}
	return false, nil
}

func (c coreAdmins) users(ctx context.Context) ([]hassUser, error) {
	conn, err := (&net.Dialer{}).DialContext(ctx, "tcp", c.addr)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	}
	ws, err := wsHandshake(conn, c.addr, coreWebSocketPath)
	if err != nil {
		return nil, err
	}
	var msg struct {
		ID      int             `json:"id"`
		Type    string          `json:"type"`
		Success bool            `json:"success"`
		Result  json.RawMessage `json:"result"`
	}
	read := func() error {
		raw, err := ws.read()
		if err != nil {
			return err
		}
		msg.ID, msg.Type, msg.Success, msg.Result = 0, "", false, nil
		return json.Unmarshal(raw, &msg)
	}
	if err := read(); err != nil {
		return nil, err
	}
	if msg.Type != "auth_required" {
		return nil, fmt.Errorf("unexpected first message %q", msg.Type)
	}
	if err := ws.writeJSON(map[string]string{"type": "auth", "access_token": c.token}); err != nil {
		return nil, err
	}
	if err := read(); err != nil {
		return nil, err
	}
	if msg.Type != "auth_ok" {
		return nil, fmt.Errorf("the sign-in was refused (%s): the add-on needs homeassistant_api", msg.Type)
	}
	const id = 1
	if err := ws.writeJSON(map[string]any{"id": id, "type": "config/auth/list"}); err != nil {
		return nil, err
	}
	// Home Assistant can send other messages first: wait for the answer to this id.
	for {
		if err := read(); err != nil {
			return nil, err
		}
		if msg.Type == "result" && msg.ID == id {
			break
		}
	}
	if !msg.Success {
		return nil, errors.New("Home Assistant refused config/auth/list")
	}
	var users []hassUser
	if err := json.Unmarshal(msg.Result, &users); err != nil {
		return nil, fmt.Errorf("user list: %w", err)
	}
	return users, nil
}

// wsConn is the client side of one WebSocket connection.
type wsConn struct {
	conn net.Conn
	r    *bufio.Reader
}

func wsHandshake(conn net.Conn, host, path string) (*wsConn, error) {
	nonce := make([]byte, 16)
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	key := base64.StdEncoding.EncodeToString(nonce)
	req, err := http.NewRequest(http.MethodGet, "http://"+host+path, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Upgrade", "websocket")
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Sec-WebSocket-Key", key)
	req.Header.Set("Sec-WebSocket-Version", "13")
	if err := req.Write(conn); err != nil {
		return nil, err
	}
	r := bufio.NewReader(conn)
	resp, err := http.ReadResponse(r, req)
	if err != nil {
		return nil, err
	}
	resp.Body.Close()
	sum := sha1.Sum([]byte(key + wsGUID))
	if resp.StatusCode != http.StatusSwitchingProtocols ||
		resp.Header.Get("Sec-WebSocket-Accept") != base64.StdEncoding.EncodeToString(sum[:]) {
		return nil, fmt.Errorf("no WebSocket at %s%s: %s", host, path, resp.Status)
	}
	return &wsConn{conn: conn, r: r}, nil
}

const (
	wsText  = 0x1
	wsClose = 0x8
	wsPing  = 0x9
	wsPong  = 0xA
)

// write sends one frame. A client masks every frame (RFC 6455, 5.3).
func (w *wsConn) write(opcode byte, payload []byte) error {
	frame := []byte{0x80 | opcode}
	switch n := len(payload); {
	case n < 126:
		frame = append(frame, 0x80|byte(n))
	case n <= 0xFFFF:
		frame = binary.BigEndian.AppendUint16(append(frame, 0x80|126), uint16(n))
	default:
		frame = binary.BigEndian.AppendUint64(append(frame, 0x80|127), uint64(n))
	}
	var mask [4]byte
	if _, err := rand.Read(mask[:]); err != nil {
		return err
	}
	frame = append(frame, mask[:]...)
	for i, b := range payload {
		frame = append(frame, b^mask[i%4])
	}
	_, err := w.conn.Write(frame)
	return err
}

func (w *wsConn) writeJSON(v any) error {
	raw, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return w.write(wsText, raw)
}

// read returns the next data message. It joins the fragments, answers a ping
// and stops on a close frame.
func (w *wsConn) read() ([]byte, error) {
	var message []byte
	for {
		var head [2]byte
		if _, err := io.ReadFull(w.r, head[:]); err != nil {
			return nil, err
		}
		fin, opcode := head[0]&0x80 != 0, head[0]&0x0F
		if head[0]&0x70 != 0 {
			return nil, errors.New("websocket: a frame uses an extension that was not asked for")
		}
		size := uint64(head[1] & 0x7F)
		switch size {
		case 126:
			var b [2]byte
			if _, err := io.ReadFull(w.r, b[:]); err != nil {
				return nil, err
			}
			size = uint64(binary.BigEndian.Uint16(b[:]))
		case 127:
			var b [8]byte
			if _, err := io.ReadFull(w.r, b[:]); err != nil {
				return nil, err
			}
			size = binary.BigEndian.Uint64(b[:])
		}
		if size > wsMaxMessage || uint64(len(message))+size > wsMaxMessage {
			return nil, errors.New("websocket: the message is too long")
		}
		var mask [4]byte
		masked := head[1]&0x80 != 0
		if masked {
			if _, err := io.ReadFull(w.r, mask[:]); err != nil {
				return nil, err
			}
		}
		payload := make([]byte, size)
		if _, err := io.ReadFull(w.r, payload); err != nil {
			return nil, err
		}
		if masked {
			for i := range payload {
				payload[i] ^= mask[i%4]
			}
		}
		switch opcode {
		case wsPing:
			if err := w.write(wsPong, payload); err != nil {
				return nil, err
			}
		case wsPong:
		case wsClose:
			return nil, errors.New("websocket: the server closed the connection")
		default:
			message = append(message, payload...)
			if fin {
				return message, nil
			}
		}
	}
}
