// Package backup keeps full backups and the WAL archive of a cluster in a
// folder or S3-compatible storage, and restores them into a new folder
// (D63, docs/ARCHITECTURE.md "Backups and point-in-time restore").
package backup

import (
	"bytes"
	"compress/gzip"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
)

// Object format (every object we store except coyote-backup.json and
// info.json, which are small plain JSON):
//
//	magic   "CYBK0001"        8 bytes
//	flags   bit0 gzip, bit1 AES-256-GCM   1 byte
//	cid     content id        32 bytes: sha256 of the raw content, or
//	                          HMAC-SHA256(key, raw) when encrypted
//	payload the raw content, gzipped if bit0, then if bit1:
//	        nonce (12 bytes) || GCM ciphertext with the header and the
//	        object key as associated data
const (
	magic      = "CYBK0001"
	HeaderSize = len(magic) + 1 + 32

	flagGzip    byte = 1
	flagEncrypt byte = 2
)

// KeySize is the encryption key length (AES-256).
const KeySize = 32

// ErrWrongKey: the object is encrypted and the key is missing or wrong, or
// the object was changed or renamed.
var ErrWrongKey = errors.New("the backup could not be decrypted: the backup key is missing or wrong, or the file was changed")

// Codec seals and opens objects. A nil Key means no encryption.
type Codec struct {
	Key []byte
}

// NewKey returns a fresh random key.
func NewKey() ([]byte, error) {
	k := make([]byte, KeySize)
	if _, err := rand.Read(k); err != nil {
		return nil, err
	}
	return k, nil
}

// EncodeKey and DecodeKey turn a key into the text people keep and back.
func EncodeKey(k []byte) string { return "cybk-" + base64.RawURLEncoding.EncodeToString(k) }

// DecodeKey parses a key written by EncodeKey.
func DecodeKey(s string) ([]byte, error) {
	if len(s) < 5 || s[:5] != "cybk-" {
		return nil, errors.New(`a backup key starts with "cybk-"`)
	}
	k, err := base64.RawURLEncoding.DecodeString(s[5:])
	if err != nil || len(k) != KeySize {
		return nil, errors.New("this is not a valid backup key (it may be cut short)")
	}
	return k, nil
}

// KeyID is a short, non-secret fingerprint of a key, stored in the archive
// identity so a wrong key is named before anything is read.
func KeyID(k []byte) string {
	if len(k) == 0 {
		return ""
	}
	m := hmac.New(sha256.New, k)
	m.Write([]byte("coyote backup key id"))
	return fmt.Sprintf("%x", m.Sum(nil)[:8])
}

// ContentID is the content id of raw under this codec.
func (c Codec) ContentID(raw []byte) [32]byte {
	if len(c.Key) > 0 {
		m := hmac.New(sha256.New, c.Key)
		m.Write(raw)
		var out [32]byte
		copy(out[:], m.Sum(nil))
		return out
	}
	return sha256.Sum256(raw)
}

// Seal encodes raw for storage under objKey.
func (c Codec) Seal(raw []byte, objKey string, compress bool) ([]byte, error) {
	var flags byte
	payload := raw
	if compress {
		flags |= flagGzip
		var b bytes.Buffer
		zw, _ := gzip.NewWriterLevel(&b, gzip.BestSpeed)
		if _, err := zw.Write(raw); err != nil {
			return nil, err
		}
		if err := zw.Close(); err != nil {
			return nil, err
		}
		payload = b.Bytes()
	}
	if len(c.Key) > 0 {
		flags |= flagEncrypt
	}
	cid := c.ContentID(raw)
	hdr := make([]byte, 0, HeaderSize)
	hdr = append(hdr, magic...)
	hdr = append(hdr, flags)
	hdr = append(hdr, cid[:]...)
	if flags&flagEncrypt == 0 {
		return append(hdr, payload...), nil
	}
	gcm, err := c.gcm()
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	out := append(hdr, nonce...)
	return gcm.Seal(out, nonce, payload, aad(hdr, objKey)), nil
}

// Open decodes an object stored under objKey and checks its content id.
func (c Codec) Open(obj []byte, objKey string) ([]byte, error) {
	h, err := ParseHeader(obj)
	if err != nil {
		return nil, err
	}
	payload := obj[HeaderSize:]
	if h.Encrypted {
		if len(c.Key) == 0 {
			return nil, ErrWrongKey
		}
		gcm, err := c.gcm()
		if err != nil {
			return nil, err
		}
		if len(payload) < gcm.NonceSize() {
			return nil, ErrWrongKey
		}
		nonce := payload[:gcm.NonceSize()]
		payload, err = gcm.Open(nil, nonce, payload[gcm.NonceSize():], aad(obj[:HeaderSize], objKey))
		if err != nil {
			return nil, ErrWrongKey
		}
	}
	raw := payload
	if h.Gzip {
		zr, err := gzip.NewReader(bytes.NewReader(payload))
		if err != nil {
			return nil, fmt.Errorf("%s: %w", objKey, err)
		}
		raw, err = io.ReadAll(zr)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", objKey, err)
		}
	}
	// Unencrypted objects carry a plain sha256; check it. An encrypted
	// one is already authenticated by GCM, and its id needs the key.
	want := c.ContentID(raw)
	if !h.Encrypted && len(c.Key) > 0 {
		want = sha256.Sum256(raw)
	}
	if want != h.CID {
		return nil, fmt.Errorf("%s: content does not match its checksum (damaged file)", objKey)
	}
	return raw, nil
}

func (c Codec) gcm() (cipher.AEAD, error) {
	if len(c.Key) != KeySize {
		return nil, fmt.Errorf("backup key must be %d bytes", KeySize)
	}
	b, err := aes.NewCipher(c.Key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(b)
}

func aad(hdr []byte, objKey string) []byte {
	return append(append([]byte{}, hdr...), objKey...)
}

// Header is an object's parsed header.
type Header struct {
	Gzip, Encrypted bool
	CID             [32]byte
}

// ParseHeader reads the header at the start of b.
func ParseHeader(b []byte) (Header, error) {
	if len(b) < HeaderSize || string(b[:len(magic)]) != magic {
		return Header{}, errors.New("not a backup object (wrong header)")
	}
	f := b[len(magic)]
	h := Header{Gzip: f&flagGzip != 0, Encrypted: f&flagEncrypt != 0}
	copy(h.CID[:], b[len(magic)+1:HeaderSize])
	return h, nil
}
