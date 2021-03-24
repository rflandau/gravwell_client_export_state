/*************************************************************************
 * Copyright 2021 Gravwell, Inc. All rights reserved.
 * Contact: <legal@gravwell.io>
 *
 * This software may be modified and distributed under the terms of the
 * BSD 2-clause license. See the LICENSE file for details.
 **************************************************************************/

// Package client wraps the Gravwell REST API.
package client

import (
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"path"
	"strings"
	"sync"
	"time"

	"github.com/gravwell/gravwell/v3/client/objlog"
	"github.com/gravwell/gravwell/v3/client/types"

	"github.com/gorilla/websocket"
	"golang.org/x/net/publicsuffix"
)

const (
	maxRedirects    = 3
	methodLogoutAll = `DELETE`
	methodLogout    = `PUT`

	// Allow for crazy long timeouts in case we are sitting on crazy large downloads
	// this is kind of a crazy safety net, where we kill connections if things actually stall out
	defaultRequestTimeout = time.Hour * 24
	clientUserAgent       = `GravwellCLI`
	authHeaderName        = `Authorization`
)

var (
	errNoRedirect error = nil

	ErrInvalidTestStatus error = errors.New("Invalid status on webserver test")
	ErrAccountLocked     error = errors.New(`Account is Locked`)
	ErrLoginFail         error = errors.New(`Username and Password are incorrect`)
	ErrNotSynced         error = errors.New(`Client has not been synced`)
	ErrNoLogin           error = errors.New("Not logged in")
	ErrEmptyUserAgent    error = errors.New("UserAgent cannot be empty")
)

// Client handles interaction with the server's REST APIs and websockets.
type Client struct {
	hm          *headerMap //additional header values to add to requests
	qm          *queryMap  // stuff to append to the URL e.g. ?admin=true
	server      string
	serverURL   *url.URL
	clnt        *http.Client
	timeout     time.Duration
	mtx         *sync.Mutex
	state       clientState
	lastNotifId uint64
	enforceCert bool
	sessionData ActiveSession
	userDetails types.UserDetails
	objLog      objlog.ObjLog
	wsScheme    string
	httpScheme  string
	userAgent   string
	tlsConfig   *tls.Config
	transport   *http.Transport
}

// The ActiveSession structure represents a login session on the server. The
// JWT field contains a negotiated authentication token (with expiration).
type ActiveSession struct {
	JWT                string
	LastNotificationID uint64
}

func init() {
	errNoRedirect = errors.New(`Refused to follow redirect`)
}

// NewClient connects to the specified server and returns a new Client object.
// The useHttps parameter enables or disables SSL.
// Setting enforceCertificate to false will disable SSL certificate validation,
// allowing self-signed certs.
func NewClient(server string, enforceCertificate, useHttps bool, objLogger objlog.ObjLog) (*Client, error) {
	var wsScheme string
	var httpScheme string
	var tlsConfig *tls.Config
	if server == "" {
		return nil, errors.New("invalid base URL")
	}
	if useHttps {
		wsScheme = `wss`
		httpScheme = `https`
		tlsConfig = &tls.Config{InsecureSkipVerify: !enforceCertificate}
	} else {
		wsScheme = `ws`
		httpScheme = `http`
	}
	serverURL, err := url.Parse(fmt.Sprintf("%s://%s", httpScheme, server))
	if err != nil {
		return nil, err
	}

	//setup a transport that allows a bad client if the user asks for it
	tr := &http.Transport{
		TLSClientConfig: tlsConfig,
	}
	options := cookiejar.Options{
		PublicSuffixList: publicsuffix.List,
	}
	jar, err := cookiejar.New(&options)
	if err != nil {
		log.Fatal(err)
	}
	clnt := http.Client{
		Transport:     tr,
		CheckRedirect: redirectPolicy, //use default redirect policy
		Timeout:       defaultRequestTimeout,
		Jar:           jar,
	}
	//create the header map and stuff our user-agent in there
	hdrMap := newHeaderMap()
	hdrMap.add(`User-Agent`, clientUserAgent)

	//if not object logger is passed in, just get a nil one
	if objLogger == nil {
		objLogger, _ = objlog.NewNilLogger()
	}

	//actually build and return the client
	return &Client{
		server:      server,
		serverURL:   serverURL,
		clnt:        &clnt,
		timeout:     defaultRequestTimeout,
		mtx:         &sync.Mutex{},
		state:       STATE_NEW,
		enforceCert: enforceCertificate,
		hm:          hdrMap,
		qm:          newQueryMap(),
		objLog:      objLogger,
		wsScheme:    wsScheme,
		httpScheme:  httpScheme,
		tlsConfig:   tlsConfig,
		transport:   tr,
		userAgent:   clientUserAgent,
	}, nil
}

