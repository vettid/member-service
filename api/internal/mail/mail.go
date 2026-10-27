// Package mail sends the member API's one-time codes. The code proves
// that a visitor controls an email address before the service vault is
// asked to bind it (service-vault README "Email binding"). Codes and
// addresses are never logged.
package mail

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	netmail "net/mail"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sesv2"
	"github.com/aws/aws-sdk-go-v2/service/sesv2/types"
)

// Kind is what a code is for; it picks the email's text.
type Kind int

const (
	// Connect: joining, the address is proven before the invitation.
	Connect Kind = iota
	// KeyChange: a sign-in was approved with a changed VettID credential.
	KeyChange
)

// Mailer sends a one-time code to an address.
type Mailer interface {
	SendCode(ctx context.Context, to, code string, kind Kind) error
}

// Subject and Body are the code email. The code is also in the subject,
// so it can be read from a notification.
func Subject(code string) string { return code + " is your VettID member service code" }

// Body is the code email's plain-text body.
func Body(code string, kind Kind) string {
	if kind == KeyChange {
		return "Someone signing in to member.vettid.org as you approved the sign-in in VettID with a credential that has changed since you last signed in. That is expected after you reset your credential or moved to a new phone.\n\n" +
			"If that was you, enter this code on the sign-in page: " + code + ". It works for 10 minutes.\n\n" +
			"Never share this code with anyone, including anyone who says they are from VettID: we never ask for it. " +
			"If you did not just sign in, do not use the code, and contact support@vettid.org: someone else may have recovered your VettID account.\n"
	}
	return "Your code for member.vettid.org is " + code + ".\n\n" +
		"Enter it on the page where you asked for it. It works for 10 minutes.\n\n" +
		"If you did not ask for a code, ignore this email: nothing happens without the code.\n"
}

// SESAPI is the part of the SES v2 client SES uses.
type SESAPI interface {
	SendEmail(ctx context.Context, in *sesv2.SendEmailInput, opts ...func(*sesv2.Options)) (*sesv2.SendEmailOutput, error)
}

// SES sends through Amazon SES from From (an address of a verified
// domain identity, e.g. no-reply@staging.vettid.org).
type SES struct {
	Client SESAPI
	From   string
}

// SendCode implements Mailer.
func (s *SES) SendCode(ctx context.Context, to, code string, kind Kind) error {
	_, err := s.Client.SendEmail(ctx, &sesv2.SendEmailInput{
		FromEmailAddress: aws.String(s.From),
		Destination:      &types.Destination{ToAddresses: []string{to}},
		Content: &types.EmailContent{Simple: &types.Message{
			Subject: &types.Content{Data: aws.String(Subject(code)), Charset: aws.String("UTF-8")},
			Body:    &types.Body{Text: &types.Content{Data: aws.String(Body(code, kind)), Charset: aws.String("UTF-8")}},
		}},
	})
	return err
}

// Dir writes each email to a file in a directory, for local development
// without SES: never in a deployment.
type Dir struct{ Path string }

// SendCode implements Mailer.
func (d *Dir) SendCode(_ context.Context, to, code string, kind Kind) error {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return err
	}
	msg := fmt.Sprintf("To: %s\nSubject: %s\n\n%s", to, Subject(code), Body(code, kind))
	name := filepath.Join(d.Path, hex.EncodeToString(b[:])+".eml")
	return os.WriteFile(name, []byte(msg), 0o600)
}

// Normalize trims an address and lower-cases it. The service vault keys
// its email index the same way for the purpose of lookups; this is the
// member API's own key for rate limits and flows.
func Normalize(addr string) string { return strings.ToLower(strings.TrimSpace(addr)) }

// Valid reports whether addr (normalized) is an address the service
// vault accepts: the same rule as its NormalizeEmail (service-vault
// internal/connect), so that a visitor never gets a code for an address
// the invitation would then refuse. At most 254 bytes, valid UTF-8, no
// angle brackets, parentheses, quotes or spaces, a bare RFC 5322 address
// (net/mail), and a dot in the domain.
func Valid(addr string) bool {
	if addr == "" || len(addr) > 254 || !utf8.ValidString(addr) || strings.ContainsAny(addr, "<>()\" \t\r\n") {
		return false
	}
	a, err := netmail.ParseAddress(addr)
	if err != nil || a.Name != "" || a.Address != addr {
		return false
	}
	at := strings.LastIndexByte(addr, '@')
	return at > 0 && at < len(addr)-1 && strings.Contains(addr[at+1:], ".")
}
