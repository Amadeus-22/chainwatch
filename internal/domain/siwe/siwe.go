// Package siwe parses, formats and validates Sign-In with Ethereum (EIP-4361)
// messages. Parsing is strict: fields must appear in the order the EIP's ABNF
// gives and unknown lines are errors. Signature recovery lives in ethsig; the
// single-use nonce check needs storage and lives in app.Auth.
package siwe

import (
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/Amadeus-22/chainwatch/internal/domain"
	"github.com/Amadeus-22/chainwatch/internal/domain/ethsig"
)

// ErrInvalidMessage means the text is not a well-formed EIP-4361 message or it
// fails validation (wrong domain, expired, unsupported chain…).
var ErrInvalidMessage = errors.New("invalid sign-in message")

const (
	headerSuffix = " wants you to sign in with your Ethereum account:"
	// MaxMessageBytes bounds what the parser accepts.
	MaxMessageBytes = 4096
)

// Message is an EIP-4361 message.
type Message struct {
	Scheme         string // optional, e.g. "https"
	Domain         string // RFC 3986 authority: host[:port]
	Address        domain.Address
	Statement      string // optional, one line
	URI            string
	Version        string // always "1"
	ChainID        uint64
	Nonce          string
	IssuedAt       time.Time
	ExpirationTime *time.Time
	NotBefore      *time.Time
	RequestID      string
	Resources      []string
}

// String formats m exactly as EIP-4361 specifies (what the wallet shows and signs).
func (m Message) String() string {
	var b strings.Builder
	if m.Scheme != "" {
		b.WriteString(m.Scheme + "://")
	}
	b.WriteString(m.Domain + headerSuffix + "\n")
	b.WriteString(ethsig.Checksum(m.Address) + "\n\n")
	if m.Statement != "" {
		b.WriteString(m.Statement + "\n")
	}
	b.WriteString("\nURI: " + m.URI)
	b.WriteString("\nVersion: " + m.Version)
	b.WriteString("\nChain ID: " + strconv.FormatUint(m.ChainID, 10))
	b.WriteString("\nNonce: " + m.Nonce)
	b.WriteString("\nIssued At: " + m.IssuedAt.UTC().Format(time.RFC3339Nano))
	if m.ExpirationTime != nil {
		b.WriteString("\nExpiration Time: " + m.ExpirationTime.UTC().Format(time.RFC3339Nano))
	}
	if m.NotBefore != nil {
		b.WriteString("\nNot Before: " + m.NotBefore.UTC().Format(time.RFC3339Nano))
	}
	if m.RequestID != "" {
		b.WriteString("\nRequest ID: " + m.RequestID)
	}
	if len(m.Resources) > 0 {
		b.WriteString("\nResources:")
		for _, r := range m.Resources {
			b.WriteString("\n- " + r)
		}
	}
	return b.String()
}

// Parse reads an EIP-4361 message.
func Parse(text string) (Message, error) {
	if len(text) > MaxMessageBytes {
		return Message{}, fmt.Errorf("%w: longer than %d bytes", ErrInvalidMessage, MaxMessageBytes)
	}
	if strings.Contains(text, "\r") {
		return Message{}, fmt.Errorf("%w: lines must end with LF, not CRLF", ErrInvalidMessage)
	}
	p := &parser{lines: strings.Split(text, "\n")}
	var m Message

	header := p.next()
	authority, ok := strings.CutSuffix(header, headerSuffix)
	if !ok {
		return Message{}, p.fail("first line must end with %q", headerSuffix)
	}
	if scheme, rest, found := strings.Cut(authority, "://"); found {
		m.Scheme, authority = scheme, rest
	}
	if authority == "" || strings.ContainsAny(authority, " /") {
		return Message{}, p.fail("invalid domain %q", authority)
	}
	m.Domain = authority

	addr, err := ethsig.ParseChecksummed(p.next())
	if err != nil {
		return Message{}, p.fail("%v", err)
	}
	m.Address = addr

	if p.next() != "" {
		return Message{}, p.fail("expected an empty line after the address")
	}
	if line := p.next(); line != "" {
		m.Statement = line
		if p.next() != "" {
			return Message{}, p.fail("expected an empty line after the statement")
		}
	}

	if m.URI, err = p.field("URI"); err != nil {
		return Message{}, err
	}
	if m.Version, err = p.field("Version"); err != nil {
		return Message{}, err
	}
	chain, err := p.field("Chain ID")
	if err != nil {
		return Message{}, err
	}
	if m.ChainID, err = strconv.ParseUint(chain, 10, 64); err != nil || m.ChainID == 0 {
		return Message{}, p.fail("chain id %q is not a positive integer", chain)
	}
	if m.Nonce, err = p.field("Nonce"); err != nil {
		return Message{}, err
	}
	if m.IssuedAt, err = p.timeField("Issued At"); err != nil {
		return Message{}, err
	}
	if p.peek("Expiration Time: ") {
		t, err := p.timeField("Expiration Time")
		if err != nil {
			return Message{}, err
		}
		m.ExpirationTime = &t
	}
	if p.peek("Not Before: ") {
		t, err := p.timeField("Not Before")
		if err != nil {
			return Message{}, err
		}
		m.NotBefore = &t
	}
	if p.peek("Request ID: ") {
		m.RequestID, _ = p.field("Request ID")
	}
	if p.peek("Resources:") {
		if p.next() != "Resources:" {
			return Message{}, p.fail("expected \"Resources:\"")
		}
		for p.peek("- ") {
			m.Resources = append(m.Resources, strings.TrimPrefix(p.next(), "- "))
		}
	}
	if p.i < len(p.lines) {
		return Message{}, p.fail("unexpected line %q", p.lines[p.i])
	}
	if err := m.wellFormed(); err != nil {
		return Message{}, err
	}
	return m, nil
}

