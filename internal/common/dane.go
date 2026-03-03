package common

import (
	"bytes"
	"crypto/sha256"
	"crypto/sha512"
	"crypto/x509"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/miekg/dns"
)

// TLSA represents a TLSA DNS record parameters
type TLSA struct {
	Usage        uint8
	Selector     uint8
	MatchingType uint8
	Data         []byte
}

// VerifyDANE checks a list of peer certificates against a single TLSA record
func VerifyDANE(peerCerts []*x509.Certificate, tlsa *TLSA) error {
	if len(peerCerts) == 0 {
		return errors.New("no peer certificates provided")
	}

	var certsToCheck []*x509.Certificate
	switch tlsa.Usage {
	case 1, 3: // End-entity certificate
		certsToCheck = []*x509.Certificate{peerCerts[0]}
	case 0, 2: // Trust anchor (can be any in the chain)
		certsToCheck = peerCerts
	default:
		return fmt.Errorf("unsupported TLSA Usage: %d", tlsa.Usage)
	}

	for _, cert := range certsToCheck {
		if matchTLSA(cert, tlsa) {
			return nil
		}
	}

	return errors.New("TLSA record does not match any provided certificates")
}

func matchTLSA(cert *x509.Certificate, tlsa *TLSA) bool {
	var dataToMatch []byte
	switch tlsa.Selector {
	case 0: // Full certificate
		dataToMatch = cert.Raw
	case 1: // SubjectPublicKeyInfo
		dataToMatch = cert.RawSubjectPublicKeyInfo
	default:
		return false
	}

	var hash []byte
	switch tlsa.MatchingType {
	case 0: // Exact match
		hash = append([]byte(nil), dataToMatch...)
	case 1: // SHA-256
		h := sha256.Sum256(dataToMatch)
		hash = h[:]
	case 2: // SHA-512
		h := sha512.Sum512(dataToMatch)
		hash = h[:]
	default:
		return false
	}

	return bytes.Equal(hash, tlsa.Data)
}

// ParseTLSAData converts hex encoded cert association data into bytes
func ParseTLSAData(hexData string) ([]byte, error) {
	return hex.DecodeString(hexData)
}

// VerifyDANEParams is a helper to verify with raw parameters
func VerifyDANEParams(peerCerts []*x509.Certificate, usage, selector, matchingType uint8, hexData string) error {
	data, err := ParseTLSAData(hexData)
	if err != nil {
		return fmt.Errorf("invalid TLSA hex data: %v", err)
	}

	tlsa := &TLSA{
		Usage:        usage,
		Selector:     selector,
		MatchingType: matchingType,
		Data:         data,
	}

	return VerifyDANE(peerCerts, tlsa)
}

// getNameserver looks up a nameserver from /etc/resolv.conf
func getNameserver() string {
	ns := "8.8.8.8:53"
	data, err := os.ReadFile("/etc/resolv.conf")
	if err == nil {
		lines := strings.Split(string(data), "\n")
		for _, line := range lines {
			if strings.HasPrefix(line, "nameserver ") {
				if parts := strings.Fields(line); len(parts) >= 2 {
					ip := parts[1]
					if strings.Contains(ip, ":") {
						ns = "[" + ip + "]:53"
					} else {
						ns = ip + ":53"
					}
					break
				}
			}
		}
	}
	return ns
}

// ResolveAndVerifyDANE resolves the TLSA records for a given domain/port and verifies peer certificates
func ResolveAndVerifyDANE(domain string, port string, peerCerts []*x509.Certificate) error {
	if len(peerCerts) == 0 {
		return errors.New("no peer certificates provided")
	}

	qNameStr := fmt.Sprintf("_%s._tcp.%s.", port, domain)

	m := new(dns.Msg)
	m.SetQuestion(dns.Fqdn(qNameStr), dns.TypeTLSA)
	m.RecursionDesired = true

	c := new(dns.Client)
	in, _, err := c.Exchange(m, getNameserver())
	if err != nil {
		return fmt.Errorf("failed to lookup TLSA records: %v", err)
	}

	if in.Rcode != dns.RcodeSuccess {
		return fmt.Errorf("DNS query failed with Rcode: %v", in.Rcode)
	}

	foundTLSA := false
	var lastErr error

	for _, a := range in.Answer {
		if tlsaRecord, ok := a.(*dns.TLSA); ok {
			foundTLSA = true

			// We construct our TLSA struct based on the parsed data
			certData, err := hex.DecodeString(tlsaRecord.Certificate)
			if err != nil {
				lastErr = fmt.Errorf("failed to decode certificate hex: %v", err)
				continue
			}

			tlsa := &TLSA{
				Usage:        tlsaRecord.Usage,
				Selector:     tlsaRecord.Selector,
				MatchingType: tlsaRecord.MatchingType,
				Data:         certData,
			}

			err = VerifyDANE(peerCerts, tlsa)
			if err == nil {
				return nil // Successfully matched at least one TLSA record
			}
			lastErr = err
		}
	}

	if !foundTLSA {
		return fmt.Errorf("no TLSA records found for %s", qNameStr)
	}

	return fmt.Errorf("certificate verification failed against all TLSA records: %v", lastErr)
}
