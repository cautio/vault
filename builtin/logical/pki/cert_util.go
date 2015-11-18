package pki

import (
	"crypto/rand"
	"crypto/sha1"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/asn1"
	"encoding/pem"
	"fmt"
	"net"
	"regexp"
	"strings"
	"time"

	"github.com/hashicorp/vault/helper/certutil"
	"github.com/hashicorp/vault/logical"
	"github.com/hashicorp/vault/logical/framework"
)

type certUsage int

const (
	serverUsage certUsage = 1 << iota
	clientUsage
	codeSigningUsage
	emailProtectionUsage
	caUsage
)

type creationBundle struct {
	CommonName     string
	DNSNames       []string
	EmailAddresses []string
	IPAddresses    []net.IP
	IsCA           bool
	KeyType        string
	KeyBits        int
	SigningBundle  *caInfoBundle
	TTL            time.Duration
	Usage          certUsage

	// Only used when signing a CA cert
	UseCSRValues bool

	// URLs to encode into the certificate
	URLs *urlEntries

	// The maximum path length to encode
	MaxPathLength int
}

type caInfoBundle struct {
	certutil.ParsedCertBundle
	URLs *urlEntries
}

var (
	hostnameRegex                = regexp.MustCompile(`^(([a-zA-Z0-9]|[a-zA-Z0-9][a-zA-Z0-9\-]*[a-zA-Z0-9])\.)*([A-Za-z0-9]|[A-Za-z0-9][A-Za-z0-9\-]*[A-Za-z0-9])$`)
	oidExtensionBasicConstraints = []int{2, 5, 29, 19}
)

func oidInExtensions(oid asn1.ObjectIdentifier, extensions []pkix.Extension) bool {
	for _, e := range extensions {
		if e.Id.Equal(oid) {
			return true
		}
	}
	return false
}

func getFormat(data *framework.FieldData) string {
	format := data.Get("format").(string)
	switch format {
	case "pem":
	case "der":
	default:
		format = ""
	}
	return format
}

func validateKeyTypeLength(keyType string, keyBits int) *logical.Response {
	switch keyType {
	case "rsa":
		switch keyBits {
		case 1024:
		case 2048:
		case 4096:
		case 8192:
		default:
			return logical.ErrorResponse(fmt.Sprintf(
				"unsupported bit length for RSA key: %d", keyBits))
		}
	case "ec":
		switch keyBits {
		case 224:
		case 256:
		case 384:
		case 521:
		default:
			return logical.ErrorResponse(fmt.Sprintf(
				"unsupported bit length for EC key: %d", keyBits))
		}
	default:
		return logical.ErrorResponse(fmt.Sprintf(
			"unknown key type %s", keyType))
	}

	return nil
}

// Fetches the CA info. Unlike other certificates, the CA info is stored
// in the backend as a CertBundle, because we are storing its private key
func fetchCAInfo(req *logical.Request) (*caInfoBundle, error) {
	bundleEntry, err := req.Storage.Get("config/ca_bundle")
	if err != nil {
		return nil, certutil.InternalError{Err: fmt.Sprintf("unable to fetch local CA certificate/key: %v", err)}
	}
	if bundleEntry == nil {
		return nil, certutil.UserError{Err: "backend must be configured with a CA certificate/key"}
	}

	var bundle certutil.CertBundle
	if err := bundleEntry.DecodeJSON(&bundle); err != nil {
		return nil, certutil.InternalError{Err: fmt.Sprintf("unable to decode local CA certificate/key: %v", err)}
	}

	parsedBundle, err := bundle.ToParsedCertBundle()
	if err != nil {
		return nil, certutil.InternalError{Err: err.Error()}
	}

	if parsedBundle.Certificate == nil {
		return nil, certutil.InternalError{Err: "stored CA information not able to be parsed"}
	}

	caInfo := &caInfoBundle{*parsedBundle, nil}

	entries, err := getURLs(req)
	if err != nil {
		return nil, certutil.InternalError{Err: fmt.Sprintf("unable to fetch URL information: %v", err)}
	}
	if entries == nil {
		entries = &urlEntries{
			IssuingCertificates:   []string{},
			CRLDistributionPoints: []string{},
			OCSPServers:           []string{},
		}
	}
	caInfo.URLs = entries

	return caInfo, nil
}

