package authority

import (
	"context"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/smallstep/certificates/authority/admin"
	"github.com/smallstep/certificates/authority/provisioner"
	"github.com/smallstep/certificates/errs"
	"go.step.sm/crypto/jose"
	"go.step.sm/linkedca"
	"golang.org/x/crypto/ssh"
)

// Claims extends jose.Claims with step attributes.
type Claims struct {
	jose.Claims
	SANs  []string `json:"sans,omitempty"`
	Email string   `json:"email,omitempty"`
	Nonce string   `json:"nonce,omitempty"`
}

type skipTokenReuseKey struct{}

// NewContextWithSkipTokenReuse creates a new context from ctx and attaches a
// value to skip the token reuse.
func NewContextWithSkipTokenReuse(ctx context.Context) context.Context {
	return context.WithValue(ctx, skipTokenReuseKey{}, true)
}

// SkipTokenReuseFromContext returns if the token reuse needs to be ignored.
func SkipTokenReuseFromContext(ctx context.Context) bool {
	m, _ := ctx.Value(skipTokenReuseKey{}).(bool)
	return m
}

// authorizeToken parses the token and returns the provisioner used to generate
// the token. This method enforces the One-Time use policy (tokens can only be
// used once).
func (a *Authority) authorizeToken(ctx context.Context, token string) (provisioner.Interface, error) {
	// Validate payload
	tok, err := jose.ParseSigned(token)
	if err != nil {
		return nil, errs.Wrap(http.StatusUnauthorized, err, "authority.authorizeToken: error parsing token")
	}

	// Get claims w/out verification. We need to look up the provisioner
	// key in order to verify the claims and we need the issuer from the claims
	// before we can look up the provisioner.
	var claims Claims
	if err := tok.UnsafeClaimsWithoutVerification(&claims); err != nil {
		return nil, errs.Wrap(http.StatusUnauthorized, err, "authority.authorizeToken")
	}

	// TODO: use new persistence layer abstraction.
	// Do not accept tokens issued before the start of the ca.
	// This check is meant as a stopgap solution to the current lack of a persistence layer.
	if a.config.AuthorityConfig != nil && !a.config.AuthorityConfig.DisableIssuedAtCheck {
		if claims.IssuedAt != nil && claims.IssuedAt.Time().Before(a.startTime) {
			return nil, errs.Unauthorized("authority.authorizeToken: token issued before the bootstrap of certificate authority")
		}
	}

	// This method will also validate the audiences for JWK provisioners.
	p, ok := a.provisioners.LoadByToken(tok, &claims.Claims)
	if !ok {
		return nil, errs.Unauthorized("authority.authorizeToken: provisioner "+
			"not found or invalid audience (%s)", strings.Join(claims.Audience, ", "))
	}

	// Store the token to protect against reuse unless it's skipped.
	// If we cannot get a token id from the provisioner, just hash the token.
	if !SkipTokenReuseFromContext(ctx) {
		if err := a.UseToken(token, p); err != nil {
			return nil, err
		}
	}

	return p, nil
}

