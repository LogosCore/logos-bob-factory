// Command minion is the program BOB compiles for its linux-elf target: a
// dummy minion. BOB writes the build's description to minion.json before
// compiling, so it is baked into the binary (the minion.json kept here is a
// placeholder). Run, the minion stages with core through the first of its
// connections it can use (conns.go), says hello to open a session, then checks in with a get_tasks
// every check-in interval of the connection, with its jitter, and logs each
// step (docs: contracts/minion-messages.md). It keeps its session through
// failures on the way, and starts over when core closes the session, after
// 24 hours of nothing but failures, or when staging or the hello fails.
// "minion describe" prints the description instead.
package main

import (
	"bytes"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math/big"
	mrand "math/rand/v2"
	"net"
	"net/http"
	"os"
	"os/user"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"time"

	"github.com/logoscore/logos-golang-protocol/protocol"
)

//go:embed minion.json
var minion []byte

// version is the minion implementation's own, sent in its hello.
const version = "0.2.0"

// description is what the minion reads of the description it was built with.
type description struct {
	BuildID     string                     `json:"build_id"`
	Crypto      *protocol.BuildCrypto      `json:"crypto"`
	Connections []protocol.BuildConnection `json:"connections"`
	// SessionID is the persisted session to claim; empty for none.
	SessionID string `json:"session_id"`
	// Ops are the numbers of the commands the minion was built with
	// (ops.go); Greeting what its hello command answers.
	Ops      []uint64 `json:"ops"`
	Greeting string   `json:"greeting"`
}

func main() {
	if len(os.Args) > 1 && os.Args[1] == "describe" {
		_, _ = os.Stdout.Write(minion)
		return
	}
	log.SetFlags(log.Ltime | log.Lmicroseconds)
	log.SetPrefix("minion ")
	// run goes on until the process is stopped: it returns only when it
	// cannot start (a description or keys it cannot read).
	log.Printf("failed: %v", run())
	os.Exit(1)
}

// run stages, says hello under the temporary id with the new keys, and
// checks in once under the session id.
func run() error {
	var d description
	if err := json.Unmarshal(minion, &d); err != nil {
		return fmt.Errorf("read the description: %w", err)
	}
	buildID, err := parseUUID(d.BuildID)
	if err != nil {
		return fmt.Errorf("build id: %w", err)
	}
	if d.Crypto == nil || d.Crypto.Suite != protocol.CryptoAES256CBCHMACSHA256 {
		return errors.New("built without the aes-256-cbc-hmac-sha256 keys")
	}
	var keys protocol.AES256CBCHMACSHA256Material
	if err := json.Unmarshal(d.Crypto.Material, &keys); err != nil {
		return fmt.Errorf("the build's keys: %w", err)
	}
	tr, err := newTransport(d.Connections)
	if err != nil {
		return err
	}
	// The schedule is the first connection's; a connect leaves it as it is.
	sched := tr.conn().schedule()
	log.Printf("build %s, crypto %s", d.BuildID, d.Crypto.Suite)
	// A reverse-persistent connection keeps a WebSocket open: core pushes
	// tasks over it; check-ins go on, on the schedule, to restate the
	// allowance.
	log.Printf("%s of %d, check-in every %s ± %g%%", tr.describe(), len(tr.conns), sched.interval, sched.jitter)

	// The persisted session the build claims, if it has one: given up for
	// this process once another process takes it (session_replaced).
	var claim []byte
	if d.SessionID != "" {
		sid, err := parseUUID(d.SessionID)
		if err != nil {
			return fmt.Errorf("session id: %w", err)
		}
		claim = sid[:]
		log.Printf("persisted session %s: claimed in every hello", d.SessionID)
	}

	// Starting over: a stage or hello that fails, core closing the session,
	// another process claiming it, or the backstop, stages again after a
	// check-in interval.
	for {
		err := session(tr, sched, d, buildID, keys, claim)
		if errors.Is(err, errReplaced) && claim != nil {
			claim = nil
			log.Printf("another process holds the persisted session: this one goes on in a session of its own")
		}
		wait := sched.next()
		log.Printf("%v; starting over in %s", err, wait.Round(time.Millisecond))
		time.Sleep(wait)
	}
}