// Allows fetching certificates from the backend; it handles the slightly
// separate pathing for CA, CRL, and revoked certificates.
func fetchCertBySerial(req *logical.Request, prefix, serial string) (*logical.StorageEntry, error) {
	var path string

	switch {
	case serial == "ca":
		path = "ca"
	case serial == "crl":
		path = "crl"
	case strings.HasPrefix(prefix, "revoked/"):
		path = "revoked/" + strings.Replace(strings.ToLower(serial), "-", ":", -1)
	default:
		path = "certs/" + strings.Replace(strings.ToLower(serial), "-", ":", -1)
	}

	certEntry, err := req.Storage.Get(path)
	if err != nil || certEntry == nil {
		return nil, certutil.InternalError{Err: fmt.Sprintf("certificate with serial number %s not found", serial)}
	}

	if certEntry.Value == nil || len(certEntry.Value) == 0 {
		return nil, certutil.InternalError{Err: fmt.Sprintf("returned certificate bytes for serial %s were empty", serial)}
	}

	return certEntry, nil
}

// Given a set of requested names for a certificate, verifies that all of them
// match the various toggles set in the role for controlling issuance.
// If one does not pass, it is returned in the string argument.
func validateNames(req *logical.Request, names []string, role *roleEntry) (string, error) {
	for _, name := range names {
		sanitizedName := name
		emailDomain := name
		isEmail := false
		isWildcard := false
		if strings.Contains(name, "@") {
			if !role.EmailProtectionFlag && !role.AllowAnyName {
				return name, nil
			}
			splitEmail := strings.Split(name, "@")
			if len(splitEmail) != 2 {
				return name, nil
			}
			sanitizedName = splitEmail[1]
			emailDomain = splitEmail[1]
			isEmail = true
		}
		if strings.HasPrefix(sanitizedName, "*.") {
			sanitizedName = sanitizedName[2:]
			isWildcard = true
		}

		if role.EnforceHostnames {
			if !hostnameRegex.MatchString(sanitizedName) {
				return name, nil
			}
		}

		if role.AllowAnyName {
			continue
		}

		if role.AllowLocalhost {
			if name == "localhost" || (isEmail && emailDomain == "localhost") {
				continue
			}

			if role.AllowSubdomains {
				if strings.HasSuffix(sanitizedName, "."+req.DisplayName) {
					// Regex won't match empty string, so protected against
					// someone trying to get base without it being allowed
					trimmed := strings.TrimSuffix(sanitizedName, "."+req.DisplayName)
					if hostnameRegex.MatchString(trimmed) {
						continue
					}
				}

				// The exception to matching the empty string is if it's *.
				// which is pulled out of the sanitized name, so see if it's
				// a wildcard matching the allowed base domain
				if isWildcard && sanitizedName == role.AllowedBaseDomain {
					continue
				}
			}
		}

		if role.AllowTokenDisplayName {
			// Don't check sanitized here because it needs to be exact
			if name == req.DisplayName || (isEmail && emailDomain == req.DisplayName) {
				continue
			}

			if role.AllowSubdomains {
				if strings.HasSuffix(sanitizedName, "."+req.DisplayName) {
					trimmed := strings.TrimSuffix(sanitizedName, "."+req.DisplayName)
					if hostnameRegex.MatchString(trimmed) {
						continue
					}
				}

				if isWildcard && sanitizedName == role.AllowedBaseDomain {
					continue
				}
			}
		}

		if len(role.AllowedBaseDomain) != 0 {
			if role.AllowBaseDomain &&
				(name == role.AllowedBaseDomain ||
					(isEmail && emailDomain == role.AllowedBaseDomain)) {
				continue
			}

			if role.AllowSubdomains {
				if strings.HasSuffix(sanitizedName, "."+role.AllowedBaseDomain) {
					trimmed := strings.TrimSuffix(sanitizedName, "."+role.AllowedBaseDomain)
					if hostnameRegex.MatchString(trimmed) {
						continue
					}
				}

				if isWildcard && sanitizedName == role.AllowedBaseDomain {
					continue
				}
			}
		}

		return name, nil
	}

	return "", nil
}