// AuthorizeAdminToken authorize an Admin token.
func (a *Authority) AuthorizeAdminToken(r *http.Request, token string) (*linkedca.Admin, error) {
	jwt, err := jose.ParseSigned(token)
	if err != nil {
		return nil, admin.WrapError(admin.ErrorUnauthorizedType, err, "adminHandler.authorizeToken; error parsing x5c token")
	}

	verifiedChains, err := jwt.Headers[0].Certificates(x509.VerifyOptions{
		Roots:     a.rootX509CertPool,
		KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	})
	if err != nil {
		return nil, admin.WrapError(admin.ErrorUnauthorizedType, err,
			"adminHandler.authorizeToken; error verifying x5c certificate chain in token")
	}
	leaf := verifiedChains[0][0]

	if leaf.KeyUsage&x509.KeyUsageDigitalSignature == 0 {
		return nil, admin.NewError(admin.ErrorUnauthorizedType, "adminHandler.authorizeToken; certificate used to sign x5c token cannot be used for digital signature")
	}

	// Using the leaf certificates key to validate the claims accomplishes two
	// things:
	//   1. Asserts that the private key used to sign the token corresponds
	//      to the public certificate in the `x5c` header of the token.
	//   2. Asserts that the claims are valid - have not been tampered with.
	var claims jose.Claims
	if err := jwt.Claims(leaf.PublicKey, &claims); err != nil {
		return nil, admin.WrapError(admin.ErrorUnauthorizedType, err, "adminHandler.authorizeToken; error parsing x5c claims")
	}

	prov, err := a.LoadProvisionerByCertificate(leaf)
	if err != nil {
		return nil, err
	}

	// Check that the token has not been used.
	if err := a.UseToken(token, prov); err != nil {
		return nil, admin.WrapError(admin.ErrorUnauthorizedType, err, "adminHandler.authorizeToken; error with reuse token")
	}

	// According to "rfc7519 JSON Web Token" acceptable skew should be no
	// more than a few minutes.
	if err := claims.ValidateWithLeeway(jose.Expected{
		Issuer: prov.GetName(),
		Time:   time.Now().UTC(),
	}, time.Minute); err != nil {
		return nil, admin.WrapError(admin.ErrorUnauthorizedType, err, "x5c.authorizeToken; invalid x5c claims")
	}

	// validate audience: path matches the current path
	if r.URL.Path != claims.Audience[0] {
		return nil, admin.NewError(admin.ErrorUnauthorizedType,
			"x5c.authorizeToken; x5c token has invalid audience "+
				"claim (aud); expected %s, but got %s", r.URL.Path, claims.Audience)
	}

	if claims.Subject == "" {
		return nil, admin.NewError(admin.ErrorUnauthorizedType,
			"x5c.authorizeToken; x5c token subject cannot be empty")
	}

	var (
		ok  bool
		adm *linkedca.Admin
	)
	adminFound := false
	adminSANs := append([]string{leaf.Subject.CommonName}, leaf.DNSNames...)
	adminSANs = append(adminSANs, leaf.EmailAddresses...)
	for _, san := range adminSANs {
		if adm, ok = a.LoadAdminBySubProv(san, claims.Issuer); ok {
			adminFound = true
			break
		}
	}
	if !adminFound {
		return nil, admin.NewError(admin.ErrorUnauthorizedType,
			"adminHandler.authorizeToken; unable to load admin with subject(s) %s and provisioner '%s'",
			adminSANs, claims.Issuer)
	}

	if strings.HasPrefix(r.URL.Path, "/admin/admins") && (r.Method != "GET") && adm.Type != linkedca.Admin_SUPER_ADMIN {
		return nil, admin.NewError(admin.ErrorUnauthorizedType, "must have super admin access to make this request")
	}

	return adm, nil
}

// UseToken stores the token to protect against reuse.
//
// This method currently ignores any error coming from the GetTokenID, but it
// should specifically ignore the error provisioner.ErrAllowTokenReuse.
func (a *Authority) UseToken(token string, prov provisioner.Interface) error {
	if reuseKey, err := prov.GetTokenID(token); err == nil {
		if reuseKey == "" {
			sum := sha256.Sum256([]byte(token))
			reuseKey = strings.ToLower(hex.EncodeToString(sum[:]))
		}
		ok, err := a.db.UseToken(reuseKey, token)
		if err != nil {
			return errs.Wrap(http.StatusInternalServerError, err,
				"authority.authorizeToken: failed when attempting to store token")
		}
		if !ok {
			return errs.Unauthorized("authority.authorizeToken: token already used")
		}
	}
	return nil
}

