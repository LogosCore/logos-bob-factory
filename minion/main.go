// Command minion is the program BOB compiles for its targets: a dummy
// minion. BOB writes the build's minion config (core's, numbered CBOR:
// protocol.MinionConfig) to minion.cfg before compiling, so it is baked into
// the binary as it came (the empty minion.cfg kept here is a placeholder),
// and the minion reads it by numbers alone (fields.go). Run, the minion
// stages with core through the first of its
// connections it can use (conns.go), says hello to open a session, then checks in with a get_tasks
// every check-in interval of the connection, with its jitter, and logs each
// step (docs: contracts/minion-messages.md). It keeps its session through
// failures on the way, and starts over when core closes the session, after
// 24 hours of nothing but failures, or when staging or the hello fails.
// "minion describe" prints its config instead, the bytes as they are.
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
	"sync"
	"time"

	"github.com/logoscore/logos-golang-protocol/protocol"
)

//go:embed minion.cfg
var minion []byte

// version is the minion implementation's own, sent in its hello.
const version = "0.2.0"

// description is what the minion reads of the config it was built with
// (readDescription).
type description struct {
	Build       [16]byte
	Keys        protocol.AES256CBCHMACSHA256Material
	Connections []protocol.MinionConnection
	// Session is the persisted session to claim; nil for none.
	Session []byte
	// Ops are the numbers of the commands the minion was built with
	// (ops.go); Greeting what its hello command answers.
	Ops      []uint64
	Greeting string
	// Failover is the build's failover policy (failover.go); nil: the
	// defaults.
	Failover *protocol.MinionFailover
	// MaxTasks is how many tasks the minion runs at once as it starts (core
	// changes it live: max_tasks); nil: protocol.DefaultMaxTasks.
	MaxTasks *uint64
	// Packs says the minion packs its items (packing.go): the build has
	// compression on.
	Packs bool
}

// readDescription reads the minion's config: built with core's keys, as
// BOB's builds are.
func readDescription(raw []byte) (description, error) {
	c, err := protocol.ReadMinionConfig(raw)
	if err != nil {
		return description{}, err
	}
	if c.Crypto == nil || c.Crypto.EncKey == nil {
		return description{}, errors.New("built without core's keys")
	}
	d := description{
		Keys: c.Crypto.Keys(), Connections: c.Connections, Session: c.Session, Ops: c.Ops,
		Failover: c.Failover, MaxTasks: c.MaxTasks, Packs: c.Compression == protocol.PackDeflate,
	}
	copy(d.Build[:], c.Build)
	d.Greeting, _ = protocol.ArgString(c.Values, fieldGreeting)
	return d, nil
}

// maxTasks is how many tasks the build says the minion runs at once.
func (d description) maxTasks() uint64 {
	if d.MaxTasks == nil {
		return protocol.DefaultMaxTasks
	}
	return min(*d.MaxTasks, protocol.MaxTasksLimit)
}

func main() {
	if len(os.Args) > 1 && os.Args[1] == "describe" {
		_, _ = os.Stdout.Write(minion)
		return
	}
	log.SetFlags(log.Ltime | log.Lmicroseconds)
	log.SetPrefix("minion ")
	// run goes on until the process is stopped: it returns only when it
	// cannot start (a config it cannot read).
	log.Printf("failed: %v", run())
	os.Exit(1)
}

