package protocol

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"fmt"
)

// The sealed frame of a minion message (docs: contracts/minion-messages.md),
// for the suite CryptoAES256CBCHMACSHA256:
//
//	IV (16) ‖ ver (1) ‖ ciphertext (16·k) ‖ dir (1) ‖ tag (32)
//
// ciphertext is AES-256-CBC of the message with EncKey and IV, PKCS#7
// padded; tag is HMAC-SHA256 with MACKey over id ‖ IV ‖ ver ‖ ciphertext ‖
// dir, where id is the 16 raw bytes of the UUID the message is sent under (a
// build id, a temporary id or a session id), authenticated but not sent.

const (
	// FrameV1 is the frame's ver byte.
	FrameV1 byte = 0x01
	// FrameMinionToCore and FrameCoreToMinion are the frame's dir byte.
	FrameMinionToCore byte = 0x01
	FrameCoreToMinion byte = 0x02

	frameIVSize  = aes.BlockSize
	frameTagSize = sha256.Size
	// frameFixed is what a frame adds besides the padding: IV, ver, dir and
	// tag. The padding adds 1 to 16 bytes more.
	frameFixed = frameIVSize + 1 + 1 + frameTagSize
	// MinFrameSize is the smallest frame: an empty message pads to a block.
	MinFrameSize = frameFixed + aes.BlockSize
)

// ErrFrame is any frame that does not open: too short, another version or
// direction, a bad tag or padding. Which one is deliberately not said.
var ErrFrame = errors.New("the frame does not open")

// SealFrame encrypts and authenticates message from dir under id with keys.
func SealFrame(keys AES256CBCHMACSHA256Material, id [16]byte, dir byte, message []byte) ([]byte, error) {
	if err := checkFrameKeys(keys); err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(keys.EncKey)
	if err != nil {
		return nil, err
	}
	pad := aes.BlockSize - len(message)%aes.BlockSize
	padded := make([]byte, len(message)+pad)
	copy(padded, message)
	copy(padded[len(message):], bytes.Repeat([]byte{byte(pad)}, pad))

	frame := make([]byte, frameIVSize+1+len(padded)+1, frameIVSize+1+len(padded)+1+frameTagSize)
	iv := frame[:frameIVSize]
	if _, err := rand.Read(iv); err != nil {
		return nil, err
	}
	frame[frameIVSize] = FrameV1
	cipher.NewCBCEncrypter(block, iv).CryptBlocks(frame[frameIVSize+1:frameIVSize+1+len(padded)], padded)
	frame[len(frame)-1] = dir
	return append(frame, frameTag(keys.MACKey, id, frame)...), nil
}

// OpenFrame checks a frame sent under id in direction dir and returns its
// message. It checks the length, the version and direction, then the tag in
// constant time, and decrypts only after: any failure is ErrFrame.
func OpenFrame(keys AES256CBCHMACSHA256Material, id [16]byte, dir byte, frame []byte) ([]byte, error) {
	if err := checkFrameKeys(keys); err != nil {
		return nil, err
	}
	if len(frame) < MinFrameSize || (len(frame)-frameFixed)%aes.BlockSize != 0 {
		return nil, ErrFrame
	}
	body, tag := frame[:len(frame)-frameTagSize], frame[len(frame)-frameTagSize:]
	if body[frameIVSize] != FrameV1 || body[len(body)-1] != dir {
		return nil, ErrFrame
	}
	if subtle.ConstantTimeCompare(tag, frameTag(keys.MACKey, id, body)) != 1 {
		return nil, ErrFrame
	}
	block, err := aes.NewCipher(keys.EncKey)
	if err != nil {
		return nil, err
	}
	ct := body[frameIVSize+1 : len(body)-1]
	plain := make([]byte, len(ct))
	cipher.NewCBCDecrypter(block, body[:frameIVSize]).CryptBlocks(plain, ct)
	pad := int(plain[len(plain)-1])
	if pad == 0 || pad > aes.BlockSize {
		return nil, ErrFrame
	}
	for _, b := range plain[len(plain)-pad:] {
		if int(b) != pad {
			return nil, ErrFrame
		}
	}
	return plain[:len(plain)-pad], nil
}

// frameTag is the tag of a frame's body (everything but the tag) under id.
func frameTag(macKey []byte, id [16]byte, body []byte) []byte {
	m := hmac.New(sha256.New, macKey)
	m.Write(id[:])
	m.Write(body)
	return m.Sum(nil)
}

func checkFrameKeys(keys AES256CBCHMACSHA256Material) error {
	if len(keys.EncKey) != 32 || len(keys.MACKey) != 32 {
		return fmt.Errorf("%s keys must be 32 bytes each", CryptoAES256CBCHMACSHA256)
	}
	return nil
}