// Authorize grabs the method from the context and authorizes the request by
// validating the one-time-token.
func (a *Authority) Authorize(ctx context.Context, token string) ([]provisioner.SignOption, error) {
	var opts = []interface{}{errs.WithKeyVal("token", token)}

	switch m := provisioner.MethodFromContext(ctx); m {
	case provisioner.SignMethod:
		signOpts, err := a.authorizeSign(ctx, token)
		return signOpts, errs.Wrap(http.StatusInternalServerError, err, "authority.Authorize", opts...)
	case provisioner.RevokeMethod:
		return nil, errs.Wrap(http.StatusInternalServerError, a.authorizeRevoke(ctx, token), "authority.Authorize", opts...)
	case provisioner.SSHSignMethod:
		if a.sshCAHostCertSignKey == nil && a.sshCAUserCertSignKey == nil {
			return nil, errs.NotImplemented("authority.Authorize; ssh certificate flows are not enabled", opts...)
		}
		signOpts, err := a.authorizeSSHSign(ctx, token)
		return signOpts, errs.Wrap(http.StatusInternalServerError, err, "authority.Authorize", opts...)
	case provisioner.SSHRenewMethod:
		if a.sshCAHostCertSignKey == nil && a.sshCAUserCertSignKey == nil {
			return nil, errs.NotImplemented("authority.Authorize; ssh certificate flows are not enabled", opts...)
		}
		_, err := a.authorizeSSHRenew(ctx, token)
		return nil, errs.Wrap(http.StatusInternalServerError, err, "authority.Authorize", opts...)
	case provisioner.SSHRevokeMethod:
		return nil, errs.Wrap(http.StatusInternalServerError, a.authorizeSSHRevoke(ctx, token), "authority.Authorize", opts...)
	case provisioner.SSHRekeyMethod:
		if a.sshCAHostCertSignKey == nil && a.sshCAUserCertSignKey == nil {
			return nil, errs.NotImplemented("authority.Authorize; ssh certificate flows are not enabled", opts...)
		}
		_, signOpts, err := a.authorizeSSHRekey(ctx, token)
		return signOpts, errs.Wrap(http.StatusInternalServerError, err, "authority.Authorize", opts...)
	default:
		return nil, errs.InternalServer("authority.Authorize; method %d is not supported", append([]interface{}{m}, opts...)...)
	}
}

// authorizeSign loads the provisioner from the token and calls the provisioner
// AuthorizeSign method. Returns a list of methods to apply to the signing flow.
func (a *Authority) authorizeSign(ctx context.Context, token string) ([]provisioner.SignOption, error) {
	p, err := a.authorizeToken(ctx, token)
	if err != nil {
		return nil, errs.Wrap(http.StatusInternalServerError, err, "authority.authorizeSign")
	}
	signOpts, err := p.AuthorizeSign(ctx, token)
	if err != nil {
		return nil, errs.Wrap(http.StatusInternalServerError, err, "authority.authorizeSign")
	}
	return signOpts, nil
}

// AuthorizeSign authorizes a signature request by validating and authenticating
// a token that must be sent w/ the request.
//
// NOTE: This method is deprecated and should not be used. We make it available
// in the short term os as not to break existing clients.
func (a *Authority) AuthorizeSign(token string) ([]provisioner.SignOption, error) {
	ctx := provisioner.NewContextWithMethod(context.Background(), provisioner.SignMethod)
	return a.Authorize(ctx, token)
}

// authorizeRevoke locates the provisioner used to generate the authenticating
// token and then performs the token validation flow.
func (a *Authority) authorizeRevoke(ctx context.Context, token string) error {
	p, err := a.authorizeToken(ctx, token)
	if err != nil {
		return errs.Wrap(http.StatusInternalServerError, err, "authority.authorizeRevoke")
	}
	if err := p.AuthorizeRevoke(ctx, token); err != nil {
		return errs.Wrap(http.StatusInternalServerError, err, "authority.authorizeRevoke")
	}
	return nil
}

// authorizeRenew locates the provisioner (using the provisioner extension in the cert), and checks
// if for the configured provisioner, the renewal is enabled or not. If the
// extra extension cannot be found, authorize the renewal by default.
//
// TODO(mariano): should we authorize by default?
func (a *Authority) authorizeRenew(cert *x509.Certificate) error {
	serial := cert.SerialNumber.String()
	var opts = []interface{}{errs.WithKeyVal("serialNumber", serial)}
	isRevoked, err := a.IsRevoked(serial)
	if err != nil {
		return errs.Wrap(http.StatusInternalServerError, err, "authority.authorizeRenew", opts...)
	}
	if isRevoked {
		return errs.Unauthorized("authority.authorizeRenew: certificate has been revoked", opts...)
	}

	p, ok := a.provisioners.LoadByCertificate(cert)
	if !ok {
		return errs.Unauthorized("authority.authorizeRenew: provisioner not found", opts...)
	}
	if err := p.AuthorizeRenew(context.Background(), cert); err != nil {
		return errs.Wrap(http.StatusInternalServerError, err, "authority.authorizeRenew", opts...)
	}
	return nil
}