func generateCert(b *backend,
	role *roleEntry,
	signingBundle *caInfoBundle,
	isCA bool,
	req *logical.Request,
	data *framework.FieldData) (*certutil.ParsedCertBundle, error) {

	creationBundle, err := generateCreationBundle(b, role, signingBundle, nil, req, data)
	if err != nil {
		return nil, err
	}

	if isCA {
		creationBundle.IsCA = isCA

		if signingBundle == nil {
			// Generating a self-signed root certificate
			entries, err := getURLs(req)
			if err != nil {
				return nil, certutil.InternalError{Err: fmt.Sprintf("unable to fetch URL information: %v", err)}
			}
			if entries == nil {
				entries = &urlEntries{
					IssuingCertificates:   []string{},
					CRLDistributionPoints: []string{},
					OCSPServers:           []string{},
				}
			}
			creationBundle.URLs = entries

			if role.MaxPathLength == nil {
				creationBundle.MaxPathLength = -1
			} else {
				creationBundle.MaxPathLength = *role.MaxPathLength
			}
		}
	}

	parsedBundle, err := createCertificate(creationBundle)
	if err != nil {
		return nil, err
	}

	return parsedBundle, nil
}

// N.B.: This is only meant to be used for generating intermediate CAs.
// It skips some sanity checks.
func generateIntermediateCSR(b *backend,
	role *roleEntry,
	signingBundle *caInfoBundle,
	req *logical.Request,
	data *framework.FieldData) (*certutil.ParsedCSRBundle, error) {

	creationBundle, err := generateCreationBundle(b, role, signingBundle, nil, req, data)
	if err != nil {
		return nil, err
	}

	parsedBundle, err := createCSR(creationBundle)
	if err != nil {
		return nil, err
	}

	return parsedBundle, nil
}

func signCert(b *backend,
	role *roleEntry,
	signingBundle *caInfoBundle,
	isCA bool,
	useCSRValues bool,
	req *logical.Request,
	data *framework.FieldData) (*certutil.ParsedCertBundle, error) {

	csrString := data.Get("csr").(string)
	if csrString == "" {
		return nil, certutil.UserError{Err: fmt.Sprintf("\"csr\" is empty")}
	}

	pemBytes := []byte(csrString)
	pemBlock, pemBytes := pem.Decode(pemBytes)
	if pemBlock == nil {
		return nil, certutil.UserError{Err: "csr contains no data"}
	}
	csr, err := x509.ParseCertificateRequest(pemBlock.Bytes)
	if err != nil {
		return nil, certutil.UserError{Err: "certificate request could not be parsed"}
	}

	creationBundle, err := generateCreationBundle(b, role, signingBundle, csr, req, data)
	if err != nil {
		return nil, err
	}

	creationBundle.IsCA = isCA
	creationBundle.UseCSRValues = useCSRValues

	parsedBundle, err := signCertificate(creationBundle, csr)
	if err != nil {
		return nil, err
	}

	return parsedBundle, nil
}

