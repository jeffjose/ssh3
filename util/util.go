package util

import (
	"context"
	"crypto"
	"crypto/ed25519"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"strings"
	"sync"

	"github.com/golang-jwt/jwt/v5"
	"github.com/rs/zerolog"
	"golang.org/x/crypto/cryptobyte"
	cryptobyte_asn1 "golang.org/x/crypto/cryptobyte/asn1"
)

type UnknownSSHPubkeyType struct {
	pubkey crypto.PublicKey
}

func (m UnknownSSHPubkeyType) Error() string {
	return fmt.Sprintf("unknown signing method: %T", m.pubkey)
}

// copied from "net/http/internal/ascii"
// EqualFold is strings.EqualFold, ASCII only. It reports whether s and t
// are equal, ASCII-case-insensitively.
func EqualFold(s, t string) bool {
	if len(s) != len(t) {
		return false
	}
	for i := 0; i < len(s); i++ {
		if lower(s[i]) != lower(t[i]) {
			return false
		}
	}
	return true
}

// lower returns the ASCII lowercase version of b.
func lower(b byte) byte {
	if 'A' <= b && b <= 'Z' {
		return b + ('a' - 'A')
	}
	return b
}

func ConfigureLogger(logLevel string) {
	switch strings.ToLower(logLevel) {
	case "debug":
		zerolog.SetGlobalLevel(zerolog.DebugLevel)
	case "info":
		zerolog.SetGlobalLevel(zerolog.InfoLevel)
	case "warning":
		zerolog.SetGlobalLevel(zerolog.WarnLevel)
	case "error":
		zerolog.SetGlobalLevel(zerolog.WarnLevel)
	default:
		zerolog.SetGlobalLevel(zerolog.WarnLevel)
	}
}


// Accept queue copied from https://github.com/quic-go/webtransport-go/blob/master/session.go
type AcceptQueue[T any] struct {
	mx sync.Mutex
	// The channel is used to notify consumers (via Chan) about new incoming items.
	// Needs to be buffered to preserve the notification if an item is enqueued
	// between a call to Next and to Chan.
	c chan struct{}
	// Contains all the streams waiting to be accepted.
	// There's no explicit limit to the length of the queue, but it is implicitly
	// limited by the stream flow control provided by QUIC.
	queue []T
}

func NewAcceptQueue[T any]() *AcceptQueue[T] {
	return &AcceptQueue[T]{c: make(chan struct{}, 1)}
}

func (q *AcceptQueue[T]) Add(str T) {
	q.mx.Lock()
	q.queue = append(q.queue, str)
	q.mx.Unlock()

	select {
	case q.c <- struct{}{}:
	default:
	}
}

func (q *AcceptQueue[T]) Next() T {
	q.mx.Lock()
	defer q.mx.Unlock()

	if len(q.queue) == 0 {
		return *new(T)
	}
	str := q.queue[0]
	q.queue = q.queue[1:]
	return str
}

func (q *AcceptQueue[T]) Chan() <-chan struct{} { return q.c }


type DatagramsQueue struct {
	c chan []byte
}

func NewDatagramsQueue(len uint64) *DatagramsQueue {
	return &DatagramsQueue{c: make(chan []byte, len)}
}

// returns true if added, false otherwise
func (q *DatagramsQueue) Add(datagram []byte) bool {
	select {
	case q.c <- datagram:
		return true
	default:
		return false
	}
}

// returns nil if added, the context closing error (context.Cause(ctx)) otherwise
func (q *DatagramsQueue) WaitAdd(ctx context.Context, datagram []byte) error {
	select {
	case q.c <- datagram:
		return nil
	case <-ctx.Done():
		return context.Cause(ctx)
	}
}


func (q *DatagramsQueue) Next() []byte {
	select {
	case datagram := <-q.c:
		return datagram
	default:
		return nil
	}
}

func (q *DatagramsQueue) WaitNext(ctx context.Context) ([]byte, error) {
	select {
	case datagram := <-q.c:
		return datagram, nil
	case <-ctx.Done():
		return nil, context.Cause(ctx)
	}
}

func JWTSigningMethodFromCryptoPubkey(pubkey crypto.PublicKey) (jwt.SigningMethod, error) {
	switch pubkey.(type) {
	case *rsa.PublicKey:
		return jwt.SigningMethodRS256, nil
	case *ed25519.PublicKey:
		return jwt.SigningMethodEdDSA, nil
	}
	return nil, UnknownSSHPubkeyType{pubkey: pubkey}
}

func Sha256Fingerprint(in []byte) string {
	hash := sha256.Sum256(in)
	return base64.StdEncoding.EncodeToString(hash[:])
}


func getSANExtension(cert *x509.Certificate) []byte {
	oidExtensionSubjectAltName := []int{2, 5, 29, 17}
	for _, e := range cert.Extensions {
		if e.Id.Equal(oidExtensionSubjectAltName) {
			return e.Value
		}
	}
	return nil
}


func forEachSAN(der cryptobyte.String, callback func(tag int, data []byte) error) error {
	if !der.ReadASN1(&der, cryptobyte_asn1.SEQUENCE) {
		return errors.New("x509: invalid subject alternative names")
	}
	for !der.Empty() {
		var san cryptobyte.String
		var tag cryptobyte_asn1.Tag
		if !der.ReadAnyASN1(&san, &tag) {
			return errors.New("x509: invalid subject alternative name")
		}
		if err := callback(int(tag^0x80), san); err != nil {
			return err
		}
	}

	return nil
}


// returns true whether the certificat contains a SubjectAltName extension
// with at least one IP address record
func CertHasIPSANs(cert *x509.Certificate) (bool, error) {
	SANExtension := getSANExtension(cert)
	if SANExtension == nil {
		return false, nil
	}
	nameTypeIP := 7
	var ipAddresses []net.IP

	err := forEachSAN(SANExtension, func(tag int, data []byte) error {
		switch tag {
		case nameTypeIP:
			switch len(data) {
			case net.IPv4len, net.IPv6len:
				ipAddresses = append(ipAddresses, data)
			default:
				return fmt.Errorf("x509: cannot parse IP address of length %d",len(data))
			}
		default:
		}

		return nil
	})
	return len(ipAddresses) > 0, err
}