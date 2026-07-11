package auth

import (
	"golang.org/x/crypto/bcrypt"
)

// PasswordChecker verifies a submitted password against a stored bcrypt hash.
type PasswordChecker struct {
	hash []byte
}

// NewPasswordChecker wraps a bcrypt hash for verification.
func NewPasswordChecker(bcryptHash string) *PasswordChecker {
	return &PasswordChecker{hash: []byte(bcryptHash)}
}

// HashPassword returns a bcrypt hash of the given plaintext password.
func HashPassword(plain string) (string, error) {
	b, err := bcrypt.GenerateFromPassword([]byte(plain), bcrypt.DefaultCost)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// Verify reports whether plain matches the stored hash.
func (p *PasswordChecker) Verify(plain string) bool {
	if len(p.hash) == 0 {
		return false
	}
	return bcrypt.CompareHashAndPassword(p.hash, []byte(plain)) == nil
}

// ProxyHeaderAuth trusts a reverse proxy to set an identity header (e.g.
// X-Forwarded-User) and optionally enforces an allowlist of subjects.
type ProxyHeaderAuth struct {
	Header    string
	Allowlist map[string]struct{} // empty => any non-empty subject allowed
}

// NewProxyHeaderAuth builds a proxy-header authenticator. An empty allowlist
// permits any non-empty header value.
func NewProxyHeaderAuth(header string, allow []string) *ProxyHeaderAuth {
	set := make(map[string]struct{}, len(allow))
	for _, a := range allow {
		if a != "" {
			set[a] = struct{}{}
		}
	}
	if header == "" {
		header = "X-Forwarded-User"
	}
	return &ProxyHeaderAuth{Header: header, Allowlist: set}
}

// Subject returns the trusted subject if the header is present and allowed.
func (p *ProxyHeaderAuth) Subject(value string) (string, bool) {
	if value == "" {
		return "", false
	}
	if len(p.Allowlist) == 0 {
		return value, true
	}
	if _, ok := p.Allowlist[value]; ok {
		return value, true
	}
	return "", false
}
