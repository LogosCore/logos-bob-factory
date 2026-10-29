package protocol

// Payload crypto suites: how a minion and core encrypt what passes between
// them. A factory declares the one its minions implement (BuildSchema.Crypto);
// core generates a minion's keys for it and sends them in the build request
// (BuildRequest.Crypto).

// CryptoAES256CBCHMACSHA256 is AES-256-CBC with an HMAC-SHA256 tag
// (encrypt-then-MAC). Its material is AES256CBCHMACSHA256Material.
const CryptoAES256CBCHMACSHA256 = "aes-256-cbc-hmac-sha256"

// AES256CBCHMACSHA256Material is the key material of
// CryptoAES256CBCHMACSHA256: two independent 32-byte keys, base64 in JSON,
// byte strings in CBOR (a stage_response's keys).
type AES256CBCHMACSHA256Material struct {
	// EncKey is the AES-256 key.
	EncKey []byte `json:"enc_key" cbor:"enc_key"`
	// MACKey is the HMAC-SHA256 key.
	MACKey []byte `json:"mac_key" cbor:"mac_key"`
}