func (c *Client) Server() string {
	return c.server
}

// ServerIP attempts to return an IP address for the webserver.
// If it cannot resolve the hostname, it will return an unspecified IP
func (c *Client) ServerIP() net.IP {
	// Split if necessary
	server := c.server
	if h, _, err := net.SplitHostPort(server); err == nil {
		server = h
	}
	// First try and parse it as an IP
	if ip := net.ParseIP(server); ip != nil {
		return ip
	}

	// Then do a lookup
	if addrs, err := net.LookupIP(server); err == nil && len(addrs) > 0 {
		return addrs[0]
	}
	return net.IPv4(0, 0, 0, 0)
}

//we allow a single redirect to allow for the muxer to clean up requests
//basically the gorilla muxer we are using will force a 301 redirect on a path
//such as '//' to '/'  We allow for one of those
func redirectPolicy(req *http.Request, via []*http.Request) error {
	if len(via) >= 2 {
		return errors.New("Disallowed multiple redirects")
	} else if len(via) == 1 {
		if path.Clean(req.URL.Path) == path.Clean(via[0].URL.Path) {
			//ensure that any set headers are transported forward
			lReq := via[len(via)-1]
			for k, v := range lReq.Header {
				_, ok := req.Header[k]
				if !ok {
					req.Header[k] = v
				}
			}
			return nil
		}
		return errors.New("Disallowed non-equivelent redirects")
	}
	return errors.New("Uknown redirect chain")
}

// Test checks if the webserver is responding to HTTP requests.
func (c *Client) Test() error {
	c.mtx.Lock()
	defer c.mtx.Unlock()
	uri := fmt.Sprintf("%s://%s%s", c.httpScheme, c.server, TEST_URL)
	resp, err := c.clnt.Get(uri)
	if err != nil {
		return err
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		return ErrInvalidTestStatus
	}
	return nil
}

// TestLogin checks if the client is successfully logged in, indicated by a nil return value.
func (c *Client) TestLogin() error {
	c.mtx.Lock()
	defer c.mtx.Unlock()

	return c.getStaticURL(TEST_AUTH_URL, nil)
}

// Login authenticates the client to the webserver using the specified username and password.
func (c *Client) Login(user, pass string) error {
	c.mtx.Lock()
	defer c.mtx.Unlock()

	if c.state != STATE_NEW && c.state != STATE_LOGGED_OFF {
		return errors.New("Client not ready for login")
	}
	if user == "" {
		return errors.New("Invalid username")
	}

	//build up URL we are going to throw at
	uri := fmt.Sprintf("%s://%s%s", c.httpScheme, c.server, LOGIN_URL)

	//build up the form that we are going to throw at login url
	loginCreds := url.Values{}
	loginCreds.Add(USER_FIELD, user)
	loginCreds.Add(PASS_FIELD, pass)

	//build up the request
	req, err := http.NewRequest(`POST`, uri, strings.NewReader(loginCreds.Encode()))
	c.hm.populateRequest(req.Header)
	req.Header.Set(`Content-Type`, `application/x-www-form-urlencoded`)

	//post the form to the base login url
	resp, err := c.clnt.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	//check response
	if resp == nil {
		//this really should never happen
		return errors.New("Invalid response")
	}

	//look for the redirect response
	switch resp.StatusCode {
	case http.StatusLocked:
		return ErrAccountLocked
	case http.StatusUnprocessableEntity:
		return ErrLoginFail
	case http.StatusOK:
	default:
		return fmt.Errorf("Invalid response: %d", resp.StatusCode)
	}

	var loginResp types.LoginResponse
	if err := json.NewDecoder(resp.Body).Decode(&loginResp); err != nil {
		return err
	}
	return c.processLoginResponse(loginResp)
}

