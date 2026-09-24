package daemon

// debug: the frame inspection endpoints, POST /api/debug/dissect and
// POST /api/debug/decrypt.
//
// Both handlers run entirely off the engine goroutine. tado.Install and
// tado.Device are read-only after the config load, and Install.NewDissector
// builds a private reassembler, so each request makes its own session and needs
// neither a d.do() hop nor a lock. The engine keeps its own reassembler for the
// radio: a caller frame must never reach it, because a forged fragment would
// corrupt the reassembly of a real datagram.

import (
	"encoding/binary"
	"encoding/hex"
	"net/http"
	"strconv"

	"freedo/tado"
)

const (
	// debugMaxBody bounds one request body; the caps below bound the work it asks for.
	debugMaxBody   = 1 << 20
	debugMaxFrames = 512
	debugMaxHex    = 1024
)

const debugMICFailure = "MIC failure: wrong key or not a Data frame"

// dissectSession renders the frames of one request. The dissector joins the
// 6LoWPAN fragments of the one-line output and keeps the assembled message to
// itself, so the session runs a second reassembler for the structured fields.
type dissectSession struct {
	key   []byte
	dis   *tado.Dissector
	reasm *tado.Reassembler
}

func (d *daemon) newDissectSession() *dissectSession {
	inst := d.inst.Load()
	return &dissectSession{
		key:   inst.NetworkKey(),
		dis:   inst.NewDissector(),
		reasm: tado.NewReassembler(),
	}
}

// frameView describes one frame: the dissector line plus the structured fields
// the frame class allows. A frame that does not decrypt keeps its class and its
// line and reports decrypted false, which is an answer, not an error.
func (s *dissectSession) frameView(frame []byte) map[string]any {
	class := tado.Classify(frame)
	view := map[string]any{
		"len":       len(frame),
		"class":     class.String(),
		"dissect":   s.dis.Dissect(frame),
		"decrypted": false,
	}
	if class != tado.ClassData && class != tado.ClassDataPending {
		return view
	}
	f, err := tado.ParseData(s.key, frame)
	if err != nil {
		return view
	}
	view["decrypted"] = true
	view["seq"] = f.Seq
	view["src"] = hex.EncodeToString(f.SrcEUI())
	view["dst"] = hex.EncodeToString(f.DstEUI)
	view["plaintext"] = hex.EncodeToString(f.Plaintext)
	if len(f.Plaintext) >= 8 {
		view["counter"] = binary.LittleEndian.Uint32(f.Plaintext[4:8])
	}
	if tado.IsFragment(f.Plaintext) {
		if rs, ok := s.reasm.AddFrame(f); ok && rs.Message != nil {
			fillCoAPView(view, rs.Message, tado.ParseTLVs(rs.Message.Payload))
		}
		return view
	}
	if app := tado.ParseApp(f.Plaintext); app != nil {
		if app.CoAP != nil {
			fillCoAPView(view, app.CoAP, app.TLVs)
		} else {
			view["tlvs"] = tlvViews("", app.TLVs)
		}
	}
	return view
}

func fillCoAPView(view map[string]any, m *tado.CoAPMessage, tlvs []tado.TLV) {
	view["coap"] = map[string]any{
		"type":    m.Type,
		"code":    m.Code,
		"mid":     m.MID,
		"token":   hex.EncodeToString(m.Token),
		"uri":     m.URI(),
		"summary": m.Summary(),
	}
	view["tlvs"] = tlvViews(m.URI(), tlvs)
}

func tlvViews(uri string, tlvs []tado.TLV) []map[string]any {
	res := tado.ResourceOf(uri, tlvs)
	out := make([]map[string]any, 0, len(tlvs))
	for _, t := range tlvs {
		out = append(out, map[string]any{
			"tag":   t.Tag,
			"label": t.TagLabelIn(res),
			"hex":   hex.EncodeToString(t.Val),
		})
	}
	return out
}

type debugFramesBody struct {
	Hex    string   `json:"hex"`
	Frames []string `json:"frames"`
}

// postDebugDissect dissects one frame or a list of them. The whole list shares
// one session, so a datagram fragmented over several frames of the same request
// reassembles and reads as its CoAP message.
func (d *daemon) postDebugDissect(w http.ResponseWriter, r *http.Request) {
	if d.inst.Load() == nil {
		writeResolveErr(w, errNoInstall)
		return
	}
	var b debugFramesBody
	if !decodeBodyLimit(w, r, &b, debugMaxBody) {
		return
	}
	items := make([]string, 0, len(b.Frames)+1)
	if b.Hex != "" {
		items = append(items, b.Hex)
	}
	items = append(items, b.Frames...)
	if len(items) == 0 {
		writeErr(w, http.StatusBadRequest, "hex or frames is required")
		return
	}
	if len(items) > debugMaxFrames {
		writeErr(w, http.StatusBadRequest, "too many frames")
		return
	}
	frames, ok := decodeDebugFrames(w, items)
	if !ok {
		return
	}
	session := d.newDissectSession()
	classes := map[string]int{}
	views := make([]map[string]any, 0, len(frames))
	for i, frame := range frames {
		view := session.frameView(frame)
		view["index"] = i
		classes[view["class"].(string)]++
		views = append(views, view)
	}
	writeJSON(w, http.StatusOK, map[string]any{"classes": classes, "frames": views})
}

// postDebugDecrypt decrypts one frame under the installation key. It takes no
// caller key on purpose: the daemon is not a decryption oracle.
func (d *daemon) postDebugDecrypt(w http.ResponseWriter, r *http.Request) {
	if d.inst.Load() == nil {
		writeResolveErr(w, errNoInstall)
		return
	}
	var b debugFramesBody
	if !decodeBodyLimit(w, r, &b, debugMaxBody) {
		return
	}
	if b.Hex == "" {
		writeErr(w, http.StatusBadRequest, "hex is required")
		return
	}
	frames, ok := decodeDebugFrames(w, []string{b.Hex})
	if !ok {
		return
	}
	view := d.newDissectSession().frameView(frames[0])
	if decrypted, _ := view["decrypted"].(bool); !decrypted {
		view["error"] = debugMICFailure
	}
	writeJSON(w, http.StatusOK, view)
}

func decodeDebugFrames(w http.ResponseWriter, items []string) ([][]byte, bool) {
	frames := make([][]byte, 0, len(items))
	for i, item := range items {
		if len(item) > debugMaxHex {
			writeErr(w, http.StatusBadRequest, "frame "+strconv.Itoa(i)+" is longer than "+strconv.Itoa(debugMaxHex)+" hex characters")
			return nil, false
		}
		frame, err := hex.DecodeString(item)
		if err != nil {
			writeErr(w, http.StatusBadRequest, "frame "+strconv.Itoa(i)+": bad hex: "+err.Error())
			return nil, false
		}
		// Classify reads the 2-byte FCF, the dissectors read the sequence byte too.
		if len(frame) < 3 {
			writeErr(w, http.StatusBadRequest, "frame "+strconv.Itoa(i)+" is shorter than 3 bytes")
			return nil, false
		}
		frames = append(frames, frame)
	}
	return frames, true
}
