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
	"os"
	"path/filepath"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sesv2"
	"github.com/aws/aws-sdk-go-v2/service/sesv2/types"
)

// Mailer sends a one-time code to an address.
type Mailer interface {
	SendCode(ctx context.Context, to, code string) error
}

// Subject and Body are the code email. The code is also in the subject,
// so it can be read from a notification.
func Subject(code string) string { return code + " is your VettID member service code" }

// Body is the code email's plain-text body.
func Body(code string) string {
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
func (s *SES) SendCode(ctx context.Context, to, code string) error {
	_, err := s.Client.SendEmail(ctx, &sesv2.SendEmailInput{
		FromEmailAddress: aws.String(s.From),
		Destination:      &types.Destination{ToAddresses: []string{to}},
		Content: &types.EmailContent{Simple: &types.Message{
			Subject: &types.Content{Data: aws.String(Subject(code)), Charset: aws.String("UTF-8")},
			Body:    &types.Body{Text: &types.Content{Data: aws.String(Body(code)), Charset: aws.String("UTF-8")}},
		}},
	})
	return err
}

// Dir writes each email to a file in a directory, for local development
// without SES: never in a deployment.
type Dir struct{ Path string }

// SendCode implements Mailer.
func (d *Dir) SendCode(_ context.Context, to, code string) error {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return err
	}
	msg := fmt.Sprintf("To: %s\nSubject: %s\n\n%s", to, Subject(code), Body(code))
	name := filepath.Join(d.Path, hex.EncodeToString(b[:])+".eml")
	return os.WriteFile(name, []byte(msg), 0o600)
}

// Normalize trims an address and lower-cases it. The service vault keys
// its email index the same way for the purpose of lookups; this is the
// member API's own key for rate limits and flows.
func Normalize(addr string) string { return strings.ToLower(strings.TrimSpace(addr)) }

// Valid reports whether addr looks like an address: one @, something on
// both sides, a dot in the domain, at most 254 bytes, no spaces or
// control characters. The code email is the real check.
func Valid(addr string) bool {
	if len(addr) > 254 || strings.ContainsFunc(addr, func(r rune) bool { return r <= ' ' || r == 0x7f }) {
		return false
	}
	local, domain, ok := strings.Cut(addr, "@")
	return ok && local != "" && !strings.Contains(domain, "@") && strings.Contains(domain, ".") &&
		!strings.HasPrefix(domain, ".") && !strings.HasSuffix(domain, ".")
}