func (c *Client) importLoginToken(token string) (err error) {
	if len(token) == 0 {
		err = errors.New("invalid token")
	} else {
		//save away our tokens in our header map, which will be injected into requests
		c.hm.add(authHeaderName, "Bearer "+token)

		c.sessionData = ActiveSession{
			JWT: token,
		}

		c.state = STATE_AUTHED //we just assume that we are logged in if we are importing a token
	}
	return
}

// ImportLoginToken takes an existing JWT token and loads it into the client.
// The token is not validated by the client at this point; use the TestLogin function
// to verify that the token is valid.
// If you need to save and restore sessions, consider using the SessionData and InheritSession
// functions instead.
func (c *Client) ImportLoginToken(token string) (err error) {
	c.mtx.Lock()
	err = c.importLoginToken(token)
	c.mtx.Unlock()
	return
}

func (c *Client) processLoginResponse(loginResp types.LoginResponse) error {
	//check that we had a good login
	if !loginResp.LoginStatus {
		return errors.New(loginResp.Reason)
	}

	//double check that we have the JWT
	if loginResp.JWT == "" {
		return errors.New("Failed to retrieve JWT")
	}
	return c.importLoginToken(loginResp.JWT)
}

// Logout terminates the current session on the server.
func (c *Client) Logout() error {
	if c.state != STATE_AUTHED {
		return errors.New("not logged in")
	}
	if err := c.methodStaticURL(methodLogout, LOGOUT_URL, nil); err != nil {
		return err
	}
	return nil
}

// LogoutAll asks the server to terminate the current session and every other session for our user.
func (c *Client) LogoutAll() error {
	if c.state != STATE_AUTHED {
		return errors.New("not logged in")
	}
	if err := c.methodStaticURL(methodLogoutAll, LOGOUT_URL, nil); err != nil {
		return err
	}
	return nil
}

// InheritSession loads an ActiveSession object into the client and verifies that
// the session data is still valid. Session objects may be retrieved using the SessionData
// function, serialized to a file, and later restored using InheritSession to implement
// basic persistent session functionality.
func (c *Client) InheritSession(sess *ActiveSession) (bool, error) {
	c.mtx.Lock()
	defer c.mtx.Unlock()

	if c.state != STATE_NEW && c.state != STATE_LOGGED_OFF {
		return false, errors.New("Client not ready for login")
	}
	//we were able to inherit session, lets set the CSRF
	c.hm.add(authHeaderName, "Bearer "+sess.JWT)

	//try to hit the test page
	if err := c.nolockTestGet(USER_INFO_URL); err != nil {
		return false, nil
	}
	c.state = STATE_AUTHED
	c.sessionData = *sess
	return true, nil
}

// LoggedIn returns true if the client is in an authenticated state.
func (c *Client) LoggedIn() bool {
	c.mtx.Lock()
	defer c.mtx.Unlock()
	return c.state == STATE_AUTHED
}

// SessionData returns a structure containing auth tokens for the current login session.
func (c Client) SessionData() (ActiveSession, error) {
	c.mtx.Lock()
	defer c.mtx.Unlock()
	if c.state != STATE_AUTHED {
		return ActiveSession{}, ErrNoLogin
	}
	return c.sessionData, nil
}

// TestGet performs a GET request to the specified URL path, e.g. `/api/test`.
// It returns nil for response code 200 or an error otherwise.
func (c *Client) TestGet(path string) error {
	c.mtx.Lock()
	defer c.mtx.Unlock()
	if c.state != STATE_AUTHED {
		return ErrNoLogin
	}
	return c.nolockTestGet(path)
}

// Sync fetches some useful information for local reference, such as user details.
// In general, you should call Sync immediately after authenticating.
func (c *Client) Sync() (err error) {
	c.mtx.Lock()
	err = c.syncNoLock()
	c.mtx.Unlock()
	return
}