// session stages, says hello and checks in for as long as that works; it
// returns why it stopped.
// exchanger sends a message under id and returns core's reply: through a
// request, or over the WebSocket.
type exchanger func(id [16]byte, keys protocol.AES256CBCHMACSHA256Material, items ...any) (protocol.CoreMessage, error)

func session(tr *transport, sched schedule, d description, buildID [16]byte, keys protocol.AES256CBCHMACSHA256Material, claim []byte) error {
	// A new session starts on a new WebSocket.
	defer tr.closeLink()
	ex := tr.exchange
	tempID, newKeys, err := stage(ex, buildID, keys)
	if err != nil {
		return fmt.Errorf("stage: %w", err)
	}
	sessionID, err := hello(ex, tempID, newKeys, claim, sched, tr.place())
	if err != nil {
		return fmt.Errorf("hello: %w", err)
	}
	// Check in every interval, with jitter, or sooner when a task has output:
	// with a get_tasks for one task when none runs, and what the tasks said
	// (tasks.go). The session's keys live only in this process, so a failure
	// on the way (the channel out of reach, its 404, a reply that does not
	// open) keeps the session, and the message goes again as it was: only
	// core's sealed session_closed, or the backstop, starts over (docs:
	// "When an exchange fails").
	tasks := newRunner(d, &sched)
	// A WebSocket that closed is opened again at once, then after 1s,
	// doubling up to the endpoint's ws_reconnect_backoff: the check-in that
	// reopens it links it to the session again.
	backoff := time.Second
	lastOK := time.Now()
	// retrying: the last exchange failed, so the next goes on the schedule
	// even over a WebSocket.
	retrying := false
	// back is the move back a connect with a duration owes; nil for none.
	var back *revert
	// answered takes core's answer to items: false when the session ends
	// (err says why).
	answered := func(n int, items []any, answer protocol.CoreMessage) error {
		lastOK = time.Now()
		backoff = time.Second
		if tasks.delivered(items) {
			log.Printf("check-in %d: the exit task's end is delivered: exiting", n)
			os.Exit(0)
		}
		if e, ok := coreErrItem(answer); ok {
			switch e.Code {
			case protocol.ErrItemSessionClosed:
				return fmt.Errorf("check-in %d: core closed the session", n)
			case protocol.ErrItemSessionReplaced:
				return fmt.Errorf("check-in %d: %w", n, errReplaced)
			}
			// About one of the items (a chunk core does not take): the
			// tasks see it with the rest of the answer.
			log.Printf("check-in %d: core says err %d: %s; keeping the session", n, e.Code, e.Msg)
		}
		tasks.take(answer, items)
		return nil
	}
	// move goes to the connection at place: a check-in through it, with a
	// conn_update saying why, taken like any check-in's answer (on a
	// WebSocket it links it and sets the allowance at once). It fails,
	// changing nothing, when the new connection does not answer: what the
	// check-in carried goes again through the one in use.
	move := func(n, place int, why string) error {
		from := tr.place()
		rest := tasks.outgoing()
		update := protocol.ConnUpdateItem{T: protocol.ItemConnUpdate, Conn: uint64(place), Reason: why} //nolint:gosec // a small place
		items := append([]any{update}, rest...)
		answer, err := tr.try(place, sessionID, newKeys, items...)
		if err != nil {
			tasks.failed(rest)
			return fmt.Errorf("connection %d did not answer, staying on %d: %w", place, from, err)
		}
		logSwitch(from, place, why)
		log.Printf("now on %s", tr.describe())
		return answered(n, items, answer)
	}
	// switchTo carries out a connect: the session's own errors end it.
	var ended error
	switchTo := func(n int, req switchReq) {
		if err := tr.check(req.place); err != nil {
			req.done <- err
			return
		}
		from := tr.place()
		why := "operator"
		if req.dur > 0 {
			why = fmt.Sprintf("operator, for %s", req.dur)
		}
		err := move(n, req.place, why)
		switch {
		case err == nil:
			back = nil
			if req.dur > 0 {
				back = &revert{place: from, at: time.Now().Add(req.dur), after: req.dur}
			}
			req.done <- nil
		case tr.place() == req.place:
			// Moved, and then the session ended.
			req.done <- nil
			ended = err
		default:
			req.done <- err
		}
	}
	for n := 1; ; n++ {
		if ended != nil {
			return ended
		}
		// A connect waiting, or a move back that is due, goes first.
		select {
		case req := <-tasks.switches:
			switchTo(n, req)
			continue
		default:
		}
		if back != nil && !time.Now().Before(back.at) {
			if err := move(n, back.place, fmt.Sprintf("back after %s", back.after)); err != nil {
				if tr.place() == back.place {
					return err
				}
				log.Printf("moving back: %v; trying again at the next check-in", err)
				back.at = time.Now().Add(sched.next())
			} else {
				back = nil
			}
			continue
		}
		// On a WebSocket the first check-in goes at once: it links the
		// connection to the session and sets the allowance core pushes
		// tasks within. After that, while it stays open, nothing goes on a
		// schedule: only what there is to send (output, acks), what core
		// pushes, and a failed exchange's retry.
		if !tasks.pending() && (tr.ws == nil || n > 1) {
			wait := sched.next()
			if p := tasks.pollEvery(); p > 0 && p < wait {
				wait = p
			}
			var due <-chan time.Time
			if back != nil {
				due = time.After(time.Until(back.at))
			}
			scheduled := tr.ws == nil || retrying || tasks.pollEvery() > 0
			if scheduled {
				log.Printf("next check-in in %s", wait.Round(time.Millisecond))
			} else {
				log.Printf("WebSocket open: no check-in until there is something to send")
			}
			timer := time.NewTimer(wait)
			if !scheduled {
				timer.Stop()
			}
		waiting:
			for {
				select {
				case <-timer.C:
					break waiting
				case <-tasks.wake:
					timer.Stop()
					break waiting
				case <-due:
					timer.Stop()
					break waiting
				case req := <-tasks.switches:
					timer.Stop()
					switchTo(n, req)
					break waiting
				case <-tr.lost():
					timer.Stop()
					// It worked until now: the backstop counts from here.
					lastOK = time.Now()
					log.Printf("the WebSocket closed: connecting again in %s", backoff)
					time.Sleep(backoff)
					backoff = min(2*backoff, tr.conn().wsBackoff())
					break waiting
				case push := <-tr.pushes():
					// What core starts over the WebSocket: tasks within the
					// allowance, cancels. Their acks go with the next message.
					if e, ok := coreErrItem(push); ok && e.Code == protocol.ErrItemSessionClosed {
						timer.Stop()
						return errors.New("core closed the session")
					}
					tasks.take(push, nil)
					// A task is acknowledged on receipt: its ack goes now.
					if tasks.pending() {
						timer.Stop()
						break waiting
					}
				}
			}
		}
		if ended != nil || (back != nil && !time.Now().Before(back.at)) {
			continue
		}
		// A connect that came meanwhile goes first.
		if len(tasks.switches) > 0 {
			continue
		}
		items := tasks.outgoing()
		answer, err := tr.exchange(sessionID, newKeys, items...)
		if err != nil {
			tasks.failed(items)
			retrying = true
			if since := time.Since(lastOK); since >= backstop {
				return fmt.Errorf("check-in %d: nothing has worked for %s: %w", n, since.Round(time.Second), err)
			}
			log.Printf("check-in %d failed, keeping the session: %v", n, err)
			// Not at once again: the channel, or core behind it, may be out
			// of reach. Over a WebSocket, as a reconnect: 1s, doubling up to
			// ws_reconnect_backoff (no schedule runs while it is up); through
			// requests, the check-in schedule.
			if tr.ws != nil {
				// The failed attempt's own close says nothing new.
				select {
				case <-tr.lost():
				default:
				}
				log.Printf("trying again in %s", backoff)
				time.Sleep(backoff)
				backoff = min(2*backoff, tr.conn().wsBackoff())
			} else {
				time.Sleep(sched.next())
			}
			continue
		}
		retrying = false
		if err := answered(n, items, answer); err != nil {
			return err
		}
		if asked := slices.ContainsFunc(items, func(it any) bool { _, ok := it.(protocol.GetTasksItem); return ok }); asked {
			log.Printf("check-in %d: get_tasks max 1, %d item(s) sent, %d back", n, len(items), len(answer.Items))
		} else {
			log.Printf("check-in %d: a task runs, %d item(s) sent, %d back", n, len(items), len(answer.Items))
		}
	}
}