// wellFormed checks the value rules of the ABNF that Parse does not enforce
// structurally.
func (m Message) wellFormed() error {
	if m.Version != "1" {
		return fmt.Errorf("%w: version must be 1, got %q", ErrInvalidMessage, m.Version)
	}
	if len(m.Nonce) < 8 || !isAlnum(m.Nonce) {
		return fmt.Errorf("%w: nonce must be at least 8 alphanumeric characters", ErrInvalidMessage)
	}
	u, err := url.Parse(m.URI)
	if err != nil || u.Scheme == "" {
		return fmt.Errorf("%w: URI %q is not an absolute URI", ErrInvalidMessage, m.URI)
	}
	return nil
}

// Expect is what the relying party (chainwatch) requires of a message.
type Expect struct {
	Domain   string          // must equal the message domain and the URI's host
	ChainIDs map[uint64]bool // accepted chain ids
	Now      time.Time
	Skew     time.Duration // tolerated clock difference for Issued At / Not Before
}

// Validate checks domain binding, URI, chain id and the time window. The nonce's
// single use and the signature are checked by the caller.
func (m Message) Validate(e Expect) error {
	if m.Domain != e.Domain {
		return fmt.Errorf("%w: domain %q does not match %q", ErrInvalidMessage, m.Domain, e.Domain)
	}
	if u, err := url.Parse(m.URI); err != nil || u.Host != e.Domain {
		return fmt.Errorf("%w: URI %q is not on domain %q", ErrInvalidMessage, m.URI, e.Domain)
	}
	if !e.ChainIDs[m.ChainID] {
		return fmt.Errorf("%w: chain id %d is not one of the configured chains", ErrInvalidMessage, m.ChainID)
	}
	if m.IssuedAt.After(e.Now.Add(e.Skew)) {
		return fmt.Errorf("%w: issued in the future (%s)", ErrInvalidMessage, m.IssuedAt.Format(time.RFC3339))
	}
	if m.ExpirationTime != nil && !e.Now.Before(*m.ExpirationTime) {
		return fmt.Errorf("%w: expired at %s", ErrInvalidMessage, m.ExpirationTime.Format(time.RFC3339))
	}
	if m.NotBefore != nil && m.NotBefore.After(e.Now.Add(e.Skew)) {
		return fmt.Errorf("%w: not valid before %s", ErrInvalidMessage, m.NotBefore.Format(time.RFC3339))
	}
	return nil
}

type parser struct {
	lines []string
	i     int
}

func (p *parser) next() string {
	if p.i >= len(p.lines) {
		p.i++
		return "\x00" // matches nothing: a missing line fails the caller's check
	}
	l := p.lines[p.i]
	p.i++
	return l
}

func (p *parser) peek(prefix string) bool {
	return p.i < len(p.lines) && strings.HasPrefix(p.lines[p.i], prefix)
}

func (p *parser) fail(format string, args ...any) error {
	return fmt.Errorf("%w: line %d: %s", ErrInvalidMessage, p.i, fmt.Sprintf(format, args...))
}

func (p *parser) field(name string) (string, error) {
	line := p.next()
	v, ok := strings.CutPrefix(line, name+": ")
	if !ok || v == "" {
		return "", p.fail("expected %q", name+": <value>")
	}
	return v, nil
}

func (p *parser) timeField(name string) (time.Time, error) {
	v, err := p.field(name)
	if err != nil {
		return time.Time{}, err
	}
	t, err := time.Parse(time.RFC3339Nano, v)
	if err != nil {
		return time.Time{}, p.fail("%s %q is not an RFC 3339 timestamp", name, v)
	}
	return t, nil
}

func isAlnum(s string) bool {
	for _, c := range s {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z') {
			return false
		}
	}
	return true
}
