package network

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/asn1"
	"errors"
	"math/big"
	"net"
	"sync"
	"time"

	"github.com/dedis/kyber/sign/schnorr"
	"github.com/dedis/kyber/util/random"
	"github.com/dedis/onet/log"
)

// certMaker holds the data necessary to make a certificate on the
// fly, cache it, expire it, and give it to crypto/tls via the
// GetCertificate and GetClientCertificate callbacks.
//
// TODO: make the CN be the public key, and include a signature over the CN in the cert proving that we
// hold the private key associated with the public key.

type certMaker struct {
	sync.Mutex
	c       *tls.Certificate
	expires time.Time
	si      *ServerIdentity
	suite   Suite
	serial  *big.Int
}

func newCertMaker(si *ServerIdentity, s Suite) (*certMaker, error) {
	cm := &certMaker{
		si:     si,
		suite:  s,
		serial: new(big.Int),
	}

	// Choose a random 128-bit serial number to start with.
	r := random.Bits(128, true, random.New())
	cm.serial.SetBytes(r)

	return cm, nil
}

func (cm *certMaker) getCertificate(hello *tls.ClientHelloInfo) (*tls.Certificate, error) {
	cm.Lock()
	defer cm.Unlock()

	if cm.expires.Before(time.Now()) {
		err := cm.makeCert()
		if err != nil {
			return nil, err
		}
	}

	return cm.c, nil
}

// TODO: Get an enterprise object ID for DEDIS.
var oidDedisSig = asn1.ObjectIdentifier{1, 3, 6, 1, 4, 1, 2499, 1, 1}

func isDedisSig(in asn1.ObjectIdentifier) bool {
	if len(in) != len(oidDedisSig) {
		return false
	}
	for i := range in {
		if in[i] != oidDedisSig[i] {
			return false
		}
	}
	return true
}

func (cm *certMaker) makeCert() error {
	// For each new certificate, increment the serial number.
	one := new(big.Int).SetUint64(1)
	cm.serial.Add(cm.serial, one)

	subj := pkix.Name{CommonName: cm.si.Public.String()}

	// Create a signature that proves that:
	// 1. during the lifetime of this certificate (i.e. for this serial number)
	// 2. for this public key
	// 3. we have control of the private key that is associated with the public
	// key named in the CN.
	// Do this using the same standardized ASN.1 marshaling that x509 uses so
	// that anyone trying to check these signatures themselves will be able to
	// easily do so.
	buf := &bytes.Buffer{}
	serAsn1, err := asn1.Marshal(cm.serial)
	if err != nil {
		return err
	}
	buf.Write(serAsn1)
	subAsn1, err := asn1.Marshal(subj.CommonName)
	if err != nil {
		return err
	}
	buf.Write(subAsn1)
	sig, err := schnorr.Sign(cm.suite, cm.si.private, buf.Bytes())

	tmpl := &x509.Certificate{
		BasicConstraintsValid: true,
		MaxPathLen:            1,
		IsCA:                  false,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
		NotAfter:              time.Now().Add(2 * 24 * time.Hour),
		NotBefore:             time.Now().Add(-1 * 24 * time.Hour),
		SerialNumber:          cm.serial,
		SignatureAlgorithm:    x509.ECDSAWithSHA384,
		Subject:               subj,
		ExtraExtensions: []pkix.Extension{
			{
				Id:       oidDedisSig,
				Critical: false,
				Value:    sig,
			},
		},
	}

	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return err
	}

	cDer, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, k.Public(), k)
	if err != nil {
		return err
	}
	certs, err := x509.ParseCertificates(cDer)
	if err != nil {
		return err
	}
	if len(certs) < 1 {
		return errors.New("no certificate found")
	}

	cm.c = &tls.Certificate{
		PrivateKey:  k,
		Certificate: [][]byte{cDer},
		Leaf:        certs[0],
	}
	// To be safe, we expire our cache of this cert one hour
	// before clients will refuse it.
	cm.expires = tmpl.NotAfter.Add(-1 * time.Hour)

	return nil
}