// errReplaced: another process claimed the persisted session.
var errReplaced = errors.New("another process claimed the session")

// backstop is how long the minion keeps a session whose exchanges all fail:
// core may have forgotten it, and cannot say so.
const backstop = 24 * time.Hour

// schedule is when the minion checks in: every interval, spread at random
// by up to jitter percent of it either way.
type schedule struct {
	interval time.Duration
	jitter   float64
}

// next is the wait before the next check-in.
func (s schedule) next() time.Duration {
	spread := float64(s.interval) * s.jitter / 100
	return time.Duration(max(0, float64(s.interval)+(mrand.Float64()*2-1)*spread))
}

// stage sends the minion's first message, a stage, under the build id with
// the build's keys, and returns the temporary id and the new keys.
func stage(ex exchanger, buildID [16]byte, keys protocol.AES256CBCHMACSHA256Material) ([16]byte, protocol.AES256CBCHMACSHA256Material, error) {
	var tempID [16]byte
	var newKeys protocol.AES256CBCHMACSHA256Material

	// 1–2. A key pair of the minion's own, and a correlation id.
	start := time.Now()
	priv, err := rsa.GenerateKey(rand.Reader, protocol.StageKeySize)
	if err != nil {
		return tempID, newKeys, err
	}
	pub, _ := x509.MarshalPKIXPublicKey(&priv.PublicKey)
	corr := protocol.NewMessageID()
	log.Printf("generated an RSA-%d key pair in %s; public key sha256 %s", protocol.StageKeySize,
		time.Since(start).Round(time.Millisecond), fingerprint(pub))
	log.Printf("correlation id %x", corr)

	// 3. The stage, sealed with the build's keys, under the build id.
	answer, err := ex(buildID, keys, protocol.StageItem{T: protocol.ItemStage, Key: pub, Corr: corr})
	if err != nil {
		return tempID, newKeys, err
	}
	if err := coreErr(answer); err != nil {
		return tempID, newKeys, err
	}

	// 4–6. Core's answer: a temporary id and new keys, encrypted to ours.
	raw := item(answer, protocol.ItemStageResponse)
	if raw == nil {
		return tempID, newKeys, errors.New("core's reply has no stage_response")
	}
	var resp protocol.StageResponseItem
	if err := protocol.UnmarshalCBOR(raw, &resp); err != nil {
		return tempID, newKeys, fmt.Errorf("stage_response: %w", err)
	}
	if !bytes.Equal(resp.Corr, corr) {
		return tempID, newKeys, errors.New("stage_response: another correlation id")
	}
	if len(resp.TID) != 16 {
		return tempID, newKeys, errors.New("stage_response: the temporary id is not 16 bytes")
	}
	material, err := protocol.DecryptStageKeys(priv, resp.Keys)
	if err != nil {
		return tempID, newKeys, fmt.Errorf("decrypt the new keys: %w", err)
	}
	if err := protocol.UnmarshalCBOR(material, &newKeys); err != nil || len(newKeys.EncKey) != 32 || len(newKeys.MACKey) != 32 {
		return tempID, newKeys, fmt.Errorf("the new keys do not read: %v", err)
	}
	copy(tempID[:], resp.TID)
	log.Printf("stage_response: correlation id matches")
	log.Printf("temporary id %s", formatUUID(resp.TID))
	log.Printf("new keys decrypted with our private key: enc_key sha256 %s, mac_key sha256 %s",
		fingerprint(newKeys.EncKey), fingerprint(newKeys.MACKey))
	log.Printf("staged")
	return tempID, newKeys, nil
}