func generateCreationBundle(b *backend,
	role *roleEntry,
	signingBundle *caInfoBundle,
	csr *x509.CertificateRequest,
	req *logical.Request,
	data *framework.FieldData) (*creationBundle, error) {
	var err error

	// Get the common name(s)
	var cn string
	if csr != nil {
		if role.UseCSRCommonName {
			cn = csr.Subject.CommonName
		}
	}
	if cn == "" {
		cn = data.Get("common_name").(string)
		if cn == "" {
			return nil, certutil.UserError{Err: `the common_name field is required, or must be provided in a CSR with "use_csr_common_name" set to true`}
		}
	}

	dnsNames := []string{}
	emailAddresses := []string{}
	if strings.Contains(cn, "@") {
		emailAddresses = append(emailAddresses, cn)
	} else {
		dnsNames = append(dnsNames, cn)
	}
	cnAltInt, ok := data.GetOk("alt_names")
	if ok {
		cnAlt := cnAltInt.(string)
		if len(cnAlt) != 0 {
			for _, v := range strings.Split(cnAlt, ",") {
				if strings.Contains(v, "@") {
					emailAddresses = append(emailAddresses, cn)
				} else {
					dnsNames = append(dnsNames, v)
				}
			}
		}
	}

	// Get any IP SANs
	ipAddresses := []net.IP{}
	ipAltInt, ok := data.GetOk("ip_sans")
	if ok {
		ipAlt := ipAltInt.(string)
		if len(ipAlt) != 0 {
			if !role.AllowIPSANs {
				return nil, certutil.UserError{Err: fmt.Sprintf(
					"IP Subject Alternative Names are not allowed in this role, but was provided %s", ipAlt)}
			}
			for _, v := range strings.Split(ipAlt, ",") {
				parsedIP := net.ParseIP(v)
				if parsedIP == nil {
					return nil, certutil.UserError{Err: fmt.Sprintf(
						"the value '%s' is not a valid IP address", v)}
				}
				ipAddresses = append(ipAddresses, parsedIP)
			}
		}
	}

	var ttlField string
	ttlFieldInt, ok := data.GetOk("ttl")
	if !ok {
		ttlField = role.TTL
	} else {
		ttlField = ttlFieldInt.(string)
	}

	var ttl time.Duration
	if len(ttlField) == 0 {
		ttl = b.System().DefaultLeaseTTL()
	} else {
		ttl, err = time.ParseDuration(ttlField)
		if err != nil {
			return nil, certutil.UserError{Err: fmt.Sprintf(
				"invalid requested ttl: %s", err)}
		}
	}

	var maxTTL time.Duration
	if len(role.MaxTTL) == 0 {
		maxTTL = b.System().MaxLeaseTTL()
	} else {
		maxTTL, err = time.ParseDuration(role.MaxTTL)
		if err != nil {
			return nil, certutil.UserError{Err: fmt.Sprintf(
				"invalid ttl: %s", err)}
		}
	}

	if ttl > maxTTL {
		// Don't error if they were using system defaults, only error if
		// they specifically chose a bad TTL
		if len(ttlField) == 0 {
			ttl = maxTTL
		} else {
			return nil, certutil.UserError{Err: fmt.Sprintf(
				"ttl is larger than maximum allowed (%d)", maxTTL/time.Second)}
		}
	}

	if signingBundle != nil &&
		time.Now().Add(ttl).After(signingBundle.Certificate.NotAfter) {
		return nil, certutil.UserError{Err: fmt.Sprintf(
			"cannot satisfy request, as TTL is beyond the expiration of the CA certificate")}
	}

	badName, err := validateNames(req, dnsNames, role)
	if len(badName) != 0 {
		return nil, certutil.UserError{Err: fmt.Sprintf(
			"name %s not allowed by this role", badName)}
	} else if err != nil {
		return nil, certutil.InternalError{Err: fmt.Sprintf(
			"error validating name %s: %s", badName, err)}
	}

	badName, err = validateNames(req, emailAddresses, role)
	if len(badName) != 0 {
		return nil, certutil.UserError{Err: fmt.Sprintf(
			"email %s not allowed by this role", badName)}
	} else if err != nil {
		return nil, certutil.InternalError{Err: fmt.Sprintf(
			"error validating name %s: %s", badName, err)}
	}

	var usage certUsage
	if role.ServerFlag {
		usage = usage | serverUsage
	}
	if role.ClientFlag {
		usage = usage | clientUsage
	}
	if role.CodeSigningFlag {
		usage = usage | codeSigningUsage
	}
	if role.EmailProtectionFlag {
		usage = usage | emailProtectionUsage
	}

	creationBundle := &creationBundle{
		CommonName:     cn,
		DNSNames:       dnsNames,
		EmailAddresses: emailAddresses,
		IPAddresses:    ipAddresses,
		KeyType:        role.KeyType,
		KeyBits:        role.KeyBits,
		SigningBundle:  signingBundle,
		TTL:            ttl,
		Usage:          usage,
	}

	if signingBundle == nil {
		return creationBundle, nil
	}

	creationBundle.URLs = signingBundle.URLs
	if role.MaxPathLength != nil {
		creationBundle.MaxPathLength = *role.MaxPathLength
	} else {
		switch {
		case signingBundle.Certificate.MaxPathLen < 0:
			creationBundle.MaxPathLength = -1
		case signingBundle.Certificate.MaxPathLen == 0 &&
			signingBundle.Certificate.MaxPathLenZero:
			// The signing function will ensure that we do not issue a CA cert
			creationBundle.MaxPathLength = 0
		default:
			// If this takes it to zero, we handle this case later if
			// necessary
			creationBundle.MaxPathLength = signingBundle.Certificate.MaxPathLen - 1
		}
	}

	return creationBundle, nil
}