// authorizeSSHCertificate returns an error if the given certificate is revoked.
func (a *Authority) authorizeSSHCertificate(ctx context.Context, cert *ssh.Certificate) error {
	var err error
	var isRevoked bool

	serial := strconv.FormatUint(cert.Serial, 10)
	if lca, ok := a.adminDB.(interface {
		IsSSHRevoked(string) (bool, error)
	}); ok {
		isRevoked, err = lca.IsSSHRevoked(serial)
	} else {
		isRevoked, err = a.db.IsSSHRevoked(serial)
	}
	if err != nil {
		return errs.Wrap(http.StatusInternalServerError, err, "authority.authorizeSSHCertificate", errs.WithKeyVal("serialNumber", serial))
	}
	if isRevoked {
		return errs.Unauthorized("authority.authorizeSSHCertificate: certificate has been revoked", errs.WithKeyVal("serialNumber", serial))
	}
	return nil
}

// authorizeSSHSign loads the provisioner from the token, checks that it has not
// been used again and calls the provisioner AuthorizeSSHSign method. Returns a
// list of methods to apply to the signing flow.
func (a *Authority) authorizeSSHSign(ctx context.Context, token string) ([]provisioner.SignOption, error) {
	p, err := a.authorizeToken(ctx, token)
	if err != nil {
		return nil, errs.Wrap(http.StatusUnauthorized, err, "authority.authorizeSSHSign")
	}
	signOpts, err := p.AuthorizeSSHSign(ctx, token)
	if err != nil {
		return nil, errs.Wrap(http.StatusUnauthorized, err, "authority.authorizeSSHSign")
	}
	return signOpts, nil
}

// authorizeSSHRenew authorizes an SSH certificate renewal request, by
// validating the contents of an SSHPOP token.
func (a *Authority) authorizeSSHRenew(ctx context.Context, token string) (*ssh.Certificate, error) {
	p, err := a.authorizeToken(ctx, token)
	if err != nil {
		return nil, errs.Wrap(http.StatusInternalServerError, err, "authority.authorizeSSHRenew")
	}
	cert, err := p.AuthorizeSSHRenew(ctx, token)
	if err != nil {
		return nil, errs.Wrap(http.StatusInternalServerError, err, "authority.authorizeSSHRenew")
	}
	return cert, nil
}

// authorizeSSHRekey authorizes an SSH certificate rekey request, by
// validating the contents of an SSHPOP token.
func (a *Authority) authorizeSSHRekey(ctx context.Context, token string) (*ssh.Certificate, []provisioner.SignOption, error) {
	p, err := a.authorizeToken(ctx, token)
	if err != nil {
		return nil, nil, errs.Wrap(http.StatusInternalServerError, err, "authority.authorizeSSHRekey")
	}
	cert, signOpts, err := p.AuthorizeSSHRekey(ctx, token)
	if err != nil {
		return nil, nil, errs.Wrap(http.StatusInternalServerError, err, "authority.authorizeSSHRekey")
	}
	return cert, signOpts, nil
}

// authorizeSSHRevoke authorizes an SSH certificate revoke request, by
// validating the contents of an SSHPOP token.
func (a *Authority) authorizeSSHRevoke(ctx context.Context, token string) error {
	p, err := a.authorizeToken(ctx, token)
	if err != nil {
		return errs.Wrap(http.StatusInternalServerError, err, "authority.authorizeSSHRevoke")
	}
	if err = p.AuthorizeSSHRevoke(ctx, token); err != nil {
		return errs.Wrap(http.StatusInternalServerError, err, "authority.authorizeSSHRevoke")
	}
	return nil
}