// hello sends the host and process under the temporary id, sealed with the
// new keys, with its check-in schedule, claiming the persisted session when
// claim is set, and returns the session id core gave.
func hello(ex exchanger, tempID [16]byte, keys protocol.AES256CBCHMACSHA256Material, claim []byte, sched schedule, conn int) ([16]byte, error) {
	var sessionID [16]byte
	h := describeHost()
	h.SID = claim
	// The connection it says hello through, by its place in the build's.
	h.Conn = uint64(conn) //nolint:gosec // a small place
	// The schedule it checks in by, for core to tell when it is late.
	h.Sleep = &protocol.Sleep{Interval: uint64(max(1, sched.interval.Milliseconds())), Jitter: uint64(sched.jitter)}
	log.Printf("hello: pid %d, %s/%s, host %q, user %q, integrity %d, addrs %v", h.PID, h.OS, h.Arch, h.Host, h.User, h.Integrity, h.Addrs)
	answer, err := ex(tempID, keys, h)
	if err != nil {
		return sessionID, err
	}
	if err := coreErr(answer); err != nil {
		return sessionID, err
	}
	raw := item(answer, protocol.ItemHelloResponse)
	if raw == nil {
		return sessionID, errors.New("core's reply has no hello_response")
	}
	var resp protocol.HelloResponseItem
	if err := protocol.UnmarshalCBOR(raw, &resp); err != nil || len(resp.SID) != 16 {
		return sessionID, fmt.Errorf("hello_response does not read: %v", err)
	}
	copy(sessionID[:], resp.SID)
	switch {
	case claim == nil:
		log.Printf("session id %s", formatUUID(resp.SID))
	case bytes.Equal(resp.SID, claim):
		log.Printf("session id %s: the persisted session, claimed", formatUUID(resp.SID))
	default:
		log.Printf("session id %s: core did not give the persisted session, a new one instead", formatUUID(resp.SID))
	}
	return sessionID, nil
}