func (c *Client) syncNoLock() error {
	//get the user details pulled down and populated
	//attempt to populate the userDetails structure
	userDets, err := c.getMyInfo()
	if err != nil {
		return err
	}
	c.userDetails = userDets
	return nil
}

// Close shuts down the client and cleans up connections. It does NOT terminate sessions.
func (c *Client) Close() error {
	c.mtx.Lock()
	defer c.mtx.Unlock()
	if c.state == STATE_CLOSED {
		return errors.New("Client already closed")
	}
	if c.transport != nil {
		c.transport.CloseIdleConnections()
	}
	c.state = STATE_CLOSED
	return nil
}

// SetRequestTimeout overrides the client request timeout value.
// The timeout defaults to a very high value because large downloads may take significant time.
func (c *Client) SetRequestTimeout(to time.Duration) error {
	c.mtx.Lock()
	defer c.mtx.Unlock()
	if c.state == STATE_CLOSED {
		return errors.New("Client already closed")
	}
	if to <= 0 {
		return errors.New("invalid timeout")
	}
	c.clnt.Timeout = to
	c.timeout = to
	return nil
}

// RequestTimeout returns the current client request timeout value.
func (c *Client) RequestTimeout() (time.Duration, error) {
	c.mtx.Lock()
	defer c.mtx.Unlock()
	if c.state == STATE_CLOSED {
		return 0, errors.New("Client already closed")
	}
	return c.timeout, nil
}

// displayNotifications pulls back any notifications for this user and displays the count
func (c *Client) displayNotifications() error {
	notifs, err := c.MyNewNotifications()
	if err != nil {
		return err
	}
	if len(notifs) == 0 {
		return nil
	}
	fmt.Println("---- NEW NOTIFICATIONS ----")
	for _, v := range notifs {
		fmt.Println(v.Msg)
	}
	fmt.Println("")
	return nil
}

// DialWebsocket uses the client's auth tokens to connect to a websocket on the server,
// returning the websocket connection.
func (c *Client) DialWebsocket(pth string) (conn *websocket.Conn, resp *http.Response, err error) {
	//connect get a websocket fired up against the search agent url
	u := url.URL{
		Scheme: c.wsScheme,
		Host:   c.serverURL.Host,
		Path:   pth,
	}
	dlr := &websocket.Dialer{
		Proxy:            http.ProxyFromEnvironment,
		HandshakeTimeout: 10 * time.Second,
		TLSClientConfig:  c.tlsConfig,
		Jar:              c.clnt.Jar,
	}
	hdr := make(http.Header)
	c.hm.populateRequest(hdr)
	if conn, resp, err = dlr.Dial(u.String(), hdr); err != nil {
		if resp != nil {
			resp.Body.Close()
		}
		if conn != nil {
			conn.Close()
		}
		conn = nil
		err = fmt.Errorf("Dial returned %v", err)
	}
	return
}

// SetUserAgent changes the User-Agent field the client sends with requests (default: ``GravwellCLI'').
func (c *Client) SetUserAgent(v string) error {
	if v == `` {
		return ErrEmptyUserAgent
	} else if strings.Contains(v, "\n\t") {
		return errors.New("User agent contains illegal characters")
	}
	c.mtx.Lock()
	c.hm.add(`User-Agent`, v)
	c.userAgent = v
	c.mtx.Unlock()
	return nil
}

// SetAdminMode sets the ?admin=true parameter on future API requests. Note that setting this
// parameter has no effect for non-admin users.
// Admin users should use this parameter carefully, as it gives access to objects belonging
// to other users and makes it easy to break things.
func (c *Client) SetAdminMode() {
	c.qm.set("admin", "true")
}

// ClearAdminMode unsets the ?admin=true parameter for future API requests.
func (c *Client) ClearAdminMode() {
	c.qm.remove("admin")
}

// AdminMode returns true if the ?admin=true parameter is set for API requests.
func (c *Client) AdminMode() bool {
	v, ok := c.qm.get("admin")
	if !ok {
		return false
	}
	return v == `true`
}