// Performs the heavy lifting of creating a certificate. Returns
// a fully-filled-in ParsedCertBundle.
func createCertificate(creationInfo *creationBundle) (*certutil.ParsedCertBundle, error) {
	var err error
	result := &certutil.ParsedCertBundle{}

	serialNumber, err := certutil.GenerateSerialNumber()
	if err != nil {
		return nil, err
	}

	resultIface := interface{}(result)
	if err := certutil.GeneratePrivateKey(creationInfo.KeyType,
		creationInfo.KeyBits,
		resultIface.(certutil.EmbeddedParsedPrivateKeyContainer)); err != nil {
		return nil, err
	}

	subjKeyID, err := certutil.GetSubjKeyID(result.PrivateKey)
	if err != nil {
		return nil, certutil.InternalError{Err: fmt.Sprintf("error getting subject key ID: %s", err)}
	}

	subject := pkix.Name{
		CommonName: creationInfo.CommonName,
	}

	certTemplate := &x509.Certificate{
		SerialNumber:   serialNumber,
		Subject:        subject,
		NotBefore:      time.Now(),
		NotAfter:       time.Now().Add(creationInfo.TTL),
		KeyUsage:       x509.KeyUsage(x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment | x509.KeyUsageKeyAgreement),
		IsCA:           false,
		SubjectKeyId:   subjKeyID,
		DNSNames:       creationInfo.DNSNames,
		EmailAddresses: creationInfo.EmailAddresses,
		IPAddresses:    creationInfo.IPAddresses,
	}

	if creationInfo.Usage&serverUsage != 0 {
		certTemplate.ExtKeyUsage = append(certTemplate.ExtKeyUsage, x509.ExtKeyUsageServerAuth)
	}
	if creationInfo.Usage&clientUsage != 0 {
		certTemplate.ExtKeyUsage = append(certTemplate.ExtKeyUsage, x509.ExtKeyUsageClientAuth)
	}
	if creationInfo.Usage&codeSigningUsage != 0 {
		certTemplate.ExtKeyUsage = append(certTemplate.ExtKeyUsage, x509.ExtKeyUsageCodeSigning)
	}
	if creationInfo.Usage&emailProtectionUsage != 0 {
		certTemplate.ExtKeyUsage = append(certTemplate.ExtKeyUsage, x509.ExtKeyUsageEmailProtection)
	}

	certTemplate.IssuingCertificateURL = creationInfo.URLs.IssuingCertificates
	certTemplate.CRLDistributionPoints = creationInfo.URLs.CRLDistributionPoints
	certTemplate.OCSPServer = creationInfo.URLs.OCSPServers

	var certBytes []byte
	if creationInfo.SigningBundle != nil {
		switch creationInfo.SigningBundle.PrivateKeyType {
		case certutil.RSAPrivateKey:
			certTemplate.SignatureAlgorithm = x509.SHA256WithRSA
		case certutil.ECPrivateKey:
			certTemplate.SignatureAlgorithm = x509.ECDSAWithSHA256
		}

		caCert := creationInfo.SigningBundle.Certificate

		certBytes, err = x509.CreateCertificate(rand.Reader, certTemplate, caCert, result.PrivateKey.Public(), creationInfo.SigningBundle.PrivateKey)
	} else {
		// Creating a self-signed root
		if creationInfo.MaxPathLength == 0 {
			certTemplate.MaxPathLen = 0
			certTemplate.MaxPathLenZero = true
		} else {
			certTemplate.MaxPathLen = creationInfo.MaxPathLength
		}

		switch creationInfo.KeyType {
		case "rsa":
			certTemplate.SignatureAlgorithm = x509.SHA256WithRSA
		case "ec":
			certTemplate.SignatureAlgorithm = x509.ECDSAWithSHA256
		}

		certTemplate.BasicConstraintsValid = true
		certTemplate.IsCA = true
		certTemplate.KeyUsage = x509.KeyUsage(certTemplate.KeyUsage | x509.KeyUsageCertSign | x509.KeyUsageCRLSign)
		certTemplate.ExtKeyUsage = append(certTemplate.ExtKeyUsage, x509.ExtKeyUsageOCSPSigning)
		certBytes, err = x509.CreateCertificate(rand.Reader, certTemplate, certTemplate, result.PrivateKey.Public(), result.PrivateKey)
	}

	if err != nil {
		return nil, certutil.InternalError{Err: fmt.Sprintf("unable to create certificate: %s", err)}
	}

	result.CertificateBytes = certBytes
	result.Certificate, err = x509.ParseCertificate(certBytes)
	if err != nil {
		return nil, certutil.InternalError{Err: fmt.Sprintf("unable to parse created certificate: %s", err)}
	}

	if creationInfo.SigningBundle != nil {
		result.IssuingCABytes = creationInfo.SigningBundle.CertificateBytes
		result.IssuingCA = creationInfo.SigningBundle.Certificate
	} else {
		result.IssuingCABytes = result.CertificateBytes
		result.IssuingCA = result.Certificate
	}

	return result, nil
}