// describeHost is the hello: what the minion can learn of its host and
// process.
func describeHost() protocol.HelloItem {
	h := protocol.HelloItem{T: protocol.ItemHello, PID: uint64(os.Getpid()), OS: runtime.GOOS, Arch: runtime.GOARCH,
		Version: version, Integrity: protocol.IntegrityUser}
	h.Host, _ = os.Hostname()
	h.MachineSource, h.MachineValue = machineID()
	if u, err := user.Current(); err == nil {
		h.User = u.Username
	}
	// The process's name, as the contract has it: not the path, which says
	// only where it was saved.
	if exe, err := os.Executable(); err == nil {
		h.Proc = filepath.Base(exe)
	}
	if os.Geteuid() == 0 {
		h.Integrity = protocol.IntegrityRoot
	}
	if addrs, err := net.InterfaceAddrs(); err == nil {
		for _, a := range addrs {
			if ipn, ok := a.(*net.IPNet); ok && !ipn.IP.IsLoopback() && !ipn.IP.IsLinkLocalUnicast() {
				ip := ipn.IP
				if v4 := ip.To4(); v4 != nil {
					ip = v4
				}
				h.Addrs = append(h.Addrs, ip)
			}
		}
	}
	return h
}

// exchange sends one message with items under id, sealed with keys, and
// returns core's answer to it: a message with no items when core had
// nothing to say.
func exchange(conn connection, id [16]byte, keys protocol.AES256CBCHMACSHA256Material, items ...any) (protocol.CoreMessage, error) {
	var answer protocol.CoreMessage
	msgID, frame, plainLen, err := seal(id, keys, items)
	if err != nil {
		return answer, err
	}
	body, err := pack(id, frame)
	if err != nil {
		return answer, err
	}
	log.Printf("message %x under %s: %d bytes of CBOR, a %d-byte sealed frame, %d bytes packed for the channel",
		msgID, formatUUID(id[:]), plainLen, len(frame), len(body))

	reply, status, took, err := conn.send(body)
	if err != nil {
		return answer, err
	}
	log.Printf("channel answered %d with %d bytes in %s", status, len(reply), took.Round(time.Millisecond))
	if status == http.StatusNoContent {
		return answer, nil
	}
	if status != http.StatusOK {
		return answer, fmt.Errorf("the channel answered %d: core refused the message, or could not be reached", status)
	}
	replyID, sealed, err := unpack(reply)
	if err != nil {
		return answer, err
	}
	if replyID != id {
		return answer, fmt.Errorf("the reply is for %x, not %x", replyID, id)
	}
	if len(sealed) == 0 {
		// The channel's empty reply: core had nothing to say (a check-in,
		// or a message it took as a replay).
		return answer, nil
	}
	plain, err := protocol.OpenFrame(keys, id, protocol.FrameCoreToMinion, sealed)
	if err != nil {
		return answer, fmt.Errorf("core's reply: %w", err)
	}
	if err := protocol.UnmarshalCBOR(plain, &answer); err != nil {
		return answer, fmt.Errorf("core's reply: %w", err)
	}
	if err := protocol.ValidateMessageHead(answer.V, answer.ID, answer.Re, len(answer.Items)); err != nil {
		return answer, fmt.Errorf("core's reply: %w", err)
	}
	if !bytes.Equal(answer.Re, msgID) {
		return answer, fmt.Errorf("core's reply answers %x, not our message", answer.Re)
	}
	log.Printf("opened core's reply: %d bytes of CBOR, %d item(s)", len(plain), len(answer.Items))
	return answer, nil
}

