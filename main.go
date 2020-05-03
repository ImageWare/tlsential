package main

import (
	"crypto/rand"
	"crypto/tls"
	"flag"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/ImageWare/TLSential/acme"
	"github.com/ImageWare/TLSential/api"
	"github.com/ImageWare/TLSential/certificate"
	"github.com/ImageWare/TLSential/repository/boltdb"
	"github.com/ImageWare/TLSential/service"
	"github.com/ImageWare/TLSential/ui"

	"github.com/boltdb/bolt"
)

// Version is the official version of the server app.
const Version = "v0.0.1"

func main() {
	fmt.Println("///- Starting up TLSential")
	fmt.Printf("//- Version %s\n", Version)

	var email string
	var port int
	var dbFile string
	var secretReset bool
	var tlsCert string
	var tlsKey string
	var noHTTPS bool
	var noHTTPRedirect bool

	// Grab any command line arguments
	flag.StringVar(&email, "email", "test@example.com", "Email address for Let's Encrypt account")
	flag.IntVar(&port, "port", 443, "port for webserver to run on")
	flag.StringVar(&dbFile, "db", "tlsential.db", "filename for boltdb database")
	flag.BoolVar(&secretReset, "secret-reset", false, "reset the JWT secret - invalidates all API sessions")
	flag.StringVar(&tlsCert, "tls-cert", "/etc/pki/tlsential.crt", "file path for tls certificate")
	flag.StringVar(&tlsKey, "tls-key", "/etc/pki/tlsential.key", "file path for tls private key")
	flag.BoolVar(&noHTTPS, "no-https", false, "flag to run over http (HIGHLY INSECURE)")
	flag.BoolVar(&noHTTPRedirect, "no-http-redirect", false, "flag to not redirect HTTP requests to HTTPS")

	flag.Parse()

	// Open our database file.
	db, err := bolt.Open(dbFile, 0666, nil)
	if err != nil {
		log.Fatal(err)
	}
	defer db.Close()

	if secretReset {
		resetSecret(db)
	}

	initSecret(db)

	// Start a goroutine to automatically renew certificates in the DB.
	cs := newCertService(db)
	as := newACMEService(db)
	go autoRenewal(cs, as)

	// Run http server concurrently
	// Load routes for the server
	mux := NewMux(db)

	tlsConfig := &tls.Config{
		// Causes servers to use Go's default ciphersuite preferences,
		// which are tuned to avoid attacks. Does nothing on clients.
		PreferServerCipherSuites: true,
		// Only use curves which have assembly implementations
		CurvePreferences: []tls.CurveID{
			tls.CurveP256,
			tls.X25519, // Go 1.8 only
		},
		MinVersion: tls.VersionTLS12,
		CipherSuites: []uint16{
			tls.TLS_ECDHE_ECDSA_WITH_AES_256_GCM_SHA384,
			tls.TLS_ECDHE_RSA_WITH_AES_256_GCM_SHA384,
			tls.TLS_ECDHE_ECDSA_WITH_CHACHA20_POLY1305, // Go 1.8 only
			tls.TLS_ECDHE_RSA_WITH_CHACHA20_POLY1305,   // Go 1.8 only
			tls.TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256,
			tls.TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256,

			// Best disabled, as they don't provide Forward Secrecy,
			// but might be necessary for some clients
			// tls.TLS_RSA_WITH_AES_256_GCM_SHA384,
			// tls.TLS_RSA_WITH_AES_128_GCM_SHA256,
		},
	}

	s := http.Server{
		ReadTimeout:  5 * time.Second,
		WriteTimeout: 10 * time.Second,
		IdleTimeout:  120 * time.Second,
		Addr:         fmt.Sprintf(":%d", port),
		Handler:      removeTrailingSlash(mux),
		TLSConfig:    tlsConfig,
	}

	if noHTTPS {
		fmt.Println("*** WARNING ***")
		fmt.Println("* It is extremely unsafe to use this app without proper HTTPS *")
		log.Fatal(s.ListenAndServe())
	} else {

		//Create an HTTP server that exists solely to redirect to https
		httpSrv := http.Server{
			ReadTimeout:  5 * time.Second,
			WriteTimeout: 5 * time.Second,
			IdleTimeout:  5 * time.Second,
			// Addr:         ":8081",
			Handler: http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				w.Header().Set("Connection", "close")
				url := "https://" + req.Host + req.URL.String()
				http.Redirect(w, req, url, http.StatusMovedPermanently)
			}),
		}

		if !noHTTPRedirect {
			go func() { log.Fatal(httpSrv.ListenAndServe()) }()
		}

		log.Fatal(s.ListenAndServeTLS(tlsCert, tlsKey))
	}

}

