// Command minion is the program BOB compiles for its linux-elf target: a
// dummy minion. BOB writes the build's description to minion.json before
// compiling, so it is baked into the binary (the minion.json kept here is a
// placeholder). Run, the minion stages with core through its first HTTP
// connection and logs each step (docs: contracts/minion-messages.md);
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
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/fxamacker/cbor/v2"
	"github.com/logoscore/logos-golang-protocol/protocol"
)

//go:embed minion.json
var minion []byte

// description is what the minion reads of the description it was built with.
type description struct {
	BuildID     string                     `json:"build_id"`
	Crypto      *protocol.BuildCrypto      `json:"crypto"`
	Connections []protocol.BuildConnection `json:"connections"`
}

func main() {
	if len(os.Args) > 1 && os.Args[1] == "describe" {
		_, _ = os.Stdout.Write(minion)
		return
	}
	log.SetFlags(log.Ltime | log.Lmicroseconds)
	log.SetPrefix("minion ")
	if err := stage(); err != nil {
		log.Printf("stage failed: %v", err)
		os.Exit(1)
	}
}

// stage sends the minion's first message, a stage, and reads core's answer.
func stage() error {
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
	conn, err := httpConnection(d.Connections)
	if err != nil {
		return err
	}
	log.Printf("build %s, crypto %s", d.BuildID, d.Crypto.Suite)
	log.Printf("channel http %q (%s %s)", conn.Endpoint.Name, conn.method(), conn.url())

	// 1–2. A key pair of the minion's own, and a correlation id.
	start := time.Now()
	priv, err := rsa.GenerateKey(rand.Reader, protocol.StageKeySize)
	if err != nil {
		return err
	}
	pub, _ := x509.MarshalPKIXPublicKey(&priv.PublicKey)
	corr, msgID := protocol.NewMessageID(), protocol.NewMessageID()
	log.Printf("generated an RSA-%d key pair in %s; public key sha256 %s", protocol.StageKeySize,
		time.Since(start).Round(time.Millisecond), fingerprint(pub))
	log.Printf("correlation id %x, message id %x", corr, msgID)

	// 3. The stage, sealed with the build's keys, under the build id.
	item, err := protocol.MarshalCBOR(protocol.StageItem{T: protocol.ItemStage, Key: pub, Corr: corr})
	if err != nil {
		return err
	}
	plain, err := protocol.MarshalCBOR(protocol.MinionMessage{V: protocol.MinionMessageV1, ID: msgID, Items: []cbor.RawMessage{item}})
	if err != nil {
		return err
	}
	frame, err := protocol.SealFrame(keys, buildID, protocol.FrameMinionToCore, plain)
	if err != nil {
		return err
	}
	body, err := pack(buildID, frame)
	if err != nil {
		return err
	}
	log.Printf("stage: %d bytes of CBOR, a %d-byte sealed frame, %d bytes packed for the channel", len(plain), len(frame), len(body))

	reply, status, took, err := conn.send(body)
	if err != nil {
		return err
	}
	log.Printf("channel answered %d with %d bytes in %s", status, len(reply), took.Round(time.Millisecond))
	if status != http.StatusOK {
		return fmt.Errorf("the channel answered %d: core refused the message, or could not be reached", status)
	}
	replyID, sealed, err := unpack(reply)
	if err != nil {
		return err
	}
	if replyID != buildID {
		return fmt.Errorf("the reply is for %x, not this build", replyID)
	}
	if len(sealed) == 0 {
		return errors.New("core had nothing to say: it took the message as a replay")
	}

	// 4–6. Core's answer: a temporary id and new keys, encrypted to ours.
	plain, err = protocol.OpenFrame(keys, buildID, protocol.FrameCoreToMinion, sealed)
	if err != nil {
		return fmt.Errorf("core's reply: %w", err)
	}
	var answer protocol.CoreMessage
	if err := protocol.UnmarshalCBOR(plain, &answer); err != nil {
		return fmt.Errorf("core's reply: %w", err)
	}
	if err := protocol.ValidateMessageHead(answer.V, answer.ID, answer.Re, len(answer.Items)); err != nil {
		return fmt.Errorf("core's reply: %w", err)
	}
	if !bytes.Equal(answer.Re, msgID) {
		return fmt.Errorf("core's reply answers %x, not our message", answer.Re)
	}
	log.Printf("opened core's reply: %d bytes of CBOR, %d item(s)", len(plain), len(answer.Items))
	for _, raw := range answer.Items {
		switch protocol.ItemType(raw) {
		case protocol.ItemErr:
			var e protocol.ErrItem
			_ = protocol.UnmarshalCBOR(raw, &e)
			return fmt.Errorf("core says %s: %s", e.Code, e.Msg)
		case protocol.ItemStageResponse:
			return staged(raw, corr, priv)
		}
	}
	return errors.New("core's reply has no stage_response")
}

// staged reads a stage_response and logs what the minion now has.
func staged(raw []byte, corr []byte, priv *rsa.PrivateKey) error {
	var resp protocol.StageResponseItem
	if err := protocol.UnmarshalCBOR(raw, &resp); err != nil {
		return fmt.Errorf("stage_response: %w", err)
	}
	if !bytes.Equal(resp.Corr, corr) {
		return errors.New("stage_response: another correlation id")
	}
	if len(resp.TID) != 16 {
		return errors.New("stage_response: the temporary id is not 16 bytes")
	}
	material, err := protocol.DecryptStageKeys(priv, resp.Keys)
	if err != nil {
		return fmt.Errorf("decrypt the new keys: %w", err)
	}
	var keys protocol.AES256CBCHMACSHA256Material
	if err := protocol.UnmarshalCBOR(material, &keys); err != nil || len(keys.EncKey) != 32 || len(keys.MACKey) != 32 {
		return fmt.Errorf("the new keys do not read: %v", err)
	}
	log.Printf("stage_response: correlation id matches")
	log.Printf("temporary id %s", formatUUID(resp.TID))
	log.Printf("new keys decrypted with our private key: enc_key sha256 %s, mac_key sha256 %s",
		fingerprint(keys.EncKey), fingerprint(keys.MACKey))
	log.Printf("staged. Next: hello under the temporary id, sealed with the new keys (not built yet)")
	return nil
}

// connection is the minion's HTTP connection: the channel's settings and
// BOB's own parameters for it.
type connection struct {
	protocol.BuildConnection
}

// httpConnection is the first connection through an http channel.
func httpConnection(conns []protocol.BuildConnection) (connection, error) {
	for _, c := range conns {
		if c.Endpoint.Channel.ModuleName == "http" {
			return connection{c}, nil
		}
	}
	return connection{}, errors.New("built without an http connection")
}

func (c connection) param(key string) string {
	s, _ := c.Endpoint.Params[key].(string)
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

// send posts body to the channel as the connection's settings say.
func (c connection) send(body []byte) ([]byte, int, time.Duration, error) {
	timeout := 40 * time.Second
	if d, err := time.ParseDuration(c.param("request_timeout")); err == nil && d > 0 {
		timeout = d
	}
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
	defer resp.Body.Close()
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