// seal makes a minion message of items under id, sealed with keys: its id,
// the sealed frame, and the plaintext's size.
func seal(id [16]byte, keys protocol.AES256CBCHMACSHA256Material, items []any) ([]byte, []byte, int, error) {
	msgID := protocol.NewMessageID()
	msg := protocol.MinionMessage{V: protocol.MinionMessageV3, ID: msgID}
	for _, it := range items {
		raw, err := protocol.MarshalCBOR(it)
		if err != nil {
			return nil, nil, 0, err
		}
		msg.Items = append(msg.Items, raw)
	}
	plain, err := protocol.MarshalCBOR(msg)
	if err != nil {
		return nil, nil, 0, err
	}
	frame, err := protocol.SealFrame(keys, id, protocol.FrameMinionToCore, plain)
	return msgID, frame, len(plain), err
}

// coreErr is the first err item of core's answer, as an error.
func coreErr(answer protocol.CoreMessage) error {
	if e, ok := coreErrItem(answer); ok {
		return fmt.Errorf("core says err %d: %s", e.Code, e.Msg)
	}
	return nil
}

// coreErrItem is the first err item of core's answer.
func coreErrItem(answer protocol.CoreMessage) (protocol.ErrItem, bool) {
	var e protocol.ErrItem
	raw := item(answer, protocol.ItemErr)
	if raw == nil {
		return e, false
	}
	_ = protocol.UnmarshalCBOR(raw, &e)
	return e, true
}

// item is the answer's first item of type t, or nil.
func item(answer protocol.CoreMessage, t int) []byte {
	for _, raw := range answer.Items {
		if protocol.ItemType(raw) == t {
			return raw
		}
	}
	return nil
}

// connection is the minion's HTTP connection: the channel's settings and
// BOB's own parameters for it.
type connection struct {
	protocol.BuildConnection
}

func (c connection) param(key string) string {
	s, _ := c.Endpoint.Params[key].(string)
	return s
}

// schedule is the endpoint's check-in interval and jitter, or the HTTP
// channel's defaults (60s, 20%) where it has none.
func (c connection) schedule() schedule {
	s := schedule{interval: 60 * time.Second, jitter: 20}
	if d, err := time.ParseDuration(c.param("checkin_interval")); err == nil && d > 0 {
		s.interval = d
	}
	if j, ok := c.Endpoint.Params["checkin_jitter"].(float64); ok && j >= 0 && j <= 100 {
		s.jitter = j
	}
	return s
}