// run stages, says hello under the temporary id with the new keys, and
// checks in once under the session id.
func run() error {
	d, err := readDescription(minion)
	if err != nil {
		return fmt.Errorf("read the config: %w", err)
	}
	buildID, keys := d.Build, d.Keys
	tr, err := newTransport(d.Connections)
	if err != nil {
		return err
	}
	packs = d.Packs
	// The schedule is the first connection's; a connect leaves it as it is.
	sched := tr.conn().schedule()
	log.Printf("build %s", formatUUID(buildID[:]))
	// A reverse-persistent connection keeps a WebSocket open: core pushes
	// tasks over it; check-ins go on, on the schedule, to restate the
	// allowance.
	log.Printf("%s of %d, check-in every %s ± %g%%", tr.describe(), len(tr.conns), sched.interval, sched.jitter)

	// The persisted session the build claims, if it has one: given up for
	// this process once another process takes it (session_replaced).
	var claim []byte
	if d.Session != nil {
		claim = d.Session
		log.Printf("persisted session %s: claimed in every hello", formatUUID(claim))
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
// awaitDial is a switch to a connection the minion listens on: its port
// open (link), until core's dial, or until; from is the connection the
// minion was on.
type awaitDial struct {
	req   switchReq
	from  int
	link  *tcpLink
	until time.Time
}

// fail ends the switch: its task fails with err.
func (a *awaitDial) fail(err error) {
	select {
	case a.req.done <- err:
	default:
	}
}

// cancelled reports whether the switch's task was stopped.
func (a *awaitDial) cancelled() bool { return a.req.ctx != nil && a.req.ctx.Err() != nil }

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
	sessionID, err := hello(ex, tempID, newKeys, claim, sched, tr.place(), d.maxTasks())
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
	// Its tunnels' connections go with the session, and its delegate
	// tasks'.
	defer tasks.tun.closeAll()
	defer tasks.dlg.closeAll()
	// Feedback reported meanwhile wakes this session's runner.
	feedback.setWake(tasks.nudge)
	defer feedback.setWake(nil)
	// Check-ins failing in a row: said once one works again (until then
	// nothing reaches core).
	var streak failStreak
	// A hello core asks for (rehello) says the host and process again,
	// through the connection in use.
	tasks.hello = func() protocol.HelloItem {
		h := describeHost()
		h.Conn = uint64(tr.place()) //nolint:gosec // a small place
		h.Sleep = &protocol.Sleep{Interval: uint64(max(1, sched.interval.Milliseconds())), Jitter: uint64(sched.jitter)}
		// Its limit of tasks outgoing adds: it calls this under the
		// runner's lock.
		return h
	}
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
	// awaiting is a switch to a connection the minion listens on, waiting
	// for core's dial; nil for none.
	var awaiting *awaitDial
	// fo moves the minion between its connections on its own, as the
	// build's failover policy says (failover.go).
	fo := newFailover(d.Failover, tr.conns)
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
		if tr.conns[req.place-1].bound() {
			// Core reaches it: open its port, say so, and go on as before
			// until the dial comes (awaitDial, below).
			if awaiting != nil {
				awaiting.fail(fmt.Errorf("a switch to connection %d came first", req.place))
			}
			link := tr.listen(req.place)
			awaiting = &awaitDial{req: req, from: from, link: link, until: time.Now().Add(listenWait)}
			log.Printf("switch to connection %d: listening on %s, waiting for core's dial", req.place, tr.conns[req.place-1].listenAddr())
			if req.listening != nil {
				req.listening(tr.conns[req.place-1].listenAddr())
			}
			return
		}
		why := "operator"
		if req.dur > 0 {
			why = fmt.Sprintf("operator, for %s", req.dur)
		}
		err := move(n, req.place, why)
		switch {
		case err == nil:
			fo.switched()
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
	// dialCame moves through core's dial on the listener a switch opened;
	// one that does not answer leaves it listening, for another dial.
	dialCame := func(n int) {
		a := awaiting
		why := "operator"
		if a.req.dur > 0 {
			why = fmt.Sprintf("operator, for %s", a.req.dur)
		}
		if err := move(n, a.req.place, why); err != nil {
			if tr.place() == a.req.place {
				awaiting = nil
				a.req.done <- nil
				ended = err
				return
			}
			log.Printf("core's dial on connection %d: %v; still listening", a.req.place, err)
			return
		}
		awaiting = nil
		fo.switched()
		back = nil
		if a.req.dur > 0 {
			back = &revert{place: a.from, at: time.Now().Add(a.req.dur), after: a.req.dur}
		}
		a.req.done <- nil
	}
	// dialTimedOut gives up a switch no dial came for: the port closes.
	dialTimedOut := func() {
		if awaiting.cancelled() {
			log.Printf("switch to connection %d: cancelled; no longer listening", awaiting.req.place)
		} else {
			log.Printf("switch to connection %d: no dial from core in %s; no longer listening", awaiting.req.place, listenWait)
		}
		tr.stopListening()
		awaiting.fail(fmt.Errorf("core did not dial connection %d within %s (is there a target for it, connected?); staying on %d",
			awaiting.req.place, listenWait, tr.place()))
		awaiting = nil
	}
	// takePushes takes everything core pushed over the connection in use
	// (tasks within the allowance, cancels, tunnel data and acks): their
	// acks and answers go with the next message. An error once core said
	// it closed the session.
	takePushes := func() error {
		for _, push := range tr.pushes().take() {
			if e, ok := coreErrItem(push); ok && e.Code == protocol.ErrItemSessionClosed {
				return errors.New("core closed the session")
			}
			tasks.take(push, nil)
		}
		return nil
	}
	for n := 1; ; n++ {
		if ended != nil {
			return ended
		}
		// What the next message keeps within, and how it asks for tasks:
		// the connection in use's.
		tasks.setLimit(tr.conn().MessageLimit())
		tasks.setPersistent(tr.persistent())
		// A switch waiting for core's dial: the dial, or its time up.
		if awaiting != nil {
			select {
			case <-awaiting.link.dialed():
				dialCame(n)
				continue
			default:
			}
			if awaiting.cancelled() || !time.Now().Before(awaiting.until) {
				dialTimedOut()
				continue
			}
		}
		// A connect waiting, or a move back that is due, goes first.
		select {
		case req := <-tasks.switches:
			switchTo(n, req)
			continue
		default:
		}
		if back != nil && !time.Now().Before(back.at) && tr.conns[back.place-1].bound() {
			// Back to a connection the minion listens on: its port opens,
			// and the move goes through core's dial, as a switch's.
			if awaiting == nil {
				log.Printf("back after %s: listening on %s, waiting for core's dial", back.after, tr.conns[back.place-1].listenAddr())
				awaiting = &awaitDial{req: switchReq{place: back.place, done: make(chan error, 1)}, from: tr.place(),
					link: tr.listen(back.place), until: time.Now().Add(listenWait)}
			}
			back = nil
			continue
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
		// Failover took the minion off its first connection a while ago:
		// that one again, once (an operator's move back, or a switch under
		// way, goes first).
		if back == nil && awaiting == nil {
			if first := fo.failbackDue(tr.place(), time.Now()); first > 0 {
				if err := move(n, first, fmt.Sprintf("failback after %s", fo.failbackWait)); err != nil {
					if tr.place() == first {
						return err
					}
					log.Printf("failback to connection %d: %v; trying again in %s", first, err, fo.failbackWait)
					fo.failbackMissed(time.Now())
				} else {
					fo.ok(first)
				}
				continue
			}
		}
		// On a WebSocket the first check-in goes at once: it links the
		// connection to the session and sets the allowance core pushes
		// tasks within. After that, while it stays open, nothing goes on a
		// schedule: only what there is to send (output, acks), what core
		// pushes, and a failed exchange's retry.
		if !tasks.pending() && (!tr.persistent() || n > 1) {
			wait := sched.next()
			p, polled := tasks.pollEvery()
			if polled && p < wait {
				wait = p
			}
			var due <-chan time.Time
			if back != nil {
				due = time.After(time.Until(back.at))
			}
			scheduled := !tr.persistent() || retrying || polled
			if scheduled {
				log.Printf("next check-in in %s", wait.Round(time.Millisecond))
			} else {
				log.Printf("connection open: no check-in until there is something to send")
			}
			timer := time.NewTimer(wait)
			if !scheduled {
				timer.Stop()
			}
			// A switch waiting for core's dial wakes the loop when it comes,
			// or when its time is up.
			var dialed <-chan struct{}
			var dialDue <-chan time.Time
			if awaiting != nil {
				dialed = awaiting.link.dialed()
				dialDue = time.After(min(time.Until(awaiting.until), 5*time.Second))
			}
		waiting:
			for {
				select {
				case <-timer.C:
					break waiting
				case <-dialed:
					timer.Stop()
					dialCame(n)
					break waiting
				case <-dialDue:
					// Time up, or the task stopped: the loop gives it up.
					// Otherwise only the watch goes on: no check-in for it.
					if awaiting.cancelled() || !time.Now().Before(awaiting.until) {
						timer.Stop()
						break waiting
					}
					dialDue = time.After(min(time.Until(awaiting.until), 5*time.Second))
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
					if wait := tr.reconnectWait(backoff); wait > 0 {
						log.Printf("the WebSocket closed: connecting again in %s", wait)
						time.Sleep(wait)
						backoff = min(2*backoff, tr.conn().wsBackoff())
					} else {
						log.Printf("the connection closed: waiting for the channel to dial again")
					}
					break waiting
				case <-tr.pushes().readyCh():
					// What core starts over the connection kept open, all
					// that came at once.
					if err := takePushes(); err != nil {
						timer.Stop()
						return err
					}
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
		// What core pushed meanwhile goes with this message: pushes never
		// wait on a loop busy exchanging, or retrying.
		if err := takePushes(); err != nil {
			return err
		}
		items := tasks.outgoing()
		answer, err := tr.exchange(sessionID, newKeys, items...)
		if err != nil {
			tasks.failed(items)
			streak.failed(err)
			retrying = true
			if since := time.Since(lastOK); since >= backstop {
				return fmt.Errorf("check-in %d: nothing has worked for %s: %w", n, since.Round(time.Second), err)
			}
			log.Printf("check-in %d failed, keeping the session: %v", n, err)
			// Failing again and again: the next connection, as the build's
			// failover policy says.
			to, pause, why := fo.failed(tr.place(), time.Now())
			if pause > 0 {
				log.Printf("every connection failed in a row: pausing %s before the next round", pause)
				time.Sleep(pause)
				continue
			}
			if to > 0 {
				from := tr.place()
				if mErr := move(n, to, why); mErr != nil {
					if tr.place() == to {
						return mErr
					}
					log.Printf("failover from connection %d to %d: %v", from, to, mErr)
					fo.missed(to)
				} else {
					fo.moved(to, time.Now())
					streak.worked()
					retrying = false
					continue
				}
			}
			// Not at once again: the channel, or core behind it, may be out
			// of reach. Over a WebSocket, as a reconnect: 1s, doubling up to
			// ws_reconnect_backoff (no schedule runs while it is up); through
			// requests, the check-in schedule.
			if tr.persistent() {
				// The failed attempt's own close says nothing new.
				select {
				case <-tr.lost():
				default:
				}
				if wait := tr.reconnectWait(backoff); wait > 0 {
					log.Printf("trying again in %s", wait)
					time.Sleep(wait)
					backoff = min(2*backoff, tr.conn().wsBackoff())
				}
			} else {
				time.Sleep(sched.next())
			}
			continue
		}
		retrying = false
		streak.worked()
		fo.ok(tr.place())
		if err := answered(n, items, answer); err != nil {
			return err
		}
		if i := slices.IndexFunc(items, func(it any) bool { _, ok := it.(protocol.GetTasksItem); return ok }); i >= 0 {
			log.Printf("check-in %d: get_tasks max %d, %d item(s) sent, %d back", n, *items[i].(protocol.GetTasksItem).Max, len(items),
				len(answer.Items))
		} else {
			log.Printf("check-in %d: no room for tasks, %d item(s) sent, %d back", n, len(items), len(answer.Items))
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
func hello(ex exchanger, tempID [16]byte, keys protocol.AES256CBCHMACSHA256Material, claim []byte, sched schedule, conn int,
	maxTasks uint64) ([16]byte, error) {
	var sessionID [16]byte
	h := describeHost()
	h.SID = claim
	// The connection it says hello through, by its place in the build's.
	h.Conn = uint64(conn) //nolint:gosec // a small place
	// The schedule it checks in by, for core to tell when it is late.
	h.Sleep = &protocol.Sleep{Interval: uint64(max(1, sched.interval.Milliseconds())), Jitter: uint64(sched.jitter)}
	// How many tasks it runs at once: the build's, which core may change.
	h.MaxTasks = &maxTasks
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
		report(protocol.FeedbackWarning, "session_not_claimed", nil,
			"core did not give the persisted session %s: this process goes on in a new one", formatUUID(claim))
	}
	return sessionID, nil
}

// describeHost is the hello: what the minion can learn of its host and
// process.
func describeHost() protocol.HelloItem {
	h := protocol.HelloItem{T: protocol.ItemHello, PID: uint64(os.Getpid()), OS: runtime.GOOS, Arch: runtime.GOARCH,
		Version: version, Integrity: integrityLevel(), Domain: hostDomain()}
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
	if conn.tcp() {
		// A reverse call through the TCP channel: a connection for it.
		log.Printf("message %x under %s: %d bytes of CBOR, a %d-byte sealed frame, to %s", msgID, formatUUID(id[:]), plainLen,
			len(frame), conn.dialAddr())
		sealed, took, err := conn.tcpCall(id, frame)
		if err != nil {
			return answer, err
		}
		log.Printf("the channel answered with %d bytes in %s", len(sealed), took.Round(time.Millisecond))
		if len(sealed) == 0 {
			return answer, nil
		}
		answer, err = openReply(keys, id, msgID, sealed)
		if err == nil {
			log.Printf("opened core's reply: %d item(s)", len(answer.Items))
		}
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
	if answer, err = readCore(plain); err != nil {
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
		// Packed where the build packs and it pays (packing.go).
		msg.Items = append(msg.Items, packItem(raw))
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

// connection is one of the minion's connections: its channel's settings and
// BOB's own parameters for it, by number (fields.go).
type connection struct {
	protocol.MinionConnection
}

// pick is the number of a field both channels have: the HTTP channel's, or
// the TCP channel's.
func (c connection) pick(http, tcp uint64) uint64 {
	if c.tcp() {
		return tcp
	}
	return http
}

// param is the channel's text field n; "" when it has none.
func (c connection) param(n uint64) string {
	s, _ := protocol.ArgString(c.Params, n)
	return s
}

// duration is the channel's duration field n, def when it has none.
func (c connection) duration(n uint64, def time.Duration) time.Duration {
	if d, ok := protocol.ArgDuration(c.Params, n); ok && d > 0 {
		return d
	}
	return def
}

// intParam is the channel's integer field n, def when it has none.
func (c connection) intParam(n uint64, def int) int {
	if v, ok := protocol.ArgInt(c.Params, n); ok {
		return int(v)
	}
	return def
}

// schedule is the endpoint's check-in interval and jitter, or the
// channels' defaults (60s, 20%) where it has none.
func (c connection) schedule() schedule {
	s := schedule{interval: c.duration(c.pick(httpCheckinInterval, tcpCheckinInterval), 60*time.Second), jitter: 20}
	if j, ok := protocol.ArgInt(c.Params, c.pick(httpCheckinJitter, tcpCheckinJitter)); ok && j >= 0 && j <= 100 {
		s.jitter = float64(j)
	}
	return s
}

func (c connection) method() string {
	if m, _ := protocol.ArgString(c.Values, paramMethod); m != "" {
		return m
	}
	return http.MethodPost
}

// url is the base URL with BOB's path prefix.
func (c connection) url() string {
	prefix, _ := protocol.ArgString(c.Values, paramPathPrefix)
	if prefix == "" {
		prefix = "/"
	}
	return strings.TrimRight(c.param(httpBaseURL), "/") + prefix
}

// wsBackoff is the endpoint's longest wait between reconnects: a
// WebSocket's, a TCP connection's.
func (c connection) wsBackoff() time.Duration {
	return c.duration(c.pick(httpWSReconnectBackoff, tcpReconnectBackoff), 60*time.Second)
}

// timeout is the endpoint's request timeout: how long the minion waits for
// a reply.
func (c connection) timeout() time.Duration {
	return c.duration(c.pick(httpRequestTimeout, tcpRequestTimeout), 40*time.Second)
}

// headers are the extra headers the HTTP channel's endpoint adds.
func (c connection) headers() map[string]string {
	h, _ := protocol.ArgStringMap(c.Params, httpHeaders)
	return h
}

// verify says whether the HTTP channel's certificate is checked.
func (c connection) verify() bool {
	v, _ := protocol.ArgBool(c.Params, httpTLSVerify)
	return v
}

// clientKey is what an HTTP client depends on: its TLS settings and its
// timeout.
type clientKey struct {
	verify  bool
	sni     string
	timeout time.Duration
}

// clients are the minion's HTTP clients, one for each set of settings, kept
// for as long as it runs: a client's transport keeps its connection to the
// channel open and reuses it from one check-in to the next. A client made
// for each request would leave its connection open behind it, one more a
// check-in.
var clients sync.Map // clientKey → *http.Client

// idleConnTimeout closes a connection to the channel no check-in used for
// this long: a minion that checks in rarely holds none open between.
const idleConnTimeout = 30 * time.Second

// client is the HTTP client for the connection's settings.
func (c connection) client() *http.Client {
	k := clientKey{verify: c.verify(), sni: c.param(httpSNI), timeout: c.timeout()}
	if cl, ok := clients.Load(k); ok {
		return cl.(*http.Client)
	}
	cl, _ := clients.LoadOrStore(k, &http.Client{Timeout: k.timeout, Transport: &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: !k.verify, ServerName: k.sni}, //nolint:gosec // the endpoint's own setting
		IdleConnTimeout: idleConnTimeout,
	}})
	return cl.(*http.Client)
}

// send posts body to the channel as the connection's settings say.
func (c connection) send(body []byte) ([]byte, int, time.Duration, error) {
	client := c.client()
	req, err := http.NewRequest(c.method(), c.url(), bytes.NewReader(body))
	if err != nil {
		return nil, 0, 0, err
	}
	req.Header.Set("Content-Type", "application/octet-stream")
	if h := c.param(httpHostHeader); h != "" {
		req.Host = h
	}
	if ua := c.param(httpUserAgent); ua != "" {
		req.Header.Set("User-Agent", ua)
	}
	for k, v := range c.headers() {
		req.Header.Set(k, v)
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

func formatUUID(b []byte) string {
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// fingerprint is the first 8 bytes of b's SHA-256, for logs: keys are never
// printed.
func fingerprint(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:8])
}