// Creates a CSR. This is currently only meant for use when
// generating an intermediate certificate.
func createCSR(creationInfo *creationBundle) (*certutil.ParsedCSRBundle, error) {
	var err error
	result := &certutil.ParsedCSRBundle{}

	resultIface := interface{}(result)
	if err := certutil.GeneratePrivateKey(creationInfo.KeyType,
		creationInfo.KeyBits,
		resultIface.(certutil.EmbeddedParsedPrivateKeyContainer)); err != nil {
		return nil, err
	}

	// Like many root CAs, other information is ignored
	subject := pkix.Name{
		CommonName: creationInfo.CommonName,
	}

	csrTemplate := &x509.CertificateRequest{
		Subject:        subject,
		DNSNames:       creationInfo.DNSNames,
		EmailAddresses: creationInfo.EmailAddresses,
		IPAddresses:    creationInfo.IPAddresses,
	}

	switch creationInfo.KeyType {
	case "rsa":
		csrTemplate.SignatureAlgorithm = x509.SHA256WithRSA
	case "ec":
		csrTemplate.SignatureAlgorithm = x509.ECDSAWithSHA256
	}

	csr, err := x509.CreateCertificateRequest(rand.Reader, csrTemplate, result.PrivateKey)
	if err != nil {
		return nil, certutil.InternalError{Err: fmt.Sprintf("unable to create certificate: %s", err)}
	}

	result.CSRBytes = csr
	result.CSR, err = x509.ParseCertificateRequest(csr)
	if err != nil {
		return nil, certutil.InternalError{Err: fmt.Sprintf("unable to parse created certificate: %s", err)}
	}

	return result, nil
}