func (c connection) method() string {
	if m, _ := c.Values["method"].(string); m != "" {
		return m
	}
	return http.MethodPost
}

// url is the base URL with BOB's path prefix.
func (c connection) url() string {
	prefix, _ := c.Values["path_prefix"].(string)
	if prefix == "" {
		prefix = "/"
	}
	return strings.TrimRight(c.param("base_url"), "/") + prefix
}

// wsBackoff is the endpoint's longest wait between WebSocket reconnects.
func (c connection) wsBackoff() time.Duration {
	if d, err := time.ParseDuration(c.param("ws_reconnect_backoff")); err == nil && d > 0 {
		return d
	}
	return 60 * time.Second
}

// timeout is the endpoint's request timeout: how long the minion waits for
// a reply.
func (c connection) timeout() time.Duration {
	if d, err := time.ParseDuration(c.param("request_timeout")); err == nil && d > 0 {
		return d
	}
	return 40 * time.Second
}

// send posts body to the channel as the connection's settings say.
func (c connection) send(body []byte) ([]byte, int, time.Duration, error) {
	timeout := c.timeout()
	verify, _ := c.Endpoint.Params["tls_verify"].(bool)
	client := &http.Client{Timeout: timeout, Transport: &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: !verify, ServerName: c.param("sni")}, //nolint:gosec // the endpoint's own setting
	}}
	req, err := http.NewRequest(c.method(), c.url(), bytes.NewReader(body))
	if err != nil {
		return nil, 0, 0, err
	}
	req.Header.Set("Content-Type", "application/octet-stream")
	if h := c.param("host_header"); h != "" {
		req.Host = h
	}
	if ua := c.param("user_agent"); ua != "" {
		req.Header.Set("User-Agent", ua)
	}
	if headers, ok := c.Endpoint.Params["headers"].(map[string]any); ok {
		for k, v := range headers {
			if s, ok := v.(string); ok {
				req.Header.Set(k, s)
			}
		}
	}
	start := time.Now()
	resp, err := client.Do(req)
	if err != nil {
		return nil, 0, 0, fmt.Errorf("reach the channel: %w", err)
	}
	defer func() { _ = resp.Body.Close() }() // read to its end, or the exchange failed anyway
	reply, err := io.ReadAll(io.LimitReader(resp.Body, protocol.MaxMinionMessageSize+1024))
	return reply, resp.StatusCode, time.Since(start), err
}

// pack is the HTTP channel's message layout: [offset][part 1][id][part 2],
// the id hidden at a random offset in the ciphertext.
func pack(id [16]byte, frame []byte) ([]byte, error) {
	n, err := rand.Int(rand.Reader, big.NewInt(int64(min(len(frame), 255)+1)))
	if err != nil {
		return nil, err
	}
	off := int(n.Int64())
	out := make([]byte, 0, 1+len(frame)+16)
	out = append(out, byte(off))
	out = append(out, frame[:off]...)
	out = append(out, id[:]...)
	return append(out, frame[off:]...), nil
}

// unpack undoes pack on a reply.
func unpack(body []byte) ([16]byte, []byte, error) {
	var id [16]byte
	if len(body) < 17 || 1+int(body[0])+16 > len(body) {
		return id, nil, fmt.Errorf("the reply is not a packed message (%d bytes)", len(body))
	}
	off := int(body[0])
	copy(id[:], body[1+off:1+off+16])
	return id, append(append([]byte{}, body[1:1+off]...), body[1+off+16:]...), nil
}

func parseUUID(s string) ([16]byte, error) {
	var id [16]byte
	raw, err := hex.DecodeString(strings.ReplaceAll(s, "-", ""))
	if err != nil || len(raw) != 16 {
		return id, fmt.Errorf("%q is not a UUID", s)
	}
	copy(id[:], raw)
	return id, nil
}

func formatUUID(b []byte) string {
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// fingerprint is the first 8 bytes of b's SHA-256, for logs: keys are never
// printed.
func fingerprint(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:8])
}
