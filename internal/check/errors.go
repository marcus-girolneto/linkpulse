package check

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io"
	"net"
	"strings"
	"syscall"
)

// retryableErr reports whether a transport error is worth another attempt.
// Timeouts, resets and truncated responses are typically transient; DNS
// misses, refused connections and TLS failures are not.
func retryableErr(err error) bool {
	if errors.Is(err, ErrTimeout) ||
		errors.Is(err, io.ErrUnexpectedEOF) ||
		errors.Is(err, io.EOF) ||
		errors.Is(err, syscall.ECONNRESET) ||
		errors.Is(err, syscall.EPIPE) {
		return true
	}
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return dnsErr.IsTimeout || dnsErr.IsTemporary
	}
	var netErr net.Error
	return errors.As(err, &netErr) && netErr.Timeout()
}

// Describe turns an error from Result.Err into a short, stable phrase
// suitable for a report column.
func Describe(err error) string {
	if err == nil {
		return ""
	}
	var (
		dnsErr      *net.DNSError
		certErr     *tls.CertificateVerificationError
		unknownAuth x509.UnknownAuthorityError
		hostErr     x509.HostnameError
		invalidErr  x509.CertificateInvalidError
		recordErr   tls.RecordHeaderError
	)
	switch {
	case errors.Is(err, context.Canceled):
		return "canceled"
	case errors.Is(err, ErrTimeout), errors.Is(err, context.DeadlineExceeded):
		return "timeout"
	case errors.Is(err, ErrTooManyRedirects):
		return "too many redirects"
	case errors.Is(err, ErrRedirectLoop):
		return "redirect loop"
	case errors.As(err, &dnsErr):
		if dnsErr.IsNotFound {
			return "dns: no such host"
		}
		return "dns: lookup failed"
	case errors.Is(err, syscall.ECONNREFUSED):
		return "connection refused"
	case errors.Is(err, syscall.ECONNRESET):
		return "connection reset"
	case errors.Is(err, io.EOF), errors.Is(err, io.ErrUnexpectedEOF):
		return "connection closed by server"
	case errors.As(err, &hostErr):
		return "tls: certificate hostname mismatch"
	case errors.As(err, &unknownAuth):
		return "tls: unknown certificate authority"
	case errors.As(err, &invalidErr):
		if invalidErr.Reason == x509.Expired {
			return "tls: certificate expired"
		}
		return "tls: invalid certificate"
	case errors.As(err, &certErr):
		return "tls: certificate verification failed"
	case errors.As(err, &recordErr):
		return "tls: server did not speak TLS"
	}
	msg := err.Error()
	if len(msg) > 120 {
		msg = strings.TrimSpace(msg[:117]) + "..."
	}
	return msg
}
