package radiusauth

import (
	"errors"
	"fmt"
	"net"
	"net/http"
	"time"

	"github.com/boltdb/bolt"
	"github.com/jamesboswell/radius"
	"github.com/mholt/caddy/caddyhttp/httpserver"
)

// RADIUS is the
type RADIUS struct {
	// Connection
	Next     httpserver.Handler
	SiteRoot string
	Config   radiusConfig
	db       *bolt.DB
}
type radiusConfig struct {
	Server        string
	Secret        string
	Timeout       int
	Retries       int
	nasid         string
	realm         string
	requestFilter filter
	cache         string
	cachetimeout  time.Duration
}

// ServeHTTP implements the http.Handler
func (a RADIUS) ServeHTTP(w http.ResponseWriter, r *http.Request) (int, error) {

	realm := "Basic realm=" + fmt.Sprintf("\"%s\"", a.Config.realm)
	// Pass-through when no paths match filter or no filters
	// if filter not nil and auth is NOT required, then just return
	if a.Config.requestFilter != nil && !a.Config.requestFilter.shouldAuthenticate(r) {
		return a.Next.ServeHTTP(w, r)
	}

	username, password, ok := r.BasicAuth()

	if !ok {
		w.Header().Set("WWW-Authenticate", realm)
		return http.StatusUnauthorized, nil
	}
	if username == "" || password == "" {
		w.Header().Set("WWW-Authenticate", realm)
		return http.StatusUnauthorized, errors.New("[radiusauth] Blank username or password")
	}
	// cacheseek checks if provided Basic Auth credentials are cached and match
	// if credentials do not match cached entry, force RADIUS authentication
	isCached, err := cacheSeek(a, username, password)
	if isCached == true && err == nil {
		return a.Next.ServeHTTP(w, r)
	}
	if err != nil {
		fmt.Println(err)
	}

	// Provided credentials not found in cache or did not match
	// send username, password to RADIUS server for authentication
	isAuthenticated, err := auth(a.Config, username, password)

	// Handle auth errors
	if err != nil {
		// If RADIUS server timing out return 504 - StatusGatewayTimeout
		if isTimeout(err) {
			return http.StatusGatewayTimeout, err
		}
		// otherwise return 500 Internal Server Error
		return http.StatusInternalServerError, err
	}

	// if RADIUS authentication failed, return 401
	if !isAuthenticated {
		w.Header().Set("WWW-Authenticate", realm)
		return http.StatusUnauthorized, nil
	}
	// if RADIUS authenticated, cache the username, password entry return Handler
	if isAuthenticated {
		if err := cacheWrite(a, username, password); err != nil {
			return http.StatusInternalServerError, fmt.Errorf("[radiusauth] cache-write for %s FAILED: %s", username, err)
		}
	}
	return a.Next.ServeHTTP(w, r)
}

// auth generates a RADIUS authentication request for username
func auth(r radiusConfig, username string, password string) (bool, error) {
	// Create a new RADIUS packet for Access-Request
	// NAS-Identifier required by some servers such as CiscoSecure ACS
	packet := radius.New(radius.CodeAccessRequest, []byte(r.Secret))
	packet.Add("User-Name", username)
	packet.Add("User-Password", password)
	packet.Add("NAS-Identifier", r.nasid)

	client := radius.Client{
		DialTimeout: 3 * time.Second, // TODO user defined timeouts
		ReadTimeout: 3 * time.Second,
	}

	radiusServer := r.Server
	reply, err := client.Exchange(packet, radiusServer)
	if err != nil {
		if isTimeout(err) {
			return false, err
		}
		//TODO handle other errors?
		return false, err
	}

	// RADIUS Access-Accept is a successful authentication
	if reply.Code == radius.CodeAccessAccept {
		return true, nil
	}
	// Any other reply is a failed authentication
	return false, nil
}

// isTimeout checks for net timeout
func isTimeout(err error) bool {
	if opErr, ok := err.(*net.OpError); ok && opErr.Timeout() {
		return true
	}
	return false
}