// NewTLSListener makes a new TCPListner that is configured for TLS.
// TODO: Why can't we just use NewTCPListener like usual, but detect
// the ConnType from the ServerIdentity?
func NewTLSListener(si *ServerIdentity, s Suite) (*TCPListener, error) {
	tcp, err := NewTCPListener(si.Address, s)
	if err != nil {
		return nil, err
	}

	ch, err := newCertMaker(si, s)
	if err != nil {
		return nil, err
	}

	tlsCfg := &tls.Config{
		GetCertificate: ch.getCertificate,
	}
	tcp.listener = tls.NewListener(tcp.listener, tlsCfg)
	return tcp, nil
}

// NewTLSAddress returns a new Address that has type TLS with the given
// address addr.
func NewTLSAddress(addr string) Address {
	return NewAddress(TLS, addr)
}

func tlsConfig(si *ServerIdentity, suite Suite) *tls.Config {
	return &tls.Config{
		// InsecureSkipVerify means that crypto/tls will not be checking
		// the cert for us.
		InsecureSkipVerify: true,
		// Thus, we need to have our own verification function.
		VerifyPeerCertificate: func(rawCerts [][]byte, vrf [][]*x509.Certificate) (err error) {
			defer func() {
				if err == nil {
					log.Lvl3("verify cert ->", "ok")
				} else {
					log.Lvl3("verify cert ->", err)
				}
			}()

			if len(rawCerts) != 1 {
				return errors.New("expected exactly one certificate")
			}
			certs, err := x509.ParseCertificates(rawCerts[0])
			if err != nil {
				return err
			}
			if len(certs) != 1 {
				return errors.New("expected exactly one certificate")
			}
			cert := certs[0]

			// Check that the certificate is self-signed as expected and not expired.
			self := x509.NewCertPool()
			self.AddCert(cert)
			opts := x509.VerifyOptions{
				Roots: self,
			}
			_, err = cert.Verify(opts)
			if err != nil {
				return err
			}

			// Check that the CN is the same as the public key.
			err = cert.VerifyHostname(si.Public.String())
			if err != nil {
				return err
			}

			// Check that our extension exists.
			var sig []byte
			for _, x := range cert.Extensions {
				if isDedisSig(x.Id) {
					sig = x.Value
					break
				}
			}
			if sig == nil {
				return errors.New("DEDIS signature not found")
			}

			// Check that the DEDIS signature is valid w.r.t. si.Public.
			buf := &bytes.Buffer{}
			serAsn1, err := asn1.Marshal(cert.SerialNumber)
			if err != nil {
				return err
			}
			buf.Write(serAsn1)
			subAsn1, err := asn1.Marshal(cert.Subject.CommonName)
			if err != nil {
				return err
			}
			buf.Write(subAsn1)
			err = schnorr.Verify(suite, si.Public, buf.Bytes(), sig)

			return err
		},
	}
}

// NewTLSConn will open a TCPConn to the given server over TLS.
// It will check that the remote server has proven
// it holds the given Public key by self-signing a certificate
// linked to that key.
func NewTLSConn(si *ServerIdentity, suite Suite) (conn *TCPConn, err error) {
	log.Lvl3("NewTLSConn to:", si.Public)
	if si.Address.ConnType() != TLS {
		return nil, errors.New("not a tls server")
	}
	netAddr := si.Address.NetworkAddress()
	for i := 1; i <= MaxRetryConnect; i++ {
		var c net.Conn
		c, err = tls.Dial("tcp", netAddr, tlsConfig(si, suite))
		if err == nil {
			conn = &TCPConn{
				endpoint: si.Address,
				conn:     c,
				suite:    suite,
			}
			return
		}
		if i < MaxRetryConnect {
			time.Sleep(WaitRetry)
		}
	}
	if err == nil {
		err = ErrTimeout
	}
	return
}