// removeTrailingSlash removes any final / off the end of routes, otherwise
// gorilla mux treats url/ and url differently which is unneeded in this app.
func removeTrailingSlash(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			r.URL.Path = strings.TrimSuffix(r.URL.Path, "/")
		}
		next.ServeHTTP(w, r)
	})
}

// NewMux returns a new http.ServeMux with established routes.
func NewMux(db *bolt.DB) *http.ServeMux {
	apiHandler := newAPIHandler(db)
	cs := newCertService(db)
	uiHandler := ui.NewHandler("TLSential", cs)

	s := http.NewServeMux()
	s.Handle("/", uiHandler.Route())
	s.Handle("/api/", apiHandler.Route())

	return s
}

func initSecret(db *bolt.DB) {
	crepo, err := boltdb.NewConfigRepository(db)
	if err != nil {
		log.Fatal(err)
	}

	s, err := crepo.JWTSecret()
	if err != nil {
		log.Fatal(err)
	}
	if s.ValidSecret() != nil {
		c := 32
		b := make([]byte, c)
		_, err := rand.Read(b)
		if err != nil {
			log.Fatal(err)
		}
		crepo.SetJWTSecret(b)
	}
}

func resetSecret(db *bolt.DB) {
	crepo, err := boltdb.NewConfigRepository(db)
	if err != nil {
		log.Fatal(err)
	}

	err = crepo.SetJWTSecret(nil)
	if err != nil {
		log.Fatal(err)
	}
}

// newAppController takes a bolt.DB and builds all necessary repos and usescases
// for this app.
func newAPIHandler(db *bolt.DB) api.Handler {
	urepo, err := boltdb.NewUserRepository(db)
	if err != nil {
		log.Fatal(err)
	}

	crepo, err := boltdb.NewConfigRepository(db)
	if err != nil {
		log.Fatal(err)
	}

	chrepo, err := boltdb.NewChallengeConfigRepository(db)
	if err != nil {
		log.Fatal(err)
	}

	certrepo, err := boltdb.NewCertificateRepository(db)
	if err != nil {
		log.Fatal(err)
	}

	us := service.NewUserService(urepo)
	cs := service.NewConfigService(crepo, us)
	chs := service.NewChallengeConfigService(chrepo)
	crs := service.NewCertificateService(certrepo)
	as := service.NewAcmeService(crs, chs)

	return api.NewHandler(Version, us, cs, chs, crs, as)
}

// helper for creating an ACME Service from a db.
func newACMEService(db *bolt.DB) acme.Service {
	chrepo, err := boltdb.NewChallengeConfigRepository(db)
	if err != nil {
		log.Fatal(err)
	}

	certrepo, err := boltdb.NewCertificateRepository(db)
	if err != nil {
		log.Fatal(err)
	}

	chs := service.NewChallengeConfigService(chrepo)
	crs := service.NewCertificateService(certrepo)
	as := service.NewAcmeService(crs, chs)

	return as
}

// helper for creating an Certificate Service from a db.
func newCertService(db *bolt.DB) certificate.Service {
	certrepo, err := boltdb.NewCertificateRepository(db)
	if err != nil {
		log.Fatal(err)
	}

	crs := service.NewCertificateService(certrepo)

	return crs
}