// Performs the heavy lifting of generating a certificate from a CSR.
// Returns a ParsedCertBundle sans private keys.
func signCertificate(creationInfo *creationBundle,
	csr *x509.CertificateRequest) (*certutil.ParsedCertBundle, error) {
	switch {
	case creationInfo == nil:
		return nil, certutil.UserError{Err: "nil creation info given to signCertificate"}
	case creationInfo.SigningBundle == nil:
		return nil, certutil.UserError{Err: "nil signing bundle given to signCertificate"}
	case csr == nil:
		return nil, certutil.UserError{Err: "nil csr given to signCertificate"}
	}

	err := csr.CheckSignature()
	if err != nil {
		return nil, certutil.UserError{Err: "request signature invalid"}
	}

	result := &certutil.ParsedCertBundle{}

	serialNumber, err := certutil.GenerateSerialNumber()
	if err != nil {
		return nil, err
	}

	marshaledKey, err := x509.MarshalPKIXPublicKey(csr.PublicKey)
	if err != nil {
		return nil, certutil.InternalError{Err: fmt.Sprintf("error marshalling public key: %s", err)}
	}
	subjKeyID := sha1.Sum(marshaledKey)

	subject := pkix.Name{
		CommonName: creationInfo.CommonName,
	}

	certTemplate := &x509.Certificate{
		SerialNumber: serialNumber,
		Subject:      subject,
		NotBefore:    time.Now(),
		NotAfter:     time.Now().Add(creationInfo.TTL),
		SubjectKeyId: subjKeyID[:],
	}

	switch creationInfo.SigningBundle.PrivateKeyType {
	case certutil.RSAPrivateKey:
		certTemplate.SignatureAlgorithm = x509.SHA256WithRSA
	case certutil.ECPrivateKey:
		certTemplate.SignatureAlgorithm = x509.ECDSAWithSHA256
	}

	if creationInfo.UseCSRValues {
		certTemplate.Subject = csr.Subject

		certTemplate.DNSNames = csr.DNSNames
		certTemplate.EmailAddresses = csr.EmailAddresses
		certTemplate.IPAddresses = csr.IPAddresses

		certTemplate.ExtraExtensions = csr.Extensions
		// Do not sign a CA certificate if they didn't go through the sign-intermediate
		// endpoint
		if !creationInfo.IsCA && oidInExtensions(oidExtensionBasicConstraints, certTemplate.ExtraExtensions) {
			return nil, certutil.UserError{Err: "will not sign a CSR asking for CA rights through this endpoint"}
		}

	} else {
		certTemplate.DNSNames = creationInfo.DNSNames
		certTemplate.EmailAddresses = creationInfo.EmailAddresses
		certTemplate.IPAddresses = creationInfo.IPAddresses

		certTemplate.KeyUsage = x509.KeyUsage(x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment | x509.KeyUsageKeyAgreement)

		if creationInfo.Usage&serverUsage != 0 {
			certTemplate.ExtKeyUsage = append(certTemplate.ExtKeyUsage, x509.ExtKeyUsageServerAuth)
		}
		if creationInfo.Usage&clientUsage != 0 {
			certTemplate.ExtKeyUsage = append(certTemplate.ExtKeyUsage, x509.ExtKeyUsageClientAuth)
		}
		if creationInfo.Usage&codeSigningUsage != 0 {
			certTemplate.ExtKeyUsage = append(certTemplate.ExtKeyUsage, x509.ExtKeyUsageCodeSigning)
		}
		if creationInfo.Usage&emailProtectionUsage != 0 {
			certTemplate.ExtKeyUsage = append(certTemplate.ExtKeyUsage, x509.ExtKeyUsageEmailProtection)
		}

		if creationInfo.IsCA {
			certTemplate.KeyUsage = x509.KeyUsage(certTemplate.KeyUsage | x509.KeyUsageCertSign | x509.KeyUsageCRLSign)
			certTemplate.ExtKeyUsage = append(certTemplate.ExtKeyUsage, x509.ExtKeyUsageOCSPSigning)
		}
	}

	var certBytes []byte
	caCert := creationInfo.SigningBundle.Certificate

	certTemplate.IssuingCertificateURL = creationInfo.URLs.IssuingCertificates
	certTemplate.CRLDistributionPoints = creationInfo.URLs.CRLDistributionPoints
	certTemplate.OCSPServer = creationInfo.SigningBundle.URLs.OCSPServers

	if creationInfo.IsCA {
		certTemplate.BasicConstraintsValid = true
		certTemplate.IsCA = true

		if creationInfo.SigningBundle.Certificate.MaxPathLen == 0 &&
			creationInfo.SigningBundle.Certificate.MaxPathLenZero {
			return nil, certutil.UserError{Err: "signing certificate has a max path length of zero, and cannot issue further CA certificates"}
		}

		certTemplate.MaxPathLen = creationInfo.MaxPathLength
		if certTemplate.MaxPathLen == 0 {
			certTemplate.MaxPathLenZero = true
		}
	}

	certBytes, err = x509.CreateCertificate(rand.Reader, certTemplate, caCert, csr.PublicKey, creationInfo.SigningBundle.PrivateKey)

	if err != nil {
		return nil, certutil.InternalError{Err: fmt.Sprintf("unable to create certificate: %s", err)}
	}

	result.CertificateBytes = certBytes
	result.Certificate, err = x509.ParseCertificate(certBytes)
	if err != nil {
		return nil, certutil.InternalError{Err: fmt.Sprintf("unable to parse created certificate: %s", err)}
	}

	result.IssuingCABytes = creationInfo.SigningBundle.CertificateBytes
	result.IssuingCA = creationInfo.SigningBundle.Certificate

	return result, nil
}
